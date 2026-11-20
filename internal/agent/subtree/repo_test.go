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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

const (
	branchMain = "main"
	dashboards = "grafana/dashboards"
	fileX      = "grafana/dashboards/x.json"
	fileReadme = "README.md"
	fileOther  = "other/y.txt"

	fileAlerts    = "grafana/alerts/a.json"
	fileOne       = "one.json"
	contentReadme = "readme\n"
	contentOther2 = "o2\n"
	contentD1     = "d1\n"
)

// repo is a checkout's Git directory with the objects of a real repository fetched into it, and the Runner of the agent on it,
// as the checkout makes them.
type repo struct {
	t      *testing.T
	f      *gittest.Fixture
	l      layout.Layout
	runner *git.Runner
}

// newRepo makes the Git directory of a layout with the sub path data in a claim of its own, with nothing fetched.
func newRepo(t *testing.T, f *gittest.Fixture) *repo {
	t.Helper()
	l, err := layout.New(gittest.TempDir(t), "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := l.MakeWorkTree(); err != nil {
		t.Fatal(err)
	}
	r := &repo{t: t, f: f, l: l}
	runner, err := git.New(l.RunnerOptions(f.GitOptions()))
	if err != nil {
		t.Fatal(err)
	}
	r.runner = runner
	r.run("init", "--quiet", "--template=", "--initial-branch="+branchMain)
	return r
}

// run runs a subcommand through the Runner and returns its output without the newline at the end. It fails the test.
func (r *repo) run(args ...string) string {
	r.t.Helper()
	out, err := r.runner.Run(context.Background(), args...)
	if err != nil {
		r.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSuffix(string(out.Stdout), "\n")
}

// fetchReal fetches the real branch into the ref that a fetch brings the tip to, as the checkout and the pull do, and returns the
// tip. The projection moves the ref of the repository's tip when it has made the snapshot.
func (r *repo) fetchReal() string {
	r.t.Helper()
	r.run("fetch", "--quiet", "--no-tags", "--", r.f.URL, "+refs/heads/"+branchMain+":"+subtree.FetchRef(branchMain))
	return r.run("rev-parse", "--verify", subtree.FetchRef(branchMain)+"^{commit}")
}

// truthReplace is the tree that Git itself makes when the directory of root is replaced by sub: an index of its own, the real tree
// read into it, the directory removed and the new one read in with a prefix.
func (r *repo) truthReplace(root, dir, sub string) string {
	r.t.Helper()
	index, work := filepath.Join(r.t.TempDir(), "index"), r.t.TempDir()
	step := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_DIR=" + r.l.GitDir, "GIT_WORK_TREE=" + work, "GIT_INDEX_FILE=" + index}
		out, err := cmd.CombinedOutput()
		if err != nil {
			r.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	step("read-tree", root)
	step("rm", "--cached", "-r", "-q", "--ignore-unmatch", "--", dir)
	if sub != subtree.EmptyTree {
		step("read-tree", "--prefix="+dir+"/", sub)
	}
	return step("write-tree")
}

// message is the message of a commit exactly as it is stored, with every newline at the end of it.
func (r *repo) message(commit string) string {
	r.t.Helper()
	out, err := r.runner.Run(context.Background(), "cat-file", "commit", commit)
	if err != nil {
		r.t.Fatal(err)
	}
	_, message, _ := strings.Cut(string(out.Stdout), "\n\n")
	return message
}
