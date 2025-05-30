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

package parameters

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/apecloud/kubeblocks/apis/apps/v1alpha1"
	parametersv1alpha1 "github.com/apecloud/kubeblocks/apis/parameters/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/configuration/core"
	"github.com/apecloud/kubeblocks/pkg/constant"
	"github.com/apecloud/kubeblocks/pkg/controller/model"
	"github.com/apecloud/kubeblocks/pkg/controller/multicluster"
	"github.com/apecloud/kubeblocks/pkg/controller/render"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

// ReconfigureReconciler reconciles a ReconfigureRequest object
type ReconfigureReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

const (
	ConfigReconcileInterval = time.Second * 1
)

const (
	configurationNoChangedMessage           = "the configuration file has not been modified, skip reconfigure"
	configurationNotUsingMessage            = "the configmap is not used by any container, skip reconfigure"
	configurationNotRelatedComponentMessage = "related component does not found any configSpecs, skip reconfigure"
)

var reconfigureRequiredLabels = []string{
	constant.AppInstanceLabelKey,
	constant.KBAppComponentLabelKey,
	constant.CMConfigurationTemplateNameLabelKey,
	constant.CMConfigurationTypeLabelKey,
	constant.CMConfigurationSpecProviderLabelKey,
}

// +kubebuilder:rbac:groups=core,resources=configmap,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmap/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ReconfigureRequest object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *ReconfigureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqCtx := intctrlutil.RequestCtx{
		Ctx:      ctx,
		Req:      req,
		Log:      log.FromContext(ctx).WithName("ReconfigureRequestReconcile").WithValues("ConfigMap", req.NamespacedName),
		Recorder: r.Recorder,
	}

	config := &corev1.ConfigMap{}
	// TODO(leon): data or universal?
	if err := r.Client.Get(reqCtx.Ctx, reqCtx.Req.NamespacedName, config, inDataContextUnspecified()); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}
	if model.IsObjectDeleting(config) {
		return intctrlutil.Reconciled()
	}
	if !checkConfigurationObject(config) {
		return intctrlutil.Reconciled()
	}

	reqCtx.Log = reqCtx.Log.
		WithValues("ClusterName", config.Labels[constant.AppInstanceLabelKey]).
		WithValues("ComponentName", config.Labels[constant.KBAppComponentLabelKey])

	isAppliedConfigs, err := checkAndApplyConfigsChanged(r.Client, reqCtx, config)
	if err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log,
			errors.Wrap(err, "failed to check last-applied-configuration").Error())
	}
	if isAppliedConfigs {
		return updateConfigPhase(r.Client, reqCtx, config, parametersv1alpha1.CFinishedPhase, configurationNoChangedMessage)
	}

	resources, err := prepareRelatedResource(reqCtx, r.Client, config)
	if err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log,
			errors.Wrap(err, "failed to fetch related resources").Error())
	}
	if resources.configSpec == nil {
		reqCtx.Log.Info(fmt.Sprintf("not found configSpec[%s] in the component[%s].",
			config.Labels[constant.CMConfigurationSpecProviderLabelKey], resources.componentName))
		reqCtx.Recorder.Event(config,
			corev1.EventTypeWarning,
			appsv1alpha1.ReasonReconfigureFailed,
			configurationNotRelatedComponentMessage)
		return updateConfigPhase(r.Client, reqCtx, config, parametersv1alpha1.CFinishedPhase, configurationNotRelatedComponentMessage)
	}

	return r.sync(reqCtx, config, resources)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReconfigureReconciler) SetupWithManager(mgr ctrl.Manager, multiClusterMgr multicluster.Manager) error {
	b := intctrlutil.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: int(math.Ceil(viper.GetFloat64(constant.CfgKBReconcileWorkers) / 4)),
		})

	if multiClusterMgr != nil {
		eventHandler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Namespace: obj.GetNamespace(),
						Name:      obj.GetName(),
					},
				},
			}
		})
		multiClusterMgr.Watch(b, &corev1.ConfigMap{}, eventHandler)
	}

	return b.WithEventFilter(predicate.NewPredicateFuncs(checkConfigurationObject)).Complete(r)
}

func checkConfigurationObject(object client.Object) bool {
	return checkConfigLabels(object, reconfigureRequiredLabels)
}

func (r *ReconfigureReconciler) sync(reqCtx intctrlutil.RequestCtx, configMap *corev1.ConfigMap, resources *reconfigureRelatedResource) (ctrl.Result, error) {
	rctx := newParameterReconcileContext(reqCtx,
		&render.ResourceCtx{
			Context:       reqCtx.Ctx,
			Client:        r.Client,
			Namespace:     configMap.Namespace,
			ClusterName:   resources.clusterName,
			ComponentName: resources.componentName,
		},
		configMap,
		nil,
		resources.configSpec.Name,
		resources.componentMatchLabels())
	if err := rctx.GetRelatedObjects(); err != nil {
		return intctrlutil.RequeueWithErrorAndRecordEvent(configMap, r.Recorder, err, reqCtx.Log)
	}

	// Assumption: It is required that the cluster must have a component.
	if rctx.ClusterComObj == nil {
		reqCtx.Log.Info("not found component.")
		return intctrlutil.Reconciled()
	}

	if len(rctx.InstanceSetList) == 0 {
		reqCtx.Recorder.Event(configMap, corev1.EventTypeWarning, appsv1alpha1.ReasonReconfigureFailed,
			"the configmap is not used by any container, skip reconfigure")
		return updateConfigPhase(r.Client, reqCtx, configMap, parametersv1alpha1.CFinishedPhase, configurationNotUsingMessage)
	}

	configPatch, forceRestart, err := createConfigPatch(configMap, rctx.ConfigRender, rctx.ParametersDefs)
	if err != nil {
		return intctrlutil.RequeueWithErrorAndRecordEvent(configMap, r.Recorder, err, reqCtx.Log)
	}

	// No parameters updated
	if configPatch != nil && !configPatch.IsModify {
		reqCtx.Recorder.Event(configMap, corev1.EventTypeNormal, appsv1alpha1.ReasonReconfigureRunning, "nothing changed, skip reconfigure")
		return r.updateConfigCMStatus(reqCtx, configMap, core.ReconfigureNoChangeType, nil)
	}

	if configPatch != nil {
		reqCtx.Log.V(1).Info(fmt.Sprintf(
			"reconfigure params: \n\tadd: %s\n\tdelete: %s\n\tupdate: %s",
			configPatch.AddConfig,
			configPatch.DeleteConfig,
			configPatch.UpdateConfig))
	}

	tasks, err := genReconfigureActionTasks(resources.configSpec, rctx, configPatch, forceRestart)
	if err != nil {
		return intctrlutil.RequeueWithErrorAndRecordEvent(configMap, r.Recorder, err, reqCtx.Log)
	}
	return r.performUpgrade(rctx, tasks)
}

func (r *ReconfigureReconciler) updateConfigCMStatus(reqCtx intctrlutil.RequestCtx, cfg *corev1.ConfigMap, reconfigureType string, result *intctrlutil.Result) (ctrl.Result, error) {
	configData, err := json.Marshal(cfg.Data)
	if err != nil {
		return intctrlutil.RequeueWithErrorAndRecordEvent(cfg, r.Recorder, err, reqCtx.Log)
	}

	if ok, err := updateAppliedConfigs(r.Client, reqCtx, cfg, configData, reconfigureType, result); err != nil || !ok {
		return intctrlutil.RequeueAfter(ConfigReconcileInterval, reqCtx.Log, "failed to patch status and retry...", "error", err)
	}

	return intctrlutil.Reconciled()
}

func (r *ReconfigureReconciler) performUpgrade(rctx *ReconcileContext, reloadTasks []ReloadAction) (ctrl.Result, error) {
	var err error
	var returnedStatus ReturnedStatus
	var reloadType string

	for _, task := range reloadTasks {
		reloadType = task.ReloadType()
		returnedStatus, err = task.ExecReload()
		if err != nil || returnedStatus.Status != ESNone {
			return r.status(rctx, returnedStatus, reloadType, err)
		}
	}
	return r.succeed(rctx, reloadType, returnedStatus)
}

func (r *ReconfigureReconciler) status(rctx *ReconcileContext, returnedStatus ReturnedStatus, policy string, err error) (ctrl.Result, error) {
	updatePhase := func(phase parametersv1alpha1.ParameterPhase, options ...options) (ctrl.Result, error) {
		return updateConfigPhaseWithResult(rctx.Client, rctx.RequestCtx, rctx.ConfigMap, reconciled(returnedStatus, policy, phase, options...))
	}

	switch returnedStatus.Status {
	case ESFailedAndRetry:
		return updatePhase(parametersv1alpha1.CFailedPhase, withFailed(err, true))
	case ESRetry:
		return updatePhase(parametersv1alpha1.CUpgradingPhase)
	case ESFailed:
		return updatePhase(parametersv1alpha1.CFailedAndPausePhase, withFailed(err, false))
	case ESNone:
		return r.succeed(rctx, policy, returnedStatus)
	default:
		return updatePhase(parametersv1alpha1.CFailedAndPausePhase, withFailed(core.MakeError("unknown status"), false))
	}
}

func (r *ReconfigureReconciler) succeed(rctx *ReconcileContext, reloadType string, returnedStatus ReturnedStatus) (ctrl.Result, error) {
	rctx.Recorder.Eventf(rctx.ConfigMap,
		corev1.EventTypeNormal,
		appsv1alpha1.ReasonReconfigureSucceed,
		"the reconfigure[%s] has been processed successfully",
		reloadType)

	result := reconciled(returnedStatus, reloadType, parametersv1alpha1.CFinishedPhase)
	return r.updateConfigCMStatus(rctx.RequestCtx, rctx.ConfigMap, reloadType, &result)
}
