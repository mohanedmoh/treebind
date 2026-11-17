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

// Package settle says which of the changes that a scan finds are done: a file is settled when what the
// scan saw of it has not changed for the settle time, so that a file that is still being written is not
// committed half done (FR-3, R-10).
//
// The time is the agent's own. The tracker is given the time of each scan by its caller, and compares
// those, and never reads a time that the volume reports: a file server's clock is not the agent's, and
// the modification time of a file can be set to anything. What the volume reports is only compared with
// itself, between scans, as what a scan saw of a file (package scan).
//
// Each file settles on its own. A file that keeps changing, a log, waits and does not hold back the files
// that have stopped; the price is that what an application writes over a few seconds can be split over
// more than one commit. A deletion settles like any other change, so the delete half of an atomic save,
// which is followed by the file again, is not committed.
package settle

import (
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/transient"
)

// Observation is what the tracker says of the changes of one scan.
type Observation struct {
	// Settled are the changes that have not changed for the settle time, in the order of the scan.
	Settled []scan.Change
	// Waiting are the changes that have, in the order of the scan.
	Waiting []scan.Change
	// Next is how long until the first of the waiting changes settles if nothing changes it, or zero when
	// nothing is waiting. It is when to look again.
	Next time.Duration
}

// Tracker follows the changes from scan to scan. It is not safe for concurrent use: the agent has one
// loop, and the scans are in it.
type Tracker struct {
	settle time.Duration
	// since is, for each file that has a change, the change as it was last seen and the time of the scan
	// that first saw it so.
	since map[string]entry
}

type entry struct {
	change scan.Change
	at     time.Time
}

// New returns a Tracker for a settle time. A settle time that is not positive settles every change at once.
func New(settle time.Duration) *Tracker {
	return &Tracker{settle: settle, since: map[string]entry{}}
}

// Len is how many files the tracker holds a change of.
func (t *Tracker) Len() int { return len(t.since) }

// Observe takes the changes of a scan that was made at now, and says which are settled. A change that is
// the same as it was at the last scan, in what it is and in what the scan saw of the file, keeps the time
// it had. Any other change, a file that was not there or that was written again or deleted, starts its
// quiet period at now. A file that has no change in this scan is forgotten: it was committed, or put back.
func (t *Tracker) Observe(now time.Time, changes []scan.Change) Observation {
	var o Observation
	next := make(map[string]entry, len(changes))
	for _, c := range changes {
		e := entry{change: c, at: now}
		if prev, ok := t.since[c.Path]; ok && prev.change == c {
			e.at = prev.at
		}
		next[c.Path] = e
		if transient.Settled(e.at, now, t.settle) {
			o.Settled = append(o.Settled, c)
			continue
		}
		o.Waiting = append(o.Waiting, c)
		if left := t.settle - now.Sub(e.at); o.Next == 0 || left < o.Next {
			o.Next = left
		}
	}
	t.since = next
	return o
}
