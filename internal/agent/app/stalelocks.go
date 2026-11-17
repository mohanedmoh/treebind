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
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/git"
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

// listed names the first few of the files, and how many more there are.
func listed(names []string) string {
	shown := names[:min(len(names), maxNamedLocks)]
	text := strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}
