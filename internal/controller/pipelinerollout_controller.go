/*
Copyright 2023.

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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	numaflowv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	"github.com/numaproj/numaplane/internal/common"
	"github.com/numaproj/numaplane/internal/controller/config"
	"github.com/numaproj/numaplane/internal/usde"
	"github.com/numaproj/numaplane/internal/util"
	"github.com/numaproj/numaplane/internal/util/kubernetes"
	"github.com/numaproj/numaplane/internal/util/logger"
	"github.com/numaproj/numaplane/internal/util/metrics"
	apiv1 "github.com/numaproj/numaplane/pkg/apis/numaplane/v1alpha1"
)

const (
	ControllerPipelineRollout = "pipeline-rollout-controller"
	loggerName                = "pipelinerollout-reconciler"
	numWorkers                = 16 // can consider making configurable
)

var (
	pipelineROReconciler *PipelineRolloutReconciler
	initTime             time.Time
)

// PipelineSpec keeps track of minimum number of fields we need to know about
type PipelineSpec struct {
	InterStepBufferServiceName string    `json:"interStepBufferServiceName"`
	Lifecycle                  Lifecycle `json:"lifecycle,omitempty"`
}

func (pipeline PipelineSpec) getISBSvcName() string {
	if pipeline.InterStepBufferServiceName == "" {
		return "default"
	}
	return pipeline.InterStepBufferServiceName
}

type Lifecycle struct {
	// DesiredPhase used to bring the pipeline from current phase to desired phase
	// +kubebuilder:default=Running
	// +optional
	DesiredPhase string `json:"desiredPhase,omitempty"`
}

// PipelineRolloutReconciler reconciles a PipelineRollout object
type PipelineRolloutReconciler struct {
	client client.Client
	scheme *runtime.Scheme

	// queue contains the list of PipelineRollouts that currently need to be reconciled
	// both PipelineRolloutReconciler.Reconcile() and other Rollout reconcilers can add PipelineRollouts to this queue to be processed as needed
	// a set of Workers is used to process this queue
	queue workqueue.TypedRateLimitingInterface[interface{}]
	// shutdownWorkerWaitGroup is used when shutting down the workers processing the queue for them to indicate that they're done
	shutdownWorkerWaitGroup *sync.WaitGroup
	// customMetrics is used to generate the custom metrics for the Pipeline
	customMetrics *metrics.CustomMetrics
	// the recorder is used to record events
	recorder record.EventRecorder

	// maintain inProgressStrategies in memory and in PipelineRollout Status
	inProgressStrategyMgr *inProgressStrategyMgr
}

func NewPipelineRolloutReconciler(
	c client.Client,
	s *runtime.Scheme,
	customMetrics *metrics.CustomMetrics,
	recorder record.EventRecorder,
) *PipelineRolloutReconciler {

	numaLogger := logger.GetBaseLogger().WithName(loggerName)
	// update the context with this Logger so downstream users can incorporate these values in the logs
	ctx := logger.WithLogger(context.Background(), numaLogger)

	// create a queue to process PipelineRollout reconciliations
	// the benefit of the queue is that other reconciliation code can also add PipelineRollouts to it so they'll be processed
	pipelineRolloutQueue := util.NewWorkQueue("pipeline_rollout_queue")

	r := &PipelineRolloutReconciler{
		c,
		s,
		pipelineRolloutQueue,
		&sync.WaitGroup{},
		customMetrics,
		recorder,
		nil, // defined below
	}
	pipelineROReconciler = r

	pipelineROReconciler.inProgressStrategyMgr = newInProgressStrategyMgr(
		// getRolloutStrategy function:
		func(ctx context.Context, rollout client.Object) *apiv1.UpgradeStrategy {
			pipelineRollout := rollout.(*apiv1.PipelineRollout)

			if pipelineRollout.Status.UpgradeInProgress != "" {
				return (*apiv1.UpgradeStrategy)(&pipelineRollout.Status.UpgradeInProgress)
			} else {
				return nil
			}
		},
		// setRolloutStrategy function:
		func(ctx context.Context, rollout client.Object, strategy apiv1.UpgradeStrategy) {
			pipelineRollout := rollout.(*apiv1.PipelineRollout)
			pipelineRollout.Status.SetUpgradeInProgress(strategy)
		},
	)

	r.runWorkers(ctx)

	return r
}

//+kubebuilder:rbac:groups=numaplane.numaproj.io,resources=pipelinerollouts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=numaplane.numaproj.io,resources=pipelinerollouts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=numaplane.numaproj.io,resources=pipelinerollouts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *PipelineRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	numaLogger := logger.GetBaseLogger().WithName(loggerName).WithValues("pipelinerollout", req.NamespacedName)
	r.enqueuePipeline(req.NamespacedName)
	numaLogger.Debugf("PipelineRollout Reconciler added PipelineRollout %v to queue", req.NamespacedName)
	r.customMetrics.PipelineRolloutQueueLength.WithLabelValues().Set(float64(r.queue.Len()))
	return ctrl.Result{}, nil
}

func (r *PipelineRolloutReconciler) enqueuePipeline(namespacedName k8stypes.NamespacedName) {
	key := namespacedNameToKey(namespacedName)
	r.queue.Add(key)
}

func (r *PipelineRolloutReconciler) processPipelineRollout(ctx context.Context, namespacedName k8stypes.NamespacedName) (ctrl.Result, error) {
	syncStartTime := time.Now()
	numaLogger := logger.FromContext(ctx).WithValues("pipelinerollout", namespacedName)
	// update the context with this Logger so downstream users can incorporate these values in the logs
	ctx = logger.WithLogger(ctx, numaLogger)
	r.customMetrics.PipelineROSyncs.WithLabelValues().Inc()

	// Get PipelineRollout CR
	pipelineRollout := &apiv1.PipelineRollout{}
	if err := r.client.Get(ctx, namespacedName, pipelineRollout); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		} else {
			r.ErrorHandler(pipelineRollout, err, "GetPipelineRolloutFailed", "Failed to get PipelineRollout")
			return ctrl.Result{}, err
		}
	}

	// save off a copy of the original before we modify it
	pipelineRolloutOrig := pipelineRollout
	pipelineRollout = pipelineRolloutOrig.DeepCopy()

	pipelineRollout.Status.Init(pipelineRollout.Generation)

	requeue, existingPipelineDef, err := r.reconcile(ctx, pipelineRollout, syncStartTime)
	if err != nil {
		r.ErrorHandler(pipelineRollout, err, "ReconcileFailed", "Failed to reconcile PipelineRollout")
		statusUpdateErr := r.updatePipelineRolloutStatusToFailed(ctx, pipelineRollout, err)
		if statusUpdateErr != nil {
			r.ErrorHandler(pipelineRollout, statusUpdateErr, "UpdateStatusFailed", "Failed to update PipelineRollout status")
			return ctrl.Result{}, statusUpdateErr
		}

		return ctrl.Result{}, err
	}

	// Update PipelineRollout Status based on child resource (Pipeline) Status
	err = r.processPipelineStatus(ctx, pipelineRollout, existingPipelineDef)
	if err != nil {
		r.ErrorHandler(pipelineRollout, err, "ProcessPipelineStatusFailed", "Failed to process Pipeline Status")
		statusUpdateErr := r.updatePipelineRolloutStatusToFailed(ctx, pipelineRollout, err)
		if statusUpdateErr != nil {
			r.ErrorHandler(pipelineRollout, statusUpdateErr, "UpdateStatusFailed", "Failed to update PipelineRollout status")
			return ctrl.Result{}, statusUpdateErr
		}

		return ctrl.Result{}, err
	}

	// Update the Spec if needed
	if r.needsUpdate(pipelineRolloutOrig, pipelineRollout) {
		pipelineRolloutStatus := pipelineRollout.Status
		if err := r.client.Update(ctx, pipelineRollout); err != nil {
			r.ErrorHandler(pipelineRollout, err, "UpdateFailed", "Failed to update PipelineRollout")
			statusUpdateErr := r.updatePipelineRolloutStatusToFailed(ctx, pipelineRollout, err)
			if statusUpdateErr != nil {
				r.ErrorHandler(pipelineRollout, statusUpdateErr, "UpdateStatusFailed", "Failed to update PipelineRollout status")
				return ctrl.Result{}, statusUpdateErr
			}
			return ctrl.Result{}, err
		}
		// restore the original status, which would've been wiped in the previous call to Update()
		pipelineRollout.Status = pipelineRolloutStatus
	}

	// Update the Status subresource
	if pipelineRollout.DeletionTimestamp.IsZero() { // would've already been deleted
		statusUpdateErr := r.updatePipelineRolloutStatus(ctx, pipelineRollout)
		if statusUpdateErr != nil {
			r.ErrorHandler(pipelineRollout, statusUpdateErr, "UpdateStatusFailed", "Failed to update PipelineRollout status")
			return ctrl.Result{}, statusUpdateErr
		}
	}

	// generate the metrics for the Pipeline.
	r.customMetrics.IncPipelineROsRunning(pipelineRollout.Name, pipelineRollout.Namespace)

	if requeue {
		return ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}, nil
	}

	r.recorder.Eventf(pipelineRollout, "Normal", "ReconcileSuccess", "Reconciliation successful")
	numaLogger.Debug("reconciliation successful")

	return ctrl.Result{}, nil
}

func (r *PipelineRolloutReconciler) Shutdown(ctx context.Context) {
	numaLogger := logger.FromContext(ctx)

	numaLogger.Info("shutting down PipelineRollout queue")
	r.queue.ShutDown()

	// wait for all the workers to have stopped
	r.shutdownWorkerWaitGroup.Wait()
}

// runWorkers starts up the workers processing the queue of PipelineRollouts
func (r *PipelineRolloutReconciler) runWorkers(ctx context.Context) {

	for i := 0; i < numWorkers; i++ {
		r.shutdownWorkerWaitGroup.Add(numWorkers)
		go r.runWorker(ctx)
	}
}

// runWorker starts up one of the workers processing the queue of PipelineRollouts
func (r *PipelineRolloutReconciler) runWorker(ctx context.Context) {
	numaLogger := logger.FromContext(ctx)

	for {
		key, quit := r.queue.Get()
		if quit {
			numaLogger.Info("PipelineRollout worker done")
			r.shutdownWorkerWaitGroup.Done()
			return
		}
		r.processQueueKey(ctx, key.(string))
		r.queue.Done(key)
	}

}

func (r *PipelineRolloutReconciler) processQueueKey(ctx context.Context, key string) {
	numaLogger := logger.FromContext(ctx).WithValues("key", key)
	// update the context with this Logger so downstream users can incorporate these values in the logs
	ctx = logger.WithLogger(ctx, numaLogger)

	// get namespace/name from key
	namespacedName, err := keyToNamespacedName(key)
	if err != nil {
		numaLogger.Fatal(err, "Queue key not derivable")
	}

	numaLogger.Debugf("processing PipelineRollout %v", namespacedName)
	result, err := r.processPipelineRollout(ctx, namespacedName)

	// based on result, may need to add this back to the queue
	if err != nil {
		numaLogger.Errorf(err, "PipelineRollout %v reconcile returned error: %v", namespacedName, err)
		r.queue.AddRateLimited(key)
	} else {
		if result.Requeue {
			numaLogger.Debugf("PipelineRollout %v reconcile requests requeue", namespacedName)
			r.queue.AddRateLimited(key)
		} else if result.RequeueAfter > 0 {
			numaLogger.Debugf("PipelineRollout %v reconcile requests requeue after %d seconds", namespacedName, result.RequeueAfter)
			r.queue.AddAfter(key, result.RequeueAfter)
		} else {
			numaLogger.Debugf("PipelineRollout %v reconcile complete", namespacedName)
		}
	}
}

func keyToNamespacedName(key string) (k8stypes.NamespacedName, error) {
	index := strings.Index(key, "/")
	if index < 0 {
		return k8stypes.NamespacedName{}, fmt.Errorf("improperly formatted key: %q", key)
	}
	return k8stypes.NamespacedName{Namespace: key[0:index], Name: key[index+1:]}, nil
}

func namespacedNameToKey(namespacedName k8stypes.NamespacedName) string {
	return fmt.Sprintf("%s/%s", namespacedName.Namespace, namespacedName.Name)
}

// reconcile does the real logic, it returns true if the event
// needs to be re-queued.
func (r *PipelineRolloutReconciler) reconcile(
	ctx context.Context,
	pipelineRollout *apiv1.PipelineRollout,
	syncStartTime time.Time,
) (bool, *kubernetes.GenericObject, error) {
	numaLogger := logger.FromContext(ctx)
	defer func() {
		if pipelineRollout.Status.IsHealthy() {
			r.customMetrics.PipelinesRolloutHealth.WithLabelValues(pipelineRollout.Namespace, pipelineRollout.Name).Set(1)
		} else {
			r.customMetrics.PipelinesRolloutHealth.WithLabelValues(pipelineRollout.Namespace, pipelineRollout.Name).Set(0)
		}
	}()

	// is PipelineRollout being deleted? need to remove the finalizer, so it can
	// (OwnerReference will delete the underlying Pipeline through Cascading deletion)
	if !pipelineRollout.DeletionTimestamp.IsZero() {
		numaLogger.Info("Deleting PipelineRollout")
		if controllerutil.ContainsFinalizer(pipelineRollout, finalizerName) {
			controllerutil.RemoveFinalizer(pipelineRollout, finalizerName)
		}
		// generate the metrics for the Pipeline deletion.
		r.customMetrics.DecPipelineROsRunning(pipelineRollout.Name, pipelineRollout.Namespace)
		r.customMetrics.ReconciliationDuration.WithLabelValues(ControllerPipelineRollout, "delete").Observe(time.Since(syncStartTime).Seconds())
		r.customMetrics.PipelinesRolloutHealth.DeleteLabelValues(pipelineRollout.Namespace, pipelineRollout.Name)
		return false, nil, nil
	}

	// add Finalizer so we can ensure that we take appropriate action when CRD is deleted
	if !controllerutil.ContainsFinalizer(pipelineRollout, finalizerName) {
		controllerutil.AddFinalizer(pipelineRollout, finalizerName)
	}

	newPipelineDef, err := r.makeRunningPipelineDefinition(ctx, pipelineRollout)
	if err != nil {
		return false, nil, err
	}

	// Get the object to see if it exists
	existingPipelineDef, err := kubernetes.GetResource(ctx, r.client, newPipelineDef.GroupVersionKind(),
		k8stypes.NamespacedName{Name: newPipelineDef.Name, Namespace: newPipelineDef.Namespace})
	if err != nil {
		// create object as it doesn't exist
		if apierrors.IsNotFound(err) {
			numaLogger.Debugf("Pipeline %s/%s doesn't exist so creating", pipelineRollout.Namespace, pipelineRollout.Name)
			pipelineRollout.Status.MarkPending()

			err = kubernetes.CreateResource(ctx, r.client, newPipelineDef)
			if err != nil {
				return false, nil, err
			}
			pipelineRollout.Status.MarkDeployed(pipelineRollout.Generation)
			r.customMetrics.ReconciliationDuration.WithLabelValues(ControllerPipelineRollout, "create").Observe(time.Since(syncStartTime).Seconds())
			return false, existingPipelineDef, nil
		}

		return false, existingPipelineDef, fmt.Errorf("error getting Pipeline: %v", err)
	}

	// Object already exists
	// if Pipeline is not owned by Rollout, fail and return
	if !checkOwnerRef(existingPipelineDef.OwnerReferences, pipelineRollout.UID) {
		errStr := fmt.Sprintf("Pipeline %s already exists in namespace, not owned by a PipelineRollout", existingPipelineDef.Name)
		numaLogger.Debugf("PipelineRollout %s failed because %s", pipelineRollout.Name, errStr)
		return false, existingPipelineDef, errors.New(errStr)
	}
	newPipelineDef, err = r.merge(existingPipelineDef, newPipelineDef)
	if err != nil {
		return false, nil, err
	}
	err = r.processExistingPipeline(ctx, pipelineRollout, existingPipelineDef, newPipelineDef, syncStartTime)
	return false, existingPipelineDef, err
}

// determine if this Pipeline is owned by this PipelineRollout
func checkOwnerRef(ownerRefs []metav1.OwnerReference, uid k8stypes.UID) bool {
	// no owners
	if len(ownerRefs) == 0 {
		return false
	}
	for _, ref := range ownerRefs {
		if ref.Kind == "PipelineRollout" && ref.UID == uid {
			return true
		}
	}
	return false
}

// take the existing pipeline and merge anything needed from the new pipeline definition
func (r *PipelineRolloutReconciler) merge(existingPipeline *kubernetes.GenericObject, newPipeline *kubernetes.GenericObject) (*kubernetes.GenericObject, error) {
	resultPipeline := existingPipeline.DeepCopy()
	resultPipeline.Spec = *newPipeline.Spec.DeepCopy()
	if resultPipeline.Labels == nil {
		resultPipeline.Labels = map[string]string{}
	}
	for key, value := range newPipeline.Labels {
		resultPipeline.Labels[key] = value
	}
	if resultPipeline.Annotations == nil {
		resultPipeline.Annotations = map[string]string{}
	}
	for key, value := range newPipeline.Annotations {
		resultPipeline.Annotations[key] = value
	}
	return resultPipeline, nil
}

func (r *PipelineRolloutReconciler) processExistingPipeline(ctx context.Context, pipelineRollout *apiv1.PipelineRollout,
	existingPipelineDef *kubernetes.GenericObject, newPipelineDef *kubernetes.GenericObject, syncStartTime time.Time) error {

	numaLogger := logger.FromContext(ctx)

	// what is the preferred strategy for this namespace?
	userPreferredStrategy, err := usde.GetUserStrategy(ctx, newPipelineDef.Namespace)
	if err != nil {
		return err
	}

	// does the Resource need updating, and if so how?
	pipelineNeedsToUpdate, upgradeStrategyType, err := usde.ResourceNeedsUpdating(ctx, newPipelineDef, existingPipelineDef)
	if err != nil {
		return err
	}
	numaLogger.
		WithValues("pipelineNeedsToUpdate", pipelineNeedsToUpdate, "upgradeStrategyType", upgradeStrategyType).
		Debug("Upgrade decision result")

	// set the Status appropriately to "Pending" or "Deployed" depending on whether pipeline needs to update
	if pipelineNeedsToUpdate {
		pipelineRollout.Status.MarkPending()
	} else {
		pipelineRollout.Status.MarkDeployed(pipelineRollout.Generation)
	}

	// is there currently an inProgressStrategy for the pipeline? (This will override any new decision)
	inProgressStrategy := r.inProgressStrategyMgr.getStrategy(ctx, pipelineRollout)
	numaLogger.Debugf("current inProgressStrategy=%s", inProgressStrategy)
	inProgressStrategySet := (inProgressStrategy != apiv1.UpgradeStrategyNoOp)

	// if not, should we set one?
	if !inProgressStrategySet {
		if userPreferredStrategy == config.PPNDStrategyID {
			// if the preferred strategy is PPND, do we need to start the process for PPND (if we haven't already)?
			needPPND := false
			ppndRequired, err := r.needPPND(ctx, pipelineRollout, newPipelineDef, upgradeStrategyType == apiv1.UpgradeStrategyPPND)
			if err != nil {
				return err
			}
			if ppndRequired == nil { // not enough information
				// TODO: mark something in the Status for why we're remaining in "Pending" here
				return nil
			}
			needPPND = *ppndRequired
			if needPPND {
				inProgressStrategy = apiv1.UpgradeStrategyPPND
				r.inProgressStrategyMgr.setStrategy(ctx, pipelineRollout, inProgressStrategy)
			}
		}
		if userPreferredStrategy == config.ProgressiveStrategyID {
			if upgradeStrategyType == apiv1.UpgradeStrategyProgressive {
				inProgressStrategy = apiv1.UpgradeStrategyProgressive
				r.inProgressStrategyMgr.setStrategy(ctx, pipelineRollout, inProgressStrategy)
			}
		}
	}

	// don't risk out-of-date cache while performing PPND or Progressive strategy - get
	// the most current version of the Pipeline just in case
	if inProgressStrategy != apiv1.UpgradeStrategyNoOp {
		existingPipelineDef, err = kubernetes.GetLiveResource(ctx, newPipelineDef, "pipelines")
		if err != nil {
			if apierrors.IsNotFound(err) {
				numaLogger.WithValues("pipelineDefinition", *newPipelineDef).Warn("Pipeline not found.")
			} else {
				return fmt.Errorf("error getting Pipeline for status processing: %v", err)
			}
		}
		newPipelineDef, err = r.merge(existingPipelineDef, newPipelineDef)
		if err != nil {
			return err
		}
	}

	// now do whatever the inProgressStrategy is
	switch inProgressStrategy {
	case apiv1.UpgradeStrategyPPND:
		numaLogger.Debug("processing pipeline with PPND")
		done, err := r.processExistingPipelineWithPPND(ctx, pipelineRollout, existingPipelineDef, newPipelineDef)
		if err != nil {
			return err
		}
		if done {
			r.inProgressStrategyMgr.unsetStrategy(ctx, pipelineRollout)
		}

	case apiv1.UpgradeStrategyProgressive:
		if pipelineNeedsToUpdate {
			numaLogger.Debug("processing pipeline with Progressive")
			done, err := processResourceWithProgressive(ctx, pipelineRollout, existingPipelineDef, r, r.client)
			if err != nil {
				return err
			}
			if done {
				r.inProgressStrategyMgr.unsetStrategy(ctx, pipelineRollout)
			}
		}
	default:
		if pipelineNeedsToUpdate && upgradeStrategyType == apiv1.UpgradeStrategyApply {
			if err := updatePipelineSpec(ctx, r.client, newPipelineDef); err != nil {
				return err
			}
			pipelineRollout.Status.MarkDeployed(pipelineRollout.Generation)
		}
	}
	// clean up recyclable pipelines
	err = garbageCollectChildren(ctx, pipelineRollout, r, r.client)
	if err != nil {
		return err
	}

	if pipelineNeedsToUpdate {
		r.customMetrics.ReconciliationDuration.WithLabelValues(ControllerPipelineRollout, "update").Observe(time.Since(syncStartTime).Seconds())
	}
	return nil
}
func pipelineObservedGenerationCurrent(generation int64, observedGeneration int64) bool {
	return generation <= observedGeneration
}

// Set the Condition in the Status for child resource health

func (r *PipelineRolloutReconciler) processPipelineStatus(ctx context.Context, pipelineRollout *apiv1.PipelineRollout, existingPipelineDef *kubernetes.GenericObject) error {
	numaLogger := logger.FromContext(ctx)

	// Only fetch the latest pipeline object while deleting the pipeline object, i.e. when pipelineRollout.DeletionTimestamp.IsZero() is false
	if existingPipelineDef == nil {
		pipelineDef, err := r.makeRunningPipelineDefinition(ctx, pipelineRollout)
		if err != nil {
			return err
		}
		livePipelineDef, err := kubernetes.GetLiveResource(ctx, pipelineDef, "pipelines")
		if err != nil {
			if apierrors.IsNotFound(err) {
				numaLogger.WithValues("pipelineDefinition", *pipelineDef).Warn("Pipeline not found. Unable to process status during this reconciliation.")
				return nil
			} else {
				return fmt.Errorf("error getting Pipeline for status processing: %v", err)
			}
		}
		existingPipelineDef = livePipelineDef
	}

	pipelineStatus, err := kubernetes.ParseStatus(existingPipelineDef)
	if err != nil {
		return fmt.Errorf("failed to parse Pipeline Status from pipeline CR: %+v, %v", existingPipelineDef, err)
	}

	numaLogger.Debugf("pipeline status: %v", pipelineStatus)

	r.setChildResourcesHealthCondition(pipelineRollout, existingPipelineDef, &pipelineStatus)
	r.setChildResourcesPauseCondition(pipelineRollout, existingPipelineDef, &pipelineStatus)

	return nil
}

func (r *PipelineRolloutReconciler) setChildResourcesHealthCondition(pipelineRollout *apiv1.PipelineRollout, pipeline *kubernetes.GenericObject, pipelineStatus *kubernetes.GenericStatus) {

	pipelinePhase := numaflowv1.PipelinePhase(pipelineStatus.Phase)
	pipelineChildResourceStatus, pipelineChildResourceReason := getPipelineChildResourceHealth(pipelineStatus.Conditions)

	if pipelineChildResourceReason == "Progressing" || !pipelineObservedGenerationCurrent(pipeline.Generation, pipelineStatus.ObservedGeneration) {
		pipelineRollout.Status.MarkChildResourcesUnhealthy("Progressing", "Pipeline Progressing", pipelineRollout.Generation)
	} else if pipelinePhase == numaflowv1.PipelinePhaseFailed {
		pipelineRollout.Status.MarkChildResourcesUnhealthy("PipelineFailed", "Pipeline Phase=Failed", pipelineRollout.Generation)
	} else if pipelineChildResourceStatus == "False" {
		pipelineRollout.Status.MarkChildResourcesUnhealthy("PipelineFailed", "Pipeline Failed, Pipeline Child Resource(s) Unhealthy", pipelineRollout.Generation)
	} else if pipelinePhase == numaflowv1.PipelinePhasePaused || pipelinePhase == numaflowv1.PipelinePhasePausing {
		pipelineRollout.Status.MarkChildResourcesHealthUnknown("PipelineUnknown", "Pipeline Pausing - health unknown", pipelineRollout.Generation)
	} else if pipelinePhase == numaflowv1.PipelinePhaseDeleting {
		pipelineRollout.Status.MarkChildResourcesUnhealthy("PipelineDeleting", "Pipeline Deleting", pipelineRollout.Generation)
	} else if pipelinePhase == numaflowv1.PipelinePhaseUnknown || pipelineChildResourceStatus == "Unknown" {
		pipelineRollout.Status.MarkChildResourcesHealthUnknown("PipelineUnknown", "Pipeline Phase Unknown", pipelineRollout.Generation)
	} else {
		pipelineRollout.Status.MarkChildResourcesHealthy(pipelineRollout.Generation)
	}

}

func (r *PipelineRolloutReconciler) setChildResourcesPauseCondition(pipelineRollout *apiv1.PipelineRollout, pipeline *kubernetes.GenericObject, pipelineStatus *kubernetes.GenericStatus) {

	pipelinePhase := numaflowv1.PipelinePhase(pipelineStatus.Phase)

	if pipelinePhase == numaflowv1.PipelinePhasePaused || pipelinePhase == numaflowv1.PipelinePhasePausing {
		// if BeginTime hasn't been set yet, we must have just started pausing - set it
		if pipelineRollout.Status.PauseStatus.LastPauseBeginTime == metav1.NewTime(initTime) || !pipelineRollout.Status.PauseStatus.LastPauseBeginTime.After(pipelineRollout.Status.PauseStatus.LastPauseEndTime.Time) {
			pipelineRollout.Status.PauseStatus.LastPauseBeginTime = metav1.NewTime(time.Now())
		}
		reason := fmt.Sprintf("Pipeline%s", string(pipelinePhase))
		msg := fmt.Sprintf("Pipeline %s", strings.ToLower(string(pipelinePhase)))
		r.updatePauseMetric(pipelineRollout)
		pipelineRollout.Status.MarkPipelinePausingOrPaused(reason, msg, pipelineRollout.Generation)
	} else {
		// only set EndTime if BeginTime has been previously set AND EndTime is before/equal to BeginTime
		// EndTime is either just initialized or the end of a previous pause which is why it will be before the new BeginTime
		if (pipelineRollout.Status.PauseStatus.LastPauseBeginTime != metav1.NewTime(initTime)) && !pipelineRollout.Status.PauseStatus.LastPauseEndTime.After(pipelineRollout.Status.PauseStatus.LastPauseBeginTime.Time) {
			pipelineRollout.Status.PauseStatus.LastPauseEndTime = metav1.NewTime(time.Now())
			r.updatePauseMetric(pipelineRollout)
		}
		pipelineRollout.Status.MarkPipelineUnpaused(pipelineRollout.Generation)
	}

}

func (r *PipelineRolloutReconciler) updatePauseMetric(pipelineRollout *apiv1.PipelineRollout) {

	var pipelineSpec PipelineSpec
	_ = json.Unmarshal(pipelineRollout.Spec.Pipeline.Spec.Raw, &pipelineSpec)

	timeElapsed := time.Since(pipelineRollout.Status.PauseStatus.LastPauseBeginTime.Time)
	if r.isSpecBasedPause(pipelineSpec) {
		r.customMetrics.PipelinePausedSeconds.WithLabelValues(pipelineRollout.Namespace, pipelineRollout.Name, "user_pause").Set(timeElapsed.Seconds())
	} else {
		r.customMetrics.PipelinePausedSeconds.WithLabelValues(pipelineRollout.Namespace, pipelineRollout.Name, "system_pause").Set(timeElapsed.Seconds())
	}

}

func (r *PipelineRolloutReconciler) needsUpdate(old, new *apiv1.PipelineRollout) bool {
	if old == nil {
		return true
	}

	// check for any fields we might update in the Spec - generally we'd only update a Finalizer or maybe something in the metadata
	if !equality.Semantic.DeepEqual(old.Finalizers, new.Finalizers) {
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {

	controller, err := runtimecontroller.New(ControllerPipelineRollout, mgr, runtimecontroller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch PipelineRollouts
	if err := controller.Watch(source.Kind(mgr.GetCache(), &apiv1.PipelineRollout{},
		&handler.TypedEnqueueRequestForObject[*apiv1.PipelineRollout]{}, predicate.TypedGenerationChangedPredicate[*apiv1.PipelineRollout]{})); err != nil {
		return fmt.Errorf("failed to watch PipelineRollouts: %v", err)
	}

	// Watch Pipelines
	pipelineUns := &unstructured.Unstructured{}
	pipelineUns.SetGroupVersionKind(schema.GroupVersionKind{
		Kind:    common.NumaflowPipelineKind,
		Group:   common.NumaflowAPIGroup,
		Version: common.NumaflowAPIVersion,
	})
	if err := controller.Watch(source.Kind(mgr.GetCache(), pipelineUns,
		handler.TypedEnqueueRequestForOwner[*unstructured.Unstructured](mgr.GetScheme(), mgr.GetRESTMapper(),
			&apiv1.PipelineRollout{}, handler.OnlyControllerOwner()), predicate.TypedResourceVersionChangedPredicate[*unstructured.Unstructured]{})); err != nil {
		return fmt.Errorf("failed to watch Pipelines: %v", err)
	}

	return nil
}

// remove 'lifecycle.desiredPhase' key/value pair from spec
// also remove 'lifecycle' if it's an empty map
func withoutDesiredPhase(obj *kubernetes.GenericObject) (map[string]interface{}, error) {
	var specAsMap map[string]any
	if err := json.Unmarshal(obj.Spec.Raw, &specAsMap); err != nil {
		return nil, err
	}
	// remove "lifecycle.desiredPhase"
	comparisonExcludedPaths := []string{"lifecycle.desiredPhase"}
	util.RemovePaths(specAsMap, comparisonExcludedPaths, ".")
	// if "lifecycle" is there and empty, remove it
	lifecycleMap, found := specAsMap["lifecycle"].(map[string]interface{})
	if found && len(lifecycleMap) == 0 {
		util.RemovePaths(specAsMap, []string{"lifecycle"}, ".")
	}
	return specAsMap, nil
}

func checkPipelineStatus(ctx context.Context, pipeline *kubernetes.GenericObject, phase numaflowv1.PipelinePhase) bool {
	numaLogger := logger.FromContext(ctx)
	pipelineStatus, err := kubernetes.ParseStatus(pipeline)
	if err != nil {
		numaLogger.Errorf(err, "failed to parse Pipeline Status from pipeline CR: %+v, %v", pipeline, err)
		return false
	}

	return numaflowv1.PipelinePhase(pipelineStatus.Phase) == phase
}

func updatePipelineSpec(ctx context.Context, c client.Client, obj *kubernetes.GenericObject) error {
	return kubernetes.UpdateResource(ctx, c, obj)
}

// take the Metadata (Labels and Annotations) specified in the PipelineRollout plus any others that apply to all Pipelines
func getBasePipelineMetadata(pipelineRollout *apiv1.PipelineRollout) (apiv1.Metadata, error) {
	labelMapping := map[string]string{}
	for key, val := range pipelineRollout.Spec.Pipeline.Labels {
		labelMapping[key] = val
	}
	var pipelineSpec PipelineSpec

	if err := json.Unmarshal(pipelineRollout.Spec.Pipeline.Spec.Raw, &pipelineSpec); err != nil {
		return apiv1.Metadata{}, fmt.Errorf("failed to unmarshal pipeline spec: %v", err)
	}

	labelMapping[common.LabelKeyISBServiceNameForPipeline] = pipelineSpec.getISBSvcName()
	labelMapping[common.LabelKeyParentRollout] = pipelineRollout.Name

	return apiv1.Metadata{Labels: labelMapping, Annotations: pipelineRollout.Spec.Pipeline.Annotations}, nil

}

func (r *PipelineRolloutReconciler) updatePipelineRolloutStatus(ctx context.Context, pipelineRollout *apiv1.PipelineRollout) error {
	return r.client.Status().Update(ctx, pipelineRollout)
}

func (r *PipelineRolloutReconciler) updatePipelineRolloutStatusToFailed(ctx context.Context, pipelineRollout *apiv1.PipelineRollout, err error) error {
	pipelineRollout.Status.MarkFailed(err.Error())
	return r.updatePipelineRolloutStatus(ctx, pipelineRollout)
}

func (r *PipelineRolloutReconciler) makeRunningPipelineDefinition(
	ctx context.Context,
	pipelineRollout *apiv1.PipelineRollout,
) (*kubernetes.GenericObject, error) {
	pipelineName, err := getChildName(ctx, pipelineRollout, r, string(common.LabelValueUpgradePromoted))
	if err != nil {
		return nil, err
	}

	metadata, err := getBasePipelineMetadata(pipelineRollout)
	if err != nil {
		return nil, err
	}
	metadata.Labels[common.LabelKeyUpgradeState] = string(common.LabelValueUpgradePromoted)

	return r.makePipelineDefinition(pipelineRollout, pipelineName, metadata)
}

func (r *PipelineRolloutReconciler) makePipelineDefinition(
	pipelineRollout *apiv1.PipelineRollout,
	pipelineName string,
	metadata apiv1.Metadata,
) (*kubernetes.GenericObject, error) {

	return &kubernetes.GenericObject{
		TypeMeta: metav1.TypeMeta{
			Kind:       common.NumaflowPipelineKind,
			APIVersion: common.NumaflowAPIGroup + "/" + common.NumaflowAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            pipelineName,
			Namespace:       pipelineRollout.Namespace,
			Labels:          metadata.Labels,
			Annotations:     metadata.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(pipelineRollout.GetObjectMeta(), apiv1.PipelineRolloutGroupVersionKind)},
		},
		Spec: pipelineRollout.Spec.Pipeline.Spec,
	}, nil
}

// the following functions enable PipelineRolloutReconciler to implement progressiveController interface
func (r *PipelineRolloutReconciler) listChildren(ctx context.Context, rolloutObject RolloutObject, labelSelector string, fieldSelector string) ([]*kubernetes.GenericObject, error) {
	pipelineRollout := rolloutObject.(*apiv1.PipelineRollout)
	return kubernetes.ListLiveResource(
		ctx, common.NumaflowAPIGroup, common.NumaflowAPIVersion, "pipelines",
		pipelineRollout.Namespace, labelSelector, fieldSelector)
}

func (r *PipelineRolloutReconciler) createBaseChildDefinition(rolloutObject RolloutObject, name string) (*kubernetes.GenericObject, error) {
	pipelineRollout := rolloutObject.(*apiv1.PipelineRollout)
	metadata, err := getBasePipelineMetadata(pipelineRollout)
	if err != nil {
		return nil, err
	}
	return r.makePipelineDefinition(pipelineRollout, name, metadata)
}

func (r *PipelineRolloutReconciler) getCurrentChildCount(rolloutObject RolloutObject) (int32, bool) {
	pipelineRollout := rolloutObject.(*apiv1.PipelineRollout)
	if pipelineRollout.Status.NameCount == nil {
		return int32(0), false
	} else {
		return *pipelineRollout.Status.NameCount, true
	}
}

func (r *PipelineRolloutReconciler) updateCurrentChildCount(ctx context.Context, rolloutObject RolloutObject, nameCount int32) error {
	pipelineRollout := rolloutObject.(*apiv1.PipelineRollout)
	pipelineRollout.Status.NameCount = &nameCount
	return r.updatePipelineRolloutStatus(ctx, pipelineRollout)
}

// increment the child count for the Rollout and return the count to use
func (r *PipelineRolloutReconciler) incrementChildCount(ctx context.Context, rolloutObject RolloutObject) (int32, error) {
	currentNameCount, found := r.getCurrentChildCount(rolloutObject)
	if !found {
		currentNameCount = int32(0)
		err := r.updateCurrentChildCount(ctx, rolloutObject, int32(0))
		if err != nil {
			return int32(0), err
		}
	}

	err := r.updateCurrentChildCount(ctx, rolloutObject, currentNameCount+1)
	if err != nil {
		return int32(0), err
	}
	return currentNameCount, nil
}

func (r *PipelineRolloutReconciler) childIsDrained(ctx context.Context, pipelineDef *kubernetes.GenericObject) (bool, error) {
	pipelineStatus, err := parsePipelineStatus(pipelineDef)
	if err != nil {
		return false, fmt.Errorf("failed to parse Pipeline Status from pipeline CR: %+v, %v", pipelineDef, err)
	}
	pipelinePhase := pipelineStatus.Phase

	return pipelinePhase == numaflowv1.PipelinePhasePaused && pipelineStatus.DrainedOnPause, nil
}

func (r *PipelineRolloutReconciler) drain(ctx context.Context, pipeline *kubernetes.GenericObject) error {
	patchJson := `{"spec": {"lifecycle": {"desiredPhase": "Paused"}}}`
	return kubernetes.PatchResource(ctx, r.client, pipeline, patchJson, k8stypes.MergePatchType)
}

// childNeedsUpdating() tests for essential equality, with any irrelevant fields eliminated from the comparison
func (r *PipelineRolloutReconciler) childNeedsUpdating(ctx context.Context, a *kubernetes.GenericObject, b *kubernetes.GenericObject) (bool, error) {
	numaLogger := logger.FromContext(ctx)
	// remove lifecycle.desiredPhase field from comparison to test for equality
	pipelineWithoutDesiredPhaseA, err := withoutDesiredPhase(a)
	if err != nil {
		return false, err
	}
	pipelineWithoutDesiredPhaseB, err := withoutDesiredPhase(b)
	if err != nil {
		return false, err
	}
	numaLogger.Debugf("comparing specs: pipelineWithoutDesiredPhaseA=%v, pipelineWithoutDesiredPhaseB=%v\n", pipelineWithoutDesiredPhaseA, pipelineWithoutDesiredPhaseB)

	return !reflect.DeepEqual(pipelineWithoutDesiredPhaseA, pipelineWithoutDesiredPhaseB), nil
}

// getPipelineRolloutName gets the PipelineRollout name from the pipeline
// by locating the last index of '-' and trimming the suffix.
func getPipelineRolloutName(pipeline string) string {
	index := strings.LastIndex(pipeline, "-")
	return pipeline[:index]
}

func getPipelineChildResourceHealth(conditions []metav1.Condition) (metav1.ConditionStatus, string) {
	for _, cond := range conditions {
		switch cond.Type {
		case "VerticesHealthy", "SideInputsManagersHealthy", "DaemonServiceHealthy":
			// if any child resource unhealthy return status (false/unknown)
			if cond.Status != "True" {
				return cond.Status, cond.Reason
			}
		}
	}
	return "True", ""
}

func (r *PipelineRolloutReconciler) ErrorHandler(pipelineRollout *apiv1.PipelineRollout, err error, reason, msg string) {
	r.customMetrics.PipelineROSyncErrors.WithLabelValues().Inc()
	r.recorder.Eventf(pipelineRollout, corev1.EventTypeWarning, reason, msg+" %v", err.Error())
}

func parsePipelineStatus(obj *kubernetes.GenericObject) (numaflowv1.PipelineStatus, error) {
	if obj == nil || len(obj.Status.Raw) == 0 {
		return numaflowv1.PipelineStatus{}, nil
	}

	var status numaflowv1.PipelineStatus
	err := json.Unmarshal(obj.Status.Raw, &status)
	if err != nil {
		return numaflowv1.PipelineStatus{}, err
	}

	return status, nil
}
