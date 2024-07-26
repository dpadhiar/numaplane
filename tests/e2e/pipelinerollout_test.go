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

package e2e

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	numaflowv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"

	apiv1 "github.com/numaproj/numaplane/pkg/apis/numaplane/v1alpha1"
)

var pipelineSpecSourceRPU = int64(5)
var pipelineSpecSourceDuration = metav1.Duration{
	Duration: time.Second,
}
var pipelineSpec = numaflowv1.PipelineSpec{
	InterStepBufferServiceName: "my-isbsvc",
	Vertices: []numaflowv1.AbstractVertex{
		{
			Name: "in",
			Source: &numaflowv1.Source{
				Generator: &numaflowv1.GeneratorSource{
					RPU:      &pipelineSpecSourceRPU,
					Duration: &pipelineSpecSourceDuration,
				},
			},
		},
		{
			Name: "out",
			Sink: &numaflowv1.Sink{
				AbstractSink: numaflowv1.AbstractSink{
					Log: &numaflowv1.Log{},
				},
			},
		},
	},
	Edges: []numaflowv1.Edge{
		{
			From: "in",
			To:   "out",
		},
	},
}

var _ = Describe("PipelineRollout e2e", func() {

	const (
		namespace           = "numaplane-system"
		pipelineRolloutName = "e2e-pipeline-rollout"
	)

	rolloutgvr := getGVRForPipelineRollout()
	pipelinegvr := getGVRForPipeline()

	It("Should create the PipelineRollout if it does not exist", func() {

		pipelineRolloutSpec := createPipelineRolloutSpec(pipelineRolloutName, namespace)

		err := createPipelineRollout(ctx, pipelineRolloutSpec)
		Expect(err).ShouldNot(HaveOccurred())

		// TODO: make this a common function
		createdResource := &unstructured.Unstructured{}
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(rolloutgvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdResource = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())

		createPipelineSpec := numaflowv1.PipelineSpec{}
		rawPipelineSpec := createdResource.Object["spec"].(map[string]interface{})["pipeline"].(map[string]interface{})["spec"].(map[string]interface{})
		rawPipelineSpecBytes, err := json.Marshal(rawPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())
		err = json.Unmarshal(rawPipelineSpecBytes, &createPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())

		By("Verifying the content of the pipeline spec field")
		Expect(createPipelineSpec).Should(Equal(pipelineSpec))

	})

	It("Should create a Pipeline", func() {

		createdPipeline := &unstructured.Unstructured{}
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdPipeline = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())

		createdPipelineSpec := numaflowv1.PipelineSpec{}
		rawPipelineSpec := createdPipeline.Object["spec"].(map[string]interface{})
		rawPipelineSpecBytes, err := json.Marshal(rawPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())
		err = json.Unmarshal(rawPipelineSpecBytes, &createdPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())

		By("Verifying the content of the pipeline spec")
		Expect(createdPipelineSpec).Should(Equal(pipelineSpec))

	})

	It("Should automatically heal a Pipeline if it is updated directly", func() {

		// get child Pipeline
		createdPipeline := &unstructured.Unstructured{}
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdPipeline = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())

		// modify spec to have different isbsvc name
		createdPipeline.Object["spec"].(map[string]interface{})["interStepBufferServiceName"] = "new-isbsvc"

		// update child Pipeline
		_, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Update(ctx, createdPipeline, metav1.UpdateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		// allow time for self healing to reconcile
		time.Sleep(5 * time.Second)

		// get updated Pipeline again to compare spec
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdPipeline = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())
		createdPipelineSpec := numaflowv1.PipelineSpec{}
		rawPipelineSpec := createdPipeline.Object["spec"].(map[string]interface{})
		rawPipelineSpecBytes, err := json.Marshal(rawPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())
		err = json.Unmarshal(rawPipelineSpecBytes, &createdPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())

		By("Verifying that the child Pipeline spec has been restored to the original")
		Expect(createdPipelineSpec).Should(Equal(pipelineSpec))

	})

	It("Should update the child Pipeline if the PipelineRollout is updated", func() {

		// new Pipeline spec
		updatedPipelineSpec := pipelineSpec
		updatedPipelineSpec.InterStepBufferServiceName = "updated-isbsvc"

		// get current PipelineRollout
		createdResource := &unstructured.Unstructured{}
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(rolloutgvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdResource = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())

		// update spec.pipeline.spec of returned PipelineRollout object
		createdResource.Object["spec"].(map[string]interface{})["pipeline"].(map[string]interface{})["spec"] = updatedPipelineSpec

		// update the PipelineRollout
		_, err := dynamicClient.Resource(rolloutgvr).Namespace(namespace).Update(ctx, createdResource, metav1.UpdateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		// wait for update to reconcile
		time.Sleep(5 * time.Second)

		// get Pipeline to check that spec has been updated
		createdPipeline := &unstructured.Unstructured{}
		Eventually(func() bool {
			unstruct, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			createdPipeline = unstruct
			return true
		}).WithTimeout(timeout).Should(BeTrue())
		createdPipelineSpec := numaflowv1.PipelineSpec{}
		rawPipelineSpec := createdPipeline.Object["spec"].(map[string]interface{})
		rawPipelineSpecBytes, err := json.Marshal(rawPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())
		err = json.Unmarshal(rawPipelineSpecBytes, &createdPipelineSpec)
		Expect(err).ShouldNot(HaveOccurred())

		By("Verifying the content of the pipeline spec")
		Expect(createdPipelineSpec).Should(Equal(updatedPipelineSpec))

	})

	It("Should delete the PipelineRollout and child Pipeline", func() {

		err := deletePipelineRollout(ctx, namespace, pipelineRolloutName)
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() bool {
			_, err := dynamicClient.Resource(rolloutgvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				if !errors.IsNotFound(err) {
					Fail("An unexpected error occurred when fetching the PipelineRollout: " + err.Error())
				}
				return false
			}
			return true
		}, timeout).Should(BeFalse(), "The PipelineRollout should have been deleted but it was found.")

		Eventually(func() bool {
			_, err := dynamicClient.Resource(pipelinegvr).Namespace(namespace).Get(ctx, pipelineRolloutName, metav1.GetOptions{})
			if err != nil {
				if !errors.IsNotFound(err) {
					Fail("An unexpected error occurred when fetching the Pipeline: " + err.Error())
				}
				return false
			}
			return true
		}, timeout).Should(BeFalse(), "The Pipeline should have been deleted but it was found.")

	})

})

func createPipelineRolloutSpec(name, namespace string) *unstructured.Unstructured {

	pipelineSpecRaw, err := json.Marshal(pipelineSpec)
	Expect(err).ToNot(HaveOccurred())

	pipelineRollout := &apiv1.PipelineRollout{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "numaplane.numaproj.io/v1alpha1",
			Kind:       "PipelineRollout",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: apiv1.PipelineRolloutSpec{
			Pipeline: apiv1.Pipeline{
				Spec: runtime.RawExtension{
					Raw: pipelineSpecRaw,
				},
			},
		},
	}

	unstructuredObj, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(pipelineRollout)
	return &unstructured.Unstructured{Object: unstructuredObj}

}

func createPipelineRollout(ctx context.Context, rollout *unstructured.Unstructured) error {
	_, err := dynamicClient.Resource(getGVRForPipelineRollout()).Namespace(rollout.GetNamespace()).Create(ctx, rollout, metav1.CreateOptions{})
	return err
}

func deletePipelineRollout(ctx context.Context, namespace, name string) error {
	err := dynamicClient.Resource(getGVRForPipelineRollout()).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	return err
}

func getGVRForPipelineRollout() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "numaplane.numaproj.io",
		Version:  "v1alpha1",
		Resource: "pipelinerollouts",
	}
}

func getGVRForPipeline() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "numaflow.numaproj.io",
		Version:  "v1alpha1",
		Resource: "pipelines",
	}
}
