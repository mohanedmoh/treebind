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
	"fmt"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

// sent pushes what Translate made to the repository, as the push does, and returns it.
func (r *repo) sent(tip string) string {
	r.t.Helper()
	r.f.Plain(r.l.GitDir, "push", "-q", r.f.Bare, tip+":refs/heads/"+branchMain)
	return tip
}

// After the commits were sent the checkout is at the tip: the snapshot of the commit that was sent is on the old snapshot, with
// the tree that the checkout has, and the branch of the checkout is moved to it, so that nothing is waiting and nothing is behind.
// Neither the files nor the index are touched.
func TestAfterAPushTheCheckoutIsAtASnapshotOfTheCommitThatWasSent(t *testing.T) {
	r, p, first := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, "one")
	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	pushed := r.sent(got.Tip)
	before := r.f.InCheckout(r.l, "status", "--porcelain=v2", "--untracked-files=all")

	if err := p.Settle(context.Background(), head, pushed, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := r.commitOf(subtree.TrackingRef(branchMain))
	if snapshot == head || snapshot == first.Snapshot {
		t.Fatalf("%s holds %s, want a new snapshot", subtree.TrackingRef(branchMain), snapshot)
	}
	if parents := r.parents(snapshot); len(parents) != 1 || parents[0] != first.Snapshot {
		t.Errorf("the snapshot has parents %v, want the old snapshot %s and not the commit that was sent", parents, first.Snapshot)
	}
	if r.run("rev-parse", snapshot+"^{tree}") != r.run("rev-parse", head+"^{tree}") {
		t.Error("the snapshot does not have the tree of the checkout")
	}
	if !strings.Contains(r.message(snapshot), subtree.TrailerSource+": "+pushed) {
		t.Errorf("the snapshot does not say that it is of %s:\n%s", pushed, r.message(snapshot))
	}
	if r.commitOf(subtree.RealRef(branchMain)) != pushed {
		t.Errorf("%s holds %s, want %s", subtree.RealRef(branchMain), r.commitOf(subtree.RealRef(branchMain)), pushed)
	}
	if r.commitOf("refs/heads/"+branchMain) != snapshot {
		t.Error("the branch was not moved to the snapshot")
	}
	if after := r.f.InCheckout(r.l, "status", "--porcelain=v2", "--untracked-files=all"); after != before {
		t.Errorf("the status of the files changed:\n%s\n%s", before, after)
	}

	// nothing is behind and nothing waits: a pull that fetches the tip that was sent makes no snapshot
	r.fetchReal()
	if made, err := p.Project(context.Background()); err != nil || made.Made || made.Snapshot != snapshot {
		t.Errorf("Project = %+v, %v; want the snapshot as it is", made, err)
	}
	if again, err := p.Translate(context.Background(), snapshot); err != nil || again.Commits != 0 || again.Tip != pushed {
		t.Errorf("Translate = %+v, %v; want nothing to send, at %s", again, err, pushed)
	}
}

// The first push into a repository that has no commit: the snapshot has no parent.
func TestTheFirstPushIntoAnEmptyRepositoryMakesARootSnapshot(t *testing.T) {
	r := newRepo(t, gittest.New(t))
	p := r.translator()
	r.run("symbolic-ref", "HEAD", "refs/heads/"+branchMain)
	head := r.local(map[string]string{"first.json": "1\n"}, "first")
	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	pushed := r.sent(got.Tip)
	if err := p.Settle(context.Background(), head, pushed, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := r.commitOf(subtree.TrackingRef(branchMain))
	if snapshot == "" || len(r.parents(snapshot)) != 0 || r.commitOf("refs/heads/"+branchMain) != snapshot {
		t.Errorf("snapshot %q with parents %v, branch at %s", snapshot, r.parents(snapshot), r.commitOf("refs/heads/"+branchMain))
	}
}

// A lock that cannot be checked is a push that is not settled and writes nothing: the next pull sees the commit that was sent
// and puts the checkout at it.
func TestAFenceThatRefusesMeansNothingIsWritten(t *testing.T) {
	r, p, first := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, "one")
	got, err := p.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	pushed := r.sent(got.Tip)
	lost := errors.New("the lock is lost")
	err = p.Settle(context.Background(), head, pushed, func(context.Context) error { return lost })
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the error of the fence", err)
	}
	if r.commitOf(subtree.TrackingRef(branchMain)) != first.Snapshot || r.commitOf(subtree.RealRef(branchMain)) != first.Real || r.commitOf("refs/heads/"+branchMain) != head {
		t.Error("a refused fence left a ref moved")
	}
}

// A commit that does not have the files of the checkout is not what was sent, and the checkout is not put at it.
func TestACommitThatDoesNotHaveTheFilesOfTheCheckoutIsNotSettled(t *testing.T) {
	r, p, first := projected(t)
	head := r.local(map[string]string{fileOne: "1\n"}, "one")
	if err := p.Settle(context.Background(), head, first.Real, nil); err == nil {
		t.Fatal("want an error: the commit of the repository does not hold one.json")
	}
	if r.commitOf(subtree.TrackingRef(branchMain)) != first.Snapshot || r.commitOf("refs/heads/"+branchMain) != head {
		t.Error("a refused settle left a ref moved")
	}
}

// The agent is killed after each of the writes of the settling. What is left is something that the next pass puts right: the
// repository's tip is never moved ahead of the snapshot that is of it, because the pull asks the repository for its tip and
// fetches only when it is not the one that the checkout holds, and a checkout that holds the tip with no snapshot of it would
// never be put right. The pass that follows, a fetch and a projection, ends with the snapshot that has the tree of the checkout.
func TestAKillAfterAnyWriteOfTheSettlingLeavesWhatTheNextPassPutsRight(t *testing.T) {
	for kill := 1; kill <= 3; kill++ {
		t.Run(fmt.Sprintf("after write %d", kill), func(t *testing.T) {
			r, p, _ := projected(t)
			head := r.local(map[string]string{fileOne: "1\n"}, "one")
			got, err := p.Translate(context.Background(), head)
			if err != nil {
				t.Fatal(err)
			}
			pushed := r.sent(got.Tip)

			options := r.f.GitOptions()
			options.Git = gittest.KilledAfter(t, "update-ref", kill)
			killed, err := subtree.New(subtree.Options{Layout: r.l, Git: options, Path: dashboards, Branch: branchMain, Author: botAuthor})
			if err != nil {
				t.Fatal(err)
			}
			settled := killed.Settle(context.Background(), head, pushed, nil)
			if (settled == nil) != (kill == 3) {
				t.Fatalf("Settle = %v after write %d of 3", settled, kill)
			}

			// what is left, with the Git of the agent again
			tree := func(ref string) string {
				out, err := r.runner.Run(context.Background(), "rev-parse", "--verify", "--quiet", ref+"^{tree}")
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(out.Stdout))
			}
			if r.commitOf(subtree.RealRef(branchMain)) == pushed && tree(subtree.TrackingRef(branchMain)) != r.run("rev-parse", head+"^{tree}") {
				t.Error("the repository's tip is at the commit that was sent and the snapshot at the tip is not of it: the pull would never fetch")
			}
			r.fetchReal()
			projection, err := p.Project(context.Background())
			if err != nil {
				t.Fatalf("the next pass cannot project: %v", err)
			}
			if got, want := r.run("rev-parse", projection.Snapshot+"^{tree}"), r.run("rev-parse", head+"^{tree}"); got != want {
				t.Errorf("the snapshot of the tip has the tree %s, not the one of the checkout, %s", got, want)
			}
			if projection.Real != pushed {
				t.Errorf("the real tip is %s, want the commit that was sent, %s", projection.Real, pushed)
			}
		})
	}
}
