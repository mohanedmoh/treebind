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
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The agent pulls every spec.sync.interval, in the direction of spec.sync.direction, and takes the
// writer lock under the name of its pod (docs/adr/0021). That takes three things that no test of one
// side alone can show: the operator puts them in the pod, the agent accepts what the operator puts there,
// and the API server and the agent agree on which intervals and directions exist.

func TestTheAgentIsToldHowToSyncAndWhoThePodIs(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Sync.Interval = &metav1.Duration{Duration: 90 * time.Second}
	treebind.Spec.Sync.Direction = treebindv1alpha1.SyncDirectionPull

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	if config.SyncInterval != 90*time.Second || config.SyncDirection != agentconfig.DirectionPull || config.PodName != testPodName {
		t.Errorf("the agent reads %v, %q and %q, want the spec's interval and direction and the name of the pod", config.SyncInterval, config.SyncDirection, config.PodName)
	}
	if env[agentconfig.EnvSyncInterval] != ninetySeconds {
		t.Errorf("%s = %q, want the interval as Go writes a duration", agentconfig.EnvSyncInterval, env[agentconfig.EnvSyncInterval])
	}
}

func TestTheAgentIsToldTheCapOnDeletionsOnlyWhenTheSpecSaysOne(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	config, env, err := agentConfigOf(t, treebind)
	if err != nil {
		t.Fatal(err)
	}
	if value, set := env[agentconfig.EnvSyncMaxDeletions]; set || config.MaxDeletions != 0 {
		t.Errorf("%s is %q and the agent reads %d, want neither: a pod template that never had the variable is not changed, "+
			"and the pull has a cap of its own", agentconfig.EnvSyncMaxDeletions, value, config.MaxDeletions)
	}

	limit := int32(250)
	treebind.Spec.Sync.MaxDeletions = &limit
	config, env, err = agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	// The name is written out, because it is what ADR 0027 and the page about the guard tell a person.
	if env["TREEBIND_SYNC_MAX_DELETIONS"] != "250" || config.MaxDeletions != 250 {
		t.Errorf("TREEBIND_SYNC_MAX_DELETIONS = %q and the agent reads %d, want 250 on both sides",
			env["TREEBIND_SYNC_MAX_DELETIONS"], config.MaxDeletions)
	}
}

func TestTheAgentIsToldTheConflictPolicyOnlyWhenItIsFollowLocal(t *testing.T) {
	// followRemote is the default of the CRD and of the agent, so it is not passed: a pod template that never had the
	// variable is not changed, and its pod is not replaced (docs/adr/0034).
	for _, policy := range []treebindv1alpha1.ConflictPolicy{"", treebindv1alpha1.ConflictPolicyFollowRemote} {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Sync.ConflictPolicy = policy

		config, env, err := agentConfigOf(t, treebind)

		if err != nil {
			t.Fatal(err)
		}
		if value, set := env[agentconfig.EnvSyncConflictPolicy]; set || config.ConflictPolicy != agentconfig.ConflictFollowRemote {
			t.Errorf("for %q the pod has %s = %q and the agent reads %q, want the variable left out and followRemote",
				policy, agentconfig.EnvSyncConflictPolicy, value, config.ConflictPolicy)
		}
	}

	treebind := newTreebind(testTreebindName)
	treebind.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowLocal
	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	// The name is written out, because it is what ADR 0034 tells a person.
	if env["TREEBIND_SYNC_CONFLICT_POLICY"] != "followLocal" || config.ConflictPolicy != agentconfig.ConflictFollowLocal {
		t.Errorf("TREEBIND_SYNC_CONFLICT_POLICY = %q and the agent reads %q, want followLocal on both sides",
			env["TREEBIND_SYNC_CONFLICT_POLICY"], config.ConflictPolicy)
	}
}

func TestTheNamesAndTheDefaultOfTheConflictPoliciesAreTheOnesOfTheAPI(t *testing.T) {
	if string(treebindv1alpha1.ConflictPolicyFollowRemote) != agentconfig.ConflictFollowRemote ||
		string(treebindv1alpha1.ConflictPolicyFollowLocal) != agentconfig.ConflictFollowLocal {
		t.Errorf("the API names %q and %q, the agent %q and %q", treebindv1alpha1.ConflictPolicyFollowRemote,
			treebindv1alpha1.ConflictPolicyFollowLocal, agentconfig.ConflictFollowRemote, agentconfig.ConflictFollowLocal)
	}
	// The default of the CRD is followRemote, a marker that a test with a real API server holds (treebind_schema_test.go),
	// and the agent has no other default.
	if agentconfig.DefaultConflictPolicy != string(treebindv1alpha1.ConflictPolicyFollowRemote) {
		t.Errorf("the agent defaults to %q, want %q, the default of the CRD", agentconfig.DefaultConflictPolicy,
			treebindv1alpha1.ConflictPolicyFollowRemote)
	}
}

func TestTheAgentAcceptsEveryCapOnDeletionsTheOperatorCanPutInThePod(t *testing.T) {
	for _, limit := range []int32{agentconfig.MinMaxDeletions, 2, 100, 101, 1000000, agentconfig.MaxMaxDeletions} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Sync.MaxDeletions = &limit

			config, _, err := agentConfigOf(t, treebind)

			if err != nil || config.MaxDeletions != int(limit) {
				t.Errorf("the agent reads %d, %v, want %d and no refusal", config.MaxDeletions, err, limit)
			}
		})
	}
}

func TestThePodNameComesFromThePodAndIsNotWrittenIn(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace
	container, _ := agentContainer(t, treebind)

	for _, variable := range container.Env {
		if variable.Name != agentconfig.EnvPodName {
			continue
		}
		if variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.FieldRef == nil ||
			variable.ValueFrom.FieldRef.FieldPath != fieldPodName {
			t.Errorf("%s = %+v, want it read from metadata.name: the pod of a StatefulSet is named by it, and a name that is written in is the same for every pod", variable.Name, variable)
		}
		return
	}
	t.Errorf("%s is not set", agentconfig.EnvPodName)
}

func TestNoIntervalVariableWithoutAnInterval(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Sync.Interval = nil

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatal(err)
	}
	if value, set := env[agentconfig.EnvSyncInterval]; set {
		t.Errorf("%s is %q: the CRD fills in the interval, and the agent has the same default for one that was not", agentconfig.EnvSyncInterval, value)
	}
	if config.SyncInterval != agentconfig.DefaultSyncInterval {
		t.Errorf("the agent syncs every %v, want the default %v", config.SyncInterval, agentconfig.DefaultSyncInterval)
	}
}

func TestTheAgentAcceptsEveryDirectionAndIntervalTheOperatorCanPutInThePod(t *testing.T) {
	for _, direction := range []treebindv1alpha1.SyncDirection{treebindv1alpha1.SyncDirectionPull, treebindv1alpha1.SyncDirectionBidirectional} {
		for _, interval := range []time.Duration{time.Second, 30 * time.Second, 90 * time.Second, 5 * time.Minute, 36 * time.Hour} {
			t.Run(fmt.Sprintf("%s every %v", direction, interval), func(t *testing.T) {
				treebind := newTreebind(testTreebindName)
				treebind.Spec.Sync.Direction = direction
				treebind.Spec.Sync.Interval = &metav1.Duration{Duration: interval}

				config, _, err := agentConfigOf(t, treebind)

				if err != nil {
					t.Fatalf("the agent refuses the environment of its own pod: %v", err)
				}
				if config.SyncInterval != interval || config.SyncDirection != string(direction) {
					t.Errorf("the agent reads %v and %q, want %v and %q", config.SyncInterval, config.SyncDirection, interval, direction)
				}
			})
		}
	}
}

// intervalCandidates are intervals on both sides of every rule of the API server (CEL and the length
// limit) and of the agent (CheckSyncInterval), which have to agree on each of them.
func intervalCandidates() []time.Duration {
	return []time.Duration{
		0, 1, time.Millisecond, 999 * time.Millisecond, time.Second - 1, time.Second, time.Second + 1, 1500 * time.Millisecond,
		30 * time.Second, time.Minute, 90 * time.Second, time.Hour, 36 * time.Hour, 24 * 365 * time.Hour,
		math.MaxInt64, -1, -time.Second, -time.Hour, math.MinInt64 + 1,
	}
}

// ninetySeconds is 90 seconds as Go writes a duration.
const ninetySeconds = "1m30s"

// durationTexts are strings that a client can send as a duration, on both sides of every rule of the API
// server and of the agent.
var durationTexts = []string{
	"30s", "1s", "999ms", ninetySeconds, "1h", "1.5s", ".5s", "1.s", "1.0s", "+1s", "-1s", "0", "0s", "30", "1d", "1w", "30S", "1 s", " 1s",
	"1s ", "1\u00b5s", "1us", "1ns", "100000h", "1e3s", "1m1", "m1s", "1h2m3s4ms", "1s1s", "01s", "1_000s", "NaN", "infinite",
	"9223372036s", "9223372037s", "2562047h47m16.854775807s", "2562047h47m16.854775808s",
}

var _ = Describe("sync.interval", func() {
	var counter int

	created := func(ctx context.Context, interval *metav1.Duration) (*treebindv1alpha1.Treebind, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("interval-%d", counter))
		treebind.Spec.Sync.Interval = interval
		err := k8sClient.Create(ctx, treebind)
		return treebind, err
	}

	It("is filled in by the API server with the default that the agent has", func() {
		ctx := context.Background()
		treebind, err := created(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, treebind)).To(Succeed()) })

		var got treebindv1alpha1.Treebind
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name}, &got)).To(Succeed())

		Expect(got.Spec.Sync.Interval).NotTo(BeNil())
		Expect(got.Spec.Sync.Interval.Duration).To(Equal(agentconfig.DefaultSyncInterval))
	})

	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, interval := range intervalCandidates() {
			treebind, err := created(ctx, &metav1.Duration{Duration: interval})
			_, agentErr := agentconfig.CheckSyncInterval(interval.String())
			agentOK := agentErr == nil
			if (err == nil) != agentOK {
				Fail(fmt.Sprintf("interval %v (%q): the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					interval, interval.String(), err == nil, err, agentOK, agentErr))
			}
			if err == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		// The list has to hold both, or the check above says nothing.
		Expect(accepted).To(BeNumerically(">", 5))
		Expect(refused).To(BeNumerically(">", 5))
	})

	// The Go values above are all written the way Go writes a duration. A client can send any string,
	// which the API server reads with its own parser of a duration, and the agent reads with Go's.
	It("is accepted by the API server exactly when the agent accepts it, whatever string it is written as", func() {
		ctx := context.Background()
		var accepted, refused int
		for i, text := range durationTexts {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("text-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, text, "spec", "sync", "interval")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			createErr := k8sClient.Create(ctx, treebind)

			_, agentErr := agentconfig.CheckSyncInterval(text)
			if (createErr == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("interval %q: the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					text, createErr == nil, createErr, agentErr == nil, agentErr))
			}
			if createErr == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		Expect(accepted).To(BeNumerically(">", 5))
		Expect(refused).To(BeNumerically(">", 5))
	})

	It("has the least that the agent has", func() {
		Expect(agentconfig.MinSyncInterval).To(Equal(time.Second))
		ctx := context.Background()
		treebind, err := created(ctx, &metav1.Duration{Duration: agentconfig.MinSyncInterval})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		_, err = created(ctx, &metav1.Duration{Duration: agentconfig.MinSyncInterval - 1})
		Expect(err).To(MatchError(ContainSubstring("interval must be at least 1s")))
	})
})

var _ = Describe("sync.maxDeletions", func() {
	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for i, limit := range []int64{
			math.MinInt64, -1 << 40, -2147483649, -2147483648, -1, 0, 1, 2, 99, 100, 101, 1 << 20, math.MaxInt32 - 1, math.MaxInt32,
			math.MaxInt32 + 1, 1 << 32, 1 << 40, math.MaxInt64,
		} {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("max-deletions-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, limit, "spec", "sync", "maxDeletions")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			createErr := k8sClient.Create(ctx, treebind)

			// The operator writes the number as Go does, which is what a value that the API server took reaches the agent as.
			_, agentErr := agentconfig.CheckMaxDeletions(strconv.FormatInt(limit, 10))
			if (createErr == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("maxDeletions %d: the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					limit, createErr == nil, createErr, agentErr == nil, agentErr))
			}
			if createErr == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		// The list has to hold both, or the check above says nothing.
		Expect(accepted).To(BeNumerically(">", 5))
		Expect(refused).To(BeNumerically(">", 5))
	})

	// What is not a whole number is not for the agent to read, so the API server has to refuse it.
	It("is refused by the API server when it is not a whole number", func() {
		ctx := context.Background()
		for i, value := range []any{"100", 1.5, true, []any{int64(1)}, map[string]any{"n": int64(1)}} {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("max-deletions-type-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, value, "spec", "sync", "maxDeletions")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			Expect(k8sClient.Create(ctx, treebind)).NotTo(Succeed(), "%#v", value)
		}
	})

	It("is not filled in by the API server, so that the pod of a Treebind that does not say is not changed", func() {
		ctx := context.Background()
		treebind := newTreebind("max-deletions-unset")
		Expect(k8sClient.Create(ctx, treebind)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, treebind)).To(Succeed()) })

		var got treebindv1alpha1.Treebind
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name}, &got)).To(Succeed())

		Expect(got.Spec.Sync.MaxDeletions).To(BeNil())
	})
})

var _ = Describe("sync.direction", func() {
	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for i, direction := range []string{
			"pull", "bidirectional", "", "push", "Pull", "PULL", " pull", "pull ", "both", "bidirectional ", "pull,push", "none", "read", "\x00",
		} {
			treebind := newTreebind(fmt.Sprintf("direction-%d", i))
			treebind.Spec.Sync.Direction = treebindv1alpha1.SyncDirection(direction)

			err := k8sClient.Create(ctx, treebind)

			agentOK := agentconfig.CheckSyncDirection(direction) == nil
			if (err == nil) != agentOK {
				Fail(fmt.Sprintf("direction %q: the API server accepts it = %v (%v), the agent accepts it = %v: "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					direction, err == nil, err, agentOK))
			}
			if err == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		Expect(accepted).To(Equal(2))
		Expect(refused).To(BeNumerically(">", 5))
	})
})

// The agent commits a file once it has been quiet for spec.sync.settleTime (#34). As for the
// interval, the operator puts it in the pod, the agent accepts what the operator puts there, and the API
// server and the agent agree on which settle times exist.

func TestTheAgentIsToldTheSettleTimeOnlyWhenTheSpecSaysOne(t *testing.T) {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Sync.SettleTime = nil
	config, env, err := agentConfigOf(t, treebind)
	if err != nil {
		t.Fatal(err)
	}
	if value, set := env[agentconfig.EnvSyncSettleTime]; set {
		t.Errorf("%s is %q: the CRD fills in the settle time, and the agent has the same default for one that was not", agentconfig.EnvSyncSettleTime, value)
	}
	if config.SettleTime != agentconfig.DefaultSettleTime {
		t.Errorf("the agent settles after %v, want the default %v", config.SettleTime, agentconfig.DefaultSettleTime)
	}

	treebind.Spec.Sync.SettleTime = &metav1.Duration{Duration: 90 * time.Second}
	config, env, err = agentConfigOf(t, treebind)
	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	if config.SettleTime != 90*time.Second || env[agentconfig.EnvSyncSettleTime] != ninetySeconds {
		t.Errorf("the agent reads %v from %q, want the settle time of the spec as Go writes a duration", config.SettleTime, env[agentconfig.EnvSyncSettleTime])
	}
}

func TestTheAgentAcceptsEverySettleTimeTheOperatorCanPutInThePod(t *testing.T) {
	for _, settle := range []time.Duration{time.Second, 5 * time.Second, 90 * time.Second, 5 * time.Minute, 36 * time.Hour} {
		t.Run(settle.String(), func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Sync.SettleTime = &metav1.Duration{Duration: settle}

			config, _, err := agentConfigOf(t, treebind)

			if err != nil {
				t.Fatalf("the agent refuses the environment of its own pod: %v", err)
			}
			if config.SettleTime != settle {
				t.Errorf("the agent reads %v, want %v", config.SettleTime, settle)
			}
		})
	}
}

var _ = Describe("sync.settleTime", func() {
	var counter int

	created := func(ctx context.Context, settle *metav1.Duration) (*treebindv1alpha1.Treebind, error) {
		GinkgoHelper()
		counter++
		treebind := newTreebind(fmt.Sprintf("settle-%d", counter))
		treebind.Spec.Sync.SettleTime = settle
		err := k8sClient.Create(ctx, treebind)
		return treebind, err
	}

	It("is filled in by the API server with the default that the agent has", func() {
		ctx := context.Background()
		treebind, err := created(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, treebind)).To(Succeed()) })

		var got treebindv1alpha1.Treebind
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name}, &got)).To(Succeed())

		Expect(got.Spec.Sync.SettleTime).NotTo(BeNil())
		Expect(got.Spec.Sync.SettleTime.Duration).To(Equal(agentconfig.DefaultSettleTime))
	})

	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for _, settle := range intervalCandidates() {
			treebind, err := created(ctx, &metav1.Duration{Duration: settle})
			_, agentErr := agentconfig.CheckSettleTime(settle.String())
			if (err == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("settle time %v (%q): the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					settle, settle.String(), err == nil, err, agentErr == nil, agentErr))
			}
			if err == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		Expect(accepted).To(BeNumerically(">", 5))
		Expect(refused).To(BeNumerically(">", 5))
	})

	It("is accepted by the API server exactly when the agent accepts it, whatever string it is written as", func() {
		ctx := context.Background()
		var accepted, refused int
		for i, text := range durationTexts {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("settle-text-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, text, "spec", "sync", "settleTime")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			createErr := k8sClient.Create(ctx, treebind)

			_, agentErr := agentconfig.CheckSettleTime(text)
			if (createErr == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("settle time %q: the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					text, createErr == nil, createErr, agentErr == nil, agentErr))
			}
			if createErr == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		Expect(accepted).To(BeNumerically(">", 5))
		Expect(refused).To(BeNumerically(">", 5))
	})

	It("has the least that the agent has", func() {
		Expect(agentconfig.MinSettleTime).To(Equal(time.Second))
		ctx := context.Background()
		treebind, err := created(ctx, &metav1.Duration{Duration: agentconfig.MinSettleTime})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
		_, err = created(ctx, &metav1.Duration{Duration: agentconfig.MinSettleTime - 1})
		Expect(err).To(MatchError(ContainSubstring("settleTime must be at least 1s")))
	})
})
