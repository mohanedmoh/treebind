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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// An update that was killed in the middle of the files is put back, and the next pull applies it (docs/adr/0028).
// The kill is a real one: the merge of the pull is held by a filter or a hook while it writes, and is killed
// with SIGKILL, so that it leaves what an out-of-memory kill or a lost node leaves.

const (
	oldA   = "a1\n"
	oldB   = "b1\n"
	oldC   = "c1\n"
	oldE   = "e1\n"
	newA   = "a2\n"
	newB   = "b2\n"
	newC   = "c2\n"
	newD   = "d2\n"
	noteUn = "was cut off, and was undone"
)

// before is the repository before the update, and after is what the update does: it changes a, b and c, adds d, and
// removes e.
var (
	before = map[string]string{fileA: oldA, fileB: oldB, fileC: oldC, fileE: oldE}
	update = map[string]string{fileA: newA, fileB: newB, fileC: newC, fileD: newD, fileE: ""}
)

// cut is a checkout whose update was cut off, and the agent that starts again after it.
type cut struct {
	*world
	from, to string
}

// cutOff makes a checkout, publishes the update, applies it with a merge that hold keeps in the middle, kills the
// merge, and returns a world whose Puller is a new one, as a restart makes. The locks that the killed Git left are
// removed, as the agent removes them before a pull.
func cutOff(t *testing.T, sub string, hold func(f *gittest.Fixture) *gittest.HeldMerge, change ...func(*Options)) *cut {
	t.Helper()
	w := newWorldOf(t, sub, before)
	held := hold(w.f)
	from := w.head()
	to := w.f.Publish(branchMain, update)
	killed := puller(t, w.f, w.l, func(o *Options) { o.Git.Git = held.Binary })

	done := make(chan error, 1)
	go func() {
		_, err := killed.Pull(context.Background())
		done <- err
	}()
	held.WaitHeld()
	held.Kill()
	if err := <-done; err == nil {
		t.Fatal("the pull of the merge that was killed did not fail")
	}
	if _, err := git.RemoveStaleLocks(w.l.GitDir); err != nil {
		t.Fatal(err)
	}
	w.puller = puller(t, w.f, w.l, change...)
	return &cut{world: w, from: from, to: to}
}

// files says what each of the five files is, or that it is not there.
func (w *world) files(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, name := range []string{fileA, fileB, fileC, fileD, fileE} {
		if data, err := os.ReadFile(filepath.Join(w.l.WorkTree, name)); err == nil {
			got[name] = string(data)
		}
	}
	return got
}

func sameFiles(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for name, content := range want {
		if got[name] != content {
			return false
		}
	}
	return true
}

func journalPresent(w *world) bool {
	_, err := os.Lstat(filepath.Join(w.l.StateDir, journalName))
	return err == nil
}

func TestAnUpdateThatWasKilledInTheMiddleOfTheFilesIsUndoneAndTheNextPullAppliesIt(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		c := cutOff(t, sub, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })
		// What the kill left: a is written, b is gone, c is not reached, d is not there yet, and e is deleted.
		if got, want := c.files(t), map[string]string{fileA: newA, fileC: oldC}; !sameFiles(got, want) {
			t.Fatalf("the cut-off update left %v, want %v: the test does not hold the merge where it means to", got, want)
		}
		if !journalPresent(c.world) {
			t.Fatal("the journal of the update is not there after the kill")
		}

		// The repair alone puts the checkout back and applies nothing.
		note, failure := c.puller.repair(context.Background(), c.head())
		if failure != nil {
			t.Fatal(failure)
		}
		if !strings.Contains(note, noteUn) || !strings.Contains(note, "2 file(s) put back, 0 removed") {
			t.Errorf("repair says %q, want the update undone, with 2 files put back and none removed", note)
		}
		if got, want := c.files(t), map[string]string{fileA: oldA, fileB: oldB, fileC: oldC}; !sameFiles(got, want) {
			t.Errorf("after the repair the files are %v, want %v: the update is undone, and what it deletes is left", got, want)
		}
		if c.head() != c.from || journalPresent(c.world) {
			t.Errorf("HEAD = %s, journal present %v, want the commit before and no journal: a repair applies no update", c.head(), journalPresent(c.world))
		}

		// The next pull applies it, with every check it has.
		got, err := c.pull(t)
		if err != nil || got.Outcome != Advanced || got.To != c.to {
			t.Fatalf("pull = %+v, %v, want the update applied", got, err)
		}
		if want := map[string]string{fileA: newA, fileB: newB, fileC: newC, fileD: newD}; !sameFiles(c.files(t), want) || c.dirty(t) != "" {
			t.Errorf("files = %v, status %q, want the files of the update, and a clean checkout", c.files(t), c.dirty(t))
		}
	})
}

func TestAnUpdateThatWasKilledAfterItsFilesAndItsIndexIsUndoneToo(t *testing.T) {
	// The merge was killed at the update of the branch: every file and the index are the update's, and HEAD is the
	// commit before. The files are put back by the same rule, and the index is reset first, so that they are put
	// back as the commit before has them.
	c := cutOff(t, subPath, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMergeAtRef("refs/heads/main") })
	if got := c.dirty(t); !strings.Contains(got, "M  a.json") {
		t.Fatalf("status = %q, want the update staged", got)
	}

	got, err := c.pull(t)

	if err != nil || got.Outcome != Advanced || got.To != c.to {
		t.Fatalf("pull = %+v, %v, want the update applied", got, err)
	}
	for _, want := range []string{"the index had files staged", noteUn, "3 file(s) put back, 1 removed"} {
		if !strings.Contains(got.Repaired, want) {
			t.Errorf("repaired = %q, want it to say %q", got.Repaired, want)
		}
	}
	if want := map[string]string{fileA: newA, fileB: newB, fileC: newC, fileD: newD}; !sameFiles(c.files(t), want) || c.dirty(t) != "" {
		t.Errorf("files = %v, status %q, want the files of the update, and a clean checkout", c.files(t), c.dirty(t))
	}
}

func TestAnUpdateThatWasKilledBeforeItWroteAFileHasNothingToPutBack(t *testing.T) {
	c := cutOff(t, subPath, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMergeAtRef("ORIG_HEAD") })

	got, err := c.pull(t)

	if err != nil || got.Outcome != Advanced {
		t.Fatalf("pull = %+v, %v, want the update applied", got, err)
	}
	if !strings.Contains(got.Repaired, "0 file(s) put back, 0 removed") || journalPresent(c.world) {
		t.Errorf("repaired = %q, journal present %v, want an update that had changed no file, and no journal", got.Repaired, journalPresent(c.world))
	}
}

func TestTheFirstUpdateOfACheckoutWithNoCommitIsUndoneToo(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	// An empty repository, and a file of the application's own: the checkout is adopted, with no commit.
	gittest.Write(t, l, "mine.json", mine)
	if made, err := checkoutOpen(t, f, l); err != nil || made != "" {
		t.Fatalf("checkout = %q, %v, want one with no commit", made, err)
	}
	held := f.HoldMerge(fileB)
	to := f.Publish(branchMain, map[string]string{fileA: oldA, fileB: oldB, fileC: oldC})
	killed := puller(t, f, l, func(o *Options) { o.Git.Git = held.Binary })
	done := make(chan error, 1)
	go func() {
		_, err := killed.Pull(context.Background())
		done <- err
	}()
	held.WaitHeld()
	held.Kill()
	if err := <-done; err == nil {
		t.Fatal("the pull of the merge that was killed did not fail")
	}
	if _, err := git.RemoveStaleLocks(l.GitDir); err != nil {
		t.Fatal(err)
	}
	w := &world{f: f, l: l, puller: puller(t, f, l)}
	if !gittest.Exists(filepath.Join(l.WorkTree, fileA)) {
		t.Fatal("the cut-off update wrote no file: the test does not hold the merge where it means to")
	}

	got, err := w.pull(t)

	if err != nil || got.Outcome != Advanced || got.To != to || got.From != "" {
		t.Fatalf("pull = %+v, %v, want the first update applied", got, err)
	}
	if !strings.Contains(got.Repaired, noteUn) {
		t.Errorf("repaired = %q, want the update undone", got.Repaired)
	}
	if gittest.Read(t, l, "mine.json") != mine || gittest.Read(t, l, fileA) != oldA || gittest.Read(t, l, fileC) != oldC {
		t.Error("the file of the application is gone, or the files of the update are not there")
	}
}

func TestAJournalOfAnUpdateThatFinishedIsRemovedAndNothingIsTouched(t *testing.T) {
	w := newWorld(t, subPath)
	from := w.head()
	to := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("pull = %+v, %v", got, err)
	}
	// The pull ended after the merge and before the journal was removed.
	if err := w.puller.writeJournal(from, to); err != nil {
		t.Fatal(err)
	}

	got, err := w.pull(t)

	if err != nil || got.Outcome != UpToDate || !strings.Contains(got.Repaired, "had finished, and its journal was removed") || journalPresent(w) {
		t.Errorf("pull = %+v, %v, journal present %v, want the journal removed, and said", got, err, journalPresent(w))
	}
	if gittest.Read(t, w.l, fileA) != aChanged {
		t.Error("a file of the update was changed")
	}
}

func TestAJournalOfAnUpdateThatTheCheckoutIsNotAtEitherEndOfIsRemovedAndNothingIsTouched(t *testing.T) {
	w := newWorld(t, subPath)
	if err := w.puller.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, w.l, fileA, edited)

	got, err := w.pull(t)

	if err != nil || got.Outcome != UpToDate || !strings.Contains(got.Repaired, "which is neither") || journalPresent(w) {
		t.Errorf("pull = %+v, %v, journal present %v, want the journal removed, and said", got, err, journalPresent(w))
	}
	if gittest.Read(t, w.l, fileA) != edited {
		t.Error("a file was changed by a repair that had nothing to put back")
	}
}

func TestAJournalThatIsTornIsSaidAndIgnoredAndThePullGoesOn(t *testing.T) {
	w := newWorld(t, subPath)
	if err := os.WriteFile(filepath.Join(w.l.StateDir, journalName), []byte(`{"version":1,"fr`), 0o600); err != nil {
		t.Fatal(err)
	}
	tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Advanced || got.To != tip {
		t.Fatalf("pull = %+v, %v, want the pull to go on and apply the update", got, err)
	}
	if !strings.Contains(got.Repaired, "cannot be used, and was ignored") || !strings.Contains(got.Repaired, "torn") {
		t.Errorf("repaired = %q, want it to say that the journal was torn and ignored", got.Repaired)
	}
	if journalPresent(w) {
		t.Error("the torn journal is still there")
	}
}

func TestARepairChecksTheFenceBeforeItWritesAndNotWhenThereIsNothingToPutRight(t *testing.T) {
	var asked int
	w := newWorld(t, subPath, func(o *Options) {
		o.Fence = func(context.Context) error { asked++; return nil }
	})
	if got, err := w.pull(t); err != nil || got.Repaired != "" || asked != 0 {
		t.Fatalf("pull = %+v, %v, asked the fence %d times, want nothing to put right and no call", got, err, asked)
	}

	if err := w.puller.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}
	if got, err := w.pull(t); err != nil || got.Repaired == "" || asked != 1 {
		t.Errorf("pull = %+v, %v, asked the fence %d times, want the journal dealt with after one call", got, err, asked)
	}
}

func TestARepairIsNotDoneWhenTheFenceRefusesAndNothingIsChanged(t *testing.T) {
	refused := errors.New("the lock is another agent's")
	w := newWorld(t, subPath, func(o *Options) {
		o.Fence = func(context.Context) error { return refused }
	})
	if err := w.puller.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}

	_, err := w.pull(t)

	if wantReason(t, err, ReasonRefused); !errors.Is(err, refused) && !strings.Contains(err.Error(), refused.Error()) {
		t.Errorf("error = %v, want the reason of the fence in it", err)
	}
	if !journalPresent(w) {
		t.Error("the journal was removed by a pull that was refused")
	}
}

func TestAJournalIsLeftAloneByAPullThatRefusesACheckoutThatIsNotOnTheBranch(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.InCheckout(w.l, "symbolic-ref", "HEAD", "refs/heads/other")
	if err := w.puller.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}

	_, err := w.pull(t)

	wantReason(t, err, ReasonPullFailed)
	if !strings.Contains(err.Error(), "does not switch it") || !journalPresent(w) {
		t.Errorf("error = %v, journal present %v, want the checkout refused for the branch, before anything is put right", err, journalPresent(w))
	}
}

// checkoutOpen makes the checkout of a layout and returns the commit it is at.
func checkoutOpen(t *testing.T, f *gittest.Fixture, l layout.Layout) (string, error) {
	t.Helper()
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	return made.Commit, err
}
