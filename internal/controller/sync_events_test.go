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
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/app"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	eventNormal  = corev1.EventTypeNormal
	eventWarning = corev1.EventTypeWarning

	// inSyncEvent is the event of Synced coming to InSync.
	inSyncEvent = "Normal Synced InSync"

	// maxEventNote is the most bytes that the events.k8s.io API takes in the note of an event: one that is longer is
	// refused, and the event is lost.
	maxEventNote = 1024
)

// awaiting is what the reconcile of the spec gives a new Treebind: Synced, Conflict and AgentUnreachable Unknown with
// AwaitingAgentReport.
func awaiting() []metav1.Condition {
	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	addAwaitingReportConditions(treebind)
	return treebind.Status.Conditions
}

// poll is the conditions after a poll of a Treebind of the direction with the conditions before it, which got the
// report, nil for none, and made AgentUnreachable say unreachable.
func pollFrom(
	before []metav1.Condition, direction treebindv1alpha1.SyncDirection, report *status.Report,
	unreachable metav1.ConditionStatus,
) []metav1.Condition {
	treebind := syncTreebind(direction, treebindv1alpha1.TreebindStatus{Conditions: slices.Clone(before)})
	syncStatusOf(treebind, report, unreachable, 0, hashOfSpec).apply(&treebind.Status)
	return treebind.Status.Conditions
}

// answered is the conditions after a poll that read the report.
func answered(before []metav1.Condition, report *status.Report) []metav1.Condition {
	return pollFrom(before, bidirectional, report, no)
}

// The reports of the transitions below.
var (
	pendingReport  = syncReport(bidirectional, "")
	inSyncReport   = syncReport(bidirectional, commitOne)
	rejectedReport = syncReport(bidirectional, commitOne, failedCheck(app.CheckPush, reasonPushRejected))
	conflictReport = syncReport(bidirectional, commitOne,
		failedCheck(app.CheckPull, reasonTooManyDeletions), failedCheck(app.CheckConflict, reasonTooManyDeletions))
	resolvedReport = syncReport(bidirectional, commitTwo,
		checkOf(app.CheckConflict, preflight.Passed, treebindv1alpha1.ReasonConflictResolved))
)

// summary is an event as the tests compare it: its type, the condition, and the reason.
func summary(events []syncEvent) []string {
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.eventType+" "+event.condition+" "+event.reason)
	}
	return got
}

func TestAChangeOfTheOutcomeMakesOneEventForEachConditionThatChanged(t *testing.T) {
	inSync := answered(answered(awaiting(), pendingReport), inSyncReport)
	unreachable := pollFrom(inSync, bidirectional, nil, yes)
	conflict := answered(inSync, conflictReport)

	tests := []struct {
		name          string
		before, after []metav1.Condition
		want          []string
	}{
		{"the first report, of an agent that has not synced yet", awaiting(), answered(awaiting(), pendingReport),
			[]string{"Normal Synced SyncPending"}},
		{"the first sync after a start: Conflict comes to NoConflict from Unknown, which says nothing new",
			answered(awaiting(), pendingReport), inSync, []string{inSyncEvent}},
		{"a push that the host refused", inSync, answered(inSync, rejectedReport), []string{"Warning Synced PushRejected"}},
		{"a conflict that could not be resolved: the pull failed, and a conflict waits", inSync, conflict,
			[]string{"Warning Synced TooManyDeletions", "Warning Conflict TooManyDeletions"}},
		{"the conflict, resolved", conflict, answered(conflict, resolvedReport),
			[]string{inSyncEvent, "Normal Conflict ConflictResolved"}},
		{"the conflict, gone without a resolution", conflict, answered(conflict, inSyncReport),
			[]string{inSyncEvent, "Normal Conflict NoConflict"}},
		{"a conflict resolved while Synced stays InSync", inSync, answered(inSync, resolvedReport),
			[]string{"Normal Conflict ConflictResolved"}},
		{"the operator stops reading the agent: Conflict says what Synced says", inSync, unreachable,
			[]string{"Warning Synced AgentUnreachable"}},
		{"and reads it again", unreachable, answered(unreachable, inSyncReport), []string{inSyncEvent}},
		{"and finds a conflict that waits", unreachable, answered(unreachable, conflictReport),
			[]string{"Warning Synced TooManyDeletions", "Warning Conflict TooManyDeletions"}},
		{"a restart of the agent", inSync, answered(inSync, pendingReport), []string{"Normal Synced SyncPending"}},
		{"a report of the spec before", inSync, pollFrom(inSync, bidirectional, &status.Report{
			Version: status.Version, TemplateHash: hashOfBefore,
		}, no), []string{"Normal Synced AgentOutdated"}},
		{"a reason of another release", inSync,
			answered(inSync, syncReport(bidirectional, commitOne, failedCheck(app.CheckPush, reasonNotKnown))),
			[]string{"Warning Synced SyncFailed"}},
		{"a failure that changes its reason", answered(inSync, rejectedReport),
			answered(inSync, syncReport(bidirectional, commitOne, failedCheck(app.CheckPull, reasonFetchFailed))),
			[]string{"Warning Synced FetchFailed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summary(syncEventsOf(tt.before, tt.after)); !slices.Equal(got, tt.want) {
				t.Errorf("events = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAPollThatChangesNoStatusAndNoReasonMakesNoEvent(t *testing.T) {
	inSync := answered(answered(awaiting(), pendingReport), inSyncReport)
	rejected := answered(inSync, rejectedReport)
	conflict := answered(inSync, conflictReport)
	unreachable := pollFrom(inSync, bidirectional, nil, yes)
	newerGeneration := slices.Clone(inSync)
	for i := range newerGeneration {
		newerGeneration[i].ObservedGeneration++
	}

	tests := []struct {
		name          string
		before, after []metav1.Condition
	}{
		{"InSync again", inSync, answered(inSync, inSyncReport)},
		{"InSync at a newer commit, which changes the message", inSync,
			answered(inSync, syncReport(bidirectional, commitTwo))},
		{"the same refusal", rejected, answered(rejected, rejectedReport)},
		{"the same refusal at a newer commit", rejected,
			answered(rejected, syncReport(bidirectional, commitTwo, failedCheck(app.CheckPush, reasonPushRejected)))},
		{"the same conflict", conflict, answered(conflict, conflictReport)},
		{"still unreachable", unreachable, pollFrom(unreachable, bidirectional, nil, yes)},
		{"a poll that failed for less than the minutes it takes to say so", inSync,
			pollFrom(inSync, bidirectional, nil, unknown)},
		{"a poll that got no report", inSync, pollFrom(inSync, bidirectional, nil, no)},
		{"another generation alone", inSync, newerGeneration},
		{"the conditions before the first report", awaiting(), awaiting()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := syncEventsOf(tt.before, tt.after); len(got) != 0 {
				t.Errorf("events = %q, want none", summary(got))
			}
		})
	}
}

func TestAConflictThatComesToNoConflictMakesAnEventOnlyFromAStateThatSaidSomething(t *testing.T) {
	condition := func(value metav1.ConditionStatus, reason string) []metav1.Condition {
		return []metav1.Condition{{Type: treebindv1alpha1.ConditionConflict, Status: value, Reason: reason}}
	}
	noConflict := condition(no, reasonNoConflict)
	for _, tt := range []struct {
		name   string
		before []metav1.Condition
		want   bool
	}{
		{"from nothing", nil, false},
		{"from Unknown", condition(unknown, reasonPending), false},
		{"from True", condition(yes, reasonBlocked), true},
		{"from ConflictResolved, as after a restart that no poll saw", condition(no, reasonResolved), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(syncEventsOf(tt.before, noConflict)) == 1; got != tt.want {
				t.Errorf("an event: %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTheTypeOfAnEventIsWarningForWhatAPersonShouldLookAt(t *testing.T) {
	for _, tt := range []struct {
		condition metav1.Condition
		want      string
	}{
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: yes, Reason: reasonInSync}, eventNormal},
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: no, Reason: reasonFetchFailed}, eventWarning},
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: no, Reason: reasonPushRejected}, eventWarning},
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: unknown, Reason: reasonUnreachable}, eventWarning},
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: unknown, Reason: reasonPending}, eventNormal},
		{metav1.Condition{Type: treebindv1alpha1.ConditionSynced, Status: unknown, Reason: reasonOutdated}, eventNormal},
		{metav1.Condition{Type: treebindv1alpha1.ConditionConflict, Status: yes, Reason: reasonBlocked}, eventWarning},
		{metav1.Condition{Type: treebindv1alpha1.ConditionConflict, Status: no, Reason: reasonResolved}, eventNormal},
		{metav1.Condition{Type: treebindv1alpha1.ConditionConflict, Status: no, Reason: reasonNoConflict}, eventNormal},
	} {
		if got := eventTypeOf(tt.condition); got != tt.want {
			t.Errorf("%s %s %s: %s, want %s", tt.condition.Type, tt.condition.Status, tt.condition.Reason, got, tt.want)
		}
	}
}

// Every event that a report can make says which condition it is of and what the condition says, carries nothing that
// the agent wrote, and fits in the note of an event, for every step and every reason that the operator defines, one of
// another release, and both directions.
func TestEveryEventNamesItsConditionCarriesNoTextOfTheAgentAndFitsTheAPI(t *testing.T) {
	steps := []string{
		preflight.CheckMount, preflight.CheckWritable, preflight.CheckGitDir, app.CheckLock, app.CheckCheckout,
		app.CheckPull, app.CheckChanges, app.CheckCommit, app.CheckPush, app.CheckConflict, unknownCheckName,
	}
	reasons := make([]string, 0, 1+len(failedStepReasons))
	reasons = append(reasons, reasonNotKnown)
	for reason := range failedStepReasons {
		reasons = append(reasons, reason)
	}
	seen := 0
	for _, direction := range []treebindv1alpha1.SyncDirection{bidirectional, pullOnly} {
		for _, step := range steps {
			for _, reason := range reasons {
				before := pollFrom(awaiting(), direction, syncReport(direction, commitOne), no)
				after := pollFrom(before, direction, syncReport(direction, commitOne, failedCheck(step, reason)), no)
				for _, event := range syncEventsOf(before, after) {
					seen++
					name := fmt.Sprintf("%s %s %s: %s", direction, step, reason, event.note)
					prefix := event.condition + " is " + string(event.status) + ": "
					if !strings.HasPrefix(event.note, prefix) {
						t.Errorf("%s: the note does not begin with %q", name, prefix)
					}
					if strings.Contains(event.note, agentText) || strings.Contains(event.note, unknownCheckName) {
						t.Errorf("%s: the note carries text of the agent", name)
					}
					if len(event.note) > maxEventNote {
						t.Errorf("%s: the note has %d bytes, more than the API takes", name, len(event.note))
					}
					if event.condition != treebindv1alpha1.ConditionSynced && event.condition != treebindv1alpha1.ConditionConflict {
						t.Errorf("%s: an event of the condition %s", name, event.condition)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no report made an event")
	}
}
