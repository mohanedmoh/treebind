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
	"errors"
	"fmt"
	"reflect"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The sync pod of a claim that one node mounts at a time has to run on the node of the application (FR-25, R-4):
// required pod affinity to the pods that spec.placement.appSelector selects, and never a nodeName (docs/adr/0046).

const (
	appLabelKey   = "app"
	appLabelValue = "grafana"
	otherAppValue = "loki"
)

func appSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: appLabelValue}}
}

func treebindBesideItsApplication() *treebindv1alpha1.Treebind {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Placement.AppSelector = appSelector()
	return treebind
}

func modes(list ...corev1.PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	return list
}

// accessModeCases says for each set of access modes a claim can have whether the sync pod has to run beside the
// application.
var accessModeCases = []struct {
	name   string
	modes  []corev1.PersistentVolumeAccessMode
	beside bool
}{
	{"ReadWriteOnce", modes(corev1.ReadWriteOnce), true},
	{"ReadWriteOncePod", modes(corev1.ReadWriteOncePod), true},
	{"ReadWriteOnce and ReadOnlyMany", modes(corev1.ReadWriteOnce, corev1.ReadOnlyMany), true},
	{"ReadWriteMany", modes(corev1.ReadWriteMany), false},
	{"ReadWriteOnce and ReadWriteMany", modes(corev1.ReadWriteOnce, corev1.ReadWriteMany), false},
	{"ReadOnlyMany", modes(corev1.ReadOnlyMany), false},
	{"a claim that is not there", nil, true},
}

// besideTheApplication is the affinity that the sync pod of treebindBesideItsApplication has: the pods that the
// selector selects, less the sync pods, which would otherwise let one satisfy its own affinity.
func besideTheApplication() *corev1.Affinity {
	return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{appLabelKey: appLabelValue},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app.kubernetes.io/managed-by", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"treebind-operator"},
				}},
			},
			TopologyKey: "kubernetes.io/hostname",
		}},
	}}
}

func statefulSetFor(
	t *testing.T, treebind *treebindv1alpha1.Treebind, claimModes ...corev1.PersistentVolumeAccessMode,
) *appsv1.StatefulSet {
	t.Helper()
	statefulSet, err := desiredStatefulSet(treebind,
		agentSettings{Image: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}, claimModes...)
	if err != nil {
		t.Fatalf("desiredStatefulSet() error = %v", err)
	}
	return statefulSet
}

func TestTheSyncPodRunsBesideTheApplicationUnlessManyNodesCanMountTheClaim(t *testing.T) {
	for _, tt := range accessModeCases {
		t.Run(tt.name, func(t *testing.T) {
			treebind := treebindBesideItsApplication()

			pod := statefulSetFor(t, treebind, tt.modes...).Spec.Template.Spec

			var want *corev1.Affinity
			if tt.beside {
				want = besideTheApplication()
			}
			if !reflect.DeepEqual(pod.Affinity, want) {
				t.Errorf("affinity = %+v, want %+v", pod.Affinity, want)
			}
			if pod.NodeName != "" || len(pod.NodeSelector) != 0 {
				t.Errorf("nodeName = %q and nodeSelector = %v, want neither: the node is the scheduler's choice",
					pod.NodeName, pod.NodeSelector)
			}
			if !reflect.DeepEqual(treebind.Spec.Placement.AppSelector, appSelector()) {
				t.Errorf("the selector of the spec was changed to %+v", treebind.Spec.Placement.AppSelector)
			}
		})
	}
}

func TestWithoutAnAppSelectorTheTemplateDoesNotFollowTheClaim(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	without := statefulSetFor(t, treebind)

	for _, tt := range accessModeCases {
		got := statefulSetFor(t, treebind, tt.modes...)
		if got.Spec.Template.Spec.Affinity != nil || !reflect.DeepEqual(got.Spec.Template, without.Spec.Template) {
			t.Errorf("%s: the template of a Treebind without appSelector changed: affinity = %+v",
				tt.name, got.Spec.Template.Spec.Affinity)
		}
	}
}

// Kubernetes lets the first pod of a group satisfy its own required affinity: when no pod in the cluster matches a term
// and the incoming pod does, it is placed anywhere. A selector that matches the sync pod, by the operator's labels or
// by those of spec.agent, would so place it with no application at all.
func TestTheAffinityNeverMatchesASyncPod(t *testing.T) {
	for name, tt := range map[string]struct {
		selector    metav1.LabelSelector
		agentLabels map[string]string
	}{
		"a label that the operator gives every sync pod": {
			selector: metav1.LabelSelector{MatchLabels: podLabels(newTreebind(testTreebindName))},
		},
		"a label that spec.agent gives the sync pod too": {
			selector:    *appSelector(),
			agentLabels: map[string]string{appLabelKey: appLabelValue},
		},
		"an expression that pods without the label match": {
			selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Placement.AppSelector = &tt.selector
			if tt.agentLabels != nil {
				treebind.Spec.Agent = &treebindv1alpha1.AgentSpec{Labels: tt.agentLabels}
			}

			template := statefulSetFor(t, treebind, corev1.ReadWriteOnce).Spec.Template

			term := template.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
			selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
			if err != nil {
				t.Fatal(err)
			}
			if selector.Matches(labels.Set(template.Labels)) {
				t.Errorf("the affinity %s matches the labels %v of the sync pod itself", selector, template.Labels)
			}
		})
	}
}

// The status poller takes a report for one of the current spec by the hash that the agent was given, and does not read
// the claim. So the hash does not cover the affinity, which is where the pod runs and nothing that the agent does, and
// the annotation of the StatefulSet, which decides whether the template is written, does.
func TestTheAffinityChangesTheTemplateAndNotTheHashThatThePollerExpects(t *testing.T) {
	poller := &StatusPollerReconciler{AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}
	treebind := treebindBesideItsApplication()
	expected, err := agentTemplateHash(treebind, poller.agentSettings())
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range accessModeCases {
		if got := templateHashVariable(t, statefulSetFor(t, treebind, tt.modes...)); got != expected {
			t.Errorf("%s: the agent is given %s, and the poller expects %s", tt.name, got, expected)
		}
	}
	beside := statefulSetFor(t, treebind, corev1.ReadWriteOnce).Annotations[templateHashAnnotation]
	anywhere := statefulSetFor(t, treebind, corev1.ReadWriteMany).Annotations[templateHashAnnotation]
	other := treebindBesideItsApplication()
	other.Spec.Placement.AppSelector.MatchLabels[appLabelKey] = otherAppValue
	otherSelector := statefulSetFor(t, other, corev1.ReadWriteOnce).Annotations[templateHashAnnotation]
	if beside == anywhere || beside == otherSelector {
		t.Errorf("annotations: beside %s, anywhere %s, another selector %s: a change of the affinity has to rewrite "+
			"the template", beside, anywhere, otherSelector)
	}
}

// A sync pod with the affinity waits for a pod of the application, which the scheduler's words alone ("didn't match pod
// affinity rules") do not say.
func TestReadySaysThatAPendingSyncPodWaitsForTheApplication(t *testing.T) {
	const words = "0/3 nodes are available: 3 node(s) didn't match pod affinity rules."
	reconciler := &TreebindReconciler{
		APIReader: &podReader{pod: pendingSyncPod(corev1.ConditionFalse, corev1.PodReasonUnschedulable, words)},
	}
	statefulSet := statefulSetFor(t, treebindBesideItsApplication(), corev1.ReadWriteOnce)
	notReady := metav1.Condition{
		Type: treebindv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: treebindv1alpha1.ReasonSyncPodNotReady, Message: messageNoPodReady,
	}

	got := reconciler.whySyncPodNotReady(context.Background(), statefulSet, notReady)

	assertReady(t, got, metav1.ConditionFalse, treebindv1alpha1.ReasonSyncPodUnschedulable,
		messageNoPodReady+": the sync pod "+podOfSync+" is Pending, because the scheduler cannot place it, and it may "+
			"only run on the node of a pod that spec.placement.appSelector selects: "+words)
}

// claimReader reads from the API server, counts the claims it is asked for, and fails them with err when it is set.
type claimReader struct {
	client.Reader
	claims int
	err    error
}

func (r *claimReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
		r.claims++
		if r.err != nil {
			return r.err
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

var _ = Describe("The placement of the sync pod", func() {
	const name, namespace = "placed", "default"

	var (
		reader     *claimReader
		reconciler *TreebindReconciler
		claimName  string
		claimCount int
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}
	syncKey := types.NamespacedName{Name: name + "-sync", Namespace: namespace}

	createClaim := func(accessModes ...corev1.PersistentVolumeAccessMode) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: accessModes,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})).To(Succeed())
	}
	// deleteClaim takes the claim away. envtest runs no controller that would lift the finalizer of a claim in use.
	deleteClaim := func() {
		GinkgoHelper()
		claim := &corev1.PersistentVolumeClaim{}
		claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
		if err := k8sClient.Get(ctx, claimKey, claim); apierrors.IsNotFound(err) {
			return
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			claim.Finalizers = nil
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, claim))).To(Succeed())
		}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, claimKey, claim))).To(BeTrue())
	}
	createTreebind := func(selector *metav1.LabelSelector) {
		GinkgoHelper()
		treebind := newTreebind(name)
		treebind.Spec.Target.ClaimName = claimName
		treebind.Spec.Placement.AppSelector = selector
		Expect(k8sClient.Create(ctx, treebind)).To(Succeed())
	}
	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	statefulSet := func() *appsv1.StatefulSet {
		GinkgoHelper()
		got := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, syncKey, got)).To(Succeed())
		return got
	}
	affinity := func() *corev1.Affinity {
		GinkgoHelper()
		pod := statefulSet().Spec.Template.Spec
		Expect(pod.NodeName).To(BeEmpty())
		return pod.Affinity
	}
	ready := func() *metav1.Condition {
		GinkgoHelper()
		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		return meta.FindStatusCondition(got.Status.Conditions, treebindv1alpha1.ConditionReady)
	}

	BeforeEach(func() {
		claimCount++
		claimName = fmt.Sprintf("placement-data-%d", claimCount)
		metrics, err := NewMetrics(prometheus.NewRegistry())
		Expect(err).NotTo(HaveOccurred())
		reader = &claimReader{Reader: k8sClient}
		reconciler = &TreebindReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: reader, AgentImage: testImage,
			StatusIssuer: testIssuer, StatusSubject: testSubject, StatusKeys: newTestPublisher(), Metrics: metrics,
		}
	})

	AfterEach(func() {
		sync := metav1.ObjectMeta{Name: syncKey.Name, Namespace: syncKey.Namespace}
		for _, object := range []client.Object{
			&treebindv1alpha1.Treebind{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
			&appsv1.StatefulSet{ObjectMeta: sync},
			&corev1.Service{ObjectMeta: sync},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-sync-keys", Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, object))).To(Succeed())
		}
		deleteClaim()
	})

	DescribeTable("follows the access modes of the claim",
		func(accessMode corev1.PersistentVolumeAccessMode, beside bool) {
			createClaim(accessMode)
			createTreebind(appSelector())

			reconcileOnce()

			if beside {
				Expect(affinity()).To(Equal(besideTheApplication()))
			} else {
				Expect(affinity()).To(BeNil())
			}
		},
		Entry("ReadWriteOnce", corev1.ReadWriteOnce, true),
		Entry("ReadWriteOncePod", corev1.ReadWriteOncePod, true),
		Entry("ReadWriteMany", corev1.ReadWriteMany, false),
		Entry("ReadOnlyMany", corev1.ReadOnlyMany, false),
	)

	It("takes a claim that is not there for one that one node mounts, and follows the claim once it is", func() {
		createTreebind(appSelector())

		result := reconcileOnce()

		Expect(affinity()).To(Equal(besideTheApplication()))
		Expect(result.RequeueAfter).To(BeNumerically("<=", syncPodRecheck*6/5),
			"nothing watches the claim, so the pass has to be asked for again soon")

		createClaim(corev1.ReadWriteMany)
		reconcileOnce()

		Expect(affinity()).To(BeNil())

		deleteClaim()
		createClaim(corev1.ReadWriteOnce)
		reconcileOnce()

		Expect(affinity()).To(Equal(besideTheApplication()))
	})

	It("reads no claim for a Treebind without appSelector, and rewrites the template when one is added", func() {
		createClaim(corev1.ReadWriteOnce)
		createTreebind(nil)

		reconcileOnce()

		Expect(reader.claims).To(BeZero())
		Expect(affinity()).To(BeNil())

		treebind := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, key, treebind)).To(Succeed())
		treebind.Spec.Placement.AppSelector = appSelector()
		Expect(k8sClient.Update(ctx, treebind)).To(Succeed())
		reconcileOnce()

		Expect(affinity()).To(Equal(besideTheApplication()))

		written := statefulSet().ResourceVersion
		reconcileOnce()
		Expect(statefulSet().ResourceVersion).To(Equal(written), "a pass with nothing to do writes nothing")
	})

	It("says in Ready why the claim cannot be read, and changes no sync pod meanwhile", func() {
		createClaim(corev1.ReadWriteMany)
		createTreebind(appSelector())
		reconcileOnce()
		written := statefulSet().ResourceVersion

		reader.err = apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), claimName, errors.New("no rule"))
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got %v", err)
		Expect(ready().Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
		Expect(ready().Message).To(And(ContainSubstring("PersistentVolumeClaim "+namespace+"/"+claimName), ContainSubstring("forbidden")))
		Expect(statefulSet().ResourceVersion).To(Equal(written))
	})

	// The CRD only refuses a selector that selects everything. The rest of a selector is the API server's to judge,
	// which it does when the StatefulSet is written.
	It("says in Ready what the API server refuses of a selector", func() {
		createClaim(corev1.ReadWriteOnce)
		createTreebind(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: appLabelKey, Operator: "Matches", Values: []string{appLabelValue}},
		}})

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		Expect(err).To(HaveOccurred())
		Expect(ready().Reason).To(Equal(treebindv1alpha1.ReasonReconcileFailed))
		Expect(ready().Message).To(ContainSubstring("podAffinity"))
	})
})
