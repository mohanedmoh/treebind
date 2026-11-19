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

package loops

import (
	"context"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

const (
	branchMain = "main"
	subPath    = "data"
	botName    = "treebind-bot"
	botEmail   = "treebind-bot@treebind.invalid"
)

// node is one Treebind: a checkout on a claim of its own, and the real pull, scan, commit and push of the agent for it.
// The loop of the agent is left out, so that a test says when each step is taken.
type node struct {
	t         *testing.T
	f         *gittest.Fixture
	name      string
	l         layout.Layout
	puller    *pull.Puller
	scanner   *scan.Scanner
	committer *commit.Committer
	pusher    *push.Pusher
	filter    *paths.Filter

	// What the node has done, for the tests that ask whether it has stopped.
	commits, pushes, resolved, advanced int
}

// newNode makes the checkout of the branch of the repository of f on a new claim, with the policy for conflicts.
func newNode(t *testing.T, f *gittest.Fixture, name string, sub string, policy pull.Policy) *node {
	t.Helper()
	l, err := layout.New(gittest.TempDir(t), sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatalf("the checkout of %s was not made: %v", name, err)
	}
	n := &node{t: t, f: f, name: name, l: l}
	signed := f.GitOptions()
	signed.Identity = git.Identity{Name: botName, Email: botEmail}
	if n.puller, err = pull.New(pull.Options{Layout: l, URL: f.URL, Branch: branchMain, Git: signed, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if n.filter, err = paths.NewFilter(nil, nil, l.NeverSynced(), paths.Options{}); err != nil {
		t.Fatal(err)
	}
	if n.scanner, err = scan.New(scan.Options{Layout: l, Git: f.GitOptions(), Filter: n.filter}); err != nil {
		t.Fatal(err)
	}
	if n.committer, err = commit.New(commit.Options{Layout: l, Git: f.GitOptions()}); err != nil {
		t.Fatal(err)
	}
	if n.pusher, err = push.New(push.Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	return n
}

// write is what the application does: it puts a file in the synced directory.
func (n *node) write(name, content string) {
	n.t.Helper()
	gittest.Write(n.t, n.l, name, content)
}

// pull is one pull, and says what it did.
func (n *node) pull() pull.Result {
	n.t.Helper()
	got, err := n.puller.Pull(context.Background())
	if err != nil {
		n.t.Fatalf("%s: the pull failed: %v", n.name, err)
	}
	switch got.Outcome {
	case pull.Resolved:
		n.resolved++
	case pull.Advanced:
		n.advanced++
	}
	return got
}

// changes is what a scan finds.
func (n *node) changes() []scan.Change {
	n.t.Helper()
	got, err := n.scanner.Scan(context.Background())
	if err != nil {
		n.t.Fatalf("%s: the scan failed: %v", n.name, err)
	}
	return got.Changes
}

// commit commits what a scan finds, without the quiet period, and says whether it made a commit.
func (n *node) commit() bool {
	n.t.Helper()
	changes := n.changes()
	if len(changes) == 0 {
		return false
	}
	got, err := n.committer.Commit(context.Background(), changes)
	if err != nil {
		n.t.Fatalf("%s: the commit failed: %v", n.name, err)
	}
	if got.Outcome != commit.Committed {
		return false
	}
	n.commits++
	return true
}

// push is one push, and says what it did. A push that the repository does not take as a fast-forward is a result and not a
// failure of the test: it is what the next pull resolves.
func (n *node) push() push.Outcome {
	n.t.Helper()
	got, err := n.pusher.Push(context.Background())
	if err != nil {
		return ""
	}
	n.pushes += got.Commits
	return got.Outcome
}

// activity counts everything that the node did that another node could see or that moved its own checkout, so that a round
// that leaves it as it was is one in which it did nothing.
func (n *node) activity() int { return n.commits + n.pushes + n.resolved + n.advanced }

// tree is the tree of the commit the checkout is at.
func (n *node) tree() string { return n.f.InCheckout(n.l, "rev-parse", "HEAD^{tree}") }
