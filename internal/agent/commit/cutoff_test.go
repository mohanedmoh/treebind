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

package commit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

// An update that was cut off leaves files that the scan lists as changes and that are the agent's, half written for some, and
// nothing of them is committed until the pull has put them right (docs/adr/0028, #232).

const (
	// fileAdded is a file that the update adds, before fileB in the order in which Git writes the files.
	fileAdded = "added.txt"
	// fileApp is a file that the application writes while the update waits to be put right.
	fileApp = "app.txt"
	// updatedB is what the update makes of fileB, and halfB the start of it, which is what a write that was cut off leaves.
	updatedB = "b, updated\n"
	halfB    = "b, upd"
)

// update is what the update makes of the files.
var update = map[string]string{fileA: "a, updated\n", fileAdded: "added\n", fileB: updatedB, fileC: "c, updated\n"}

// cutOff applies an update of the files with a Git that is killed in the middle of them, at fileB, as a kernel that is short of
// memory does, and returns the commit that it went to. What the update leaves is fileA and fileAdded written whole, fileB not there
// and fileC as it was; fileB is then given the start of its new content by hand, as the tests of the pull do, because Git writes a
// file with one write and the state of a file that is the start of the new one cannot be had on cue.
func (w *world) cutOff() string {
	w.t.Helper()
	held := w.f.HoldMerge(fileB)
	tip := w.f.Publish(branchMain, update)
	options := pull.Options{Layout: w.l, URL: w.f.URL, Branch: branchMain, Git: w.f.GitOptions()}
	options.Git.Git = held.Binary
	killed, err := pull.New(options)
	if err != nil {
		w.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := killed.Pull(context.Background())
		done <- err
	}()
	held.WaitHeld()
	held.Kill()
	if err := <-done; err == nil {
		w.t.Fatal("the pull of the merge that was killed did not fail")
	}
	if gittest.Read(w.t, w.l, fileA) != update[fileA] || gittest.Exists(w.path(fileB)) || gittest.Read(w.t, w.l, fileC) != "c\n" {
		w.t.Fatal("the update was not cut off in the middle of the files: the test does not hold the merge where it means to")
	}
	if cut, err := pull.UpdateCutOff(w.l); err != nil || !cut {
		w.t.Fatalf("UpdateCutOff = %v, %v after the merge was killed, want the journal there", cut, err)
	}
	w.write(fileB, halfB)
	return tip
}

// clearLocks removes the lock files that the killed Git left, as the agent does before each commit and each pull.
func (w *world) clearLocks() {
	w.t.Helper()
	if _, err := git.RemoveStaleLocks(w.l.GitDir); err != nil {
		w.t.Fatal(err)
	}
}

// repairs runs the pull that a start runs, after the lock files are removed: it puts the update right and applies it again.
func (w *world) repairs() pull.Result {
	w.t.Helper()
	w.clearLocks()
	puller, err := pull.New(pull.Options{Layout: w.l, URL: w.f.URL, Branch: branchMain, Git: w.f.GitOptions()})
	if err != nil {
		w.t.Fatal(err)
	}
	got, err := puller.Pull(context.Background())
	if err != nil {
		w.t.Fatalf("the pull after the update was cut off failed: %v", err)
	}
	return got
}

func pathsOf(changes []scan.Change) []string {
	names := make([]string, 0, len(changes))
	for _, c := range changes {
		names = append(names, c.Path)
	}
	return names
}

func TestNothingIsCommittedWhileAnUpdateThatWasCutOffIsNotPutRightAndTheApplicationsFilesAreCommittedAfter(t *testing.T) {
	g := &gate{}
	w := newWorld(t, "", base, gated(g))
	before := w.head()
	tip := w.cutOff()
	// The application writes a file of its own while the update waits to be put right.
	w.write(fileApp, "the application's\n")

	// What the scan lists is the files that the update wrote, the half one among them, beside the application's. The lock of the
	// index that the killed Git left is gone, as the agent removes it before a commit, so that nothing but the journal is in the way.
	w.clearLocks()
	listed := w.changes()
	if got, want := pathsOf(listed), []string{fileA, fileAdded, fileApp, fileB}; !slices.Equal(got, want) {
		t.Fatalf("the scan lists %q, want %q: the update's files are what it takes for changes", got, want)
	}
	got, err := w.committer.Commit(context.Background(), listed)
	if err != nil || got != (Result{Outcome: Deferred}) {
		t.Fatalf("Commit = %+v, %v, want it deferred, with nothing else said", got, err)
	}
	if w.head() != before {
		t.Fatal("a commit was made of the files of an update that was cut off")
	}
	if staged := w.staged(); len(staged) != 0 {
		t.Errorf("the index holds %q, want nothing staged", staged)
	}
	if n := w.fenced.Load(); n != 0 || g.asked != 0 {
		t.Errorf("the writer lock was checked %d times and the breaker asked %d times, want neither: nothing is written", n, g.asked)
	}
	if _, err := os.Lstat(filepath.Join(w.l.StateDir, listFile)); err == nil {
		t.Error("the file of paths was written")
	}

	// The pull puts the update right: it undoes it, applies it again, and the files are whole.
	pulled := w.repairs()
	if pulled.Outcome != pull.Advanced || pulled.To != tip ||
		!strings.Contains(pulled.Repaired, "was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed") {
		t.Fatalf("pull = %+v, want the update undone, 2 files put back and 1 removed, and applied", pulled)
	}
	for name, want := range update {
		if got := gittest.Read(t, w.l, name); got != want {
			t.Errorf("%s is %q after the pull, want %q", name, got, want)
		}
	}

	// What is still a change is the application's, and it is committed, alone.
	listed = w.changes()
	if got, want := pathsOf(listed), []string{fileApp}; !slices.Equal(got, want) {
		t.Fatalf("the scan lists %q after the pull, want %q", got, want)
	}
	got, err = w.committer.Commit(context.Background(), listed)
	if err != nil || got.Outcome != Committed || got.Files != 1 {
		t.Fatalf("Commit after the pull = %+v, %v, want the application's file committed", got, err)
	}
	w.wantCommitted("A " + fileApp)
	if parent := w.git("rev-parse", "HEAD^"); parent != tip {
		t.Errorf("the commit is on %s, want it on the commit of the update, %s", parent, tip)
	}
}

func TestAJournalThatIsNotOneStillHoldsTheCommitsBack(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, journal string){
		"torn": func(t *testing.T, journal string) {
			if err := os.WriteFile(journal, []byte(`{"version":1,"fro`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, journal string) {
			if err := os.Mkdir(journal, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, "", base)
			before := w.head()
			plant(t, filepath.Join(w.l.StateDir, "updating"))
			w.write(fileNew, "new\n")

			// What a journal says is the pull's to read: one that it cannot use is removed by it, and said in its check.
			if got, err := w.committer.Commit(context.Background(), w.changes()); err != nil || got.Outcome != Deferred {
				t.Fatalf("Commit = %+v, %v, want it deferred", got, err)
			}
			if w.head() != before {
				t.Fatal("a commit was made while a journal was there")
			}
		})
	}
}

func TestAStateDirectoryThatCannotBeLookedAtForAJournalIsAFailedCommitThatWritesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root looks into a directory that it cannot search")
	}
	w := newWorld(t, "", base)
	before := w.head()
	w.write(fileNew, "new\n")
	listed := w.changes()
	if err := os.Chmod(w.l.StateDir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.l.StateDir, 0o700) })

	_, err := w.committer.Commit(context.Background(), listed)

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed ||
		!strings.Contains(failure.Message, "cannot be looked at for an update that was cut off") {
		t.Fatalf("error = %v, want a CommitFailed that says the journal could not be looked for", err)
	}
	if n := w.fenced.Load(); n != 0 {
		t.Errorf("the writer lock was checked %d times, want none: nothing is written", n)
	}
	if err := os.Chmod(w.l.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if w.head() != before {
		t.Error("a commit was made although it could not be told whether an update was cut off")
	}
}
