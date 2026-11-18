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
	"slices"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/settle"
	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

// CheckChanges is the name of the check that says what the last scan for local changes found. It is in the
// report after the pull, for a Treebind that syncs in both directions.
const CheckChanges = "Changes"

// CheckCommit is the name of the check that says what came of the last commit of the files that the scans found
// settled. It is in the report after the changes, for a Treebind that syncs in both directions and commits.
const CheckCommit = "Commit"

// Committer commits the changes that have settled. The Committer of package commit is one.
type Committer interface {
	Commit(ctx context.Context, settled []scan.Change) (commit.Result, error)
}

// Scanner scans the synced directory for local changes once. The Scanner of package scan is one.
type Scanner interface {
	Scan(ctx context.Context) (scan.Result, error)
}

// Watcher tells the agent that the synced directory may have changed. The Watcher of package watch is one.
type Watcher interface {
	// Events delivers a signal when something may have changed, and is closed when the watcher is.
	Events() <-chan struct{}
	// Refresh watches the directories that have no watch.
	Refresh()
	// Stats says how much is watched.
	Stats() watch.Stats
	// Close stops the watcher.
	Close() error
}

// scansFollow says whether the pulls are followed by scans for local changes: the Treebind syncs in both
// directions, and there is something to scan with. A Treebind that only pulls never looks at what the
// application writes.
func (a *agent) scansFollow() bool {
	return a.pullsFollow() && a.NewScanner != nil && a.Config.SyncDirection == config.DirectionBidirectional
}

// commitsFollow says whether the scans are followed by commits: there are scans, and something to commit with.
func (a *agent) commitsFollow() bool { return a.scansFollow() && a.NewCommitter != nil }

// scans is the state of the scans of one run of the loop (docs/adr/0029).
type scans struct {
	a       *agent
	p       *progress
	scanner Scanner
	// committer is what commits the settled changes. It is nil when it could not be made, which the Commit
	// check says, and when the agent has none.
	committer Committer
	gitDir    string
	// loggedCommit is the last result of the Commit check that was logged, so that a commit that fails in the same
	// way at every scan is one line of the log.
	loggedCommit []string
	// watcher is nil when there are no events: none was given, or it could not be made, and watchErr says why.
	watcher  Watcher
	watchErr error
	tracker  *settle.Tracker
	// interval is how often a scan is made at the latest, for the message, and how often the directories
	// are walked for the watches that they lack.
	interval time.Duration
	// refreshedAt is when they were last walked, and is zero before the first scan.
	refreshedAt time.Time
	logged      []string
}

// startScans makes what scans, and puts the first state of the Changes check in the report. It returns nil,
// after saying so in the check, when the scanner cannot be made: the pulls go on without it.
func (a *agent) startScans(p *progress, done checkedOut) *scans {
	p.changes = notTried(CheckChanges, "running: the result of the first scan is here when it ends")
	s := &scans{a: a, p: p, tracker: settle.New(a.Config.SettleTime), interval: a.ScanInterval}
	if s.interval <= 0 {
		s.interval = syncloop.DefaultScanInterval
	}
	l := done.options.Layout
	filter, err := paths.NewFilter(a.Config.PathsInclude, a.Config.PathsExclude, l.NeverSynced(), paths.Options{})
	if err == nil {
		s.scanner, err = a.NewScanner(scan.Options{Layout: l, Git: done.options.Git, Filter: filter})
	}
	if err != nil {
		if a.commitsFollow() {
			p.commit = notTried(CheckCommit, "not tried, because the scan cannot be set up")
		}
		s.report(preflight.Result{
			Check: CheckChanges, Outcome: preflight.Failed, Reason: scan.ReasonScanFailed,
			Message: shortened(fmt.Sprintf("the scan cannot be set up: %v", err)),
		})
		return nil
	}
	s.gitDir = l.GitDir
	if a.commitsFollow() {
		p.commit = notTried(CheckCommit, "not tried, because no file has settled yet")
		committer, err := a.NewCommitter(commit.Options{
			Layout: l, Git: done.options.Git, Fence: a.fence,
			AuthorName: a.Config.CommitAuthorName, AuthorEmail: a.Config.CommitAuthorEmail,
			MessageTemplate: a.Config.CommitMessageTemplate,
		})
		if err != nil {
			// A wrong author or template is a failed check, and the scans and the pulls go on: the files wait for a
			// pod that has the settings put right.
			s.reportCommit(preflight.Result{
				Check: CheckCommit, Outcome: preflight.Failed, Reason: commit.ReasonCommitFailed,
				Message: shortened(fmt.Sprintf("the commit cannot be set up: %v; the files that settle are not committed until "+
					"the settings are put right", err)),
			})
		} else {
			s.committer = committer
		}
	}
	if a.NewWatcher != nil {
		made, watchErr := a.NewWatcher(watch.Options{Root: l.WorkTree, Skip: filter.SkipsDir})
		if watchErr != nil {
			s.watchErr = watchErr
		} else {
			s.watcher = made
		}
	}
	a.publish(p)
	return s
}

// wake is where the events of the file system come from, or nil.
func (s *scans) wake() <-chan struct{} {
	if s.watcher == nil {
		return nil
	}
	return s.watcher.Events()
}

// close stops the watcher.
func (s *scans) close() {
	if s.watcher != nil {
		_ = s.watcher.Close()
	}
}

// once makes one scan at the time now, puts what came of it in the report, and says how long until a change
// that it found settles, which is when the loop is to scan again if that is soonest. It reports nothing of a
// scan that was cut short by a stop.
func (s *scans) once(ctx context.Context, now time.Time) time.Duration {
	if s.watcher != nil && (s.refreshedAt.IsZero() || now.Sub(s.refreshedAt) >= s.interval) {
		// What a rename of a directory can do to a watch is put right once in a scan interval, which is as late
		// as a missed event is found. A scan that an event called for does not walk every directory: that
		// would be done as often as the files change.
		s.watcher.Refresh()
		s.refreshedAt = now
	}
	result, err := s.scanner.Scan(ctx)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		reason := scan.ReasonScanFailed
		if failure, ok := errors.AsType[*scan.Error](err); ok {
			reason = failure.Reason
		}
		// A failed check is where a person looks, and it says that there are no events as well.
		s.report(preflight.Result{Check: CheckChanges, Outcome: preflight.Failed, Reason: reason, Message: shortened(err.Error() + s.watchNote())})
		return 0
	}
	seen := s.tracker.Observe(now, result.Changes)
	s.report(preflight.Result{Check: CheckChanges, Outcome: preflight.Passed, Message: shortened(s.message(result, seen))})
	if len(seen.Settled) > 0 {
		s.commitSettled(ctx, seen.Settled)
	} else if len(result.Changes) == 0 {
		s.forgetFailedCommit()
	}
	return seen.Next
}

// forgetFailedCommit takes a commit that failed out of the report once the scans find no change at all: the files
// that could not be committed were put back or are gone, and the failure says nothing about what is there. A
// commit that passed stays, because it says what was committed last. A committer that could not be made stays
// failed too, because that is about the settings and not about the files.
func (s *scans) forgetFailedCommit() {
	if s.committer == nil || s.p.commit == nil || s.p.commit.Outcome != preflight.Failed {
		return
	}
	s.p.commit = notTried(CheckCommit, "not tried, because no file has settled yet")
	s.a.publish(s.p)
	// The same failure that comes back is a new line of the log.
	s.loggedCommit = nil
}

// commitSettled commits the files that have settled, and puts what came of it in the Commit check. A commit
// that fails is tried again at the next scan, because the files are still there. It reports nothing of a commit
// that was cut short by a stop.
func (s *scans) commitSettled(ctx context.Context, settled []scan.Change) {
	if s.committer == nil {
		return
	}
	// A Git command that was stopped in the middle of a write leaves a lock file that fails every command after
	// it. The writer lock is held and no command runs, so what is there is stale (docs/adr/0028).
	cleared, err := clearLocks(ctx, s.gitDir, s.a.fence)
	var result commit.Result
	if err == nil {
		result, err = s.committer.Commit(ctx, settled)
	}
	if ctx.Err() != nil {
		return
	}
	s.reportCommit(commitResult(result, err, cleared))
}

// reportCommit puts the result of a commit in the report and in the log, which has a line when what it says
// changes and not at every scan that fails in the same way.
func (s *scans) reportCommit(result preflight.Result) {
	s.p.commit = &result
	s.a.publish(s.p)
	s.loggedCommit = s.a.logChanges("commit", []preflight.Result{result}, s.loggedCommit)
}

// commitResult turns what a commit did, or why it could not, into the result of the check, with the lock files
// that were removed before it.
func commitResult(result commit.Result, err error, cleared []string) preflight.Result {
	checked := preflight.Result{Check: CheckCommit, Outcome: preflight.Passed}
	if err != nil {
		reason := commit.ReasonCommitFailed
		if failure, ok := errors.AsType[*commit.Error](err); ok {
			reason = failure.Reason
		}
		checked = preflight.Result{Check: CheckCommit, Outcome: preflight.Failed, Reason: reason, Message: shortened(err.Error())}
	} else {
		switch result.Outcome {
		case commit.Committed:
			checked.Message = fmt.Sprintf("committed %s as %s", plural(result.Files, "file", "files"), shortOf(result.Commit))
		default:
			checked.Message = "the files that had settled came to nothing to commit: they were changed back, or are gone"
		}
		if result.Gone > 0 {
			if result.Gone == 1 {
				checked.Message += "; 1 file that had settled was gone before it could be committed"
			} else {
				checked.Message += fmt.Sprintf("; %d files that had settled were gone before they could be committed", result.Gone)
			}
		}
	}
	if note := clearedNote(cleared); note != "" {
		checked.Message += "; " + note
	}
	return checked
}

// report puts the result of a scan in the report and in the log, which has a line when what it says changes
// and not at every scan.
func (s *scans) report(result preflight.Result) {
	s.p.changes = &result
	s.a.publish(s.p)
	s.logged = s.a.logChanges("scan", []preflight.Result{result}, s.logged)
}

// message says what a scan found, without any time in it, so that a scan that finds the same again is not a
// new line of the log.
func (s *scans) message(result scan.Result, seen settle.Observation) string {
	var text string
	if len(result.Changes) == 0 {
		text = "no local changes"
	} else {
		text = fmt.Sprintf("%s changed on the volume: %d settled, %d waiting for their quiet period",
			plural(len(result.Changes), "file", "files"), len(seen.Settled), len(seen.Waiting))
	}
	if len(result.Left) > 0 {
		reasons := make([]string, 0, len(result.Left))
		for reason := range result.Left {
			reasons = append(reasons, string(reason))
		}
		slices.Sort(reasons)
		for i, reason := range reasons {
			reasons[i] = fmt.Sprintf("%s %d", reason, result.Left[paths.Reason(reason)])
		}
		text += "; left out: " + strings.Join(reasons, ", ")
	}
	return text + s.watchNote()
}

// watchNote says what the events cannot do, when there is something they cannot. The count of the
// directories that are watched is not in it, because it changes with every directory.
func (s *scans) watchNote() string {
	if s.watcher == nil {
		note := "; no events of the file system"
		if s.watchErr != nil {
			note += fmt.Sprintf(" (%v)", s.watchErr)
		}
		return note + fmt.Sprintf(", so a scan every %s finds the changes", s.interval)
	}
	var note string
	stats := s.watcher.Stats()
	if stats.Unwatched > 0 {
		note += fmt.Sprintf("; %s cannot be watched (%s), and the scan finds the changes in them", plural(stats.Unwatched, "directory", "directories"), stats.Reason)
	}
	if stats.Lost > 0 {
		note += "; events were lost, and a scan followed"
	}
	return note
}

// plural is a count and a noun, in the plural when the count is not one.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
