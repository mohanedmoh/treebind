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

var botAuthor = git.Identity{Name: "treebind-bot", Email: "treebind-bot@treebind.invalid"}

// translator is the subtree.Projector of the repository for the directory of the tests, with the author of the commits of the agent.
func (r *repo) translator() *subtree.Projector {
	r.t.Helper()
	p, err := subtree.New(subtree.Options{Layout: r.l, Git: r.f.GitOptions(), Path: dashboards, Branch: branchMain, Author: botAuthor})
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// checkoutSnapshot makes the checkout of the snapshot: the files in the synced directory and the branch at the snapshot.
func (r *repo) checkoutSnapshot(snapshot string) {
	r.t.Helper()
	r.run("checkout", "--quiet", "--force", "-b", branchMain, snapshot)
}

// local is a commit of the application's changes on the checkout, as the agent makes one: the files are written, staged by name
// and committed with the message.
func (r *repo) local(files map[string]string, message string) string {
	r.t.Helper()
	for name, content := range files {
		if content == "" {
			r.f.InCheckout(r.l, "rm", "--quiet", "--", name)
			continue
		}
		gittest.Write(r.t, r.l, name, content)
	}
	r.f.InCheckout(r.l, "add", "--all")
	r.f.InCheckout(r.l, "commit", "--quiet", "--cleanup=verbatim", "--allow-empty", "--message="+message)
	return r.f.InCheckout(r.l, "rev-parse", "HEAD")
}

// projected is a repository that has been fetched, projected and checked out, with the translator ready.
func projected(t *testing.T) (*repo, *subtree.Projector, subtree.Projection) {
	t.Helper()
	r := newRepo(t, seeded(t))
	r.fetchReal()
	p := r.translator()
	first, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.checkoutSnapshot(first.Snapshot)
	return r, p, first
}

// One commit of the application is one commit of the repository, on the tip of the repository, with the tree of the repository
// and the directory replaced, the message as it was written and the author of the agent.
func TestACommitOnTheSnapshotIsACommitOnTheRepositoryWithTheDirectoryReplaced(t *testing.T) {
	r, p, first := projected(t)
	message := "treebind: sync 2 file(s)\n\n\nwith  blank lines and spaces at the end of a line  \n\n"
	head := r.local(map[string]string{fileX[len(dashboards)+1:]: "local\n", "deep/new.json": "n\n"}, message)

	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 1 || got.Tip == first.Real {
		t.Fatalf("translation = %+v, want one commit on top of %s", got, first.Real)
	}
	if parents := r.parents(got.Tip); len(parents) != 1 || parents[0] != first.Real {
		t.Errorf("the commit has parents %v, want the tip %s", parents, first.Real)
	}
	root := r.run("rev-parse", first.Real+"^{tree}")
	if want := r.truthReplace(root, dashboards, r.run("rev-parse", head+"^{tree}")); r.run("rev-parse", got.Tip+"^{tree}") != want {
		t.Error("the tree is not the tree of the repository with the directory replaced")
	}
	if outside := r.run("diff-tree", "-r", "--name-only", "--no-renames", first.Real, got.Tip); strings.Contains(strings.ReplaceAll(outside, dashboards+"/", ""), "other/") ||
		strings.Contains(outside, "README") || strings.Contains(outside, "alerts") {
		t.Errorf("the commit changes files outside the directory:\n%s", outside)
	}
	if got, want := r.message(got.Tip), r.message(head); got != want {
		t.Errorf("the message was changed:\n%q\n%q", got, want)
	}
	raw := r.run("cat-file", "commit", got.Tip)
	for _, role := range []string{"author", "committer"} {
		if !strings.Contains(raw, "\n"+role+" "+botAuthor.Name+" <"+botAuthor.Email+"> ") && !strings.HasPrefix(raw, role+" "+botAuthor.Name) {
			t.Errorf("the %s is not the author of the agent:\n%s", role, raw)
		}
	}
}

// Commits that wait are sent one for one, each on the one before, and each changes only what its own commit changed.
func TestSeveralCommitsAreSentInTheOrderTheyWereMadeEachOnTheLast(t *testing.T) {
	r, p, first := projected(t)
	r.local(map[string]string{fileOne: "1\n"}, "one")
	r.local(map[string]string{"two.json": "2\n"}, "two")
	head := r.local(map[string]string{fileOne: ""}, "three")

	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 3 {
		t.Fatalf("commits = %d, want 3", got.Commits)
	}
	chain := strings.Fields(r.run("rev-list", "--reverse", got.Tip+"^{commit}", "^"+first.Real))
	subjects := make([]string, 0, len(chain))
	for _, c := range chain {
		subjects = append(subjects, r.run("rev-list", "--format=%s", "--max-count=1", c)[len("commit "+c+"\n"):])
	}
	if strings.Join(subjects, ",") != "one,two,three" {
		t.Errorf("the commits are %v, want one, two, three", subjects)
	}
	if names := r.run("ls-tree", "-r", "--name-only", got.Tip+":"+dashboards); !strings.Contains(names, "two.json") || strings.Contains(names, fileOne) {
		t.Errorf("the directory at the tip has:\n%s", names)
	}
}

// What the checkout is at and has not changed is not made again, and an author is not needed for it.
func TestNothingWaitingMakesNoCommitAndNeedsNoAuthor(t *testing.T) {
	r, _, first := projected(t)
	got, err := r.projector(dashboards).Translate(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 0 || got.Tip != first.Real {
		t.Errorf("translation = %+v, want the real tip %s and no commit", got, first.Real)
	}
}

func TestACommitThatWaitsNeedsAnAuthor(t *testing.T) {
	r, _, _ := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, "one")
	_, err := r.projector(dashboards).Translate(context.Background(), head)
	if err == nil || !strings.Contains(err.Error(), "no valid author") {
		t.Errorf("err = %v, want it to say there is no valid author", err)
	}
}

// The repository changed the directory since the snapshot was made: the commits that wait are on a directory that is not the
// one at the tip, and sending them would undo the change. The pull comes first.
func TestACommitIsNotSentOnADirectoryThatTheRepositoryChanged(t *testing.T) {
	r, p, _ := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, "one")
	r.f.Publish(branchMain, map[string]string{fileX: "changed upstream\n"})
	// the repository's tip, moved with no snapshot of it: what a state that the agent does not make looks like
	r.run("update-ref", subtree.RealRef(branchMain), r.fetchReal())
	if _, err := p.Translate(context.Background(), head); !errors.Is(err, subtree.ErrBehind) {
		t.Errorf("err = %v, want subtree.ErrBehind", err)
	}
}

// A repository that has no commit yet takes a commit that has no parent, with the directory in it and nothing else.
func TestTheFirstCommitOfAnEmptyRepositoryHasNoParentAndOnlyTheDirectory(t *testing.T) {
	f := gittest.New(t)
	r := newRepo(t, f)
	p := r.translator()
	r.run("symbolic-ref", "HEAD", "refs/heads/"+branchMain)
	head := r.local(map[string]string{"first.json": "1\n"}, "first")
	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 1 || len(r.parents(got.Tip)) != 0 {
		t.Fatalf("translation = %+v, parents %v", got, r.parents(got.Tip))
	}
	if names := r.run("ls-tree", "-r", "--name-only", got.Tip); names != dashboards+"/first.json" {
		t.Errorf("the tree has %q, want only the directory with the file", names)
	}
}

// A commit that says it is a snapshot is not sent again: it stands for the commit it is of. A merge of the repository's rewritten
// branch has the old snapshot for a parent through the commits that wait, and gets the commit of the repository that the old
// snapshot stood for.
func TestAMergeAfterARewriteIsMadeOfTheCommitsTheSnapshotsStandFor(t *testing.T) {
	f := seeded(t)
	f.Publish(branchMain, map[string]string{fileOther: contentOther2})
	r := newRepo(t, f)
	oldReal := r.fetchReal()
	p := r.translator()
	old, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.checkoutSnapshot(old.Snapshot)
	local := r.local(map[string]string{"mine.json": "m\n"}, "mine")

	f.Rewrite()
	f.Publish(branchMain, map[string]string{fileX: "rewritten\n"})
	newReal := r.fetchReal()
	next, err := p.Project(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// what followLocal makes: a merge of the commit that waits and the new snapshot
	tree := r.run("rev-parse", local+"^{tree}")
	merge := r.f.InCheckout(r.l, "commit-tree", tree, "-p", local, "-p", next.Snapshot, "-m", "merge")

	got, err := p.Translate(context.Background(), merge)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 2 {
		t.Fatalf("commits = %d, want the commit that waits and the merge", got.Commits)
	}
	parents := r.parents(got.Tip)
	if len(parents) != 2 || parents[1] != newReal {
		t.Fatalf("the merge has parents %v, want the made-again commit and %s", parents, newReal)
	}
	if grand := r.parents(parents[0]); len(grand) != 1 || grand[0] != oldReal {
		t.Errorf("the commit that waited has parents %v, want %s, which the old snapshot stood for", grand, oldReal)
	}
}

// What a template says does not make a commit a snapshot: a message with the two trailers whose source has another directory is
// a commit of the application like any other.
func TestAMessageThatSaysItIsASnapshotIsBelievedOnlyWhenTheDirectoryIsTheSame(t *testing.T) {
	r, p, first := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, subtree.SnapshotMessage(first.Real, dashboards))
	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 1 {
		t.Errorf("commits = %d, want the commit made again: its tree is not the directory at %s", got.Commits, subtree.Short(first.Real))
	}
}

// The snapshots are a chain of snapshots, and a commit that waits stands on one of them. One that stands on a commit that is neither
// is not a state that the agent makes, and nothing of the repository can be made for it to stand on.
func TestACommitWithAParentThatStandsForNothingIsAnError(t *testing.T) {
	r, p, first := projected(t)
	tree := r.run("rev-parse", first.Snapshot+"^{tree}")
	stray := r.f.InCheckout(r.l, "commit-tree", tree, "-m", "stray")
	tip := r.f.InCheckout(r.l, "commit-tree", tree, "-p", stray, "-m", subtree.SnapshotMessage(first.Real, dashboards))
	r.run("update-ref", subtree.TrackingRef(branchMain), tip)
	waiting := r.f.InCheckout(r.l, "commit-tree", tree, "-p", stray, "-m", "waiting")

	_, err := p.Translate(context.Background(), waiting)
	if err == nil || !strings.Contains(err.Error(), "neither a snapshot nor a commit that is waiting") {
		t.Errorf("err = %v, want it to say that the parent stands for nothing", err)
	}
}

// A commit that waits and says it is a snapshot of another directory is not a state that the agent makes: it is refused, and not
// sent as a commit of this one.
func TestACommitThatIsASnapshotOfAnotherDirectoryIsRefused(t *testing.T) {
	r, p, first := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, subtree.SnapshotMessage(first.Real, "grafana/alerts"))
	_, err := p.Translate(context.Background(), head)
	refusal, ok := errors.AsType[*subtree.Error](err)
	if !ok || refusal.Reason != subtree.ReasonPathMismatch {
		t.Errorf("err = %v, want %s", err, subtree.ReasonPathMismatch)
	}
}
