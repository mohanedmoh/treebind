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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/heartbeat"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

const (
	updatedA     = "a, updated\n"
	lockIndex    = "index.lock"
	lockRemote   = "refs/remotes/origin/main.lock"
	lockPacked   = "packed-refs.lock"
	refRecovered = "refs/treebind/recovery/00000001"
)

func plantLock(t *testing.T, gitDir, name string) string {
	t.Helper()
	path := filepath.Join(gitDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClearLocksChecksTheWriterLockOnlyWhenThereIsALockFileToRemove(t *testing.T) {
	gitDir := t.TempDir()
	var asked int
	fence := func(context.Context) error { asked++; return nil }

	if removed, err := clearLocks(context.Background(), gitDir, fence); err != nil || len(removed) != 0 || asked != 0 {
		t.Fatalf("with no lock file: removed %v, %v, asked the fence %d times, want nothing, no error and no call", removed, err, asked)
	}

	path := plantLock(t, gitDir, lockIndex)
	fence = func(context.Context) error {
		asked++
		if _, err := os.Lstat(path); err != nil {
			t.Error("the lock file was removed before the writer lock was checked")
		}
		return nil
	}
	removed, err := clearLocks(context.Background(), gitDir, fence)
	if err != nil || !slices.Equal(removed, []string{lockIndex}) || asked != 1 {
		t.Errorf("removed %v, %v, asked the fence %d times, want the lock file removed after one call", removed, err, asked)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Error("the lock file is still there")
	}
}

func TestClearLocksRemovesNothingWhenTheWriterLockCannotBeChecked(t *testing.T) {
	gitDir := t.TempDir()
	path := plantLock(t, gitDir, lockIndex)
	lost := errors.New("the lock is another agent's")

	removed, err := clearLocks(context.Background(), gitDir, func(context.Context) error { return lost })

	if len(removed) != 0 || !errors.Is(err, errFence) || !errors.Is(err, lost) {
		t.Errorf("removed %v, %v, want nothing, and an error that says the lock could not be checked, with its cause", removed, err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Error("a lock file was removed by an agent that does not hold the writer lock")
	}
}

func TestClearLocksSaysWhatItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file in a directory that it cannot write")
	}
	gitDir := t.TempDir()
	plantLock(t, gitDir, lockIndex)
	plantLock(t, gitDir, "refs/heads/main.lock")
	if err := os.Chmod(filepath.Join(gitDir, "refs", "heads"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(gitDir, "refs", "heads"), 0o755) })

	removed, err := clearLocks(context.Background(), gitDir, func(context.Context) error { return nil })

	if err == nil || errors.Is(err, errFence) || !strings.Contains(err.Error(), "refs/heads/main.lock") || !slices.Equal(removed, []string{lockIndex}) {
		t.Errorf("removed %v, %v, want the lock file that was removed, and an error that names the one that was not", removed, err)
	}
}

func TestAFailureToClearLocksIsRefusedWhenTheWriterLockCouldNotBeCheckedAndAFailureOfThePullOtherwise(t *testing.T) {
	fenced := fmt.Errorf("%w: it was lost", errFence)
	got := clearFailed(fenced)
	if got.Reason != pull.ReasonRefused || !errors.Is(got, errFence) || got.Message != fenced.Error() {
		t.Errorf("failure = %+v, want the reason Refused, the message of the error and the error underneath", got)
	}
	other := errors.New("the lock file cannot be removed")
	got = clearFailed(other)
	if got.Reason != pull.ReasonPullFailed || !errors.Is(got, other) || got.Message != other.Error() {
		t.Errorf("failure = %+v, want the reason PullFailed, the message of the error and the error underneath", got)
	}
}

func TestTheNoteNamesAFewOfTheLockFilesThatWereRemoved(t *testing.T) {
	if got := clearedNote(nil); got != "" {
		t.Errorf("note = %q, want none when nothing was removed", got)
	}
	if got, want := clearedNote([]string{lockIndex}), "removed the lock file that a Git command that was stopped left: index.lock"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
	got := clearedNote([]string{"a.lock", "b.lock", "c.lock", "d.lock", "e.lock"})
	if want := "removed 5 lock files that a Git command that was stopped left: a.lock, b.lock, c.lock and 2 more"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

// agentOn starts the agent on the claim of a fixture, with the checkout and the pulls of the packages, and
// the clock of the loop that the test passes by hand.
func agentOn(t *testing.T, f *gittest.Fixture, clock *gatedClock) *running {
	t.Helper()
	return start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval = testInterval
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(options pull.Options) (Puller, error) {
			options.Git = f.GitOptions()
			return pull.New(options)
		}
	})
}

func TestTheCheckoutOfAnAgentThatStartsAfterAKilledGitGoesOnAndSaysWhatItRemoved(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n"})
	where := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(where, testBranch)); err != nil {
		t.Fatal(err)
	}
	// The repository has a commit that the checkout does not, and a fetch of it is killed while it holds the
	// lock of the ref it updates: what a stop that Git could not answer, or a pod that was lost, leaves.
	f.Publish(testBranch, map[string]string{jsonA: "a, edited upstream\n"})
	f.HoldRef(where, "refs/remotes/origin/main", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--", f.Bare,
		"+refs/heads/main:refs/remotes/origin/main").Kill()
	stale := filepath.Join(where.GitDir, filepath.FromSlash(lockRemote))
	if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("the killed fetch left no lock: %v", err)
	}

	r := agentOn(t, f, newGatedClock())

	eventually(t, "the checkout in the report", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome != outcomeSkipped
	})
	got, _ := checkoutCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "reopened the checkout") ||
		!strings.Contains(got.Message, "removed the lock file that a Git command that was stopped left: "+lockRemote) {
		t.Errorf("checkout = %+v, want it reopened, with the lock file that was removed", got)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("the lock file is still there")
	}
}

func TestAPullAfterAKilledGitGoesOnWithoutAPersonForTheIndexAndForARef(t *testing.T) {
	for _, c := range []struct {
		name string
		hold func(f *gittest.Fixture, where layout.Layout) *gittest.Held
		lock string
		// said is what the check says of what was removed.
		said string
	}{
		{"the index", func(f *gittest.Fixture, where layout.Layout) *gittest.Held {
			gittest.Write(t, where, jsonB, "the application's edit\n")
			return f.HoldIndex(where, jsonB)
		}, lockIndex, "removed the lock file that a Git command that was stopped left: index.lock"},
		{"a ref", func(f *gittest.Fixture, where layout.Layout) *gittest.Held {
			f.Publish(testBranch, map[string]string{jsonA: "a, edited upstream\n"})
			return f.HoldRef(where, "refs/remotes/origin/main", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--",
				f.Bare, "+refs/heads/main:refs/remotes/origin/main")
		}, lockRemote, "removed the lock file that a Git command that was stopped left: refs/remotes/origin/main.lock"},
		{"a ref that is removed, which also locks packed-refs", func(f *gittest.Fixture, where layout.Layout) *gittest.Held {
			f.InCheckout(where, "update-ref", refRecovered, "HEAD")
			return f.HoldRef(where, refRecovered, "update-ref", "-d", refRecovered)
		}, lockPacked, "removed 2 lock files that a Git command that was stopped left: packed-refs.lock, refs/treebind/recovery/00000001.lock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := gittest.New(t)
			f.Publish(testBranch, map[string]string{jsonA: "a\n", jsonB: "b\n"})
			where := f.Layout(subDir)
			clock := newGatedClock()
			r := agentOn(t, f, clock)
			// The first pull finds nothing to do, and the agent waits for the next.
			clock.wait(t).pass()
			waiting := clock.wait(t)

			// A Git of the agent is killed while it writes, as a kernel that is short of memory does, and a
			// commit that the checkout does not have is waiting in the repository.
			c.hold(f, where).Kill()
			stale := filepath.Join(where.GitDir, filepath.FromSlash(c.lock))
			if _, err := os.Lstat(stale); err != nil {
				t.Fatalf("the killed Git left no %s: %v", c.lock, err)
			}
			tip := f.Publish(testBranch, map[string]string{jsonA: updatedA, jsonC: "c\n"})
			waiting.pass()
			clock.wait(t)

			got, _ := pullCheck(t, r)
			if got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced the checkout") ||
				!strings.Contains(got.Message, c.said) {
				t.Fatalf("pull = %+v, want it advanced, with the lock file that was removed said", got)
			}
			if gittest.Read(t, where, jsonA) != updatedA || f.InCheckout(where, "rev-parse", "HEAD") != tip {
				t.Error("the update is not in the checkout")
			}
			if _, err := os.Lstat(stale); err == nil {
				t.Errorf("%s is still there", c.lock)
			}
			if c.lock == lockIndex && gittest.Read(t, where, jsonB) != "the application's edit\n" {
				t.Error("the edit of the application was lost")
			}
		})
	}
}

func TestAPullThatFindsNoLockFileSaysNothingOfThem(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n"})
	f.Layout(subDir)
	clock := newGatedClock()
	r := agentOn(t, f, clock)
	clock.wait(t).pass()
	clock.wait(t)

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomePassed || strings.Contains(got.Message, "lock file") {
		t.Errorf("pull = %+v, want it passed, with nothing about lock files", got)
	}
}

func TestAPullThatCannotRemoveALockFileSaysSoAndDoesNotGoOn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file in a directory that it cannot write")
	}
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n"})
	where := f.Layout(subDir)
	clock := newGatedClock()
	r := agentOn(t, f, clock)
	clock.wait(t).pass()
	waiting := clock.wait(t)

	heads := filepath.Join(where.GitDir, "refs", "heads")
	plantLock(t, where.GitDir, "refs/heads/main.lock")
	if err := os.Chmod(heads, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(heads, 0o755) })
	waiting.pass()
	waiting = clock.wait(t)

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonPullFailed || !strings.Contains(got.Message, "refs/heads/main.lock") {
		t.Errorf("pull = %+v, want it failed with PullFailed, naming the lock file that cannot be removed", got)
	}
	if waiting.d <= testInterval {
		t.Errorf("the wait after it is %v, want one that grew, as for a pull that the volume did not let through", waiting.d)
	}
}

// agentThatLostTheLock is an agent that held the writer lock, and another agent has taken it over.
func agentThatLostTheLock(t *testing.T) *agent {
	t.Helper()
	_, lockDir := claimWithState(t)
	held, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: testPod, TTL: lockTTL, Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{
		lost:    make(chan error, 1),
		beats:   heartbeat.New(nil),
		Options: Options{Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	a.held.Store(held)
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })
	return a
}

type neverPuller struct{ called bool }

func (p *neverPuller) Pull(context.Context) (pull.Result, error) {
	p.called = true
	return pull.Result{}, nil
}

func TestAnAgentThatLostTheWriterLockRemovesNoLockFileBeforeAPullAndSaysSo(t *testing.T) {
	a := agentThatLostTheLock(t)
	gitDir := t.TempDir()
	stale := plantLock(t, gitDir, lockIndex)
	puller := &neverPuller{}
	l := &pulls{a: a, p: &progress{pulling: notTried(CheckPull, "waiting")}, options: pull.Options{Layout: layout.Layout{GitDir: gitDir}}, puller: puller}

	failed := l.once(context.Background())

	if !failed || puller.called {
		t.Errorf("failed = %v, pulled = %v, want a pull that failed and was not made", failed, puller.called)
	}
	if got := l.p.pulling; got.Outcome != preflight.Failed || got.Reason != pull.ReasonRefused || !strings.Contains(got.Message, "index.lock") {
		t.Errorf("pull = %+v, want it Refused, naming the lock file that was not removed", got)
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Error("a lock file was removed by an agent that lost the writer lock")
	}
	if len(a.lost) != 1 {
		t.Error("the loss of the lock was not said to the agent: the fence that stopped the removal is the one that says it")
	}
}

func TestAnAgentThatLostTheWriterLockRemovesNoLockFileBeforeTheCheckoutAndSaysSo(t *testing.T) {
	a := agentThatLostTheLock(t)
	var opened bool
	a.Checkout = func(context.Context, checkout.Options) (checkout.Result, error) {
		opened = true
		return checkout.Result{}, nil
	}
	gitDir := t.TempDir()
	stale := plantLock(t, gitDir, lockIndex)

	result, _ := a.runCheckout(context.Background(), checkout.Options{Layout: layout.Layout{GitDir: gitDir}})

	if opened || result.Outcome != preflight.Failed || result.Reason != checkout.ReasonCheckoutFailed ||
		!strings.Contains(result.Message, "index.lock") {
		t.Errorf("checkout = %+v, opened = %v, want it failed, naming the lock file, and not made", result, opened)
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Error("a lock file was removed by an agent that lost the writer lock")
	}
}

func TestAnAgentThatStartsAfterAnUpdateWasKilledInTheMiddleOfItsFilesUndoesItAndAppliesIt(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n", jsonB: "b\n", jsonC: "c\n"})
	where := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(where, testBranch)); err != nil {
		t.Fatal(err)
	}
	// An agent applies an update, and its Git is killed in the middle of the files, as a kernel that is short of memory does.
	held := f.HoldMerge(jsonB)
	tip := f.Publish(testBranch, map[string]string{jsonA: updatedA, jsonB: "b, updated\n", jsonC: "c, updated\n"})
	options := pull.Options{Layout: where, URL: f.URL, Branch: testBranch, Git: f.GitOptions()}
	options.Git.Git = held.Binary
	killed, err := pull.New(options)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := killed.Pull(context.Background())
		done <- err
	}()
	held.WaitHeld()
	held.Kill()
	if err := <-done; err == nil {
		t.Fatal("the pull of the merge that was killed did not fail")
	}
	if gittest.Read(t, where, jsonA) != updatedA || gittest.Exists(filepath.Join(where.WorkTree, jsonB)) {
		t.Fatal("the update was not cut off in the middle of the files: the test does not hold the merge where it means to")
	}

	// The agent that starts again removes the lock the killed Git left, undoes the update, and applies it.
	clock := newGatedClock()
	r := agentOn(t, f, clock)
	clock.wait(t).pass()
	clock.wait(t)

	// The lock is removed before the checkout is opened, which is where it is said.
	if opened, _ := checkoutCheck(t, r); !strings.Contains(opened.Message, "removed the lock file that a Git command that was stopped left: index.lock") {
		t.Errorf("checkout = %+v, want the lock file that was removed said", opened)
	}
	got, _ := pullCheck(t, r)
	for _, want := range []string{
		"advanced the checkout", "was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 0 removed",
	} {
		if got.Outcome != outcomePassed || !strings.Contains(got.Message, want) {
			t.Errorf("pull = %+v, want it passed, with %q said", got, want)
		}
	}
	if gittest.Read(t, where, jsonA) != updatedA || gittest.Read(t, where, jsonB) != "b, updated\n" ||
		gittest.Read(t, where, jsonC) != "c, updated\n" || f.InCheckout(where, "rev-parse", "HEAD") != tip {
		t.Error("the update is not in the checkout")
	}
	if n := r.logs.count("was cut off, and was undone"); n != 1 {
		t.Errorf("what the pull put right was logged %d times, want once\n%s", n, r.logs.String())
	}
}
