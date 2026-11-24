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
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/app"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	// The hash of the pod template that the spec gives now, and of the one before.
	hashOfSpec   = "1111111111111111111111111111111111111111111111111111111111111111"
	hashOfBefore = "2222222222222222222222222222222222222222222222222222222222222222"

	// Two commits of the repository, as the agent reports them, and as a message shows them.
	commitOne = "0123456789abcdef0123456789abcdef01234567"
	commitTwo = "fedcba9876543210fedcba9876543210fedcba98"

	syncGeneration = int64(7)

	// agentText stands for what the agent writes in the message of a check: the names of files and the words of the Git
	// host. A condition must not carry it, and neither the name of a check that the operator does not know.
	agentText        = "secret/plan.txt: remote: Permission to team/repo denied to deploy-bot"
	unknownCheckName = "Frobnicate secret/plan.txt"

	// The reasons that more than one case spells out. They are API, so the table spells them as users match on them
	// and not through the constants.
	reasonPending          = "SyncPending"
	reasonOutdated         = "AgentOutdated"
	reasonUnreachable      = "AgentUnreachable"
	reasonNoConflict       = "NoConflict"
	reasonResolved         = "ConflictResolved"
	reasonFetchFailed      = "FetchFailed"
	reasonPushFailed       = "PushFailed"
	reasonPushRejected     = "PushRejected"
	reasonBlocked          = "Blocked"
	reasonNonFastForward   = "NonFastForward"
	reasonTooManyDeletions = "TooManyDeletions"
	reasonTooManyCommits   = "TooManyCommits"
	reasonCommitFailed     = "CommitFailed"
	reasonNotKnown         = "Overheated"

	// The messages, and the parts of them, that more than one case expects.
	msgReadMore    = "The log of the sync pod says more"
	msgAgreedOne   = "The volume and the repository last agreed at commit 0123456789ab"
	msgAgreedTwo   = "The volume and the repository last agreed at commit fedcba987654"
	msgNeverAgreed = "The operator has not seen the volume and the repository agree yet"
	msgRetries     = "The sync agent tries again by itself"
	msgUnresolved  = "A conflict between the volume and the repository could not be resolved, and no pull has " +
		"passed since"
	msgTooManyDelete = "the update would remove more files than spec.sync.maxDeletions allows at once, or every file of " +
		"the branch"
	msgBlocked = "the update changes a file that has a local edit, or that an untracked file is in the way of"
	msgPending = "The sync agent has not seen the volume and the repository agree since it started, and no step has " +
		"failed: the lock, the checkout, the first pull or the first push is still to come"
	msgOutdated = "The sync agent's report is not of the pod template that the spec gives now, as while the sync pod " +
		"is replaced, so it says nothing of the sync of this spec"
	msgUnreachable = "The operator has not been able to read the sync agent's report for 3 minutes or more, so it does " +
		"not know what the sync does; AgentUnreachable says why"
	msgNoConflict = "The sync agent knows of no conflict between the volume and the repository that waits to be " +
		"resolved, and has resolved none since it started"
	msgPullOnly = "The Treebind only pulls, so it has no conflict to resolve: an update that it cannot take makes " +
		"Synced False"
	msgResolved = "The last conflict between the volume and the repository that the sync agent found since it started " +
		"was resolved by spec.sync.conflictPolicy. The log of the sync pod says how"
	msgSyncFailed = "the agent gave a reason that this release of the operator does not define, as an agent of " +
		"another release can"
)

var (
	bidirectional = treebindv1alpha1.SyncDirectionBidirectional
	pullOnly      = treebindv1alpha1.SyncDirectionPull

	// agreedAt is when the agent first saw the volume and the repository agree at the commit it reports.
	agreedAt = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
)

func checkOf(name string, outcome preflight.Outcome, reason string) status.Check {
	return status.Check{Name: name, Outcome: string(outcome), Reason: reason, Message: agentText}
}

func failedCheck(name, reason string) status.Check {
	return checkOf(name, preflight.Failed, reason)
}

// syncReport returns the report of an agent of the current template with the checks of the direction, the ones given in
// place of the ones of the same name and the rest passed, except the Conflict check, which is skipped until a conflict
// was resolved. A check whose name the direction does not have is added at the end. agreed is the commit of lastSynced,
// and none when it is empty.
func syncReport(direction treebindv1alpha1.SyncDirection, agreed string, given ...status.Check) *status.Report {
	names := []string{
		preflight.CheckMount, preflight.CheckWritable, preflight.CheckGitDir, app.CheckLock, app.CheckCheckout, app.CheckPull,
	}
	if direction == bidirectional {
		names = append(names, app.CheckChanges, app.CheckCommit, app.CheckPush, app.CheckConflict)
	}
	checks := make([]status.Check, 0, len(names)+len(given))
	for _, name := range names {
		check := checkOf(name, preflight.Passed, "")
		if name == app.CheckConflict {
			check = checkOf(name, preflight.Skipped, preflight.ReasonNotChecked)
		}
		checks = append(checks, check)
	}
	for _, check := range given {
		replaced := false
		for i := range checks {
			if checks[i].Name == check.Name {
				checks[i], replaced = check, true
			}
		}
		if !replaced {
			checks = append(checks, check)
		}
	}
	report := &status.Report{
		Version:      status.Version,
		Treebind:     status.Treebind{Namespace: testNamespace, Name: testTreebindName},
		StartedAt:    agreedAt.Add(-time.Hour),
		TemplateHash: hashOfSpec,
		Time:         agreedAt.Add(time.Minute),
		CheckedAt:    agreedAt.Add(time.Minute),
		Checks:       checks,
	}
	if agreed != "" {
		report.LastSynced = status.LastSynced{Commit: agreed, Time: agreedAt, By: status.ByPull}
	}
	return report
}

// syncTreebind returns a Treebind of the direction at syncGeneration, with the status.
func syncTreebind(
	direction treebindv1alpha1.SyncDirection, current treebindv1alpha1.TreebindStatus,
) *treebindv1alpha1.Treebind {
	treebind := testTreebindWithGeneration(syncGeneration)
	treebind.Spec.Sync.Direction = direction
	treebind.Status = current
	return treebind
}

// agreedStatus is the status of a Treebind whose volume and repository agreed at the commit, with a Synced condition
// that was written for another generation.
func agreedStatus(commit string) treebindv1alpha1.TreebindStatus {
	agreed := metav1.NewTime(agreedAt.Add(-24 * time.Hour))
	return treebindv1alpha1.TreebindStatus{
		LastSyncedCommit: commit,
		LastSyncTime:     &agreed,
		Conditions: []metav1.Condition{{
			Type: treebindv1alpha1.ConditionSynced, Status: no, Reason: reasonFetchFailed, Message: "an older message",
			ObservedGeneration: syncGeneration - 1, LastTransitionTime: agreed,
		}},
	}
}

// roundTrip returns the Treebind as the API server gives it back: through JSON, which keeps times in whole seconds.
func roundTrip(t *testing.T, treebind *treebindv1alpha1.Treebind) *treebindv1alpha1.Treebind {
	t.Helper()
	data, err := json.Marshal(treebind)
	if err != nil {
		t.Fatalf("encoding the Treebind: %v", err)
	}
	stored := &treebindv1alpha1.Treebind{}
	if err := json.Unmarshal(data, stored); err != nil {
		t.Fatalf("decoding the Treebind: %v", err)
	}
	return stored
}

// wantCondition is a condition that a poll writes. An empty message is not compared.
type wantCondition struct {
	status          metav1.ConditionStatus
	reason, message string
}

type syncCase struct {
	name      string
	direction treebindv1alpha1.SyncDirection
	current   treebindv1alpha1.TreebindStatus
	report    *status.Report
	// unreachable is the status of AgentUnreachable after the poll, False when it is empty.
	unreachable metav1.ConditionStatus
	// unreadFor is how long the polls have got no report, for any reason.
	unreadFor time.Duration
	// synced and conflict are the conditions the poll writes, and nil when it leaves them as they are.
	synced, conflict *wantCondition
	// commit is the commit the poll writes into status.lastSyncedCommit, with agreedAt, and none when it is empty.
	commit string
}

func falseCondition(reason, message string) *wantCondition {
	return &wantCondition{status: no, reason: reason, message: message}
}

func bothUnknown(reason, message string) (synced, conflict *wantCondition) {
	return &wantCondition{status: unknown, reason: reason, message: message},
		&wantCondition{status: unknown, reason: reason, message: message}
}

// syncCases are the reports of a poll and what the poll makes of them.
func syncCases() []syncCase {
	inSyncAtOne := &wantCondition{status: yes, reason: reasonInSync,
		message: "No step of the sync failed. " + msgAgreedOne}
	noConflict := falseCondition(reasonNoConflict, msgNoConflict)
	pullNoConflict := falseCondition(reasonNoConflict, msgPullOnly)
	pendingSynced, pendingConflict := bothUnknown(reasonPending, msgPending)
	outdatedSynced, outdatedConflict := bothUnknown(reasonOutdated, msgOutdated)
	unreachableSynced, unreachableConflict := bothUnknown(reasonUnreachable, msgUnreachable)

	withHash := func(report *status.Report, hash string) *status.Report {
		report.TemplateHash = hash
		return report
	}
	withAgreementAt := func(report *status.Report, at time.Time) *status.Report {
		report.LastSynced.Time = at
		return report
	}

	return []syncCase{
		// In sync, and the agreement.
		{
			name:      "a Treebind that syncs in both directions and agrees",
			direction: bidirectional,
			report:    syncReport(bidirectional, commitOne),
			synced:    inSyncAtOne, conflict: noConflict, commit: commitOne,
		},
		{
			name:      "a Treebind that only pulls and agrees",
			direction: pullOnly,
			report:    syncReport(pullOnly, commitOne),
			synced:    inSyncAtOne, conflict: pullNoConflict, commit: commitOne,
		},
		{
			name:      "a skipped check fails nothing",
			direction: bidirectional,
			report: syncReport(bidirectional, commitOne,
				checkOf(app.CheckPush, preflight.Skipped, preflight.ReasonNotChecked),
				checkOf(app.CheckCommit, preflight.Skipped, preflight.ReasonNotChecked)),
			synced: inSyncAtOne, conflict: noConflict, commit: commitOne,
		},
		{
			name:      "a pull that finds nothing new, or an agent that started again, writes no agreement",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report:    withAgreementAt(syncReport(bidirectional, commitOne), agreedAt.Add(time.Hour+time.Second/2)),
			synced:    inSyncAtOne, conflict: noConflict,
		},
		{
			name:      "a newer agreement while a step fails is written, and the message names it",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report:    syncReport(bidirectional, commitTwo, failedCheck(app.CheckPush, reasonPushRejected)),
			synced: falseCondition(reasonPushRejected, "The last push failed: the repository refused the push, as for a "+
				"protected branch, a hook or a rule. "+msgAgreedTwo+". "+msgReadMore),
			conflict: noConflict, commit: commitTwo,
		},
		{
			name:      "a report with no agreement after a restart keeps the one of the status in the message",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report:    syncReport(bidirectional, "", failedCheck(app.CheckPush, reasonPushRejected)),
			synced: falseCondition(reasonPushRejected, "The last push failed: the repository refused the push, as for a "+
				"protected branch, a hook or a rule. "+msgAgreedOne+". "+msgReadMore),
			conflict: noConflict,
		},

		// Nothing to say yet.
		{
			name:      "no failure and no agreement yet in both directions",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report:    syncReport(bidirectional, ""),
			synced:    pendingSynced, conflict: pendingConflict,
		},
		{
			name:      "no failure and no agreement yet when the Treebind only pulls",
			direction: pullOnly,
			report:    syncReport(pullOnly, ""),
			synced:    pendingSynced, conflict: pendingConflict,
		},
		{
			name:      "no check has finished yet",
			direction: bidirectional,
			report: func() *status.Report {
				report := syncReport(bidirectional, "")
				report.Checks, report.CheckedAt = []status.Check{}, time.Time{}
				return report
			}(),
			synced: pendingSynced, conflict: pendingConflict,
		},
		{
			name:      "a resolution that waits for its push",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				checkOf(app.CheckConflict, preflight.Passed, reasonResolved)),
			synced: pendingSynced, conflict: pendingConflict,
		},

		// The Conflict condition.
		{
			name:      "a resolved conflict",
			direction: bidirectional,
			report: syncReport(bidirectional, commitOne,
				checkOf(app.CheckConflict, preflight.Passed, reasonResolved)),
			synced: inSyncAtOne, conflict: falseCondition(reasonResolved, msgResolved), commit: commitOne,
		},
		{
			name:      "a conflict that could not be resolved",
			direction: bidirectional,
			current:   agreedStatus(commitTwo),
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonTooManyDeletions), failedCheck(app.CheckConflict, reasonTooManyDeletions)),
			synced: falseCondition(reasonTooManyDeletions,
				"The last pull failed: "+msgTooManyDelete+". "+msgAgreedTwo+". "+msgReadMore),
			conflict: &wantCondition{status: yes, reason: reasonTooManyDeletions,
				message: msgUnresolved + ": " + msgTooManyDelete + ". " + msgReadMore},
		},
		{
			name:      "a conflict that could not be resolved keeps its reason while a later pull fails for the host",
			direction: bidirectional,
			current:   agreedStatus(commitTwo),
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonFetchFailed), failedCheck(app.CheckConflict, reasonTooManyDeletions)),
			synced: falseCondition(reasonTooManyDeletions,
				msgUnresolved+": "+msgTooManyDelete+". "+msgAgreedTwo+". "+msgReadMore),
			conflict: &wantCondition{status: yes, reason: reasonTooManyDeletions},
		},
		{
			name:      "a conflict that the next scan and pull put right",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonBlocked), failedCheck(app.CheckConflict, reasonBlocked)),
			synced: falseCondition(reasonBlocked,
				"The last pull failed: "+msgBlocked+". "+msgRetries+". "+msgNeverAgreed+". "+msgReadMore),
			conflict: &wantCondition{status: yes, reason: reasonBlocked,
				message: msgUnresolved + ": " + msgBlocked + ". " + msgRetries + ". " + msgReadMore},
		},
		{
			name:      "a conflict that could not be resolved with a reason this release does not define",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonNotKnown), failedCheck(app.CheckConflict, reasonNotKnown)),
			synced:   falseCondition(reasonSyncFailed, ""),
			conflict: &wantCondition{status: yes, reason: reasonSyncFailed},
		},
		{
			name:      "a volume that fails its checks, so that the agent has not come to the checkout",
			direction: bidirectional,
			report: func() *status.Report {
				report := syncReport(bidirectional, "")
				report.Checks = []status.Check{
					failedCheck(preflight.CheckMount, "NotMounted"),
					checkOf(preflight.CheckWritable, preflight.Skipped, preflight.ReasonNotChecked),
					checkOf(preflight.CheckGitDir, preflight.Skipped, preflight.ReasonNotChecked),
				}
				return report
			}(),
			synced: falseCondition("NotMounted", "The check of the volume's mount failed: the path is on the container's "+
				"own filesystem and not on a mounted volume. "+msgNeverAgreed+". "+msgReadMore),
			conflict: noConflict,
		},
		{
			name:      "a Treebind that only pulls and cannot take the update",
			direction: pullOnly,
			report:    syncReport(pullOnly, "", failedCheck(app.CheckPull, reasonBlocked)),
			synced: falseCondition(reasonBlocked,
				"The last pull failed: "+msgBlocked+". "+msgNeverAgreed+". "+msgReadMore),
			conflict: pullNoConflict,
		},
		{
			name:      "a Treebind that only pulls and diverged",
			direction: pullOnly,
			report:    syncReport(pullOnly, "", failedCheck(app.CheckPull, "Diverged")),
			synced:    falseCondition("Diverged", ""), conflict: pullNoConflict,
		},
		{
			name:      "a Treebind that only pulls has no conflict, whatever a check of that name says",
			direction: pullOnly,
			report:    syncReport(pullOnly, commitOne, failedCheck(app.CheckConflict, reasonTooManyDeletions)),
			synced:    falseCondition(reasonTooManyDeletions, ""), conflict: pullNoConflict, commit: commitOne,
		},

		// The order of the failed steps.
		{
			name:      "a reason that waits for a person comes before one that waits for the repository",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonFetchFailed), failedCheck(app.CheckPush, reasonPushRejected)),
			synced: falseCondition(reasonPushRejected, "The last push failed: the repository refused the push, as for a "+
				"protected branch, a hook or a rule. "+msgAgreedOne+". "+msgReadMore),
			conflict: noConflict,
		},
		{
			name:      "of two reasons that wait for the repository the first step's",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonFetchFailed), failedCheck(app.CheckPush, reasonPushFailed)),
			synced: falseCondition(reasonFetchFailed, "The last pull failed: the repository could not be asked: the host "+
				"cannot be reached, the key is refused, or the repository does not exist. "+msgRetries+". "+
				msgAgreedOne+". "+msgReadMore),
			conflict: noConflict,
		},
		{
			name:      "of two reasons that wait for a person the first step's",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPull, reasonTooManyDeletions), failedCheck(app.CheckCommit, reasonCommitFailed)),
			synced: falseCondition(reasonTooManyDeletions, ""), conflict: noConflict,
		},
		{
			name:      "Blocked waits for the repository when the Treebind syncs in both directions",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckCheckout, reasonFetchFailed), failedCheck(app.CheckPull, reasonBlocked)),
			synced: falseCondition(reasonFetchFailed, ""), conflict: noConflict,
		},
		{
			name:      "Blocked waits for a person when the Treebind only pulls",
			direction: pullOnly,
			report: syncReport(pullOnly, "",
				failedCheck(app.CheckCheckout, reasonFetchFailed), failedCheck(app.CheckPull, reasonBlocked)),
			synced: falseCondition(reasonBlocked, ""), conflict: pullNoConflict,
		},
		{
			name:      "NonFastForward waits for the repository when the Treebind syncs in both directions",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPush, reasonNonFastForward), failedCheck(app.CheckConflict, reasonTooManyCommits)),
			synced:   falseCondition(reasonTooManyCommits, ""),
			conflict: &wantCondition{status: yes, reason: reasonTooManyCommits},
		},
		{
			name:      "NonFastForward waits for a person when the Treebind only pulls",
			direction: pullOnly,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckPush, reasonNonFastForward), failedCheck(app.CheckConflict, reasonTooManyCommits)),
			synced: falseCondition(reasonNonFastForward, ""), conflict: pullNoConflict,
		},
		{
			name:      "the only failure waits for the repository",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report:    syncReport(bidirectional, commitOne, failedCheck(app.CheckPush, reasonNonFastForward)),
			synced: falseCondition(reasonNonFastForward, "The last push failed: the repository has commits on the branch "+
				"that the volume lacks, so the push is not a fast-forward. "+msgRetries+". "+msgAgreedOne+". "+msgReadMore),
			conflict: noConflict,
		},

		// A reason or a step that this release does not know.
		{
			name:      "a reason that this release does not define",
			direction: bidirectional,
			report:    syncReport(bidirectional, "", failedCheck(app.CheckPull, reasonNotKnown)),
			synced: falseCondition(reasonSyncFailed,
				"The last pull failed: "+msgSyncFailed+". "+msgNeverAgreed+". "+msgReadMore),
			conflict: noConflict,
		},
		{
			name:      "a reason that this release does not define waits for a person",
			direction: bidirectional,
			report: syncReport(bidirectional, "",
				failedCheck(app.CheckCheckout, reasonFetchFailed), failedCheck(app.CheckPull, reasonNotKnown)),
			synced: falseCondition(reasonSyncFailed, ""), conflict: noConflict,
		},
		{
			name:      "a failed step with no reason",
			direction: pullOnly,
			report:    syncReport(pullOnly, "", failedCheck(app.CheckPull, "")),
			synced:    falseCondition(reasonSyncFailed, ""), conflict: pullNoConflict,
		},
		{
			name:      "a failed step with the reason of a skipped one",
			direction: pullOnly,
			report:    syncReport(pullOnly, "", failedCheck(app.CheckPull, preflight.ReasonNotChecked)),
			synced:    falseCondition(reasonSyncFailed, ""), conflict: pullNoConflict,
		},
		{
			name:      "a step that this release does not know",
			direction: bidirectional,
			report:    syncReport(bidirectional, "", failedCheck(unknownCheckName, reasonPushRejected)),
			synced: falseCondition(reasonPushRejected, "A step of the sync that this release of the operator does not "+
				"name failed: the repository refused the push, as for a protected branch, a hook or a rule. "+
				msgNeverAgreed+". "+msgReadMore),
			conflict: noConflict,
		},

		// A report of another template.
		{
			name:      "a report of the template of the spec before",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
			report: withHash(syncReport(bidirectional, commitTwo, failedCheck(app.CheckPush, reasonPushRejected)),
				hashOfBefore),
			synced: outdatedSynced, conflict: outdatedConflict,
		},
		{
			name:      "a report of an agent of the release before, which has no hash",
			direction: bidirectional,
			report:    withHash(syncReport(bidirectional, commitOne), ""),
			synced:    outdatedSynced, conflict: outdatedConflict,
		},
		{
			name:      "a report of another template when the Treebind only pulls",
			direction: pullOnly,
			report:    withHash(syncReport(pullOnly, commitOne), hashOfBefore),
			synced:    outdatedSynced, conflict: outdatedConflict,
		},

		// AgentUnreachable.
		{
			name:        "an agent that cannot be read",
			direction:   bidirectional,
			current:     agreedStatus(commitOne),
			unreachable: yes,
			synced:      unreachableSynced, conflict: unreachableConflict,
		},
		{
			name:        "an agent that cannot be read when the Treebind only pulls",
			direction:   pullOnly,
			unreachable: yes,
			synced:      unreachableSynced, conflict: unreachableConflict,
		},
		{
			name:        "AgentUnreachable True comes before a report",
			direction:   bidirectional,
			report:      syncReport(bidirectional, commitTwo),
			unreachable: yes,
			synced:      unreachableSynced, conflict: unreachableConflict,
		},
		{
			name:        "a poll that failed for less than the time leaves everything",
			direction:   bidirectional,
			current:     agreedStatus(commitOne),
			unreachable: unknown,
		},
		{
			name:        "AgentUnreachable Unknown leaves everything, also with a report",
			direction:   bidirectional,
			current:     agreedStatus(commitOne),
			report:      syncReport(bidirectional, commitTwo),
			unreachable: unknown,
		},

		// No report for the time, while AgentUnreachable stays Unknown: a sync pod that is Pending, whose name does not
		// resolve, or an operator that has no token (#266).
		{
			name:        "no report for just under the time leaves everything",
			direction:   bidirectional,
			current:     agreedStatus(commitOne),
			unreachable: unknown,
			unreadFor:   statusUnreachableAfter - time.Nanosecond,
		},
		{
			name:        "no report for the time, while AgentUnreachable is Unknown",
			direction:   bidirectional,
			current:     agreedStatus(commitOne),
			unreachable: unknown,
			unreadFor:   statusUnreachableAfter,
			synced:      unreachableSynced, conflict: unreachableConflict,
		},
		{
			name:        "no report for longer, when the Treebind only pulls",
			direction:   pullOnly,
			current:     agreedStatus(commitOne),
			unreachable: unknown,
			unreadFor:   time.Hour,
			synced:      unreachableSynced, conflict: unreachableConflict,
		},
		{
			name:      "no report leaves everything",
			direction: bidirectional,
			current:   agreedStatus(commitOne),
		},
	}
}

func TestTheSyncConditionsForAReport(t *testing.T) {
	for _, tc := range syncCases() {
		t.Run(tc.name, func(t *testing.T) {
			unreachable := tc.unreachable
			if unreachable == "" {
				unreachable = no
			}
			treebind := syncTreebind(tc.direction, tc.current)

			got := syncStatusOf(treebind, tc.report, unreachable, tc.unreadFor, hashOfSpec)

			expectCondition(t, treebindv1alpha1.ConditionSynced, got.synced, tc.synced)
			expectCondition(t, treebindv1alpha1.ConditionConflict, got.conflict, tc.conflict)
			if got.lastSyncedCommit != tc.commit {
				t.Errorf("lastSyncedCommit = %q, want %q", got.lastSyncedCommit, tc.commit)
			}
			switch {
			case tc.commit == "" && got.lastSyncTime != nil:
				t.Errorf("lastSyncTime = %v, want none written", got.lastSyncTime)
			case tc.commit != "" && (got.lastSyncTime == nil || !got.lastSyncTime.Time.Equal(agreedAt)):
				t.Errorf("lastSyncTime = %v, want %v, when the agent first saw them agree", got.lastSyncTime, agreedAt)
			}

			if tc.synced == nil && tc.conflict == nil && tc.commit == "" {
				before := treebind.Status.DeepCopy()
				if got.apply(&treebind.Status) || !reflect.DeepEqual(before, &treebind.Status) {
					t.Errorf("a poll that says nothing changed the status:\n%+v\nwant\n%+v", treebind.Status, *before)
				}
			}
		})
	}
}

func expectCondition(t *testing.T, conditionType string, got *metav1.Condition, want *wantCondition) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %s %s %q, want it left as it is", conditionType, got.Status, got.Reason, got.Message)
	case want == nil:
	case got == nil:
		t.Errorf("%s is left as it is, want %s %s", conditionType, want.status, want.reason)
	default:
		if got.Type != conditionType || got.Status != want.status || got.Reason != want.reason {
			t.Errorf("%s = %s %s %s, want %s %s", conditionType, got.Type, got.Status, got.Reason, want.status, want.reason)
		}
		if want.message != "" && got.Message != want.message {
			t.Errorf("%s message =\n%q\nwant\n%q", conditionType, got.Message, want.message)
		}
		if got.ObservedGeneration != syncGeneration {
			t.Errorf("%s observedGeneration = %d, want the Treebind's, %d",
				conditionType, got.ObservedGeneration, syncGeneration)
		}
		if strings.Contains(got.Message, agentText) || strings.Contains(got.Message, unknownCheckName) {
			t.Errorf("%s message %q carries text of the agent's report", conditionType, got.Message)
		}
	}
}

// The poller writes the status only when a poll changed it, so a poll that finds what the last one found has to say
// that nothing changed, also once the status has been through the API server.
func TestAPollThatFindsTheSameThingChangesNothing(t *testing.T) {
	for _, tc := range syncCases() {
		t.Run(tc.name, func(t *testing.T) {
			unreachable := tc.unreachable
			if unreachable == "" {
				unreachable = no
			}
			treebind := syncTreebind(tc.direction, tc.current)
			changed := syncStatusOf(treebind, tc.report, unreachable, tc.unreadFor, hashOfSpec).apply(&treebind.Status)
			if wantChange := tc.synced != nil || tc.conflict != nil || tc.commit != ""; changed != wantChange {
				t.Errorf("the first poll says changed = %v, want %v", changed, wantChange)
			}

			stored := roundTrip(t, treebind)
			if syncStatusOf(stored, tc.report, unreachable, tc.unreadFor, hashOfSpec).apply(&stored.Status) {
				t.Errorf("the same poll again changed the status, so the poller would write at every poll")
			}
		})
	}
}

func TestLastSyncTimeIsWrittenInWholeSeconds(t *testing.T) {
	agreedWithin := agreedAt.Add(123456789 * time.Nanosecond)
	report := syncReport(bidirectional, commitOne)
	report.LastSynced.Time = agreedWithin
	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})

	if !syncStatusOf(treebind, report, no, 0, hashOfSpec).apply(&treebind.Status) {
		t.Fatalf("the first agreement changed nothing")
	}
	written := treebind.Status.LastSyncTime
	if written == nil || !written.Time.Equal(agreedAt) {
		t.Errorf("lastSyncTime = %v, want %v: the time of the report to the second", written, agreedAt)
	}
	stored := roundTrip(t, treebind)
	if !stored.Status.LastSyncTime.Equal(written) {
		t.Errorf("lastSyncTime is %v as written and %v as stored: the status that the poller holds is not the one "+
			"that the API server keeps", written, stored.Status.LastSyncTime)
	}
	if syncStatusOf(stored, report, no, 0, hashOfSpec).apply(&stored.Status) {
		t.Errorf("the same report changed the stored status again")
	}
}

// The operator gives every pod template that it writes its hash. A hash that it could not compute is no hash of the
// spec, and must not be taken for the hash that an agent of the release before does not report.
func TestAReportWithNoHashIsNotOfASpecWithNone(t *testing.T) {
	report := syncReport(bidirectional, commitOne)
	report.TemplateHash = ""
	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})

	got := syncStatusOf(treebind, report, no, 0, "")

	outdatedSynced, outdatedConflict := bothUnknown(reasonOutdated, msgOutdated)
	expectCondition(t, treebindv1alpha1.ConditionSynced, got.synced, outdatedSynced)
	expectCondition(t, treebindv1alpha1.ConditionConflict, got.conflict, outdatedConflict)
	if got.lastSyncedCommit != "" {
		t.Errorf("lastSyncedCommit = %q from a report of no known template, want none written", got.lastSyncedCommit)
	}
}

// A message is a fixed sentence for its cause, so a report that says the same thing in other words, at another time
// or after a restart of the agent makes the same conditions, and the poller writes nothing.
func TestTheMessagesStayWhileTheCauseStays(t *testing.T) {
	first := syncReport(bidirectional, commitOne,
		failedCheck(app.CheckPush, reasonPushRejected), failedCheck(app.CheckConflict, reasonTooManyCommits))
	later := syncReport(bidirectional, commitOne,
		failedCheck(app.CheckPush, reasonPushRejected), failedCheck(app.CheckConflict, reasonTooManyCommits))
	for i := range later.Checks {
		later.Checks[i].Message = "another text of the agent for " + later.Checks[i].Name
	}
	later.StartedAt, later.Time, later.CheckedAt =
		agreedAt.Add(time.Hour), agreedAt.Add(2*time.Hour), agreedAt.Add(2*time.Hour)
	restarted := syncReport(bidirectional, "",
		failedCheck(app.CheckPush, reasonPushRejected), failedCheck(app.CheckConflict, reasonTooManyCommits))
	restarted.StartedAt = agreedAt.Add(3 * time.Hour)

	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	made := syncStatusOf(treebind, first, no, 0, hashOfSpec)
	made.apply(&treebind.Status)
	for _, report := range []*status.Report{later, restarted} {
		again := syncStatusOf(treebind, report, no, 0, hashOfSpec)
		if again.synced.Message != made.synced.Message || again.conflict.Message != made.conflict.Message {
			t.Errorf("the messages changed while the cause stayed:\n%q\n%q\nwant\n%q\n%q",
				again.synced.Message, again.conflict.Message, made.synced.Message, made.conflict.Message)
		}
		if again.apply(&treebind.Status) {
			t.Errorf("a report of the same cause changed the status")
		}
	}
}

// The poller writes the status when apply says that something changed, and the agreement is written only for another
// commit: the time of an agreement at the commit of the status is when the agent saw it again, not when it came about.
func TestApplyWritesTheAgreementOnlyForAnotherCommit(t *testing.T) {
	treebind := syncTreebind(bidirectional, agreedStatus(commitOne))
	keptTime := treebind.Status.LastSyncTime.DeepCopy()
	later := metav1.NewTime(agreedAt)

	if (syncStatus{lastSyncedCommit: commitOne, lastSyncTime: &later}).apply(&treebind.Status) {
		t.Errorf("the agreement at the commit of the status changed the status")
	}
	if !treebind.Status.LastSyncTime.Equal(keptTime) {
		t.Errorf("lastSyncTime = %v, want %v kept", treebind.Status.LastSyncTime, keptTime)
	}
	if !(syncStatus{lastSyncedCommit: commitTwo, lastSyncTime: &later}).apply(&treebind.Status) {
		t.Errorf("an agreement at another commit says that nothing changed, so the poller would not write it")
	}
	if treebind.Status.LastSyncedCommit != commitTwo || !treebind.Status.LastSyncTime.Equal(&later) {
		t.Errorf("the status agrees at %q since %v, want %q since %v",
			treebind.Status.LastSyncedCommit, treebind.Status.LastSyncTime, commitTwo, later)
	}
}

func TestWhichReasonsWaitForTheRepository(t *testing.T) {
	healing := map[treebindv1alpha1.SyncDirection][]string{
		pullOnly:      {reasonFetchFailed, reasonPushFailed},
		bidirectional: {reasonFetchFailed, reasonPushFailed, reasonNonFastForward, reasonBlocked},
	}
	reasons := make([]string, 0, 1+len(failedStepReasons))
	reasons = append(reasons, reasonSyncFailed)
	for reason := range failedStepReasons {
		reasons = append(reasons, reason)
	}
	for direction, want := range healing {
		for _, reason := range reasons {
			wantsRepository := false
			for _, healed := range want {
				wantsRepository = wantsRepository || healed == reason
			}
			if got := WaitsForRepository(reason, direction); got != wantsRepository {
				t.Errorf("WaitsForRepository(%q, %s) = %v, want %v", reason, direction, got, wantsRepository)
			}
		}
	}
}

// The operator names the steps of the report as the agent does, and does not build package app in.
func TestTheStepsAreNamedAsTheAgentNamesThem(t *testing.T) {
	for _, step := range []struct{ operator, agent string }{
		{checkLock, app.CheckLock},
		{checkCheckout, app.CheckCheckout},
		{checkPull, app.CheckPull},
		{checkChanges, app.CheckChanges},
		{checkCommit, app.CheckCommit},
		{checkPush, app.CheckPush},
		{checkConflict, app.CheckConflict},
	} {
		if step.operator != step.agent {
			t.Errorf("the operator names the step %q and the agent %q", step.operator, step.agent)
		}
	}
	for _, check := range []string{
		preflight.CheckMount, preflight.CheckWritable, preflight.CheckGitDir, app.CheckLock, app.CheckCheckout,
		app.CheckPull, app.CheckChanges, app.CheckCommit, app.CheckPush, app.CheckConflict,
	} {
		if phrase := stepPhrase(check); phrase == unknownStepPhrase || phrase == "" {
			t.Errorf("the message of a failed %s step does not say which step it is", check)
		}
	}
}
