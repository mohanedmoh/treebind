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

// Package lock is the writer lock of a checkout: it lets at most one agent write
// to a volume at a time, also when a pod is replaced, when two pods overlap and
// when a pod is frozen and wakes up later (NFR-1, NFR-17).
//
// The lock is a directory on the volume that holds a chain of numbered slots. A
// slot is a file made with an exclusive create and never changed afterwards.
// Whoever made the highest slot holds the lock. The holder's heartbeat makes the
// next slot and a takeover makes that same slot, so the two meet in one exclusive
// create and the file system lets one of them win. Nothing here uses flock or
// fcntl locks, which network file systems do not keep reliably, and nothing is
// renamed (docs/adr/0012).
//
// A contender takes over only after it has seen the same highest slot for a whole
// TTL on its own clock. It never reads a time that another machine wrote.
//
// The holder calls Check before every commit, apply and push. A holder that was
// frozen while another agent took over finds out there, before it writes.
package lock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// The ways a lock ends, and the one reason Acquire refuses to wait.
var (
	// ErrLost means another agent holds the lock now, or that this one could not
	// renew it for a whole TTL and has to assume so. It is final: the Lock never
	// passes a Check again, and the agent has to stop writing and acquire anew.
	ErrLost = errors.New("lock: lost to another holder")
	// ErrReleased is what a Lock answers once Release has given it up.
	ErrReleased = errors.New("lock: released")
	// ErrNewerVersion means the lock is held in a format this agent cannot read.
	// How such a holder shows that it is alive is unknown, so it is never taken
	// over.
	ErrNewerVersion = errors.New("lock: held by a newer version of Treebind")
)

// maxHolder bounds a holder's name, so that a record always fits in maxRecord.
const maxHolder = 256

// Clock is where the lock gets its time. Durations are measured on it and never
// against a time from the volume, so it has to be this process's own clock.
type Clock interface {
	// Now is the current time. The system clock's carries a monotonic reading.
	Now() time.Time
	// After returns a channel that delivers once d has passed.
	After(d time.Duration) <-chan time.Time
}

// systemClock is the clock of the process.
type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config says where the lock lives and how patient it is. Every agent that
// shares a checkout has to use the same Dir and the same TTL.
type Config struct {
	// Dir is the directory of the lock, on the volume of the checkout it guards.
	// The lock owns it: it is created if it is missing (its parent is not), and
	// nothing else may write there.
	Dir string
	// Holder names this agent in the record, for logs and for the agents that
	// wait: the pod's name and something that differs between two starts of it.
	// Exclusion does not depend on it.
	Holder string
	// TTL is how long a contender has to see no heartbeat before it takes over.
	TTL time.Duration
	// Interval is how often the holder renews and a contender looks. It is a
	// third of the TTL when zero, and at most half of it, so that one late
	// heartbeat does not make a live holder look gone.
	Interval time.Duration
	// Clock is the system clock when nil.
	Clock Clock
}

// checked fills in the defaults and refuses a configuration that cannot work.
func (c Config) checked() (Config, error) {
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	if c.Interval == 0 {
		c.Interval = c.TTL / 3
	}
	switch {
	case c.Dir == "":
		return c, errors.New("lock: no directory is set")
	case c.Holder == "" || len(c.Holder) > maxHolder:
		return c, fmt.Errorf("lock: the holder's name must have 1 to %d bytes", maxHolder)
	case c.TTL <= 0:
		return c, errors.New("lock: the TTL must be positive")
	case c.Interval <= 0 || c.Interval > c.TTL/2:
		return c, errors.New("lock: the interval must be positive and at most half the TTL")
	}
	return c, nil
}

// Lock is a held lock. Its methods may be called from several goroutines.
type Lock struct {
	cfg   Config
	epoch uint64

	mu sync.Mutex
	// last is the highest slot this holder made.
	last uint64
	// renewed is when a slot was last made and seen to be the highest.
	renewed time.Time
	// ended is ErrLost or ErrReleased once the lock is no longer held.
	ended error
}

// Acquire waits until the lock is free and takes it. The lock is free when
// nobody ever held it, when its holder released it, or when this process has
// seen no heartbeat for a TTL. Agents that wait here are the standby replicas.
//
// It returns the context's error when the context ends first, ErrNewerVersion
// when the holder's record is of a newer format, and any failure of the volume.
// A volume that let the slot be made and then refused its record leaves a slot
// that nobody holds, which is taken over after a TTL like any other.
func Acquire(ctx context.Context, cfg Config) (*Lock, error) {
	cfg, err := cfg.checked()
	if err != nil {
		return nil, err
	}
	// Under the agent's umask (internal/agent/umask) the group may write to the
	// directory, so that agents that reach the volume as different users of one
	// group can all make slots. A directory that exists keeps its mode.
	if err := os.Mkdir(cfg.Dir, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("lock: create the lock directory: %w", err)
	}

	var seen watch
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slots, err := listSlots(cfg.Dir)
		if err != nil {
			return nil, err
		}
		top := highest(slots)
		free, err := seen.free(cfg, top)
		if err != nil {
			return nil, err
		}
		if free {
			if l, err := claim(cfg, top+1); l != nil || err != nil {
				return l, err
			}
		}
		select {
		case <-ctx.Done():
		case <-cfg.Clock.After(cfg.Interval):
		}
	}
}

// watch is what a contender remembers between two looks at the lock. The zero
// watch has seen no slot for as long as time goes back, so a contender that
// finds a lock nobody ever held takes it at once.
type watch struct {
	// top is the highest slot it saw, or zero for none, and since is when it
	// first saw it there.
	top   uint64
	since time.Time
}

// free reports whether the slot after top may be claimed now. Only the number of
// the highest slot counts as a sign of life: a heartbeat always makes a new one.
// The time in a record is not read, because the clock that wrote it may be wrong.
// Slots that vanish are a change like any other, and start the wait anew.
func (w *watch) free(cfg Config, top uint64) (bool, error) {
	if top > 0 {
		// A record that cannot be read says nothing: its holder is treated as any
		// holder that may still be alive.
		if rec, err := readRecord(cfg.Dir, top); err == nil {
			if rec.Version > Version {
				return false, fmt.Errorf("%w: its record has format %d and this agent reads up to %d",
					ErrNewerVersion, rec.Version, Version)
			}
			if rec.Released {
				return true, nil
			}
		}
	}
	now := cfg.Clock.Now()
	if top != w.top {
		w.top, w.since = top, now
		return false, nil
	}
	return now.Sub(w.since) >= cfg.TTL, nil
}

// claim starts a tenure with slot n. It returns no lock and no error when
// another agent made the slot first, or made a higher one in the meantime.
func claim(cfg Config, n uint64) (*Lock, error) {
	l := &Lock{cfg: cfg, epoch: n, last: n - 1, renewed: cfg.Clock.Now()}
	switch err := l.advance(false); {
	case errors.Is(err, ErrLost):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return l, nil
}

// Epoch is the number of the slot this tenure began with. It is higher than the
// epoch of every earlier holder, so it orders what holders did.
func (l *Lock) Epoch() uint64 { return l.epoch }

// Check proves that the lock is still held, by making the next slot. Call it
// directly before every commit, apply and push, and do not write when it returns
// an error. ErrLost is final. Any other error means the volume did not let the
// lock be renewed this time.
func (l *Lock) Check() error { return l.advance(false) }

// Release gives the lock up, so that the next agent does not have to wait for a
// TTL. Call it when the agent stops cleanly and has finished writing. It returns
// ErrLost, and releases nothing, when another agent holds the lock already.
func (l *Lock) Release() error { return l.advance(true) }

// Keep renews the lock every interval. It returns ErrLost when the lock is lost,
// and nil when the context ends or the lock was released. Run it for as long as
// the lock is held and stop the work when it returns an error. A renewal that
// the volume refused is tried again at the next interval, and after a TTL of
// refusals the lock counts as lost.
func (l *Lock) Keep(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-l.cfg.Clock.After(l.cfg.Interval):
		}
		switch err := l.Check(); {
		case errors.Is(err, ErrReleased):
			return nil
		case errors.Is(err, ErrLost):
			return err
		}
	}
}

// advance makes the next slot and records what that says about the lock.
func (l *Lock) advance(released bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended != nil {
		return l.ended
	}
	before := l.cfg.Clock.Now()
	err := l.makeNext(released)
	switch {
	case err == nil:
		// A contender counts its TTL from when the slot appeared, which is no
		// earlier than this, and not from when the holder was done with it.
		l.renewed = before
	case errors.Is(err, ErrLost):
		l.ended = ErrLost
	case l.ended == nil && l.cfg.Clock.Now().Sub(l.renewed) >= l.cfg.TTL:
		// A contender may have judged this holder gone by now. It cannot prove
		// otherwise, so it gives up instead of holding a lock it cannot renew.
		l.ended = ErrLost
		err = fmt.Errorf("%w: it could not be renewed for %s: %w", ErrLost, l.cfg.TTL, err)
	}
	return err
}

// makeNext makes the slot after the last one and checks that no slot is above
// it. Unless it is a release, the directory is listed only after the slot is
// durable, so that a holder that was slow to write still sees a takeover that
// happened meanwhile.
func (l *Lock) makeNext(released bool) error {
	next := l.last + 1
	if released {
		// The next agent may follow a release at once, so a slot above the release
		// is no loss. A release therefore looks for a loss before it makes its
		// slot, and not after.
		slots, err := listSlots(l.cfg.Dir)
		if err != nil {
			return err
		}
		if highest(slots) > l.last {
			return ErrLost
		}
	}
	made, err := makeSlot(l.cfg.Dir, next, record{
		Version:  Version,
		Epoch:    l.epoch,
		Holder:   l.cfg.Holder,
		Time:     l.cfg.Clock.Now().UTC(),
		Released: released,
	})
	if errors.Is(err, fs.ErrExist) {
		return ErrLost
	}
	if !made {
		return err
	}
	l.last = next
	if released {
		l.ended = ErrReleased
		return err
	}
	if err != nil {
		return err
	}

	// The slot was free, but it may have been used and removed while this agent
	// was frozen. Then a higher slot exists, and its maker holds the lock.
	slots, err := listSlots(l.cfg.Dir)
	if err != nil {
		return err
	}
	if highest(slots) > next {
		return ErrLost
	}
	// The first slot of the tenure stays, so that the holder before this one
	// meets it whenever it wakes up.
	for _, n := range slots {
		if n < next && n != l.epoch {
			_ = os.Remove(slotPath(l.cfg.Dir, n))
		}
	}
	return nil
}
