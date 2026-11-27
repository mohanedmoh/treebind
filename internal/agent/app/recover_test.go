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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

// A pull that fails after it began an update leaves the journal of the update, and the commits wait for it to be put right. The
// agent puts it right at once and does not leave it to the next pull, which comes after the longer wait of a pull that failed
// (docs/adr/0028, #246).

const recoveredNote = "the update from 0123456789ab to fedcba987654 was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed"

// recoveringPuller is a fakePuller that can also put an update right, as the puller of package pull can.
type recoveringPuller struct {
	*fakePuller
	note  string
	err   error
	calls atomic.Int32
}

func (p *recoveringPuller) Recover(context.Context) (string, error) {
	p.calls.Add(1)
	return p.note, p.err
}

// cutPull is the failure of a pull whose update was begun and failed in the middle of its files.
func cutPull(message string) answer {
	return answer{err: &pull.Error{Reason: pull.ReasonPullFailed, Message: message, Cut: true}}
}

// claimOf makes a claim, with the journal of an update that was cut off when journal is set.
func claimOf(t *testing.T, journal bool) string {
	t.Helper()
	claim := gittest.TempDir(t)
	if journal {
		path := filepath.Join(claim, config.StateDirName, "updating")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return claim
}

// recovering starts an agent whose pulls are the puller's, with a claim that has the journal of an update that was cut off when
// journal is set.
func recovering(t *testing.T, p *recoveringPuller, journal bool) *running {
	t.Helper()
	claim := claimOf(t, journal)
	change, _ := withPuller(p.fakePuller, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Config.MountPath = claim
		o.NewPuller = func(pull.Options) (Puller, error) { return p, nil }
	})
	p.awaitBegun(t)
	return r
}

func TestAPullThatFailedAfterItBeganAnUpdateIsFollowedAtOnceByTheRepairOfIt(t *testing.T) {
	p := &recoveringPuller{fakePuller: newFakePuller(), note: recoveredNote}
	r := recovering(t, p, true)

	p.answer(t, cutPull("the update could not be applied"))

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonPullFailed ||
		!strings.Contains(got.Message, "the update could not be applied") || !strings.Contains(got.Message, recoveredNote) {
		t.Errorf("pull = %+v, want it failed, with the failure and what the repair did said", got)
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("the update was put right %d times, want once", n)
	}
}

func TestAPullThatFailedForAnotherCauseThanAnUpdateIsNotFollowedByARepair(t *testing.T) {
	p := &recoveringPuller{fakePuller: newFakePuller(), note: recoveredNote}
	r := recovering(t, p, true)

	// A journal is there, from an update that was cut off before, and this pull did not begin an update: its own repair failed.
	p.answer(t, failedPull(pull.ReasonPullFailed, "the journal cannot be removed"))

	if got, _ := pullCheck(t, r); strings.Contains(got.Message, recoveredNote) {
		t.Errorf("pull = %+v, want no second repair said", got)
	}
	if n := p.calls.Load(); n != 0 {
		t.Errorf("the update was put right %d times, want none: the cause of this failure is still there", n)
	}
}

func TestAPullThatFailedWithNoJournalLeftHasNothingToPutRight(t *testing.T) {
	p := &recoveringPuller{fakePuller: newFakePuller(), note: recoveredNote}
	recovering(t, p, false)

	p.answer(t, cutPull("the update could not be applied"))

	if n := p.calls.Load(); n != 0 {
		t.Errorf("the update was put right %d times, want none: no update was cut off", n)
	}
}

func TestAPullThatPassedIsNotFollowedByARepair(t *testing.T) {
	p := &recoveringPuller{fakePuller: newFakePuller(), note: recoveredNote}
	recovering(t, p, true)

	p.answer(t, advanced(testCommit, otherCommit))

	if n := p.calls.Load(); n != 0 {
		t.Errorf("the update was put right %d times, want none: the pull passed, and puts its own right at the next", n)
	}
}

func TestARepairThatFailsIsSaidInThePullCheckWithWhatItDidBeforeItFailed(t *testing.T) {
	p := &recoveringPuller{
		fakePuller: newFakePuller(),
		note:       "the update was undone: 2 file(s) put back, 1 removed",
		err:        &pull.Error{Reason: pull.ReasonPullFailed, Message: "the journal updating of the update that was cut off cannot be removed"},
	}
	r := recovering(t, p, true)

	p.answer(t, cutPull("the update could not be applied"))

	got, _ := pullCheck(t, r)
	for _, want := range []string{
		"the update could not be applied", "the update was undone: 2 file(s) put back, 1 removed",
		"the journal updating of the update that was cut off cannot be removed",
	} {
		if got.Outcome != outcomeFailed || !strings.Contains(got.Message, want) {
			t.Errorf("pull = %+v, want it failed, and saying %q", got, want)
		}
	}
}

// stopsOnPull is a puller whose pull ends with the failure of an update, as the agent is told to stop.
type stopsOnPull struct {
	release chan struct{}
	cancel  context.CancelFunc
	calls   atomic.Int32
}

func (p *stopsOnPull) Pull(context.Context) (pull.Result, error) {
	<-p.release
	p.cancel()
	return pull.Result{}, &pull.Error{Reason: pull.ReasonPullFailed, Message: "the update could not be applied", Cut: true}
}

func (p *stopsOnPull) Recover(context.Context) (string, error) {
	p.calls.Add(1)
	return recoveredNote, nil
}

func TestAnAgentThatIsToldToStopDoesNotPutTheUpdateRightAndLeavesItToTheNextStart(t *testing.T) {
	p := &stopsOnPull{release: make(chan struct{})}
	change, _ := withPuller(newFakePuller(), &loopClock{})
	claim := claimOf(t, true)
	r := start(t, func(o *Options) {
		change(o)
		o.Config.MountPath = claim
		o.NewPuller = func(pull.Options) (Puller, error) { return p, nil }
	})
	p.cancel = r.cancel
	close(p.release)

	_ = r.waitStopped(t)

	if n := p.calls.Load(); n != 0 {
		t.Errorf("the update was put right %d times, want none: the agent was told to stop, and the next start puts it right", n)
	}
}
