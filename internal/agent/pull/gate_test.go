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

package pull

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// A resolution that makes a commit is bounded by the breaker as the commits of the commit step are (docs/adr/0035): it asks
// before it writes anything, and says that a commit was made once the checkout is at it.

// gate is a breaker that answers as the test says and notes what it was asked.
type gate struct {
	allow, made error
	asked, told int
	// atTold is called when the agent says that a commit was made, with the context of the call.
	atTold func(ctx context.Context)
}

func (g *gate) Allow(context.Context) error {
	g.asked++
	return g.allow
}

func (g *gate) Made(ctx context.Context) error {
	g.told++
	if g.atTold != nil {
		g.atTold(ctx)
	}
	return g.made
}

// gated gives the Puller a breaker, and a fence that counts how many times the lock was checked.
func gated(g *gate, fenced *atomic.Int32) func(*Options) {
	return func(o *Options) {
		o.Gate = g
		o.Fence = func(context.Context) error {
			fenced.Add(1)
			return nil
		}
	}
}

var held = &breaker.HeldError{Count: 60, Limit: 60}

func TestAResolutionThatNeedsACommitIsNotMadeWhenTheBreakerHoldsAndWritesNothing(t *testing.T) {
	cases := []struct {
		name          string
		policy        Policy
		local, remote map[string]string
	}{
		{"followLocal makes a merge", FollowLocal, map[string]string{fileA: mine}, map[string]string{fileA: aChanged}},
		{"followRemote carries a file of the checkout", FollowRemote, map[string]string{fileA: mine, fileC: contentC}, map[string]string{fileA: aChanged}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fenced atomic.Int32
			g := &gate{allow: held}
			w := newWorld(t, subPath, resolving(c.policy), gated(g, &fenced))
			local := w.local(t, c.local)
			w.f.Publish(branchMain, c.remote)
			remote := w.remoteRefs(t)

			got, err := w.pull(t)

			failure := errorOf(t, err, breaker.ReasonTooManyCommits)
			if !errors.Is(err, held) {
				t.Errorf("error = %v, want it to wrap what the breaker said", err)
			}
			for _, want := range []string{"was not resolved", held.Error()} {
				if !strings.Contains(failure.Message, want) {
					t.Errorf("message = %q, want it to have %q", failure.Message, want)
				}
			}
			if got.Outcome != "" || got.Resolution != nil {
				t.Errorf("result = %+v, want none", got)
			}
			if w.head() != local || !w.indexIsClean() {
				t.Errorf("HEAD = %s and the index is clean = %t, want the checkout as it was, at %s", w.head(), w.indexIsClean(), local)
			}
			// A ref for the commits of the checkout is a write, and a resolution that is held at every interval would make one at each.
			w.wantLocalRefs(t)
			w.wantRemote(t, remote)
			if n := fenced.Load(); n != 0 {
				t.Errorf("the writer lock was checked %d times, want none: nothing is written", n)
			}
			if g.told != 0 {
				t.Errorf("the breaker was told of %d commits, want none", g.told)
			}

			// What is held is resolved at the next pull that the breaker lets through.
			g.allow = nil
			if next, err := w.pull(t); err != nil || next.Outcome != Resolved || next.Resolution.Commit == "" {
				t.Errorf("the pull after the hold = %+v, %v, want the conflict resolved with a commit", next, err)
			}
		})
	}
}

func TestAResolutionThatMakesNoCommitIsNotAskedOfTheBreakerNorHeldByIt(t *testing.T) {
	cases := []struct {
		name          string
		policy        Policy
		local, remote map[string]string
	}{
		{"both sides hold the same files, followLocal", FollowLocal, map[string]string{fileA: aChanged}, map[string]string{fileA: aChanged}},
		{"both sides hold the same files, followRemote", FollowRemote, map[string]string{fileA: aChanged}, map[string]string{fileA: aChanged}},
		{"followRemote has nothing of the checkout's to put on the tip", FollowRemote, map[string]string{fileA: mine}, map[string]string{fileA: aChanged}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fenced atomic.Int32
			g := &gate{allow: held}
			w := newWorld(t, subPath, resolving(c.policy), gated(g, &fenced))
			w.local(t, c.local)
			tip := w.f.Publish(branchMain, c.remote)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Commit != "" || w.head() != tip {
				t.Fatalf("result = %+v, %v, want Resolved with no commit, at the tip %s", got, err, tip)
			}
			if g.asked != 0 || g.told != 0 {
				t.Errorf("the breaker was asked %d times and told %d times, want neither: no commit was to be made", g.asked, g.told)
			}
		})
	}
}

func TestAPullThatMakesNoResolutionIsNotAskedOfTheBreaker(t *testing.T) {
	var fenced atomic.Int32
	g := &gate{allow: held}
	w := newWorld(t, subPath, resolving(FollowLocal), gated(g, &fenced))
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("the pull = %+v, %v, want Advanced", got, err)
	}
	if got, err := w.pull(t); err != nil || got.Outcome != UpToDate {
		t.Fatalf("the pull = %+v, %v, want UpToDate", got, err)
	}
	w.local(t, map[string]string{fileB: mine})
	if got, err := w.pull(t); err != nil || got.Outcome != Ahead {
		t.Fatalf("the pull = %+v, %v, want Ahead", got, err)
	}
	if g.asked != 0 {
		t.Errorf("the breaker was asked %d times, want none: a pull that makes no commit is not bounded", g.asked)
	}
}

func TestAResolutionIsToldToTheBreakerOnceTheCheckoutIsAtItAndNotWhenTheMoveFailed(t *testing.T) {
	t.Run("a resolution that made a commit", func(t *testing.T) {
		var fenced atomic.Int32
		g := &gate{}
		w := newWorld(t, subPath, resolving(FollowLocal), gated(g, &fenced))
		w.local(t, map[string]string{fileA: mine})
		w.f.Publish(branchMain, map[string]string{fileA: aChanged})
		var headWhenTold string
		var deadline bool
		g.atTold = func(ctx context.Context) {
			headWhenTold = w.head()
			_, deadline = ctx.Deadline()
		}
		ctx := t.Context()

		got, err := w.puller.Pull(ctx)

		if err != nil || got.Outcome != Resolved || got.Warning != "" {
			t.Fatalf("result = %+v, %v, want a resolution with nothing to warn of", got, err)
		}
		if g.asked != 1 || g.told != 1 {
			t.Errorf("the breaker was asked %d times and told %d times, want once each", g.asked, g.told)
		}
		if headWhenTold != got.Resolution.Commit {
			t.Errorf("HEAD was %s when the breaker was told, want the commit of the resolution %s: it is told once the checkout is at it", headWhenTold, got.Resolution.Commit)
		}
		if !deadline {
			t.Error("the context that the breaker is told with has no time limit: it is the context of the pull, which a stop cancels")
		}
	})

	t.Run("a move that Git refused", func(t *testing.T) {
		var fenced atomic.Int32
		g := &gate{}
		w := newWorld(t, subPath, resolving(FollowLocal), gated(g, &fenced))
		w.local(t, map[string]string{fileA: mine})
		w.f.Publish(branchMain, map[string]string{fileB: bChanged})
		// An edit of the application in the way of the file that the merge brings.
		gittest.Write(t, w.l, fileB, edited)

		_, err := w.pull(t)

		wantReason(t, err, ReasonBlocked)
		if g.told != 0 {
			t.Errorf("the breaker was told of %d commits, want none: the checkout did not move", g.told)
		}
	})
}

func TestAResolutionThatCouldNotBeCountedStandsAndSaysSo(t *testing.T) {
	var fenced atomic.Int32
	g := &gate{made: errors.New("the file of times is on a volume that is full")}
	w := newWorld(t, subPath, resolving(FollowLocal), gated(g, &fenced))
	w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Resolved || got.Resolution.Commit == "" || got.To != w.head() {
		t.Fatalf("result = %+v, %v, want the resolution to stand", got, err)
	}
	for _, want := range []string{"not counted", "volume that is full"} {
		if !strings.Contains(got.Warning, want) {
			t.Errorf("warning = %q, want it to have %q", got.Warning, want)
		}
	}
}

func TestABreakerThatCannotCountIsAFailedPullAndNotAHold(t *testing.T) {
	var fenced atomic.Int32
	g := &gate{allow: errors.New("the times of the commits cannot be read: permission denied")}
	w := newWorld(t, subPath, resolving(FollowLocal), gated(g, &fenced))
	local := w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	failure := errorOf(t, err, ReasonPullFailed)
	if !errors.Is(err, g.allow) || !strings.Contains(failure.Message, "permission denied") || !strings.Contains(failure.Message, "cannot be counted") {
		t.Errorf("error = %v, want what is wrong and that the commits cannot be counted", err)
	}
	if w.head() != local || fenced.Load() != 0 {
		t.Error("something was written although the commits could not be counted")
	}
}
