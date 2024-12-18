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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"

	numaflowv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	"github.com/numaproj/numaplane/internal/common"
	"github.com/numaproj/numaplane/internal/controller/config"
	"github.com/numaproj/numaplane/internal/util"
	"github.com/numaproj/numaplane/internal/util/kubernetes"
	"github.com/numaproj/numaplane/internal/util/logger"
	"github.com/numaproj/numaplane/internal/util/metrics"
	apiv1 "github.com/numaproj/numaplane/pkg/apis/numaplane/v1alpha1"
	commontest "github.com/numaproj/numaplane/tests/common"
)

var (
	defaultNamespace         = "default"
	defaultISBSvcRolloutName = "isbservicerollout-test"
)

var _ = Describe("ISBServiceRollout Controller", Ordered, func() {
	ctx := context.Background()

	isbsSpec := numaflowv1.InterStepBufferServiceSpec{
		Redis: &numaflowv1.RedisBufferService{},
		JetStream: &numaflowv1.JetStreamBufferService{
			Version: "latest",
			Persistence: &numaflowv1.PersistenceStrategy{
				VolumeSize: &numaflowv1.DefaultVolumeSize,
			},
		},
	}

	isbsSpecRaw, err := json.Marshal(isbsSpec)
	Expect(err).ToNot(HaveOccurred())

	isbServiceRollout := &apiv1.ISBServiceRollout{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaultNamespace,
			Name:      defaultISBSvcRolloutName,
		},
		Spec: apiv1.ISBServiceRolloutSpec{
			InterStepBufferService: apiv1.InterStepBufferService{
				Spec: k8sruntime.RawExtension{
					Raw: isbsSpecRaw,
				},
			},
		},
	}

	resourceLookupKey := types.NamespacedName{Name: defaultISBSvcRolloutName, Namespace: defaultNamespace}

	Context("When applying a ISBServiceRollout spec", func() {
		It("Should create the ISBServiceRollout if it does not exist", func() {
			Expect(k8sClient.Create(ctx, isbServiceRollout)).Should(Succeed())

			createdResource := &apiv1.ISBServiceRollout{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, resourceLookupKey, createdResource)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			createdInterStepBufferServiceSpec := numaflowv1.InterStepBufferServiceSpec{}
			Expect(json.Unmarshal(createdResource.Spec.InterStepBufferService.Spec.Raw, &createdInterStepBufferServiceSpec)).ToNot(HaveOccurred())

			By("Verifying the content of the ISBServiceRollout spec field")
			Expect(createdInterStepBufferServiceSpec).Should(Equal(isbsSpec))
		})

		It("Should have created an InterStepBufferService ", func() {
			createdISBResource := &numaflowv1.InterStepBufferService{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, resourceLookupKey, createdISBResource)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Verifying the content of the InterStepBufferService spec")
			Expect(createdISBResource.Spec).Should(Equal(isbsSpec))
		})

		It("Should have the ISBServiceRollout Status Phase as Deployed and ObservedGeneration matching Generation", func() {
			verifyStatusPhase(ctx, apiv1.ISBServiceRolloutGroupVersionKind, defaultNamespace, defaultISBSvcRolloutName, apiv1.PhaseDeployed)
		})

		It("Should have created an PodDisruptionBudget for ISB ", func() {
			isbPDB := &policyv1.PodDisruptionBudget{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, resourceLookupKey, isbPDB)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Verifying the content of the InterStepBufferService spec")
			Expect(isbPDB.Spec.MaxUnavailable.IntVal).Should(Equal(int32(1)))
		})

		It("Should have the metrics updated", func() {
			By("Verifying the ISBService metric")
			Expect(testutil.ToFloat64(customMetrics.ISBServiceRolloutsRunning.WithLabelValues(defaultNamespace))).Should(Equal(float64(1)))
			Expect(testutil.ToFloat64(customMetrics.ISBServiceROSyncs.WithLabelValues())).Should(BeNumerically(">", 1))
		})

		It("Should update the ISBServiceRollout and InterStepBufferService", func() {
			By("updating the ISBServiceRollout")

			currentISBServiceRollout := &apiv1.ISBServiceRollout{}
			Expect(k8sClient.Get(ctx, resourceLookupKey, currentISBServiceRollout)).ToNot(HaveOccurred())

			// Prepare a new spec for update
			newIsbsSpec := numaflowv1.InterStepBufferServiceSpec{
				Redis: &numaflowv1.RedisBufferService{},
				JetStream: &numaflowv1.JetStreamBufferService{
					Version: "an updated version",
					Persistence: &numaflowv1.PersistenceStrategy{
						VolumeSize: &numaflowv1.DefaultVolumeSize,
					},
				},
			}

			newIsbsSpecRaw, err := json.Marshal(newIsbsSpec)
			Expect(err).ToNot(HaveOccurred())

			// Update the spec
			currentISBServiceRollout.Spec.InterStepBufferService.Spec.Raw = newIsbsSpecRaw //runtime.RawExtension{Raw: newIsbsSpecRaw}

			Expect(k8sClient.Update(ctx, currentISBServiceRollout)).ToNot(HaveOccurred())

			By("Verifying the content of the ISBServiceRollout")
			Eventually(func() (numaflowv1.InterStepBufferServiceSpec, error) {
				updatedResource := &apiv1.ISBServiceRollout{}
				err := k8sClient.Get(ctx, resourceLookupKey, updatedResource)
				if err != nil {
					return numaflowv1.InterStepBufferServiceSpec{}, err
				}

				createdInterStepBufferServiceSpec := numaflowv1.InterStepBufferServiceSpec{}
				Expect(json.Unmarshal(updatedResource.Spec.InterStepBufferService.Spec.Raw, &createdInterStepBufferServiceSpec)).ToNot(HaveOccurred())

				return createdInterStepBufferServiceSpec, nil
			}, timeout, interval).Should(Equal(newIsbsSpec))

			By("Verifying the content of the InterStepBufferService ")
			Eventually(func() (numaflowv1.InterStepBufferServiceSpec, error) {
				updatedChildResource := &numaflowv1.InterStepBufferService{}
				err := k8sClient.Get(ctx, resourceLookupKey, updatedChildResource)
				if err != nil {
					return numaflowv1.InterStepBufferServiceSpec{}, err
				}
				return updatedChildResource.Spec, nil
			}, timeout, interval).Should(Equal(newIsbsSpec))

			By("Verifying that the ISBServiceRollout Status Phase is Deployed and ObservedGeneration matches Generation")
			verifyStatusPhase(ctx, apiv1.ISBServiceRolloutGroupVersionKind, defaultNamespace, defaultISBSvcRolloutName, apiv1.PhaseDeployed)
		})

		It("Should auto heal the InterStepBufferService with the ISBServiceRollout pipeline spec when the InterStepBufferService spec is changed", func() {
			By("updating the InterStepBufferService and verifying the changed field is the same as the original and not the modified version")
			verifyAutoHealing(ctx, numaflowv1.ISBGroupVersionKind, defaultNamespace, defaultISBSvcRolloutName, "spec.jetstream.version", "1.2.3.4.5")
		})

		It("Should delete the ISBServiceRollout and InterStepBufferService", func() {
			Expect(k8sClient.Delete(ctx, &apiv1.ISBServiceRollout{
				ObjectMeta: isbServiceRollout.ObjectMeta,
			})).Should(Succeed())

			deletedResource := &apiv1.ISBServiceRollout{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, resourceLookupKey, deletedResource)
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())

			deletingChildResource := &numaflowv1.InterStepBufferService{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, resourceLookupKey, deletingChildResource)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(deletingChildResource.OwnerReferences).Should(HaveLen(1))
			Expect(deletedResource.UID).Should(Equal(deletingChildResource.OwnerReferences[0].UID))

			// TODO: use this on real cluster for e2e tests
			// NOTE: it's necessary to run on existing cluster to allow for deletion of child resources.
			// See https://book.kubebuilder.io/reference/envtest#testing-considerations for more details.
			// Could also reuse the env var used to set useExistingCluster to skip or perform the deletion based on CI settings.
			// Eventually(func() bool {
			// 	deletedChildResource := &apiv1.ISBServiceRollout{}
			// 	err := k8sClient.Get(ctx, resourceLookupKey, deletedChildResource)
			// 	return errors.IsNotFound(err)
			// }, timeout, interval).Should(BeTrue())
		})
	})

	Context("When applying an invalid ISBServiceRollout spec", func() {
		It("Should not create the ISBServiceRollout", func() {
			Expect(k8sClient.Create(ctx, &apiv1.ISBServiceRollout{
				Spec: isbServiceRollout.Spec,
			})).ShouldNot(Succeed())

			Expect(k8sClient.Create(ctx, &apiv1.ISBServiceRollout{
				ObjectMeta: isbServiceRollout.ObjectMeta,
			})).ShouldNot(Succeed())

			Expect(k8sClient.Create(ctx, &apiv1.ISBServiceRollout{
				ObjectMeta: isbServiceRollout.ObjectMeta,
				Spec:       apiv1.ISBServiceRolloutSpec{},
			})).ShouldNot(Succeed())
		})
	})
})

// test reconcile() for the case of PPND

func Test_reconcile_isbservicerollout_PPND(t *testing.T) {
	ctx := context.Background()

	numaLogger := logger.New()
	numaLogger.SetLevel(3)
	logger.SetBaseLogger(numaLogger)
	ctx = logger.WithLogger(ctx, numaLogger)

	restConfig, numaflowClientSet, numaplaneClient, k8sClientSet, err := commontest.PrepareK8SEnvironment()
	assert.Nil(t, err)
	assert.Nil(t, kubernetes.SetDynamicClient(restConfig))

	config.GetConfigManagerInstance().UpdateUSDEConfig(config.USDEConfig{DefaultUpgradeStrategy: config.PPNDStrategyID})

	// other tests may call this, but it fails if called more than once
	if customMetrics == nil {
		customMetrics = metrics.RegisterCustomMetrics()
	}

	recorder := record.NewFakeRecorder(64)

	r := NewISBServiceRolloutReconciler(numaplaneClient, scheme.Scheme, customMetrics, recorder)

	trueValue := true
	falseValue := false

	pipelineROReconciler = &PipelineRolloutReconciler{queue: util.NewWorkQueue("fake_queue")}

	testCases := []struct {
		name                      string
		newISBSvcSpec             numaflowv1.InterStepBufferServiceSpec
		existingISBSvcDef         *numaflowv1.InterStepBufferService
		existingStatefulSetDef    *appsv1.StatefulSet
		existingPipelineRollout   *apiv1.PipelineRollout
		existingPipeline          *numaflowv1.Pipeline
		existingPauseRequest      *bool // was ISBServiceRollout previously requesting pause?
		initialInProgressStrategy apiv1.UpgradeStrategy
		expectedPauseRequest      *bool // after reconcile(), should it be requesting pause?
		expectedRolloutPhase      apiv1.Phase
		// require these Conditions to be set (note that in real life, previous reconciliations may have set other Conditions from before which are still present)
		expectedConditionsSet      map[apiv1.ConditionType]metav1.ConditionStatus
		expectedISBSvcSpec         numaflowv1.InterStepBufferServiceSpec
		expectedInProgressStrategy apiv1.UpgradeStrategy
	}{
		{
			name:                      "new ISBService",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.3"),
			existingISBSvcDef:         nil,
			existingStatefulSetDef:    nil,
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhaseRunning),
			existingPauseRequest:      nil,
			initialInProgressStrategy: apiv1.UpgradeStrategyNoOp,
			expectedPauseRequest:      nil,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionChildResourceDeployed: metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.3"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyNoOp,
		},
		{
			name:                      "existing ISBService - no change",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.3"),
			existingISBSvcDef:         createDefaultISBService("2.10.3", numaflowv1.ISBSvcPhaseRunning, true),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.3", true),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhaseRunning),
			existingPauseRequest:      &falseValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyNoOp,
			expectedPauseRequest:      &falseValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet:     map[apiv1.ConditionType]metav1.ConditionStatus{}, // some Conditions may be set from before, but in any case nothing new to verify
			expectedISBSvcSpec:        createDefaultISBServiceSpec("2.10.3"),
		},
		{
			name:                       "existing ISBService - new spec - pipelines not paused",
			newISBSvcSpec:              createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:          createDefaultISBService("2.10.3", numaflowv1.ISBSvcPhaseRunning, true),
			existingStatefulSetDef:     createDefaultISBStatefulSet("2.10.3", true),
			existingPipelineRollout:    createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:           createDefaultPipelineOfPhase(numaflowv1.PipelinePhaseRunning),
			existingPauseRequest:       &falseValue,
			initialInProgressStrategy:  apiv1.UpgradeStrategyNoOp,
			expectedPauseRequest:       &trueValue,
			expectedRolloutPhase:       apiv1.PhasePending,
			expectedConditionsSet:      map[apiv1.ConditionType]metav1.ConditionStatus{apiv1.ConditionPausingPipelines: metav1.ConditionTrue},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.3"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyPPND,
		},
		{
			name:                      "existing ISBService - new spec - pipelines paused",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:         createDefaultISBService("2.10.3", numaflowv1.ISBSvcPhaseRunning, true),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.3", true),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhasePaused),
			existingPauseRequest:      &trueValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyPPND,
			expectedPauseRequest:      &trueValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionChildResourceDeployed: metav1.ConditionTrue,
				apiv1.ConditionPausingPipelines:      metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.11"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyPPND,
		},
		{
			name:                      "existing ISBService - new spec - pipelines failed",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:         createDefaultISBService("2.10.3", numaflowv1.ISBSvcPhaseRunning, true),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.3", true),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhaseFailed),
			existingPauseRequest:      &trueValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyPPND,
			expectedPauseRequest:      &trueValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionChildResourceDeployed: metav1.ConditionTrue,
				apiv1.ConditionPausingPipelines:      metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.11"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyPPND,
		},
		{
			name:                      "existing ISBService - new spec - pipelines set to allow data loss",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:         createDefaultISBService("2.10.3", numaflowv1.ISBSvcPhasePending, true),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.3", true),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{common.LabelKeyAllowDataLoss: "true"}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhasePausing),
			existingPauseRequest:      &trueValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyPPND,
			expectedPauseRequest:      &trueValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionChildResourceDeployed: metav1.ConditionTrue,
				apiv1.ConditionPausingPipelines:      metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.11"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyPPND,
		},
		{
			name:                      "existing ISBService - spec already updated - isbsvc reconciling",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:         createDefaultISBService("2.10.11", numaflowv1.ISBSvcPhaseRunning, false),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.3", false),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhasePaused),
			existingPauseRequest:      &trueValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyPPND,
			expectedPauseRequest:      &trueValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionPausingPipelines: metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.11"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyPPND,
		},
		{
			name:                      "existing ISBService - spec already updated - isbsvc done reconciling",
			newISBSvcSpec:             createDefaultISBServiceSpec("2.10.11"),
			existingISBSvcDef:         createDefaultISBService("2.10.11", numaflowv1.ISBSvcPhaseRunning, true),
			existingStatefulSetDef:    createDefaultISBStatefulSet("2.10.11", true),
			existingPipelineRollout:   createPipelineRollout(numaflowv1.PipelineSpec{InterStepBufferServiceName: defaultISBSvcRolloutName}, map[string]string{}, map[string]string{}),
			existingPipeline:          createDefaultPipelineOfPhase(numaflowv1.PipelinePhasePaused),
			existingPauseRequest:      &trueValue,
			initialInProgressStrategy: apiv1.UpgradeStrategyPPND,
			expectedPauseRequest:      &falseValue,
			expectedRolloutPhase:      apiv1.PhaseDeployed,
			expectedConditionsSet: map[apiv1.ConditionType]metav1.ConditionStatus{
				apiv1.ConditionChildResourceDeployed: metav1.ConditionTrue,
			},
			expectedISBSvcSpec:         createDefaultISBServiceSpec("2.10.11"),
			expectedInProgressStrategy: apiv1.UpgradeStrategyNoOp,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			// first delete any previous resources if they already exist, in Kubernetes
			_ = numaflowClientSet.NumaflowV1alpha1().InterStepBufferServices(defaultNamespace).Delete(ctx, defaultISBSvcRolloutName, metav1.DeleteOptions{})
			_ = k8sClientSet.AppsV1().StatefulSets(defaultNamespace).Delete(ctx, deriveISBSvcStatefulSetName(defaultISBSvcRolloutName), metav1.DeleteOptions{})
			_ = numaflowClientSet.NumaflowV1alpha1().Pipelines(defaultNamespace).Delete(ctx, defaultPipelineName, metav1.DeleteOptions{})
			_ = numaplaneClient.Delete(ctx, &apiv1.PipelineRollout{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace, Name: defaultPipelineRolloutName}})

			isbsvcList, err := numaflowClientSet.NumaflowV1alpha1().InterStepBufferServices(defaultNamespace).List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)
			assert.Len(t, isbsvcList.Items, 0)
			ssList, err := k8sClientSet.AppsV1().StatefulSets(defaultNamespace).List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)
			assert.Len(t, ssList.Items, 0)
			pipelineList, err := numaflowClientSet.NumaflowV1alpha1().Pipelines(defaultNamespace).List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)
			assert.Len(t, pipelineList.Items, 0)

			// create ISBServiceRollout definition
			rollout := createISBServiceRollout(tc.newISBSvcSpec)

			// the Reconcile() function does this, so we need to do it before calling reconcile() as well
			rollout.Status.Init(rollout.Generation)

			// create the already-existing ISBSvc in Kubernetes
			if tc.existingISBSvcDef != nil {
				createISBSvcInK8S(ctx, t, numaflowClientSet, tc.existingISBSvcDef)
			}

			// create the already-existing StatefulSet in Kubernetes
			if tc.existingStatefulSetDef != nil {
				createStatefulSetInK8S(ctx, t, k8sClientSet, tc.existingStatefulSetDef)
			}

			createPipelineRolloutInK8S(ctx, t, numaplaneClient, tc.existingPipelineRollout)

			createPipelineInK8S(ctx, t, numaflowClientSet, tc.existingPipeline)

			pm := GetPauseModule()
			pm.pauseRequests[pm.getISBServiceKey(defaultNamespace, defaultISBSvcRolloutName)] = tc.existingPauseRequest

			rollout.Status.UpgradeInProgress = tc.initialInProgressStrategy
			r.inProgressStrategyMgr.store.setStrategy(k8stypes.NamespacedName{Namespace: defaultNamespace, Name: defaultPipelineRolloutName}, tc.initialInProgressStrategy)

			// call reconcile()
			_, err = r.reconcile(ctx, rollout, time.Now())
			assert.NoError(t, err)

			////// check results:

			// Check in-memory pause request:
			assert.Equal(t, tc.expectedPauseRequest, (pm.pauseRequests[pm.getISBServiceKey(defaultNamespace, defaultISBSvcRolloutName)]))

			// Check Phase of Rollout:
			assert.Equal(t, tc.expectedRolloutPhase, rollout.Status.Phase)
			// Check In-Progress Strategy
			assert.Equal(t, tc.expectedInProgressStrategy, rollout.Status.UpgradeInProgress)
			// Check isbsvc
			resultISBSVC, err := numaflowClientSet.NumaflowV1alpha1().InterStepBufferServices(defaultNamespace).Get(ctx, defaultISBSvcRolloutName, metav1.GetOptions{})
			assert.NoError(t, err)
			assert.NotNil(t, resultISBSVC)
			assert.Equal(t, tc.expectedISBSvcSpec, resultISBSVC.Spec)

			// Check Conditions:
			for conditionType, conditionStatus := range tc.expectedConditionsSet {
				found := false
				for _, condition := range rollout.Status.Conditions {
					if condition.Type == string(conditionType) && condition.Status == conditionStatus {
						found = true
						break
					}
				}
				assert.True(t, found, "condition type %s failed, conditions=%+v", conditionType, rollout.Status.Conditions)
			}

		})
	}
}

func createDefaultISBServiceSpec(jetstreamVersion string) numaflowv1.InterStepBufferServiceSpec {
	return numaflowv1.InterStepBufferServiceSpec{
		Redis: &numaflowv1.RedisBufferService{},
		JetStream: &numaflowv1.JetStreamBufferService{
			Version:     jetstreamVersion,
			Persistence: nil,
		},
	}
}

func createDefaultISBService(jetstreamVersion string, phase numaflowv1.ISBSvcPhase, fullyReconciled bool) *numaflowv1.InterStepBufferService {
	status := numaflowv1.InterStepBufferServiceStatus{
		Phase: phase,
	}
	if fullyReconciled {
		status.ObservedGeneration = 1
	} else {
		status.ObservedGeneration = 0
	}
	return &numaflowv1.InterStepBufferService{
		TypeMeta: metav1.TypeMeta{
			Kind:       common.NumaflowISBServiceKind,
			APIVersion: common.NumaflowAPIGroup + "/" + common.NumaflowAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultISBSvcRolloutName,
			Namespace: defaultNamespace,
		},
		Spec:   createDefaultISBServiceSpec(jetstreamVersion),
		Status: status,
	}
}

func createDefaultISBStatefulSet(jetstreamVersion string, fullyReconciled bool) *appsv1.StatefulSet {
	var status appsv1.StatefulSetStatus
	if fullyReconciled {
		status.ObservedGeneration = 1
		status.Replicas = 3
		status.UpdatedReplicas = 3
	} else {
		status.ObservedGeneration = 0
		status.Replicas = 3
	}
	replicas := int32(3)
	labels := map[string]string{
		"app.kubernetes.io/component":      "isbsvc",
		"app.kubernetes.io/managed-by":     "isbsvc-controller",
		"app.kubernetes.io/part-of":        "numaflow",
		"numaflow.numaproj.io/isbsvc-name": defaultISBSvcRolloutName,
		"numaflow.numaproj.io/isbsvc-type": "jetstream",
	}
	selector := metav1.LabelSelector{MatchLabels: labels}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deriveISBSvcStatefulSetName(defaultISBSvcRolloutName),
			Namespace: defaultNamespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &selector,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{Containers: []v1.Container{{
					Image: fmt.Sprintf("nats:%s", jetstreamVersion),
					Name:  "main",
				}}},
			},
		},
		Status: status,
	}
}

func deriveISBSvcStatefulSetName(isbsvcName string) string {
	return fmt.Sprintf("isbsvc-%s-js", isbsvcName)
}

func createISBServiceRollout(isbsvcSpec numaflowv1.InterStepBufferServiceSpec) *apiv1.ISBServiceRollout {
	isbsSpecRaw, _ := json.Marshal(isbsvcSpec)
	return &apiv1.ISBServiceRollout{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         defaultNamespace,
			Name:              defaultISBSvcRolloutName,
			UID:               "some-uid",
			CreationTimestamp: metav1.NewTime(time.Now()),
			Generation:        1,
		},
		Spec: apiv1.ISBServiceRolloutSpec{
			InterStepBufferService: apiv1.InterStepBufferService{
				Spec: k8sruntime.RawExtension{
					Raw: isbsSpecRaw,
				},
			},
		},
	}
}
