/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Fork from https://github.com/ahmetb/gen-crd-api-reference-docs
*/

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/pkg/errors"
	"github.com/russross/blackfriday/v2"
	"k8s.io/gengo/parser"
	"k8s.io/gengo/types"
	"k8s.io/klog/v2"
)

var (
	flConfig            = flag.String("config", "", "path to config file")
	flAPIDir            = flag.String("api-dir", "", "api directory (or import path), point this to pkg/apis")
	flTemplateDir       = flag.String("template-dir", "template", "path to template/ dir")
	flOutDir            = flag.String("out-dir", "", "path to output file to save the result")
	filterPrefixPattern = regexp.MustCompile(`^\+|TODO|FIXME`)
)

const (
	docCommentForceIncludes = "// +gencrdrefdocs:force"
)

type generatorConfig struct {
	// HiddenMemberFields hides fields with specified names on all types.
	HiddenMemberFields []string `json:"hideMemberFields"`

	// HideTypePatterns hides types matching the specified patterns from the
	// output.
	HideTypePatterns []string `json:"hideTypePatterns"`

	// ExternalPackages lists recognized external package references and how to
	// link to them.
	ExternalPackages []externalPackage `json:"externalPackages"`

	// TypeDisplayNamePrefixOverrides is a mapping of how to override displayed
	// name for types with certain prefixes with what value.
	TypeDisplayNamePrefixOverrides map[string]string `json:"typeDisplayNamePrefixOverrides"`

	// MarkdownDisabled controls markdown rendering for comment lines.
	MarkdownDisabled bool `json:"markdownDisabled"`

	// PackageGroups is a list of package groups to be rendered.
	PackageGroups []PackageGroup `json:"packageGroups"`
}

type PackageGroup struct {
	// GroupName is the name of the package group.
	GroupName string `json:"groupName"`
	// Packages is a list of packages to be rendered.
	Packages []string `json:"packages"`
	// Order is the order of the package group.
	Order int `json:"order"`
}

type externalPackage struct {
	TypeMatchPrefix string `json:"typeMatchPrefix"`
	DocsURLTemplate string `json:"docsURLTemplate"`
}

type apiPackage struct {
	apiGroup   string
	apiVersion string
	GoPackages []*types.Package
	Types      []*types.Type // because multiple 'types.Package's can add types to an apiVersion
	Constants  []*types.Type
}

func (v *apiPackage) identifier() string { return fmt.Sprintf("%s/%s", v.apiGroup, v.apiVersion) }

func init() {
	klog.InitFlags(nil)
	err := flag.Set("alsologtostderr", "true")
	if err != nil {
		return
	}
	flag.Parse()

	if *flConfig == "" {
		panic("-config not specified")
	}
	if *flAPIDir == "" {
		panic("-api-dir not specified")
	}
	if *flOutDir == "" {
		panic("-out-file or -http-addr must be specified")
	}
	if err := resolveTemplateDir(*flTemplateDir); err != nil {
		panic(err)
	}

}

func resolveTemplateDir(dir string) error {
	path, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(path); err != nil {
		return errors.Wrapf(err, "cannot read the %s directory", path)
	} else if !fi.IsDir() {
		return errors.Errorf("%s path is not a directory", path)
	}
	return nil
}

func main() {
	defer klog.Flush()

	f, err := os.Open(*flConfig)
	if err != nil {
		klog.Fatalf("failed to open config file: %+v", err)
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	var config generatorConfig
	if err := d.Decode(&config); err != nil {
		klog.Fatalf("failed to parse config file: %+v", err)
	}

	klog.Infof("parsing go packages in directory %s", *flAPIDir)
	pkgs, err := parseAPIPackages()
	if err != nil {
		klog.Fatal(err)
	}
	if len(pkgs) == 0 {
		klog.Fatalf("no API packages found in %s", *flAPIDir)
	}

	allAPIPackages, err := combineAPIPackages(pkgs)
	if err != nil {
		klog.Fatal(err)
	}

	mkOutput := func(groupName string, groupAPIPakcages []*apiPackage) (string, error) {
		var b bytes.Buffer
		err := render(&b, groupName, groupAPIPakcages, config)
		if err != nil {
			return "", errors.Wrap(err, "failed to render the result")
		}

		// remove leading & trailing whitespace from each html line for markdown renderers
		// however preserving whitespace indent for code blocks, so they're properly formatted

		// remove every line's trailing whitespace & empty line
		s := regexp.MustCompile(`(?m)\s+$`).ReplaceAllString(b.String(), "")
		// remove rest empty lines
		s = regexp.MustCompile(`(?m)^$[\r\n]*`).ReplaceAllString(s, "")
		// remove leading whitespace if a line begins with an HTML tag "<...>"
		s = regexp.MustCompile(`(?m)^\s+<`).ReplaceAllString(s, "<")
		// remove leading whitespace if the last line ends with an HTML tag "<...>"
		s = regexp.MustCompile(`>\n\s+`).ReplaceAllString(s, ">\n")
		// remove leading whitespace if a line ends with an HTML tag "<...>"
		re := regexp.MustCompile(`(?m)^\s+(.*)>$`)
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			match = re.ReplaceAllString(match, "$1>")
			return match
		})
		return s, nil
	}

	for _, pg := range config.PackageGroups {
		klog.Infof("using group: %s with packages: %s", pg.GroupName, pg.Packages)
		var groupAPIPakcages []*apiPackage
		for _, pkg := range allAPIPackages {
			for _, groupPkg := range pg.Packages {
				if pkg.apiGroup == groupPkg {
					groupAPIPakcages = append(groupAPIPakcages, pkg)
				}
			}
		}

		outfile := fmt.Sprintf("%s%s.md", *flOutDir, pg.GroupName)
		dir := filepath.Dir(outfile)
		if err := os.MkdirAll(dir, 0755); err != nil {
			klog.Fatalf("failed to create dir %s: %v", dir, err)
		}
		s, err := mkOutput(pg.GroupName, groupAPIPakcages)
		if err != nil {
			klog.Fatalf("failed: %+v", err)
		}
		if err := os.WriteFile(outfile, []byte(s), 0644); err != nil {
			klog.Fatalf("failed to write to out file: %v", err)
		}
		klog.Infof("written to %s\n", outfile)
	}

}

// groupName extracts the "//+groupName" meta-comment from the specified
// package's comments, or returns empty string if it cannot be found.
func groupName(pkg *types.Package) string {
	m := types.ExtractCommentTags("+", pkg.Comments)
	v := m["groupName"]
	if len(v) == 1 {
		return v[0]
	}
	return ""
}

func parseAPIPackages() ([]*types.Package, error) {
	b := parser.New()
	// the following will silently fail (turn on -v=4 to see logs)
	if err := b.AddDirRecursive(*flAPIDir); err != nil {
		return nil, err
	}
	scan, err := b.FindTypes()
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse pkgs and types")
	}
	var pkgNames []string
	for p := range scan {
		pkg := scan[p]
		klog.V(3).Infof("trying package=%v groupName=%s", p, groupName(pkg))

		// Do not pick up packages that are in vendor/ as API packages. (This
		// happened in knative/eventing-sources/vendor/..., where a package
		// matched the pattern, but it didn't have a compatible import path).
		if isVendorPackage(pkg) {
			klog.V(3).Infof("package=%v coming from vendor/, ignoring.", p)
			continue
		}

		if groupName(pkg) != "" && len(pkg.Types) > 0 || containsString(pkg.DocComments, docCommentForceIncludes) {
			klog.V(3).Infof("package=%v has groupName and has types", p)
			pkgNames = append(pkgNames, p)
		}
	}
	sort.Strings(pkgNames)
	var pkgs []*types.Package
	for _, p := range pkgNames {
		pkgs = append(pkgs, scan[p])
	}
	return pkgs, nil
}

func containsString(sl []string, str string) bool {
	for _, s := range sl {
		if str == s {
			return true
		}
	}
	return false
}

// combineAPIPackages groups the Go packages by the <apiGroup+apiVersion> they
// offer, and combines the types in them.
func combineAPIPackages(pkgs []*types.Package) ([]*apiPackage, error) {
	pkgMap := make(map[string]*apiPackage)
	var pkgIds []string

	flattenTypes := func(typeMap map[string]*types.Type) []*types.Type {
		typeList := make([]*types.Type, 0, len(typeMap))

		for _, t := range typeMap {
			typeList = append(typeList, t)
		}

		return typeList
	}

	for _, pkg := range pkgs {
		apiGroup, apiVersion, err := apiVersionForPackage(pkg)
		if err != nil {
			return nil, errors.Wrapf(err, "could not get apiVersion for package %s", pkg.Path)
		}

		id := fmt.Sprintf("%s/%s", apiGroup, apiVersion)
		v, ok := pkgMap[id]
		if !ok {
			pkgMap[id] = &apiPackage{
				apiGroup:   apiGroup,
				apiVersion: apiVersion,
				Types:      flattenTypes(pkg.Types),
				Constants:  flattenTypes(pkg.Constants),
				GoPackages: []*types.Package{pkg},
			}
			pkgIds = append(pkgIds, id)
		} else {
			v.Types = append(v.Types, flattenTypes(pkg.Types)...)
			v.Constants = append(v.Constants, flattenTypes(pkg.Constants)...)
			v.GoPackages = append(v.GoPackages, pkg)
		}
	}

	sort.Strings(pkgIds)

	out := make([]*apiPackage, 0, len(pkgMap))
	for _, id := range pkgIds {
		out = append(out, pkgMap[id])
	}
	return out, nil
}

// isVendorPackage determines if package is coming from vendor/ dir.
func isVendorPackage(pkg *types.Package) bool {
	vendorPattern := string(os.PathSeparator) + "vendor" + string(os.PathSeparator)
	return strings.Contains(pkg.SourcePath, vendorPattern)
}

func findTypeReferences(pkgs []*apiPackage) map[*types.Type][]*types.Type {
	m := make(map[*types.Type][]*types.Type)
	for _, pkg := range pkgs {
		for _, typ := range pkg.Types {
			for _, member := range typ.Members {
				t := member.Type
				t = tryDereference(t)
				m[t] = append(m[t], typ)
			}
		}
	}
	return m
}

func isExportedType(t *types.Type) bool {
	return strings.Contains(strings.Join(t.SecondClosestCommentLines, "\n"), "+genclient") || strings.Contains(strings.Join(t.SecondClosestCommentLines, "\n"), "+kubebuilder:object:root=true")
}

func fieldName(m types.Member) string {
	v := reflect.StructTag(m.Tags).Get("json")
	v = strings.TrimSuffix(v, ",omitempty")
	v = strings.TrimSuffix(v, ",inline")
	if v != "" {
		return v
	}
	return m.Name
}

func fieldEmbedded(m types.Member) bool {
	return strings.Contains(reflect.StructTag(m.Tags).Get("json"), ",inline")
}

func isLocalType(t *types.Type, typePkgMap map[*types.Type]*apiPackage) bool {
	t = tryDereference(t)
	_, ok := typePkgMap[t]
	return ok
}

func renderComments(s []string, markdown bool) string {
	s = filterCommentTags(s)
	doc := strings.Join(s, "\n")

	if markdown {
		doc = string(blackfriday.Run([]byte(doc)))
		doc = strings.ReplaceAll(doc, "{", string(template.HTML("&#123;")))
		doc = strings.ReplaceAll(doc, "}", string(template.HTML("&#125;")))
		return doc
	}
	return nl2br(doc)
}

func safe(s string) template.HTML { return template.HTML(s) }

func toTitle(s string) string { return cases.Title(language.English).String(s) }

func nl2br(s string) string {
	return strings.ReplaceAll(s, "\n\n", string(template.HTML("<br/><br/>")))
}

func hiddenMember(m types.Member, c generatorConfig) bool {
	for _, v := range c.HiddenMemberFields {
		if m.Name == v {
			return true
		}
	}
	return false
}

func typeIdentifier(t *types.Type) string {
	t = tryDereference(t)
	return t.Name.String() // {PackagePath.Name}
}

// apiGroupForType looks up apiGroup for the given type
func apiGroupForType(t *types.Type, typePkgMap map[*types.Type]*apiPackage) string {
	t = tryDereference(t)

	v := typePkgMap[t]
	if v == nil {
		klog.Warningf("WARNING: cannot read apiVersion for %s from type=>pkg map", t.Name.String())
		return "<UNKNOWN_API_GROUP>"
	}

	return v.identifier()
}

// anchorIDForLocalType returns the #anchor string for the local type
func anchorIDForLocalType(t *types.Type, typePkgMap map[*types.Type]*apiPackage) string {
	return fmt.Sprintf("%s.%s", apiGroupForType(t, typePkgMap), t.Name.Name)
}

// linkForType returns an anchor to the type if it can be generated. returns
// empty string if it is not a local type or unrecognized external type.
func linkForType(t *types.Type, c generatorConfig, typePkgMap map[*types.Type]*apiPackage) (string, error) {
	t = tryDereference(t) // dereference kind=Pointer

	if isLocalType(t, typePkgMap) {
		return "#" + anchorIDForLocalType(t, typePkgMap), nil
	}

	var arrIndex = func(a []string, i int) string {
		return a[(len(a)+i)%len(a)]
	}

	// types like k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta,
	// k8s.io/api/core/v1.Container, k8s.io/api/autoscaling/v1.CrossVersionObjectReference,
	// github.com/knative/build/pkg/apis/build/v1alpha1.BuildSpec
	if t.Kind == types.Struct || t.Kind == types.Pointer || t.Kind == types.Interface || t.Kind == types.Alias {
		id := typeIdentifier(t)                        // gives {{ImportPath.Identifier}} for type
		segments := strings.Split(t.Name.Package, "/") // to parse [meta, v1] from "k8s.io/apimachinery/pkg/apis/meta/v1"

		for _, v := range c.ExternalPackages {
			r, err := regexp.Compile(v.TypeMatchPrefix)
			if err != nil {
				return "", errors.Wrapf(err, "pattern %q failed to compile", v.TypeMatchPrefix)
			}
			if r.MatchString(id) {
				tpl, err := texttemplate.New("").Funcs(map[string]interface{}{
					"lower":    strings.ToLower,
					"arrIndex": arrIndex,
				}).Parse(v.DocsURLTemplate)
				if err != nil {
					return "", errors.Wrap(err, "docs URL template failed to parse")
				}

				var b bytes.Buffer
				if err := tpl.
					Execute(&b, map[string]interface{}{
						"TypeIdentifier":  t.Name.Name,
						"PackagePath":     t.Name.Package,
						"PackageSegments": segments,
					}); err != nil {
					return "", errors.Wrap(err, "docs url template execution error")
				}
				return b.String(), nil
			}
		}
		klog.Warningf("not found external link source for type %v", t.Name)
	}
	return "", nil
}

// tryDereference returns the underlying type when t is a pointer, map, or slice.
func tryDereference(t *types.Type) *types.Type {
	for t.Elem != nil {
		t = t.Elem
	}
	return t
}

// finalUnderlyingTypeOf walks the type hierarchy for t and returns
// its base type (i.e. the type that has no further underlying type).
func finalUnderlyingTypeOf(t *types.Type) *types.Type {
	for {
		if t.Underlying == nil {
			return t
		}

		t = t.Underlying
	}
}

func typeDisplayName(t *types.Type, c generatorConfig, typePkgMap map[*types.Type]*apiPackage) string {
	s := typeIdentifier(t)

	if isLocalType(t, typePkgMap) {
		s = tryDereference(t).Name.Name
	}

	if t.Kind == types.Pointer {
		s = strings.TrimLeft(s, "*")
	}

	switch t.Kind {
	case types.Struct,
		types.Interface,
		types.Alias,
		types.Pointer,
		types.Slice,
		types.Builtin:
		// noop
	case types.Map:
		// return original name
		return t.Name.Name
	case types.DeclarationOf:
		// For constants, we want to display the value
		// rather than the name of the constant, since the
		// value is what users will need to write into YAML
		// specs.
		if t.ConstValue != nil {
			u := finalUnderlyingTypeOf(t)
			// Quote string constants to make it clear to the documentation reader.
			if u.Kind == types.Builtin && u.Name.Name == "string" {
				return strconv.Quote(*t.ConstValue)
			}

			return *t.ConstValue
		}
		klog.Fatalf("type %s is a non-const declaration, which is unhandled", t.Name)
	default:
		klog.Fatalf("type %s has kind=%v which is unhandled", t.Name, t.Kind)
	}

	// substitute prefix, if registered
	for prefix, replacement := range c.TypeDisplayNamePrefixOverrides {
		if strings.HasPrefix(s, prefix) {
			s = strings.Replace(s, prefix, replacement, 1)
		}
	}

	if t.Kind == types.Slice {
		s = "[]" + s
	}

	return s
}

func hideType(t *types.Type, c generatorConfig) bool {
	for _, pattern := range c.HideTypePatterns {
		if regexp.MustCompile(pattern).MatchString(t.Name.String()) {
			return true
		}
	}
	if !isExportedType(t) && unicode.IsLower(rune(t.Name.Name[0])) {
		// types that start with lowercase
		return true
	}
	return false
}

func typeReferences(t *types.Type, c generatorConfig, references map[*types.Type][]*types.Type) []*types.Type {
	var out []*types.Type
	m := make(map[*types.Type]struct{})
	for _, ref := range references[t] {
		if !hideType(ref, c) {
			m[ref] = struct{}{}
		}
	}
	for k := range m {
		out = append(out, k)
	}
	sortTypes(out)
	return out
}

func sortTypes(typs []*types.Type) []*types.Type {
	sort.Slice(typs, func(i, j int) bool {
		t1, t2 := typs[i], typs[j]
		if isExportedType(t1) && !isExportedType(t2) {
			return true
		} else if !isExportedType(t1) && isExportedType(t2) {
			return false
		}
		return t1.Name.String() < t2.Name.String()
	})
	return typs
}

func visibleTypes(in []*types.Type, c generatorConfig) []*types.Type {
	var out []*types.Type
	for _, t := range in {
		if !hideType(t, c) {
			out = append(out, t)
		}
	}
	return out
}

func filterCommentTags(comments []string) []string {
	var out []string
	for _, v := range comments {
		if !filterPrefixPattern.MatchString(v) {
			out = append(out, v)
		}
	}
	return out
}

func isOptionalMember(m types.Member) bool {
	tags := types.ExtractCommentTags("+", m.CommentLines)
	_, ok := tags["optional"]
	return ok
}

func apiVersionForPackage(pkg *types.Package) (string, string, error) {
	group := groupName(pkg)
	version := pkg.Name // assumes basename (i.e. "v1" in "core/v1") is apiVersion
	r := `^v\d+((alpha|beta|api|stable)[a-z0-9]+)?$`
	if !regexp.MustCompile(r).MatchString(version) {
		return "", "", errors.Errorf("cannot infer kubernetes apiVersion of go package %s (basename %q doesn't match expected pattern %s that's used to determine apiVersion)", pkg.Path, version, r)
	}
	return group, version, nil
}

// extractTypeToPackageMap creates a *types.Type map to apiPackage
func extractTypeToPackageMap(pkgs []*apiPackage) map[*types.Type]*apiPackage {
	out := make(map[*types.Type]*apiPackage)
	for _, ap := range pkgs {
		for _, t := range ap.Types {
			out[t] = ap
		}
		for _, t := range ap.Constants {
			out[t] = ap
		}
	}
	return out
}

// constantsOfType finds all the constants in pkg that have the
// same underlying type as t. This is intended for use by enum
// type validation, where users need to specify one of a specific
// set of constant values for a field.
func constantsOfType(t *types.Type, pkg *apiPackage) []*types.Type {
	var constants []*types.Type

	for _, c := range pkg.Constants {
		if c.Underlying == t {
			constants = append(constants, c)
		}
	}

	return sortTypes(constants)
}

func getAPIDocOrder(filename string, pg []PackageGroup) int {
	for _, p := range pg {
		if filename == p.GroupName {
			return p.Order
		}
	}
	return 1000
}

func render(w io.Writer, groupName string, pkgs []*apiPackage, config generatorConfig) error {
	references := findTypeReferences(pkgs)
	typePkgMap := extractTypeToPackageMap(pkgs)

	t, err := template.New("").Funcs(map[string]interface{}{
		"isExportedType":     isExportedType,
		"fieldName":          fieldName,
		"fieldEmbedded":      fieldEmbedded,
		"typeIdentifier":     typeIdentifier,
		"typeDisplayName":    func(t *types.Type) string { return typeDisplayName(t, config, typePkgMap) },
		"visibleTypes":       func(t []*types.Type) []*types.Type { return visibleTypes(t, config) },
		"renderComments":     func(s []string) string { return renderComments(s, !config.MarkdownDisabled) },
		"packageDisplayName": func(p *apiPackage) string { return p.identifier() },
		"apiGroup":           func(t *types.Type) string { return apiGroupForType(t, typePkgMap) },
		"packageAnchorID": func(p *apiPackage) string {
			return strings.ReplaceAll(p.identifier(), " ", "")
		},
		"linkForType": func(t *types.Type) string {
			v, err := linkForType(t, config, typePkgMap)
			if err != nil {
				klog.Fatal(errors.Wrapf(err, "error getting link for type=%s", t.Name))
				return ""
			}
			return v
		},
		"anchorIDForType":  func(t *types.Type) string { return anchorIDForLocalType(t, typePkgMap) },
		"safe":             safe,
		"toTitle":          toTitle,
		"sortedTypes":      sortTypes,
		"typeReferences":   func(t *types.Type) []*types.Type { return typeReferences(t, config, references) },
		"hiddenMember":     func(m types.Member) bool { return hiddenMember(m, config) },
		"isLocalType":      isLocalType,
		"isOptionalMember": isOptionalMember,
		"constantsOfType":  func(t *types.Type) []*types.Type { return constantsOfType(t, typePkgMap[t]) },
	}).ParseGlob(filepath.Join(*flTemplateDir, "*.tpl"))
	if err != nil {
		return errors.Wrap(err, "parse error")
	}

	filerOrder := getAPIDocOrder(groupName, config.PackageGroups)

	return errors.Wrap(t.ExecuteTemplate(w, "packages", map[string]interface{}{
		"packages":   pkgs,
		"apiName":    groupName,
		"filerOrder": filerOrder,
	}), "template execution error")
}
