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
	"reflect"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The values of newAgentSpec that the tests look for, and the claim that has no counterpart in the pod.
const (
	testLabelKey        = "team"
	testLabelValue      = "storage"
	testAnnotationKey   = "owner"
	testAnnotationValue = "storage-team"
	testResourceClaim   = "gpu"
)

func quantities(pairs ...string) corev1.ResourceList {
	list := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		list[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return list
}

// sameResources compares two lists by value, as the API server does, and not by how a quantity is written.
func sameResources(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for name, q := range a {
		other, ok := b[name]
		if !ok || q.Cmp(other) != 0 {
			return false
		}
	}
	return true
}

func TestAgentResourcesCombineWithTheDefaults(t *testing.T) {
	defaults := corev1.ResourceRequirements{
		Requests: quantities("cpu", "10m", "memory", "32Mi"),
		Limits:   quantities("memory", "128Mi"),
	}
	tests := []struct {
		name string
		set  *corev1.ResourceRequirements
		want corev1.ResourceRequirements
	}{
		{name: "nothing set is the defaults", want: defaults},
		{name: "empty lists are the defaults", set: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{},
		}, want: defaults},
		{name: "a request takes the place of the default of its resource", set: &corev1.ResourceRequirements{
			Requests: quantities("cpu", "100m"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "100m", "memory", "32Mi"), Limits: quantities("memory", "128Mi"),
		}},
		{name: "a memory request above the default limit raises the limit to it", set: &corev1.ResourceRequirements{
			Requests: quantities("memory", "256Mi"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "10m", "memory", "256Mi"), Limits: quantities("memory", "256Mi"),
		}},
		{name: "a memory limit below the default request lowers the request to it", set: &corev1.ResourceRequirements{
			Limits: quantities("memory", "16Mi"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "10m", "memory", "16Mi"), Limits: quantities("memory", "16Mi"),
		}},
		{name: "a cpu limit below the default request lowers the request to it", set: &corev1.ResourceRequirements{
			Limits: quantities("cpu", "5m"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "5m", "memory", "32Mi"), Limits: quantities("cpu", "5m", "memory", "128Mi"),
		}},
		{name: "a request above a limit, both set, is left for the API server to refuse", set: &corev1.ResourceRequirements{
			Requests: quantities("memory", "512Mi"), Limits: quantities("memory", "64Mi"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "10m", "memory", "512Mi"), Limits: quantities("memory", "64Mi"),
		}},
		{name: "another resource is passed on", set: &corev1.ResourceRequirements{
			Requests: quantities("ephemeral-storage", "1Gi"), Limits: quantities("ephemeral-storage", "2Gi"),
		}, want: corev1.ResourceRequirements{
			Requests: quantities("cpu", "10m", "memory", "32Mi", "ephemeral-storage", "1Gi"),
			Limits:   quantities("memory", "128Mi", "ephemeral-storage", "2Gi"),
		}},
		{name: "claims have no counterpart in the pod and are left out", set: &corev1.ResourceRequirements{
			Claims: []corev1.ResourceClaim{{Name: testResourceClaim}},
		}, want: defaults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var setCopy *corev1.ResourceRequirements
			if tt.set != nil {
				setCopy = tt.set.DeepCopy()
			}
			got := agentResources(tt.set)
			if !sameResources(got.Requests, tt.want.Requests) || !sameResources(got.Limits, tt.want.Limits) || len(got.Claims) != 0 {
				t.Errorf("agentResources() = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.set, setCopy) {
				t.Errorf("agentResources() changed the spec: %+v, was %+v", tt.set, setCopy)
			}
		})
	}
}

// A spec.agent that sets nothing, or sets only what the operator does not apply, is the template without it, byte for
// byte, so that its pod is not replaced.
func TestAnAgentThatSetsNothingAppliedIsTheTemplateWithout(t *testing.T) {
	without := mustDesiredStatefulSet(t, newTreebind(testTreebindName), testImage).Annotations[templateHashAnnotation]
	fsGroup := int64(2000)
	for name, agent := range map[string]*treebindv1alpha1.AgentSpec{
		"an empty block": {},
		"empty collections": {
			Resources: &corev1.ResourceRequirements{}, NodeSelector: map[string]string{}, Tolerations: []corev1.Toleration{},
			ImagePullSecrets: []corev1.LocalObjectReference{}, Labels: map[string]string{}, Annotations: map[string]string{},
		},
		"only what is not applied": {
			FSGroup: &fsGroup, Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: testResourceClaim}}},
		},
	} {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Agent = agent
		if got := mustDesiredStatefulSet(t, treebind, testImage).Annotations[templateHashAnnotation]; got != without {
			t.Errorf("%s: hash %s, want %s, the one without spec.agent", name, got, without)
		}
	}
}

func TestTheAgentOverridesReachTheSyncPod(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Agent = newAgentSpec()
	agent := treebind.Spec.Agent
	statefulSet := mustDesiredStatefulSet(t, treebind, testImage)
	pod := statefulSet.Spec.Template
	spec := pod.Spec

	if !reflect.DeepEqual(spec.NodeSelector, agent.NodeSelector) {
		t.Errorf("nodeSelector = %v, want %v", spec.NodeSelector, agent.NodeSelector)
	}
	if !reflect.DeepEqual(spec.Tolerations, agent.Tolerations) {
		t.Errorf("tolerations = %v, want %v", spec.Tolerations, agent.Tolerations)
	}
	if spec.PriorityClassName != agent.PriorityClassName {
		t.Errorf("priorityClassName = %q, want %q", spec.PriorityClassName, agent.PriorityClassName)
	}
	if !reflect.DeepEqual(spec.ImagePullSecrets, agent.ImagePullSecrets) {
		t.Errorf("imagePullSecrets = %v, want %v", spec.ImagePullSecrets, agent.ImagePullSecrets)
	}
	if pod.Labels[testLabelKey] != testLabelValue || pod.Annotations[testAnnotationKey] != testAnnotationValue {
		t.Errorf("labels %v and annotations %v miss the ones of spec.agent", pod.Labels, pod.Annotations)
	}

	security := spec.SecurityContext
	if security.RunAsUser == nil || *security.RunAsUser != *agent.RunAsUser ||
		security.RunAsGroup == nil || *security.RunAsGroup != *agent.RunAsGroup {
		t.Errorf("pod security context %+v, want runAsUser %d and runAsGroup %d", security, *agent.RunAsUser, *agent.RunAsGroup)
	}
	if security.FSGroup != nil || security.FSGroupChangePolicy != nil {
		t.Errorf("fsGroup %v and its policy %v are set: the agent refuses its own directory after the kubelet's walk "+
			"(docs/adr/0044), so fsGroup is not applied", security.FSGroup, security.FSGroupChangePolicy)
	}
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.SeccompProfile == nil ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v lost runAsNonRoot or the RuntimeDefault seccomp profile", security)
	}
	baseline := mustDesiredStatefulSet(t, newTreebind(testTreebindName), testImage).Spec.Template.Spec.Containers[0]
	container := spec.Containers[0]
	if !reflect.DeepEqual(container.SecurityContext, baseline.SecurityContext) {
		t.Errorf("container security context %+v, want the one without spec.agent, %+v", container.SecurityContext, baseline.SecurityContext)
	}
	if got := container.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("cpu request = %s, want the 100m of spec.agent", got.String())
	}

	// User 1000 has no entry in the image's passwd file, so the pod gets one (docs/adr/0042), with the resources of the
	// sync container.
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != passwdContainerName {
		t.Fatalf("init containers = %v, want the passwd one for a user other than %d", spec.InitContainers, imageUserID)
	}
	if !reflect.DeepEqual(spec.InitContainers[0].Resources, container.Resources) {
		t.Errorf("passwd resources = %+v, want the ones of the sync container, %+v", spec.InitContainers[0].Resources, container.Resources)
	}
}

func TestThePasswdFileComesWithAUserOtherThanTheImagesOne(t *testing.T) {
	user := func(id int64) *treebindv1alpha1.AgentSpec { return &treebindv1alpha1.AgentSpec{RunAsUser: &id} }
	group := int64(1000)
	for name, tt := range map[string]struct {
		agent     *treebindv1alpha1.AgentSpec
		arbitrary bool
		want      bool
	}{
		"no spec.agent":                         {want: false},
		"the image's user":                      {agent: user(imageUserID), want: false},
		"only a group":                          {agent: &treebindv1alpha1.AgentSpec{RunAsGroup: &group}, want: false},
		"another user":                          {agent: user(1000), want: true},
		"another user with the setting on":      {agent: user(1000), arbitrary: true, want: true},
		"the image's user with the setting on":  {agent: user(imageUserID), arbitrary: true, want: true},
		"no spec.agent with the setting on too": {arbitrary: true, want: true},
	} {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Agent = tt.agent
		settings := agentSettings{Image: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}
		settings.ArbitraryUserID = tt.arbitrary
		statefulSet, err := desiredStatefulSet(treebind, settings)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(statefulSet.Spec.Template.Spec.InitContainers) == 1; got != tt.want {
			t.Errorf("%s: passwd init container = %v, want %v", name, got, tt.want)
		}
	}
}

// What the operator sets itself is not overridden by spec.agent: the labels that select the pod, the annotation of the
// cluster autoscaler of the passwd file, and the security context of the container.
func TestSpecAgentDoesNotOverrideWhatTheOperatorSets(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	other := int64(1000)
	treebind.Spec.Agent = &treebindv1alpha1.AgentSpec{
		RunAsUser: &other,
		Labels: map[string]string{
			"app.kubernetes.io/instance": "someone-else", "app.kubernetes.io/managed-by": "me", testLabelKey: testLabelValue,
		},
		Annotations: map[string]string{safeToEvictLocalVolumes: "data", testAnnotationKey: testAnnotationValue},
	}
	statefulSet := mustDesiredStatefulSet(t, treebind, testImage)
	pod := statefulSet.Spec.Template

	for key, value := range objectLabels(treebind) {
		if pod.Labels[key] != value {
			t.Errorf("label %s = %q, want the operator's %q", key, pod.Labels[key], value)
		}
	}
	if pod.Labels[testLabelKey] != testLabelValue {
		t.Errorf("label %s = %q, want %s", testLabelKey, pod.Labels[testLabelKey], testLabelValue)
	}
	if got := pod.Annotations[safeToEvictLocalVolumes]; got != passwdVolumeName {
		t.Errorf("annotation %s = %q, want the operator's %q", safeToEvictLocalVolumes, got, passwdVolumeName)
	}
	if !reflect.DeepEqual(statefulSet.Spec.Selector.MatchLabels, podLabels(treebind)) {
		t.Errorf("selector = %v, want %v", statefulSet.Spec.Selector.MatchLabels, podLabels(treebind))
	}
}

// A user without a group would run in group 0, the default of the runtime, and not in the group 65532 that the files of
// the claim are shared with (docs/pod-security.md).
func TestAUserWithoutAGroupRunsInTheImagesGroup(t *testing.T) {
	user, group := int64(1000), int64(3000)
	for name, tt := range map[string]struct {
		agent     *treebindv1alpha1.AgentSpec
		wantGroup *int64
	}{
		"no user and no group": {agent: &treebindv1alpha1.AgentSpec{}},
		"a user only":          {agent: &treebindv1alpha1.AgentSpec{RunAsUser: &user}, wantGroup: new(imageGroupID)},
		"a user and a group":   {agent: &treebindv1alpha1.AgentSpec{RunAsUser: &user, RunAsGroup: &group}, wantGroup: &group},
		"a group only":         {agent: &treebindv1alpha1.AgentSpec{RunAsGroup: &group}, wantGroup: &group},
	} {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Agent = tt.agent
		got := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template.Spec.SecurityContext.RunAsGroup
		if !reflect.DeepEqual(got, tt.wantGroup) {
			t.Errorf("%s: runAsGroup = %v, want %v", name, got, tt.wantGroup)
		}
	}
}

// The annotations of the deprecated security fields would loosen the profile of the pod on a cluster that still reads
// them, and the sync pod keeps its own (docs/adr/0044). Every other annotation is passed on.
func TestSpecAgentAnnotationsCannotLoosenTheSecurityOfThePod(t *testing.T) {
	const unconfined = "unconfined"
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Agent = &treebindv1alpha1.AgentSpec{Annotations: map[string]string{
		"container.apparmor.security.beta.kubernetes.io/agent": unconfined,
		"seccomp.security.alpha.kubernetes.io/pod":             unconfined,
		"container.seccomp.security.alpha.kubernetes.io/agent": unconfined,
		testAnnotationKey: testAnnotationValue,
	}}
	annotations := mustDesiredStatefulSet(t, treebind, testImage).Spec.Template.Annotations
	if want := map[string]string{testAnnotationKey: testAnnotationValue}; !reflect.DeepEqual(annotations, want) {
		t.Errorf("annotations = %v, want %v", annotations, want)
	}
}

func TestRunsAsRoot(t *testing.T) {
	zero, other := int64(0), int64(1000)
	for name, tt := range map[string]struct {
		agent *treebindv1alpha1.AgentSpec
		want  bool
	}{
		"no spec.agent":  {},
		"no user":        {agent: &treebindv1alpha1.AgentSpec{RunAsGroup: &zero}},
		"another user":   {agent: &treebindv1alpha1.AgentSpec{RunAsUser: &other}},
		"user 0 is root": {agent: &treebindv1alpha1.AgentSpec{RunAsUser: &zero}, want: true},
	} {
		if got := runsAsRoot(tt.agent); got != tt.want {
			t.Errorf("%s: runsAsRoot() = %v, want %v", name, got, tt.want)
		}
	}
}

var _ = Describe("The spec.agent overrides of a Treebind", func() {
	const (
		name      = "agent-overrides"
		namespace = "default"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}
	syncKey := types.NamespacedName{Name: name + "-sync", Namespace: namespace}
	keysKey := types.NamespacedName{Name: name + "-sync-keys", Namespace: namespace}
	var reconciler *TreebindReconciler

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
	setAgent := func(agent *treebindv1alpha1.AgentSpec) {
		GinkgoHelper()
		resource := get()
		resource.Spec.Agent = agent
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())
	}
	statefulSet := func() *appsv1.StatefulSet {
		GinkgoHelper()
		got := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, syncKey, got)).To(Succeed())
		return got
	}

	BeforeEach(func() {
		metrics, err := NewMetrics(prometheus.NewRegistry())
		Expect(err).NotTo(HaveOccurred())
		reconciler = &TreebindReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), AgentImage: testImage,
			StatusIssuer: testIssuer, StatusSubject: testSubject, StatusKeys: newTestPublisher(), Metrics: metrics,
		}
		Expect(k8sClient.Create(ctx, newTreebind(name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		sync := metav1.ObjectMeta{Name: syncKey.Name, Namespace: namespace}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: sync}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: sync}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: keysKey.Name, Namespace: namespace},
		}))).To(Succeed())
	})

	It("gives the sync pod every field that it applies, which the API server takes, and reports only fsGroup", func() {
		setAgent(newAgentSpec())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		spec := statefulSet().Spec.Template.Spec
		Expect(spec.PriorityClassName).To(Equal("background"))
		Expect(*spec.SecurityContext.RunAsUser).To(Equal(int64(1000)))
		supported := condition(treebindv1alpha1.ConditionSpecSupported)
		Expect(supported.Status).To(Equal(metav1.ConditionFalse))
		Expect(supported.Message).To(HaveSuffix(": spec.agent.fsGroup"))
	})

	It("says in Ready what the API server refuses of the sync pod", func() {
		setAgent(&treebindv1alpha1.AgentSpec{Labels: map[string]string{testLabelKey: "not a label value"}})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())

		ready := condition(treebindv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
		Expect(ready.Message).To(ContainSubstring("spec.template.labels"))
	})

	It("makes no pod for user 0 and leaves the one there was, and Ready says why", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		before := statefulSet()

		root := int64(0)
		setAgent(&treebindv1alpha1.AgentSpec{RunAsUser: &root})
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		Expect(statefulSet().ResourceVersion).To(Equal(before.ResourceVersion))
		ready := condition(treebindv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(treebindv1alpha1.ReasonUnsupportedAgentUser))
		Expect(ready.Message).To(And(ContainSubstring("spec.agent.runAsUser"), ContainSubstring("root")))
		Expect(ready.ObservedGeneration).To(Equal(get().Generation))
	})
})
