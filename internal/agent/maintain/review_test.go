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

package maintain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// unreachable adds blobs that nothing reaches, which a repack of the loose objects leaves loose.
func (r *repository) unreachable(n int) {
	r.t.Helper()
	for i := range n {
		r.hash(fmt.Sprintf("nothing reaches this %d\n", i))
	}
}

// A repack of the loose objects packs only those that something reaches, so loose objects that nothing reaches stay, and would
// be due again at the next pull. They are put in one pack with the rest.
func TestLooseObjectsThatNothingReachesArePutInOnePackWhenTheyAreStillAtTheLimit(t *testing.T) {
	r := newRepository(t)
	r.unreachable(5)
	before, parked := r.objects(), r.parked()

	got, err := r.maintainer(func(o *Options) { o.LooseLimit = 4 }).Maintain(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Consolidated || got.After.Loose != 0 || got.Remaining {
		t.Errorf("the maintenance = %+v, want every object in one pack and nothing left at the limit", got)
	}
	r.usable(before, parked)
}

// Without the room for one pack the loose objects that nothing reaches stay, and the agent is told that the limit is still
// reached, so that it does not repack at every pull. The note counts what was packed, and not what was there.
func TestLooseObjectsThatNothingReachesAndNoRoomForOnePackLeaveTheLimitReached(t *testing.T) {
	r := newRepository(t)
	r.unreachable(5)
	s, err := survey(r.l.GitDir)
	if err != nil {
		t.Fatal(err)
	}
	m := r.maintainer(func(o *Options) {
		o.LooseLimit = 4
		o.Free = func(string) (int64, error) {
			return s.looseSize + int64(s.Loose)*indexPerObject + fixedPerPack + DefaultReserve, nil
		}
	})

	got, err := m.Maintain(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Packed || !got.Remaining || got.After.Loose < 4 {
		t.Fatalf("the maintenance = %+v, want the reachable objects packed and the limit still reached", got)
	}
	packedCount := got.Before.Loose - got.After.Loose
	if note := got.String(); !strings.Contains(note, fmt.Sprintf("packed %d loose objects", packedCount)) ||
		!strings.Contains(note, fmt.Sprintf("%d loose objects that nothing reaches", got.After.Loose)) {
		t.Errorf("the note %q, want what was packed, %d, and what is left", note, packedCount)
	}
}

func writeMarker(t *testing.T, r *repository, at time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.l.StateDir, markerName), []byte(strconv.FormatInt(at.Unix(), 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A repack that was cut off with the agent can leave a partial pack that keeps the claim full, and the start removes only what has
// not changed for 31 minutes. A maintenance that is held removes it, behind the fence.
func TestAMaintenanceThatIsHeldRemovesWhatTheRepackThatWasCutOffLeft(t *testing.T) {
	r := newRepository(t)
	now := time.Unix(1_800_000_000, 0)
	writeMarker(t, r, now.Add(-time.Minute))
	left := filepath.Join(r.l.GitDir, "objects", "pack", "tmp_pack_Ab12Cd")
	if err := os.WriteFile(left, []byte("half a pack"), 0o600); err != nil {
		t.Fatal(err)
	}

	lost := errors.New("the lock is lost")
	got, err := r.maintainer(func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Fence = func(context.Context) error { return lost }
	}).Maintain(context.Background())
	if !errors.Is(err, lost) || !gittest.Exists(left) {
		t.Fatalf("with the fence failing the maintenance = %+v, %v, want the error of the fence and nothing removed", got, err)
	}

	got, err = r.maintainer(func(o *Options) { o.Now = func() time.Time { return now } }).Maintain(context.Background())

	if err != nil || got.Outcome != Held {
		t.Fatalf("the maintenance = %+v, %v, want it held", got, err)
	}
	if gittest.Exists(left) || len(got.Removed) != 1 {
		t.Errorf("removed %v, want the partial pack removed", got.Removed)
	}
}

// The out-of-memory killer can take the repack or the pack-objects it started and not the agent. The agent did not send that
// SIGKILL, so the repack would be killed again: the maintenance is held as for one that was cut off with the agent.
func TestARepackThatIsKilledByAnotherIsHeldForADay(t *testing.T) {
	for name, follow := range map[string]bool{"the repack": false, "pack-objects": true} {
		t.Run(name, func(t *testing.T) {
			r := newRepository(t)
			now := time.Unix(1_800_000_000, 0)
			killed := r.maintainer(func(o *Options) {
				o.Git.Git = straceGit(t, follow, callRename, 1)
				o.Now = func() time.Time { return now }
			})
			if _, err := killed.Maintain(context.Background()); err == nil {
				t.Fatal("the maintenance passed, want the killed repack to fail it")
			}

			got, err := r.maintainer(func(o *Options) { o.Now = func() time.Time { return now.Add(time.Hour) } }).Maintain(context.Background())

			if err != nil || got.Outcome != Held || !got.Since.Equal(now) {
				t.Errorf("the next maintenance = %+v, %v, want it held since the kill", got, err)
			}
		})
	}
}

// The marker is the agent's own write, and is removed only behind the fence: an agent that has lost its lock leaves it, and one
// whose fence fails at the next maintenance does not remove an old one.
func TestTheMarkerIsRemovedOnlyBehindTheFence(t *testing.T) {
	r := newRepository(t)
	now := time.Unix(1_800_000_000, 0)
	writeMarker(t, r, now.Add(-48*time.Hour))
	lost := errors.New("the lock is lost")
	_, err := r.maintainer(func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Fence = func(context.Context) error { return lost }
	}).Maintain(context.Background())
	if !errors.Is(err, lost) || !gittest.Exists(filepath.Join(r.l.StateDir, markerName)) {
		t.Errorf("the maintenance = %v, and the marker is gone, want the error of the fence and the marker kept", err)
	}

	r = newRepository(t)
	calls := 0
	_, _ = r.maintainer(func(o *Options) {
		o.Fence = func(context.Context) error {
			if calls++; calls > 1 {
				return lost
			}
			return nil
		}
	}).Maintain(context.Background())
	if !gittest.Exists(filepath.Join(r.l.StateDir, markerName)) {
		t.Error("the marker was removed after the lock was lost")
	}
}

func TestTheNoteNamesAFewOfTheFilesThatWereRemoved(t *testing.T) {
	removed := make([]string, 0, 30)
	for i := range 30 {
		removed = append(removed, fmt.Sprintf("objects/pack/tmp_pack_%02d", i))
	}
	note := Result{Outcome: Packed, Removed: removed}.String()
	if !strings.Contains(note, "tmp_pack_00") || strings.Contains(note, "tmp_pack_29") || !strings.Contains(note, "and 20 more") {
		t.Errorf("the note %q, want ten names and how many more", note)
	}
}
