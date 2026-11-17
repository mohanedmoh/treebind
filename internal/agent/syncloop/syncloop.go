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

// Package syncloop decides when the agent pulls (docs/adr/0024). It is a schedule and a loop and
// knows nothing about Git: what a pull is, and whether it failed, are given to it.
//
// A pull comes every interval, give or take 20%, so that the agents of many Treebinds that started
// together drift apart and do not ask one Git host at the same moment (R-19). The first pull waits a
// phase that comes from a hash of the namespace and the name of the Treebind, so that they do not
// start together either. After a pull that failed the wait grows: it is drawn at random between the
// interval and a ceiling that doubles with each failure in a row, up to ten minutes, and goes back to
// the interval at the first pull that did not fail.
//
// The loop runs one pull at a time and waits for it. That is the whole of the cap on what one agent
// asks of a Git host: it never has two operations that ask the repository running at once, and a
// push that comes later is made in the same loop and not beside it.
//
// A loop that is given a scan for local changes (#34, docs/adr/0029) has a second kind of turn, in the
// same loop and so never beside a pull. The first scan is at once, and the next is a scan interval
// after the end of the one before, or sooner: when the scan says that a file settles before that, at
// the time it does, and when an event of the file system calls for one, after a short gap that keeps
// a program that writes a great deal from being scanned all the time. A pull and a scan that are due
// together are taken pull first. Scans are local, so they have no jitter and no backoff.
package syncloop

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"time"
)

const (
	// JitterFraction is how far the interval is moved either way: a wait is between 80% and 120% of it.
	JitterFraction = 0.2

	// DefaultMaxWait is the longest wait after a failed pull.
	DefaultMaxWait = 10 * time.Minute

	// DefaultScanInterval is the longest wait between two scans for local changes, which is how long a
	// change that the events of the file system did not tell takes to be found. A scan is one call of
	// Git that lists what differs, and the time it takes grows with the number of files.
	DefaultScanInterval = 30 * time.Second

	// DefaultScanGap is the shortest time from the end of a scan to the next one that an event calls for.
	DefaultScanGap = time.Second
)

// Clock is where the loop gets its waits. A test gives its own, so that no test waits.
type Clock interface {
	// After returns a channel that delivers once d has passed.
	After(d time.Duration) <-chan time.Time
}

// WallClock is a Clock that also says what time it is. A loop that has scans needs one, to tell how long
// is left of a wait that an event cut short.
type WallClock interface {
	Clock
	// Now is the time on the clock.
	Now() time.Time
}

type systemClock struct{}

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) Now() time.Time                         { return time.Now() }

// Options say how often to pull and what a pull is.
type Options struct {
	// Interval is how often to pull while pulls do not fail. It has to be positive.
	Interval time.Duration
	// Phase is how long to wait before the first pull: Phase of the namespace and the name.
	Phase time.Duration
	// MaxWait is the longest wait after a failed pull. DefaultMaxWait when it is zero.
	MaxWait time.Duration
	// Clock is the system clock when nil.
	Clock Clock
	// Random returns a number from 0 up to but not including 1, for the jitter. It is the random
	// source of the process when nil. The jitter is for spreading the load, and is not secret.
	Random func() float64
	// Pull runs one pull, and says whether it failed in a way that a longer wait is for: the
	// repository did not answer, or the volume did not let it write. A pull that found something
	// that a person has to put right, and that the repository answered, did not fail in this sense.
	// It is called again when its context is not done, whatever it returned.
	Pull func(ctx context.Context) (failed bool)

	// Scan, when it is set, is a turn of the loop beside the pull: it looks for local changes, at the time
	// now, which is the time on the clock when the scan began, and says how long until a change that it
	// found settles, or zero when none is waiting. That is when the next scan is due, if it is sooner
	// than a scan interval. A loop with a Scan needs a Clock that is a WallClock.
	Scan func(ctx context.Context, now time.Time) (next time.Duration)
	// ScanInterval is the longest wait between two scans. DefaultScanInterval when it is zero.
	ScanInterval time.Duration
	// ScanGap is the shortest time from the end of a scan to the next one that an event calls for.
	// DefaultScanGap when it is zero.
	ScanGap time.Duration
	// Wake delivers a signal when something may have changed, which calls for a scan sooner than the
	// interval. It can be nil, and the loop does without it when it is closed.
	Wake <-chan struct{}
}

// Run pulls until ctx is done, which is when it returns nil. It returns an error only for options that
// cannot work, and then it has not pulled.
func Run(ctx context.Context, o Options) error {
	if o.Interval <= 0 {
		return errors.New("syncloop: the interval must be positive")
	}
	if o.Pull == nil {
		return errors.New("syncloop: there is no pull to run")
	}
	if o.MaxWait <= 0 {
		o.MaxWait = DefaultMaxWait
	}
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Random == nil {
		o.Random = rand.Float64
	}
	if o.Scan != nil {
		return runWithScans(ctx, o)
	}

	wait := o.Phase
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-o.Clock.After(wait):
		}
		// A wait that has passed and a stop that has come are both ready at times, and the stop wins.
		if ctx.Err() != nil {
			return nil
		}
		failed := o.Pull(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if failed {
			failures++
		} else {
			failures = 0
		}
		wait = Next(o.Interval, o.MaxWait, failures, o.Random())
	}
}

// Next returns how long to wait before the next pull, after the number of pulls that failed in a row,
// which is zero after one that did not. random is a number from 0 up to but not including 1.
func Next(interval, maxWait time.Duration, failures int, random float64) time.Duration {
	if failures <= 0 {
		return Jitter(interval, random)
	}
	return Backoff(interval, maxWait, failures, random)
}

// Jitter moves the interval by up to JitterFraction either way: by the most downwards when random is 0,
// and almost by the most upwards when it is just below 1.
func Jitter(interval time.Duration, random float64) time.Duration {
	return time.Duration(float64(interval) * (1 + JitterFraction*(2*random-1)))
}

// Backoff is the wait after the given number of failed pulls in a row, which is one or more. It is
// drawn over the whole length between the interval and a ceiling that is the interval doubled once
// for each failure, up to maxWait. A failing pull is not asked again sooner than the interval, and
// agents that fail together are not asked again together. An interval that is longer than maxWait has
// no room to grow, and is the wait, moved by the jitter as every other is: a failure does not make it
// shorter.
func Backoff(interval, maxWait time.Duration, failures int, random float64) time.Duration {
	ceiling := interval
	for range failures {
		if ceiling >= maxWait {
			break
		}
		ceiling *= 2
	}
	ceiling = max(interval, min(ceiling, maxWait))
	if ceiling == interval {
		return Jitter(interval, random)
	}
	return interval + time.Duration(random*float64(ceiling-interval))
}

// Phase is how long the first pull of a Treebind waits: a point in the interval that comes from a
// hash of its namespace and name, the same at every start of its agent, and different for different
// Treebinds, so that agents that start together do not pull together.
func Phase(namespace, name string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	// The separator is not allowed in a namespace or in a name, so that two pairs do not make one string.
	_, _ = h.Write([]byte(namespace + "/" + name))
	return time.Duration(h.Sum64() % uint64(interval))
}

// runWithScans is Run for a loop that has scans. Each kind of turn has the time it is due, and the loop
// waits for the earlier of the two, or for an event, which may make the scan due sooner.
func runWithScans(ctx context.Context, o Options) error {
	clock, ok := o.Clock.(WallClock)
	if !ok {
		return errors.New("syncloop: a loop with scans needs a clock that has Now")
	}
	if o.ScanInterval <= 0 {
		o.ScanInterval = DefaultScanInterval
	}
	if o.ScanGap <= 0 {
		o.ScanGap = DefaultScanGap
	}

	now := clock.Now()
	pullAt, scanAt := now.Add(o.Phase), now
	var lastScanEnd time.Time
	failures := 0
	wake := o.Wake
	for {
		now = clock.Now()
		due := pullAt
		if scanAt.Before(due) {
			due = scanAt
		}
		select {
		case <-ctx.Done():
			return nil
		case <-clock.After(max(due.Sub(now), 0)):
		case _, open := <-wake:
			if !open {
				wake = nil
				continue
			}
			// Something may have changed: scan once the gap since the last scan has passed, if that is
			// sooner than the scan was due. The events that come meanwhile are one scan.
			soonest := now
			if gapEnds := lastScanEnd.Add(o.ScanGap); gapEnds.After(soonest) {
				soonest = gapEnds
			}
			if soonest.Before(scanAt) {
				scanAt = soonest
			}
			continue
		}
		// A wait that has passed and a stop that has come are both ready at times, and the stop wins.
		if ctx.Err() != nil {
			return nil
		}

		now = clock.Now()
		pullDue, scanDue := !pullAt.After(now), !scanAt.After(now)
		switch {
		case pullDue && (!scanDue || !scanAt.Before(pullAt)):
			failed := o.Pull(ctx)
			if ctx.Err() != nil {
				return nil
			}
			if failed {
				failures++
			} else {
				failures = 0
			}
			pullAt = clock.Now().Add(Next(o.Interval, o.MaxWait, failures, o.Random()))
		case scanDue:
			started := now
			next := o.Scan(ctx, started)
			if ctx.Err() != nil {
				return nil
			}
			lastScanEnd = clock.Now()
			scanAt = lastScanEnd.Add(o.ScanInterval)
			if settles := started.Add(next); next > 0 && settles.Before(scanAt) {
				// The file settles when the scan began plus next. A scan of a large tree can end after that, and
				// scans that follow one another with nothing between them are all that the tree would do, so
				// the next is not sooner than the gap after the end of this one.
				scanAt = settles
				if floor := lastScanEnd.Add(o.ScanGap); scanAt.Before(floor) {
					scanAt = floor
				}
			}
		}
	}
}
