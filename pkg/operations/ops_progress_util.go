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

package operations

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubectl/pkg/util/podutils"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/apecloud/kubeblocks/apis/apps/v1"
	opsv1alpha1 "github.com/apecloud/kubeblocks/apis/operations/v1alpha1"
	workloads "github.com/apecloud/kubeblocks/apis/workloads/v1"
	"github.com/apecloud/kubeblocks/pkg/constant"
	intctrlcomp "github.com/apecloud/kubeblocks/pkg/controller/component"
	"github.com/apecloud/kubeblocks/pkg/controller/instanceset"
	"github.com/apecloud/kubeblocks/pkg/controller/sharding"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
)

// getProgressObjectKey gets progress object key from the client.Object.
func getProgressObjectKey(kind, name string) string {
	return fmt.Sprintf("%s/%s", kind, name)
}

// isCompletedProgressStatus checks the progress detail with final state, either Failed or Succeed.
func isCompletedProgressStatus(status opsv1alpha1.ProgressStatus) bool {
	return slices.Contains([]opsv1alpha1.ProgressStatus{opsv1alpha1.SucceedProgressStatus,
		opsv1alpha1.FailedProgressStatus}, status)
}

// setComponentStatusProgressDetail sets the corresponding progressDetail in progressDetails to newProgressDetail.
// progressDetails must be non-nil.
// 1. the startTime and endTime will be filled automatically.
// 2. if the progressDetail of the specified objectKey does not exist, it will be appended to the progressDetails.
func setComponentStatusProgressDetail(
	recorder record.EventRecorder,
	opsRequest *opsv1alpha1.OpsRequest,
	progressDetails *[]opsv1alpha1.ProgressStatusDetail,
	newProgressDetail opsv1alpha1.ProgressStatusDetail) {
	if progressDetails == nil {
		return
	}
	var existingProgressDetail *opsv1alpha1.ProgressStatusDetail
	if newProgressDetail.ObjectKey != "" {
		existingProgressDetail = findStatusProgressDetail(*progressDetails, newProgressDetail.ObjectKey)
	} else {
		existingProgressDetail = findActionProgress(*progressDetails, newProgressDetail.ActionName)
	}
	if existingProgressDetail == nil {
		updateProgressDetailTime(&newProgressDetail)
		*progressDetails = append(*progressDetails, newProgressDetail)
		sendProgressDetailEvent(recorder, opsRequest, newProgressDetail)
		return
	}
	if existingProgressDetail.Status == newProgressDetail.Status &&
		existingProgressDetail.Message == newProgressDetail.Message {
		return
	}
	// if existing progress detail is 'Failed' and new progress detail is not 'Succeed', ignores the new one.
	if existingProgressDetail.Status == opsv1alpha1.FailedProgressStatus &&
		newProgressDetail.Status != opsv1alpha1.SucceedProgressStatus {
		return
	}
	existingProgressDetail.Status = newProgressDetail.Status
	existingProgressDetail.Message = newProgressDetail.Message
	existingProgressDetail.ActionTasks = newProgressDetail.ActionTasks
	updateProgressDetailTime(existingProgressDetail)
	sendProgressDetailEvent(recorder, opsRequest, newProgressDetail)
}

// findStatusProgressDetail finds the progressDetail of the specified objectKey in progressDetails.
func findStatusProgressDetail(progressDetails []opsv1alpha1.ProgressStatusDetail,
	objectKey string) *opsv1alpha1.ProgressStatusDetail {
	for i := range progressDetails {
		if progressDetails[i].ObjectKey == objectKey {
			return &progressDetails[i]
		}
	}
	return nil
}

func findActionProgress(progressDetails []opsv1alpha1.ProgressStatusDetail, actionName string) *opsv1alpha1.ProgressStatusDetail {
	for i := range progressDetails {
		if actionName == progressDetails[i].ActionName {
			return &progressDetails[i]
		}
	}
	return nil
}

// getProgressDetailEventType gets the event type with progressDetail status.
func getProgressDetailEventType(status opsv1alpha1.ProgressStatus) string {
	if status == opsv1alpha1.FailedProgressStatus {
		return corev1.EventTypeWarning
	}
	return corev1.EventTypeNormal
}

// getProgressDetailEventReason gets the event reason with progressDetail status.
func getProgressDetailEventReason(status opsv1alpha1.ProgressStatus) string {
	switch status {
	case opsv1alpha1.SucceedProgressStatus:
		return "Succeed"
	case opsv1alpha1.ProcessingProgressStatus:
		return "Processing"
	case opsv1alpha1.FailedProgressStatus:
		return "Failed"
	}
	return ""
}

// sendProgressDetailEvent sends the progress detail changed events.
func sendProgressDetailEvent(recorder record.EventRecorder,
	opsRequest *opsv1alpha1.OpsRequest,
	progressDetail opsv1alpha1.ProgressStatusDetail) {
	status := progressDetail.Status
	if status == opsv1alpha1.PendingProgressStatus {
		return
	}
	recorder.Event(opsRequest, getProgressDetailEventType(status),
		getProgressDetailEventReason(status), progressDetail.Message)
}

// updateProgressDetailTime updates the progressDetail startTime or endTime according to the status.
func updateProgressDetailTime(progressDetail *opsv1alpha1.ProgressStatusDetail) {
	if progressDetail.Status == opsv1alpha1.ProcessingProgressStatus &&
		progressDetail.StartTime.IsZero() {
		progressDetail.StartTime = metav1.NewTime(time.Now())
	}
	if isCompletedProgressStatus(progressDetail.Status) &&
		progressDetail.EndTime.IsZero() {
		progressDetail.EndTime = metav1.NewTime(time.Now())
	}
}

// handleComponentStatusProgress handles the component status progressDetails.
// if all the pods of the component are affected, use this function to reconcile the progressDetails.
func handleComponentStatusProgress(
	reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	podApplyOps func(*opsv1alpha1.OpsRequest, *corev1.Pod, *progressResource) bool) (int32, int32, error) {
	var (
		pods             []*corev1.Pod
		clusterComponent = pgRes.clusterComponent
		completedCount   int32
		err              error
	)
	if clusterComponent == nil {
		return 0, 0, nil
	}
	if pods, err = intctrlcomp.ListOwnedPods(reqCtx.Ctx, cli, opsRes.Cluster.Namespace, opsRes.Cluster.Name, pgRes.fullComponentName); err != nil {
		return 0, completedCount, err
	}
	expectReplicas := clusterComponent.Replicas
	if len(pgRes.updatedPodSet) > 0 {
		//  pods need to updated during this operation.
		var updatedPods []*corev1.Pod
		for i := range pods {
			if _, ok := pgRes.updatedPodSet[pods[i].Name]; ok {
				updatedPods = append(updatedPods, pods[i])
			}
		}
		pods = updatedPods
		expectReplicas = int32(len(pgRes.updatedPodSet))
	}
	minReadySeconds, err := intctrlcomp.GetMinReadySeconds(reqCtx.Ctx, cli, *opsRes.Cluster, pgRes.clusterComponent.Name)
	if err != nil {
		return expectReplicas, completedCount, err
	}
	if opsRes.OpsRequest.Status.Phase == opsv1alpha1.OpsCancellingPhase {
		completedCount = handleCancelProgressForPodsRollingUpdate(opsRes, pods, pgRes, compStatus, minReadySeconds, podApplyOps)
	} else {
		completedCount = handleProgressForPodsRollingUpdate(opsRes, pods, pgRes, compStatus, minReadySeconds, podApplyOps)
	}
	if opsRes.OpsRequest.Status.Phase == opsv1alpha1.OpsCancellingPhase {
		// only rollback the actual re-created pod during cancelling.
		expectReplicas = int32(len(compStatus.ProgressDetails))
	}
	return expectReplicas, completedCount, err
}

// handleProgressForPodsRollingUpdate handles the progress of pods during rolling update.
func handleProgressForPodsRollingUpdate(
	opsRes *OpsResource,
	pods []*corev1.Pod,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	minReadySeconds int32,
	podApplyOps func(*opsv1alpha1.OpsRequest, *corev1.Pod, *progressResource) bool) int32 {
	opsRequest := opsRes.OpsRequest
	var completedCount int32
	for _, v := range pods {
		objectKey := getProgressObjectKey(constant.PodKind, v.Name)
		progressDetail := opsv1alpha1.ProgressStatusDetail{ObjectKey: objectKey}
		if podProcessedSuccessful(pgRes, opsRequest, v, minReadySeconds, podApplyOps) {
			completedCount += 1
			handleSucceedProgressDetail(opsRes, pgRes, compStatus, progressDetail)
			continue
		}
		// no re-create the pod or no any changes applied in place.
		if notRecreatedDuringOperation(opsRequest.Status.StartTimestamp, v) &&
			!podApplyOps(opsRequest, v, pgRes) {
			handlePendingProgressDetail(opsRes, compStatus, progressDetail)
			continue
		}
		completedCount += handleFailedOrProcessingProgressDetail(opsRes, pgRes, compStatus, progressDetail, v)
	}
	return completedCount
}

// handleCancelProgressForPodsRollingUpdate handles the cancel progress of pods during rolling update.
func handleCancelProgressForPodsRollingUpdate(
	opsRes *OpsResource,
	pods []*corev1.Pod,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	minReadySeconds int32,
	podApplyOps func(*opsv1alpha1.OpsRequest, *corev1.Pod, *progressResource) bool) int32 {
	var newProgressDetails []opsv1alpha1.ProgressStatusDetail
	for _, v := range compStatus.ProgressDetails {
		// remove the pending progressDetail
		if v.Status != opsv1alpha1.PendingProgressStatus {
			newProgressDetails = append(newProgressDetails, v)
		}
	}
	compStatus.ProgressDetails = newProgressDetails
	pgRes.opsMessageKey = fmt.Sprintf("%s with rollback", pgRes.opsMessageKey)
	var completedCount int32
	for _, pod := range pods {
		objectKey := getProgressObjectKey(constant.PodKind, pod.Name)
		progressDetail := opsv1alpha1.ProgressStatusDetail{ObjectKey: objectKey}
		if podProcessedSuccessful(pgRes, opsRes.OpsRequest, pod, minReadySeconds, podApplyOps) {
			completedCount += 1
			handleSucceedProgressDetail(opsRes, pgRes, compStatus, progressDetail)
			continue
		}
		if notRecreatedDuringOperation(opsRes.OpsRequest.Status.CancelTimestamp, pod) &&
			!podApplyOps(opsRes.OpsRequest, pod, pgRes) {
			continue
		}
		completedCount += handleFailedOrProcessingProgressDetail(opsRes, pgRes, compStatus, progressDetail, pod)
	}
	return completedCount
}

func needToCheckRole(pgRes *progressResource) bool {
	if pgRes.componentDef == nil {
		panic("componentDef is nil")
	}
	return len(pgRes.componentDef.Spec.Roles) > 0
}

func podIsAvailable(pgRes *progressResource, pod *corev1.Pod, minReadySeconds int32) bool {
	if pod == nil {
		return false
	}
	if needToCheckRole(pgRes) {
		return intctrlutil.PodIsReadyWithLabel(*pod)
	}
	return podutils.IsPodAvailable(pod, minReadySeconds, metav1.Time{Time: time.Now()})
}

// handlePendingProgressDetail handles the pending progressDetail and sets it to progressDetails.
func handlePendingProgressDetail(opsRes *OpsResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	progressDetail opsv1alpha1.ProgressStatusDetail,
) {
	progressDetail.Status = opsv1alpha1.PendingProgressStatus
	setComponentStatusProgressDetail(opsRes.Recorder, opsRes.OpsRequest,
		&compStatus.ProgressDetails, progressDetail)
}

// handleSucceedProgressDetail handles the successful progressDetail and sets it to progressDetails.
func handleSucceedProgressDetail(opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	progressDetail opsv1alpha1.ProgressStatusDetail,
) {
	progressDetail.SetStatusAndMessage(opsv1alpha1.SucceedProgressStatus,
		getProgressSucceedMessage(pgRes.opsMessageKey, progressDetail.ObjectKey, pgRes.clusterComponent.Name))
	setComponentStatusProgressDetail(opsRes.Recorder, opsRes.OpsRequest,
		&compStatus.ProgressDetails, progressDetail)
}

// handleFailedOrProcessingProgressDetail handles failed or processing progressDetail and sets it to progressDetails.
func handleFailedOrProcessingProgressDetail(opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	progressDetail opsv1alpha1.ProgressStatusDetail,
	pod *corev1.Pod) (completedCount int32) {
	componentName := pgRes.clusterComponent.Name
	isFailed, isTimeout, _ := intctrlutil.IsPodFailedAndTimedOut(pod)
	if isFailed && isTimeout {
		podMessage := getFailedPodMessage(opsRes.Cluster, componentName, pod)
		message := getProgressFailedMessage(pgRes.opsMessageKey, progressDetail.ObjectKey, componentName, podMessage)
		progressDetail.SetStatusAndMessage(opsv1alpha1.FailedProgressStatus, message)
		completedCount = 1
	} else {
		progressDetail.SetStatusAndMessage(opsv1alpha1.ProcessingProgressStatus,
			getProgressProcessingMessage(pgRes.opsMessageKey, progressDetail.ObjectKey, componentName))
	}
	setComponentStatusProgressDetail(opsRes.Recorder, opsRes.OpsRequest,
		&compStatus.ProgressDetails, progressDetail)
	return completedCount
}

// notRecreatedDuringOperation checks if pod is re-created during the component's operation.
func notRecreatedDuringOperation(opsStartTime metav1.Time, pod *corev1.Pod) bool {
	return pod.CreationTimestamp.Before(&opsStartTime) && pod.DeletionTimestamp.IsZero()
}

// podProcessedSuccessful checks if the pod has been processed successfully:
// 1. the pod is recreated after OpsRequest.status.startTime and pod is available.
// 2. the component is running and pod is available.
func podProcessedSuccessful(pgRes *progressResource,
	opsRequest *opsv1alpha1.OpsRequest,
	pod *corev1.Pod,
	minReadySeconds int32,
	podApplyOps func(*opsv1alpha1.OpsRequest, *corev1.Pod, *progressResource) bool) bool {
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	if !podIsAvailable(pgRes, pod, minReadySeconds) {
		return false
	}
	return podApplyOps(opsRequest, pod, pgRes)
}

func getProgressProcessingMessage(opsMessageKey, objectKey, componentName string) string {
	return fmt.Sprintf("Start to %s: %s in Component: %s", opsMessageKey, objectKey, componentName)
}

func getProgressSucceedMessage(opsMessageKey, objectKey, componentName string) string {
	return fmt.Sprintf("Successfully %s: %s in Component: %s", opsMessageKey, objectKey, componentName)
}

func getProgressFailedMessage(opsMessageKey, objectKey, componentName, podMessage string) string {
	return fmt.Sprintf("Failed to %s: %s in Component: %s, message: %s", opsMessageKey, objectKey, componentName, podMessage)
}

// getFailedPodMessage gets the failed pod message from cluster component status
func getFailedPodMessage(cluster *appsv1.Cluster, componentName string, pod *corev1.Pod) string {
	clusterCompStatus := cluster.Status.Components[componentName]
	return clusterCompStatus.GetObjectMessage(constant.PodKind, pod.Name)
}

// handleComponentProgressDetails handles the component progressDetails when scale the replicas.
// @return expectProgressCount,
// @return completedCount
// @return error
func handleComponentProgressForScalingReplicas(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus) (int32, int32, error) {
	var (
		clusterComponent = pgRes.clusterComponent
		err              error
		updatedPodCount  = int32(len(pgRes.createdPodSet) + len(pgRes.deletedPodSet))
		completedCount   int32
	)
	if clusterComponent == nil {
		return 0, 0, nil
	}
	// if no any pod needs to create or delete, return
	if updatedPodCount == 0 {
		return 0, 0, nil
	}
	itsName := constant.GenerateClusterComponentName(opsRes.Cluster.Name, pgRes.fullComponentName)
	its := &workloads.InstanceSet{}
	if err = cli.Get(reqCtx.Ctx, client.ObjectKey{Name: itsName, Namespace: opsRes.OpsRequest.Namespace}, its); err != nil {
		return 0, 0, err
	}
	if len(pgRes.createdPodSet) > 0 {
		scaleOutCompletedCount, scaleOutErr := handleScaleOutProgressWithInstanceSet(opsRes, pgRes, its, compStatus)
		if scaleOutErr != nil {
			err = scaleOutErr
		}
		completedCount += scaleOutCompletedCount
	}
	if len(pgRes.deletedPodSet) > 0 {
		scaleInCompletedCount, scaleInErr := handleScaleInProgressWithInstanceSet(opsRes, pgRes, its, compStatus)
		if scaleInErr != nil {
			err = fmt.Errorf(scaleInErr.Error(), err)
		}
		completedCount += scaleInCompletedCount
	}
	return updatedPodCount, completedCount, err
}

func updateProgressDetailForHScale(
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	objectKey string, status opsv1alpha1.ProgressStatus) {
	var group string
	if pgRes.fullComponentName != "" {
		group = fmt.Sprintf("%s/%s", pgRes.fullComponentName, pgRes.opsMessageKey)
	}
	progressDetail := opsv1alpha1.ProgressStatusDetail{
		Group:     group,
		ObjectKey: objectKey,
		Status:    status,
	}
	var messagePrefix string
	switch status {
	case opsv1alpha1.SucceedProgressStatus:
		messagePrefix = "Successfully"
	case opsv1alpha1.ProcessingProgressStatus:
		messagePrefix = "Start to"
	case opsv1alpha1.PendingProgressStatus:
		messagePrefix = "wait to"
	}
	progressDetail.Message = fmt.Sprintf(`%s %s "%s" in Component: %s`,
		messagePrefix, strings.ToLower(pgRes.opsMessageKey), objectKey, pgRes.clusterComponent.Name)
	setComponentStatusProgressDetail(opsRes.Recorder, opsRes.OpsRequest,
		&compStatus.ProgressDetails, progressDetail)
}

func handleScaleOutProgressWithInstanceSet(
	opsRes *OpsResource,
	pgRes *progressResource,
	its *workloads.InstanceSet,
	compStatus *opsv1alpha1.OpsRequestComponentStatus) (completedCount int32, err error) {
	currPodRevisionMap, _ := instanceset.GetRevisions(its.Status.CurrentRevisions)
	notReadyPodSet := instanceset.GetPodNameSetFromInstanceSetCondition(its, workloads.InstanceReady)
	notAvailablePodSet := instanceset.GetPodNameSetFromInstanceSetCondition(its, workloads.InstanceAvailable)
	failurePodSet := instanceset.GetPodNameSetFromInstanceSetCondition(its, workloads.InstanceFailure)
	pgRes.opsMessageKey = "Create"
	memberStatusMap := map[string]sets.Empty{}
	if needToCheckRole(pgRes) {
		for _, v := range its.Status.MembersStatus {
			memberStatusMap[v.PodName] = sets.Empty{}
		}
	}
	for podName := range pgRes.createdPodSet {
		objectKey := getProgressObjectKey(constant.PodKind, podName)
		if _, ok := currPodRevisionMap[podName]; !ok {
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.PendingProgressStatus)
			continue
		}
		if _, ok := failurePodSet[podName]; ok {
			completedCount += 1
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.FailedProgressStatus)
			continue
		}
		if _, ok := notReadyPodSet[podName]; ok {
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.ProcessingProgressStatus)
			continue
		}
		if _, ok := notAvailablePodSet[podName]; ok {
			continue
		}
		if _, ok := memberStatusMap[podName]; !ok && needToCheckRole(pgRes) {
			continue
		}
		completedCount += 1
		updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.SucceedProgressStatus)
	}
	return completedCount, nil
}

func handleScaleInProgressWithInstanceSet(
	opsRes *OpsResource,
	pgRes *progressResource,
	its *workloads.InstanceSet,
	compStatus *opsv1alpha1.OpsRequestComponentStatus) (completedCount int32, err error) {
	currPodRevisionMap, _ := instanceset.GetRevisions(its.Status.CurrentRevisions)
	notReadyPodSet := instanceset.GetPodNameSetFromInstanceSetCondition(its, workloads.InstanceReady)
	pgRes.opsMessageKey = "Delete"
	for podName := range pgRes.deletedPodSet {
		objectKey := getProgressObjectKey(constant.PodKind, podName)
		if _, ok := currPodRevisionMap[podName]; !ok {
			completedCount += 1
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.SucceedProgressStatus)
			continue
		}
		if _, ok := notReadyPodSet[podName]; ok {
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.ProcessingProgressStatus)
			continue
		}
		updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.PendingProgressStatus)
	}
	return completedCount, nil
}

func syncProgressToOpsRequest(
	reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	oldOpsRequest *opsv1alpha1.OpsRequest,
	completedCount, expectCount int) error {
	// sync progress
	opsRes.OpsRequest.Status.Progress = fmt.Sprintf("%d/%d", completedCount, expectCount)
	if !reflect.DeepEqual(opsRes.OpsRequest.Status, oldOpsRequest.Status) {
		return cli.Status().Patch(reqCtx.Ctx, opsRes.OpsRequest, client.MergeFrom(oldOpsRequest))
	}
	return nil
}

// handleComponentProgressForScalingShards handles the component progressDetails when scaling the shards.
// @return expectProgressCount,
// @return completedCount
// @return error
func handleComponentProgressForScalingShards(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus) (int32, int32, error) {
	var (
		lastCompConfiguration = opsRes.OpsRequest.Status.LastConfiguration.Components[pgRes.compOps.GetComponentName()]
		err                   error
		completedCount        int32
		updateShards          int32
	)
	updateShards = *pgRes.shards - *lastCompConfiguration.Shards
	if updateShards > 0 {
		completedCount, err = handleScaleOutForShards(reqCtx, cli, opsRes, pgRes, compStatus)
	} else if updateShards < 0 {
		updateShards *= -1
		completedCount, err = handleScaleInForShards(reqCtx, cli, opsRes, pgRes, compStatus, updateShards)
	}
	if completedCount > updateShards {
		// completedCount may exceed updated shards if components have been rebuilt by other operations.
		completedCount = updateShards
	}
	return updateShards, completedCount, err
}

func handleScaleOutForShards(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus) (int32, error) {
	compList, err := sharding.ListShardingComponents(reqCtx.Ctx, cli, opsRes.Cluster, pgRes.compOps.GetComponentName())
	if err != nil {
		return 0, err
	}
	var completedCount int32
	for _, comp := range compList {
		if comp.CreationTimestamp.Before(&opsRes.OpsRequest.Status.StartTimestamp) {
			continue
		}
		objectKey := getProgressObjectKey(appsv1.ComponentKind, comp.Name)
		pgRes.opsMessageKey = "create"
		switch comp.Status.Phase {
		case appsv1.RunningComponentPhase:
			completedCount += 1
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.SucceedProgressStatus)
		case appsv1.FailedComponentPhase:
			completedCount += 1
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.FailedProgressStatus)
		default:
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, objectKey, opsv1alpha1.ProcessingProgressStatus)
		}
	}
	return completedCount, nil
}

func handleScaleInForShards(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	opsRes *OpsResource,
	pgRes *progressResource,
	compStatus *opsv1alpha1.OpsRequestComponentStatus,
	updateShards int32) (int32, error) {
	compList, err := sharding.ListShardingComponents(reqCtx.Ctx, cli, opsRes.Cluster, pgRes.compOps.GetComponentName())
	if err != nil {
		return 0, err
	}
	var completedCount int32
	compMap := make(map[string]struct{}, len(compList))
	for _, comp := range compList {
		objKey := getProgressObjectKey(appsv1.ComponentKind, comp.Name)
		compMap[objKey] = struct{}{}
		if comp.DeletionTimestamp.IsZero() || comp.DeletionTimestamp.Before(&opsRes.OpsRequest.Status.StartTimestamp) {
			continue
		}
		pgRes.opsMessageKey = "delete"
		updateProgressDetailForHScale(opsRes, pgRes, compStatus, objKey, opsv1alpha1.ProcessingProgressStatus)
	}
	for i := range compStatus.ProgressDetails {
		progressDetail := &compStatus.ProgressDetails[i]
		if _, ok := compMap[progressDetail.ObjectKey]; !ok {
			completedCount += 1
			updateProgressDetailForHScale(opsRes, pgRes, compStatus, progressDetail.ObjectKey, opsv1alpha1.SucceedProgressStatus)
		}
	}
	if int32(len(compList)) == *pgRes.compOps.(opsv1alpha1.HorizontalScaling).Shards {
		completedCount = updateShards
	}
	return completedCount, nil
}
