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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

const (
	fileD    = "d.json"
	fileE    = "e.json"
	contentD = "d\n"
	contentE = "e\n"

	// recoveryDir is where the recovery refs are, and the first of them.
	recoveryDir = "refs/treebind/recovery/"
	firstRef    = recoveryDir + "00000001"
	secondRef   = recoveryDir + "00000002"
)

// five is a repository of five files, so that an update can remove several and leave some.
var five = map[string]string{fileA: contentA, fileB: contentB, fileC: contentC, fileD: contentD, fileE: contentE}

// gone is an update that removes the files.
func gone(names ...string) map[string]string {
	update := map[string]string{}
	for _, name := range names {
		update[name] = ""
	}
	return update
}

// capped is an option that sets the cap on deletions.
func capped(n int) func(*Options) { return func(o *Options) { o.MaxDeletions = n } }

// ref is a recovery ref and the commit it points at.
type ref struct{ name, commit string }

// refs lists the refs of the recovery namespace, by name.
func (w *world) refs(t *testing.T) []ref {
	t.Helper()
	var found []ref
	for line := range strings.Lines(w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname) %(objectname)", recoveryDir)) {
		name, commit, _ := strings.Cut(strings.TrimSpace(line), " ")
		found = append(found, ref{name, commit})
	}
	return found
}

// wantRefs fails the test unless the recovery refs are the ones given, each with the commit it points at.
func (w *world) wantRefs(t *testing.T, want ...ref) {
	t.Helper()
	got := w.refs(t)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("recovery refs = %v, want %v", got, want)
	}
}

// wantNothingWritten fails the test unless the checkout is at the commit and no file of the update is there.
func (w *world) wantAt(t *testing.T, commit string) {
	t.Helper()
	if got := w.head(); got != commit {
		t.Errorf("HEAD = %s, want %s: the checkout was moved although the update was not applied", got, commit)
	}
}

func TestPullAppliesAnUpdateThatRemovesNoMoreFilesThanTheCap(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorldOf(t, sub, five, capped(2))
		tip := w.f.Publish(branchMain, gone(fileA, fileB))

		got, err := w.pull(t)

		if err != nil || got.Outcome != Advanced || got.To != tip {
			t.Fatalf("result = %+v, %v, want Advanced to %s: it removes two files and the cap is two", got, err, tip)
		}
		if gittest.Exists(filepath.Join(w.l.WorkTree, fileA)) || !gittest.Exists(filepath.Join(w.l.WorkTree, fileC)) {
			t.Error("the files of the update are not removed, or others are")
		}
	})
}

func TestPullRefusesAnUpdateThatRemovesMoreFilesThanTheCapAndWritesNothing(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		var fenced int
		w := newWorldOf(t, sub, five, capped(2), func(o *Options) {
			o.Fence = func(context.Context) error { fenced++; return nil }
		})
		w.f.Publish(branchMain, gone(fileA, fileB, fileC, fileD))

		_, err := w.pull(t)

		got := errorOf(t, err, ReasonTooManyDeletions)
		if !strings.Contains(got.Message, "4 file(s)") || !strings.Contains(got.Message, "more than the 2 ") ||
			strings.Count(got.Message, ".json") != maxNamed || !strings.Contains(got.Message, "Nothing was written") ||
			!strings.Contains(got.Message, "set spec.sync.maxDeletions to 4 or more, or remove those files yourself") {
			t.Errorf("message = %q, want the count, the cap, %d names, that nothing was written and what to do", got.Message, maxNamed)
		}
		w.wantAt(t, w.tip)
		for name := range five {
			if !gittest.Exists(filepath.Join(w.l.WorkTree, name)) {
				t.Errorf("%s is gone although the update was refused", name)
			}
		}
		if fenced != 0 {
			t.Errorf("the fence was called %d times: the cap is looked at before it, and reads only", fenced)
		}
		w.wantRefs(t)
	})
}

func TestPullHasACapOfOneHundredFilesWhenNoneIsGiven(t *testing.T) {
	files := make([]string, 0, 102)
	initial := map[string]string{}
	for i := range 102 {
		name := fmt.Sprintf("f%03d.json", i)
		files = append(files, name)
		initial[name] = fmt.Sprintf("%d\n", i)
	}
	t.Run("a hundred", func(t *testing.T) {
		w := newWorldOf(t, subPath, initial)
		tip := w.f.Publish(branchMain, gone(files[:100]...))

		got, err := w.pull(t)

		if err != nil || got.To != tip {
			t.Errorf("result = %+v, %v, want Advanced: a hundred files is the cap and not over it", got, err)
		}
	})
	t.Run("a hundred and one", func(t *testing.T) {
		w := newWorldOf(t, subPath, initial)
		w.f.Publish(branchMain, gone(files[:101]...))

		_, err := w.pull(t)

		got := errorOf(t, err, ReasonTooManyDeletions)
		if !strings.Contains(got.Message, "101 file(s)") || !strings.Contains(got.Message, "more than the 100 ") {
			t.Errorf("message = %q, want 101 files and the cap of 100", got.Message)
		}
		w.wantAt(t, w.tip)
	})
}

func TestPullRefusesAnUpdateThatLeavesTheBranchWithNoFileWhateverTheCap(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		w.f.Publish(branchMain, gone(fileA, fileB))

		_, err := w.pull(t)

		got := errorOf(t, err, ReasonTooManyDeletions)
		if !strings.Contains(got.Message, "2 file(s)") || !strings.Contains(got.Message, "no file at all") ||
			!strings.Contains(got.Message, "Nothing was written") || strings.Contains(got.Message, "maxDeletions") ||
			!strings.Contains(got.Message, "remove those files yourself") {
			t.Errorf("message = %q, want the count and that the branch would have no file at all", got.Message)
		}
		w.wantAt(t, w.tip)
		if !gittest.Exists(filepath.Join(w.l.WorkTree, fileA)) {
			t.Error("a file is gone although the update was refused")
		}
	})
}

func TestPullSaysThatNoNumberLetsItThroughWhenItEmptiesTheBranchAndAlsoRemovesMoreThanTheCap(t *testing.T) {
	w := newWorldOf(t, subPath, five, capped(2))
	w.f.Publish(branchMain, gone(fileA, fileB, fileC, fileD, fileE))

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "no file at all") || strings.Contains(got.Message, "maxDeletions") ||
		!strings.Contains(got.Message, "5 file(s)") {
		t.Errorf("message = %q, want the one that no number lifts, and not advice to raise a number that the next refusal would not accept", got.Message)
	}
}

func TestPullAppliesAnUpdateThatReplacesEveryFileOfTheBranchWithOthers(t *testing.T) {
	w := newWorld(t, subPath)
	update := gone(fileA, fileB)
	update[fileC] = contentC
	tip := w.f.Publish(branchMain, update)

	got, err := w.pull(t)

	if err != nil || got.To != tip {
		t.Errorf("result = %+v, %v, want Advanced: the branch still has a file", got, err)
	}
}

func TestPullDoesNotCountAFileThatIsAlreadyGone(t *testing.T) {
	w := newWorldOf(t, subPath, five, capped(2))
	tip := w.f.Publish(branchMain, gone(fileA, fileB, fileC))
	// The application took one away itself: the update removes two files from the directory.
	if err := os.Remove(filepath.Join(w.l.WorkTree, fileA)); err != nil {
		t.Fatal(err)
	}

	got, err := w.pull(t)

	if err != nil || got.To != tip {
		t.Fatalf("result = %+v, %v, want Advanced: of the three files of the update only two are in the directory", got, err)
	}
	for _, name := range []string{fileA, fileB, fileC} {
		if gittest.Exists(filepath.Join(w.l.WorkTree, name)) {
			t.Errorf("%s is still there", name)
		}
	}
}

func TestPullApprovesAnUpdateThatEmptiesTheBranchOnceThePersonHasRemovedTheFiles(t *testing.T) {
	w := newWorld(t, subPath)
	tip := w.f.Publish(branchMain, gone(fileA, fileB))
	if err := os.Remove(filepath.Join(w.l.WorkTree, fileA)); err != nil {
		t.Fatal(err)
	}

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "1 file(s)") || !strings.Contains(got.Message, fileB) || strings.Contains(got.Message, fileA) {
		t.Errorf("message = %q, want the one file that is left, and not the one that is gone", got.Message)
	}
	if err := os.Remove(filepath.Join(w.l.WorkTree, fileB)); err != nil {
		t.Fatal(err)
	}

	result, err := w.pull(t)

	if err != nil || result.Outcome != Advanced || result.To != tip {
		t.Errorf("result = %+v, %v, want Advanced to %s: there is nothing left for the update to remove", result, err, tip)
	}
}

func TestPullCountsAFileThatMovedAsRemoved(t *testing.T) {
	w := newWorld(t, subPath, capped(1))
	w.f.Publish(branchMain, map[string]string{fileA: "", fileB: "", "x.json": contentA, "y.json": contentB})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "2 file(s)") {
		t.Errorf("message = %q, want two files: a move is a removal and an addition", got.Message)
	}
}

func TestPullDoesNotCapACheckoutThatHasNoCommitYet(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	gittest.Write(t, l, "mine.json", mine)
	if made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil || made.Commit != "" {
		t.Fatalf("checkout = %+v, %v, want one with no commit", made, err)
	}
	p := puller(t, f, l, capped(1))
	tip := f.Publish(branchMain, versionOne)

	got, err := p.Pull(context.Background())

	if err != nil || got.Outcome != Advanced || got.To != tip || got.Recovery != "" {
		t.Fatalf("result = %+v, %v, want Advanced to %s with nothing to keep: a checkout with no commit has nothing to remove", got, err, tip)
	}
	if refs := f.InCheckout(l, "for-each-ref", recoveryDir); refs != "" {
		t.Errorf("refs = %q, want none: there was no commit to keep", refs)
	}
}

func TestPullSaysWhenTheFilesOfAnUpdateCannotBeListedAndWritesNothing(t *testing.T) {
	w := newWorld(t, subPath)
	tip := w.f.Publish(branchMain, map[string]string{fileC: contentC})
	// The commit is fetched, so that the pull asks no more of the repository, and then the tree of
	// it is lost.
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	tree := w.f.InCheckout(w.l, "rev-parse", tip+"^{tree}")
	if err := os.Remove(filepath.Join(w.l.GitDir, "objects", tree[:2], tree[2:])); err != nil {
		t.Skipf("the tree is not a loose object, so the test cannot lose it: %v", err)
	}

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "cannot be listed") || got.Err == nil {
		t.Errorf("message = %q, error underneath = %v, want the files that the update removes cannot be listed, with the cause", got.Message, got.Err)
	}
	w.wantAt(t, w.tip)
	w.wantRefs(t)
}

func TestPullSaysWhenTheFilesOfTheBranchCannotBeListedAndWritesNothing(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.Git.Git = breaking(t, o.Git.Git, "ls-tree") })
	// The update removes a file, so the pull goes on to look at what the branch would have left.
	w.f.Publish(branchMain, gone(fileA))

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "files of the branch cannot be listed") || got.Err == nil {
		t.Errorf("message = %q, error underneath = %v, want that the files of the branch cannot be listed, with the cause", got.Message, got.Err)
	}
	w.wantAt(t, w.tip)
	w.wantRefs(t)
}

func TestPullSaysTooManyDeletionsWhenTheNamesDoNotFitTheOutputLimit(t *testing.T) {
	long := map[string]string{}
	names := make([]string, 0, 5)
	for i := range 5 {
		name := fmt.Sprintf("a-file-with-a-long-name-number-%d.json", i)
		names = append(names, name)
		long[name] = "x\n"
	}
	w := newWorldOf(t, subPath, long, func(o *Options) { o.Git.MaxOutput = 100 })
	w.f.Publish(branchMain, gone(names[:4]...))

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "so many files") || !strings.Contains(got.Message, "Nothing was written") ||
		strings.Contains(got.Message, "100") || strings.Contains(got.Message, "remove those files") ||
		!strings.Contains(got.Message, "put the files back") {
		t.Errorf("message = %q, want that so many files go that Treebind cannot list them, with no number, which a cap that was raised would make false, "+
			"and no advice to remove files, which does not help", got.Message)
	}
	w.wantAt(t, w.tip)
}

func TestPullKeepsTheCommitTheCheckoutWasAtBeforeItAppliesAnUpdate(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		first := w.f.Publish(branchMain, map[string]string{fileC: contentC})

		got, err := w.pull(t)

		if err != nil || got.Outcome != Advanced || got.Recovery != firstRef {
			t.Fatalf("result = %+v, %v, want Advanced with the recovery ref %s", got, err, firstRef)
		}
		w.wantRefs(t, ref{firstRef, w.tip})
		second := w.f.Publish(branchMain, map[string]string{fileA: aChanged})

		got, err = w.pull(t)

		if err != nil || got.To != second || got.Recovery != secondRef {
			t.Fatalf("result = %+v, %v, want Advanced to %s with the recovery ref %s", got, err, second, secondRef)
		}
		w.wantRefs(t, ref{firstRef, w.tip}, ref{secondRef, first})
		if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
			t.Errorf("the Git directory is not what the agent makes after the refs: %v", err)
		}
		if gittest.Exists(filepath.Join(w.l.GitDir, "packed-refs")) {
			t.Error("there is a packed-refs, which the agent refuses in its Git directory")
		}
	})
}

func TestPullKeepsNothingWhenThereIsNothingToApply(t *testing.T) {
	w := newWorld(t, subPath)

	got, err := w.pull(t)

	if err != nil || got.Outcome != UpToDate || got.Recovery != "" {
		t.Errorf("result = %+v, %v, want UpToDate with no ref", got, err)
	}
	w.wantRefs(t)
}

func TestPullKeepsTheCommitOnlyAfterTheFenceAndNotWhenTheFenceRefuses(t *testing.T) {
	var w *world
	var seen []ref
	w = newWorld(t, subPath, func(o *Options) {
		o.Fence = func(context.Context) error { seen = w.refs(t); return nil }
	})
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 0 {
		t.Errorf("the refs at the fence = %v, want none: the first write is after the lock is checked", seen)
	}
	w.wantRefs(t, ref{firstRef, w.tip})

	refused := newWorld(t, subPath, func(o *Options) {
		o.Fence = func(context.Context) error { return fmt.Errorf("the lock is lost") }
	})
	refused.f.Publish(branchMain, map[string]string{fileC: contentC})
	_, err := refused.pull(t)
	wantReason(t, err, ReasonRefused)
	refused.wantRefs(t)
}

func TestPullKeepsOneRefForAnUpdateThatIsBlockedAndTriedAgain(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, fileA, edited)
	tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
	for range 3 {
		_, err := w.pull(t)
		wantReason(t, err, ReasonBlocked)
	}
	w.wantRefs(t, ref{firstRef, w.tip})
	// The person puts the file back, and the update goes on.
	gittest.Write(t, w.l, fileA, contentA)

	got, err := w.pull(t)

	if err != nil || got.To != tip || got.Recovery != firstRef {
		t.Fatalf("result = %+v, %v, want Advanced to %s, with the ref that was kept when the update was first tried", got, err, tip)
	}
	w.wantRefs(t, ref{firstRef, w.tip})
}

func TestPullKeepsTheLastRefsAndRemovesTheOlderOnes(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.RecoveryRefs = 3 })
	commits := make([]string, 1, 6)
	commits[0] = w.tip
	for i := range 5 {
		commits = append(commits, w.f.Publish(branchMain, map[string]string{fileA: fmt.Sprintf("version %d\n", i)}))
		if _, err := w.pull(t); err != nil {
			t.Fatal(err)
		}
	}

	// The update n was applied from the commit n-1 of the list, and the ref of it is numbered n.
	w.wantRefs(t,
		ref{recoveryDir + "00000003", commits[2]},
		ref{recoveryDir + "00000004", commits[3]},
		ref{recoveryDir + "00000005", commits[4]})
	if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
		t.Errorf("the Git directory is not what the agent makes after refs were removed: %v", err)
	}
	if gittest.Exists(filepath.Join(w.l.GitDir, "packed-refs")) {
		t.Error("there is a packed-refs after refs were removed, which the agent refuses in its Git directory")
	}
}

func TestPullLeavesARefOfAPersonInTheNamespaceAlone(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.RecoveryRefs = 1 })
	pinned := recoveryDir + "pinned"
	w.f.InCheckout(w.l, "update-ref", pinned, w.tip)
	for i := range 3 {
		w.f.Publish(branchMain, map[string]string{fileA: fmt.Sprintf("version %d\n", i)})
		if _, err := w.pull(t); err != nil {
			t.Fatal(err)
		}
	}

	got := w.refs(t)

	if len(got) != 2 || got[0].name != recoveryDir+"00000003" || got[1] != (ref{pinned, w.tip}) {
		t.Errorf("recovery refs = %v, want the newest of the agent's and the one of the person, and the count to be of the agent's alone", got)
	}
}

func TestPullNumbersTheNextRefAfterTheHighestNumberAndNotTheLastName(t *testing.T) {
	w := newWorld(t, subPath)
	// Two refs whose names sort the other way round than their numbers do.
	w.f.InCheckout(w.l, "update-ref", recoveryDir+"9", w.tip)
	w.f.InCheckout(w.l, "update-ref", recoveryDir+"00000010", w.tip)
	first := w.f.Publish(branchMain, map[string]string{fileC: contentC})

	got, err := w.pull(t)

	// The ref at the highest number already keeps this commit, so it is the one that is named.
	if err != nil || got.To != first || got.Recovery != recoveryDir+"00000010" {
		t.Fatalf("result = %+v, %v, want Advanced with %s: it is the highest number and keeps the commit", got, err, recoveryDir+"00000010")
	}
	second := w.f.Publish(branchMain, map[string]string{fileD: contentD})

	got, err = w.pull(t)

	if err != nil || got.To != second || got.Recovery != recoveryDir+"00000011" {
		t.Errorf("result = %+v, %v, want Advanced with %s, the number after the highest", got, err, recoveryDir+"00000011")
	}
}

func TestPullDoesNotApplyAnUpdateWhenItCannotKeepTheCommit(t *testing.T) {
	w := newWorld(t, subPath)
	// A lock of the ref that a process that was killed left behind: Git does not remove it.
	plantLock(t, w, "00000001")
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "recovery ref") || got.Err == nil {
		t.Errorf("message = %q, error underneath = %v, want that the recovery ref cannot be written, with the cause", got.Message, got.Err)
	}
	w.wantAt(t, w.tip)
	if gittest.Exists(filepath.Join(w.l.WorkTree, fileC)) {
		t.Error("the update was applied although its commit could not be kept")
	}
}

func TestPullAppliesAnUpdateAndSaysWhenAnOlderRefCannotBeRemoved(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.RecoveryRefs = 1 })
	first := w.f.Publish(branchMain, map[string]string{fileC: contentC})
	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}
	plantLock(t, w, "00000001")
	second := w.f.Publish(branchMain, map[string]string{fileD: contentD})

	got, err := w.pull(t)

	// Tidying the refs is not what the pull is for, and the ref that keeps the commit is written.
	if err != nil || got.To != second || got.Recovery != secondRef {
		t.Fatalf("result = %+v, %v, want Advanced to %s with %s: a ref that cannot be removed does not stop the update", got, err, second, secondRef)
	}
	if !strings.Contains(got.Warning, "recovery ref "+firstRef+" cannot be removed") {
		t.Errorf("warning = %q, want that the old ref cannot be removed, with its name", got.Warning)
	}
	w.wantRefs(t, ref{firstRef, w.tip}, ref{secondRef, first})
	if err := os.Remove(filepath.Join(w.l.GitDir, filepath.FromSlash(firstRef)+".lock")); err != nil {
		t.Fatal(err)
	}
	w.f.Publish(branchMain, map[string]string{fileE: contentE})

	got, err = w.pull(t)

	if err != nil || got.Warning != "" {
		t.Fatalf("result = %+v, %v, want Advanced and no warning once the lock is gone", got, err)
	}
	w.wantRefs(t, ref{recoveryDir + "00000003", second})
}

func TestPullDoesNotTidyTheRefsWhenTheUpdateIsNotApplied(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.RecoveryRefs = 1 })
	w.f.InCheckout(w.l, "update-ref", firstRef, w.tip)
	w.f.InCheckout(w.l, "update-ref", secondRef, w.tip)
	gittest.Write(t, w.l, fileA, edited)
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	wantReason(t, err, ReasonBlocked)
	// The ref that keeps the commit is there, and the old ones are tidied after an update that was applied.
	w.wantRefs(t, ref{firstRef, w.tip}, ref{secondRef, w.tip})
	gittest.Write(t, w.l, fileA, contentA)

	got, err := w.pull(t)

	// The ref that keeps the commit is the one of the try before, and the update tidies the older ones.
	if err != nil || got.Recovery != secondRef {
		t.Fatalf("result = %+v, %v, want Advanced with %s", got, err, secondRef)
	}
	w.wantRefs(t, ref{secondRef, w.tip})
}

func TestPullDoesNotTakeANameThatIsTooBigForANumberOfItsOwnForARefOfItsOwn(t *testing.T) {
	w := newWorld(t, subPath, func(o *Options) { o.RecoveryRefs = 1 })
	huge := recoveryDir + "18446744073709551615"
	w.f.InCheckout(w.l, "update-ref", huge, w.tip)
	for i := range 2 {
		w.f.Publish(branchMain, map[string]string{fileA: fmt.Sprintf("version %d\n", i)})
		if _, err := w.pull(t); err != nil {
			t.Fatal(err)
		}
	}

	got := w.refs(t)

	if len(got) != 2 || got[0].name != secondRef || got[1] != (ref{huge, w.tip}) {
		t.Errorf("recovery refs = %v, want the name that is too big left alone and counted for nothing, and the agent's at %s", got, secondRef)
	}
}

func TestPullCountsAFileThatItCannotLookAtAsOneThatIsThere(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can look at any file, so there is no directory that it cannot")
	}
	w := newWorldOf(t, subPath, map[string]string{"dir/a.json": contentA, "dir/b.json": contentB, "dir/c.json": contentC, fileD: contentD}, capped(2))
	w.f.Publish(branchMain, gone("dir/a.json", "dir/b.json", "dir/c.json"))
	dir := filepath.Join(w.l.WorkTree, "dir")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "3 file(s)") {
		t.Errorf("message = %q, want the three files counted: a failure to look is not a file that is gone", got.Message)
	}
}

func TestPullDoesNotCountAFileUnderSomethingThatIsNotADirectory(t *testing.T) {
	w := newWorldOf(t, subPath, map[string]string{"dir/a.json": contentA, "dir/b.json": contentB, "dir/c.json": contentC, fileD: contentD}, capped(2))
	w.f.Publish(branchMain, gone("dir/a.json", "dir/b.json", "dir/c.json"))
	// The application made a file of what was a directory: the three files are not there.
	if err := os.RemoveAll(filepath.Join(w.l.WorkTree, "dir")); err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, w.l, "dir", mine)

	_, err := w.pull(t)

	var got *Error
	if errors.As(err, &got) && got.Reason == ReasonTooManyDeletions {
		t.Errorf("error = %v, want no refusal for files that are not in the directory", err)
	}
}

func TestPullDoesNotApplyAnUpdateWhenTheRefsCannotBeListed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld(t, subPath, func(o *Options) {
		// The fence is the last thing before the refs are looked at.
		o.Fence = func(context.Context) error { cancel(); return nil }
	})
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	_, err := w.puller.Pull(ctx)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "recovery refs cannot be looked at") {
		t.Errorf("message = %q, want that the recovery refs cannot be looked at", got.Message)
	}
	w.wantAt(t, w.tip)
}

func TestGitRefusesToOverwriteARefWhenTheOldValueIsEmpty(t *testing.T) {
	// The pull gives update-ref an empty old value, and relies on Git to refuse a ref that is there.
	w := newWorld(t, subPath)
	other := w.f.Publish(branchMain, map[string]string{fileC: contentC})
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	w.f.InCheckout(w.l, "update-ref", firstRef, w.tip, "")

	_, err := w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree},
		"update-ref", firstRef, other, "")

	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want Git to refuse a ref that exists", err)
	}
	w.wantRefs(t, ref{firstRef, w.tip})
}

func TestNewFillsInTheCapAndTheNumberOfRefs(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)

	p, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions()})

	if err != nil || p.o.MaxDeletions != DefaultMaxDeletions || p.o.RecoveryRefs != DefaultRecoveryRefs {
		t.Errorf("cap = %d, refs = %d, %v, want %d and %d", p.o.MaxDeletions, p.o.RecoveryRefs, err, DefaultMaxDeletions, DefaultRecoveryRefs)
	}
	if DefaultMaxDeletions != 100 || DefaultRecoveryRefs != 20 {
		t.Errorf("the defaults are %d and %d, which the docs and ADR 0026 give as 100 and 20", DefaultMaxDeletions, DefaultRecoveryRefs)
	}
}

// breaking returns a git that fails the subcommand, and runs the real one for every other.
func breaking(t *testing.T, real, subcommand string) string {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do\n\tif [ \"$a\" = %s ]; then echo 'broken on purpose' >&2; exit 3; fi\ndone\nexec %s \"$@\"\n",
		subcommand, real)
	path := filepath.Join(gittest.TempDir(t), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// plantLock leaves the lock file that Git makes next to a recovery ref, as a process that was killed
// in the middle of writing it would.
func plantLock(t *testing.T, w *world, name string) {
	t.Helper()
	dir := filepath.Join(w.l.GitDir, filepath.FromSlash(recoveryDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
