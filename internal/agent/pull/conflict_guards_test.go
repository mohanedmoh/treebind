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
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

const dotEnv = ".env"

// A resolution moves the checkout the way an update does, so what an update is refused for, a resolution is refused
// for: a local edit or an untracked file in the way (docs/adr/0020), the cap on deletions (docs/adr/0026) and a lock
// that is lost (docs/adr/0012). Whatever refuses it, nothing of the directory was written.

// divergent makes a checkout and a repository that went different ways: the application changed a.json and the
// repository changed a.json and b.json, so that the move of either policy writes b.json.
func divergent(t *testing.T, policy Policy, change ...func(*Options)) (w *world, local, tip string) {
	t.Helper()
	w = newWorld(t, subPath, append([]func(*Options){resolving(policy)}, change...)...)
	local = w.local(t, map[string]string{fileA: mine})
	tip = w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileB: bChanged})
	return w, local, tip
}

// wantNotMoved fails the test unless the checkout is as it was: at the commit, with a clean index and no journal.
func (w *world) wantNotMoved(t *testing.T, local string) {
	t.Helper()
	if w.head() != local {
		t.Errorf("HEAD = %s, want %s: the checkout was moved although the resolution was refused", w.head(), local)
	}
	if !w.indexIsClean() {
		t.Error("the index holds files that are staged after a resolution that was refused")
	}
	if journalPresent(w) {
		t.Error("a journal of an update is there after a resolution that was refused before it wrote a file")
	}
}

func TestAResolutionDoesNotWriteOverALocalEditToAFileItChangesAndSaysSo(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w, local, tip := divergent(t, policy)
			gittest.Write(t, w.l, fileB, edited)

			_, err := w.pull(t)

			got := errorOf(t, err, ReasonBlocked)
			if !strings.Contains(got.Message, fileB) {
				t.Errorf("message = %q, want the file named", got.Message)
			}
			w.wantNotMoved(t, local)
			wantFiles(t, w, map[string]string{fileA: mine, fileB: edited})
			// The person puts the file back, and the resolution goes on.
			gittest.Write(t, w.l, fileB, contentB)
			resolved, err := w.pull(t)
			if err != nil || resolved.Outcome != Resolved {
				t.Fatalf("the next pull = %+v, %v, want Resolved", resolved, err)
			}
			wantFiles(t, w, map[string]string{fileB: bChanged})
			if err := w.check("merge-base", "--is-ancestor", tip, w.head()); err != nil {
				t.Errorf("the tip of the repository is not an ancestor of the result: %v", err)
			}
		})
	}
}

func TestAResolutionKeepsALocalEditToAFileItDoesNotChange(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, map[string]string{fileA: aChanged})
			// c.json is a file of the application's that has a change that is not committed yet.
			gittest.Write(t, w.l, fileC, edited)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("result = %+v, %v, want Resolved", got, err)
			}
			wantFiles(t, w, map[string]string{fileC: edited})
			if dirty := w.dirty(t); dirty != "?? "+fileC {
				t.Errorf("status = %q, want only the application's file", dirty)
			}
		})
	}
}

func TestAResolutionKeepsAnEditToAFileTheRepositoryDidNotChangeAndTheApplicationCommitted(t *testing.T) {
	// The application committed a.json and then edited it again: what the move writes is the committed version only
	// when the move changes the file, and here it does not, so the edit stays.
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, map[string]string{fileB: bChanged})
			gittest.Write(t, w.l, fileA, edited)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("result = %+v, %v, want Resolved", got, err)
			}
			wantFiles(t, w, map[string]string{fileA: edited, fileB: bChanged})
		})
	}
}

func TestAResolutionDoesNotWriteOverAnUntrackedFileInTheWay(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			local := w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileD: contentD})
			gittest.Write(t, w.l, fileD, mine)

			_, err := w.pull(t)

			wantReason(t, err, ReasonBlocked)
			w.wantNotMoved(t, local)
			wantFiles(t, w, map[string]string{fileD: mine})
		})
	}
}

func TestAResolutionDoesNotWriteOverAFileOfTheApplicationThatAGitignoreHides(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			f := gittest.New(t)
			f.Publish(branchMain, map[string]string{".gitignore": dotEnv + "\n", fileA: contentA})
			l := f.Layout(subPath)
			if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
				t.Fatal(err)
			}
			w := &world{f: f, l: l, puller: puller(t, f, l, resolving(policy))}
			local := w.local(t, map[string]string{fileA: mine})
			gittest.Write(t, l, dotEnv, "SECRET=mine\n")
			f.PublishIgnored(branchMain, map[string]string{dotEnv: "SECRET=theirs\n"})

			_, err := w.pull(t)

			wantReason(t, err, ReasonBlocked)
			w.wantNotMoved(t, local)
			wantFiles(t, w, map[string]string{dotEnv: "SECRET=mine\n"})
		})
	}
}

func TestAResolutionThatIsBlockedAndTriedAgainKeepsOneRefForTheLocalCommits(t *testing.T) {
	w, local, _ := divergent(t, FollowRemote)
	gittest.Write(t, w.l, fileB, edited)
	for range 3 {
		_, err := w.pull(t)
		wantReason(t, err, ReasonBlocked)
	}
	w.wantLocalRefs(t, ref{firstLocal, local})
}

func TestTheRefsForTheLocalCommitsAreKeptUpToTheNumberAndTheOlderOnesAreRemoved(t *testing.T) {
	w := newWorld(t, subPath, resolving(FollowRemote), func(o *Options) { o.LocalRefs = 2 })
	locals := make([]string, 0, 4)
	for i := range 4 {
		locals = append(locals, w.local(t, map[string]string{fileA: fmt.Sprintf("mine %d\n", i)}))
		w.f.Publish(branchMain, map[string]string{fileA: fmt.Sprintf("theirs %d\n", i)})
		if got, err := w.pull(t); err != nil || got.Outcome != Resolved {
			t.Fatalf("round %d: %+v, %v", i, got, err)
		}
	}
	w.wantLocalRefs(t, ref{localDir + "00000003", locals[2]}, ref{localDir + "00000004", locals[3]})
	if refs := w.refs(t); len(refs) != 0 {
		t.Errorf("recovery refs = %v, want none: a resolution does not make them, the local ref keeps the commits", refs)
	}
}

func TestARefOfAPersonInTheLocalNamespaceIsLeftAloneAndNeverCounted(t *testing.T) {
	w := newWorld(t, subPath, resolving(FollowRemote), func(o *Options) { o.LocalRefs = 1 })
	w.f.InCheckout(w.l, "update-ref", localDir+"keep-this", w.tip)
	w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	got, err := w.pull(t)

	if err != nil || got.Resolution == nil || got.Resolution.Local != firstLocal {
		t.Fatalf("result = %+v, %v, want the first numbered ref", got, err)
	}
	if err := w.check("rev-parse", "--verify", "--quiet", localDir+"keep-this"); err != nil {
		t.Error("a ref of a person was removed")
	}
}

func TestAResolutionIsRefusedWhenItWouldRemoveMoreFilesThanTheCapAndWritesNothing(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorldOf(t, subPath, five, resolving(policy), capped(2))
			local := w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, gone(fileB, fileC, fileD, fileE))

			_, err := w.pull(t)

			got := errorOf(t, err, ReasonTooManyDeletions)
			if !strings.Contains(got.Message, "4 file(s)") {
				t.Errorf("message = %q, want the number of files", got.Message)
			}
			w.wantNotMoved(t, local)
			wantFiles(t, w, map[string]string{fileA: mine, fileB: contentB, fileE: contentE})
		})
	}
}

func TestAResolutionIsRefusedWhenItBringsAFileIntoTheStateDirectory(t *testing.T) {
	w := newWorld(t, "", resolving(FollowRemote))
	local := w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{".treebind/git/config": "[core]\n\tfsmonitor = /bin/sh\n"})

	_, err := w.pull(t)

	wantReason(t, err, ReasonReservedPath)
	w.wantNotMoved(t, local)
}

// seenByTheFence is what the checkout looked like when the fence was called.
type seenByTheFence struct {
	head  string
	index bool
	refs  int
}

// fence is a fence that records what the checkout looks like each time it is called, and refuses the call of the number
// given, counting from one. It looks at the world that is set once the checkout is made.
type fence struct {
	w      *world
	refuse int
	lost   error
	seen   []seenByTheFence
}

func (f *fence) option(t *testing.T) func(*Options) {
	f.lost = errors.New("the lock is lost")
	return func(o *Options) {
		o.Fence = func(context.Context) error {
			f.seen = append(f.seen, seenByTheFence{head: f.w.head(), index: f.w.indexIsClean(), refs: len(f.w.refsUnder(t, localDir))})
			if len(f.seen) == f.refuse {
				return f.lost
			}
			return nil
		}
	}
}

func TestAResolutionChecksTheFenceBeforeItsFirstWriteAndAgainBeforeItMovesTheCheckout(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			f := &fence{}
			w, local, _ := divergent(t, policy, f.option(t))
			f.w = w

			if got, err := w.pull(t); err != nil || got.Outcome != Resolved {
				t.Fatalf("result = %+v, %v, want Resolved", got, err)
			}

			if len(f.seen) != 2 {
				t.Fatalf("the fence was called %d times, want twice: before the first write and before the move", len(f.seen))
			}
			if first := f.seen[0]; first.refs != 0 || first.head != local || !first.index {
				t.Errorf("at the first call the checkout was %+v, want nothing written", first)
			}
			wantRefs := 0
			if policy == FollowRemote {
				wantRefs = 1
			}
			if second := f.seen[1]; second.head != local || !second.index || second.refs != wantRefs {
				t.Errorf("at the second call the checkout was %+v, want it not moved, the index put back and %d local ref(s)", second, wantRefs)
			}
		})
	}
}

func TestAResolutionWritesNothingWhenTheFenceRefusesAndOnlyTheLocalRefWhenItRefusesTheMove(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		for _, refuse := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, call %d", policy, refuse), func(t *testing.T) {
				f := &fence{refuse: refuse}
				w, local, _ := divergent(t, policy, f.option(t))
				f.w = w

				_, err := w.pull(t)

				got := errorOf(t, err, ReasonRefused)
				if !errors.Is(err, f.lost) || !strings.Contains(got.Message, "the lock is lost") {
					t.Errorf("error = %v, want the refusal of the caller underneath and in the message", err)
				}
				w.wantNotMoved(t, local)
				wantFiles(t, w, map[string]string{fileA: mine, fileB: contentB})
				if refuse == 1 {
					w.wantLocalRefs(t)
				}
				if len(f.seen) != refuse {
					t.Errorf("the fence was called %d times, want %d: nothing is written after it refuses", len(f.seen), refuse)
				}
			})
		}
	}
}

func TestAResolutionIsNotBegunOnceTheAgentWasToldToStop(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w, local, _ := divergent(t, policy)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := w.puller.Pull(ctx)

			if err == nil {
				t.Fatal("a pull with a context that is done resolved a conflict")
			}
			w.wantNotMoved(t, local)
			w.wantLocalRefs(t)
		})
	}
}

func TestTheGitDirectoryIsWhatTheAgentMakesAfterAResolution(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w, _, _ := divergent(t, policy)

			if _, err := w.pull(t); err != nil {
				t.Fatal(err)
			}

			if exists := gittest.Exists(filepath.Join(w.l.GitDir, "packed-refs")); exists {
				t.Error("there is a packed-refs, which the agent refuses in its Git directory")
			}
			// The next pull looks at the Git directory first, and refuses one that holds more than the agent made.
			// With followRemote everything that the application changed was overridden, so the checkout is at the tip.
			want := UpToDate
			if policy == FollowLocal {
				want = Ahead
			}
			if got, err := w.pull(t); err != nil || got.Outcome != want {
				t.Errorf("the pull after the resolution = %+v, %v, want %s", got, err, want)
			}
		})
	}
}

func TestAResolutionThatCannotBeMadeSaysThatTheConflictWasNotResolved(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w, _, _ := divergent(t, policy)
			gittest.Write(t, w.l, fileB, edited)

			_, err := w.pull(t)

			got := errorOf(t, err, ReasonBlocked)
			if !strings.HasPrefix(got.Message, "the conflict between the checkout and the branch main of the repository was not resolved: ") ||
				!strings.Contains(got.Message, fileB) {
				t.Errorf("message = %q, want it to say that the conflict was not resolved, and the file", got.Message)
			}
		})
	}
}

func TestAResolutionWithoutAnAuthorIsRefusedBeforeItWritesAnything(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, func(o *Options) { o.Policy = policy })
			local := w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, map[string]string{fileB: bChanged})

			_, err := w.pull(t)

			got := errorOf(t, err, ReasonPullFailed)
			if !strings.Contains(got.Message, "no valid author") || !strings.Contains(got.Message, "was not resolved") {
				t.Errorf("message = %q, want it to say that the commits of the agent have no valid author", got.Message)
			}
			w.wantNotMoved(t, local)
			w.wantLocalRefs(t)
		})
	}
}

func TestAResolutionThatIsRefusedAgainAndAgainAsTheApplicationCommitsKeepsOnlyTheNumberOfLocalRefs(t *testing.T) {
	// Each try has a new head, so each makes a ref, and none of them moves the checkout, which is when the old ones went.
	w, _, _ := divergent(t, FollowRemote, func(o *Options) { o.LocalRefs = 2 })
	gittest.Write(t, w.l, fileB, edited)
	heads := make([]string, 0, 3)
	for i := range 3 {
		heads = append(heads, w.local(t, map[string]string{fmt.Sprintf("n%d.json", i): mine}))

		_, err := w.pull(t)

		wantReason(t, err, ReasonBlocked)
	}

	w.wantLocalRefs(t, ref{localDir + "00000002", heads[1]}, ref{localDir + "00000003", heads[2]})
}

func TestAResolutionThatTheFenceRefusesRemovesNoOldLocalRefs(t *testing.T) {
	// The lock is not held any more, as far as anyone knows, so nothing is written, and removing a ref is a write.
	f := &fence{refuse: 2}
	w, _, _ := divergent(t, FollowRemote, func(o *Options) { o.LocalRefs = 1 }, f.option(t))
	f.w = w
	w.f.InCheckout(w.l, "update-ref", localDir+"00000001", w.tip)
	w.f.InCheckout(w.l, "update-ref", localDir+"00000002", w.tip)

	_, err := w.pull(t)

	wantReason(t, err, ReasonRefused)
	if refs := w.refsUnder(t, localDir); len(refs) != 3 {
		t.Errorf("local refs = %v, want the 2 that were there and the 1 that was made: nothing is removed after the lock was refused", refs)
	}
}

func TestNameSomeNamesTheFirstThreeAndCountsTheRest(t *testing.T) {
	for want, names := range map[string][]string{
		"a":                  {"a"},
		"a, b, c":            {"a", "b", "c"},
		"a, b, c and 2 more": {"a", "b", "c", "d", "e"},
	} {
		if got := NameSome(names); got != want {
			t.Errorf("NameSome(%v) = %q, want %q", names, got, want)
		}
	}
}
