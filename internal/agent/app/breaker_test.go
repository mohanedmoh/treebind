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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

// The commits that the agent makes are bounded by one breaker, which the commit step and the resolutions of the pulls share
// (docs/adr/0035): they count in the same file, against the same limit.

// pullOptionsOf makes the agent of a test take the options of its puller, and returns where they are.
func pullOptionsOf() (func(*Options), <-chan pull.Options) {
	options := make(chan pull.Options, 10)
	return func(o *Options) {
		o.NewPuller = func(asked pull.Options) (Puller, error) {
			options <- asked
			return autoPuller{}, nil
		}
	}, options
}

// untilPulled answers the scans of the world, which finds nothing, until the loop has made its puller, which it does at the
// first pull, and returns the options of the puller.
func (w *world) untilPulled(t *testing.T, options <-chan pull.Options) pull.Options {
	t.Helper()
	w.scanner.awaitBegun(t)
	for range 100 {
		select {
		case got := <-options:
			return got
		default:
			w.next(t, found())
		}
	}
	t.Fatal("no puller was made in 100 scans")
	return pull.Options{}
}

func TestTheCommitterAndThePullerAreGivenTheSameBreakerWithTheLimitOfTheSettings(t *testing.T) {
	w := newWorld()
	takePuller, pullOptions := pullOptionsOf()
	start(t, w.with(takePuller, func(o *Options) {
		o.Config.CommitMaxPerHour = 7
		// The first pull is due within an interval, and with a short one the scans that wait for it are few.
		o.Config.SyncInterval = testInterval
	}))

	puller := w.untilPulled(t, pullOptions)
	committer := <-w.commitOpt

	if committer.Gate == nil || committer.Gate != puller.Gate {
		t.Fatalf("the committer has %v and the puller %v, want one breaker for both: they count in one file", committer.Gate, puller.Gate)
	}
	made, ok := committer.Gate.(*breaker.Breaker)
	if !ok || made.Limit() != 7 {
		t.Errorf("the gate is %T with the limit %v, want a breaker with the limit of the settings, 7", committer.Gate, made)
	}
}

func TestTheBreakerHasTheDefaultLimitWhenTheSettingsHaveNone(t *testing.T) {
	w := newWorld()
	start(t, w.with())

	made, ok := (<-w.commitOpt).Gate.(*breaker.Breaker)

	if !ok || made.Limit() != breaker.DefaultLimit {
		t.Errorf("the breaker has the limit %v, want the default %d", made, breaker.DefaultLimit)
	}
}

func TestATreebindThatOnlyPullsHasNoBreakerAndNoCommitter(t *testing.T) {
	w := newWorld()
	takePuller, pullOptions := pullOptionsOf()
	start(t, w.with(takePuller, func(o *Options) { o.Config.SyncDirection = config.DirectionPull }))

	if got := <-pullOptions; got.Gate != nil {
		t.Errorf("the puller of a Treebind that only pulls has %v, want no breaker: it makes no commit", got.Gate)
	}
}

func TestTheBreakerCountsInTheStateDirectoryAndIsFencedByTheLockThatTheAgentHolds(t *testing.T) {
	root, lockDir := claimWithState(t)
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		takesTheLock(root, nil)(o)
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.Config.CommitMaxPerHour = 2
		// The clock of this agent does not move, so it makes no heartbeat, and another agent sees the lock
		// unchanged for a whole TTL and takes it over.
		o.LockClock = newFakeClock()
	}))
	gate := (<-w.commitOpt).Gate
	journal := filepath.Join(root, ".treebind", "commit-times")
	ctx := context.Background()

	for range 2 {
		if err := gate.Allow(ctx); err != nil {
			t.Fatalf("Allow = %v, want a commit to be allowed under the limit", err)
		}
		if err := gate.Made(ctx); err != nil {
			t.Fatalf("Made = %v, want the commit to be counted while the agent holds the lock", err)
		}
	}
	if data, err := os.ReadFile(journal); err != nil || strings.Count(string(data), "\n") != 2 {
		t.Errorf("the file of the state directory holds %q, %v, want the times of the two commits", data, err)
	}
	if held, ok := errors.AsType[*breaker.HeldError](gate.Allow(ctx)); !ok || held.Limit != 2 {
		t.Errorf("Allow after two commits = %v, want a hold with the limit 2", held)
	}

	thief, err := lock.Acquire(ctx, lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	if err := gate.Made(ctx); !errors.Is(err, lock.ErrLost) {
		t.Errorf("Made = %v, want the loss of the lock that another agent took: nothing is written without it", err)
	}
	if data, _ := os.ReadFile(journal); strings.Count(string(data), "\n") != 2 {
		t.Errorf("the file holds %q after a commit that the lock did not allow to count, want it as it was", data)
	}
	if stopped := r.waitStopped(t); !errors.Is(stopped, ErrLockLost) {
		t.Errorf("Run returned %v, want ErrLockLost", stopped)
	}
}

func TestACommitThatTheBreakerHeldIsAFailedCommitWithItsReasonAndNotAPassedOne(t *testing.T) {
	w := newWorld()
	const words = "the agent made 60 commits in the last hour and its limit is 60"
	w.committer.answer = func(n int, _ []scan.Change) (commit.Result, error) {
		if n == 1 {
			return commit.Result{}, &commit.Error{Reason: breaker.ReasonTooManyCommits, Message: words}
		}
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: 1}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != "TooManyCommits" || got.Message != words {
		t.Fatalf("commit = %+v, want it failed, with TooManyCommits and the words of the breaker", got)
	}
	if check, _ := changesCheck(t, r); check.Outcome != outcomePassed {
		t.Errorf("changes = %+v, want the scans to pass while the commit is held", check)
	}

	// The file is still there, and the next scan hands it over again, which is how the commit goes on once the hour allows it.
	w.next(t, found(changed(fileOne, scan.Modified, 1)))

	if got, _ := commitCheck(t, r); got.Outcome != outcomePassed || got.Reason != "" || !strings.Contains(got.Message, "committed 1 file") {
		t.Errorf("commit = %+v, want the second try to have passed", got)
	}
}

func TestACommitThatWasNotCountedStaysPassedAndSaysSo(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: 1, Warning: "the commit was not counted"}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomePassed || !strings.HasPrefix(got.Message, "committed 1 file as "+shortOf(testCommit)) || !strings.HasSuffix(got.Message, "; the commit was not counted") {
		t.Errorf("commit = %+v, want it passed, with the warning after what was committed", got)
	}
}

func TestAResolutionThatTheBreakerHeldIsAFailedPullThatDoesNotMakeTheWaitGrow(t *testing.T) {
	checked, failed := pullResult(pull.Result{}, &pull.Error{Reason: pull.ReasonTooManyCommits, Message: "the conflict was not resolved: the agent made 60 commits"})

	if checked.Outcome != outcomeFailed || checked.Reason != "TooManyCommits" || !strings.Contains(checked.Message, "was not resolved") {
		t.Errorf("pull = %+v, want it failed, with TooManyCommits and the words of the resolution", checked)
	}
	if failed {
		t.Error("the wait before the next pull grows for a resolution that was held: the repository answered, and asking it again sooner or later costs the same one question")
	}
}

func TestAnAgentThatCannotMakeItsBreakerBoundsNothingByFailingEveryCommitAndResolutionAndSaysWhy(t *testing.T) {
	// The limit is out of what the API admits, which the settings of the pod never are: the agent does not run without a bound
	// that it cannot make, so the commit step and the resolutions fail with the cause and write nothing.
	w := newWorld()
	takePuller, pullOptions := pullOptionsOf()
	start(t, w.with(takePuller, func(o *Options) {
		o.Config.CommitMaxPerHour = breaker.MaxLimit + 1
		o.Config.SyncInterval = testInterval
	}))

	puller := w.untilPulled(t, pullOptions)
	committer := <-w.commitOpt

	for name, gate := range map[string]breaker.Gate{"the committer": committer.Gate, "the puller": puller.Gate} {
		if gate == nil {
			t.Fatalf("%s has no breaker: it would commit without a limit", name)
		}
		err := gate.Allow(context.Background())
		if _, held := errors.AsType[*breaker.HeldError](err); err == nil || held || !strings.Contains(err.Error(), "10001") {
			t.Errorf("%s: Allow = %v, want an error that is not a hold and that names the limit that cannot be used", name, err)
		}
		if err := gate.Made(context.Background()); err != nil {
			t.Errorf("%s: Made = %v, want nothing to say: no commit was made", name, err)
		}
	}
}

func TestTheBreakerCountsByTheClockOfTheLoopWhenItSaysWhatTimeItIs(t *testing.T) {
	// The clock of the loop is what a test moves, and the times in the file are of it: a breaker that read the time of the system
	// would count in another hour than the loop lives in.
	root, _ := claimWithState(t)
	w := newWorld()
	start(t, w.with(func(o *Options) {
		takesTheLock(root, nil)(o)
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.LockClock = newFakeClock()
	}))
	gate := (<-w.commitOpt).Gate

	if err := gate.Made(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, ".treebind", "commit-times"))
	if err != nil {
		t.Fatal(err)
	}
	counted, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("the file holds %q: %v", data, err)
	}
	if now := w.clock.Now().Unix(); counted < wallStart.Unix() || counted > now {
		t.Errorf("the commit was counted at %d, want a time of the clock of the loop, from %d to %d", counted, wallStart.Unix(), now)
	}
}
