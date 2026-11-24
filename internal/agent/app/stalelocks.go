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

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

// errFence is what a failure of the fence is wrapped in, so that the caller can tell a writer lock that
// could not be checked from a lock file that could not be removed.
var errFence = errors.New("the writer lock could not be checked")

// maxNamedLocks is how many lock files a message names.
const maxNamedLocks = 3

// clearLocks removes the lock files that a Git command that was stopped left in the Git directory, and
// returns their names (docs/adr/0028). The agent holds the writer lock, so no other process writes in the
// Git directory, and it runs one Git command at a time, so one that is there when nothing runs was left by
// a command that ended. It runs before the checkout and before each pull, when no command runs.
//
// The fence checks the writer lock first, as before any other write, and is not called when there is no lock
// file to remove, so that a Git directory with none costs a look and writes nothing. A lock file that the
// writer lock being lost would have let another agent's own be removed is not removed.
func clearLocks(ctx context.Context, gitDir string, fence func(context.Context) error) ([]string, error) {
	found, err := git.StaleLocks(gitDir)
	if err != nil {
		return nil, fmt.Errorf("the Git directory cannot be looked at for lock files that a Git command that was stopped left: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	if err := fence(ctx); err != nil {
		return nil, fmt.Errorf("%w, so the lock files that a Git command that was stopped left (%s) were not removed: %w",
			errFence, listed(found), err)
	}
	removed, err := git.RemoveStaleLocks(gitDir)
	if err != nil {
		return removed, fmt.Errorf("the lock files that a Git command that was stopped left cannot all be removed: %w", err)
	}
	return removed, nil
}

// clearFailed turns the error of clearLocks into the failure of a pull: Refused when the fence stopped it,
// which is what the fence of the pull itself says, and PullFailed when the volume would not let the files
// go.
func clearFailed(err error) *pull.Error {
	reason := pull.ReasonPullFailed
	if errors.Is(err, errFence) {
		reason = pull.ReasonRefused
	}
	return &pull.Error{Reason: reason, Message: err.Error(), Err: err}
}

// clearedNote says which lock files were removed, for the end of a message of the report, or is empty
// when none was.
func clearedNote(removed []string) string {
	switch len(removed) {
	case 0:
		return ""
	case 1:
		return "removed the lock file that a Git command that was stopped left: " + removed[0]
	}
	return fmt.Sprintf("removed %d lock files that a Git command that was stopped left: %s", len(removed), listed(removed))
}

// tempObjectsAge is how long a temporary object file has to have been left unchanged before an agent that does not hold the
// writer lock removes it: longer than the longest that a Git command of an agent may run, a fetch, and than the grace in which
// the Runner stops one, so that no command that still runs, of this agent or of another that holds the lock, can be the one that
// writes it. The age is read against the clock of the volume, which can be another machine's: one that is minutes off still
// leaves a margin, and a file that is removed too early fails the command that writes it and changes nothing that Git reads.
const tempObjectsAge = pull.DefaultFetchTimeout + time.Minute

// clearTempObjects removes the temporary object files that a Git command that failed left in the Git directory, and returns
// their names. A fetch that fails because the volume is full leaves what it had written, which keeps the volume full: the
// heartbeat of the writer lock cannot be written then, and the agent stops after a TTL. It runs after a pull that the volume
// refused, when no command runs.
//
// As for the lock files, the fence is called first, and only when there is something to remove, and nothing is removed when it
// says that the lock is lost: another agent may run Git in the directory then. A fence that the volume refused does not stop
// it. A full volume refuses the slot of the lock too, the lock stays this agent's until such refusals have lasted a TTL, and no
// other agent takes it over before that (docs/adr/0012). A temporary file that no command holds is nothing that Git reads, and
// removing it gives the heartbeat its room back.
func clearTempObjects(ctx context.Context, gitDir string, fence func(context.Context) error) ([]string, error) {
	found, err := git.StaleTempObjects(gitDir, 0)
	if err != nil {
		return nil, fmt.Errorf("the Git directory cannot be looked at for the temporary object files that a Git command that failed left: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	if err := fence(ctx); errors.Is(err, lock.ErrLost) {
		return nil, fmt.Errorf("the temporary object files that a Git command that failed left (%s) were not removed, because the writer lock was lost: %w",
			listed(found), err)
	}
	removed, err := git.RemoveStaleTempObjects(gitDir, 0)
	if err != nil {
		return removed, fmt.Errorf("the temporary object files that a Git command that failed left cannot all be removed: %w", err)
	}
	return removed, nil
}

// tempNote says which temporary object files were removed after a pull and what could not be done, for the end of a message of
// the report, or is empty when there is nothing to say.
func tempNote(removed []string, err error) string {
	var notes []string
	switch len(removed) {
	case 0:
	case 1:
		notes = append(notes, "removed the temporary object file that a Git command that failed left: "+removed[0])
	default:
		notes = append(notes, fmt.Sprintf("removed %d temporary object files that a Git command that failed left: %s", len(removed), listed(removed)))
	}
	if err != nil {
		notes = append(notes, err.Error())
	}
	return strings.Join(notes, "; ")
}

// removeOldTempObjects is what clearOldTempObjects removes with, and oldTempObjectsWait how long the volume checks wait for it:
// as long as they wait for the volume. A test replaces them, to hold a removal as a volume that does not answer does.
var (
	removeOldTempObjects = git.RemoveStaleTempObjects
	oldTempObjectsWait   = preflight.Timeout
)

// tidyStillRuns is what clearOldTempObjects returns as what it logged while the removal that an earlier round began has not
// ended.
const tidyStillRuns = "the removal of an earlier round has not ended"

// clearOldTempObjects removes, before the volume checks, the temporary object files that a Git command left in the Git
// directory when it was killed, or failed before the agent could remove what it left, and logs which. They can keep the volume
// full, so that the Writable check fails until a person removes them. The writer lock is not held yet, and another agent may
// run Git in the directory, so only a file older than tempObjectsAge is removed, and only from a state directory and a Git
// directory that are not links. A volume that does not answer is waited for as long as the checks wait for it, and is then
// theirs to say. The removal that it holds goes on, and no other begins until it has ended, so that a volume that does not
// answer for long does not gather one for each round of the checks. A failure, and a round that begins none, is logged when
// it differs from what was logged, which it returns: the checks that follow say what the volume refuses.
func (a *agent) clearOldTempObjects(ctx context.Context, logged string) string {
	where, err := layout.New(a.Config.MountPath, a.Config.SubPath)
	if err != nil {
		return logged
	}
	if !a.tidying.CompareAndSwap(false, true) {
		if logged != tidyStillRuns {
			a.Logger.Warn("the removal of old temporary object files that began before has not ended, as on a volume that does " +
				"not answer, so no other begins")
		}
		return tidyStillRuns
	}
	type outcome struct {
		removed []string
		err     error
	}
	done := make(chan outcome, 1)
	remove, wait := removeOldTempObjects, oldTempObjectsWait
	go func() {
		var got outcome
		if ownDirectories(where) {
			got.removed, got.err = remove(where.GitDir, tempObjectsAge)
		}
		// The next round may begin one as soon as this one is said to have ended.
		a.tidying.Store(false)
		done <- got
	}()
	var got outcome
	select {
	case got = <-done:
	case <-ctx.Done():
		return logged
	case <-time.After(wait):
		return logged
	}
	if len(got.removed) > 0 {
		a.Logger.Info("removed the temporary object files that a Git command that was stopped or failed left",
			"count", len(got.removed), "files", listed(got.removed))
	}
	if got.err == nil {
		return ""
	}
	if got.err.Error() != logged {
		a.Logger.Warn("the temporary object files that a Git command left cannot all be removed", "error", got.err)
	}
	return got.err.Error()
}

// ownDirectories reports whether the state directory and the Git directory of the layout are directories, and not links.
func ownDirectories(where layout.Layout) bool {
	for _, dir := range []string{where.StateDir, where.GitDir} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// listed names the first few of the files, and how many more there are.
func listed(names []string) string {
	shown := names[:min(len(names), maxNamedLocks)]
	text := strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}
