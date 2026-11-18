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
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/heartbeat"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// pending is how many timers are set on the clock and have not fired.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// healthz is the answer of the liveness probe.
func (r *running) healthz(t *testing.T) int {
	t.Helper()
	code, body := r.get(t, status.HealthPath)
	switch {
	case code == http.StatusOK && body != "ok\n":
		t.Errorf("healthz says %q with 200", body)
	case code == http.StatusServiceUnavailable && body != "not alive\n":
		t.Errorf("healthz says %q with 503", body)
	}
	return code
}

// wantAliveUntil advances the clock that the agent beats on to the end of a promise of the given length and
// its grace, and says what the probe answers there and a nanosecond after: alive, and then not.
func wantAliveUntil(t *testing.T, r *running, clock *fakeClock, promise time.Duration, what string) {
	t.Helper()
	clock.Advance(promise + heartbeat.Grace)
	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("%s: healthz is %d at the end of the promise of %v and the grace, want 200", what, code, promise)
	}
	clock.Advance(time.Nanosecond)
	if code := r.healthz(t); code != http.StatusServiceUnavailable {
		t.Errorf("%s: healthz is %d a nanosecond after the grace, want 503: nothing beat", what, code)
	}
}

func TestIsAliveAtStartAndTheReadinessIsNotWhatItFollows(t *testing.T) {
	clock := newFakeClock()
	r := start(t, func(o *Options) {
		o.HeartbeatClock = clock.Now
		// A volume check that does not return: the agent is not ready, and is alive.
		o.Preflight = func(ctx context.Context, _, _ string) []preflight.Result {
			<-ctx.Done()
			return nil
		}
	})

	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("healthz is %d at the start, want 200", code)
	}
	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d, want 503", code)
	}
}

func TestIsNotAliveWhenAVolumeCheckDoesNotReturnForLongerThanTheCodeMayTake(t *testing.T) {
	clock := newFakeClock()
	began := make(chan struct{}, 1)
	r := start(t, func(o *Options) {
		o.HeartbeatClock = clock.Now
		o.Preflight = func(ctx context.Context, _, _ string) []preflight.Result {
			began <- struct{}{}
			<-ctx.Done()
			return nil
		}
	})
	<-began

	wantAliveUntil(t, r, clock, heartbeat.Work, "a volume check that does not return")

	// The probe says why in the log, once, and not at every request of the kubelet.
	r.healthz(t)
	r.healthz(t)
	if n := r.logs.count("the loop of the agent did not beat within what it promised"); n != 1 {
		t.Errorf("the late loop was logged %d times, want once:\n%s", n, r.logs.String())
	}
}

func TestIsNotAliveWhenAPullRunsForLongerThanTheCodeMayTakeAndIsAliveAgainWhenItEnds(t *testing.T) {
	clock := newFakeClock()
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.HeartbeatClock = clock.Now
	})
	f.awaitBegun(t)

	wantAliveUntil(t, r, clock, heartbeat.Work, "a pull that does not return")
	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d: a loop that is late is not what readiness is about, and nothing has been pulled", code)
	}

	// The pull ends, and the loop is heard from again: a loop that was slow and went on is not restarted.
	f.answer(t, upToDate(testCommit))
	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("healthz is %d after the pull ended and the next began, want 200", code)
	}
}

func TestIsAliveForAsLongAsTheLoopWaitsForTheNextPullAndNotLonger(t *testing.T) {
	clock := newFakeClock()
	f := newFakePuller()
	waits := (&loopClock{}).stallAfter(1)
	change, _ := withPuller(f, waits)
	r := start(t, func(o *Options) {
		change(o)
		o.Config.SyncInterval = 24 * time.Hour
		o.HeartbeatClock = clock.Now
	})
	f.awaitBegun(t)
	f.answers <- upToDate(testCommit)
	// The first wait passes and the second, the interval, does not: the loop is in it.
	eventually(t, "the loop to wait for the next pull", func() bool { return len(waits.waits()) == 2 })
	if got := waits.waits()[1]; got != 24*time.Hour {
		t.Fatalf("the wait is %v, want the interval of 24h that the jitter does not move", got)
	}

	// An hour is longer than the code may take, and the loop is not late: it said that it would wait.
	clock.Advance(23 * time.Hour)
	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("healthz is %d 23h into a wait of 24h, want 200", code)
	}
	wantAliveUntil(t, r, clock, time.Hour, "the end of a wait of 24h")
}

func TestHoldsAWaitAfterAStepThatFailedToItsOwnLength(t *testing.T) {
	long := func(o *Options) { o.RetryMin, o.RetryMax = 2*time.Hour, 2*time.Hour }
	for name, change := range map[string]func(*Options){
		"the volume checks": func(o *Options) {
			o.Preflight = func(context.Context, string, string) []preflight.Result { return failed }
		},
		"the writer lock": func(o *Options) {
			o.AcquireLock = func(context.Context, lock.Config) (*lock.Lock, error) {
				return nil, errors.New("the volume refused the slot")
			}
			o.Checkout = cloned
		},
		"the checkout": func(o *Options) {
			withRepository(func(context.Context, checkout.Options) (checkout.Result, error) {
				return checkout.Result{}, errors.New("no route to host")
			})(o)
		},
	} {
		t.Run(name, func(t *testing.T) {
			clock := newFakeClock()
			r := start(t, func(o *Options) {
				long(o)
				change(o)
				o.HeartbeatClock = clock.Now
			})
			// The report is in once the step has failed and the wait is promised, in that order.
			eventually(t, "the failure in the report", func() bool {
				for _, c := range r.report(t).Checks {
					if c.Outcome == outcomeFailed {
						return true
					}
				}
				return false
			})

			wantAliveUntil(t, r, clock, 2*time.Hour, "a wait of two hours after a failed step")
		})
	}
}

func TestStaysAliveWhileAnotherAgentHoldsTheLockForAsLongAsItLikes(t *testing.T) {
	root, lockDir := claimWithState(t)
	other, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Release() })
	clock := newFakeClock()
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		// The lock clock and the clock of the beats are one, so that a look at the lock is the time passing.
		o.LockClock, o.HeartbeatClock = clock, clock.Now
		o.LockTTL = 30 * time.Second
	})

	// The reader looks every ten seconds, and the holder renews at each of them: the reader waits for as
	// long as it does. Without a beat for each look, the promise of the start would run out in 340 seconds.
	const step = 10 * time.Second
	for i := range 45 {
		eventually(t, "the agent to wait for the lock", func() bool { return clock.pending() > 0 })
		if err := other.Check(); err != nil {
			t.Fatalf("the holder's check failed after %d steps: %v", i, err)
		}
		clock.Advance(step)
		if code := r.healthz(t); code != http.StatusOK {
			t.Fatalf("healthz is %d after %v of waiting for the lock of another agent, want 200: a restart would not put that right",
				code, time.Duration(i+1)*step)
		}
	}
}

func TestIsNotAliveWhenTheWaitForTheLockOfAnotherAgentStopsLooking(t *testing.T) {
	root, lockDir := claimWithState(t)
	other, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Release() })
	// The clock of the lock is one that nothing moves, so the wait for the next look never ends, and the clock
	// of the beats is another that the test moves.
	lockClock, clock := newFakeClock(), newFakeClock()
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		o.LockClock, o.HeartbeatClock = lockClock, clock.Now
		o.LockTTL = 30 * time.Second
	})
	eventually(t, "the agent to wait for the lock", func() bool { return lockClock.pending() > 0 })

	// The wait promised ten seconds and then a look, and the look never comes. It is a wait that is stuck, and
	// the one beat that it made is what holds it to that.
	wantAliveUntil(t, r, clock, 10*time.Second+heartbeat.Work, "a wait for the lock that does not end")
}

func TestAnAgentThatHasNothingMoreToDoIsAliveForGood(t *testing.T) {
	clock := newFakeClock()
	// No lock, no checkout and no pulls: once the volume checks pass there is nothing left to do, and a restart
	// would only bring the agent back to this.
	r := start(t, func(o *Options) { o.HeartbeatClock = clock.Now })
	eventually(t, "the volume checks", func() bool { return len(r.report(t).Checks) == 2 })

	clock.Advance(1000 * time.Hour)

	eventually(t, "the probe to say alive", func() bool { return r.healthz(t) == http.StatusOK })
}

func TestTheGitRunnersOfEveryStepBeatForTheAgentWithTheTimeoutOfTheCommand(t *testing.T) {
	const longCommand = 40 * time.Minute
	// Each case parks the agent in one step, so that nothing but the test beats, and then plays the Runner
	// of that step: a command begins with its timeout, and when it ends the code that follows has its own.
	check := func(t *testing.T, r *running, clock *fakeClock, running func(time.Duration) func()) {
		t.Helper()
		if running == nil {
			t.Fatal("the Runner of this step has no hook for the agent to know a command by")
		}
		done := running(longCommand)
		wantAliveUntil(t, r, clock, longCommand, "a Git command of 40 minutes")
		done()
		if code := r.healthz(t); code != http.StatusOK {
			t.Errorf("healthz is %d once the command has ended, want 200: the code that follows has a promise of its own", code)
		}
		wantAliveUntil(t, r, clock, heartbeat.Work, "the code after a Git command")
	}

	t.Run("the checkout", func(t *testing.T) {
		clock := newFakeClock()
		asked := make(chan checkout.Options, 1)
		r := start(t, func(o *Options) {
			withRepository(func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
				asked <- options
				<-ctx.Done()
				return checkout.Result{}, ctx.Err()
			})(o)
			o.HeartbeatClock = clock.Now
		})
		options := <-asked
		check(t, r, clock, options.Git.Running)
	})

	t.Run("the pulls", func(t *testing.T) {
		clock := newFakeClock()
		f := newFakePuller()
		change, made := withPuller(f, &loopClock{})
		r := start(t, func(o *Options) {
			change(o)
			o.HeartbeatClock = clock.Now
		})
		f.awaitBegun(t)
		options := <-made
		check(t, r, clock, options.Git.Running)
	})

	t.Run("the scans", func(t *testing.T) {
		clock := newFakeClock()
		w := newWorld()
		r := start(t, w.with(func(o *Options) { o.HeartbeatClock = clock.Now }))
		w.scanner.awaitBegun(t)
		options := <-w.scanOpts
		check(t, r, clock, options.Git.Running)
	})
}

func TestTheScanBeatsWhenItBeginsAndIsAliveAgainWhenItEnds(t *testing.T) {
	clock := newFakeClock()
	w := newWorld()
	r := start(t, w.with(func(o *Options) { o.HeartbeatClock = clock.Now }))
	w.scanner.awaitBegun(t)

	wantAliveUntil(t, r, clock, heartbeat.Work, "a scan that does not return")

	w.next(t, found())
	if code := r.healthz(t); code != http.StatusOK {
		t.Errorf("healthz is %d after the scan ended and the next began, want 200", code)
	}
}

func TestAnAgentThatIsLateAndIsToldToStopStopsCleanly(t *testing.T) {
	// A stop is not a stuck loop: the late loop does not turn the stop into an error, and Run returns.
	clock := newFakeClock()
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.HeartbeatClock = clock.Now
	})
	f.awaitBegun(t)
	clock.Advance(heartbeat.Work + heartbeat.Grace + time.Hour)
	r.cancel()
	if err := r.waitStopped(t); err != nil {
		t.Errorf("Run returned %v for an agent that was late and was told to stop", err)
	}
}
