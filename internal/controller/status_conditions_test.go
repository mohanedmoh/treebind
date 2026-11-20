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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The reasons the status poller will give Synced and Conflict. The reconciler
// never writes them, so the tests that play the poller spell them out.
const (
	reasonInSync         = "InSync"
	reasonSyncFailed     = "SyncFailed"
	reasonConflictParked = "ConflictParked"
)

// The messages that more than one test expects.
const (
	wantAwaitingMessage = "No report from the sync agent has been received yet"
	messageNoPodReady   = "0 of 1 sync pods ready"
	messagePodReady     = "1 of 1 sync pods ready"
)

// syncedCondition is a Synced condition as the status poller writes it, of the given generation of the Treebind.
func syncedCondition(status metav1.ConditionStatus, reason string, generation int64) *metav1.Condition {
	return &metav1.Condition{
		Type: treebindv1alpha1.ConditionSynced, Status: status, Reason: reason, ObservedGeneration: generation,
	}
}

func TestReadyCondition(t *testing.T) {
	const statefulSetGeneration, treebindGeneration = int64(2), int64(7)

	tests := []struct {
		name                    string
		readyReplicas           int32
		statefulSetObservedGen  int64
		synced                  *metav1.Condition
		wantStatus              metav1.ConditionStatus
		wantReason, wantMessage string
	}{
		{
			name:       "no ready pod and no Synced condition",
			wantStatus: metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
		{
			name:       "no ready pod and Synced Unknown",
			synced:     syncedCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonAwaitingAgentReport, treebindGeneration),
			wantStatus: metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
		{
			name:       "no ready pod and Synced True",
			synced:     syncedCondition(metav1.ConditionTrue, reasonInSync, treebindGeneration),
			wantStatus: metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
		{
			name:       "no ready pod and Synced False keeps the pod as the reason",
			synced:     syncedCondition(metav1.ConditionFalse, reasonSyncFailed, treebindGeneration),
			wantStatus: metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
		{
			name:          "a ready pod and no Synced condition",
			readyReplicas: 1,
			wantStatus:    metav1.ConditionTrue, wantReason: treebindv1alpha1.ReasonSyncPodReady,
			wantMessage: messagePodReady,
		},
		{
			name:          "a ready pod and Synced Unknown",
			readyReplicas: 1,
			synced:        syncedCondition(metav1.ConditionUnknown, treebindv1alpha1.ReasonAwaitingAgentReport, treebindGeneration),
			wantStatus:    metav1.ConditionTrue, wantReason: treebindv1alpha1.ReasonSyncPodReady,
			wantMessage: messagePodReady,
		},
		{
			name:          "a ready pod and Synced True",
			readyReplicas: 1,
			synced:        syncedCondition(metav1.ConditionTrue, reasonInSync, treebindGeneration),
			wantStatus:    metav1.ConditionTrue, wantReason: treebindv1alpha1.ReasonSyncPodReady,
			wantMessage: messagePodReady,
		},
		{
			name:          "a ready pod and Synced False names the reason of Synced",
			readyReplicas: 1,
			synced:        syncedCondition(metav1.ConditionFalse, reasonSyncFailed, treebindGeneration),
			wantStatus:    metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonNotSynced,
			wantMessage: "1 of 1 sync pods ready, but Synced is False: SyncFailed",
		},
		{
			name:          "a ready pod and Synced False for another reason that waits for a person",
			readyReplicas: 1,
			synced:        syncedCondition(metav1.ConditionFalse, treebindv1alpha1.ReasonNotWritable, treebindGeneration),
			wantStatus:    metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonNotSynced,
			wantMessage: "1 of 1 sync pods ready, but Synced is False: NotWritable",
		},
		{
			name:                   "a ready count from a stale StatefulSet generation counts as zero",
			readyReplicas:          1,
			statefulSetObservedGen: statefulSetGeneration - 1,
			synced:                 syncedCondition(metav1.ConditionTrue, reasonInSync, treebindGeneration),
			wantStatus:             metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
		{
			name:                   "a stale StatefulSet generation wins over Synced False",
			readyReplicas:          1,
			statefulSetObservedGen: statefulSetGeneration - 1,
			synced:                 syncedCondition(metav1.ConditionFalse, reasonSyncFailed, treebindGeneration),
			wantStatus:             metav1.ConditionFalse, wantReason: treebindv1alpha1.ReasonSyncPodNotReady,
			wantMessage: messageNoPodReady,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observed := tt.statefulSetObservedGen
			if observed == 0 {
				observed = statefulSetGeneration
			}
			statefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: statefulSetGeneration},
				Status:     appsv1.StatefulSetStatus{ObservedGeneration: observed, ReadyReplicas: tt.readyReplicas},
			}

			got := readyCondition(statefulSet, tt.synced, bidirectional, treebindGeneration)

			if got.Type != treebindv1alpha1.ConditionReady {
				t.Errorf("type = %q, want %q", got.Type, treebindv1alpha1.ConditionReady)
			}
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Errorf("status/reason = %s/%s, want %s/%s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tt.wantMessage)
			}
			if got.ObservedGeneration != treebindGeneration {
				t.Errorf("observedGeneration = %d, want %d", got.ObservedGeneration, treebindGeneration)
			}
		})
	}
}

// Ready follows Synced only for the failures of the spec that Synced reports, that wait for a person. The table
// crosses what Synced says with the direction of the Treebind, whether the report is of the current generation or of
// one before, and whether the pod is ready.
func TestReadyConditionFollowsSyncedOfTheCurrentSpec(t *testing.T) {
	const treebindGeneration = int64(7)

	// Each failure of a step that this release defines waits for a person, except these, which wait for the
	// repository and which Ready does not act on: always, or for a Treebind that syncs in both directions.
	// The reason SyncFailed, which an agent of another release gets, waits for a person too.
	reasonsThatWaitForTheRepository := map[treebindv1alpha1.SyncDirection][]string{
		pullOnly:      {reasonFetchFailed, reasonPushFailed},
		bidirectional: {reasonFetchFailed, reasonPushFailed, reasonNonFastForward, reasonBlocked},
	}
	failedReasons := append([]string{reasonSyncFailed}, slices.Sorted(maps.Keys(failedStepReasons))...)

	type synced struct {
		status metav1.ConditionStatus
		reason string
		// makesNotReady says whether Synced of the current generation makes a ready pod not ready, for each
		// direction.
		makesNotReady func(direction treebindv1alpha1.SyncDirection) bool
	}
	never := func(treebindv1alpha1.SyncDirection) bool { return false }
	unknownReasons := []string{
		treebindv1alpha1.ReasonAwaitingAgentReport, treebindv1alpha1.ReasonAgentUnreachable,
		treebindv1alpha1.ReasonSyncPending, treebindv1alpha1.ReasonAgentOutdated,
	}
	cases := make([]synced, 0, len(unknownReasons)+1+len(failedReasons))
	for _, reason := range unknownReasons {
		cases = append(cases, synced{status: metav1.ConditionUnknown, reason: reason, makesNotReady: never})
	}
	cases = append(cases, synced{status: metav1.ConditionTrue, reason: reasonInSync, makesNotReady: never})
	for _, reason := range failedReasons {
		cases = append(cases, synced{
			status: metav1.ConditionFalse, reason: reason,
			makesNotReady: func(direction treebindv1alpha1.SyncDirection) bool {
				return !slices.Contains(reasonsThatWaitForTheRepository[direction], reason)
			},
		})
	}

	generations := []struct {
		name     string
		observed int64
	}{
		{"of the current generation", treebindGeneration},
		{"of the generation before", treebindGeneration - 1},
		{"of no generation", 0},
	}
	podStates := []struct {
		name          string
		readyReplicas int32
	}{{"ready", 1}, {"not ready", 0}}

	for _, direction := range []treebindv1alpha1.SyncDirection{pullOnly, bidirectional} {
		for _, c := range cases {
			for _, generation := range generations {
				for _, pod := range podStates {
					name := fmt.Sprintf("%s, Synced %s %s %s, pod %s", direction, c.status, c.reason, generation.name, pod.name)
					t.Run(name, func(t *testing.T) {
						statefulSet := &appsv1.StatefulSet{
							ObjectMeta: metav1.ObjectMeta{Generation: 2},
							Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: pod.readyReplicas},
						}
						synced := syncedCondition(c.status, c.reason, generation.observed)

						got := readyCondition(statefulSet, synced, direction, treebindGeneration)

						switch {
						case pod.readyReplicas == 0:
							assertReady(t, got, metav1.ConditionFalse, treebindv1alpha1.ReasonSyncPodNotReady, messageNoPodReady)
						case generation.observed == treebindGeneration && c.makesNotReady(direction):
							assertReady(t, got, metav1.ConditionFalse, treebindv1alpha1.ReasonNotSynced,
								"1 of 1 sync pods ready, but Synced is False: "+c.reason)
						default:
							assertReady(t, got, metav1.ConditionTrue, treebindv1alpha1.ReasonSyncPodReady, messagePodReady)
						}
						if got.ObservedGeneration != treebindGeneration {
							t.Errorf("observedGeneration = %d, want %d", got.ObservedGeneration, treebindGeneration)
						}
					})
				}
			}
		}
	}
}

func assertReady(t *testing.T, got metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	if got.Status != status || got.Reason != reason || got.Message != message {
		t.Errorf("Ready = %s/%s %q, want %s/%s %q", got.Status, got.Reason, got.Message, status, reason, message)
	}
}

func TestAddAwaitingReportConditions(t *testing.T) {
	existing := func(conditionType string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: conditionType, Status: status, Reason: reason, ObservedGeneration: 1}
	}
	awaiting := func(conditionType string) metav1.Condition {
		return metav1.Condition{
			Type:               conditionType,
			Status:             metav1.ConditionUnknown,
			Reason:             treebindv1alpha1.ReasonAwaitingAgentReport,
			Message:            wantAwaitingMessage,
			ObservedGeneration: 5,
		}
	}
	synced, conflict := treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict
	unreachable := treebindv1alpha1.ConditionAgentUnreachable
	syncedTrue := existing(synced, metav1.ConditionTrue, reasonInSync)
	conflictTrue := existing(conflict, metav1.ConditionTrue, reasonConflictParked)
	syncedFalse := existing(synced, metav1.ConditionFalse, reasonSyncFailed)
	unreachableTrue := existing(unreachable, metav1.ConditionTrue, treebindv1alpha1.ReasonConnectionFailed)

	tests := []struct {
		name       string
		conditions []metav1.Condition
		wantAdded  bool
		wantByType map[string]metav1.Condition
	}{
		{
			name:      "all three are added as Unknown when absent",
			wantAdded: true,
			wantByType: map[string]metav1.Condition{
				synced: awaiting(synced), conflict: awaiting(conflict), unreachable: awaiting(unreachable),
			},
		},
		{
			name:       "only the missing ones are added",
			conditions: []metav1.Condition{syncedTrue},
			wantAdded:  true,
			wantByType: map[string]metav1.Condition{
				synced: syncedTrue, conflict: awaiting(conflict), unreachable: awaiting(unreachable),
			},
		},
		{
			name:       "AgentUnreachable is added to a Treebind that was made before the operator knew it",
			conditions: []metav1.Condition{syncedTrue, conflictTrue},
			wantAdded:  true,
			wantByType: map[string]metav1.Condition{synced: syncedTrue, conflict: conflictTrue, unreachable: awaiting(unreachable)},
		},
		{
			name:       "existing conditions are never overwritten, whatever their value",
			conditions: []metav1.Condition{syncedFalse, conflictTrue, unreachableTrue},
			wantByType: map[string]metav1.Condition{synced: syncedFalse, conflict: conflictTrue, unreachable: unreachableTrue},
		},
		{
			name: "an Unknown that is already there is left as it is",
			conditions: []metav1.Condition{
				existing(synced, metav1.ConditionUnknown, "Other"), conflictTrue,
				existing(unreachable, metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing),
			},
			wantByType: map[string]metav1.Condition{
				synced:      existing(synced, metav1.ConditionUnknown, "Other"),
				conflict:    conflictTrue,
				unreachable: existing(unreachable, metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Generation = 5
			treebind.Status.Conditions = append([]metav1.Condition(nil), tt.conditions...)

			if got := addAwaitingReportConditions(treebind); got != tt.wantAdded {
				t.Errorf("added = %v, want %v", got, tt.wantAdded)
			}

			if len(treebind.Status.Conditions) != len(tt.wantByType) {
				t.Errorf("conditions = %+v, want only %d", treebind.Status.Conditions, len(tt.wantByType))
			}
			for conditionType, want := range tt.wantByType {
				got := meta.FindStatusCondition(treebind.Status.Conditions, conditionType)
				if got == nil {
					t.Errorf("%s is missing", conditionType)
					continue
				}
				want.LastTransitionTime = got.LastTransitionTime
				if !reflect.DeepEqual(*got, want) {
					t.Errorf("%s = %+v, want %+v", conditionType, *got, want)
				}
			}
		})
	}
}

const columnTypeString = "string"

// printerColumn is the part of an additionalPrinterColumns entry that decides
// what kubectl get prints.
type printerColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	JSONPath string `json:"jsonPath"`
}

func TestGeneratedCRDPrinterColumns(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "treebind.example.org_treebinds.yaml"))
	if err != nil {
		t.Fatalf("read the generated CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name                     string          `json:"name"`
				AdditionalPrinterColumns []printerColumn `json:"additionalPrinterColumns"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse the generated CRD: %v", err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(crd.Spec.Versions))
	}

	// Explicit printer columns replace the default Age column, so it is listed.
	want := []printerColumn{
		{Name: "Ready", Type: columnTypeString, JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
		{Name: "Synced", Type: columnTypeString, JSONPath: `.status.conditions[?(@.type=="Synced")].status`},
		{Name: "Conflict", Type: columnTypeString, JSONPath: `.status.conditions[?(@.type=="Conflict")].status`},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}
	if got := crd.Spec.Versions[0].AdditionalPrinterColumns; !reflect.DeepEqual(got, want) {
		t.Errorf("printer columns = %+v, want %+v", got, want)
	}
}

func TestReconcileFailedCondition(t *testing.T) {
	const generation = int64(4)
	const prefix = "The last reconcile failed: "

	t.Run("carries the error and the generation", func(t *testing.T) {
		condition := reconcileFailedCondition(errors.New("ensure StatefulSet a/b: invalid"), generation)

		if condition.Type != treebindv1alpha1.ConditionReady || condition.Status != metav1.ConditionFalse ||
			condition.Reason != treebindv1alpha1.ReasonReconcileFailed || condition.ObservedGeneration != generation {
			t.Errorf("unexpected condition: %+v", condition)
		}
		if want := prefix + "ensure StatefulSet a/b: invalid"; condition.Message != want {
			t.Errorf("message = %q, want %q", condition.Message, want)
		}
	})

	t.Run("keeps an error at the limit whole", func(t *testing.T) {
		text := strings.Repeat("x", maxFailureText)
		if got := reconcileFailedCondition(errors.New(text), generation).Message; got != prefix+text {
			t.Errorf("a message of %d bytes was changed", maxFailureText)
		}
	})

	t.Run("cuts a longer error and says so", func(t *testing.T) {
		message := reconcileFailedCondition(errors.New(strings.Repeat("x", maxFailureText+500)), generation).Message
		if want := prefix + strings.Repeat("x", maxFailureText) + "..."; message != want {
			t.Errorf("a long error was cut to %d bytes, want %d", len(message), len(want))
		}
	})

	t.Run("does not split a character at the cut", func(t *testing.T) {
		// Three-byte characters, so the limit of 1024 bytes falls inside the 342nd.
		text := strings.Repeat("\u20ac", maxFailureText)
		message := reconcileFailedCondition(errors.New(text), generation).Message
		if !utf8.ValidString(message) {
			t.Errorf("the message is not valid UTF-8: %q", message[len(message)-8:])
		}
		if !strings.HasSuffix(message, "...") {
			t.Error("the cut message does not end with an ellipsis")
		}
	})
}
