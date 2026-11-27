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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/maintain"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

// fakeMaintainer is due when the test says, and answers each maintenance with what the test says.
type fakeMaintainer struct {
	mu     sync.Mutex
	due    bool
	result maintain.Result
	err    error
	runs   int
}

func (f *fakeMaintainer) Due() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.due, nil
}

func (f *fakeMaintainer) Maintain(context.Context) (maintain.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return f.result, f.err
}

func (f *fakeMaintainer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// withMaintainer gives the agent the maintainer, and returns the options that it was made with.
func withMaintainer(m *fakeMaintainer) (func(*Options), *atomic.Pointer[maintain.Options]) {
	made := &atomic.Pointer[maintain.Options]{}
	return func(o *Options) {
		o.NewMaintainer = func(asked maintain.Options) (Maintainer, error) {
			made.Store(&asked)
			return m, nil
		}
	}, made
}

func packed() maintain.Result {
	return maintain.Result{Outcome: maintain.Packed, Before: maintain.Store{Loose: 6800, Bytes: 30 << 20}, After: maintain.Store{Bytes: 3 << 20}}
}

func TestAPullThatPassedIsFollowedByTheMaintenanceOfTheGitDirectoryWhenItIsDue(t *testing.T) {
	f := newFakePuller()
	m := &fakeMaintainer{due: true, result: packed()}
	change, _ := withPuller(f, &loopClock{})
	maintained, made := withMaintainer(m)
	r := start(t, func(o *Options) { change(o); maintained(o) })
	f.awaitBegun(t)

	f.answer(t, advanced(testCommit, otherCommit))

	if m.count() != 1 {
		t.Fatalf("the maintenance ran %d times after a pull that passed, want once", m.count())
	}
	got, _ := pullCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced the checkout") ||
		!strings.Contains(got.Message, "packed 6800 loose objects of the Git directory") {
		t.Errorf("pull = %+v, want it passed, and the maintenance said after it", got)
	}
	options := made.Load()
	if options.Fence == nil || options.Layout.GitDir != gitDirOfTheTests || options.Git.Running == nil {
		t.Errorf("the maintainer was made with %+v, want the fence, the layout and the Runner options of the agent", options)
	}

	// Not due: nothing runs.
	m.mu.Lock()
	m.due = false
	m.mu.Unlock()
	f.answer(t, upToDate(otherCommit))
	if m.count() != 1 {
		t.Errorf("the maintenance ran %d times, want no more while it is not due", m.count())
	}
}

func TestAPullThatFailedIsNotFollowedByTheMaintenance(t *testing.T) {
	f := newFakePuller()
	m := &fakeMaintainer{due: true, result: packed()}
	change, _ := withPuller(f, &loopClock{})
	maintained, _ := withMaintainer(m)
	start(t, func(o *Options) { change(o); maintained(o) })
	f.awaitBegun(t)

	f.answer(t, failedPull(pull.ReasonFetchFailed, "the repository cannot be reached"))

	if m.count() != 0 {
		t.Errorf("the maintenance ran %d times after a pull that failed, want none", m.count())
	}
}

// A maintenance that failed, or found no room, or found less to pack than the look at objects/17 guessed, is tried again an hour
// later and not at the next pull, which is 30 seconds later.
func TestAMaintenanceThatFailedIsNotTriedAgainForAnHour(t *testing.T) {
	for name, m := range map[string]*fakeMaintainer{
		"failed":  {due: true, err: errors.New("the Git directory could not be packed: git repack failed with exit status 128")},
		"no room": {due: true, result: maintain.Result{Outcome: maintain.NoRoom, Need: 20 << 20, Free: 1 << 20}},
		"not due": {due: true, result: maintain.Result{Outcome: maintain.NotDue}},
	} {
		t.Run(name, func(t *testing.T) {
			var now atomic.Pointer[time.Time]
			base := time.Unix(1_800_000_000, 0)
			now.Store(&base)
			f := newFakePuller()
			change, _ := withPuller(f, &loopClock{})
			maintained, _ := withMaintainer(m)
			r := start(t, func(o *Options) {
				change(o)
				maintained(o)
				o.Now = func() time.Time { return *now.Load() }
			})
			f.awaitBegun(t)

			f.answer(t, upToDate(testCommit))
			got, _ := pullCheck(t, r)
			if got.Outcome != outcomePassed || (name != "not due") != strings.Contains(got.Message, "Git directory") {
				t.Errorf("pull = %+v, want it passed, with what the maintenance said", got)
			}
			f.answer(t, upToDate(testCommit))
			if m.count() != 1 {
				t.Errorf("the maintenance ran %d times, want once: the next is an hour later", m.count())
			}

			later := base.Add(time.Hour)
			now.Store(&later)
			f.answer(t, upToDate(testCommit))
			if m.count() != 2 {
				t.Errorf("the maintenance ran %d times an hour later, want twice", m.count())
			}
		})
	}
}
