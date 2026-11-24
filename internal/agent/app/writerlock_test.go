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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

const (
	testPod      = "dashboards-sync-0"
	lockTTL      = 150 * time.Millisecond
	lockOther    = "another-agent"
	messageHolds = "holds the writer lock"
)

// fakeClock is a clock that moves when a test says. A timer set on it fires when the clock is advanced
// past it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// claimWithState makes a claim, with the state directory the agent makes, and returns the directory of
// the lock.
func claimWithState(t *testing.T) (root, lockDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".treebind"), 0o700); err != nil {
		t.Fatal(err)
	}
	lockDir = filepath.Join(root, ".treebind", "lock")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, lockDir
}

// takesTheLock makes the agent of a test take the real writer lock on a claim, with a TTL that is short.
func takesTheLock(root string, open func(context.Context, checkout.Options) (checkout.Result, error)) func(*Options) {
	return func(o *Options) {
		o.Config = config.Config{
			Namespace: testNS, Name: testName, PodName: testPod, MountPath: root, RepoURL: "https://example.com/a.git",
		}
		o.AcquireLock = lock.Acquire
		o.LockTTL = lockTTL
		if open != nil {
			o.Checkout = open
		}
	}
}

func lockCheck(t *testing.T, r *running) (check struct{ Outcome, Reason, Message string }, found bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckLock {
			return struct{ Outcome, Reason, Message string }{c.Outcome, c.Reason, c.Message}, true
		}
	}
	return check, false
}

func checkNames(t *testing.T, r *running) string {
	t.Helper()
	names := make([]string, 0, 4)
	for _, c := range r.report(t).Checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// holderOf returns the holder in the last slot of the lock.
func holderOf(t *testing.T, lockDir string) string {
	t.Helper()
	entries, err := os.ReadDir(lockDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the lock directory holds no slot: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	data, err := os.ReadFile(filepath.Join(lockDir, names[len(names)-1]))
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ Holder string }
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("the last slot is not a record: %v: %q", err, data)
	}
	return record.Holder
}

// lastSlotReleased says whether the last slot of the lock is a release.
func lastSlotReleased(t *testing.T, lockDir string) bool {
	t.Helper()
	entries, err := os.ReadDir(lockDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the lock directory holds no slot: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	data, err := os.ReadFile(filepath.Join(lockDir, names[len(names)-1]))
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ Released bool }
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("the last slot is not a record: %v: %q", err, data)
	}
	return record.Released
}

func TestTakesTheLockBeforeTheCheckoutAndReportsIt(t *testing.T) {
	root, lockDir := claimWithState(t)
	var slotsAtCheckout atomic.Int64
	r := start(t, takesTheLock(root, func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		entries, _ := os.ReadDir(lockDir)
		slotsAtCheckout.Store(int64(len(entries)))
		return cloned(ctx, o)
	}))

	eventually(t, "the checkout in the report", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})

	if slotsAtCheckout.Load() == 0 {
		t.Error("the checkout ran before the lock was held: it writes, and nothing writes without the lock")
	}
	got, _ := lockCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, messageHolds) {
		t.Errorf("lock = %+v, want it passed and holding", got)
	}
	if names := checkNames(t, r); names != "Mount,Writable,Lock,Checkout" {
		t.Errorf("checks = %s, want the lock between the volume checks and the checkout", names)
	}
}

func TestNamesTheHolderOfTheLockAfterThePodAndAToken(t *testing.T) {
	holders := map[string]bool{}
	for range 2 {
		root, lockDir := claimWithState(t)
		r := start(t, takesTheLock(root, nil))
		eventually(t, "the lock in the report", func() bool {
			got, _ := lockCheck(t, r)
			return got.Outcome == outcomePassed
		})

		holder := holderOf(t, lockDir)

		if !strings.HasPrefix(holder, testPod+"-") || len(holder) != len(testPod)+1+2*tokenBytes {
			t.Errorf("holder = %q, want the name of the pod, a dash and %d hex characters", holder, 2*tokenBytes)
		}
		holders[holder] = true
	}
	if len(holders) != 2 {
		t.Errorf("two starts of one pod named their holders %v, want two names: the pod's name is the same after a restart", holders)
	}
}

func TestWaitsForAnotherAgentThatHoldsTheLockAndTakesItWhenItIsGivenUp(t *testing.T) {
	root, lockDir := claimWithState(t)
	other, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	keeping, stopKeeping := context.WithCancel(context.Background())
	kept := make(chan error, 1)
	go func() { kept <- other.Keep(keeping) }()
	var opened atomic.Int64
	r := start(t, takesTheLock(root, func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		opened.Add(1)
		return cloned(ctx, o)
	}))

	eventually(t, "the wait in the report", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeSkipped && strings.Contains(got.Message, "waiting")
	})
	if opened.Load() != 0 {
		t.Error("the checkout ran while another agent held the lock")
	}
	if got, _ := checkoutCheck(t, r); got.Outcome != outcomeSkipped || !strings.Contains(got.Message, "lock is not held yet") {
		t.Errorf("checkout = %+v, want it skipped, and the message to say that the lock comes first", got)
	}

	stopKeeping()
	if err := <-kept; err != nil {
		t.Fatal(err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the lock to be taken", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})
	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
}

func TestKeepsTryingToTakeTheLockWhileTheVolumeRefuses(t *testing.T) {
	root, _ := claimWithState(t)
	var tries, refusals atomic.Int64
	var opened atomic.Int64
	// The volume lets the lock be made when the test says so, and not after some number of tries: the test reads
	// the report while it does not, and the agent must not have gone on by then.
	volumeBack := make(chan struct{})
	r := start(t, func(o *Options) {
		takesTheLock(root, func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return cloned(ctx, options)
		})(o)
		o.AcquireLock = func(ctx context.Context, cfg lock.Config) (*lock.Lock, error) {
			tries.Add(1)
			select {
			case <-volumeBack:
				return lock.Acquire(ctx, cfg)
			default:
				refusals.Add(1)
				return nil, errors.New("lock: create the lock directory: read-only file system")
			}
		}
	})

	eventually(t, "the failure in the report", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})
	if got, _ := lockCheck(t, r); got.Reason != ReasonLockFailed || !strings.Contains(got.Message, "read-only") {
		t.Errorf("lock = %+v, want the reason and what the volume said", got)
	}
	if opened.Load() != 0 {
		t.Error("the checkout ran while the lock could not be taken")
	}
	eventually(t, "the lock to be asked for again", func() bool { return tries.Load() >= 3 })

	close(volumeBack)
	eventually(t, "the lock to be taken", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})
	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
	if tries.Load() != refusals.Load()+1 {
		t.Errorf("the lock was asked for %d times after %d refusals, want the refusals and one that works", tries.Load(), refusals.Load())
	}
}

func TestSaysTheLockAndTheCheckoutWereNotTriedWhileAVolumeCheckFails(t *testing.T) {
	root, _ := claimWithState(t)
	var asked atomic.Int64
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		o.AcquireLock = func(context.Context, lock.Config) (*lock.Lock, error) { asked.Add(1); return nil, errors.New("no") }
		o.Preflight = func(context.Context, string, string) []preflight.Result { return failed }
	})

	eventually(t, "the lock to be skipped", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeSkipped
	})

	got, _ := lockCheck(t, r)
	if got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "volume check failed") {
		t.Errorf("lock = %+v, want it skipped with the reason that says why", got)
	}
	if next, _ := checkoutCheck(t, r); !strings.Contains(next.Message, "volume check failed") {
		t.Errorf("checkout = %+v, want it to say why it was not tried", next)
	}
	if asked.Load() != 0 {
		t.Error("the lock was asked for while a volume check failed")
	}
}

func TestSaysALayoutThatCannotBeWorkedOutAsAFailureOfTheLock(t *testing.T) {
	var asked, opened atomic.Int64
	r := start(t, func(o *Options) {
		takesTheLock("work", func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return cloned(ctx, options)
		})(o)
		o.AcquireLock = func(context.Context, lock.Config) (*lock.Lock, error) { asked.Add(1); return nil, errors.New("no") }
	})

	eventually(t, "the failure in the report", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	got, _ := lockCheck(t, r)
	if got.Reason != ReasonLockFailed || !strings.Contains(got.Message, "mount path") || asked.Load() != 0 {
		t.Errorf("lock = %+v, asked %d times, want a failure that says what is wrong with the mount path, and no lock asked for", got, asked.Load())
	}
	if opened.Load() != 0 {
		t.Error("the checkout was made by an agent that could not say where its lock is: nothing writes without the lock")
	}
}

func TestStopsWithTheLossOfTheLockWhenAnotherAgentTakesItAndStopsTheCheckout(t *testing.T) {
	root, lockDir := claimWithState(t)
	clock := newFakeClock()
	checkoutStopped := make(chan struct{})
	started := make(chan struct{})
	r := start(t, func(o *Options) {
		takesTheLock(root, func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
			close(started)
			<-ctx.Done()
			close(checkoutStopped)
			return checkout.Result{}, ctx.Err()
		})(o)
		o.LockClock = clock
	})
	<-started

	// The clock of this agent does not move, so it makes no heartbeat, and another agent, with the clock of
	// the system, sees the lock unchanged for a whole TTL and takes it over.
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })
	// Now the agent makes its heartbeat, and finds that another holds the lock.
	clock.Advance(lockTTL)

	err = r.waitStopped(t)

	if !errors.Is(err, ErrLockLost) || !errors.Is(err, lock.ErrLost) {
		t.Errorf("Run returned %v, want ErrLockLost, with the loss of the lock underneath", err)
	}
	select {
	case <-checkoutStopped:
	default:
		t.Error("the checkout was not told to stop: it would go on writing without the lock")
	}
	if n := r.logs.count("the writer lock was lost to another agent"); n != 1 {
		t.Errorf("the loss was logged %d times, want once\n%s", n, r.logs.String())
	}
}

// waiting is an agent that has only what waitToStop needs, and the log that it writes.
func waiting() (*agent, *safeBuffer) {
	logs := &safeBuffer{}
	return &agent{Options: Options{Logger: slog.New(slog.NewTextHandler(logs, nil))}, lost: make(chan error, 1)}, logs
}

// The two are ready before the select, as they are when a frozen container wakes up with the SIGTERM of its kubelet
// and a lock that another agent took meanwhile. A select picks either at random, so the test asks many times: a
// loss that is said only half of the time is a bug that a single run shows half of the time.
func TestSaysTheLossOfTheLockEveryTimeThatItComesWithAStop(t *testing.T) {
	for i := range 200 {
		a, logs := waiting()
		ctx, stop := context.WithCancel(context.Background())
		stop()
		a.lose(lock.ErrLost)

		serveErr, lostErr := a.waitToStop(ctx, make(chan error))

		if serveErr != nil || !errors.Is(lostErr, lock.ErrLost) {
			t.Fatalf("round %d: waitToStop returned %v and %v, want the loss of the lock: it was lost whatever else was asked",
				i, serveErr, lostErr)
		}
		if n := logs.count("the writer lock was lost to another agent, and this one stops"); n != 1 {
			t.Fatalf("round %d: the loss was logged %d times, want once\n%s", i, n, logs.String())
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), lock.ErrLost.Error()) {
			t.Fatalf("round %d: the loss is not logged as an error with its cause\n%s", i, logs.String())
		}
		if len(a.lost) != 0 {
			t.Fatalf("round %d: the loss was left to be said again", i)
		}
	}
}

func TestSaysNothingOfTheLockWhenOnlyToldToStop(t *testing.T) {
	a, logs := waiting()
	ctx, stop := context.WithCancel(context.Background())
	stop()

	serveErr, lostErr := a.waitToStop(ctx, make(chan error))

	if serveErr != nil || lostErr != nil {
		t.Errorf("waitToStop returned %v and %v, want neither for a stop that was only asked", serveErr, lostErr)
	}
	if logs.String() != "" {
		t.Errorf("a plain stop logged:\n%s", logs.String())
	}
}

func TestSaysTheLossOfTheLockWhenItComesAlone(t *testing.T) {
	a, logs := waiting()
	a.lose(lock.ErrLost)

	serveErr, lostErr := a.waitToStop(context.Background(), make(chan error))

	if serveErr != nil || !errors.Is(lostErr, lock.ErrLost) {
		t.Errorf("waitToStop returned %v and %v, want the loss of the lock", serveErr, lostErr)
	}
	if n := logs.count("the writer lock was lost to another agent"); n != 1 {
		t.Errorf("the loss was logged %d times, want once\n%s", n, logs.String())
	}
}

func TestSaysAHeartbeatThatTheVolumeRefusedForATTLAndNotAnotherAgent(t *testing.T) {
	a, logs := waiting()
	// What the keeper says when the volume is full: every slot of the heartbeat was refused for a TTL.
	full := fmt.Errorf("%w for 30s, so it counts as lost: %w", lock.ErrNotRenewed,
		&fs.PathError{Op: "write", Path: "/claim/.treebind/lock/slot-00000000000000000007", Err: syscall.ENOSPC})
	a.lose(full)

	serveErr, lostErr := a.waitToStop(context.Background(), make(chan error))

	if serveErr != nil || !errors.Is(lostErr, lock.ErrLost) || !errors.Is(lostErr, syscall.ENOSPC) {
		t.Errorf("waitToStop returned %v and %v, want the loss of the lock, with the error of the volume", serveErr, lostErr)
	}
	if n := logs.count("the heartbeat of the writer lock could not be written for a whole TTL"); n != 1 {
		t.Errorf("the heartbeat that could not be written was logged %d times, want once\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "no space left on device") ||
		strings.Contains(logs.String(), "another agent") {
		t.Errorf("the loss is not logged as an error with its cause, or is said to be another agent's\n%s", logs.String())
	}
}

func TestStopsWhenTheHeartbeatOfTheLockCannotBeWrittenForATTLAndSaysWhy(t *testing.T) {
	root, lockDir := claimWithState(t)
	clock := newFakeClock()
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		o.LockClock = clock
	})
	eventually(t, "the lock in the report", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})

	// The volume takes no slot from now on, and no other agent is there.
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent to stop", func() bool {
		clock.Advance(lockTTL)
		select {
		case <-r.stopped:
			return true
		default:
			return false
		}
	})
	err := r.waitStopped(t)

	// It stops as for any loss, with the exit code of one, and says that the heartbeat could not be written, and why.
	if !errors.Is(err, ErrLockLost) || !errors.Is(err, lock.ErrNotRenewed) || !errors.Is(err, fs.ErrNotExist) ||
		strings.Contains(err.Error(), "another") {
		t.Errorf("Run returned %v, want ErrLockLost, with the heartbeat that could not be written and the volume's error", err)
	}
	if n := r.logs.count("the heartbeat of the writer lock could not be written"); n != 1 {
		t.Errorf("the heartbeat that could not be written was logged %d times, want once\n%s", n, r.logs.String())
	}
	if n := r.logs.count("lost to another agent"); n != 0 {
		t.Errorf("a heartbeat that the volume refused was logged as a loss to another agent\n%s", r.logs.String())
	}
}

func TestReturnsAFailureOfTheEndpointWithNoLoss(t *testing.T) {
	a, logs := waiting()
	failure := errors.New("the endpoint failed")
	served := make(chan error, 1)
	served <- failure

	serveErr, lostErr := a.waitToStop(context.Background(), served)

	if !errors.Is(serveErr, failure) || lostErr != nil || logs.String() != "" {
		t.Errorf("waitToStop returned %v and %v, and logged %q, want the failure and no loss", serveErr, lostErr, logs.String())
	}
}

// The keeper that ends with the stop does not see a loss that is there by then, and the release of the lock does.
// The agent of the test is told to stop with the lock taken over, and nothing but the release can say so.
func TestStopsWithTheLossOfTheLockWhenAnotherAgentHasTakenItAndTheStopWasAsked(t *testing.T) {
	root, lockDir := claimWithState(t)
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		// The clock of this agent does not move, so it makes no heartbeat, and another agent sees the lock unchanged
		// for a whole TTL and takes it over.
		o.LockClock = newFakeClock()
	})
	eventually(t, "the lock in the report", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	r.cancel()
	err = r.waitStopped(t)

	if !errors.Is(err, ErrLockLost) || !errors.Is(err, lock.ErrLost) {
		t.Errorf("Run returned %v, want ErrLockLost: the lock was lost, and the stop does not change that", err)
	}
	if n := r.logs.count("the writer lock was lost to another agent, and this one stops"); n != 1 {
		t.Errorf("the loss was logged %d times, want once\n%s", n, r.logs.String())
	}
	if n := r.logs.count("could not be given up"); n != 0 {
		t.Errorf("the release said that the lock would be taken over after its TTL, and another agent holds it\n%s",
			r.logs.String())
	}
}

func TestStopsWithNothingToSayOfTheLockWhenToldToStopAndItIsStillHeld(t *testing.T) {
	root, _ := claimWithState(t)
	r := start(t, takesTheLock(root, cloned))
	eventually(t, "the lock in the report", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})

	r.cancel()
	err := r.waitStopped(t)

	if err != nil {
		t.Errorf("Run returned %v, want nil for a stop that was asked", err)
	}
	if n := r.logs.count("lost to another agent"); n != 0 {
		t.Errorf("a stop that was asked logged a loss of the lock\n%s", r.logs.String())
	}
}

func TestGivesTheLockUpWhenToldToStop(t *testing.T) {
	root, lockDir := claimWithState(t)
	r := start(t, takesTheLock(root, nil))
	eventually(t, "the lock in the report", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})

	r.cancel()
	if err := r.waitStopped(t); err != nil {
		t.Fatal(err)
	}

	// A lock that was given up is taken at once. One that was not would take the TTL of an hour.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next, err := lock.Acquire(ctx, lock.Config{Dir: lockDir, Holder: lockOther, TTL: time.Hour})
	if err != nil {
		t.Fatalf("the next agent could not take the lock: %v: the agent that stopped did not give it up", err)
	}
	if next.Epoch() == 0 {
		t.Error("the lock has no epoch")
	}
}

func TestTakesNoLockWhenNoneIsToBeTaken(t *testing.T) {
	r := start(t, func(o *Options) { withRepository(cloned)(o) })

	eventually(t, "the checkout in the report", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})

	if _, found := lockCheck(t, r); found {
		t.Error("a lock is reported, and none was asked for")
	}
}

func TestAsksForTheLockInItsDirectoryWithTheDefaultTTLAndAHolderOfItsOwn(t *testing.T) {
	root, _ := claimWithState(t)
	asked := make(chan lock.Config, 1)
	start(t, func(o *Options) {
		takesTheLock(root, nil)(o)
		o.LockTTL = 0
		o.AcquireLock = func(_ context.Context, cfg lock.Config) (*lock.Lock, error) {
			select {
			case asked <- cfg:
			default:
			}
			return nil, errors.New("no lock for this test")
		}
	})

	cfg := <-asked

	if cfg.Dir != filepath.Join(root, ".treebind", "lock") || cfg.TTL != DefaultLockTTL ||
		!strings.HasPrefix(cfg.Holder, testPod+"-") || cfg.Clock != nil {
		t.Errorf("lock.Config = %+v, want the lock directory of the layout, the default TTL, the holder after the pod, and the system clock", cfg)
	}
}

func TestStopsWhileWaitingForTheLockWithoutAFailureOfItAndWithoutACheckout(t *testing.T) {
	root, _ := claimWithState(t)
	asking := make(chan struct{})
	var once sync.Once
	var opened atomic.Int64
	r := start(t, func(o *Options) {
		takesTheLock(root, func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return cloned(ctx, options)
		})(o)
		o.AcquireLock = func(ctx context.Context, _ lock.Config) (*lock.Lock, error) {
			once.Do(func() { close(asking) })
			<-ctx.Done()
			return nil, errors.Join(errors.New("lock: waiting for the holder to give it up"), ctx.Err())
		}
	})
	<-asking

	r.cancel()
	if err := r.waitStopped(t); err != nil {
		t.Fatal(err)
	}

	if n := r.logs.count("outcome=Failed"); n != 0 {
		t.Errorf("an agent told to stop while it waited for the lock logged %d failures of it, want none\n%s", n, r.logs.String())
	}
	if opened.Load() != 0 {
		t.Error("the checkout was made by an agent that stopped before it had the lock")
	}
}

func TestGivesUpALockThatWasMadeInTheInstantOfTheStop(t *testing.T) {
	root, lockDir := claimWithState(t)
	asked := make(chan struct{})
	var once sync.Once
	r := start(t, func(o *Options) {
		takesTheLock(root, nil)(o)
		o.AcquireLock = func(ctx context.Context, cfg lock.Config) (*lock.Lock, error) {
			once.Do(func() { close(asked) })
			<-ctx.Done()
			// The lock is made after the agent was told to stop, as when the two come together.
			return lock.Acquire(context.Background(), cfg)
		}
	})
	<-asked

	r.cancel()
	if err := r.waitStopped(t); err != nil {
		t.Fatal(err)
	}

	// A lock that was given up is taken at once. One that was not would take the TTL of an hour.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := lock.Acquire(ctx, lock.Config{Dir: lockDir, Holder: lockOther, TTL: time.Hour}); err != nil {
		t.Fatalf("the next agent could not take the lock: %v: the agent that stopped did not give it up", err)
	}
}

// slowToStop is a puller that takes its time to stop once the agent is told to, as Git does while it is
// killed in the middle of an update. It returns when the test says.
type slowToStop struct {
	began, stopping, finish chan struct{}
	once                    sync.Once
}

func (s *slowToStop) Pull(ctx context.Context) (pull.Result, error) {
	s.once.Do(func() { close(s.began) })
	<-ctx.Done()
	close(s.stopping)
	<-s.finish
	return pull.Result{}, ctx.Err()
}

func TestGivesTheLockUpOnlyOnceAPullThatIsStoppingHasEnded(t *testing.T) {
	root, lockDir := claimWithState(t)
	s := &slowToStop{began: make(chan struct{}), stopping: make(chan struct{}), finish: make(chan struct{})}
	change, _ := withPuller(newFakePuller(), &loopClock{})
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		change(o)
		o.NewPuller = func(pull.Options) (Puller, error) { return s, nil }
	})
	<-s.began

	r.cancel()
	<-s.stopping

	// The keeper has been told to stop as well, and the pull is not over: the lock is still held. Nothing
	// says when a thing that must not happen would have, so the look is for as long as a release takes by far.
	deadline := time.After(250 * time.Millisecond)
	for tick := time.NewTicker(time.Millisecond); ; {
		if lastSlotReleased(t, lockDir) {
			t.Fatal("the lock was given up while the pull was still stopping: the next agent could write beside it")
		}
		select {
		case <-tick.C:
			continue
		case <-deadline:
			tick.Stop()
		}
		break
	}

	close(s.finish)
	if err := r.waitStopped(t); err != nil {
		t.Fatal(err)
	}
	if !lastSlotReleased(t, lockDir) {
		t.Error("the lock was not given up once the pull had ended")
	}
}
