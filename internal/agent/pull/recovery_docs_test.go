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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// The commands of docs/recovering-from-a-conflict-or-a-rejected-push.md that a person types on the volume are run here as they
// are written, on a real repository, in the situation that the page says they are for, as those of docs/deletion-guard.md are. The
// commands for a push that the host refused are tried in package push, which has the hook that refuses it.

const (
	recoveryPage = "recovering-from-a-conflict-or-a-rejected-push.md"

	// The file that both sides change, in three lines, so that its two edits can be joined: your directory changes the first
	// line and the repository the last.
	threeLines = "one\ntwo\nthree\n"
	yoursFirst = "ONE\ntwo\nthree\n"
	theirsLast = "one\ntwo\nTHREE\n"
	joinedBoth = "ONE\ntwo\nTHREE\n"

	// The repository's edit of the first line, which is the line that yoursFirst changed too.
	theirsFirst = "UNO\ntwo\nthree\n"
)

// runPage runs a block of the page for the checkout of the world, with the variables that the page says it needs.
func (w *world) runPage(t *testing.T, name string, programs []string, extra ...string) string {
	t.Helper()
	return w.f.RunBlock(t, w.l, recoveryPage, name, branchMain, programs, extra...)
}

// conflicted makes the situation of a conflict that followRemote or followLocal resolved: your directory changed the first
// line of a.json and added a file, and the repository changed the last line of a.json. It returns the result of the pull that
// resolved it.
func conflicted(t *testing.T, sub string, policy Policy) (*world, Result) {
	t.Helper()
	w := newWorldOf(t, sub, map[string]string{fileA: threeLines, fileB: contentB}, resolving(policy))
	w.local(t, map[string]string{fileA: yoursFirst, onlyHere: mine})
	w.f.Publish(branchMain, map[string]string{fileA: theirsLast})
	got, err := w.pull(t)
	if err != nil || got.Outcome != Resolved || got.Resolution == nil {
		t.Fatalf("result = %+v, %v, want the conflict resolved by %s", got, err, policy)
	}
	return w, got
}

func TestTheCommandsThatFindTheVersionThatLostDoSoAndChangeNothing(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w, got := conflicted(t, sub, FollowRemote)
		if got.Resolution.Local != firstLocal {
			t.Fatalf("the ref of your version = %q, want %s", got.Resolution.Local, firstLocal)
		}
		head := w.head()

		listed := w.runPage(t, "list-the-refs-of-your-version", nil)
		seen := w.runPage(t, "see-what-your-directory-changed", nil, "REF="+firstLocal, "FILE="+fileA)

		if !strings.HasPrefix(listed, firstLocal+" ") || !strings.HasSuffix(listed, " local\n") {
			t.Errorf("the refs = %q, want %s, the short id of its commit and its first line", listed, firstLocal)
		}
		short := w.f.InCheckout(w.l, "rev-parse", "--short", firstLocal)
		for _, want := range []string{
			short + " Test local\n",                       // the commit that your directory had, and its author
			"M\t" + fileA + "\n", "A\t" + onlyHere + "\n", // what it changed
			yoursFirst, // the file as your directory had it, and not as the repository has it now
		} {
			if !strings.Contains(seen, want) {
				t.Errorf("output = %q, want it to hold %q", seen, want)
			}
		}
		if strings.Contains(seen, "THREE") || strings.Contains(seen, "M\t"+fileB) {
			t.Errorf("output = %q, want only what your directory changed", seen)
		}
		if w.head() != head || gittest.Read(t, w.l, fileA) != theirsLast {
			t.Error("a command that only reads changed the checkout or a file")
		}
		if dirty := w.dirty(t); dirty != "" {
			t.Errorf("status = %q, want the checkout clean", dirty)
		}
	})
}

func TestTheCommandThatPutsBackAFileOfYourVersionDoesSoAndChangesNothingElse(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w, _ := conflicted(t, sub, FollowRemote)

		w.runPage(t, "put-back-a-file-of-your-version", nil, "REF="+firstLocal, "FILE="+fileA)

		if gittest.Read(t, w.l, fileA) != yoursFirst {
			t.Error("the file is not back as your directory had it")
		}
		if gittest.Read(t, w.l, onlyHere) != mine {
			t.Error("a file that the page does not name was changed")
		}
		// The page says it is an edit like any other, for the agent to commit.
		if dirty := w.dirty(t); dirty != "M "+fileA {
			t.Errorf("status = %q, want the one file as an edit that is not committed", dirty)
		}
		if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
			t.Errorf("the Git directory is not what the agent makes after the command: %v", err)
		}
	})
}

// join runs the first block of the page that joins the two versions of a file, with the directory that it makes in a
// directory of its own, so that the next block can be run for it. It returns what the block wrote and that directory.
func (w *world) join(t *testing.T) (out, work string) {
	t.Helper()
	tmp := gittest.TempDir(t)
	out = w.runPage(t, "join-the-two-versions-of-a-file", []string{"mktemp", "grep", "rm"}, "TMPDIR="+tmp, "REF="+firstLocal, "FILE="+fileA)
	made, err := os.ReadDir(tmp)
	if err != nil || len(made) != 1 {
		t.Fatalf("the block made %v in %s, err = %v, want the one work directory", made, tmp, err)
	}
	return out, filepath.Join(tmp, made[0].Name())
}

// putInPlace runs the second block, which puts the joined file of the work directory where the application reads it. An empty
// work leaves WORK unset, as it is in a terminal that did not run the first block.
func (w *world) putInPlace(t *testing.T, work string) string {
	t.Helper()
	extra := []string{"FILE=" + fileA}
	if work != "" {
		extra = append(extra, "WORK="+work)
	}
	return w.runPage(t, "put-the-joined-file-in-place", []string{"cp", "grep"}, extra...)
}

// conflictedOnOneLine makes what is usual after a conflict: both sides changed the first line of a.json, and followRemote took the
// repository's version, so that it is what the volume has.
func conflictedOnOneLine(t *testing.T, sub string) *world {
	t.Helper()
	w := newWorldOf(t, sub, map[string]string{fileA: threeLines, fileB: contentB}, resolving(FollowRemote))
	w.local(t, map[string]string{fileA: yoursFirst})
	w.f.Publish(branchMain, map[string]string{fileA: theirsFirst})
	if got, err := w.pull(t); err != nil || got.Outcome != Resolved {
		t.Fatalf("result = %+v, %v, want the conflict resolved", got, err)
	}
	return w
}

func TestTheCommandsThatJoinTheTwoVersionsOfAFileKeepBothEdits(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w, _ := conflicted(t, sub, FollowRemote)
		if gittest.Read(t, w.l, fileA) != theirsLast {
			t.Fatal("the volume does not have the repository's version, as the page says")
		}

		out, work := w.join(t)

		// The first block shows the result and writes it beside the file and not over it.
		if !strings.Contains(out, joinedBoth) || !strings.Contains(out, "exit status 0, places marked: 0") {
			t.Errorf("output = %q, want the joined file and no place marked", out)
		}
		if gittest.Read(t, w.l, fileA) != theirsLast || w.dirty(t) != "" {
			t.Error("the first block changed the volume")
		}
		if got, err := os.ReadFile(filepath.Join(work, "joined")); err != nil || string(got) != joinedBoth {
			t.Errorf("joined = %q, %v, want both edits: %q", got, err, joinedBoth)
		}

		w.putInPlace(t, work)

		if got := gittest.Read(t, w.l, fileA); got != joinedBoth {
			t.Errorf("the file = %q, want both edits: %q", got, joinedBoth)
		}
		if dirty := w.dirty(t); dirty != "M "+fileA {
			t.Errorf("status = %q, want the joined file as an edit that is not committed", dirty)
		}
	})
}

func TestTheCommandThatJoinsTwoEditsOfOneLineShowsTheMarksAndLeavesTheFileAlone(t *testing.T) {
	// Both sides changed the first line, which is what a conflict usually is. The block has to get through the exit status of
	// merge-file, which is not zero, and it must not put the marks in the directory that the agent commits.
	eachLayout(t, func(t *testing.T, sub string) {
		w := conflictedOnOneLine(t, sub)
		head := w.head()

		out, work := w.join(t)

		for _, want := range []string{"<<<<<<<", "ONE\n", "UNO\n", ">>>>>>>", "exit status 1, places marked: 1"} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to hold %q", out, want)
			}
		}
		if got, err := os.ReadFile(filepath.Join(work, "joined")); err != nil || !strings.Contains(string(got), "<<<<<<<") {
			t.Errorf("joined = %q, %v, want the result with its marks", got, err)
		}
		if gittest.Read(t, w.l, fileA) != theirsFirst {
			t.Error("the file of the volume is not the repository's version: the block wrote over it")
		}
		if dirty := w.dirty(t); dirty != "" || w.head() != head {
			t.Errorf("status = %q, want the checkout clean and where it was", dirty)
		}
	})
}

func TestTheCommandThatPutsTheJoinedFileInPlaceRefusesAFileThatStillHasMarks(t *testing.T) {
	// A reader who pastes both blocks one after the other must not get the marks into the directory.
	eachLayout(t, func(t *testing.T, sub string) {
		w := conflictedOnOneLine(t, sub)
		_, work := w.join(t)

		out := w.putInPlace(t, work)

		if !strings.Contains(out, "still has conflict marks") || !strings.Contains(out, "Nothing was copied") {
			t.Errorf("output = %q, want the block to say that the file has marks and that it copied nothing", out)
		}
		if gittest.Read(t, w.l, fileA) != theirsFirst || w.dirty(t) != "" {
			t.Error("the block put a file with marks in the directory")
		}
	})
}

func TestTheCommandThatPutsTheJoinedFileInPlaceCopiesItOnceTheMarksAreOut(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := conflictedOnOneLine(t, sub)
		_, work := w.join(t)
		// What the page tells the reader to do: keep one of the two versions, and take the marks out.
		if err := os.WriteFile(filepath.Join(work, "joined"), []byte(yoursFirst), 0o600); err != nil {
			t.Fatal(err)
		}

		out := w.putInPlace(t, work)

		if strings.Contains(out, "Nothing was copied") {
			t.Errorf("output = %q, want the block to copy a file with no marks", out)
		}
		if got := gittest.Read(t, w.l, fileA); got != yoursFirst {
			t.Errorf("the file = %q, want the fixed file: %q", got, yoursFirst)
		}
		if dirty := w.dirty(t); dirty != "M "+fileA {
			t.Errorf("status = %q, want the fixed file as an edit that is not committed", dirty)
		}
	})
}

func TestTheCommandsThatJoinSayWhenMergeFileFailsAndCopyNothing(t *testing.T) {
	// The file of the volume is not there, so merge-file cannot read it and ends with the status of an error, which is not a count of
	// places. The block says so and makes no joined file, so the next block copies nothing, in the terminal that ran the first one
	// and in one that does not know the work directory.
	eachLayout(t, func(t *testing.T, sub string) {
		w, _ := conflicted(t, sub, FollowRemote)
		if err := os.Remove(filepath.Join(w.l.WorkTree, fileA)); err != nil {
			t.Fatal(err)
		}

		out, work := w.join(t)

		if !strings.Contains(out, "exit status 255") || !strings.Contains(out, "nothing was joined") || strings.Contains(out, "places marked") {
			t.Errorf("output = %q, want it to say that merge-file failed and that nothing was joined", out)
		}
		if gittest.Exists(filepath.Join(work, "joined")) {
			t.Error("the block left a joined file after the failure")
		}
		for _, again := range []string{w.putInPlace(t, work), w.putInPlace(t, "")} {
			if !strings.Contains(again, "run the block above first") || !strings.Contains(again, "Nothing was copied") {
				t.Errorf("output = %q, want the block to say to run the block above first, and that it copied nothing", again)
			}
		}
		if gittest.Exists(filepath.Join(w.l.WorkTree, fileA)) {
			t.Error("the second block wrote a file that the first did not make")
		}
	})
}

func TestTheCommandsThatFindTheRepositorysVersionAfterFollowLocalDoSo(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w, got := conflicted(t, sub, FollowLocal)
		if got.Resolution.Commit == "" || got.Resolution.Local != "" {
			t.Fatalf("resolution = %+v, want a merge and no ref: the volume won, and the sides differ", got.Resolution)
		}

		out := w.runPage(t, "find-the-version-of-the-repository-that-lost", nil, "FILE="+fileA)

		// The commit that came from the repository, by the author the fixture gives it, and its version of the file.
		if !strings.Contains(out, " Test change\n") || !strings.HasSuffix(out, theirsLast) {
			t.Errorf("output = %q, want the commit that came from the repository and its version of %s", out, fileA)
		}
		if gittest.Read(t, w.l, fileA) != yoursFirst {
			t.Error("the volume's version, which won, is not the one that is there")
		}
	})
}

func TestTheCommandsThatCompareTheBranchWithTheRepositoryCountTheCommitsOfEachSide(t *testing.T) {
	w := newWorld(t, subPath)
	w.local(t, map[string]string{onlyHere: mine})
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})
	_, err := w.pull(t)
	wantReason(t, err, ReasonDiverged)

	out := w.runPage(t, "compare-with-the-repository", nil)

	counts, marked, _ := strings.Cut(out, "\n")
	if counts != "1\t2" {
		t.Errorf("counts = %q, want 1 commit that only your directory has and 2 that only the repository has", counts)
	}
	// Commits made in one second have no order that Git promises, so the marks are counted and not placed.
	var yours, theirs int
	for line := range strings.Lines(marked) {
		switch {
		case strings.HasPrefix(line, "< ") && strings.Contains(line, " Test local"):
			yours++
		case strings.HasPrefix(line, "> ") && strings.Contains(line, " Test change"):
			theirs++
		default:
			t.Errorf("line = %q, want a commit marked < that is your directory's, or > that is the repository's", line)
		}
	}
	if yours != 1 || theirs != 2 {
		t.Errorf("commits = %q, want one marked < and two marked >", marked)
	}
}

func TestTheCommandsThatMakeTheVolumeTheBranchDoSoAndKeepWhereItWas(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		w.f.Publish(branchMain, map[string]string{fileC: contentC})
		if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
			t.Fatalf("result = %+v, %v, want the first update applied", got, err)
		}
		old := w.head()
		w.f.Rewrite()
		rewritten := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
		_, err := w.pull(t)
		wantReason(t, err, ReasonDiverged)

		w.runPage(t, "make-the-volume-the-branch", []string{"date"})

		if w.head() != rewritten || gittest.Read(t, w.l, fileA) != aChanged {
			t.Errorf("HEAD = %s, want %s with its files", w.head(), rewritten)
		}
		if gittest.Exists(filepath.Join(w.l.WorkTree, fileC)) {
			t.Error("a file that only the old commit had is still there")
		}
		if dirty := w.dirty(t); dirty != "" {
			t.Errorf("status = %q, want the checkout clean", dirty)
		}
		kept := strings.Fields(w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname) %(objectname)", "refs/treebind/recovery/"))
		if len(kept) != 4 || !strings.HasPrefix(kept[2], "refs/treebind/recovery/by-hand-") || kept[3] != old {
			t.Errorf("refs = %q, want the one of the update and one of your own that keeps %s", kept, old)
		}
		if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
			t.Errorf("the Git directory is not what the agent makes after the commands: %v", err)
		}

		// The page says that the next pull finds the checkout at the tip, and that the agent leaves a ref of yours alone.
		got, err := w.pull(t)
		if err != nil || got.Outcome != UpToDate || got.To != rewritten {
			t.Fatalf("result = %+v, %v, want UpToDate at %s", got, err, rewritten)
		}
		w.f.Publish(branchMain, map[string]string{fileB: bChanged})
		got, err = w.pull(t)
		if err != nil || got.Outcome != Advanced || got.Recovery != "refs/treebind/recovery/00000002" {
			t.Fatalf("result = %+v, %v, want an update that makes the next numbered ref", got, err)
		}
		if again := w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname)", "refs/treebind/recovery/"); !strings.Contains(again, "by-hand-") {
			t.Errorf("refs = %q, want the ref of your own to stay", again)
		}
	})
}
