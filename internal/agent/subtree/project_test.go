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

package subtree_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

// projector makes the subtree.Projector of the repository for a directory, with the settings of the fixture and no author of its own,
// as the pull of a Treebind that only pulls has none.
func (r *repo) projector(dir string) *subtree.Projector {
	r.t.Helper()
	p, err := subtree.New(subtree.Options{Layout: r.l, Git: r.f.GitOptions(), Path: dir, Branch: branchMain})
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// commitOf is the commit that a ref names, or "" when there is none.
func (r *repo) commitOf(ref string) string {
	r.t.Helper()
	out, err := r.runner.Run(context.Background(), "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out.Stdout))
}

func (r *repo) parents(commit string) []string {
	r.t.Helper()
	return strings.Fields(r.run("rev-list", "--parents", "--max-count=1", commit))[1:]
}

// seeded is a repository with the files of every test.
func seeded(t *testing.T) *gittest.Fixture {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{
		fileX:                           contentD1,
		"grafana/dashboards/sub/s.json": "s1\n",
		fileAlerts:                      "a1\n",
		fileOther:                       "o1\n",
		fileReadme:                      contentReadme,
	})
	return f
}

var _ = git.Options{}

func TestTheFirstProjectionIsASnapshotOfTheDirectoryWithNoParent(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	real := r.fetchReal()
	got, err := r.projector(dashboards).Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Made || got.Real != real || got.Snapshot == "" {
		t.Fatalf("projection = %+v, want a snapshot made of %s", got, real)
	}
	if tree := r.run("rev-parse", got.Snapshot+"^{tree}"); tree != r.run("rev-parse", real+":"+dashboards) {
		t.Errorf("the snapshot's tree is %s, not the tree of the directory", tree)
	}
	if parents := r.parents(got.Snapshot); len(parents) != 0 {
		t.Errorf("the first snapshot has parents %v", parents)
	}
	if held := r.commitOf(subtree.TrackingRef(branchMain)); held != got.Snapshot {
		t.Errorf("%s holds %s, want the snapshot %s", subtree.TrackingRef(branchMain), held, got.Snapshot)
	}
	message := r.run("cat-file", "commit", got.Snapshot)
	if !strings.Contains(message, subtree.TrailerSource+": "+real) {
		t.Errorf("the snapshot does not say where it comes from:\n%s", message)
	}
}

func TestAChangeOutsideTheDirectoryMakesNoSnapshotAndOneInsideMakesOneOnTheLast(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	r.fetchReal()
	p := r.projector(dashboards)
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	f.Publish(branchMain, map[string]string{fileOther: contentOther2})
	real := r.fetchReal()
	again, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Made || again.Snapshot != first.Snapshot {
		t.Errorf("a change outside the directory gave %+v, want the snapshot %s as it was", again, first.Snapshot)
	}
	if again.Real != real {
		t.Errorf("the real tip is %s, want %s", again.Real, real)
	}

	f.Publish(branchMain, map[string]string{fileX: "d2\n"})
	real = r.fetchReal()
	next, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !next.Made || next.Snapshot == first.Snapshot {
		t.Fatalf("a change inside the directory gave %+v, want a new snapshot", next)
	}
	if parents := r.parents(next.Snapshot); len(parents) != 1 || parents[0] != first.Snapshot {
		t.Errorf("the new snapshot has parents %v, want the first one %s", parents, first.Snapshot)
	}
	if got := r.run("rev-parse", next.Snapshot+"^{tree}"); got != r.run("rev-parse", real+":"+dashboards) {
		t.Errorf("the new snapshot's tree is not the tree of the directory at %s", real)
	}
	if r.commitOf(subtree.TrackingRef(branchMain)) != next.Snapshot {
		t.Error("the ref of the snapshot was not moved")
	}
}

// A branch that was rewritten is not a continuation of the one the snapshots were made from, and its snapshot says so by having
// no parent: the pull then sees two histories that have nothing in common, which it says as it does for a force-push of the
// whole repository (docs/adr/0020).
func TestASnapshotOfARewrittenBranchHasNoParent(t *testing.T) {
	f := seeded(t)
	f.Publish(branchMain, map[string]string{fileOther: contentOther2})
	r := newRepo(t, f)
	r.fetchReal()
	p := r.projector(dashboards)
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	f.Rewrite()
	f.Publish(branchMain, map[string]string{fileX: "rewritten\n"})
	r.fetchReal()
	next, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !next.Made || next.Snapshot == first.Snapshot {
		t.Fatalf("projection = %+v, want a new snapshot", next)
	}
	if parents := r.parents(next.Snapshot); len(parents) != 0 {
		t.Errorf("the snapshot of the rewritten branch has parents %v", parents)
	}
}

// A directory that the repository does not have is a typo as often as it is a directory that is to come, and the agent would
// commit what the application writes into a new directory of the repository for it: the first snapshot is refused, with a reason
// of its own. After there has been a snapshot a directory that the repository removes is a snapshot with no file, which the
// pull's guard on deletions then judges.
func TestADirectoryThatTheRepositoryDoesNotHaveIsRefusedTheFirstTimeAndAnEmptySnapshotAfter(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	r.fetchReal()

	_, err := r.projector("grafana/nope").Project(context.Background())
	refusal, ok := errors.AsType[*subtree.Error](err)
	if !ok || refusal.Reason != subtree.ReasonPathNotFound || !strings.Contains(refusal.Message, "grafana/nope") {
		t.Fatalf("err = %v, want the refusal %s that names the directory", err, subtree.ReasonPathNotFound)
	}
	if held := r.commitOf(subtree.TrackingRef(branchMain)); held != "" {
		t.Errorf("a refused projection left %s at %s", subtree.TrackingRef(branchMain), held)
	}

	p := r.projector(dashboards)
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.Publish(branchMain, map[string]string{fileX: "", "grafana/dashboards/sub/s.json": ""})
	r.fetchReal()
	gone, err := p.Project(context.Background())
	if err != nil {
		t.Fatalf("a directory that the repository removed is a snapshot with no file, got %v", err)
	}
	if !gone.Made || r.run("rev-parse", gone.Snapshot+"^{tree}") != subtree.EmptyTree {
		t.Errorf("projection = %+v, want a new snapshot with the empty tree", gone)
	}
	if parents := r.parents(gone.Snapshot); len(parents) != 1 || parents[0] != first.Snapshot {
		t.Errorf("the empty snapshot has parents %v, want %s", parents, first.Snapshot)
	}
}

// What is at the path has to be a directory: a file, a link and a submodule cannot be the root of a synced directory.
func TestSomethingThatIsNotADirectoryAtThePathIsRefused(t *testing.T) {
	f := seeded(t)
	f.PublishLink(branchMain, "grafana/link", "dashboards")
	r := newRepo(t, f)
	r.fetchReal()
	for _, dir := range []string{fileReadme, "grafana/link", fileX} {
		_, err := r.projector(dir).Project(context.Background())
		refusal, ok := errors.AsType[*subtree.Error](err)
		if !ok || refusal.Reason != subtree.ReasonPathNotFound || !strings.Contains(refusal.Message, "not a directory") {
			t.Errorf("%s: err = %v, want the refusal %s that says it is not a directory", dir, err, subtree.ReasonPathNotFound)
		}
	}
}

// A checkout of the whole repository, which a Treebind that sets the path today has, and one that was made for another
// directory, hold files that a snapshot of this directory would write over or remove. Nothing is changed, and the refusal says
// which of the two it is (FR-18).
func TestACheckoutMadeForAnotherDirectoryOrForNoneIsRefusedAndLeftAsItIs(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	real := r.fetchReal()

	// the whole repository: the ref of the snapshot holds the real commit, as the checkout of today leaves it
	r.run("update-ref", subtree.TrackingRef(branchMain), real)
	_, err := r.projector(dashboards).Project(context.Background())
	refusal, ok := errors.AsType[*subtree.Error](err)
	if !ok || refusal.Reason != subtree.ReasonPathMismatch || !strings.Contains(refusal.Message, "whole repository") {
		t.Fatalf("err = %v, want %s that says the checkout is of the whole repository", err, subtree.ReasonPathMismatch)
	}
	if held := r.commitOf(subtree.TrackingRef(branchMain)); held != real {
		t.Errorf("the refusal moved %s to %s", subtree.TrackingRef(branchMain), held)
	}

	// another directory
	r.run("update-ref", "-d", subtree.TrackingRef(branchMain))
	if _, err := r.projector("grafana/alerts").Project(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = r.projector(dashboards).Project(context.Background())
	refusal, ok = errors.AsType[*subtree.Error](err)
	if !ok || refusal.Reason != subtree.ReasonPathMismatch || !strings.Contains(refusal.Message, "grafana/alerts") || !strings.Contains(refusal.Message, dashboards) {
		t.Errorf("err = %v, want %s that names both directories", err, subtree.ReasonPathMismatch)
	}
}

func TestAProjectionOfABranchThatWasNotFetchedIsAnError(t *testing.T) {
	r := newRepo(t, seeded(t))
	if _, err := r.projector(dashboards).Project(context.Background()); err == nil {
		t.Error("want an error")
	}
}

// The ref of the repository's tip moves when the snapshot of it has been made, and not before: a projection that is refused or that
// stops leaves the tip where it was, so that the pull, which asks the repository for its tip and fetches only when it is not the one
// that is held, finds the update again.
func TestARefusedProjectionLeavesTheRefOfTheRepositoryTipWhereItWas(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	r.fetchReal()
	p := r.projector("grafana/alerts")
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held := r.commitOf(subtree.RealRef(branchMain)); held != first.Real {
		t.Fatalf("%s holds %s after the projection, want the tip %s", subtree.RealRef(branchMain), held, first.Real)
	}

	f.Publish(branchMain, map[string]string{fileAlerts: ""})
	f.PublishLink(branchMain, "grafana/alerts", "elsewhere")
	r.fetchReal()
	if _, err := p.Project(context.Background()); err == nil {
		t.Fatal("a directory that became a link was projected")
	}

	if held := r.commitOf(subtree.RealRef(branchMain)); held != first.Real {
		t.Errorf("%s was moved to %s by a projection that was refused, want %s", subtree.RealRef(branchMain), held, first.Real)
	}
	if held := r.commitOf(subtree.TrackingRef(branchMain)); held != first.Snapshot {
		t.Errorf("%s was moved to %s by a projection that was refused", subtree.TrackingRef(branchMain), held)
	}
}

// A change of the repository outside the directory moves the ref of the repository's tip, and nothing else.
func TestAChangeOutsideTheDirectoryMovesOnlyTheRefOfTheRepositoryTip(t *testing.T) {
	f := seeded(t)
	r := newRepo(t, f)
	r.fetchReal()
	p := r.projector(dashboards)
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.Publish(branchMain, map[string]string{fileOther: contentOther2})
	tip := r.fetchReal()

	again, err := p.Project(context.Background())

	if err != nil || again.Made || again.Snapshot != first.Snapshot {
		t.Fatalf("Project = %+v, %v; want the snapshot as it was", again, err)
	}
	if held := r.commitOf(subtree.RealRef(branchMain)); held != tip {
		t.Errorf("%s holds %s, want the tip %s", subtree.RealRef(branchMain), held, tip)
	}
}

func TestAProjectorIsRefusedWhatCannotWork(t *testing.T) {
	r := newRepo(t, seeded(t))
	for name, o := range map[string]subtree.Options{
		"no path":                     {Layout: r.l, Git: r.f.GitOptions(), Branch: branchMain},
		"no branch":                   {Layout: r.l, Git: r.f.GitOptions(), Path: dashboards},
		"a path that the CRD refuses": {Layout: r.l, Git: r.f.GitOptions(), Path: "a/../b", Branch: branchMain},
	} {
		if _, err := subtree.New(o); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
