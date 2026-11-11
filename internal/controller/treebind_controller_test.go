/*
Copyright The Treebind Authors.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

var _ = Describe("Treebind Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		treebind := &treebindv1alpha1.Treebind{}
		var controllerReconciler *TreebindReconciler

		reconcileOnce := func() {
			GinkgoHelper()
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		}

		get := func() *treebindv1alpha1.Treebind {
			GinkgoHelper()
			got := &treebindv1alpha1.Treebind{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, got)).To(Succeed())
			return got
		}

		BeforeEach(func() {
			controllerReconciler = &TreebindReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the custom resource for the Kind Treebind")
			err := k8sClient.Get(ctx, typeNamespacedName, treebind)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, newTreebind(resourceName))).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &treebindv1alpha1.Treebind{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Treebind")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("reports a spec with only implemented fields as supported", func() {
			reconcileOnce()

			got := get()
			condition := meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSupported))
			Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		It("reports the fields that are accepted but not implemented, and clears them once removed", func() {
			reconcileOnce()

			resource := get()
			resource.Spec.Placement.Mode = treebindv1alpha1.PlacementModeSidecar
			resource.Spec.Agent = &treebindv1alpha1.AgentSpec{NodeSelector: newAgentSpec().NodeSelector}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			reconcileOnce()

			got := get()
			condition := meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonNotImplemented))
			Expect(condition.Message).To(ContainSubstring(fieldNodeSelector))
			Expect(condition.Message).To(ContainSubstring(fieldPlacementMode))
			Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Generation).To(BeNumerically(">", int64(1)))

			got.Spec.Placement.Mode = treebindv1alpha1.PlacementModeStandalone
			got.Spec.Agent = nil
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			reconcileOnce()

			got = get()
			condition = meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionSpecSupported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(treebindv1alpha1.ReasonSupported))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		})

		It("leaves the object untouched when nothing changed since the last reconcile", func() {
			resource := get()
			resource.Spec.Agent = &treebindv1alpha1.AgentSpec{PriorityClassName: newAgentSpec().PriorityClassName}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			reconcileOnce()
			first := get()

			reconcileOnce()
			second := get()

			Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
		})

		It("does nothing for a Treebind that no longer exists", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "gone", Namespace: resourceNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
