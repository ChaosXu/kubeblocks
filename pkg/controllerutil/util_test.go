/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package controllerutil

import (
	"context"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/tools/record"

	"github.com/apecloud/kubeblocks/pkg/constant"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

var _ = Describe("utils test", func() {
	Context("MergeList", func() {
		It("should work well", func() {
			src := []corev1.Volume{
				{
					Name: "pvc1",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc1-pod-0",
						},
					},
				},
				{
					Name: "pvc2",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc2-pod-0",
						},
					},
				},
			}
			dst := []corev1.Volume{
				{
					Name: "pvc0",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc0-pod-0",
						},
					},
				},
				{
					Name: "pvc1",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "pvc-pod-0",
						},
					},
				},
			}
			MergeList(&src, &dst, func(v corev1.Volume) func(corev1.Volume) bool {
				return func(volume corev1.Volume) bool {
					return v.Name == volume.Name
				}
			})

			Expect(dst).Should(HaveLen(3))
			slices.SortStableFunc(dst, func(a, b corev1.Volume) int {
				return strings.Compare(a.Name, b.Name)
			})
			Expect(dst[0].Name).Should(Equal("pvc0"))
			Expect(dst[1].Name).Should(Equal("pvc1"))
			Expect(dst[1].PersistentVolumeClaim).ShouldNot(BeNil())
			Expect(dst[1].PersistentVolumeClaim.ClaimName).Should(Equal("pvc1-pod-0"))
			Expect(dst[2].Name).Should(Equal("pvc2"))
		})
	})
})

func TestGetUncachedObjects(t *testing.T) {
	GetUncachedObjects()
}

func TestRequestCtxMisc(t *testing.T) {
	itFuncs := func(reqCtx *RequestCtx) {
		reqCtx.Event(nil, "type", "reason", "msg")
		reqCtx.Eventf(nil, "type", "reason", "%s", "arg")
		if reqCtx != nil {
			reqCtx.UpdateCtxValue("key", "value")
			reqCtx.WithValue("key", "value")
		}
	}
	itFuncs(nil)
	itFuncs(&RequestCtx{
		Ctx:      context.Background(),
		Recorder: record.NewFakeRecorder(100),
	})
}

func TestGetKubeVersion(t *testing.T) {
	tests := []struct {
		name        string
		versionInfo interface{}
		expected    string
		withError   bool
	}{
		{
			name:        "valid version info",
			versionInfo: version.Info{GitVersion: "v1.20"},
			expected:    "v1.20",
			withError:   false,
		},
		{
			name:        "invalid version info",
			versionInfo: "invalid",
			expected:    "",
			withError:   true,
		},
		{
			name:        "invalid major version",
			versionInfo: version.Info{GitVersion: "vmajor.20"},
			expected:    "",
			withError:   true,
		},
		{
			name:        "invalid minor version",
			versionInfo: version.Info{GitVersion: "v1.minor"},
			expected:    "",
			withError:   true,
		},
		{
			name:        "version with suffix",
			versionInfo: version.Info{GitVersion: "v1.20.0-rc1"},
			expected:    "v1.20",
			withError:   false,
		},
	}

	defer viper.Set(constant.CfgKeyServerInfo, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Set(constant.CfgKeyServerInfo, tt.versionInfo)
			ver, err := GetKubeVersion()
			assert.Equal(t, tt.expected, ver)
			if tt.withError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
