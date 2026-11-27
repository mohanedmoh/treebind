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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

func TestUsesPlainHTTP(t *testing.T) {
	tests := []struct {
		name, address string
		want          bool
	}{
		{"http", httpRepositoryURL, true},
		{"an upper-case scheme, which the API server refuses today", "HTTP://git.internal/b.git", true},
		{"a mixed-case scheme", "Http://git.internal/a.git", true},
		{"a port", "http://git.internal:8080/a.git", true},
		{"https", httpsRepositoryURL, false},
		{"https in upper case", upperHTTPSAddress, false},
		{"ssh", sshRepositoryURL, false},
		{"another scheme that starts like it", "httpx://git.internal/a.git", false},
		{"a scheme that http is the start of", "http+ssh://git.internal/a.git", false},
		{"http only in the path", "https://git.internal/http://a.git", false},
		{"no scheme", "git.internal/a.git", false},
		{"an scp-style address", "git@git.internal:example/b.git", false},
		{"an address that does not parse, which the API server refuses", "http://[::1/a.git", false},
		{"nothing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usesPlainHTTP(tt.address); got != tt.want {
				t.Errorf("usesPlainHTTP(%q) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestUnsupportedRepositorySchemeConditionNamesTheProblemAndTheFix(t *testing.T) {
	condition := unsupportedRepositorySchemeCondition(4)

	if condition.Type != treebindv1alpha1.ConditionReady || condition.Status != metav1.ConditionFalse {
		t.Errorf("the condition is %s %s, want Ready False", condition.Type, condition.Status)
	}
	if condition.Reason != "UnsupportedRepositoryScheme" {
		t.Errorf("the reason is %q, and people match on it", condition.Reason)
	}
	if condition.ObservedGeneration != 4 {
		t.Errorf("the generation is %d, want the one it was made for", condition.ObservedGeneration)
	}
	for _, want := range []string{"http://", "not supported", "ssh://", "https://", "repository.url", "docs/checkout.md"} {
		if !strings.Contains(condition.Message, want) {
			t.Errorf("the message %q does not say %q", condition.Message, want)
		}
	}
}

var _ = Describe("A Treebind whose repository address has the scheme http", func() {
	const (
		name      = "scheme-resource"
		namespace = "default"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}
	syncKey := types.NamespacedName{Name: name + "-sync", Namespace: namespace}
	keysKey := types.NamespacedName{Name: name + "-sync-keys", Namespace: namespace}
	var reconciler *TreebindReconciler

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	get := func() *treebindv1alpha1.Treebind {
		GinkgoHelper()
		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return got
	}

	condition := func(conditionType string) *metav1.Condition {
		GinkgoHelper()
		found := meta.FindStatusCondition(get().Status.Conditions, conditionType)
		Expect(found).NotTo(BeNil(), conditionType)
		return found
	}

	setURL := func(address string) {
		GinkgoHelper()
		resource := get()
		resource.Spec.Repository.URL = address
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())
	}

	statefulSetExists := func() bool {
		GinkgoHelper()
		err := k8sClient.Get(ctx, syncKey, &appsv1.StatefulSet{})
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		return err == nil
	}

	serviceExists := func() bool {
		GinkgoHelper()
		err := k8sClient.Get(ctx, syncKey, &corev1.Service{})
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		return err == nil
	}

	BeforeEach(func() {
		metrics, err := NewMetrics(prometheus.NewRegistry())
		Expect(err).NotTo(HaveOccurred())
		reconciler = &TreebindReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			AgentImage:    testImage,
			StatusIssuer:  testIssuer,
			StatusSubject: testSubject,
			StatusKeys:    newTestPublisher(),
			Metrics:       metrics,
		}
		Expect(k8sClient.Create(ctx, newTreebind(name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		// envtest has no garbage collector to remove what the Treebind owns.
		sync := metav1.ObjectMeta{Name: syncKey.Name, Namespace: namespace}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: sync}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: sync}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: keysKey.Name, Namespace: namespace},
		}))).To(Succeed())
	})

	DescribeTable("gets no sync pod, and Ready says why at the first reconcile",
		func(address string) {
			setURL(address)

			result := reconcileOnce()

			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "it is looked at again, as every other Treebind is")
			Expect(statefulSetExists()).To(BeFalse(), "a sync pod for it would exit at start for ever")
			Expect(serviceExists()).To(BeFalse())
			got := get()
			ready := condition(treebindv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonUnsupportedRepositoryScheme))
			Expect(ready.Message).To(And(
				ContainSubstring("http:// repositories are not supported"),
				ContainSubstring("ssh:// or https://"),
				ContainSubstring("docs/checkout.md"),
			))
			Expect(ready.Message).NotTo(ContainSubstring(address), "the message holds no address")
			Expect(ready.ObservedGeneration).To(Equal(got.Generation))
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			Expect(condition(treebindv1alpha1.ConditionSpecSupported).Status).To(Equal(metav1.ConditionTrue))
			for _, owned := range []string{
				treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict, treebindv1alpha1.ConditionAgentUnreachable,
			} {
				Expect(condition(owned).Status).To(Equal(metav1.ConditionUnknown), owned)
			}
		},
		Entry("with the scheme in lower case", httpRepositoryURL),
	)

	It("writes nothing when it is looked at again", func() {
		setURL(httpRepositoryURL)
		reconcileOnce()
		first := get()

		reconcileOnce()

		Expect(get().ResourceVersion).To(Equal(first.ResourceVersion))
	})

	It("gets its sync pod once the address is one that the agent takes", func() {
		setURL(httpRepositoryURL)
		reconcileOnce()
		Expect(condition(treebindv1alpha1.ConditionReady).Reason).To(Equal(treebindv1alpha1.ReasonUnsupportedRepositoryScheme))

		setURL(httpsRepositoryURL)
		reconcileOnce()

		Expect(statefulSetExists()).To(BeTrue())
		Expect(serviceExists()).To(BeTrue())
		ready := condition(treebindv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonSyncPodNotReady))
		Expect(ready.ObservedGeneration).To(Equal(get().Generation))
	})

	It("leaves the sync pod that a Treebind had as it was, and Ready says that the spec is not applied", func() {
		reconcileOnce()
		before := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, syncKey, before)).To(Succeed())

		setURL(httpRepositoryURL)
		reconcileOnce()

		after := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, syncKey, after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "the pod template is not changed to an address that the agent refuses")
		ready := condition(treebindv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonUnsupportedRepositoryScheme))
	})
})
