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

package settle

import (
	"slices"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

const quiet = 5 * time.Second

var start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// file is a change of a file as a scan saw it, with a size that stands for what was seen.
func file(path string, size int64) scan.Change {
	return scan.Change{Path: path, Kind: scan.Modified, Seen: scan.Seen{Present: true, Size: size, ModTime: size, ChangeTime: size}}
}

func gone(path string) scan.Change { return scan.Change{Path: path, Kind: scan.Deleted} }

func paths(changes []scan.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

func wantPaths(t *testing.T, what string, got []scan.Change, want ...string) {
	t.Helper()
	if have := paths(got); !slices.Equal(have, want) {
		t.Errorf("%s = %q, want %q", what, have, want)
	}
}

func TestAFileSettlesWhenWhatWasSeenOfItHasNotChangedForTheSettleTime(t *testing.T) {
	tr := New(quiet)
	f := file("a.txt", 1)

	o := tr.Observe(start, []scan.Change{f})
	wantPaths(t, "settled at first sight", o.Settled)
	wantPaths(t, "waiting at first sight", o.Waiting, "a.txt")

	o = tr.Observe(start.Add(quiet-time.Nanosecond), []scan.Change{f})
	wantPaths(t, "settled just before the time", o.Settled)

	o = tr.Observe(start.Add(quiet), []scan.Change{f})
	wantPaths(t, "settled at the time", o.Settled, "a.txt")
	wantPaths(t, "waiting at the time", o.Waiting)
}

func TestAFileThatIsWrittenAgainStartsItsQuietPeriodAgain(t *testing.T) {
	tr := New(quiet)
	tr.Observe(start, []scan.Change{file("a.txt", 1)})
	tr.Observe(start.Add(4*time.Second), []scan.Change{file("a.txt", 2)}) // written again

	o := tr.Observe(start.Add(quiet), []scan.Change{file("a.txt", 2)})
	wantPaths(t, "settled five seconds after the first sight", o.Settled)
	o = tr.Observe(start.Add(4*time.Second+quiet), []scan.Change{file("a.txt", 2)})
	wantPaths(t, "settled five seconds after the second", o.Settled, "a.txt")
}

func TestAFileThatKeepsChangingDoesNotHoldBackTheFilesThatStopped(t *testing.T) {
	tr := New(quiet)
	var o Observation
	for i := range 10 {
		now := start.Add(time.Duration(i) * time.Second)
		o = tr.Observe(now, []scan.Change{file("app.log", int64(i)), file("done.txt", 1)})
	}
	// done.txt has been the same for nine seconds, and app.log for none.
	wantPaths(t, "settled", o.Settled, "done.txt")
	wantPaths(t, "waiting", o.Waiting, "app.log")
}

func TestADeletionSettlesLikeAnyOtherChange(t *testing.T) {
	// The delete half of an atomic save is not one that is committed: the file is back before it settles.
	tr := New(quiet)
	tr.Observe(start, []scan.Change{gone("a.txt")})
	o := tr.Observe(start.Add(2*time.Second), []scan.Change{file("a.txt", 3)}) // back, with other content
	wantPaths(t, "settled", o.Settled)
	wantPaths(t, "waiting", o.Waiting, "a.txt")
	if got := o.Waiting[0].Kind; got != scan.Modified {
		t.Errorf("the file that came back is %s, want it as the scan saw it", got)
	}

	tr = New(quiet)
	tr.Observe(start, []scan.Change{gone("b.txt")})
	o = tr.Observe(start.Add(quiet), []scan.Change{gone("b.txt")})
	wantPaths(t, "settled deletion", o.Settled, "b.txt")
}

func TestAKindThatChangesWithTheSameSightStartsTheQuietPeriodAgain(t *testing.T) {
	// A pull can move the commit under a file that did not change: what it is against changes, and so
	// does what is to be committed.
	tr := New(quiet)
	added := scan.Change{Path: "a.txt", Kind: scan.Added, Seen: scan.Seen{Present: true, Size: 1}}
	modified := added
	modified.Kind = scan.Modified
	tr.Observe(start, []scan.Change{added})
	o := tr.Observe(start.Add(quiet), []scan.Change{modified})
	wantPaths(t, "settled", o.Settled)
	wantPaths(t, "waiting", o.Waiting, "a.txt")
}

func TestAFileThatIsNoLongerChangedIsForgotten(t *testing.T) {
	tr := New(quiet)
	tr.Observe(start, []scan.Change{file("a.txt", 1)})
	tr.Observe(start.Add(2*time.Second), nil) // committed, or put back by hand

	// The same sight of it again is a first sight, and not one that has lasted.
	o := tr.Observe(start.Add(quiet), []scan.Change{file("a.txt", 1)})
	wantPaths(t, "settled", o.Settled)
	wantPaths(t, "waiting", o.Waiting, "a.txt")
	if tr.Len() != 1 {
		t.Errorf("Len = %d, want 1", tr.Len())
	}
	tr.Observe(start.Add(quiet), nil)
	if tr.Len() != 0 {
		t.Errorf("Len = %d after a scan with no change, want 0: nothing is kept for a file that is clean", tr.Len())
	}
}

func TestASettledFileStaysSettledWhileItIsStillThere(t *testing.T) {
	// The commit of it failed, and the next scan finds it again.
	tr := New(quiet)
	f := file("a.txt", 1)
	tr.Observe(start, []scan.Change{f})
	for _, at := range []time.Duration{quiet, 2 * quiet, time.Hour} {
		o := tr.Observe(start.Add(at), []scan.Change{f})
		wantPaths(t, "settled", o.Settled, "a.txt")
	}
}

func TestTheNextTimeToLookIsWhenTheFirstWaitingFileSettles(t *testing.T) {
	tr := New(quiet)
	tr.Observe(start, []scan.Change{file("a.txt", 1)})
	o := tr.Observe(start.Add(3*time.Second), []scan.Change{file("a.txt", 1), file("b.txt", 1)})
	if o.Next != 2*time.Second {
		t.Errorf("Next = %v, want 2s: a.txt settles in two seconds, b.txt in five", o.Next)
	}

	o = tr.Observe(start.Add(quiet), []scan.Change{file("a.txt", 1), file("b.txt", 1)})
	if o.Next != 3*time.Second {
		t.Errorf("Next = %v, want 3s once a.txt has settled and only b.txt waits", o.Next)
	}

	o = tr.Observe(start.Add(2*quiet), []scan.Change{file("a.txt", 1), file("b.txt", 1)})
	if o.Next != 0 {
		t.Errorf("Next = %v, want 0 when nothing waits", o.Next)
	}
	o = tr.Observe(start.Add(2*quiet), nil)
	if o.Next != 0 || len(o.Settled) != 0 || len(o.Waiting) != 0 {
		t.Errorf("an empty scan gave %+v, want nothing", o)
	}
}

func TestTheOrderOfTheScanIsKept(t *testing.T) {
	tr := New(quiet)
	changes := []scan.Change{file("a", 1), file("b", 1), file("c", 1), file("d", 1)}
	tr.Observe(start, changes[:2])
	o := tr.Observe(start.Add(quiet), changes)
	wantPaths(t, "settled", o.Settled, "a", "b")
	wantPaths(t, "waiting", o.Waiting, "c", "d")
}

func TestASettleTimeThatIsNotPositiveSettlesAtOnce(t *testing.T) {
	tr := New(0)
	o := tr.Observe(start, []scan.Change{file("a.txt", 1)})
	wantPaths(t, "settled", o.Settled, "a.txt")
	if o.Next != 0 {
		t.Errorf("Next = %v, want 0", o.Next)
	}
}
