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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// The Synced and Conflict conditions and status.lastSyncedCommit and status.lastSyncTime are what the operator makes of
// the report of the sync agent (docs/adr/0004, docs/adr/0015). They are made here, from the report, the Treebind and
// the AgentUnreachable condition of the same poll, by rules that need no clock and no client, so that the poller only
// writes what comes out, in the same write as AgentUnreachable, and only when something changed.
//
// A message is a fixed sentence for the step and the reason, the short id of status.lastSyncedCommit, and where to read
// more: nothing of the agent's own text goes into it, not its messages and not the name of a check that this release
// does not know, because everyone who can read the Treebind reads the message, and the text of a check names files and
// quotes the Git host. A message does not change while its cause stays, and the commit it names is the one of the
// status, which only a newer agreement changes, so that a poll that finds the same thing writes nothing and a restart
// of the agent changes no message.

// The names of the checks of the sync agent's report after the checks of the volume, which preflight names. The agent
// spells them in package app, which the operator does not build in, and a test holds them to the same spelling.
const (
	checkLock     = "Lock"
	checkCheckout = "Checkout"
	checkPull     = "Pull"
	checkChanges  = "Changes"
	checkCommit   = "Commit"
	checkPush     = "Push"
	checkConflict = "Conflict"
)

// shortCommitLength is how much of a commit id a message shows, which is as much as the messages of the agent show.
const shortCommitLength = 12

// The messages that do not depend on a step.
const (
	syncPendingMessage = "The sync agent has not seen the volume and the repository agree since it started, and no " +
		"step has failed: the lock, the checkout, the first pull or the first push is still to come"
	agentOutdatedMessage = "The sync agent's report is not of the pod template that the spec gives now, as while the " +
		"sync pod is replaced, so it says nothing of the sync of this spec"
	readMoreSentence  = "The log of the sync pod says more"
	retriesSentence   = "The sync agent tries again by itself"
	noConflictMessage = "The sync agent knows of no conflict between the volume and the repository that waits to be " +
		"resolved, and has resolved none since it started"
	pullOnlyMessage = "The Treebind only pulls, so it has no conflict to resolve: an update that it cannot take makes " +
		"Synced False"
	conflictResolvedMessage = "The last conflict between the volume and the repository that the sync agent found since " +
		"it started was resolved by spec.sync.conflictPolicy. The log of the sync pod says how"
	unresolvedConflictPhrase = "A conflict between the volume and the repository could not be resolved, and no pull " +
		"has passed since"
)

// agentUnreachableMessage is the message of both conditions while AgentUnreachable is True.
var agentUnreachableMessage = fmt.Sprintf("The operator has not been able to read the sync agent's report for %d "+
	"minutes or more, so it does not know what the sync does; AgentUnreachable says why",
	int(statusUnreachableAfter.Minutes()))

// stepPhrases are the beginnings of the message of Synced False, for each check of the agent's report. A check that
// is not here is named by unknownStepPhrase and not by the name that the report gives it.
var stepPhrases = map[string]string{
	preflight.CheckMount:    "The check of the volume's mount failed",
	preflight.CheckWritable: "The check that the volume can be written failed",
	preflight.CheckGitDir:   "The check of the Git directory failed",
	checkLock:               "The writer lock of the volume could not be taken",
	checkCheckout:           "The checkout of the repository could not be made",
	checkPull:               "The last pull failed",
	checkChanges:            "The last scan for local changes failed",
	checkCommit:             "The last commit failed",
	checkPush:               "The last push failed",
	checkConflict:           unresolvedConflictPhrase,
}

// unknownStepPhrase begins the message of Synced False for a check that this release does not know, as an agent of
// another release can report.
const unknownStepPhrase = "A step of the sync that this release of the operator does not name failed"

// failedStepReasons are the reasons of a step that failed that this release defines, as the API names them and the
// agent spells them, each with the sentence that a message says for it. It is the one list of them: Synced False and
// Conflict True take one of these, and a failed step with any other reason, from an agent of another release, is
// written as SyncFailed. A test holds them to the reasons that the agent gives (docs/adr/0009).
var failedStepReasons = map[string]string{
	// Mount, Writable and GitDir.
	treebindv1alpha1.ReasonMountPathInvalid: "the path where the volume is mounted does not exist, cannot be read or " +
		"is not a directory",
	treebindv1alpha1.ReasonNotMounted:     "the path is on the container's own filesystem and not on a mounted volume",
	treebindv1alpha1.ReasonNotWritable:    "the user of the sync agent cannot create a file in the directory",
	treebindv1alpha1.ReasonReadOnlyVolume: "the volume refuses writes",
	treebindv1alpha1.ReasonWriteFailed:    "writing to the volume failed, as when it is full",
	treebindv1alpha1.ReasonGitDirUnsafe: "the Git directory, the state directory or a directory on the way to the " +
		"synced directory is not as the agent keeps it, so Git is not run against it",
	treebindv1alpha1.ReasonTimedOut: "the volume did not answer in time",
	// Lock.
	treebindv1alpha1.ReasonLockFailed: "the volume refused the lock, or the lock is held in a format that a newer " +
		"release wrote",
	// Checkout.
	treebindv1alpha1.ReasonDirectoryNotEmpty: "the directory holds files that are not the branch's, and the agent " +
		"does not write over data it did not put there",
	treebindv1alpha1.ReasonRepositoryMismatch: "the Git directory on the volume is of another repository or follows " +
		"another branch",
	treebindv1alpha1.ReasonBranchNotFound: "the repository has no such branch, or no default branch to follow",
	treebindv1alpha1.ReasonReservedPath:   "the branch has a file where the agent keeps its own state",
	treebindv1alpha1.ReasonFetchFailed: "the repository could not be asked: the host cannot be reached, the key is " +
		"refused, or the repository does not exist",
	treebindv1alpha1.ReasonCheckoutFailed: "the checkout could not be made on the volume",
	treebindv1alpha1.ReasonPathNotFound:   "the branch has no directory at spec.repository.path",
	treebindv1alpha1.ReasonPathMismatch: "the checkout was made for another directory of the repository than " +
		"spec.repository.path names, and a checkout is not changed to another",
	// Pull.
	treebindv1alpha1.ReasonBlocked: "the update changes a file that has a local edit, or that an untracked file is " +
		"in the way of",
	treebindv1alpha1.ReasonDiverged: "the volume and the repository each have commits that the other lacks, and a " +
		"Treebind that only pulls has no conflict policy to say which wins",
	treebindv1alpha1.ReasonBranchGone: "the repository has no such branch any more",
	treebindv1alpha1.ReasonTooManyDeletions: "the update would remove more files than spec.sync.maxDeletions allows " +
		"at once, or every file of the branch",
	treebindv1alpha1.ReasonRefused: "the agent stopped the update itself, as when it could not check the writer lock",
	treebindv1alpha1.ReasonTooManyCommits: "the agent has made as many commits in the last hour as " +
		"spec.commit.maxPerHour allows",
	treebindv1alpha1.ReasonPullFailed: "Git failed on the volume, or the checkout is not what a pull expects",
	// Changes, Commit and Push.
	treebindv1alpha1.ReasonScanFailed: "the agent could not look for the files that changed on the volume",
	treebindv1alpha1.ReasonCommitFailed: "Git refused the commit, the lock could not be checked, or the volume " +
		"refused a write",
	treebindv1alpha1.ReasonNonFastForward: "the repository has commits on the branch that the volume lacks, so the " +
		"push is not a fast-forward",
	treebindv1alpha1.ReasonPushRejected: "the repository refused the push, as for a protected branch, a hook or a rule",
	treebindv1alpha1.ReasonPushFailed: "the push could not be made: the repository did not answer, the key was " +
		"refused, the volume refused, or the lock could not be checked",
}

// syncFailedSentence is what a message says for a failed step whose reason this release does not define.
const syncFailedSentence = "the agent gave a reason that this release of the operator does not define, as an agent " +
	"of another release can"

// WaitsForRepository says whether a step that failed with the reason, as Synced names it, puts itself right without a
// person, so that Ready can stay True while Synced is False with it (docs/adr/0030). It is the case when the Git host
// did not answer, FetchFailed and PushFailed, and for a Treebind that syncs in both directions also NonFastForward and
// Blocked: the next pull brings the commits that the push lacked, and the next scan commits the local edit that was in
// the way of the update, which the next pull then resolves. A Treebind that only pulls has no scan, so a local edit in
// the way waits for a person there. Every other reason, SyncFailed among them, waits for a person: a file, a branch,
// the volume, a limit of the spec or the repository's rules have to change before the next try can pass.
func WaitsForRepository(reason string, direction treebindv1alpha1.SyncDirection) bool {
	switch reason {
	case treebindv1alpha1.ReasonFetchFailed, treebindv1alpha1.ReasonPushFailed:
		return true
	case treebindv1alpha1.ReasonNonFastForward, treebindv1alpha1.ReasonBlocked:
		return direction == treebindv1alpha1.SyncDirectionBidirectional
	}
	return false
}

// syncStatus is what one poll says of the sync: the Synced and Conflict conditions and the agreement that
// status.lastSyncedCommit and status.lastSyncTime record. A condition that is nil is left as the status has it, and an
// empty lastSyncedCommit leaves both fields as they are: they are never cleared.
type syncStatus struct {
	synced, conflict *metav1.Condition
	lastSyncedCommit string
	// lastSyncTime is in whole seconds, as the API server keeps it.
	lastSyncTime *metav1.Time
}

// syncStatusOf returns what the poll of the agent of treebind says of the sync. report is the agent's report as the
// client accepted it, nil when the poll did not get one; unreachable is the status of the AgentUnreachable condition
// that the same poll makes; templateHash is the hash of the pod template that the current spec gives, the value of the
// variable that desiredPodTemplate gives the agent.
//
//   - While AgentUnreachable is True, both conditions are Unknown with the reason AgentUnreachable: what they said is
//     no longer known to hold (docs/adr/0015). While it is Unknown, or when there is no report, nothing changes: the
//     poll says nothing of the sync.
//   - A report of another template than the spec gives, or of none, as from an agent of the release before, says
//     nothing of the sync of this spec: both conditions are Unknown with the reason AgentOutdated, and the agreement it
//     reports is not taken either, because it can be of the repository or the branch of the spec before.
//   - Otherwise the report is of the current spec, and the agreement it reports is taken when its commit is not the one
//     of the status. A report with no agreement, as after a restart of the agent, and one at the commit of the status,
//     as from a pull that found nothing new, change neither field. Synced is False when a step failed, True with InSync
//     when none did and the report has an agreement, and Unknown with SyncPending when it has none yet. Conflict is
//     Unknown whenever Synced is, with its reason, in both directions. Otherwise it is False with NoConflict for a
//     Treebind that only pulls, and what the Conflict check says for one that syncs in both directions: True when a
//     conflict could not be resolved, False with ConflictResolved when the last conflict since the agent started was
//     resolved, and False with NoConflict when none was, or when the agent has not come to the checkout.
//
// Every condition it makes carries the generation of the Treebind: the ones made from the report because the report is
// of the current template, and the Unknown ones because they say what the operator knows of the current spec. A
// condition that it leaves keeps the generation its last write gave it. The commit of the report is a full object name,
// which the client checks before the report gets here.
func syncStatusOf(
	treebind *treebindv1alpha1.Treebind, report *status.Report, unreachable metav1.ConditionStatus, templateHash string,
) syncStatus {
	generation := treebind.Generation
	switch {
	case unreachable == metav1.ConditionTrue:
		return unknownSyncStatus(treebindv1alpha1.ReasonAgentUnreachable, agentUnreachableMessage, generation)
	case unreachable == metav1.ConditionUnknown || report == nil:
		return syncStatus{}
	case report.TemplateHash == "" || report.TemplateHash != templateHash:
		// An empty hash of the spec, which no caller passes, matches no report rather than the reports that have none.
		return unknownSyncStatus(treebindv1alpha1.ReasonAgentOutdated, agentOutdatedMessage, generation)
	}

	var result syncStatus
	agreedCommit := treebind.Status.LastSyncedCommit
	if agreed := report.LastSynced; agreed.Commit != "" && agreed.Commit != agreedCommit {
		result.lastSyncedCommit, result.lastSyncTime = agreed.Commit, wholeSeconds(agreed.Time)
		agreedCommit = agreed.Commit
	}
	direction := treebind.Spec.Sync.Direction
	synced := syncedConditionOf(report, direction, agreedCommit, generation)
	conflict := conflictConditionOf(report, direction, synced, generation)
	result.synced, result.conflict = &synced, &conflict
	return result
}

// apply writes what the poll says into the status and reports whether that changed anything, so that the poller
// writes the status only when it did. A condition is set as meta.SetStatusCondition sets it, which moves its
// lastTransitionTime only when its status changes, and the two fields are written only for another commit.
func (s syncStatus) apply(st *treebindv1alpha1.TreebindStatus) bool {
	changed := false
	for _, condition := range []*metav1.Condition{s.synced, s.conflict} {
		if condition != nil {
			changed = meta.SetStatusCondition(&st.Conditions, *condition) || changed
		}
	}
	if s.lastSyncedCommit != "" && s.lastSyncedCommit != st.LastSyncedCommit {
		st.LastSyncedCommit = s.lastSyncedCommit
		st.LastSyncTime = s.lastSyncTime.DeepCopy()
		changed = true
	}
	return changed
}

// unknownSyncStatus is both conditions Unknown with the reason, and no agreement.
func unknownSyncStatus(reason, message string, generation int64) syncStatus {
	synced := metav1.Condition{
		Type: treebindv1alpha1.ConditionSynced, Status: metav1.ConditionUnknown,
		Reason: reason, Message: message, ObservedGeneration: generation,
	}
	conflict := synced
	conflict.Type = treebindv1alpha1.ConditionConflict
	return syncStatus{synced: &synced, conflict: &conflict}
}

// wholeSeconds is the time as the status keeps it. metav1.Time is written in whole seconds, so a time with a fraction
// is not equal to itself once it has been through the API server, and writing it as it is would leave the status that
// the poller holds unlike the one that is stored.
func wholeSeconds(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	kept := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &kept
}

// syncedConditionOf is the Synced condition for a report of the current template. agreedCommit is the commit of
// status.lastSyncedCommit once this poll has written it.
func syncedConditionOf(
	report *status.Report, direction treebindv1alpha1.SyncDirection, agreedCommit string, generation int64,
) metav1.Condition {
	condition := metav1.Condition{Type: treebindv1alpha1.ConditionSynced, ObservedGeneration: generation}
	if check, reason, failed := failedStep(report.Checks, direction); failed {
		condition.Status, condition.Reason = metav1.ConditionFalse, reason
		condition.Message = failureMessage(stepPhrase(check.Name), reason, direction) + ". " +
			agreementSentence(agreedCommit) + ". " + readMoreSentence
		return condition
	}
	if report.LastSynced.Commit != "" {
		condition.Status, condition.Reason = metav1.ConditionTrue, treebindv1alpha1.ReasonInSync
		condition.Message = "No step of the sync failed. " + agreementSentence(agreedCommit)
		return condition
	}
	condition.Status, condition.Reason, condition.Message =
		metav1.ConditionUnknown, treebindv1alpha1.ReasonSyncPending, syncPendingMessage
	return condition
}

// failedStep returns the failed check that Synced takes its reason from, with the reason as Synced names it, and false
// when no check failed. It is the first that failed in the order of the report, which is the order of the steps, except
// that a reason that waits for a person comes before one that waits for the repository: Ready acts on the reason of
// Synced, and a Git host that does not answer must not hide a file that a person has to move. The Conflict check is a
// step like the others, so a conflict that could not be resolved keeps its reason while a later pull fails for the
// host.
func failedStep(checks []status.Check, direction treebindv1alpha1.SyncDirection) (status.Check, string, bool) {
	var first status.Check
	var firstReason string
	for _, check := range checks {
		if check.Outcome != string(preflight.Failed) {
			continue
		}
		reason := knownReason(check.Reason)
		if !WaitsForRepository(reason, direction) {
			return check, reason, true
		}
		if firstReason == "" {
			first, firstReason = check, reason
		}
	}
	return first, firstReason, firstReason != ""
}

// knownReason is the reason of a failed step as the conditions name it: the agent's when this release defines it, and
// SyncFailed otherwise, so that the operator writes no reason it did not define.
func knownReason(reason string) string {
	if _, known := failedStepReasons[reason]; known {
		return reason
	}
	return treebindv1alpha1.ReasonSyncFailed
}

// stepPhrase begins the message of Synced False for the check.
func stepPhrase(check string) string {
	if phrase, known := stepPhrases[check]; known {
		return phrase
	}
	return unknownStepPhrase
}

// failureMessage is the phrase of the step, the sentence of the reason and, for a reason that waits for the repository,
// that the agent tries again by itself.
func failureMessage(phrase, reason string, direction treebindv1alpha1.SyncDirection) string {
	sentence, known := failedStepReasons[reason]
	if !known {
		sentence = syncFailedSentence
	}
	message := phrase + ": " + sentence
	if WaitsForRepository(reason, direction) {
		message += ". " + retriesSentence
	}
	return message
}

// agreementSentence says at which commit the volume and the repository last agreed, as the status records it.
func agreementSentence(commit string) string {
	if commit == "" {
		return "The operator has not seen the volume and the repository agree yet"
	}
	if len(commit) > shortCommitLength {
		commit = commit[:shortCommitLength]
	}
	return "The volume and the repository last agreed at commit " + commit
}

// conflictConditionOf is the Conflict condition for a report of the current template, with the Synced condition that
// the same report makes.
func conflictConditionOf(
	report *status.Report, direction treebindv1alpha1.SyncDirection, synced metav1.Condition, generation int64,
) metav1.Condition {
	condition := metav1.Condition{Type: treebindv1alpha1.ConditionConflict, ObservedGeneration: generation}
	set := func(value metav1.ConditionStatus, reason, message string) metav1.Condition {
		condition.Status, condition.Reason, condition.Message = value, reason, message
		return condition
	}
	if synced.Status == metav1.ConditionUnknown {
		return set(metav1.ConditionUnknown, synced.Reason, synced.Message)
	}
	if direction != treebindv1alpha1.SyncDirectionBidirectional {
		return set(metav1.ConditionFalse, treebindv1alpha1.ReasonNoConflict, pullOnlyMessage)
	}

	var check *status.Check
	for i := range report.Checks {
		if report.Checks[i].Name == checkConflict {
			check = &report.Checks[i]
			break
		}
	}
	switch {
	case check == nil:
		// The checks of the volume failed, and the agent has not come to the checkout since it started.
	case check.Outcome == string(preflight.Failed):
		reason := knownReason(check.Reason)
		return set(metav1.ConditionTrue, reason, failureMessage(unresolvedConflictPhrase, reason, direction)+". "+
			readMoreSentence)
	case check.Outcome == string(preflight.Passed) && check.Reason == treebindv1alpha1.ReasonConflictResolved:
		return set(metav1.ConditionFalse, treebindv1alpha1.ReasonConflictResolved, conflictResolvedMessage)
	}
	// Skipped, which the check is until a conflict is resolved, or absent.
	return set(metav1.ConditionFalse, treebindv1alpha1.ReasonNoConflict, noConflictMessage)
}
