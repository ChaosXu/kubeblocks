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

package utils

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"golang.org/x/mod/semver"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/common"
	"github.com/apecloud/kubeblocks/pkg/constant"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	dptypes "github.com/apecloud/kubeblocks/pkg/dataprotection/types"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/utils/boolptr"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

func AddTolerations(podSpec *corev1.PodSpec) (err error) {
	if cmTolerations := viper.GetString(constant.CfgKeyCtrlrMgrTolerations); cmTolerations != "" {
		if err = json.Unmarshal([]byte(cmTolerations), &podSpec.Tolerations); err != nil {
			return err
		}
	}
	if cmAffinity := viper.GetString(constant.CfgKeyCtrlrMgrAffinity); cmAffinity != "" {
		if err = json.Unmarshal([]byte(cmAffinity), &podSpec.Affinity); err != nil {
			return err
		}
	}
	if cmNodeSelector := viper.GetString(constant.CfgKeyCtrlrMgrNodeSelector); cmNodeSelector != "" {
		if err = json.Unmarshal([]byte(cmNodeSelector), &podSpec.NodeSelector); err != nil {
			return err
		}
	}
	return nil
}

// IsJobFinished if the job is completed or failed, return true.
// if the job is failed, return the failed message.
func IsJobFinished(job *batchv1.Job) (bool, batchv1.JobConditionType, string) {
	if job == nil {
		return false, "", ""
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete {
			return true, c.Type, ""
		}
		if c.Type == batchv1.JobFailed {
			return true, c.Type, c.Reason + ":" + c.Message
		}
	}
	return false, "", ""
}

func GetAssociatedPodsOfJob(ctx context.Context, cli client.Client, namespace, jobName string, opts ...client.ListOption) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	// from https://github.com/kubernetes/kubernetes/issues/24709
	opts = append(
		[]client.ListOption{
			client.InNamespace(namespace),
			client.MatchingLabels{
				"job-name": jobName,
			},
		},
		opts...)
	err := cli.List(ctx, podList, opts...)
	return podList, err
}

func RemoveDataProtectionFinalizer(ctx context.Context, cli client.Client, obj client.Object) error {
	if !controllerutil.ContainsFinalizer(obj, dptypes.DataProtectionFinalizerName) {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	controllerutil.RemoveFinalizer(obj, dptypes.DataProtectionFinalizerName)
	return cli.Patch(ctx, obj, patch)
}

// GetActionSetByName gets the ActionSet by name.
func GetActionSetByName(reqCtx intctrlutil.RequestCtx, cli client.Client, name string) (*dpv1alpha1.ActionSet, error) {
	if name == "" {
		return nil, nil
	}
	as := &dpv1alpha1.ActionSet{}
	if err := cli.Get(reqCtx.Ctx, client.ObjectKey{Name: name}, as); err != nil {
		reqCtx.Log.Error(err, "failed to get ActionSet for backup.", "ActionSet", name)
		return nil, err
	}
	return as, nil
}

func GetBackupPolicyByName(reqCtx intctrlutil.RequestCtx, cli client.Client, name string) (*dpv1alpha1.BackupPolicy, error) {
	backupPolicy := &dpv1alpha1.BackupPolicy{}
	key := client.ObjectKey{
		Namespace: reqCtx.Req.Namespace,
		Name:      name,
	}
	if err := cli.Get(reqCtx.Ctx, key, backupPolicy); err != nil {
		return nil, err
	}
	return backupPolicy, nil
}

func GetBackupMethodByName(name string, backupPolicy *dpv1alpha1.BackupPolicy) *dpv1alpha1.BackupMethod {
	for i, m := range backupPolicy.Spec.BackupMethods {
		if m.Name == name {
			return &backupPolicy.Spec.BackupMethods[i]
		}
	}
	return nil
}

func GetPodListByLabelSelector(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	labelSelector *metav1.LabelSelector) (*corev1.PodList, error) {
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}
	targetPodList := &corev1.PodList{}
	if err = cli.List(reqCtx.Ctx, targetPodList,
		client.InNamespace(reqCtx.Req.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	return targetPodList, nil
}

func GetBackupVolumeSnapshotName(backupName, volumeSource string, index int) string {
	return fmt.Sprintf("%s-%d-%s", backupName, index, volumeSource)
}

func GetOldBackupVolumeSnapshotName(backupName, volumeSource string) string {
	return fmt.Sprintf("%s-%s", backupName, volumeSource)
}

// MergeEnv merges the targetEnv to original env. if original env exist the same name var, it will be replaced.
func MergeEnv(originalEnv, targetEnv []corev1.EnvVar) []corev1.EnvVar {
	if len(targetEnv) == 0 {
		return originalEnv
	}
	originalEnvIndexMap := map[string]int{}
	for i := range originalEnv {
		originalEnvIndexMap[originalEnv[i].Name] = i
	}
	for i := range targetEnv {
		if index, ok := originalEnvIndexMap[targetEnv[i].Name]; ok {
			originalEnv[index] = targetEnv[i]
		} else {
			originalEnv = append(originalEnv, targetEnv[i])
		}
	}
	return originalEnv
}

// VolumeSnapshotEnabled checks if the volumes support snapshot.
func VolumeSnapshotEnabled(ctx context.Context, cli client.Client, pod *corev1.Pod, volumes []string) (bool, error) {
	if pod == nil {
		return false, nil
	}
	var pvcNames []string
	// get the pvcs by volumes
	for _, v := range pod.Spec.Volumes {
		for i := range volumes {
			if v.Name != volumes[i] {
				continue
			}
			if v.PersistentVolumeClaim == nil {
				return false, fmt.Errorf(`the type of volume "%s" is not PersistentVolumeClaim on pod "%s"`, v.Name, pod.Name)
			}
			pvcNames = append(pvcNames, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(pvcNames) == 0 {
		return false, fmt.Errorf(`can not find any volume by targetVolumes %v on pod "%s"`, volumes, pod.Name)
	}
	// get the storageClass by pvc
	for i := range pvcNames {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := cli.Get(ctx, types.NamespacedName{Name: pvcNames[i], Namespace: pod.Namespace}, pvc); err != nil {
			return false, nil
		}
		enabled, err := IsVolumeSnapshotEnabled(ctx, cli, pvc.Spec.VolumeName)
		if err != nil {
			return false, err
		}
		if !enabled {
			return false, fmt.Errorf(`cannot find any VolumeSnapshotClass of persistentVolumeClaim "%s" to do volume snapshot on pod "%s"`, pvc.Name, pod.Name)
		}
	}
	return true, nil
}

func SetControllerReference(owner, controlled metav1.Object, scheme *runtime.Scheme) error {
	if owner == nil || reflect.ValueOf(owner).IsNil() {
		return nil
	}
	return controllerutil.SetControllerReference(owner, controlled, scheme)
}

// CovertEnvToMap coverts env array to map.
func CovertEnvToMap(env []corev1.EnvVar) map[string]string {
	envMap := map[string]string{}
	for _, v := range env {
		if v.ValueFrom != nil {
			continue
		}
		envMap[v.Name] = v.Value
	}
	return envMap
}

func GetBackupType(actionSet *dpv1alpha1.ActionSet, useSnapshot *bool) dpv1alpha1.BackupType {
	if actionSet != nil {
		return actionSet.Spec.BackupType
	} else if boolptr.IsSetToTrue(useSnapshot) {
		return dpv1alpha1.BackupTypeFull
	}
	return ""
}

func GetBackupTypeByMethodName(reqCtx intctrlutil.RequestCtx, cli client.Client, methodName string,
	backupPolicy *dpv1alpha1.BackupPolicy) (dpv1alpha1.BackupType, error) {
	backupMethod := GetBackupMethodByName(methodName, backupPolicy)
	if backupMethod == nil {
		return "", nil
	}
	actionSet, err := GetActionSetByName(reqCtx, cli, backupMethod.ActionSetName)
	if err != nil {
		return "", err
	}
	return GetBackupType(actionSet, backupMethod.SnapshotVolumes), nil
}

// PrependSpaces prepends spaces to each line of the content.
func PrependSpaces(content string, spaces int) string {
	prefix := ""
	for i := 0; i < spaces; i++ {
		prefix += " "
	}
	r := bytes.NewBufferString(content)
	w := bytes.NewBuffer(nil)
	w.Grow(r.Len())
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			w.WriteString(prefix)
			w.WriteString(line)
		}
		if err != nil {
			break
		}
	}
	return w.String()
}

// GetFirstIndexRunningPod gets the first running pod with index.
func GetFirstIndexRunningPod(podList *corev1.PodList) *corev1.Pod {
	if podList == nil {
		return nil
	}
	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].Name < podList.Items[j].Name
	})
	for _, v := range podList.Items {
		if intctrlutil.IsPodAvailable(&v, 0) {
			return &v
		}
	}
	return nil
}

func GetPodByName(podList *corev1.PodList, name string) *corev1.Pod {
	if podList == nil {
		return nil
	}
	for i, v := range podList.Items {
		if v.Name == name {
			return &podList.Items[i]
		}
	}
	return nil
}

func SupportsCronJobV1() bool {
	kubeVersion, err := intctrlutil.GetKubeVersion()
	if err != nil {
		return true
	}
	return semver.Compare(kubeVersion, "v1.21") >= 0
}

func GetPodFirstContainerPort(pod *corev1.Pod) int32 {
	ports := pod.Spec.Containers[0].Ports
	if len(ports) == 0 {
		return 0
	}
	return ports[0].ContainerPort
}

// GetDPDBPortEnv get the EnvVar which consists of the port number of targetPod.
func GetDPDBPortEnv(pod *corev1.Pod, containerPort *dpv1alpha1.ContainerPort) (*corev1.EnvVar, error) {
	if containerPort == nil {
		return &corev1.EnvVar{Name: dptypes.DPDBPort, Value: strconv.Itoa(int(GetPodFirstContainerPort(pod)))}, nil
	}
	containerName := containerPort.ContainerName
	portName := containerPort.PortName
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		for _, port := range container.Ports {
			if port.Name == portName {
				return &corev1.EnvVar{Name: dptypes.DPDBPort, Value: strconv.Itoa(int(port.ContainerPort))}, nil
			}
		}
	}
	return nil, fmt.Errorf("the specified containerPort of targetPod is not found")
}

// ExistTargetVolume checks if the backup.status.backupMethod.targetVolumes exists the target volume which should be restored.
func ExistTargetVolume(targetVolumes *dpv1alpha1.TargetVolumeInfo, volumeName string) bool {
	for _, v := range targetVolumes.Volumes {
		if v == volumeName {
			return true
		}
	}
	for _, v := range targetVolumes.VolumeMounts {
		if v.Name == volumeName {
			return true
		}
	}
	return false
}

// GetBackupTargets gets the backup targets by 'backupMethod' and 'backupPolicy'. 'backupMethod' has a higher priority than the global targets in 'backupPolicy'.
func GetBackupTargets(backupPolicy *dpv1alpha1.BackupPolicy, backupMethod *dpv1alpha1.BackupMethod) []dpv1alpha1.BackupTarget {
	var targets []dpv1alpha1.BackupTarget
	switch {
	case backupMethod.Target != nil:
		targets = append(targets, *backupMethod.Target)
	case len(backupMethod.Targets) > 0:
		targets = backupMethod.Targets
	case backupPolicy.Spec.Target != nil:
		targets = append(targets, *backupPolicy.Spec.Target)
	case len(backupPolicy.Spec.Targets) > 0:
		targets = backupPolicy.Spec.Targets
	}
	return targets
}

func GetBackupStatusTarget(backupObj *dpv1alpha1.Backup, sourceTargetName string) *dpv1alpha1.BackupStatusTarget {
	if backupObj.Status.Target != nil {
		return backupObj.Status.Target
	}
	for _, v := range backupObj.Status.Targets {
		if sourceTargetName == v.Name {
			return &v
		}
	}
	return nil
}

func ValidateParameters(actionSet *dpv1alpha1.ActionSet, parameters []dpv1alpha1.ParameterPair, isBackup bool) error {
	if len(parameters) == 0 {
		return nil
	}
	if actionSet == nil {
		return fmt.Errorf("actionSet is empty")
	}
	var withParameters []string
	if isBackup && actionSet.Spec.Backup != nil {
		withParameters = actionSet.Spec.Backup.WithParameters
	} else if !isBackup && actionSet.Spec.Restore != nil {
		withParameters = actionSet.Spec.Restore.WithParameters
	}
	if len(withParameters) < len(parameters) {
		return fmt.Errorf("some parameters are undeclared in withParameters of actionSet %s", actionSet.Name)
	}
	// check whether the parameter is declared in withParameters
	parametersMap := map[string]string{}
	for _, pair := range parameters {
		parametersMap[pair.Name] = pair.Value
	}
	withParametersMap := map[string]struct{}{}
	for _, v := range withParameters {
		withParametersMap[v] = struct{}{}
	}
	for k := range parametersMap {
		if _, ok := withParametersMap[k]; !ok {
			return fmt.Errorf("parameter %s is undeclared in withParameters of actionSet %s", k, actionSet.Name)
		}
	}
	schema := actionSet.Spec.ParametersSchema
	if schema == nil || schema.OpenAPIV3Schema == nil || len(schema.OpenAPIV3Schema.Properties) == 0 {
		return fmt.Errorf("the parametersSchema is invalid in actionSet %s", actionSet.Name)
	}
	// convert to type map[string]interface{} and validate the schema
	params, err := common.ConvertStringToInterfaceBySchemaType(schema.OpenAPIV3Schema, parametersMap)
	if err != nil {
		return intctrlutil.NewFatalError(err.Error())
	}
	if err = common.ValidateDataWithSchema(schema.OpenAPIV3Schema, params); err != nil {
		return intctrlutil.NewFatalError(err.Error())
	}
	return nil
}

func CompareWithBackupStopTime(backupI, backupJ dpv1alpha1.Backup) bool {
	endTimeI := backupI.GetEndTime()
	endTimeJ := backupJ.GetEndTime()
	if endTimeI.IsZero() {
		return false
	}
	if endTimeJ.IsZero() {
		return true
	}
	if endTimeI.Equal(endTimeJ) {
		return backupI.Name < backupJ.Name
	}
	return endTimeI.Before(endTimeJ)
}
