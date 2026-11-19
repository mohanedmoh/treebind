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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

// layouts are the two places of the synced directory: a sub path of the claim, and the claim.
var layouts = []string{subPath, ""}

// requireAttrSource skips a test that holds only on a Git that reads the attributes from a tree, as the Runner asks it to:
// GIT_ATTR_SOURCE is from Git 2.41 (its source has it from the tag v2.41.0 and not at v2.40.1), which the image of the agent has and an older Git on a developer's machine may not.
func requireAttrSource(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	out := f.Plain(f.Claim, "--version")
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(out), "git version "), "%d.%d", &major, &minor); err != nil {
		t.Fatalf("the version of Git is not understood: %q", out)
	}
	if major < 2 || (major == 2 && minor < 41) {
		t.Skipf("Git %d.%d does not read GIT_ATTR_SOURCE, which came with 2.41: the attributes of the work tree apply to it", major, minor)
	}
}

// A pull writes files, and none of them is a change of the application, whatever is in them and whatever the Git is
// told by the repository about them: the scan lists only what the application changed (docs/adr/0029). Each case is a
// repository that is published, checked out, and then pulled again, so that the files are written by a pull.
func TestFilesThatAPullWritesAreNotChangesOfTheApplicationAndStayNoneWhenTouched(t *testing.T) {
	cases := []struct {
		name  string
		first map[string]string
		// then is published after first, and before the checkout is made. raw is published as it is, with no conversion, and
		// instead of the two.
		then, raw map[string]string
		// executable are published with the mode 0755.
		executable map[string]string
		// needsAttrSource is for a case that holds only on a Git that has GIT_ATTR_SOURCE.
		needsAttrSource bool
	}{
		{name: "plain files", first: map[string]string{fileA: "a\n", "dir/b.txt": "b\n", "dir/sub/c.txt": "c\n"}},
		{name: "eol=crlf", first: map[string]string{fileAttributes: "* text eol=crlf\n", fileA: "one\ntwo\n"}},
		{name: "eol=lf", first: map[string]string{fileAttributes: "* text eol=lf\n", fileA: "one\r\ntwo\r\n"}},
		// A blob with both kinds of line ending, committed before the attribute was set. Written with eol=crlf it is a file that
		// does not come back to the blob, and a Git that read the attribute would list it as changed at every scan that
		// finds its stat different. The Runner reads the attributes from an empty tree (docs/adr/0013), a setting that
		// Git 2.41 brought, so that older releases do list it: see the skip below.
		{name: "eol=crlf and a file with both line endings", raw: map[string]string{fileA: "one\r\ntwo\n", fileAttributes: "* text eol=crlf\n"},
			needsAttrSource: true},
		{name: "text=auto", first: map[string]string{fileAttributes: "* text=auto\n", fileA: "one\r\ntwo\n"}},
		{name: "ident", first: map[string]string{fileAttributes: "*.txt ident\n", fileA: "$Id$\nx\n"}},
		{name: "a filter that is not there", first: map[string]string{fileAttributes: "*.txt filter=lfs\n", fileA: "x\n"}},
		{name: "names that Git quotes", first: map[string]string{"dir with space/f g.txt": "x\n", "u/ç.txt": "y\n", "back\\slash.txt": "z\n", "new\nline.txt": "q\n"}},
		{name: "an executable file", first: map[string]string{fileA: "a\n"}, executable: map[string]string{"run.sh": "#!/bin/sh\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := gittest.New(t)
			if c.needsAttrSource {
				requireAttrSource(t, f)
			}
			if c.raw != nil {
				f.PublishRaw(branchMain, c.raw)
			} else {
				f.Publish(branchMain, c.first)
			}
			if c.then != nil {
				f.Publish(branchMain, c.then)
			}
			if c.executable != nil {
				f.PublishExecutable(branchMain, c.executable)
			}
			n := newNode(t, f, "A", subPath, pull.FollowRemote)
			f.Publish(branchMain, map[string]string{"later.txt": "later\n"})
			f.PublishLink(branchMain, "link", "later.txt")

			if got := n.pull(); got.Outcome != pull.Advanced {
				t.Fatalf("the pull = %+v, want Advanced: the files are to be written by it", got)
			}
			if got := n.changes(); len(got) != 0 {
				t.Errorf("the scan lists %v after the pull, want nothing", got)
			}

			// The application, or a tool that it runs, sets the times of every file to another: the content is the same.
			later := time.Now().Add(time.Hour)
			err := filepath.WalkDir(n.l.WorkTree, func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.Type().IsRegular() {
					err = os.Chtimes(path, later, later)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := n.changes(); len(got) != 0 {
				t.Errorf("the scan lists %v after every file was touched, want nothing: the content is the same", got)
			}
		})
	}
}

// The application's own write is a change, even when it is made at once after the pull's, with the same size: what the
// agent would have to go by, to leave the files of a pull out, is what the pull left, and that is not enough to tell them
// apart. Git compares the content, which is why the agent has no guard of its own against its own writes.
func TestAnApplicationsRewriteRightAfterAPullIsStillAChange(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{fileA: "aaaa\n"})
	n := newNode(t, f, "A", subPath, pull.FollowRemote)

	for i := range 30 {
		f.Publish(branchMain, map[string]string{fileA: fmt.Sprintf("v%03d\n", i)})
		if got := n.pull(); got.Outcome != pull.Advanced {
			t.Fatalf("pull %d = %+v, want Advanced", i, got)
		}
		n.write(fileA, fmt.Sprintf("w%03d\n", i))

		got := n.changes()

		if len(got) != 1 || got[0].Path != fileA || got[0].Kind != scan.Modified {
			t.Fatalf("after pull %d the scan lists %v, want a.txt as modified: it was rewritten with the same size, at once", i, got)
		}
		n.f.InCheckout(n.l, "checkout", "--", fileA)
	}
}

// What the agent does itself, a pull that writes files, a scan, a commit and a push, does not make the watcher wake the
// loop again and again: a pull that wrote files is one signal, and the rest make none. Every step that makes a signal makes
// a scan that finds nothing, and the loop ends there.
func TestTheStepsOfTheAgentMakeNoSignalForTheWatcherButTheFilesOfAPull(t *testing.T) {
	for _, sub := range layouts {
		t.Run("sub path "+sub, func(t *testing.T) {
			f := gittest.New(t)
			f.Publish(branchMain, map[string]string{fileA: "a\n", "d/b.txt": "b\n"})
			n := newNode(t, f, "A", sub, pull.FollowRemote)
			w, err := watch.New(watch.Options{Root: n.l.WorkTree, Skip: n.filter.SkipsDir})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			signals := func() int {
				// What the kernel has queued is delivered at once, and nothing that comes later is in this count.
				time.Sleep(300 * time.Millisecond)
				count := 0
				for {
					select {
					case <-w.Events():
						count++
					default:
						return count
					}
				}
			}
			signals()

			f.Publish(branchMain, map[string]string{"c.txt": "c\n", "d/e.txt": "e\n"})
			n.pull()
			if got := signals(); got == 0 {
				t.Error("the pull wrote files and the watcher gave no signal: the scan would not be sooner, and this test would hold nothing")
			}
			w.Refresh()
			n.changes()
			n.write(fileA, "a, changed\n")
			if got := signals(); got == 0 {
				t.Error("the application wrote a file and the watcher gave no signal")
			}
			steps := []struct {
				name string
				run  func()
			}{
				{"a scan", func() { n.changes() }},
				{"a commit", func() {
					if !n.commit() {
						t.Error("the commit made no commit")
					}
				}},
				{"a push", func() { n.push() }},
				{"a pull that finds the checkout at the tip", func() { n.pull() }},
			}
			for _, step := range steps {
				step.run()
				if got := signals(); got != 0 {
					t.Errorf("%s gave the watcher %d signal(s), want none", step.name, got)
				}
			}
		})
	}
}

// A commit that the agent pushed is the tip of the repository when it is pulled next, and not something new: the pull has
// nothing to apply and nothing to resolve, and the scan has nothing to commit.
func TestTheCommitThatWasPushedIsUpToDateAtTheNextPullAndMakesNothingMore(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{fileA: "a\n"})
	n := newNode(t, f, "A", subPath, pull.FollowLocal)
	n.write(fileA, "a, changed\n")

	if !n.commit() || n.push() != "Pushed" {
		t.Fatal("the commit was not made and pushed")
	}
	for pass := 1; pass <= 3; pass++ {
		if got := n.pull(); got.Outcome != pull.UpToDate {
			t.Errorf("pass %d: the pull = %+v, want UpToDate", pass, got)
		}
		if n.commit() || n.push() != "NothingToPush" {
			t.Errorf("pass %d: the scan found something to commit, or the push found something to push", pass)
		}
	}
	if n.commits != 1 || n.pushes != 1 || n.resolved != 0 {
		t.Errorf("the node made %d commits, %d pushes and %d resolutions, want 1, 1 and 0", n.commits, n.pushes, n.resolved)
	}
}

// Two Treebinds on one repository, whose applications edit at random, and whose commits, pushes and pulls come in any order,
// end at one tree on both and on the repository, with every conflict resolved, and then do nothing at all: a resolution
// that is pushed and pulled by the other does not make another (docs/adr/0034). The four pairs of the two policies are
// tried, and the schedules are the same on every run.
func TestTwoTreebindsThatEditAtRandomEndAtOneTreeAndThenDoNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("each schedule makes hundreds of Git commands")
	}
	policies := [][2]pull.Policy{
		{pull.FollowLocal, pull.FollowLocal}, {pull.FollowRemote, pull.FollowRemote},
		{pull.FollowLocal, pull.FollowRemote}, {pull.FollowRemote, pull.FollowLocal},
	}
	for _, pair := range policies {
		for seed := uint64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%s and %s, schedule %d", pair[0], pair[1], seed), func(t *testing.T) {
				f := gittest.New(t)
				f.Publish(branchMain, map[string]string{"f.txt": "0\n", "g.txt": "0\n", "h/i.txt": "0\n"})
				a, b := newNode(t, f, "A", subPath, pair[0]), newNode(t, f, "B", subPath, pair[1])
				nodes := []*node{a, b}
				files := []string{"f.txt", "g.txt", "h/i.txt", "j.txt"}
				random := &lcg{state: seed*7919 + 17}
				var trace []string

				for tick := range 40 {
					n := nodes[random.below(2)]
					switch random.below(5) {
					case 0:
						name := files[random.below(len(files))]
						n.write(name, fmt.Sprintf("%s-%d\n", n.name, tick))
						trace = append(trace, n.name+" writes "+name)
					case 1:
						trace = append(trace, fmt.Sprintf("%s pulls: %s", n.name, n.pullOutcome()))
					case 2, 3:
						trace = append(trace, fmt.Sprintf("%s commits: %t", n.name, n.commit()))
					case 4:
						trace = append(trace, fmt.Sprintf("%s pushes: %s", n.name, n.push()))
					}
				}

				// No more edits: every node pulls, commits and pushes in turn, until a whole round changes nothing.
				quiet := false
				for round := range 30 {
					before := a.activity() + b.activity()
					for _, n := range nodes {
						trace = append(trace, fmt.Sprintf("round %d: %s pulls: %s", round, n.name, n.pullOutcome()))
						n.commit()
						n.push()
					}
					if a.activity()+b.activity() == before {
						quiet = true
						break
					}
				}

				remote := strings.TrimSpace(f.Plain(f.Bare, "rev-parse", "refs/heads/main^{tree}"))
				if !quiet || a.tree() != remote || b.tree() != remote {
					t.Errorf("quiet = %t, trees A = %s, B = %s, repository = %s, want a quiet round and the same tree\n%s",
						quiet, a.tree(), b.tree(), remote, strings.Join(trace, "\n"))
				}
			})
		}
	}
}

// pullOutcome is a pull that may fail, which one that is blocked by an edit of the application does until the commit step has
// taken the edit: it says what it did, and the failure is what the next round puts right.
func (n *node) pullOutcome() string {
	n.t.Helper()
	got, err := n.puller.Pull(context.Background())
	if failure, ok := errors.AsType[*pull.Error](err); ok && failure.Reason == breaker.ReasonTooManyCommits {
		n.held++
	}
	if err != nil {
		return "failed: " + err.Error()
	}
	switch got.Outcome {
	case pull.Resolved:
		n.resolved++
	case pull.Advanced:
		n.advanced++
	}
	return string(got.Outcome)
}

// lcg is a source of numbers that is the same on every run.
type lcg struct{ state uint64 }

func (l *lcg) below(n int) int {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return int((l.state >> 33) % uint64(n))
}
