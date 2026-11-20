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
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// The report says the commit at which the agent last saw the checkout and the branch of the repository agree, and when and by
// which step it first saw them agree there.

// The commits of a directory of the repository: the repository's, which the report names, and the snapshots of the checkout.
const (
	repoBefore = "4444444444444444444444444444444444444444"
	repoAfter  = "5555555555555555555555555555555555555555"
)

// pinnedClock is the clock of the report, which the test sets.
type pinnedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *pinnedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *pinnedClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// at is a time n seconds into the test, in a time zone that is not UTC, so that a report that does not turn it into UTC
// shows it.
func at(n int) time.Time {
	return time.Date(2026, 11, 20, 12, 0, n, 0, time.FixedZone("UTC+3", 3*60*60))
}

// lastSyncedOf is the lastSynced of the report as the operator reads it, in JSON, and empty when the report has none.
func lastSyncedOf(t *testing.T, r *running) string {
	t.Helper()
	code, body := r.get(t, status.Path, "Bearer "+goodToken)
	if code != http.StatusOK {
		t.Fatalf("the report was refused: %d %s", code, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("not a report: %v: %s", err, body)
	}
	return string(fields["lastSynced"])
}

// syncedJSON is a lastSynced in the JSON of the report.
func syncedJSON(commit, when, by string) string {
	return `{"commit":"` + commit + `","time":"` + when + `","by":"` + by + `"}`
}

func TestAPullSaysTheCheckoutAndTheBranchAgreedOnlyWhenItLeftTheCheckoutAtTheTip(t *testing.T) {
	resolvedWithNoCommit := noCommitResolution()
	resolvedWithNoCommitOfADirectory := noCommitResolution()
	resolvedWithNoCommitOfADirectory.FromSource, resolvedWithNoCommitOfADirectory.ToSource = repoBefore, repoAfter
	resolvedWithACommit := resolution(pull.FollowLocal)
	resolvedWithACommit.ToSource = repoAfter
	resolvedNotSayingHow := noCommitResolution()
	resolvedNotSayingHow.Resolution = nil

	for name, tt := range map[string]struct {
		result pull.Result
		err    error
		want   string
	}{
		"up to date": {
			pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: tipAfter, To: tipAfter}, nil, tipAfter,
		},
		"advanced, at the commit it was moved to": {
			pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: tipBefore, To: tipAfter}, nil, tipAfter,
		},
		"a directory up to date, at the repository's commit and not the snapshot": {
			pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: tipAfter, To: tipAfter, ToSource: repoAfter}, nil, repoAfter,
		},
		"a directory advanced, at the repository's commit and not the snapshot": {
			pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: tipBefore, To: tipAfter, FromSource: repoBefore, ToSource: repoAfter},
			nil, repoAfter,
		},
		"resolved by moving the checkout to the tip": {
			resolvedWithNoCommit, nil, tipAfter,
		},
		"a directory resolved by moving the checkout to the tip": {
			resolvedWithNoCommitOfADirectory, nil, repoAfter,
		},
		"resolved with a commit that the repository does not have yet": {
			resolvedWithACommit, nil, "",
		},
		"resolved, not saying how": {
			resolvedNotSayingHow, nil, "",
		},
		"ahead": {
			pull.Result{Outcome: pull.Ahead, Branch: testBranch, From: tipAfter, To: tipAfter}, nil, "",
		},
		"failed, whatever the result beside the error says": {
			pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: tipAfter, To: tipAfter},
			&pull.Error{Reason: pull.ReasonBlocked, Message: "1 file has a local edit"}, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pullAgreed(tt.result, tt.err); got != tt.want {
				t.Errorf("pullAgreed = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAPushSaysTheCheckoutAndTheBranchAgreedOnlyWhenItPushed(t *testing.T) {
	pushedADirectory := pushed(2)
	pushedADirectory.FromSource, pushedADirectory.ToSource = repoBefore, repoAfter

	for name, tt := range map[string]struct {
		result push.Result
		err    error
		want   string
	}{
		"pushed, at the commit that the branch is at now": {pushed(1), nil, tipAfter},
		"pushed a directory, at the repository's commit that was made of the checkout's": {
			pushedADirectory, nil, repoAfter,
		},
		"nothing to push, which asked the repository nothing": {nothingToPush(), nil, ""},
		"failed, whatever the result beside the error says": {
			pushed(1), pushFailure(push.ReasonNonFastForward, "the branch main has commits that the checkout does not"), "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pushAgreed(tt.result, tt.err); got != tt.want {
				t.Errorf("pushAgreed = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTheReportSaysTheLastPullThatLeftTheCheckoutAtTheTipAndNoFailureClearsIt(t *testing.T) {
	f := newFakePuller()
	clock := &pinnedClock{now: at(0)}
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Now = clock.Now
	})
	f.awaitBegun(t)

	if got := lastSyncedOf(t, r); got != "" {
		t.Errorf("before the first pull the report has lastSynced %s, want none", got)
	}

	clock.set(at(1))
	f.answer(t, advanced(testCommit, otherCommit))
	want := syncedJSON(otherCommit, "2026-11-20T09:00:01Z", status.ByPull)
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a pull that advanced lastSynced = %s, want %s", got, want)
	}

	// A pull that fails, and one that finds the checkout ahead, leave it as it was.
	clock.set(at(2))
	f.answer(t, failedPull(pull.ReasonBlocked, "the checkout was not updated, because 1 file has a local edit"))
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a pull that failed lastSynced = %s, want it as it was, %s", got, want)
	}
	clock.set(at(3))
	f.answer(t, answer{result: pull.Result{Outcome: pull.Ahead, Branch: testBranch, From: tipAfter, To: tipAfter}})
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a pull that found the checkout ahead lastSynced = %s, want it as it was, %s", got, want)
	}

	// A pull that finds the two agreeing at the same commit again changes nothing: the report says when they came to agree.
	clock.set(at(4))
	f.answer(t, upToDate(otherCommit))
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a pull that found the checkout at the tip again lastSynced = %s, want it as it was, %s", got, want)
	}

	// A pull that finds them agreeing at another commit is a new agreement, with its own time.
	clock.set(at(5))
	f.answer(t, advanced(otherCommit, testCommit))
	want = syncedJSON(testCommit, "2026-11-20T09:00:05Z", status.ByPull)
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a pull that advanced to another commit lastSynced = %s, want %s", got, want)
	}
}

func TestAStepThatSeesTheAgreementAgainAtTheSameCommitKeepsTheTimeAndTheStepOfTheFirst(t *testing.T) {
	clock := &pinnedClock{now: at(1)}
	a := &agent{Options: Options{Now: clock.Now}}
	p := &progress{}

	a.agreed(p, status.ByPush, repoBefore)
	clock.set(at(2))
	a.agreed(p, status.ByPull, repoBefore)
	if want := (status.LastSynced{Commit: repoBefore, Time: at(1).UTC(), By: status.ByPush}); p.synced != want {
		t.Errorf("after a pull at the commit that a push brought synced = %+v, want the push's, %+v", p.synced, want)
	}

	clock.set(at(3))
	a.agreed(p, status.ByPull, repoAfter)
	if want := (status.LastSynced{Commit: repoAfter, Time: at(3).UTC(), By: status.ByPull}); p.synced != want {
		t.Errorf("after a step at another commit synced = %+v, want %+v", p.synced, want)
	}

	// An empty commit is a step that did not see them agree, and the record stays.
	clock.set(at(4))
	a.agreed(p, status.ByPush, "")
	if want := (status.LastSynced{Commit: repoAfter, Time: at(3).UTC(), By: status.ByPull}); p.synced != want {
		t.Errorf("after a step that saw no agreement synced = %+v, want it as it was, %+v", p.synced, want)
	}
}

func TestTheReportSaysThePushThatPushedAndNotOneWithNothingToPushOrOneThatFailed(t *testing.T) {
	w := newPushWorld()
	clock := &pinnedClock{now: at(0)}
	w.pusher.answer = func(n int) (push.Result, error) {
		switch n {
		case 1:
			return nothingToPush(), nil
		case 2:
			return pushed(1), nil
		}
		return push.Result{}, pushFailure(push.ReasonNonFastForward, "the branch main of the repository has commits that the checkout does not")
	}
	// No pull passes, so that only a push can say that the two agreed.
	r := start(t, w.with(func(o *Options) {
		o.NewPuller = func(pull.Options) (Puller, error) { return failingPuller{}, nil }
		o.Now = clock.Now
	}))
	w.scanner.awaitBegun(t)

	// The first scan pushes, and there is nothing to push.
	clock.set(at(1))
	w.next(t, found(changed(fileOne, scan.Modified, 1)))
	if n := w.pusher.calls.Load(); n != 1 {
		t.Fatalf("the pusher was called %d times, want once, at the first scan", n)
	}
	if got := lastSyncedOf(t, r); got != "" {
		t.Errorf("after a push with nothing to push lastSynced = %s, want none", got)
	}

	// The file settles at the third scan, and its commit is pushed.
	clock.set(at(2))
	w.next(t, found(changed(fileOne, scan.Modified, 1)))
	w.next(t, found(changed(fileOne, scan.Modified, 1)))
	if n := w.pusher.calls.Load(); n != 2 {
		t.Fatalf("the pusher was called %d times, want twice: the second for the commit", n)
	}
	want := syncedJSON(tipAfter, "2026-11-20T09:00:02Z", status.ByPush)
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a push that pushed lastSynced = %s, want %s", got, want)
	}

	// The next commit cannot be pushed, and the report still says the push before it.
	clock.set(at(3))
	for range 3 {
		w.next(t, found(changed(fileTwo, scan.Added, 1)))
	}
	if n := w.pusher.calls.Load(); n != 3 {
		t.Fatalf("the pusher was called %d times, want 3: the third for the second commit", n)
	}
	if got, _ := pushCheck(t, r); got.Outcome != outcomeFailed {
		t.Fatalf("push = %+v, want it failed", got)
	}
	if got := lastSyncedOf(t, r); got != want {
		t.Errorf("after a push that failed lastSynced = %s, want it as it was, %s", got, want)
	}
}

// The endpoint reads the report from its own goroutines while the loop records, and each report pairs the checks with the
// lastSynced of the same moment. Run with -race.
func TestTheReportIsReadWhileTheLoopRecordsAndPairsTheChecksWithTheLastSyncOfTheSameMoment(t *testing.T) {
	a := &agent{Options: Options{Now: time.Now}}
	p := &progress{}
	// A pull that finds the checkout at the commit with the number i, as the loop reports it.
	pulled := func(i int) {
		commit := fmt.Sprintf("%040x", i)
		p.pulling = &preflight.Result{Check: CheckPull, Outcome: preflight.Passed, Message: commit}
		a.agreedByPull(p, pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: commit, To: commit}, nil)
		a.publish(p)
	}
	pulled(0)
	stop := make(chan struct{})
	var loop sync.WaitGroup
	loop.Go(func() {
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
				pulled(i)
			}
		}
	})

	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 500 {
				report := a.report()
				if len(report.Checks) != 1 {
					t.Errorf("the report has the checks %+v, want the pull", report.Checks)
					return
				}
				if report.LastSynced.Commit != report.Checks[0].Message || report.LastSynced.By != status.ByPull {
					t.Errorf("the report pairs the pull of %s with lastSynced %+v", report.Checks[0].Message, report.LastSynced)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	loop.Wait()
}
