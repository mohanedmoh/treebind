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
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// agreedByPull records a pull that left the checkout and the branch of the repository at the same commit, and leaves the
// record as it was for one that did not (pullAgreed).
func (a *agent) agreedByPull(p *progress, result pull.Result, err error) {
	a.agreed(p, status.ByPull, pullAgreed(result, err))
}

// agreedByPush records a push that left the branch of the repository at the commit of the checkout, and leaves the record as
// it was for one that did not (pushAgreed).
func (a *agent) agreedByPush(p *progress, result push.Result, err error) {
	a.agreed(p, status.ByPush, pushAgreed(result, err))
}

// agreed records that the step saw the checkout and the branch at the commit, which the report says as lastSynced from the
// next publish on. An empty commit is a step that did not see them agree, and changes nothing: a failure, or a step that left
// them apart, says nothing about the commit that they last agreed on, and the checks say what went wrong.
//
// A step that sees them agree at the commit that is recorded already changes nothing either, so that the report says when
// they came to agree at that commit and by which step, and not the last pull that found them still agreeing: the operator
// writes status.lastSyncTime from it when the commit changes (the time that the docs of the field promise), and a Treebind
// with nothing new reports the same thing at every pull. How fresh the report is, is what checkedAt says. The record is in
// the progress, which only the loop touches, and
// goes to the endpoint in the snapshot that publish stores with the checks: the endpoint reads it from other goroutines
// with no lock, as it reads the checks, and never sees a lastSynced that is newer or older than the checks beside it. The
// time is from Options.Now, the clock of checkedAt, so that a test can pin it, and in UTC as checkedAt is. Nothing of it is
// kept on the volume: a restart of the agent starts with none, until its first pull.
func (a *agent) agreed(p *progress, by, commit string) {
	if commit == "" || p.synced.Commit == commit {
		return
	}
	p.synced = status.LastSynced{Commit: commit, Time: a.Now().UTC(), By: by}
}

// pullAgreed is the commit at which a pull saw the checkout and the branch agree, or empty when it did not:
//
//   - UpToDate: the checkout is at the tip of the branch, which the pull asked the repository for.
//   - Advanced: the checkout was moved to that tip.
//   - Resolved with no commit made: the checkout was moved to that tip, and what the application had changed is kept in a ref
//     (followRemote), or both sides held the same files.
//   - Resolved with a commit made: the checkout is at a commit that the repository does not have until it is pushed, and the
//     push that takes it records. A Resolved that does not say how it resolved is taken as one that made a commit.
//   - Ahead: the checkout has commits that the repository does not have yet, which the push records once it takes them.
//   - A failure: the pull did not see the two agree, or saw them apart.
//
// The checkout of the first start is not one: a checkout that was opened again says nothing about the branch, and the first
// pull comes within an interval of it. For a directory of the repository the commits of the checkout are snapshots, and the
// commit is the repository's whose directory the snapshot is (ToSource), which the pull always says when it passes, and which
// is the commit that the message of the Pull check names (fullOf).
func pullAgreed(result pull.Result, err error) string {
	if err != nil {
		return ""
	}
	switch result.Outcome {
	case pull.UpToDate, pull.Advanced:
	case pull.Resolved:
		if result.Resolution == nil || result.Resolution.Commit != "" {
			return ""
		}
	default:
		return ""
	}
	return fullOf(result.ToSource, result.To)
}

// pushAgreed is the commit at which a push left the branch and the checkout agreeing, or empty when it did not. Only a push
// that pushed records: the repository took the commits, and its branch is at the commit of the checkout, or for a directory
// of the repository at the repository's commit that was made of it (ToSource), which the next pull compares the snapshot of.
// NothingToPush does not record, because the agent asked the repository nothing: the checkout has no commit that the tip it
// last fetched lacks, which says nothing of where the branch is now, and the next pull asks. A push that Git found the
// repository to have already comes to NothingToPush too, which is rare and is left to that pull as well.
func pushAgreed(result push.Result, err error) string {
	if err != nil || result.Outcome != push.Pushed {
		return ""
	}
	return fullOf(result.ToSource, result.To)
}
