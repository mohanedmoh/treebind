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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/heartbeat"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	tipBefore = "1111111111111111111111111111111111111111"
	tipAfter  = "2222222222222222222222222222222222222222"
)

// sequence is what happened, in order, across the fakes of a test.
type sequence struct {
	mu     sync.Mutex
	events []string
}

func (s *sequence) add(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *sequence) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// fakePusher answers each push as the test says: a push of the commits that wait, until it is given another
// answer, and says on seq that it was called.
type fakePusher struct {
	calls  atomic.Int32
	seq    *sequence
	answer func(n int) (push.Result, error)
}

func (f *fakePusher) Push(context.Context) (push.Result, error) {
	n := int(f.calls.Add(1))
	if f.seq != nil {
		f.seq.add("push")
	}
	if f.answer != nil {
		return f.answer(n)
	}
	return pushed(1), nil
}

func pushed(commits int) push.Result {
	return push.Result{Outcome: push.Pushed, Branch: testBranch, From: tipBefore, To: tipAfter, Commits: commits}
}

func nothingToPush() push.Result {
	return push.Result{Outcome: push.NothingToPush, Branch: testBranch, From: tipAfter, To: tipAfter}
}

func pushFailure(reason, message string) error {
	return &push.Error{Reason: reason, Message: message}
}

// failingPuller is a puller that never passes, so that nothing but a push can make the agent ready.
type failingPuller struct{}

func (failingPuller) Pull(context.Context) (pull.Result, error) {
	return pull.Result{}, &pull.Error{Reason: pull.ReasonFetchFailed, Message: "no route to host"}
}

// pushWorld is a world that has a pusher too.
type pushWorld struct {
	*world
	pusher *fakePusher
	seq    *sequence
	opts   chan push.Options
	// pushErr, when it is set, is why the pusher cannot be made.
	pushErr error
}

func newPushWorld() *pushWorld {
	w := newWorld()
	seq := &sequence{}
	w.committer.answer = func(_ int, changes []scan.Change) (commit.Result, error) {
		seq.add("commit")
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: len(changes)}, nil
	}
	return &pushWorld{world: w, pusher: &fakePusher{seq: seq}, seq: seq, opts: make(chan push.Options, 10)}
}

// with is the options of the world and a pusher, and then what the test changes.
func (w *pushWorld) with(more ...func(*Options)) func(*Options) {
	return w.world.with(append([]func(*Options){func(o *Options) {
		o.Config.SyncInterval = 30 * time.Second
		o.NewPusher = func(options push.Options) (Pusher, error) {
			w.opts <- options
			if w.pushErr != nil {
				return nil, w.pushErr
			}
			return w.pusher, nil
		}
	}}, more...)...)
}

// firstScan lets the first scan end, which is when the push is tried for the first time, and returns once the
// second has begun.
func (w *pushWorld) firstScan(t *testing.T) {
	t.Helper()
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})
}

// settleNext takes a file through the three scans that make it settle, from a scan that has begun already.
func (w *pushWorld) settleNext(t *testing.T, change scan.Change) {
	t.Helper()
	for range 3 {
		w.next(t, found(change))
	}
}

func pushCheck(t *testing.T, r *running) (status.Check, bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckPush {
			return c, true
		}
	}
	return status.Check{}, false
}

func TestPushesAfterTheCommitInTheSameScanAndSaysSoInTheReport(t *testing.T) {
	w := newPushWorld()
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	// The first scan finds nothing waiting; the push after the commit is the second call.
	if got := w.seq.get(); len(got) < 2 || got[len(got)-2] != "commit" || got[len(got)-1] != "push" {
		t.Errorf("sequence = %v, want the push right after the commit, in the same turn", got)
	}
	got, ok := pushCheck(t, r)
	if !ok || got.Outcome != outcomePassed || got.Reason != "" {
		t.Fatalf("push = %+v, want it passed", got)
	}
	if want := "pushed 1 commit to the branch " + testBranch + ": " + shortOf(tipBefore) + ".." + shortOf(tipAfter); got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	if names := checkNames(t, r); !strings.HasSuffix(names, ",Changes,Commit,Push,Conflict") {
		t.Errorf("checks = %s, want the push after the commit, and the conflict after the push", names)
	}
}

func TestTriesAtTheFirstScanAndAfterACommitAndNotAtEveryScan(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) { return nothingToPush(), nil }
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	// Four scans that find nothing: the push was tried at the first, to see whether commits wait that a run
	// before this one left, and is not tried again.
	for range 4 {
		w.next(t, scanAnswer{})
	}

	if n := w.pusher.calls.Load(); n != 1 {
		t.Errorf("the pusher was called %d times by scans that found nothing, want once at the first scan", n)
	}
	got, ok := pushCheck(t, r)
	if !ok || got.Outcome != outcomePassed || !strings.HasPrefix(got.Message, "nothing to push") {
		t.Errorf("push = %+v, want it passed, with nothing to push", got)
	}
}

func TestNothingIsPushedForAPullTreebindOrWhenNoPusherIsGiven(t *testing.T) {
	t.Run("the direction is pull", func(t *testing.T) {
		w := newPushWorld()
		r := start(t, w.with(func(o *Options) { o.Config.SyncDirection = config.DirectionPull }))
		eventually(t, "the pull to pass", func() bool {
			got, found := pullCheck(t, r)
			return found && got.Outcome == outcomePassed
		})
		if _, found := pushCheck(t, r); found || len(w.opts) != 0 || w.pusher.calls.Load() != 0 {
			t.Errorf("a pull Treebind has a Push check = %v, %d pushers, %d calls: it has none", found, len(w.opts), w.pusher.calls.Load())
		}
	})
	t.Run("no pusher is given", func(t *testing.T) {
		w := newPushWorld()
		r := start(t, w.with(func(o *Options) { o.NewPusher = nil }))
		w.scanner.awaitBegun(t)
		w.next(t, scanAnswer{})
		if _, found := pushCheck(t, r); found {
			t.Error("an agent with no pusher has a Push check")
		}
	})
}

func TestThePushIsNotTriedBeforeTheCheckoutIsMadeAndSaysSo(t *testing.T) {
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

	eventually(t, "the push row", func() bool {
		got, ok := pushCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "checkout is not made yet")
	})
}

func TestAPushThatFailsIsReportedWithItsReasonAndTheTryAfterItWaitsAsAFailedPullDoes(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "the branch main cannot be pushed: no route to host")
		}
		return pushed(1), nil
	}
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	// The first scan tries, and fails. The interval is 30s, a failed push waits between that and twice it, which is
	// 45s with the random of the tests, and the scans are 3s apart.
	w.next(t, scanAnswer{})
	got, _ := pushCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != push.ReasonPushFailed || !strings.Contains(got.Message, "no route to host") {
		t.Fatalf("push = %+v, want it failed, with PushFailed and why", got)
	}
	for range 9 { // 27s more
		w.next(t, scanAnswer{})
	}
	if n := w.pusher.calls.Load(); n != 1 {
		t.Fatalf("the pusher was called %d times within 30s of a failed push, want 1: the host does not answer, and asking again at every scan is a load", n)
	}
	for range 8 { // past the 45s
		w.next(t, scanAnswer{})
	}

	if n := w.pusher.calls.Load(); n != 2 {
		t.Errorf("the pusher was called %d times after the wait, want 2", n)
	}
	if got, _ := pushCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("push = %+v, want the try after the wait to have passed", got)
	}
}

func TestAPushThatAPersonPutsRightIsTriedAfterTheIntervalAndNotSooner(t *testing.T) {
	for _, reason := range []string{push.ReasonNonFastForward, push.ReasonPushRejected} {
		t.Run(reason, func(t *testing.T) {
			w := newPushWorld()
			w.pusher.answer = func(n int) (push.Result, error) {
				if n == 1 {
					return push.Result{}, pushFailure(reason, "the repository refused it")
				}
				return pushed(1), nil
			}
			r := start(t, w.with())
			w.scanner.awaitBegun(t)
			w.next(t, scanAnswer{})
			if got, _ := pushCheck(t, r); got.Outcome != outcomeFailed || got.Reason != reason {
				t.Fatalf("push = %+v, want it failed with %s", got, reason)
			}

			// The wait is the interval, 30s, and does not grow: the host answered, and what is wrong is for a person.
			for range 9 {
				w.next(t, scanAnswer{})
			}
			if n := w.pusher.calls.Load(); n != 1 {
				t.Fatalf("the pusher was called %d times within the interval, want 1", n)
			}
			for range 3 {
				w.next(t, scanAnswer{})
			}

			if n := w.pusher.calls.Load(); n != 2 {
				t.Errorf("the pusher was called %d times after the interval, want 2: the rejection waits the interval and not longer", n)
			}
		})
	}
}

func TestAFailureThatGrowsKeepsGrowingAcrossTriesAndResetsAfterASuccess(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n <= 2 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		}
		return pushed(1), nil
	}
	start(t, w.with(func(o *Options) { o.Config.SyncInterval = 3 * time.Second }))
	w.scanner.awaitBegun(t)

	// With an interval of 3s and the random of the tests the first wait is 4.5s and the second 7.5s (the ceiling
	// doubles and the draw is in the middle of the length between the interval and it).
	for range 40 {
		w.next(t, scanAnswer{})
		if w.pusher.calls.Load() >= 3 {
			break
		}
	}

	if n := w.pusher.calls.Load(); n != 3 {
		t.Errorf("the pusher was called %d times, want 3: two that failed and one that went through", n)
	}
	before := w.pusher.calls.Load()
	for range 10 {
		w.next(t, scanAnswer{})
	}
	if w.pusher.calls.Load() != before {
		t.Error("the pusher was called again after a push that worked and nothing was committed")
	}
}

func TestACommitDuringTheWaitAfterAFailedPushWaitsForItToo(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		}
		return pushed(1), nil
	}
	start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{}) // the first try fails

	// A file settles and is committed in the middle of the wait: the host is not asked before the wait is over.
	for range 3 {
		w.next(t, found(changed(fileOne, scan.Modified, 1)))
	}

	if n := w.pusher.calls.Load(); n != 1 {
		t.Errorf("the pusher was called %d times, want 1: a commit does not end the wait of a push that failed", n)
	}
}

func TestAPushThatCouldNotBeSetUpIsAFailedCheckThatStaysAndTheCommitsGoOn(t *testing.T) {
	w := newPushWorld()
	w.pushErr = errors.New("push: the repository has no address")
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := pushCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != push.ReasonPushFailed || !strings.Contains(got.Message, "the push cannot be set up") {
		t.Fatalf("push = %+v, want it failed, because it cannot be set up", got)
	}
	w.next(t, scanAnswer{})
	if got, _ := pushCheck(t, r); got.Outcome != outcomeFailed {
		t.Errorf("push = %+v, want the failure to stay: it is about the settings", got)
	}
	if got, _ := commitCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("commit = %+v, want the commits to go on", got)
	}
	if n := w.pusher.calls.Load(); n != 0 {
		t.Errorf("a pusher that was not made was called %d times", n)
	}
}

func TestAGitDirectoryThatIsNotTrustedKeepsItsOwnReasonInThePushCheck(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) {
		return push.Result{}, pushFailure(checkout.ReasonGitDirUnsafe, "the Git directory holds more than the agent put there")
	}
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, scanAnswer{})

	if got, _ := pushCheck(t, r); got.Outcome != outcomeFailed || got.Reason != checkout.ReasonGitDirUnsafe {
		t.Errorf("push = %+v, want it failed with %s", got, checkout.ReasonGitDirUnsafe)
	}
}

func TestSaysAWarningOfAPushThatWorkedAndNotMoreThanOnceForTheSameMessage(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) {
		result := pushed(2)
		result.Warning = "the tracking ref could not be moved"
		return result, nil
	}
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, scanAnswer{})

	got, _ := pushCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "pushed 2 commits") || !strings.HasSuffix(got.Message, "; the tracking ref could not be moved") {
		t.Errorf("push = %+v, want the push and the warning", got)
	}
}

func TestSaysTheFirstCommitOfAnEmptyRepositoryAsANewBranch(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) {
		return push.Result{Outcome: push.Pushed, Branch: testBranch, From: "", To: tipAfter, Commits: 1}, nil
	}
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, scanAnswer{})

	got, _ := pushCheck(t, r)
	if want := "pushed 1 commit to the new branch " + testBranch + ", which is now at " + shortOf(tipAfter); got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
}

func TestThePusherIsMadeWithTheRepositoryTheBranchOfTheCheckoutAndTheFenceOfTheAgent(t *testing.T) {
	w := newPushWorld()
	start(t, w.with())

	var options push.Options
	select {
	case options = <-w.opts:
	case <-time.After(10 * time.Second):
		t.Fatal("no pusher was made")
	}

	if options.URL != testURL || options.Branch != testBranch {
		t.Errorf("the pusher has %q and %q, want the repository of the spec and the branch of the checkout", options.URL, options.Branch)
	}
	if options.Layout.WorkTree != workTreeOfTheTests || options.Layout.GitDir != gitDirOfTheTests {
		t.Errorf("the layout is %q and %q, want the work tree and the Git directory of the checkout", options.Layout.WorkTree, options.Layout.GitDir)
	}
	if options.Git.SSH.IdentityFile != testKeyFile {
		t.Errorf("the Git options have the key %q, want the one of the spec: %q", options.Git.SSH.IdentityFile, testKeyFile)
	}
	if options.Fence == nil {
		t.Fatal("the pusher has no fence: it would push without the writer lock")
	}
	if err := options.Fence(context.Background()); err != nil {
		t.Errorf("the fence says %v for an agent that has no lock to lose", err)
	}
}

func TestTheFenceOfThePusherIsTheOneOfTheLockThatTheAgentHolds(t *testing.T) {
	root, lockDir := claimWithState(t)
	w := newPushWorld()
	r := start(t, w.with(func(o *Options) {
		takesTheLock(root, nil)(o)
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.LockClock = newFakeClock()
	}))
	options := <-w.opts
	if err := options.Fence(context.Background()); err != nil {
		t.Fatalf("the fence says %v while the agent holds the lock", err)
	}

	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	if err := options.Fence(context.Background()); !errors.Is(err, lock.ErrLost) {
		t.Errorf("the fence of the pusher says %v, want the loss of the lock that another agent took", err)
	}
	if stopped := r.waitStopped(t); !errors.Is(stopped, ErrLockLost) {
		t.Errorf("Run returned %v, want ErrLockLost: a push that finds the lock gone stops the agent", stopped)
	}
}

// A push is a Git command, and the Runner that makes it is made from the options the agent gives, so it tells the
// liveness probe how long it may take, as a pull and a commit do.
func TestAPushBeatsForTheAgentWithItsTimeoutThroughTheRunnerOptions(t *testing.T) {
	clock := newFakeClock()
	w := newPushWorld()
	r := start(t, w.with(func(o *Options) { o.HeartbeatClock = clock.Now }))
	w.scanner.awaitBegun(t) // the loop is parked in the first scan, so nothing but the test beats
	options := <-w.opts
	if options.Git.Running == nil {
		t.Fatal("the Runner options of the pusher have no hook for the agent to know a command by")
	}

	const pushTime = 40 * time.Minute
	done := options.Git.Running(pushTime)
	wantAliveUntil(t, r, clock, pushTime, "a push of 40 minutes")
	done()
	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("healthz is %d once the push has ended, want 200: the code that follows has a promise of its own", code)
	}
	wantAliveUntil(t, r, clock, heartbeat.Work, "the code after a push")
}

func TestTheFirstPushThatWorksMakesTheAgentReadyAndNothingElseOfAPushDoes(t *testing.T) {
	// The pulls never pass in these, so that the readiness is the push's.
	failingPulls := func(o *Options) {
		o.NewPuller = func(pull.Options) (Puller, error) { return failingPuller{}, nil }
	}

	t.Run("a push that worked", func(t *testing.T) {
		w := newPushWorld()
		w.pusher.answer = func(int) (push.Result, error) { return pushed(1), nil }
		r := start(t, w.with(failingPulls))
		w.firstScan(t)

		eventually(t, "the push to be in the report", func() bool {
			got, ok := pushCheck(t, r)
			return ok && got.Outcome == outcomePassed
		})

		if code := r.readyz(t); code != http.StatusOK {
			t.Errorf("readyz is %d after a push that worked and with no pull that passed, want 200", code)
		}
		if got, _ := pullCheck(t, r); got.Outcome == outcomePassed {
			t.Errorf("pull = %+v, want no pull to have passed: the readiness is the push's", got)
		}
		if n := r.logs.count("the first push worked, and the agent reports itself ready"); n != 1 {
			t.Errorf("the readiness was logged %d times, want once:\n%s", n, r.logs.String())
		}
	})

	for name, answer := range map[string]func(int) (push.Result, error){
		"nothing to push": func(int) (push.Result, error) { return nothingToPush(), nil },
		"a rejection of the host": func(int) (push.Result, error) {
			return push.Result{}, pushFailure(push.ReasonPushRejected, "protected branch")
		},
		"the repository moved": func(int) (push.Result, error) {
			return push.Result{}, pushFailure(push.ReasonNonFastForward, "not a fast-forward")
		},
		"a repository that is away": func(int) (push.Result, error) {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		},
	} {
		t.Run("not ready after "+name, func(t *testing.T) {
			w := newPushWorld()
			w.pusher.answer = answer
			r := start(t, w.with(failingPulls))
			w.firstScan(t)

			eventually(t, "the push to be in the report", func() bool {
				got, ok := pushCheck(t, r)
				return ok && got.Outcome != outcomeSkipped
			})

			if code := r.readyz(t); code != http.StatusServiceUnavailable {
				t.Errorf("readyz is %d after %s, want 503: only a push that worked makes the agent ready", code, name)
			}
		})
	}

	t.Run("it stays ready after a later failure", func(t *testing.T) {
		w := newPushWorld()
		w.pusher.answer = func(n int) (push.Result, error) {
			if n == 1 {
				return pushed(1), nil
			}
			return push.Result{}, pushFailure(push.ReasonPushRejected, "protected branch")
		}
		r := start(t, w.with(failingPulls))
		w.firstScan(t)
		eventually(t, "the first push", func() bool {
			got, ok := pushCheck(t, r)
			return ok && got.Outcome == outcomePassed
		})
		if code := r.readyz(t); code != http.StatusOK {
			t.Fatalf("readyz is %d after the first push, want 200", code)
		}

		// A file settles, is committed, and the push of it is rejected.
		w.settleNext(t, changed(fileOne, scan.Modified, 1))
		eventually(t, "the rejection", func() bool {
			got, _ := pushCheck(t, r)
			return got.Outcome == outcomeFailed
		})

		if code := r.readyz(t); code != http.StatusOK {
			t.Errorf("readyz is %d after a push that failed, want 200: the volume is as it was", code)
		}
	})

	t.Run("a pull Treebind is as it was", func(t *testing.T) {
		w := newPushWorld()
		r := start(t, w.with(failingPulls, func(o *Options) { o.Config.SyncDirection = config.DirectionPull }))
		eventually(t, "the pull to fail", func() bool {
			got, found := pullCheck(t, r)
			return found && got.Outcome == outcomeFailed
		})
		if code := r.readyz(t); code != http.StatusServiceUnavailable {
			t.Errorf("readyz is %d for a pull Treebind whose pulls fail, want 503", code)
		}
	})
}

// The report holds the push before the probe says that the agent is ready, as it holds the pull: the agent reads
// its clock for the time of the report just before it stores it, so the probe is asked there, and has to say that
// the agent is not ready.
func TestTheReportHoldsTheFirstPushBeforeTheProbeSaysReady(t *testing.T) {
	var (
		probing atomic.Bool
		url     atomic.Pointer[string]
		mu      sync.Mutex
		codes   []int
	)
	w := newPushWorld()
	w.pusher.answer = func(int) (push.Result, error) {
		// The push is the one that is held: from here the agent reads its clock for the report.
		probing.Store(true)
		return pushed(1), nil
	}
	r := start(t, w.with(func(o *Options) {
		o.NewPuller = func(pull.Options) (Puller, error) { return failingPuller{}, nil }
		o.Now = func() time.Time {
			if base := url.Load(); base != nil && probing.Load() {
				response, err := http.Get(*base + status.ReadyPath)
				code := -1
				if err == nil {
					code = response.StatusCode
					_ = response.Body.Close()
				}
				mu.Lock()
				codes = append(codes, code)
				mu.Unlock()
			}
			return time.Now()
		}
	}))
	base := r.url
	url.Store(&base)
	w.firstScan(t)

	eventually(t, "the push to be in the report", func() bool {
		got, ok := pushCheck(t, r)
		return ok && got.Outcome == outcomePassed
	})
	probing.Store(false)

	mu.Lock()
	defer mu.Unlock()
	if len(codes) == 0 {
		t.Fatal("the report was not stored while the test was asking: nothing was held")
	}
	for _, code := range codes {
		if code != http.StatusServiceUnavailable {
			t.Errorf("readyz was %d while the report of the push was being made, want 503: the probe went first", code)
		}
	}
	if code := r.readyz(t); code != http.StatusOK {
		t.Errorf("readyz is %d after the push, want 200", code)
	}
}

func TestAPushBeforeACommitInTheSameTurnIsNotPossible(t *testing.T) {
	// The order of one turn: a settled file is committed, and what was committed is then pushed, so a push is
	// never of less than what the turn committed.
	w := newPushWorld()
	start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	events := w.seq.get()
	last := -1
	for i, event := range events {
		if event == "commit" {
			last = i
		}
	}
	if last < 0 || last+1 >= len(events) || events[last+1] != "push" {
		t.Errorf("sequence = %v, want a push straight after the commit", events)
	}
}

func TestARefusalAfterFailuresThatGrewWaitsTheIntervalAndNotTheGrownWait(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
		}
		return push.Result{}, pushFailure(push.ReasonNonFastForward, "the repository moved")
	}
	start(t, w.with())
	w.scanner.awaitBegun(t)

	// The first try fails at 0s and waits 45s (the interval of the test is 30s and the failure grows it). The second,
	// at 45s, is a refusal, and waits the interval, 30s: the third is at 75s. A wait that kept the growth would be 45s.
	for range 26 { // the scans at 0s to 75s
		w.next(t, scanAnswer{})
	}

	if n := w.pusher.calls.Load(); n != 3 {
		t.Errorf("the pusher was called %d times by 75s, want 3: a refusal waits the interval, whatever failed before it", n)
	}
}

func TestASuccessEndsTheGrowthOfTheWaitSoThatTheNextFailureWaitsFromTheStart(t *testing.T) {
	w := newPushWorld()
	w.pusher.answer = func(n int) (push.Result, error) {
		if n == 3 {
			return pushed(1), nil
		}
		return push.Result{}, pushFailure(push.ReasonPushFailed, "no route to host")
	}
	start(t, w.with(func(o *Options) { o.Config.SyncInterval = 6 * time.Second }))
	w.scanner.awaitBegun(t)

	// With an interval of 6s and the random of the tests the waits are 9s after one failure, 15s after two and 27s
	// after three. Tries at 0s and 9s fail, the one at 24s works.
	for range 9 { // the scans at 0s to 24s
		w.next(t, scanAnswer{})
	}
	if n := w.pusher.calls.Load(); n != 3 {
		t.Fatalf("the pusher was called %d times by 24s, want 3", n)
	}
	// A file settles at 33s, is committed, and the push of it fails: its wait is the first, 9s, and not the fourth.
	for range 3 { // the scans at 27s, 30s and 33s
		w.next(t, found(changed(fileOne, scan.Modified, 1)))
	}
	if n := w.pusher.calls.Load(); n != 4 {
		t.Fatalf("the pusher was called %d times when the file was committed, want 4", n)
	}
	for range 3 { // the scans at 36s, 39s and 42s
		w.next(t, scanAnswer{})
	}

	if n := w.pusher.calls.Load(); n != 5 {
		t.Errorf("the pusher was called %d times by 42s, want 5: the wait after the success started from the first, 9s", n)
	}
}

func TestSaysThePushIsNotTriedWhileTheFirstScanRuns(t *testing.T) {
	w := newPushWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t) // the first scan has begun and is not answered

	got, ok := pushCheck(t, r)
	if !ok || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "the first scan is running") {
		t.Errorf("push = %+v, want it skipped, because the first scan is running", got)
	}
}

func TestSaysThePushIsNotTriedWhenTheScanCannotBeSetUp(t *testing.T) {
	w := newPushWorld()
	r := start(t, w.with(func(o *Options) {
		o.NewScanner = func(scan.Options) (Scanner, error) { return nil, errors.New("the scanner cannot be made") }
	}))

	eventually(t, "the push row", func() bool {
		got, ok := pushCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "the scan cannot be set up")
	})
	if n := len(w.opts); n != 0 {
		t.Errorf("%d pushers were made for scans that cannot be set up", n)
	}
}

func TestSaysThePushIsNotTriedWhileAVolumeCheckFails(t *testing.T) {
	w := newPushWorld()
	r := start(t, w.with(func(o *Options) {
		o.Preflight = func(context.Context, string, string) []preflight.Result { return failed }
	}))

	eventually(t, "the push row", func() bool {
		got, ok := pushCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "a volume check failed")
	})
}
