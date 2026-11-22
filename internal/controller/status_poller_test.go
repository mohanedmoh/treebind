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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/app"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

// fakeAgents plays the agents: it answers every poll with what a test says and keeps
// the targets it was asked about.
type fakeAgents struct {
	mu      sync.Mutex
	err     error
	targets []statusclient.Target
	// report is the answer to a poll that does not fail. When it is nil the answer is
	// the report of an agent of the release before, which says nothing of the sync.
	report *status.Report
	// beforeAnswering runs when a poll is made, before it is answered.
	beforeAnswering func()
}

func (a *fakeAgents) set(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// answer makes the report the answer to the polls that do not fail.
func (a *fakeAgents) answer(report *status.Report) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.report = report
}

func (a *fakeAgents) asked() []statusclient.Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]statusclient.Target(nil), a.targets...)
}

func (a *fakeAgents) Fetch(ctx context.Context, target statusclient.Target) (*status.Report, error) {
	a.mu.Lock()
	a.targets = append(a.targets, target)
	before, err, report := a.beforeAnswering, a.err, a.report
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
	if report != nil {
		answered := *report
		answered.Checks = slices.Clone(report.Checks)
		return &answered, nil
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
		recorder  *capturedEvents
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
		recorder = &capturedEvents{}
		poller = &StatusPollerReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Agents: agents, StatusKeys: publisher, now: clock.Now,
			AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject,
			Recorder: recorder,
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
		// AgentUnreachable, Synced and Conflict are the poller's.
		others := func(conditions []metav1.Condition) []metav1.Condition {
			var kept []metav1.Condition
			for _, condition := range conditions {
				switch condition.Type {
				case treebindv1alpha1.ConditionAgentUnreachable, treebindv1alpha1.ConditionSynced,
					treebindv1alpha1.ConditionConflict:
				default:
					kept = append(kept, condition)
				}
			}
			return kept
		}
		before := others(get().Status.Conditions)
		Expect(before).To(HaveLen(2))

		agents.set(errNoConnection)
		poll()
		clock.Advance(statusUnreachableAfter)
		poll()
		Expect(meta.FindStatusCondition(get().Status.Conditions, treebindv1alpha1.ConditionSynced)).
			To(HaveField("Reason", reasonUnreachable))
		agents.set(nil)
		agents.answer(currentReport(get(), commitOne))
		poll()
		Expect(get().Status.LastSyncedCommit).To(Equal(commitOne))

		Expect(others(get().Status.Conditions)).To(Equal(before), "the poller changed a condition that is not its own")
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

	// What the report says of the sync goes into the same write as AgentUnreachable.
	// sync_conditions_test.go holds what each report makes; these hold that the poller
	// writes it, through the API server, and only when it changed.
	Context("with a report that says how the sync goes", func() {
		find := func(conditionType string) *metav1.Condition {
			GinkgoHelper()
			return meta.FindStatusCondition(get().Status.Conditions, conditionType)
		}
		expectSync := func(conditionType string, value metav1.ConditionStatus, reason string) {
			GinkgoHelper()
			condition := find(conditionType)
			Expect(condition).NotTo(BeNil(), "%s is not written", conditionType)
			Expect(condition.Status).To(Equal(value), "%s: %s %s", conditionType, condition.Reason, condition.Message)
			Expect(condition.Reason).To(Equal(reason), "%s: %s", conditionType, condition.Message)
			Expect(condition.ObservedGeneration).To(Equal(get().Generation), conditionType)
			Expect(condition.Message).NotTo(ContainSubstring(agentText), "%s carries text of the report", conditionType)
		}
		// countWrites makes the poller count the writes of the status that it makes.
		countWrites := func() *atomic.Int32 {
			GinkgoHelper()
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
			return &writes
		}

		It("says InSync and writes the commit that the volume and the repository agreed at", func() {
			agents.answer(currentReport(get(), commitOne))

			poll()

			expectCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionFalse, reasonNoConflict)
			Expect(find(treebindv1alpha1.ConditionSynced).Message).To(ContainSubstring(msgAgreedOne))
			stored := get().Status
			Expect(stored.LastSyncedCommit).To(Equal(commitOne))
			Expect(stored.LastSyncTime).NotTo(BeNil())
			Expect(stored.LastSyncTime.Time).To(BeTemporally("==", agreedAt))
		})

		It("writes nothing when the next polls find the same, though the API server keeps whole seconds", func() {
			writes := countWrites()
			report := currentReport(get(), commitOne)
			report.LastSynced.Time = agreedAt.Add(123456789 * time.Nanosecond)
			agents.answer(report)

			poll()
			Expect(writes.Load()).To(Equal(int32(1)))
			written := get()
			Expect(written.Status.LastSyncTime.Time).To(BeTemporally("==", agreedAt))

			clock.Advance(statusPollInterval)
			poll()
			// A pull that finds nothing new reports the same agreement at a later time.
			later := *report
			later.Time, later.CheckedAt = report.Time.Add(time.Minute), report.CheckedAt.Add(time.Minute)
			agents.answer(&later)
			poll()

			Expect(writes.Load()).To(Equal(int32(1)), "a poll that finds what the last one found writes the status again")
			Expect(get().ResourceVersion).To(Equal(written.ResourceVersion))
		})

		It("writes a newer agreement, and only that once", func() {
			writes := countWrites()
			agents.answer(currentReport(get(), commitOne))
			poll()
			newer := currentReport(get(), commitTwo)
			newer.LastSynced.Time, newer.LastSynced.By = agreedAt.Add(time.Hour), status.ByPush
			agents.answer(newer)

			poll()
			poll()

			Expect(writes.Load()).To(Equal(int32(2)))
			stored := get().Status
			Expect(stored.LastSyncedCommit).To(Equal(commitTwo))
			Expect(stored.LastSyncTime.Time).To(BeTemporally("==", agreedAt.Add(time.Hour)))
			Expect(find(treebindv1alpha1.ConditionSynced).Message).To(ContainSubstring(msgAgreedTwo))
		})

		It("says False with the reason of the step that failed, and keeps the last agreement", func() {
			agents.answer(currentReport(get(), commitOne))
			poll()
			agents.answer(currentReport(get(), commitOne,
				failedCheck(app.CheckPull, reasonTooManyDeletions), failedCheck(app.CheckConflict, reasonTooManyDeletions)))

			poll()

			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionFalse, reasonTooManyDeletions)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionTrue, reasonTooManyDeletions)
			Expect(find(treebindv1alpha1.ConditionSynced).Message).To(ContainSubstring(msgAgreedOne))
			Expect(get().Status.LastSyncedCommit).To(Equal(commitOne))
		})

		It("says SyncPending for an agent that has not seen the two agree since it started", func() {
			agents.answer(currentReport(get(), ""))

			poll()

			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionUnknown, reasonPending)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionUnknown, reasonPending)
			Expect(get().Status.LastSyncedCommit).To(BeEmpty())
			Expect(get().Status.LastSyncTime).To(BeNil())
		})

		It("says Unknown for both once the agent is unreachable, and keeps them while the polls only begin to fail", func() {
			agents.answer(currentReport(get(), commitOne))
			poll()
			agents.set(errNoConnection)

			poll()
			expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionFalse, reasonNoConflict)

			clock.Advance(statusUnreachableAfter)
			result := poll()
			expectCondition(metav1.ConditionTrue, treebindv1alpha1.ReasonConnectionFailed)
			within(result.RequeueAfter, statusPollRetryInterval)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionUnknown, reasonUnreachable)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionUnknown, reasonUnreachable)
			Expect(find(treebindv1alpha1.ConditionSynced).Message).To(Equal(msgUnreachable))
			Expect(get().Status.LastSyncedCommit).To(Equal(commitOne), "a poll that failed cleared the agreement")

			agents.set(nil)
			poll()
			expectCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
		})

		It("leaves them as they are while the operator cannot tell whether the agent answers", func() {
			agents.answer(currentReport(get(), commitOne))
			poll()
			agents.set(&statusclient.Error{Kind: statusclient.KindTokenUnavailable, Message: "no token"})

			clock.Advance(time.Hour)
			poll()

			expectCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonTokenUnavailable)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
		})

		It("says AgentOutdated for a report of another template, or of none, and takes no agreement from it", func() {
			report := currentReport(get(), commitOne)
			report.TemplateHash = hashOfBefore
			agents.answer(report)

			poll()

			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionUnknown, reasonOutdated)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionUnknown, reasonOutdated)
			Expect(get().Status.LastSyncedCommit).To(BeEmpty())

			// An agent of the release before reports no hash.
			report.TemplateHash = ""
			agents.answer(report)
			poll()
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionUnknown, reasonOutdated)
			Expect(get().Status.LastSyncedCommit).To(BeEmpty())
		})

		It("expects the hash of the new template once the spec changes it, and the generation follows", func() {
			before := currentReport(get(), commitOne)
			agents.answer(before)
			poll()
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)

			changed := get()
			changed.Spec.Sync.Interval = &metav1.Duration{Duration: 5 * time.Minute}
			Expect(k8sClient.Update(ctx, changed)).To(Succeed())
			Expect(currentReport(get(), commitOne).TemplateHash).NotTo(Equal(before.TemplateHash))

			// The agent still runs in the pod of the template before.
			poll()
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionUnknown, reasonOutdated)
			Expect(get().Status.LastSyncedCommit).To(Equal(commitOne))

			// The pod was replaced.
			agents.answer(currentReport(get(), commitOne))
			poll()
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)

			// A change of the spec that the agent does not see replaces no pod, and the report
			// of the same template is of the new generation.
			same := get()
			generation := same.Generation
			same.Spec.Target.MountPath = defaultMountPath
			Expect(k8sClient.Update(ctx, same)).To(Succeed())
			Expect(get().Generation).To(BeNumerically(">", generation))
			poll()
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
		})

		It("takes a report of the template that the reconcile of the spec wrote", func() {
			reconciler := &TreebindReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), AgentImage: testImage,
				StatusIssuer: testIssuer, StatusSubject: testSubject, StatusKeys: newTestPublisher(),
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			syncObject := types.NamespacedName{Namespace: namespace, Name: name.Name + "-sync"}
			statefulSet := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, syncObject, statefulSet)).To(Succeed())
			// envtest has no garbage collector to remove what the Treebind owns.
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, statefulSet)).To(Succeed())
				Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{
					Name: syncObject.Name, Namespace: namespace,
				}})).To(Succeed())
			})
			report := currentReport(get(), commitOne)
			report.TemplateHash = hashVariableOf(statefulSet.Spec.Template)
			agents.answer(report)

			poll()

			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
		})

		It("tries again after someone else wrote the status meanwhile, and loses neither condition", func() {
			agents.answer(currentReport(get(), commitOne))
			ready := metav1.Condition{
				Type: treebindv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: treebindv1alpha1.ReasonSyncPodReady, Message: "1 of 1 sync pods ready",
			}
			// The reconcile of the spec writes Ready while the poll waits for the agent.
			agents.beforeAnswering = func() {
				other := get()
				ready.ObservedGeneration = other.Generation
				meta.SetStatusCondition(&other.Status.Conditions, ready)
				Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
			}

			result := poll()

			Expect(result.RequeueAfter).To(Equal(statusPollConflictWait))
			Expect(unreachable()).To(BeNil())
			Expect(find(treebindv1alpha1.ConditionSynced)).To(BeNil())

			agents.beforeAnswering = nil
			clock.Advance(statusPollConflictWait)
			poll()

			expectCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived)
			expectSync(treebindv1alpha1.ConditionSynced, metav1.ConditionTrue, reasonInSync)
			expectSync(treebindv1alpha1.ConditionConflict, metav1.ConditionFalse, reasonNoConflict)
			Expect(get().Status.LastSyncedCommit).To(Equal(commitOne))
			Expect(find(treebindv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue), "the other write was lost")
		})
	})

	// sync_events_test.go holds what each change makes. These hold that the poller makes it from what it writes
	// through the API server, once for each change.
	Context("with events", func() {
		It("records an event for each change that it writes, and none for a poll that finds the same", func() {
			agents.answer(currentReport(get(), ""))
			poll()
			poll()
			agents.answer(currentReport(get(), commitOne))
			poll()
			poll()
			agents.answer(currentReport(get(), commitOne, failedCheck(app.CheckPush, reasonPushRejected)))
			poll()
			clock.Advance(statusPollInterval)
			poll()

			Expect(recorder.summaries()).To(Equal([]string{
				"Normal SyncPending Synced",
				"Normal InSync Synced",
				"Warning PushRejected Synced",
			}))
			stored := get()
			last := recorder.taken()[2]
			Expect(last.note).To(HavePrefix("Synced is False: The last push failed"))
			Expect(last.note).NotTo(ContainSubstring(agentText))
			Expect(last.regarding).To(Equal(name), "the event is not on the Treebind")
			Expect(last.resourceVersion).To(Equal(stored.ResourceVersion),
				"the event is not of the status that the poll wrote")
		})

		It("records the event of a change that lost a conflict with the poll that writes it", func() {
			agents.answer(currentReport(get(), commitOne))
			agents.beforeAnswering = func() {
				other := get()
				meta.SetStatusCondition(&other.Status.Conditions, metav1.Condition{
					Type: treebindv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
					Reason: treebindv1alpha1.ReasonSyncPodReady, Message: messagePodReady,
					ObservedGeneration: other.Generation,
				})
				Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
			}

			Expect(poll().RequeueAfter).To(Equal(statusPollConflictWait))
			Expect(recorder.summaries()).To(BeEmpty())

			agents.beforeAnswering = nil
			poll()
			poll()

			Expect(recorder.summaries()).To(Equal([]string{"Normal InSync Synced"}))
		})
	})
})

// capturedEvent is an event as the poller recorded it.
type capturedEvent struct {
	eventType, reason, action, note string
	regarding                       types.NamespacedName
	resourceVersion                 string
}

// capturedEvents records the events that it is given, as the recorder of the manager sends them to the API server.
type capturedEvents struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (c *capturedEvents) Eventf(regarding runtime.Object, _ runtime.Object, eventType, reason, action, note string,
	args ...any,
) {
	object := regarding.(client.Object)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{
		eventType: eventType, reason: reason, action: action, note: fmt.Sprintf(note, args...),
		regarding:       client.ObjectKeyFromObject(object),
		resourceVersion: object.GetResourceVersion(),
	})
}

func (c *capturedEvents) taken() []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// summaries are the type, the reason and the action of each event.
func (c *capturedEvents) summaries() []string {
	taken := c.taken()
	got := make([]string, 0, len(taken))
	for _, event := range taken {
		got = append(got, event.eventType+" "+event.reason+" "+event.action)
	}
	return got
}

// hashVariableOf returns the value of the variable of the agent that holds the hash of the pod template.
func hashVariableOf(template corev1.PodTemplateSpec) string {
	GinkgoHelper()
	for _, variable := range template.Spec.Containers[0].Env {
		if variable.Name == agentconfig.EnvTemplateHash {
			return variable.Value
		}
	}
	Fail("the agent is not given the hash of its pod template")
	return ""
}

// currentReport returns the report of the agent of the Treebind as syncReport makes it, of the pod template that the
// reconcile of the spec builds for its spec now, with the settings that the poller of the tests is given.
func currentReport(treebind *treebindv1alpha1.Treebind, agreed string, given ...status.Check) *status.Report {
	GinkgoHelper()
	reconciler := &TreebindReconciler{AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}
	statefulSet, err := desiredStatefulSet(treebind, reconciler.agentSettings())
	Expect(err).NotTo(HaveOccurred())
	report := syncReport(treebind.Spec.Sync.Direction, agreed, given...)
	report.Treebind = status.Treebind{Namespace: treebind.Namespace, Name: treebind.Name}
	report.TemplateHash = hashVariableOf(statefulSet.Spec.Template)
	return report
}

func TestThePollerWillNotStartWithoutWhatItNeeds(t *testing.T) {
	keys := newTestPublisher()
	for name, poller := range map[string]*StatusPollerReconciler{
		"no reader of reports":    {StatusKeys: keys},
		"no keys":                 {Agents: &fakeAgents{}},
		"keys of nothing":         {Agents: &fakeAgents{}, StatusKeys: &StatusKeyPublisher{}},
		"no issuer of the token":  {Agents: &fakeAgents{}, StatusKeys: keys, AgentImage: testImage, StatusSubject: testSubject},
		"no subject of the token": {Agents: &fakeAgents{}, StatusKeys: keys, AgentImage: testImage, StatusIssuer: testIssuer},
	} {
		// The manager is nil, so an error of the controller builder would pass for the
		// refusal: the text has to be the operator's own.
		err := poller.SetupWithManager(nil)
		if err == nil || (!strings.Contains(err.Error(), "reader of the sync agents") &&
			!strings.Contains(err.Error(), "public signing keys") &&
			!strings.Contains(err.Error(), "issuer and the subject of the operator's token")) {
			t.Errorf("%s: SetupWithManager returned %v, want the refusal of the operator", name, err)
		}
	}
}

// Without a recorder a change of the sync outcome would be a condition only (NFR-6), so the poller does not start
// without one.
func TestThePollerWillNotStartWithoutARecorderOfEvents(t *testing.T) {
	poller := &StatusPollerReconciler{
		Agents: &fakeAgents{}, StatusKeys: newTestPublisher(), AgentImage: testImage,
		StatusIssuer: testIssuer, StatusSubject: testSubject,
	}
	// The manager is nil, so an error of the controller builder would pass for the refusal: the text has to be the
	// operator's own.
	if err := poller.SetupWithManager(nil); err == nil || !strings.Contains(err.Error(), "recorder of the events") {
		t.Errorf("SetupWithManager returned %v, want the refusal that names the recorder", err)
	}
}

// The poller takes a report for one of the current spec when its hash is the one that the reconcile of the spec gives
// the agent of that spec. A hash computed in another way, or from other settings, would match no report, and Synced
// would say AgentOutdated for as long as the operator runs; one that did not follow the spec would take a report of the
// spec before for one of this spec.
func TestThePollerExpectsTheHashThatTheReconcileOfTheSpecGivesTheAgent(t *testing.T) {
	reconciler := &TreebindReconciler{AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}
	poller := &StatusPollerReconciler{AgentImage: testImage, StatusIssuer: testIssuer, StatusSubject: testSubject}

	full := newTreebind(testTreebindName)
	full.Spec.Target.SubPath = subPathNested
	full.Spec.Repository.Path = repoPathDashboards
	full.Spec.Repository.Auth = &treebindv1alpha1.AuthSpec{SSHKeySecretRef: sshKeyRef(), KnownHostsConfigMapRef: knownHostsRef()}
	full.Spec.Sync.Interval = &metav1.Duration{Duration: time.Minute}
	full.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: []string{pathsJSON}}
	pullOnly := newTreebind(testTreebindName)
	pullOnly.Spec.Sync.Direction = treebindv1alpha1.SyncDirectionPull
	slower := newTreebind(testTreebindName)
	slower.Spec.Sync.Interval = &metav1.Duration{Duration: 5 * time.Minute}

	seen := map[string]string{}
	for name, treebind := range map[string]*treebindv1alpha1.Treebind{
		"the defaults":     newTreebind(testTreebindName),
		"every setting":    full,
		"only pulls":       pullOnly,
		"another interval": slower,
	} {
		statefulSet, err := desiredStatefulSet(treebind, reconciler.agentSettings())
		if err != nil {
			t.Fatal(err)
		}
		given := templateHashVariable(t, statefulSet)

		expected, err := agentTemplateHash(treebind, poller.agentSettings())
		if err != nil {
			t.Fatal(err)
		}
		if expected != given {
			t.Errorf("%s: the poller expects %s, and the reconcile of the spec gives the agent %s", name, expected, given)
		}
		if other, taken := seen[expected]; taken {
			t.Errorf("%s and %s give the same template hash, so a report of one passes for one of the other", name, other)
		}
		seen[expected] = name
	}
}
