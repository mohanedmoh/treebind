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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// The events of the sync outcome (docs/metrics-and-events.md, docs/adr/0038). The status poller writes the Synced and
// Conflict conditions only when they change, so an event made from what its write changed is one for each change and
// not one for each poll. It is made after the write succeeded, so that a write that loses a conflict with the reconcile
// of the spec makes none, and the poll that writes the change makes it.

// EventReporter is the name of the operator in the events that it records.
const EventReporter = "treebind-operator"

// syncEvent is an event that a change of Synced or Conflict makes.
type syncEvent struct {
	// eventType is Normal or Warning.
	eventType string
	// condition is the type of the condition that changed, which is the action of the event.
	condition string
	// status and reason are what the condition changed to. The reason of the event is the reason of the condition.
	status metav1.ConditionStatus
	reason string
	// note is the condition, its status and its message, which is made of the fixed sentences of the conditions and
	// so holds nothing that the agent wrote.
	note string
}

// syncEventsOf returns the events of the changes of Synced and Conflict from the conditions before a poll to the ones
// after it, Synced first. A condition makes an event when its status or its reason changed: a change of the message
// alone, as for a newer commit, or of the generation alone makes none. Conflict makes none while it is Unknown, which it
// is exactly when Synced is and with the same reason, and none when it comes to NoConflict from Unknown or from
// nothing, which every start of the agent makes it do.
func syncEventsOf(before, after []metav1.Condition) []syncEvent {
	var events []syncEvent
	for _, conditionType := range []string{treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict} {
		now := meta.FindStatusCondition(after, conditionType)
		if now == nil {
			continue
		}
		was := meta.FindStatusCondition(before, conditionType)
		if was != nil && was.Status == now.Status && was.Reason == now.Reason {
			continue
		}
		if conditionType == treebindv1alpha1.ConditionConflict && !conflictChangeIsNews(was, now) {
			continue
		}
		events = append(events, syncEvent{
			eventType: eventTypeOf(*now),
			condition: conditionType,
			status:    now.Status,
			reason:    now.Reason,
			note:      conditionType + " is " + string(now.Status) + ": " + now.Message,
		})
	}
	return events
}

// conflictChangeIsNews says whether a change of Conflict says something that the event of Synced does not.
func conflictChangeIsNews(was, now *metav1.Condition) bool {
	if now.Status == metav1.ConditionUnknown {
		return false
	}
	return (was != nil && was.Status != metav1.ConditionUnknown) || now.Reason != treebindv1alpha1.ReasonNoConflict
}

// eventTypeOf is Warning for a condition that says that a person should look, and Normal for the others: Synced False,
// Conflict True, and Synced Unknown because the operator has not read the agent's report for minutes.
func eventTypeOf(condition metav1.Condition) string {
	switch {
	case condition.Type == treebindv1alpha1.ConditionSynced && condition.Status == metav1.ConditionFalse,
		condition.Type == treebindv1alpha1.ConditionConflict && condition.Status == metav1.ConditionTrue,
		condition.Type == treebindv1alpha1.ConditionSynced && condition.Reason == treebindv1alpha1.ReasonAgentUnreachable:
		return corev1.EventTypeWarning
	}
	return corev1.EventTypeNormal
}
