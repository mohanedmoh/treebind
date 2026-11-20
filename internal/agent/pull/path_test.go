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
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

const (
	dirPath     = "grafana/dashboards"
	fileOutside = "other/z.txt"
)

// repoFiles are the files of a repository that has the directory of the tests, another one and files outside both.
var repoFiles = map[string]string{
	dirPath + "/" + fileA: contentA,
	dirPath + "/" + fileB: contentB,
	"grafana/alerts/x":    "x\n",
	fileOutside:           "z\n",
	"README.md":           "readme\n",
}

// directoryWorld is a repository, the checkout of one directory of it on a claim, and a Puller for it.
type directoryWorld struct {
	*world
}

func newDirectoryWorld(t *testing.T, sub string) *directoryWorld {
	t.Helper()
	f := gittest.New(t)
	tip := f.Publish(branchMain, repoFiles)
	l := f.Layout(sub)
	opened := f.CheckoutOptions(l, branchMain)
	opened.Path = dirPath
	if made, err := checkout.Open(context.Background(), opened); err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	return &directoryWorld{&world{f: f, l: l, puller: puller(t, f, l, func(o *Options) { o.Path = dirPath }), tip: tip}}
}

func (w *directoryWorld) refs(t *testing.T) string {
	t.Helper()
	return w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname) %(objectname)")
}

// A change that the repository makes in the directory is brought in at the root of the synced directory, and the snapshot is the
// one that the checkout is at.
func TestAPullBringsInWhatTheRepositoryChangesInTheDirectoryAtTheRootOfTheSyncedDirectory(t *testing.T) {
	for _, sub := range []string{subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			w := newDirectoryWorld(t, sub)
			before := w.f.InCheckout(w.l, "rev-parse", "HEAD")
			tip := w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: aChanged, dirPath + "/" + fileC: contentC})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Advanced {
				t.Fatalf("Pull = %+v, %v; want Advanced", got, err)
			}
			if gittest.Read(t, w.l, fileA) != aChanged || gittest.Read(t, w.l, fileC) != contentC {
				t.Error("the files of the update are not at the root of the synced directory")
			}
			if gittest.Exists(w.l.WorkTree + "/grafana") {
				t.Error("the layout of the repository was written into the synced directory")
			}
			if got.From != before || got.To != w.f.InCheckout(w.l, "rev-parse", "HEAD") || got.To == tip {
				t.Errorf("from %s to %s; want the snapshots, from %s to the one at HEAD and not the repository's %s", got.From, got.To, before, tip)
			}
			if got.ToSource != tip {
				t.Errorf("ToSource = %s, want the repository's tip %s", got.ToSource, tip)
			}
			if got.FromSource != w.tip {
				t.Errorf("FromSource = %s, want the commit the checkout was made at, %s", got.FromSource, w.tip)
			}
			if dirty := w.dirty(t); dirty != "" {
				t.Errorf("the checkout is not clean after the pull: %s", dirty)
			}
		})
	}
}

// A change outside the directory is no change of the volume: the repository's tip is kept as it is, no snapshot is made and
// nothing is applied, and the next pull asks the repository one question and fetches nothing.
func TestAPullOfAChangeOutsideTheDirectoryAppliesNothingAndTheNextOneFetchesNothing(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	snapshot := w.f.InCheckout(w.l, "rev-parse", subtree.TrackingRef(branchMain))
	tip := w.f.Publish(branchMain, map[string]string{fileOutside: "changed\n", "README.md": "new readme\n"})

	got, err := w.pull(t)

	if err != nil || got.Outcome != UpToDate {
		t.Fatalf("Pull = %+v, %v; want UpToDate", got, err)
	}
	if now := w.f.InCheckout(w.l, "rev-parse", subtree.TrackingRef(branchMain)); now != snapshot {
		t.Errorf("a snapshot was made for a change outside the directory: %s", now)
	}
	if real := w.f.InCheckout(w.l, "rev-parse", subtree.RealRef(branchMain)); real != tip || got.ToSource != tip {
		t.Errorf("the repository's tip is kept as %s and reported as %s, want %s", real, got.ToSource, tip)
	}

	before, refs := w.f.Sessions(), w.refs(t)
	if again, err := w.pull(t); err != nil || again.Outcome != UpToDate || again.ToSource != tip {
		t.Fatalf("the next Pull = %+v, %v; want UpToDate at %s", again, err, tip)
	}
	if asked := w.f.Sessions() - before; asked != 1 {
		t.Errorf("the next pull reached the repository %d times, want 1: a question and no fetch", asked)
	}
	if w.refs(t) != refs {
		t.Error("a pull that had nothing to do moved a ref")
	}
}

func TestAPullIsRefusedByAnEditThatTheUpdateChanges(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	gittest.Write(t, w.l, fileA, edited)
	w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: aChanged})

	_, err := w.pull(t)

	wantReason(t, err, ReasonBlocked)
	if gittest.Read(t, w.l, fileA) != edited {
		t.Error("the edit of the application was written over")
	}
}

// A repository that removes the directory is a snapshot with no file, which the guard on deletions refuses as it does a branch
// that was emptied, and every file stays where it is.
func TestAPullThatRemovesTheDirectoryIsRefusedByTheGuardOnDeletions(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: "", dirPath + "/" + fileB: ""})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonTooManyDeletions)
	if !strings.Contains(got.Message, "no file at all") {
		t.Errorf("message = %q, want it to say that the branch is left with no file", got.Message)
	}
	if gittest.Read(t, w.l, fileA) != contentA || gittest.Read(t, w.l, fileB) != contentB {
		t.Error("the files were removed")
	}
}

func TestAPullWhoseDirectoryBecameAFileIsRefusedWithTheReasonOfTheDirectory(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: "", dirPath + "/" + fileB: ""})
	w.f.Plain(w.l.WorkTree, "--version")
	// the directory is replaced by a file of the same name
	w.f.PublishLink(branchMain, dirPath, "elsewhere")

	_, err := w.pull(t)

	wantReason(t, err, subtree.ReasonPathNotFound)
}

// The pull of a checkout that was made for another directory is refused before anything is fetched into the volume.
func TestAPullOfACheckoutMadeForAnotherDirectoryIsRefusedAndWritesNothing(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	w.puller = puller(t, w.f, w.l, func(o *Options) { o.Path = "grafana/alerts" })
	w.f.Publish(branchMain, map[string]string{"grafana/alerts/x": "changed\n"})
	snapshot, branch := w.f.InCheckout(w.l, "rev-parse", subtree.TrackingRef(branchMain)), w.f.InCheckout(w.l, "rev-parse", "HEAD")

	_, err := w.pull(t)

	wantReason(t, err, subtree.ReasonPathMismatch)
	if gittest.Read(t, w.l, fileA) != contentA {
		t.Error("a file was written")
	}
	// the fetch is the one thing that is allowed to have moved: it is of the repository and not of the volume
	if now := w.f.InCheckout(w.l, "rev-parse", subtree.TrackingRef(branchMain)); now != snapshot {
		t.Errorf("the ref of the snapshots was moved to %s", now)
	}
	if now := w.f.InCheckout(w.l, "rev-parse", "HEAD"); now != branch {
		t.Errorf("the branch of the checkout was moved to %s", now)
	}
}

// A file named like the state directory in the directory is refused when the update brings it into a synced directory that
// holds the state directory, as a branch that has one at its root is.
func TestAPullThatBringsTheStateDirectoryIntoTheDirectoryIsRefused(t *testing.T) {
	l, err := layout.New(gittest.TempDir(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	f := gittest.New(t)
	f.Publish(branchMain, repoFiles)
	opened := f.CheckoutOptions(l, branchMain)
	opened.Path = dirPath
	if _, err := checkout.Open(context.Background(), opened); err != nil {
		t.Fatal(err)
	}
	p := puller(t, f, l, func(o *Options) { o.Path = dirPath })
	f.Publish(branchMain, map[string]string{dirPath + "/.treebind/x": "1\n"})

	_, err = p.Pull(context.Background())

	wantReason(t, err, ReasonReservedPath)
}

func TestAPullerRefusesAPathTheCRDRefuses(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	for _, path := range []string{"/abs", "a/../b", "a//b", ".git/x", "a/"} {
		if _, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Path: path}); err == nil {
			t.Errorf("New accepted the path %q", path)
		}
	}
}

// A checkout that has a commit of the application's and a directory that the repository changed are resolved by the policy as
// they are for the whole repository: the commits are of the snapshots, and nothing in the resolution knows the difference.
func TestAConflictInTheDirectoryIsResolvedByThePolicy(t *testing.T) {
	tests := []struct {
		policy Policy
		// what the volume holds of a.json after, which both sides changed, and whether the commit of the application is kept
		wantA   string
		outcome Outcome
	}{
		{FollowRemote, "from the repository\n", Resolved},
		{FollowLocal, "from the volume\n", Resolved},
	}
	for _, tt := range tests {
		t.Run(string(tt.policy), func(t *testing.T) {
			w := newDirectoryWorld(t, subPath)
			w.puller = puller(t, w.f, w.l, func(o *Options) { o.Path = dirPath }, resolving(tt.policy))
			gittest.Write(t, w.l, fileA, "from the volume\n")
			gittest.Write(t, w.l, "mine.json", mine)
			w.f.InCheckout(w.l, "add", "-A")
			w.f.InCheckout(w.l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "local")
			w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: "from the repository\n", dirPath + "/" + fileC: contentC})

			got, err := w.pull(t)

			if err != nil || got.Outcome != tt.outcome {
				t.Fatalf("Pull = %+v, %v; want %s", got, err, tt.outcome)
			}
			if gittest.Read(t, w.l, fileA) != tt.wantA {
				t.Errorf("a.json = %q, want %q", gittest.Read(t, w.l, fileA), tt.wantA)
			}
			if gittest.Read(t, w.l, "mine.json") != mine || gittest.Read(t, w.l, fileC) != contentC {
				t.Error("a file that only one side changed was not kept")
			}
			if got.ToSource == "" {
				t.Error("the resolution does not name the commit of the repository")
			}
		})
	}
}

func TestADirectoryWithNoPolicyDivergesAsTheWholeRepositoryDoes(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	gittest.Write(t, w.l, "mine.json", mine)
	w.f.InCheckout(w.l, "add", "-A")
	w.f.InCheckout(w.l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "local")
	w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileC: contentC})

	_, err := w.pull(t)

	wantReason(t, err, ReasonDiverged)
}

// A branch that the repository rewinds to an older commit is `Ahead` for the whole repository, which has the newer commits in
// its history. The snapshot of the older directory is not a continuation of the newer one, so for a directory it is two histories
// that have nothing in common, and the page about the directory says so.
func TestABranchRewoundToAnOlderCommitIsDivergedWhenTheDirectoryDiffers(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	older := w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: aChanged})
	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("Pull = %+v, %v; want Advanced to the older commit", got, err)
	}
	w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileB: bChanged})
	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("Pull = %+v, %v; want Advanced to the newer commit", got, err)
	}
	// the repository goes back to the older commit
	w.f.Plain(w.f.Bare, "update-ref", "refs/heads/"+branchMain, older)

	_, err := w.pull(t)

	wantReason(t, err, ReasonDiverged)
	if gittest.Read(t, w.l, fileB) != bChanged {
		t.Error("the volume was written")
	}
}

// The agent is stopped after the snapshot of a fetched tip was made and before the ref of the snapshots moved. The ref of the
// repository's tip must not be ahead of the snapshot that is of it: the pull asks the repository for its tip and fetches only when it
// is not the one that the checkout holds, so a tip that is held with no snapshot of it would be taken for a pull that has nothing to
// do, and the update would never be brought in.
func TestAPullThatWasStoppedBeforeTheSnapshotWasSetFindsTheUpdateAtTheNextOne(t *testing.T) {
	w := newDirectoryWorld(t, subPath)
	w.puller = puller(t, w.f, w.l, func(o *Options) {
		o.Path = dirPath
		o.Git.Git = gittest.KilledAfter(t, "commit-tree", 1)
	})
	tip := w.f.Publish(branchMain, map[string]string{dirPath + "/" + fileA: aChanged})

	if _, err := w.pull(t); err == nil {
		t.Fatal("the pull that was stopped in the middle of the projection worked")
	}

	// the next pass, with the Git of the agent again
	w.puller = puller(t, w.f, w.l, func(o *Options) { o.Path = dirPath })
	got, err := w.pull(t)
	if err != nil || got.Outcome != Advanced || got.ToSource != tip {
		t.Fatalf("Pull = %+v, %v; want Advanced to the update that the stopped pull fetched", got, err)
	}
	if gittest.Read(t, w.l, fileA) != aChanged {
		t.Error("the update was not brought in")
	}
}
