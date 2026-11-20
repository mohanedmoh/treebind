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
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commitmsg"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// A Treebind that syncs in both directions resolves a conflict by its policy in the turn of the pull, says so in the
// Pull check and in a check of its own, and pushes what the resolution made at the next scan (docs/adr/0034).

const (
	resolvedLocalRef = "refs/treebind/local/00000003"
	resolvedCommit   = "3333333333333333333333333333333333333333"
	resolvedWarning  = "the old local ref refs/treebind/local/00000001 cannot be removed: busy"
	// notResolved is what the Conflict check says until a resolution: it says nothing of whether a conflict exists, which
	// the check cannot know while every resolution fails.
	notResolved = "no conflict has been resolved since the agent started"
	botName     = commitmsg.DefaultAuthorName
	syncAuthor  = "Grafana Sync"
	syncEmail   = "sync@example.com"
)

func conflictCheck(t *testing.T, r *running) (status.Check, bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckConflict {
			return c, true
		}
	}
	return status.Check{}, false
}

// resolution is what a pull that resolved a conflict by followRemote says.
func resolution(policy pull.Policy) pull.Result {
	res := &pull.Resolution{Policy: policy, LocalCommits: 2, RemoteCommits: 3, Overridden: 4, Carried: 5, Commit: resolvedCommit,
		Dropped: []string{"x/y.json"}}
	if policy == pull.FollowRemote {
		res.Local = resolvedLocalRef
	}
	return pull.Result{Outcome: pull.Resolved, Branch: testBranch, From: tipBefore, To: resolvedCommit, Resolution: res,
		Warning: resolvedWarning}
}

// noCommitResolution is a resolution by followRemote that made no commit, because the application had changed nothing that could
// be put on the tip: the checkout was moved to the tip, and a file was left out.
func noCommitResolution() pull.Result {
	result := resolution(pull.FollowRemote)
	result.Resolution.Commit, result.Resolution.Carried = "", 0
	result.To = tipAfter
	return result
}

// scriptedPuller answers the pull with the number n as the test says.
type scriptedPuller struct {
	calls  atomic.Int32
	answer func(n int) (pull.Result, error)
}

func (s *scriptedPuller) Pull(context.Context) (pull.Result, error) {
	return s.answer(int(s.calls.Add(1)))
}

func TestThePullerIsGivenThePolicyAndTheAuthorOfTheCommitsOfABidirectionalTreebind(t *testing.T) {
	for name, tt := range map[string]struct {
		change func(*Options)
		policy pull.Policy
		author git.Identity
	}{
		"nothing set, which is followRemote and the author of the agent": {
			func(*Options) {}, pull.FollowRemote, git.Identity{Name: botName, Email: commitmsg.DefaultAuthorEmail},
		},
		"followLocal and an author of the Treebind": {
			func(o *Options) {
				o.Config.ConflictPolicy = config.ConflictFollowLocal
				o.Config.CommitAuthorName, o.Config.CommitAuthorEmail = syncAuthor, syncEmail
			},
			pull.FollowLocal, git.Identity{Name: syncAuthor, Email: syncEmail},
		},
		"an author that Git would not keep as it is written": {
			func(o *Options) { o.Config.CommitAuthorName, o.Config.CommitAuthorEmail = "<bad>", syncEmail },
			pull.FollowRemote, git.Identity{},
		},
		"an author with a name and no email": {
			func(o *Options) { o.Config.CommitAuthorName = syncAuthor },
			pull.FollowRemote, git.Identity{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			asked := make(chan pull.Options, 4)
			start(t, w.with(tt.change, func(o *Options) {
				o.NewPuller = func(options pull.Options) (Puller, error) {
					asked <- options
					return autoPuller{}, nil
				}
			}))
			w.scanner.awaitBegun(t)
			var options pull.Options
			// The first pull is after the first scans, which wait for the test.
			for made := false; !made; {
				select {
				case options = <-asked:
					made = true
				default:
					w.next(t, scanAnswer{})
				}
			}

			if options.Policy != tt.policy || options.Git.Identity != tt.author {
				t.Errorf("the puller was made with the policy %q and the author %+v, want %q and %+v",
					options.Policy, options.Git.Identity, tt.policy, tt.author)
			}
		})
	}
}

func TestATreebindThatOnlyPullsHasNoPolicyAndNoConflictCheck(t *testing.T) {
	w := newWorld()
	asked := make(chan pull.Options, 4)
	r := start(t, w.with(func(o *Options) {
		o.Config.SyncDirection = config.DirectionPull
		o.Config.ConflictPolicy = config.ConflictFollowLocal
		o.NewPuller = func(options pull.Options) (Puller, error) {
			asked <- options
			return autoPuller{}, nil
		}
	}))

	options := <-asked

	if options.Policy != "" || options.Git.Identity != (git.Identity{}) {
		t.Errorf("a pull Treebind's puller has the policy %q and the author %+v, want none: it has no commits to resolve with", options.Policy, options.Git.Identity)
	}
	eventually(t, "the pull to pass", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomePassed
	})
	if _, found := conflictCheck(t, r); found {
		t.Error("a pull Treebind has a Conflict check")
	}
}

func TestTheConflictCheckComesAfterThePushAndIsNotTriedUntilAConflictIsFound(t *testing.T) {
	w := newPushWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})

	got, ok := conflictCheck(t, r)

	if !ok || got.Outcome != outcomeSkipped || got.Reason != "NotChecked" || got.Message != notResolved {
		t.Fatalf("conflict = %+v, want it skipped, not checked, and saying %q", got, notResolved)
	}
	if names := checkNames(t, r); names != "Mount,Writable,Checkout,Pull,Changes,Commit,Push,Conflict" {
		t.Errorf("checks = %s, want the conflict after the push", names)
	}
}

func TestAResolutionIsInThePullCheckAndInTheConflictCheckAndStaysThereAfterThePullsThatFollow(t *testing.T) {
	for _, policy := range []pull.Policy{pull.FollowRemote, pull.FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newPushWorld()
			puller := &scriptedPuller{answer: func(n int) (pull.Result, error) {
				if n == 2 {
					return resolution(policy), nil
				}
				return pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: tipBefore, To: tipBefore}, nil
			}}
			r := start(t, w.with(func(o *Options) {
				o.Config.ConflictPolicy = string(policy)
				o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil }
			}))
			w.scanner.awaitBegun(t)
			for puller.calls.Load() < 2 {
				w.next(t, scanAnswer{})
			}

			eventually(t, "the Conflict check to say what was resolved", func() bool {
				got, _ := conflictCheck(t, r)
				return got.Reason == pull.ReasonConflictResolved
			})
			conflict, _ := conflictCheck(t, r)
			wantInMessage := []string{string(policy), "the checkout had 2 commits and the branch main of the repository had 3 commits that the checkout did not",
				"4 file(s)", "5 file(s)", shortOf(resolvedCommit)}
			if policy == pull.FollowRemote {
				wantInMessage = append(wantInMessage, resolvedLocalRef, "repository's version", "are only in the commits of the checkout: x/y.json")
			} else {
				wantInMessage = append(wantInMessage, "volume's version", "are only in the commits of the repository: x/y.json")
			}
			if conflict.Outcome != outcomePassed {
				t.Errorf("conflict = %+v, want it passed", conflict)
			}
			for _, want := range wantInMessage {
				if !strings.Contains(conflict.Message, want) {
					t.Errorf("the Conflict message = %q, want it to say %q", conflict.Message, want)
				}
			}
			if !strings.Contains(conflict.Message, "1 file(s) could not be put on the winner's version") || !strings.Contains(conflict.Message, "x/y.json") {
				t.Errorf("the Conflict message = %q, want it to name the file that was left out", conflict.Message)
			}
			if policy == pull.FollowLocal && strings.Contains(conflict.Message, "refs/treebind") {
				t.Errorf("the Conflict message = %q, want no local ref for followLocal: the commits are in the history", conflict.Message)
			}
			// The pull passed, and says that it resolved, so that a person who looks at it is told to look at the other.
			pulled, _ := pullCheck(t, r)
			if !strings.Contains(pulled.Message, resolvedWarning) {
				t.Errorf("pull = %+v, want it to carry what the resolution could not do", pulled)
			}
			if pulled.Outcome != outcomePassed || !strings.Contains(pulled.Message, "resolved a conflict") || !strings.Contains(pulled.Message, "Conflict check") {
				t.Errorf("pull = %+v, want it passed, saying that it resolved a conflict and pointing at the Conflict check", pulled)
			}

			// A pull that finds nothing says so, and the Conflict check still says what the last resolution did.
			for puller.calls.Load() < 3 {
				w.next(t, scanAnswer{})
			}
			w.next(t, scanAnswer{})
			if again, _ := conflictCheck(t, r); again.Reason != pull.ReasonConflictResolved || again.Message != conflict.Message {
				t.Errorf("conflict = %+v after the next pull, want it as it was", again)
			}
			// One line of the log for the resolution, and none for the pulls that follow it.
			if n := r.logs.count("check=Conflict"); n != 1 {
				t.Errorf("the log has %d lines for the Conflict check, want 1:\n%s", n, r.logs.String())
			}
		})
	}
}

func TestAResolutionMakesTheNextScanPushWhatItMadeAndANoneIsNotPushedAtEveryScan(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) { return nothingToPush(), nil }
	puller := &scriptedPuller{answer: func(n int) (pull.Result, error) {
		if n == 2 {
			return resolution(pull.FollowRemote), nil
		}
		return pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: tipBefore, To: tipBefore}, nil
	}}
	start(t, w.with(func(o *Options) { o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil } }))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 2 {
		w.next(t, scanAnswer{})
	}
	before := w.pusher.calls.Load()
	if before != 1 {
		t.Fatalf("the pusher was called %d times before the resolution, want once, at the first scan", before)
	}

	// The scan that follows the resolution pushes, with nothing found by the scan and nothing committed.
	w.next(t, scanAnswer{})
	w.next(t, scanAnswer{})

	if n := w.pusher.calls.Load(); n != 2 {
		t.Errorf("the pusher was called %d times after the resolution, want 2: once for the commit that it made, and no more", n)
	}
}

func TestAResolutionPushesAtTheNextScanEvenWhenAFailedPushIsStillWaiting(t *testing.T) {
	// The first push failed, and waits 45s to be tried again. The resolution is at 27s, and what it made is pushed at the
	// scan after it, 30s, and not at the 45s that the wait ends.
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		}
		return pushed(1), nil
	}
	puller := &scriptedPuller{answer: func(int) (pull.Result, error) { return resolution(pull.FollowLocal), nil }}
	r := start(t, w.with(func(o *Options) { o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil } }))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 1 {
		w.next(t, scanAnswer{})
	}
	if n := w.pusher.calls.Load(); n != 1 {
		t.Fatalf("the pusher was called %d times before the resolution, want once", n)
	}

	w.next(t, scanAnswer{})
	w.next(t, scanAnswer{})

	if n := w.pusher.calls.Load(); n != 2 {
		t.Fatalf("the pusher was called %d times, want 2: the failed push is tried again after the resolution, and not at the end of its wait", n)
	}
	if got, _ := pushCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("push = %+v, want it passed", got)
	}
	if seconds := w.clock.seconds(); seconds >= 45*time.Second {
		t.Errorf("the clock is at %v, want the push to have been before the 45s of the wait", seconds)
	}
}

func TestAResolutionThatFailsIsAFailedPullAndAFailedConflictCheckWithTheSameReason(t *testing.T) {
	w := newPushWorld()
	puller := &scriptedPuller{answer: func(int) (pull.Result, error) {
		return pull.Result{}, &pull.Error{Reason: pull.ReasonBlocked, Conflict: true,
			Message: "the conflict between the checkout and the repository was not resolved: " +
				"the checkout was not updated, because 1 file(s) have a local edit"}
	}}
	r := start(t, w.with(func(o *Options) { o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil } }))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 1 {
		w.next(t, scanAnswer{})
	}

	eventually(t, "the Conflict check to fail", func() bool {
		got, _ := conflictCheck(t, r)
		return got.Outcome == outcomeFailed
	})

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonBlocked || !strings.Contains(got.Message, "was not resolved") {
		t.Errorf("pull = %+v, want it failed with Blocked, saying that the conflict was not resolved", got)
	}
	conflict, _ := conflictCheck(t, r)
	if conflict.Reason != pull.ReasonBlocked || !strings.Contains(conflict.Message, "was not resolved") ||
		!strings.Contains(conflict.Message, "1 file(s) have a local edit") {
		t.Errorf("conflict = %+v, want it failed with the reason and the words of the update", conflict)
	}
}

func TestAResolvedPullMakesTheAgentReady(t *testing.T) {
	w := newPushWorld()
	puller := &scriptedPuller{answer: func(int) (pull.Result, error) { return resolution(pull.FollowRemote), nil }}
	r := start(t, w.with(func(o *Options) {
		o.Config.SyncInterval = time.Hour
		o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil }
	}))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 1 {
		w.next(t, scanAnswer{})
	}

	eventually(t, "the agent to be ready", func() bool { return r.readyz(t) == http.StatusOK })
}

func TestTheConflictCheckIsNotTriedBeforeTheCheckoutIsMadeAndSaysSo(t *testing.T) {
	w := newPushWorld()
	started := make(chan struct{})
	r := start(t, w.with(func(o *Options) {
		o.Checkout = func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
			close(started)
			<-ctx.Done()
			return checkout.Result{}, ctx.Err()
		}
	}))
	<-started

	eventually(t, "the conflict row", func() bool {
		got, ok := conflictCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "checkout is not made yet")
	})
}

func TestTheConflictCheckIsInTheReportWhileTheLockCannotBeTaken(t *testing.T) {
	// The report that says the lock failed is the one of the run, and has every row that the run has: a row that came and
	// went would make a person think it was something else that changed.
	w := newPushWorld()
	r := start(t, w.with(func(o *Options) {
		o.AcquireLock = func(context.Context, lock.Config) (*lock.Lock, error) { return nil, errors.New("no") }
	}))
	eventually(t, "the failed lock in the report", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	got, ok := conflictCheck(t, r)

	if !ok || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "checkout is not made yet") {
		t.Errorf("conflict = %+v, %v, want it skipped, with the checkout not made yet", got, ok)
	}
}

func TestAResolutionThatMadeNoCommitSaysSoAndStillNamesWhatWasLeftOut(t *testing.T) {
	w := newPushWorld()
	puller := &scriptedPuller{answer: func(int) (pull.Result, error) { return noCommitResolution(), nil }}
	r := start(t, w.with(func(o *Options) { o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil } }))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 1 {
		w.next(t, scanAnswer{})
	}

	eventually(t, "the Conflict check to say what was resolved", func() bool {
		got, _ := conflictCheck(t, r)
		return got.Reason == pull.ReasonConflictResolved
	})

	got, _ := conflictCheck(t, r)
	for _, want := range []string{"so no commit was made", "0 file(s) that only the application changed were kept", "are only in the commits of the checkout: x/y.json"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("the Conflict message = %q, want it to say %q", got.Message, want)
		}
	}
	if strings.Contains(got.Message, "were kept in the commit") {
		t.Errorf("the Conflict message = %q, want it not to say that a commit holds the files: there is none", got.Message)
	}
}

func TestAResolutionThatMadeNoCommitLeavesTheWaitOfAFailedPushAsItWas(t *testing.T) {
	// The first push failed, and waits 45s. Nothing new waits for a push, so the Git host is not asked before the wait is
	// over, as it would be for a resolution that made a commit (R-19).
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		}
		return pushed(1), nil
	}
	puller := &scriptedPuller{answer: func(int) (pull.Result, error) { return noCommitResolution(), nil }}
	start(t, w.with(func(o *Options) { o.NewPuller = func(pull.Options) (Puller, error) { return puller, nil } }))
	w.scanner.awaitBegun(t)
	for puller.calls.Load() < 1 {
		w.next(t, scanAnswer{})
	}

	w.next(t, scanAnswer{})
	w.next(t, scanAnswer{})
	if n := w.pusher.calls.Load(); n != 1 {
		t.Fatalf("the pusher was called %d times after a resolution that made no commit, want 1: the wait of the failed push is not over", n)
	}
	for w.pusher.calls.Load() < 2 {
		w.next(t, scanAnswer{})
	}

	if seconds := w.clock.seconds(); seconds < 45*time.Second {
		t.Errorf("the second push was at %v, want it no sooner than the 45s of the wait", seconds)
	}
}

// bidirectional starts an agent that syncs in both directions, with the pulls that the test answers one at a time. It
// has no scans, which a resolution does not need to be reported.
func bidirectional(t *testing.T, direction string) (*running, *fakePuller) {
	t.Helper()
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Config.SyncDirection = direction
	})
	f.awaitBegun(t)
	return r, f
}

// failedResolution is a pull that found a conflict and could not resolve it, for the reason of the update that it tried.
func failedResolution(reason, message string) answer {
	return answer{err: &pull.Error{Reason: reason, Message: message, Conflict: true}}
}

func TestTheConflictCheckIsFailedUntilAPullPassesWithNoConflictAndThenGoesBackToTheLastResolution(t *testing.T) {
	r, f := bidirectional(t, config.DirectionBidirectional)
	const (
		blocked  = "the conflict between the checkout and the branch main of the repository was not resolved: 2 file(s) have a local edit"
		held     = "the conflict between the checkout and the branch main of the repository was not resolved: 20 commits in the last hour"
		notFound = "the checkout was not updated, because a file that the update changes has a local edit"
	)
	want := func(when, outcome, reason, message string) {
		t.Helper()
		got, ok := conflictCheck(t, r)
		if !ok || got.Outcome != outcome || got.Reason != reason || got.Message != message {
			t.Errorf("%s: conflict = %+v, want %s, %q and %q", when, got, outcome, reason, message)
		}
	}
	want("before the first pull", outcomeSkipped, "NotChecked", notResolved)

	f.answer(t, upToDate(testCommit))
	want("after a pull that found nothing, and no conflict was ever found", outcomeSkipped, "NotChecked", notResolved)

	// The same reason as the next pull, with no conflict: what is Blocked is a file in the way of an update, and the
	// Conflict check is about a resolution that could not be made.
	f.answer(t, failedPull(pull.ReasonBlocked, notFound))
	want("after an update that is no conflict was blocked", outcomeSkipped, "NotChecked", notResolved)

	f.answer(t, failedResolution(pull.ReasonBlocked, blocked))
	want("after a conflict that was blocked", outcomeFailed, pull.ReasonBlocked, blocked)
	if got, _ := pullCheck(t, r); got.Outcome != outcomeFailed || got.Reason != pull.ReasonBlocked {
		t.Errorf("pull = %+v, want it failed with Blocked as well", got)
	}

	// A pull that fails for another cause says nothing of the conflict.
	f.answer(t, failedPull(pull.ReasonFetchFailed, noRoute))
	want("after a pull that failed for another cause", outcomeFailed, pull.ReasonBlocked, blocked)

	f.answer(t, failedResolution(pull.ReasonTooManyCommits, held))
	want("after a conflict that the breaker held", outcomeFailed, pull.ReasonTooManyCommits, held)

	f.answer(t, upToDate(testCommit))
	want("after a pull that found no conflict, and none was ever resolved", outcomeSkipped, "NotChecked", notResolved)

	f.answer(t, answer{result: resolution(pull.FollowRemote)})
	resolved, _ := conflictCheck(t, r)
	if resolved.Outcome != outcomePassed || resolved.Reason != pull.ReasonConflictResolved {
		t.Fatalf("conflict = %+v after a resolution, want it passed, with ConflictResolved", resolved)
	}

	f.answer(t, failedResolution(pull.ReasonBlocked, blocked))
	want("after a conflict that was blocked, with one resolved before", outcomeFailed, pull.ReasonBlocked, blocked)
	if got := r.readyz(t); got != http.StatusOK {
		t.Errorf("readyz = %d, want ready: a conflict that cannot be resolved is not a reason for the agent to be not ready", got)
	}

	f.answer(t, advanced(testCommit, otherCommit))
	want("after a pull that found no conflict, with one resolved before", outcomePassed, pull.ReasonConflictResolved, resolved.Message)

	f.answer(t, answer{result: resolution(pull.FollowLocal)})
	if got, _ := conflictCheck(t, r); got.Outcome != outcomePassed || got.Reason != pull.ReasonConflictResolved ||
		!strings.Contains(got.Message, string(pull.FollowLocal)) {
		t.Errorf("conflict = %+v after a resolution that follows the failure, want the resolution of followLocal", got)
	}
	// The log has the resolutions, and the pull that went back to the first of them does not say again that it was made.
	if n := r.logs.count("check=Conflict"); n != 2 {
		t.Errorf("the log has %d lines for the Conflict check, want 2, one for each resolution:\n%s", n, r.logs.String())
	}
}

func TestATreebindThatOnlyPullsKeepsNoConflictCheckWhateverAPullFindsAndWhateverItSays(t *testing.T) {
	r, f := bidirectional(t, config.DirectionPull)

	for _, answered := range []answer{
		failedPull(pull.ReasonDiverged, "the checkout and the branch main of the repository have gone different ways"),
		// A pull that only pulls has no policy, so none of its failures is one of a resolution. The check does not exist
		// whatever the error says.
		failedResolution(pull.ReasonBlocked, "the conflict between the checkout and the repository was not resolved: a file"),
		upToDate(testCommit),
	} {
		f.answer(t, answered)
		if got, found := conflictCheck(t, r); found {
			t.Errorf("a Treebind that only pulls has the Conflict check %+v after %+v", got, answered)
		}
	}
	if got, _ := pullCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("pull = %+v, want it passed after the last pull", got)
	}
}
