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
	"time"
	"unicode"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

// pollOutcome is what one poll of an agent came to.
type pollOutcome struct {
	// failure is the failed poll, or nil when the agent answered with a report.
	failure *statusclient.Error
	// keysFailed says that the operator could not put the cluster's public keys where
	// the agent reads them.
	keysFailed bool
}

// The statuses that the condition has, with the reason of each outcome, are
// docs/adr/0015. The messages hold a fixed sentence for the cause and a hint at what
// to check, and nothing that the poll saw on the way: the message of a failed poll is
// the client's, which holds no token, no text of a network error and nothing taken
// from the agent's answer. A message does not change from one poll to the next while
// the cause stays, so a poll that finds the same thing writes nothing.

// agentUnreachableCondition builds the AgentUnreachable condition for the outcome of
// a poll of the agent of treebind.
//
// failingFor is how long the polls have failed in a row, counted from the first of
// them, and the first failed poll is 0. A failure is Unknown until that has reached
// statusUnreachableAfter, and then True, so that a pod that has just started, a
// kubelet that has not yet shown the keys and a short outage during a rollout do not
// make the condition flap. A condition that is already True stays True while the
// polls fail, whatever the time. A poll that gets a report, whose name does not
// resolve, that could not be made for want of a token, or that was refused because
// the keys did not get to the pod, is not the agent failing to answer, so it never
// makes the condition True.
func agentUnreachableCondition(
	treebind *treebindv1alpha1.Treebind, outcome pollOutcome, previous *metav1.Condition, failingFor time.Duration,
) metav1.Condition {
	condition := metav1.Condition{
		Type:               treebindv1alpha1.ConditionAgentUnreachable,
		ObservedGeneration: treebind.Generation,
	}
	set := func(status metav1.ConditionStatus, reason, message string) metav1.Condition {
		condition.Status, condition.Reason, condition.Message = status, reason, message
		return condition
	}

	failure := outcome.failure
	if failure == nil {
		return set(metav1.ConditionFalse, treebindv1alpha1.ReasonReportReceived, "The operator read the sync agent's report")
	}

	switch {
	case failure.Kind == statusclient.KindNotRunning:
		return set(metav1.ConditionUnknown, treebindv1alpha1.ReasonAgentNotRunning,
			"The name of the sync pod does not resolve, so the pod is probably not running yet; Ready says more")
	case failure.Kind == statusclient.KindTokenUnavailable:
		return set(metav1.ConditionUnknown, treebindv1alpha1.ReasonTokenUnavailable,
			"The operator could not get a token for the sync agent; its log says why")
	case failure.Kind == statusclient.KindTokenRefused && outcome.keysFailed:
		return set(metav1.ConditionUnknown, treebindv1alpha1.ReasonKeysUnavailable, fmt.Sprintf(
			"The operator could not put the cluster's public keys where the sync agent reads them, "+
				"so the agent refuses its token; the operator's log says why, and the ConfigMap %s may be in the way",
			statusKeysConfigMapName(treebind)))
	}

	alreadyTrue := previous != nil && previous.Status == metav1.ConditionTrue
	if !alreadyTrue && failingFor < statusUnreachableAfter {
		return set(metav1.ConditionUnknown, treebindv1alpha1.ReasonPollFailing, fmt.Sprintf(
			"The last poll failed: %s. AgentUnreachable becomes True if the polls keep failing for %d minutes",
			failure.Message, int(statusUnreachableAfter.Minutes())))
	}
	return set(metav1.ConditionTrue, reasonOf(failure.Kind), unreachableMessage(treebind, failure))
}

// reasonOf is the reason of the condition for a failed poll that makes it True. The
// kinds are spelled the way the reasons are, and a test keeps them so.
func reasonOf(kind statusclient.Kind) string {
	switch kind {
	case statusclient.KindConnectionFailed:
		return treebindv1alpha1.ReasonConnectionFailed
	case statusclient.KindTokenRefused:
		return treebindv1alpha1.ReasonTokenRefused
	case statusclient.KindUnexpectedResponse:
		return treebindv1alpha1.ReasonUnexpectedResponse
	default:
		return treebindv1alpha1.ReasonReportUnreadable
	}
}

// unreachableMessage is the message of the condition when it is True: what failed and
// what is the likeliest thing to check.
func unreachableMessage(treebind *treebindv1alpha1.Treebind, failure *statusclient.Error) string {
	what := capitalize(failure.Message)
	switch failure.Kind {
	case statusclient.KindConnectionFailed:
		return fmt.Sprintf("The operator cannot reach the sync agent: %s. A network policy or a service mesh may be "+
			"blocking it: the operator has to be allowed to reach port %d of the sync pod", failure.Message, statusclient.Port)
	case statusclient.KindTokenRefused:
		return fmt.Sprintf("%s. The cluster's public keys may not have reached the sync pod yet, "+
			"or the ConfigMap %s may hold other keys", what, statusKeysConfigMapName(treebind))
	case statusclient.KindUnexpectedResponse:
		return what + ". The log of the sync pod says more"
	default:
		return what + ". The sync agent and the operator may be of different versions; the log of the sync pod says more"
	}
}

// capitalize returns the text with its first letter in capitals, so that the message
// of a failed poll starts a sentence.
func capitalize(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if first == utf8.RuneError {
		return text
	}
	return string(unicode.ToUpper(first)) + text[size:]
}
