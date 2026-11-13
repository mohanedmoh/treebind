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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

// fakeAgents plays the agents: it answers every poll with what a test says and keeps
// the targets it was asked about.
type fakeAgents struct {
	mu      sync.Mutex
	err     error
	targets []statusclient.Target
	// beforeAnswering runs when a poll is made, before it is answered.
	beforeAnswering func()
}

func (a *fakeAgents) set(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *fakeAgents) asked() []statusclient.Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]statusclient.Target(nil), a.targets...)
}

func (a *fakeAgents) Fetch(ctx context.Context, target statusclient.Target) (*status.Report, error) {
	a.mu.Lock()
	a.targets = append(a.targets, target)
	before, err := a.beforeAnswering, a.err
	a.mu.Unlock()
	if before != nil {
		before()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return &status.Report{Version: status.Version, Treebind: status.Treebind{Namespace: target.Namespace, Name: target.Name}}, nil
}

// refreshableKeys is a key source that counts the times it is told to read again, and
// can have other keys to give after that.
type refreshableKeys struct {
	staticKeys
	refreshes atomic.Int32
	// afterRefresh are the keys to give once Refresh has been called.
	afterRefresh []byte
}

func (k *refreshableKeys) Refresh() {
	k.refreshes.Add(1)
	if k.afterRefresh != nil {
		k.set(k.afterRefresh, nil)
	}
}

// The failures of a poll, as the client reports them.
var (
	errNoConnection  = &statusclient.Error{Kind: statusclient.KindConnectionFailed, Message: msgRefusedConnection}
	errTokenRefused  = &statusclient.Error{Kind: statusclient.KindTokenRefused, Message: msgTokenRefused}
	errPodNotRunning = &statusclient.Error{
		Kind: statusclient.KindNotRunning, Message: "the name of the sync pod does not resolve",
	}
)

var _ = Describe("The status poller", func() {
	const namespace = "default"

	var (
		ctx       = context.Background()
		clock     *fakeClock
		agents    *fakeAgents
		keys      *refreshableKeys
		publisher *StatusKeyPublisher
		poller    *StatusPollerReconciler
		treebind  *treebindv1alpha1.Treebind
		name      types.NamespacedName
		counter   int
	)

	BeforeEach(func() {
		counter++
		clock, agents = newFakeClock(), &fakeAgents{}
		keys = &refreshableKeys{}
		keys.set(testKeySet(), nil)
		publisher = &StatusKeyPublisher{Source: keys, now: clock.Now}
		poller = &StatusPollerReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Agents: agents, StatusKeys: publisher, now: clock.Now,
		}

		treebind = newTreebind(fmt.Sprintf("poll-%d", counter))
		Expect(k8sClient.Create(ctx, treebind)).To(Succeed())
		name = client.ObjectKeyFromObject(treebind)
		// The reconcile of the spec has been through once.
		Expect(k8sClient.Get(ctx, name, treebind)).To(Succeed())
		treebind.Status.ObservedGeneration = treebind.Generation
		Expect(k8sClient.Status().Update(ctx, treebind)).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, treebind))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: treebind.Name + "-sync-keys", Namespace: namespace},
		}))).To(Succeed())
	})

	poll := func() reconcile.Result {
		GinkgoHelper()
		result, err := poller.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	get := func() *treebindv1alpha1.Treebind {
		GinkgoHelper()
		got := &treebindv1alpha1.Treebind{}
		Expect(k8sClient.Get(ctx, name, got)).To(Succeed())
		return got
	}
	unreachable := func() *metav1.Condition {
		GinkgoHelper()
		return meta.FindStatusCondition(get().Status.Conditions, treebindv1alpha1.ConditionAgentUnreachable)
	}
	expectCondition := func(status metav1.ConditionStatus, reason string) {
		GinkgoHelper()
		condition := unreachable()
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(status), "reason %s: %s", condition.Reason, condition.Message)
		Expect(condition.Reason).To(Equal(reason), condition.Message)
	}
	within := func(delay, base time.Duration) {
		GinkgoHelper()
		Expect(delay).To(BeNumerically(">=", base*4/5))
		Expect(delay).To(BeNumerically("<=", base*6/5))
	}

	It("polls the agent of the Treebind, for the audience that agent accepts", func() {
		poll()

		Expect(agents.asked()).To(Equal([]statusclient.Target{{
			Namespace: namespace, Name: treebind.Name, Audience: statusAudience(treebind),
		}}))
	})

	It("says False when the agent answered with a report, and polls again in about 30 seconds", func() {
		result := poll()

		expectCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived)
		within(result.RequeueAfter, statusPollInterval)
		Expect(unreachable().ObservedGeneration).To(Equal(get().Generation))
	})

	It("does not poll every Treebind at the same moment", func() {
		delays := map[time.Duration]bool{}
		for range 30 {
			result := poll()
			within(result.RequeueAfter, statusPollInterval)
			delays[result.RequeueAfter] = true
		}

		Expect(len(delays)).To(BeNumerically(">", 1), "the wait is the same every time: %v", delays)
	})

	It("says Unknown for the first minutes of a failure and True after, then False once it answers", func() {
		agents.set(errNoConnection)

		result := poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
		within(result.RequeueAfter, statusPollRetryInterval)

		clock.Advance(statusUnreachableAfter - time.Second)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)

		clock.Advance(time.Second)
		result = poll()
		expectCondition(metav1.ConditionTrue, treebindv1alpha1.ReasonConnectionFailed)
		within(result.RequeueAfter, statusPollRetryInterval)

		agents.set(nil)
		result = poll()
		expectCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived)
		within(result.RequeueAfter, statusPollInterval)

		// The count of failures began again with the answer.
		agents.set(errNoConnection)
		clock.Advance(10 * time.Minute)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
	})

	It("keeps a condition that is True while the polls fail, whatever the reason", func() {
		agents.set(errNoConnection)
		poll()
		clock.Advance(statusUnreachableAfter)
		poll()
		expectCondition(metav1.ConditionTrue, treebindv1alpha1.ReasonConnectionFailed)

		agents.set(&statusclient.Error{Kind: statusclient.KindUnexpectedResponse, Message: msgAnswered503})
		poll()

		expectCondition(metav1.ConditionTrue, treebindv1alpha1.ReasonUnexpectedResponse)
	})

	It("does not count a pod that does not exist yet as the agent failing to answer", func() {
		agents.set(errNoConnection)
		poll()
		clock.Advance(2 * time.Minute)

		agents.set(errPodNotRunning)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonAgentNotRunning)

		// The pod resolves now and refuses the connection while its container starts.
		// The two minutes before were not this failure.
		agents.set(errNoConnection)
		clock.Advance(2 * time.Minute)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
	})

	It("says Unknown and not True when the operator has no token, however long", func() {
		agents.set(&statusclient.Error{Kind: statusclient.KindTokenUnavailable, Message: "no token"})
		poll()
		clock.Advance(time.Hour)
		poll()

		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonTokenUnavailable)
	})

	It("reads the keys again when a token is refused, and puts the new ones where the agent reads them", func() {
		poll() // the ConfigMap is made with the keys of the cluster
		rotated := jwksOf("rotated", &testSigningKey().PublicKey)
		keys.afterRefresh = rotated
		agents.set(errTokenRefused)

		poll()

		Expect(keys.refreshes.Load()).To(Equal(int32(1)))
		configMap := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: treebind.Name + "-sync-keys"}, configMap)).To(Succeed())
		Expect(configMap.Data).To(Equal(map[string]string{statusKeysFile: string(rotated)}))
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
	})

	It("does not ask for the keys again when the agent answered", func() {
		poll()

		Expect(keys.refreshes.Load()).To(BeZero())
	})

	It("says that the keys are the trouble when the token is refused and they cannot be put in place", func() {
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: treebind.Name + "-sync-keys", Namespace: namespace},
			Data:       map[string]string{statusKeysFile: foreignData},
		})).To(Succeed())
		agents.set(errTokenRefused)

		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonKeysUnavailable)

		// It is not the agent failing to answer, so no time makes it True.
		clock.Advance(time.Hour)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonKeysUnavailable)
		Expect(unreachable().Message).To(ContainSubstring(treebind.Name + "-sync-keys"))

		// Once the ConfigMap is out of the way the keys are put in place on the next
		// poll, and a refusal that goes on is the agent's, counted from now.
		Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: treebind.Name + "-sync-keys", Namespace: namespace},
		})).To(Succeed())
		clock.Advance(statusKeysRepublish)
		poll()
		expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
		clock.Advance(statusUnreachableAfter)
		poll()
		expectCondition(metav1.ConditionTrue, treebindv1alpha1.ReasonTokenRefused)
	})

	It("puts the keys where the agent reads them as part of a poll", func() {
		poll()

		configMap := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: treebind.Name + "-sync-keys"}, configMap)).To(Succeed())
		Expect(configMap.Data).To(HaveKey(statusKeysFile))
	})

	It("does not poll a Treebind that the reconcile of the spec has not been through", func() {
		fresh := get()
		fresh.Status.ObservedGeneration = 0
		Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

		result := poll()

		Expect(agents.asked()).To(BeEmpty())
		Expect(result.RequeueAfter).To(Equal(statusPollNotReconciledWait))
		Expect(unreachable()).To(BeNil())
	})

	It("writes the status once when polls find the same thing, and again only when the finding changes", func() {
		var writes atomic.Int32
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		poller.Client = interceptor.NewClient(watching, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				writes.Add(1)
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		})

		agents.set(errNoConnection)
		poll()
		clock.Advance(time.Second)
		poll()
		poll()
		Expect(writes.Load()).To(Equal(int32(1)), "a poll that finds what the last one found writes the status again")

		agents.set(nil)
		poll()
		Expect(writes.Load()).To(Equal(int32(2)))
		poll()
		Expect(writes.Load()).To(Equal(int32(2)))
	})

	It("does not poll a Treebind that is being deleted, and forgets it", func() {
		agents.set(errNoConnection)
		poll()
		Expect(poller.failingSince).NotTo(BeEmpty())
		asked := len(agents.asked())

		held := get()
		controllerutil.AddFinalizer(held, "test.example.org/hold")
		Expect(k8sClient.Update(ctx, held)).To(Succeed())
		Expect(k8sClient.Delete(ctx, held)).To(Succeed())
		Expect(get().DeletionTimestamp.IsZero()).To(BeFalse())

		result := poll()

		Expect(result).To(Equal(reconcile.Result{}))
		Expect(agents.asked()).To(HaveLen(asked))
		Expect(poller.failingSince).To(BeEmpty())

		release := get()
		controllerutil.RemoveFinalizer(release, "test.example.org/hold")
		Expect(k8sClient.Update(ctx, release)).To(Succeed())
	})

	It("changes no condition but its own", func() {
		resource := get()
		for _, condition := range []metav1.Condition{
			{Type: treebindv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: treebindv1alpha1.ReasonSyncPodReady, Message: "1 of 1 sync pods ready"},
			{Type: treebindv1alpha1.ConditionSynced, Status: metav1.ConditionTrue, Reason: reasonInSync},
			{Type: treebindv1alpha1.ConditionConflict, Status: metav1.ConditionFalse, Reason: "NoConflict"},
			{Type: treebindv1alpha1.ConditionSpecSupported, Status: metav1.ConditionTrue, Reason: treebindv1alpha1.ReasonSupported},
		} {
			condition.ObservedGeneration = resource.Generation
			meta.SetStatusCondition(&resource.Status.Conditions, condition)
		}
		Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
		before := get().Status.Conditions

		agents.set(errNoConnection)
		poll()
		clock.Advance(statusUnreachableAfter)
		poll()

		var others []metav1.Condition
		for _, condition := range get().Status.Conditions {
			if condition.Type != treebindv1alpha1.ConditionAgentUnreachable {
				others = append(others, condition)
			}
		}
		Expect(others).To(Equal(before), "the poller changed a condition that is not its own")
		Expect(get().Status.ObservedGeneration).To(Equal(get().Generation))
	})

	It("forgets a Treebind that is gone, so that the count of a new one with its name begins at zero", func() {
		agents.set(errNoConnection)
		poll()
		clock.Advance(time.Hour)
		Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())

		result := poll()

		Expect(result).To(Equal(reconcile.Result{}))
		Expect(poller.failingSince).To(BeEmpty())
	})

	It("does not turn the stop of the manager into a failure of the agent", func() {
		stopping, stop := context.WithCancel(ctx)
		defer stop()
		agents.beforeAnswering = stop

		result, err := poller.Reconcile(stopping, reconcile.Request{NamespacedName: name})

		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		Expect(unreachable()).To(BeNil())
		Expect(poller.failingSince).To(BeEmpty())
	})

	It("writes no condition for a name that cannot name a sync pod, and tries again", func() {
		agents.set(errors.New("the Treebind cannot name a sync pod"))

		result := poll()

		Expect(unreachable()).To(BeNil())
		within(result.RequeueAfter, statusPollRetryInterval)
	})

	It("keeps what the poll saw on the way out of the condition", func() {
		agents.set(&statusclient.Error{
			Kind: statusclient.KindConnectionFailed, Message: msgRefusedConnection,
			Err: errors.New("dial tcp 10.244.0.7:8080: operator-token-s3cret"),
		})
		poll()
		clock.Advance(statusUnreachableAfter)
		poll()

		message := unreachable().Message
		Expect(message).NotTo(ContainSubstring("10.244.0.7"))
		Expect(message).NotTo(ContainSubstring("s3cret"))
		Expect(strings.TrimSpace(message)).NotTo(BeEmpty())
	})

	It("tries again soon when the status was changed by someone else while it held it", func() {
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		conflicting := interceptor.NewClient(watching, interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(treebindv1alpha1.GroupVersion.WithResource("treebinds").GroupResource(), treebind.Name,
					errors.New("changed meanwhile"))
			},
		})
		poller.Client = conflicting

		result, pollErr := poller.Reconcile(ctx, reconcile.Request{NamespacedName: name})

		Expect(pollErr).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(statusPollConflictWait))
	})

	It("returns the error when the status cannot be written for another reason", func() {
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		poller.Client = interceptor.NewClient(watching, interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewInternalError(errors.New("etcd is slow"))
			},
		})

		_, pollErr := poller.Reconcile(ctx, reconcile.Request{NamespacedName: name})

		Expect(pollErr).To(MatchError(ContainSubstring("update status of Treebind " + name.String())))
		Expect(pollErr).To(MatchError(ContainSubstring("etcd is slow")))
	})

	It("does nothing for a Treebind that no longer exists", func() {
		Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())

		result, err := poller.Reconcile(ctx, reconcile.Request{NamespacedName: name})

		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		Expect(agents.asked()).To(BeEmpty())
	})
})

func TestThePollerWillNotStartWithoutWhatItNeeds(t *testing.T) {
	keys := newTestPublisher()
	for name, poller := range map[string]*StatusPollerReconciler{
		"no reader of reports": {StatusKeys: keys},
		"no keys":              {Agents: &fakeAgents{}},
		"keys of nothing":      {Agents: &fakeAgents{}, StatusKeys: &StatusKeyPublisher{}},
	} {
		// The manager is nil, so an error of the controller builder would pass for the
		// refusal: the text has to be the operator's own.
		err := poller.SetupWithManager(nil)
		if err == nil || (!strings.Contains(err.Error(), "reader of the sync agents") &&
			!strings.Contains(err.Error(), "public signing keys")) {
			t.Errorf("%s: SetupWithManager returned %v, want the refusal of the operator", name, err)
		}
	}
}
