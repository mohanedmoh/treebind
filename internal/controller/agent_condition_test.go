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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/statusclient"
)

const (
	unknown = metav1.ConditionUnknown
	yes     = metav1.ConditionTrue
	no      = metav1.ConditionFalse

	// The reasons that more than one case spells out. They are API, so the table spells
	// them as users match on them and not through the constants.
	reasonPollFailing       = "PollFailing"
	reasonConnectionFailed  = "ConnectionFailed"
	messagePollFailingTail  = "AgentUnreachable becomes True if the polls keep failing for 3 minutes"
	messageUnreachableStart = "The operator cannot reach the sync agent: "
	messageUnreachableTail  = ". A network policy or a service mesh may be blocking it: " +
		"the operator has to be allowed to reach port 8080 of the sync pod"

	// The messages of the client, which the condition builds on.
	msgRefusedConnection = "the sync pod refused the connection"
	msgTokenRefused      = "the sync agent refused the operator's token"
	msgAnswered503       = "the sync agent answered 503"
	msgNotJSON           = "the sync agent's report is not JSON"

	// secretDetail stands for the text of a network error that the client keeps for the
	// log: an address, a port, a name. A condition must not carry it.
	secretDetail = "dial tcp 10.244.0.7:8080: connect: connection refused (operator-token-s3cret)"
)

// failed returns the failure of a poll with a message and a cause that is for the log.
func failed(kind statusclient.Kind, message string) *statusclient.Error {
	return &statusclient.Error{Kind: kind, Message: message, Err: errors.New(secretDetail)}
}

func testTreebindWithGeneration(generation int64) *treebindv1alpha1.Treebind {
	treebind := newTreebind(testTreebindName)
	treebind.Namespace = testNamespace
	treebind.Generation = generation
	return treebind
}

func conditionWith(status metav1.ConditionStatus) *metav1.Condition {
	return &metav1.Condition{Type: treebindv1alpha1.ConditionAgentUnreachable, Status: status}
}

func TestAgentUnreachableConditionForTheOutcomeOfAPoll(t *testing.T) {
	const keysName = testTreebindName + "-sync-keys"
	after := statusUnreachableAfter

	tests := []struct {
		name        string
		outcome     pollOutcome
		previous    *metav1.Condition
		failingFor  time.Duration
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:       "a report",
			outcome:    pollOutcome{},
			wantStatus: no, wantReason: "ReportReceived", wantMessage: "The operator read the sync agent's report",
		},
		{
			name:       "a report ends a condition that was True",
			outcome:    pollOutcome{},
			previous:   conditionWith(yes),
			failingFor: 10 * after,
			wantStatus: no, wantReason: "ReportReceived", wantMessage: "The operator read the sync agent's report",
		},
		{
			name:       "a name that does not resolve",
			outcome:    pollOutcome{failure: failed(statusclient.KindNotRunning, "the name of the sync pod does not resolve")},
			wantStatus: unknown, wantReason: "AgentNotRunning",
			wantMessage: "The name of the sync pod does not resolve, so the pod is probably not running yet; Ready says more",
		},
		{
			name:       "a name that does not resolve is never True, however long, and clears a True",
			outcome:    pollOutcome{failure: failed(statusclient.KindNotRunning, "the name of the sync pod does not resolve")},
			previous:   conditionWith(yes),
			failingFor: 10 * after,
			wantStatus: unknown, wantReason: "AgentNotRunning",
			wantMessage: "The name of the sync pod does not resolve, so the pod is probably not running yet; Ready says more",
		},
		{
			name: "no token, however long, and it clears a True",
			outcome: pollOutcome{failure: failed(statusclient.KindTokenUnavailable,
				"the operator could not get a token for the sync agent")},
			previous:   conditionWith(yes),
			failingFor: 10 * after,
			wantStatus: unknown, wantReason: "TokenUnavailable",
			wantMessage: "The operator could not get a token for the sync agent; its log says why",
		},
		{
			name:       "a refused token while the keys are not published",
			outcome:    pollOutcome{failure: failed(statusclient.KindTokenRefused, msgTokenRefused), keysFailed: true},
			failingFor: 10 * after,
			wantStatus: unknown, wantReason: "KeysUnavailable",
			wantMessage: "The operator could not put the cluster's public keys where the sync agent reads them, " +
				"so the agent refuses its token; the operator's log says why, and the ConfigMap " + keysName + " may be in the way",
		},
		{
			name:       "a refused token while the keys are not published clears a True",
			outcome:    pollOutcome{failure: failed(statusclient.KindTokenRefused, msgTokenRefused), keysFailed: true},
			previous:   conditionWith(yes),
			failingFor: 10 * after,
			wantStatus: unknown, wantReason: "KeysUnavailable",
			wantMessage: "The operator could not put the cluster's public keys where the sync agent reads them, " +
				"so the agent refuses its token; the operator's log says why, and the ConfigMap " + keysName + " may be in the way",
		},
		{
			name:       "keys that are not published say nothing about a connection that failed",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, msgRefusedConnection), keysFailed: true},
			failingFor: after,
			wantStatus: yes, wantReason: reasonConnectionFailed,
			wantMessage: messageUnreachableStart + msgRefusedConnection + messageUnreachableTail,
		},
		{
			name:       "the first failed poll",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, msgRefusedConnection)},
			wantStatus: unknown, wantReason: reasonPollFailing,
			wantMessage: "The last poll failed: the sync pod refused the connection. " + messagePollFailingTail,
		},
		{
			name:       "a failure that has lasted just under the time",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, msgRefusedConnection)},
			previous:   conditionWith(unknown),
			failingFor: after - time.Nanosecond,
			wantStatus: unknown, wantReason: reasonPollFailing,
			wantMessage: "The last poll failed: the sync pod refused the connection. " + messagePollFailingTail,
		},
		{
			name:       "a refused connection that has lasted the time",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, msgRefusedConnection)},
			previous:   conditionWith(unknown),
			failingFor: after,
			wantStatus: yes, wantReason: reasonConnectionFailed,
			wantMessage: messageUnreachableStart + msgRefusedConnection + messageUnreachableTail,
		},
		{
			name:       "no answer in time that has lasted the time",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, "the sync pod did not answer in time")},
			failingFor: after,
			wantStatus: yes, wantReason: reasonConnectionFailed,
			wantMessage: messageUnreachableStart + "the sync pod did not answer in time" + messageUnreachableTail,
		},
		{
			name:       "a refused token that has lasted the time, with the keys in place",
			outcome:    pollOutcome{failure: failed(statusclient.KindTokenRefused, msgTokenRefused)},
			failingFor: after,
			wantStatus: yes, wantReason: "TokenRefused",
			wantMessage: "The sync agent refused the operator's token. The cluster's public keys may not have reached " +
				"the sync pod yet, or the ConfigMap " + keysName + " may hold other keys",
		},
		{
			name:       "another status that has lasted the time",
			outcome:    pollOutcome{failure: failed(statusclient.KindUnexpectedResponse, msgAnswered503)},
			failingFor: after,
			wantStatus: yes, wantReason: "UnexpectedResponse",
			wantMessage: "The sync agent answered 503. The log of the sync pod says more",
		},
		{
			name:       "a report that cannot be read that has lasted the time",
			outcome:    pollOutcome{failure: failed(statusclient.KindReportUnreadable, msgNotJSON)},
			failingFor: after,
			wantStatus: yes, wantReason: "ReportUnreadable",
			wantMessage: "The sync agent's report is not JSON. " +
				"The sync agent and the operator may be of different versions; the log of the sync pod says more",
		},
		{
			name:       "a condition that is True stays True at the first failed poll, with the new reason",
			outcome:    pollOutcome{failure: failed(statusclient.KindUnexpectedResponse, msgAnswered503)},
			previous:   conditionWith(yes),
			failingFor: 0,
			wantStatus: yes, wantReason: "UnexpectedResponse",
			wantMessage: "The sync agent answered 503. The log of the sync pod says more",
		},
		{
			name:       "a condition that was False waits for the time like one that was Unknown",
			outcome:    pollOutcome{failure: failed(statusclient.KindConnectionFailed, msgRefusedConnection)},
			previous:   conditionWith(no),
			failingFor: time.Minute,
			wantStatus: unknown, wantReason: reasonPollFailing,
			wantMessage: "The last poll failed: the sync pod refused the connection. " + messagePollFailingTail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			treebind := testTreebindWithGeneration(7)

			got := agentUnreachableCondition(treebind, tt.outcome, tt.previous, tt.failingFor)

			if got.Type != treebindv1alpha1.ConditionAgentUnreachable {
				t.Errorf("type = %s, want AgentUnreachable", got.Type)
			}
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason || got.Message != tt.wantMessage {
				t.Errorf("condition = %s/%s %q\nwant        %s/%s %q",
					got.Status, got.Reason, got.Message, tt.wantStatus, tt.wantReason, tt.wantMessage)
			}
			if got.ObservedGeneration != 7 {
				t.Errorf("observedGeneration = %d, want the generation of the Treebind it describes, 7", got.ObservedGeneration)
			}
			// What a poll saw on the way stays out of the message.
			for _, secret := range []string{"10.244.0.7", "8080: connect", "operator-token", "s3cret", "dial tcp"} {
				if strings.Contains(got.Message, secret) {
					t.Errorf("the message carries %q of the cause of the poll: %s", secret, got.Message)
				}
			}
			if strings.Contains(got.Message, "\n") || len(got.Message) > 1024 {
				t.Errorf("the message is not a short line: %q", got.Message)
			}
		})
	}
}

// The kinds of a failed poll that make the condition True are the reasons of the
// condition, spelled alike so that the client and the API cannot drift apart. The
// two kinds that make it Unknown are mapped by name to their reasons.
func TestTheKindsOfAFailedPollAreTheReasonsOfTheCondition(t *testing.T) {
	for kind, reason := range map[statusclient.Kind]string{
		statusclient.KindConnectionFailed:   treebindv1alpha1.ReasonConnectionFailed,
		statusclient.KindTokenRefused:       treebindv1alpha1.ReasonTokenRefused,
		statusclient.KindUnexpectedResponse: treebindv1alpha1.ReasonUnexpectedResponse,
		statusclient.KindReportUnreadable:   treebindv1alpha1.ReasonReportUnreadable,
	} {
		if string(kind) != reason {
			t.Errorf("the client calls a failure %q, the API calls its reason %q", kind, reason)
		}
	}
	// A kind that is not one of the four falls to the report that cannot be read.
	if got := reasonOf(statusclient.Kind("Something")); got != treebindv1alpha1.ReasonReportUnreadable {
		t.Errorf("reasonOf(unknown kind) = %q, want ReportUnreadable", got)
	}
}

func TestEveryReasonOfAgentUnreachableIsDistinct(t *testing.T) {
	reasons := []string{
		treebindv1alpha1.ReasonReportReceived, treebindv1alpha1.ReasonConnectionFailed,
		treebindv1alpha1.ReasonTokenRefused, treebindv1alpha1.ReasonUnexpectedResponse,
		treebindv1alpha1.ReasonReportUnreadable, treebindv1alpha1.ReasonPollFailing,
		treebindv1alpha1.ReasonAgentNotRunning, treebindv1alpha1.ReasonTokenUnavailable,
		treebindv1alpha1.ReasonKeysUnavailable, treebindv1alpha1.ReasonAwaitingAgentReport,
	}
	seen := map[string]bool{}
	for _, reason := range reasons {
		if reason == "" || seen[reason] {
			t.Errorf("the reason %q is empty or used twice: each cause has its own (docs/adr/0004)", reason)
		}
		seen[reason] = true
	}
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"the report":        "The report",
		"The agent":         "The agent",
		"éclair":            "Éclair",
		"1 second":          "1 second",
		"\xffbroken":        "\xffbroken",
		"a":                 "A",
		"the name resolves": "The name resolves",
	} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}
