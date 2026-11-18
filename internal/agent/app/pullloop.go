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
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
)

// CheckPull is the name of the check that says how the last pull went. It is in the report after the
// checkout.
const CheckPull = "Pull"

// Puller pulls into the checkout once. The Puller of package pull is one.
type Puller interface {
	Pull(ctx context.Context) (pull.Result, error)
}

// pullLoop pulls into the checkout every interval until the agent stops, and says in the report how
// each pull went (docs/adr/0024). A pull that fails does not end it: the agent stays up, says why, and
// asks again, after a wait that grows while the repository or the volume keeps failing. The first pull
// that passes makes the agent ready, and nothing that follows makes it not ready (docs/adr/0030).
func (a *agent) pullLoop(ctx context.Context, p *progress, done checkedOut) {
	interval := a.Config.SyncInterval
	phase := syncloop.Phase(a.Config.Namespace, a.Config.Name, interval)
	options := pull.Options{
		Layout: done.options.Layout, URL: done.options.URL, Branch: done.made.Branch, Git: done.options.Git,
		// The lock is checked immediately before an update is written. The cap on deletions is the
		// Treebind's when it says, and the pull's own when it does not, and the recovery ref is the pull's
		// own (docs/adr/0026, docs/adr/0027).
		Fence: a.fence, MaxDeletions: a.Config.MaxDeletions,
	}
	l := &pulls{a: a, p: p, options: options}

	p.pulling = notTried(CheckPull, fmt.Sprintf("waiting: the first pull is due in %s", phase.Round(time.Second)))
	a.publish(p)
	loop := syncloop.Options{Interval: interval, Phase: phase, Clock: a.SyncClock, Random: a.SyncRandom, Pull: l.once, Beat: a.beat}
	if a.scansFollow() {
		// The scans are turns of the same loop, and never run beside a pull (docs/adr/0029).
		if s := a.startScans(p, done); s != nil {
			defer s.close()
			loop.Scan, loop.ScanInterval, loop.ScanGap, loop.Wake = s.once, a.ScanInterval, a.ScanGap, s.wake()
		}
	}
	err := syncloop.Run(ctx, loop)
	if err != nil {
		// Only an interval that is not positive, which the settings of the agent do not allow.
		result := preflight.Result{
			Check: CheckPull, Outcome: preflight.Failed, Reason: pull.ReasonPullFailed, Message: shortened(err.Error()),
		}
		p.pulling = &result
		a.publish(p)
		a.logChanges("pull", []preflight.Result{result}, nil)
	}
}

// pulls is the state of the pulls of one run of the loop.
type pulls struct {
	a       *agent
	p       *progress
	options pull.Options
	// puller is made at the first pull, and again at the next when it could not be made.
	puller Puller
	logged []string
}

// once runs one pull and puts what came of it in the report. It says whether the pull failed in the
// sense of syncloop: whether the wait before the next one should grow.
func (l *pulls) once(ctx context.Context) bool {
	// What a pull does between its Git commands, and before its first, is code with a promise of its own.
	l.a.working()
	if l.p.pulling.Outcome == preflight.Skipped {
		// No result yet, and a first pull can take as long as the repository does.
		l.p.pulling = notTried(CheckPull, "running: the result of the first pull is here when it ends")
		l.a.publish(l.p)
	}
	if l.puller == nil {
		made, err := l.a.NewPuller(l.options)
		if err != nil {
			return l.report(ctx, pull.Result{}, fmt.Errorf("the pull cannot be set up: %w", err), nil)
		}
		l.puller = made
	}
	// A Git command that was stopped in the middle of a write leaves a lock file that fails every command after
	// it. The writer lock is held and no command runs, so what is there is stale (docs/adr/0028).
	cleared, err := clearLocks(ctx, l.options.Layout.GitDir, l.a.fence)
	if err != nil {
		return l.report(ctx, pull.Result{}, clearFailed(err), cleared)
	}
	result, err := l.puller.Pull(ctx)
	return l.report(ctx, result, err, cleared)
}

// report publishes and logs the result of a pull, unless the agent was told to stop in the middle of it:
// a pull that was cut short says nothing about the repository. It says too which lock files were removed
// before the pull.
func (l *pulls) report(ctx context.Context, result pull.Result, err error, cleared []string) bool {
	if ctx.Err() != nil {
		return false
	}
	checked, failed := pullResult(result, err)
	if note := clearedNote(cleared); note != "" {
		checked.Message += "; " + note
	}
	l.p.pulling = &checked
	l.a.publish(l.p)
	l.logged = l.a.logChanges("pull", []preflight.Result{checked}, l.logged)
	// The report says that the pull passed before the probe says that the agent is ready.
	if checked.Outcome == preflight.Passed {
		l.a.becomeReady("the first pull passed, and the agent reports itself ready")
	}
	return failed
}

// pullResult turns what a pull did, or why it could not, into the result of the check, and says whether
// the wait before the next pull should grow.
func pullResult(result pull.Result, err error) (preflight.Result, bool) {
	if err != nil {
		reason, message := pull.ReasonPullFailed, err.Error()
		if failure, ok := errors.AsType[*pull.Error](err); ok {
			reason = failure.Reason
			if failure.Repaired != "" {
				message += "; " + failure.Repaired
			}
		}
		return preflight.Result{
			Check: CheckPull, Outcome: preflight.Failed, Reason: reason, Message: shortened(message),
		}, backsOff(reason)
	}
	var message string
	switch result.Outcome {
	case pull.Advanced:
		message = fmt.Sprintf("advanced the checkout on the branch %s from %s to %s", result.Branch, shortOf(result.From), shortOf(result.To))
		if result.Recovery != "" {
			message += "; the commit it was at is kept as " + result.Recovery
		}
		if result.Warning != "" {
			message += "; " + result.Warning
		}
	case pull.Ahead:
		message = fmt.Sprintf("the checkout is ahead of the branch %s (%s): it has commits that the repository does not have yet, "+
			"and there is nothing to pull", result.Branch, shortOf(result.To))
	default:
		message = fmt.Sprintf("the checkout is at the tip of the branch %s (%s)", result.Branch, shortOf(result.To))
	}
	if result.Repaired != "" {
		message += "; " + result.Repaired
	}
	return preflight.Result{Check: CheckPull, Outcome: preflight.Passed, Message: message}, false
}

// backsOff says whether a pull that failed for the reason is followed by a longer wait (docs/adr/0024). It
// is the case when the repository did not answer, or the agent could not use its volume: asking again
// sooner is of no use, and asking a host that fails again and again is what R-19 is about. It is not the
// case when the repository answered and what is wrong is a file, a commit, a branch, the Git directory or an
// update that removes too many files, which a person puts right: the next pull should find it at once when
// they have, and costs one question. Refused is only the fence now, a lock that could not be checked, which is
// a volume that does not answer (docs/adr/0026).
func backsOff(reason string) bool {
	switch reason {
	case pull.ReasonFetchFailed, pull.ReasonPullFailed, pull.ReasonRefused:
		return true
	}
	return false
}
