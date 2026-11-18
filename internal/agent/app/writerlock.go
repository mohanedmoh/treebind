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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/heartbeat"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

const (
	// CheckLock is the name of the check that says whether the agent holds the writer lock of the
	// volume. It is in the report between the volume checks and the checkout.
	CheckLock = "Lock"

	// ReasonLockFailed means the lock could not be taken: the volume refused the slot, or the lock is
	// held in a format this agent cannot read, which a newer release of Treebind wrote. The agent
	// tries again with a growing wait.
	ReasonLockFailed = "LockFailed"

	// DefaultLockTTL is how long another agent has to see no heartbeat before it takes the lock over.
	// A pod that was replaced gives the lock up when it stops, and the next agent takes it at once. One
	// that was lost with its node is taken over after this, and 30 seconds with a heartbeat every 10 is
	// long enough that a late heartbeat does not make a live holder look gone.
	DefaultLockTTL = 30 * time.Second

	// tokenBytes is how many random bytes tell two starts of the same pod apart.
	tokenBytes = 4
)

// ErrLockLost is what Run returns when the writer lock was lost to another agent. The agent has
// stopped what it was doing, and a restart waits for the lock again (docs/adr/0023).
var ErrLockLost = errors.New("the writer lock was lost to another agent")

// fillLockDefaults sets what the lock needs: the name of the holder and how long a heartbeat may be
// missing.
func (a *agent) fillLockDefaults() {
	if a.LockTTL <= 0 {
		a.LockTTL = DefaultLockTTL
	}
	name := a.Config.PodName
	if name == "" {
		name = a.Config.Name
	}
	// The name of a pod of a StatefulSet is the same after every restart, and the lock needs a
	// holder that differs between two starts, so that a holder that was frozen and wakes up after
	// its pod was replaced is not taken for its replacement.
	a.holder = name + "-" + token()
}

// token is a few random bytes in hex. A start of the agent that cannot get them is named by the clock.
func token() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// takeLock takes the writer lock, which waits for another agent that holds it, and keeps it until the
// agent stops. It tries again with a growing wait while the volume refuses, and returns true once the lock
// is held and false when the agent is told to stop first.
func (a *agent) takeLock(ctx context.Context, p *progress) bool {
	where, err := layout.New(a.Config.MountPath, a.Config.SubPath)
	if err != nil {
		// The settings were checked when the agent started, so this is not expected, and no wait
		// puts it right. It is said, and nothing is written.
		p.lock = lockFailed(err)
		a.publish(p)
		a.logChanges("writer lock", []preflight.Result{*p.lock}, nil)
		return false
	}
	cfg := lock.Config{
		Dir: where.LockDir, Holder: a.holder, TTL: a.LockTTL, Clock: a.LockClock,
		// Another agent may hold the lock for as long as it likes, and waiting for it is not a stuck loop. Each
		// wait of a look at the lock is promised with the look that follows it, which reads the volume.
		Beat: func(wait time.Duration) { a.beat(wait + heartbeat.Work) },
	}

	wait := a.RetryMin
	var logged []string
	for {
		a.working()
		held, err := a.AcquireLock(ctx, cfg)
		if ctx.Err() != nil {
			// A lock that was made in the instant of the stop is given up at once, so that the agent
			// that takes the place of this one does not wait for its TTL.
			if err == nil && held != nil {
				a.background.Go(func() { a.keepLock(ctx, held) })
			}
			return false
		}
		result := preflight.Result{
			Check: CheckLock, Outcome: preflight.Passed,
			Message: fmt.Sprintf("this agent holds the writer lock of the volume as %s, from epoch %d", a.holder, epochOf(held)),
		}
		if err != nil {
			result = *lockFailed(err)
			a.beat(wait)
		}
		p.lock = &result
		a.publish(p)
		logged = a.logChanges("writer lock", []preflight.Result{result}, logged)

		if err == nil {
			if held != nil {
				a.held.Store(held)
				a.background.Go(func() { a.keepLock(ctx, held) })
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, a.RetryMax)
	}
}

// epochOf is the epoch of a lock, or zero when there is none, which only a test that gives a lock
// of its own can do.
func epochOf(l *lock.Lock) uint64 {
	if l == nil {
		return 0
	}
	return l.Epoch()
}

// lockFailed is the failed result of the lock check for an error.
func lockFailed(err error) *preflight.Result {
	return &preflight.Result{Check: CheckLock, Outcome: preflight.Failed, Reason: ReasonLockFailed, Message: shortened(err.Error())}
}

// keepLock renews the lock until the agent stops, and gives it up then, so that the agent that takes the
// place of this one does not have to wait for a TTL. A lock that was lost is said to Run, which stops the
// agent: nothing is written without it (NFR-1).
func (a *agent) keepLock(ctx context.Context, held *lock.Lock) {
	if err := held.Keep(ctx); err != nil {
		a.lose(err)
		return
	}
	// The agent was told to stop. What it writes with may still be stopping, so the lock is given up
	// once that has ended. If it does not end in the grace period, the lock is left to run out.
	<-a.idle
	if err := held.Release(); err != nil {
		a.Logger.Warn("the writer lock could not be given up, and is taken over after its TTL", "error", err)
	}
}

// lose says to Run that the writer lock was lost. It does not wait: the keeper and a pull may both find
// that it is gone, and one saying is enough.
func (a *agent) lose(err error) {
	select {
	case a.lost <- err:
	default:
	}
}

// fence checks that the agent still holds the writer lock. A pull calls it immediately before it writes
// (docs/adr/0020), and does not write when it returns an error. A lock that is gone is said to Run as well,
// so that the agent stops without waiting for the next beat of the keeper. With no lock to hold there is
// nothing to check.
func (a *agent) fence(context.Context) error {
	held := a.held.Load()
	if held == nil {
		return nil
	}
	err := held.Check()
	if errors.Is(err, lock.ErrLost) {
		a.lose(err)
	}
	return err
}
