package subtree_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

// The root tree of the repository with one directory replaced is the tree that Git makes by removing the directory and reading
// the new one in with a prefix, for every depth of the directory and for names that sort in a way of their own.
func TestReplacingADirectoryMakesTheTreeThatGitMakes(t *testing.T) {
	f := gittest.New(t)
	f.PublishExecutable(branchMain, map[string]string{"grafana/dashboards/run.sh": "#!/bin/sh\n"})
	f.PublishLink(branchMain, "grafana/dashboards/link", "x.json")
	f.Publish(branchMain, map[string]string{
		fileX:                           contentD1,
		"grafana/dashboards/a b.json":   "d\n",
		"grafana/dashboards/é.json":     "u\n",
		"grafana/dashboards/new\nline":  "nl\n",
		"grafana/dashboards/a-b/c.json": "c\n",
		"grafana/dashboards/a/c.json":   "c2\n",
		"grafana/dashboards.d/x":        "sibling\n",
		"grafana/dashboards-old/x":      "sibling2\n",
		fileAlerts:                      "a\n",
		fileOther:                       "o1\n",
		"top.txt":                       "t\n",
		fileReadme:                      contentReadme,
	})
	r := newRepo(t, f)
	real := r.fetchReal()
	root := r.run("rev-parse", "--verify", real+"^{tree}")
	// a tree to put in: the same directory with other files, and a directory with nothing in common with it
	other := r.run("rev-parse", "--verify", real+":grafana/alerts")
	nested := r.run("rev-parse", "--verify", real+":grafana")

	tests := []struct {
		name string
		dir  string
		sub  string
	}{
		{"one level", "grafana", other},
		{"two levels", dashboards, other},
		{"two levels, with the tree of the parent", dashboards, nested},
		{"a directory whose name sorts beside the one that is replaced", "grafana/dashboards.d", other},
		{"a directory that is not there yet", "grafana/new/deeper/still", other},
		{"a directory at the top that is not there yet", "brand-new", other},
		{"a directory that is emptied, which Git does not keep", dashboards, subtree.EmptyTree},
		{"a directory that is emptied and takes its parent with it", "grafana/alerts", subtree.EmptyTree},
		{"a directory that is not there and is emptied", "nope/nothing", subtree.EmptyTree},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := subtree.ReplaceDirectory(context.Background(), r.runner, r.l.StateDir, root, strings.Split(tt.dir, "/"), tt.sub)
			if err != nil {
				t.Fatal(err)
			}
			if want := r.truthReplace(root, tt.dir, tt.sub); got != want {
				t.Errorf("tree = %s, want %s (Git's own)", got, want)
			}
		})
	}
}

// A file where the directory should be is not replaced by one: the repository has a name that is a file, and the push would
// remove the file and make a directory of it, which nobody asked for.
func TestAFileOnTheWayToTheDirectoryIsAnErrorAndNotReplaced(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{fileReadme: contentReadme, fileX: "d\n"})
	r := newRepo(t, f)
	root := r.run("rev-parse", "--verify", r.fetchReal()+"^{tree}")
	sub := r.run("rev-parse", "--verify", r.fetchReal()+":grafana")
	for _, dir := range []string{fileReadme, "README.md/deeper", fileX} {
		got, err := subtree.ReplaceDirectory(context.Background(), r.runner, r.l.StateDir, root, strings.Split(dir, "/"), sub)
		if err == nil || !strings.Contains(err.Error(), "is a file") {
			t.Errorf("replace(%q) = %q, %v; want an error that says a file is in the way", dir, got, err)
		}
	}
}

// The file that a tree is written to for Git to read is in the state directory, which an application that runs as the agent's own
// user can write. A link that it planted where the file goes is not followed: what it points to is not written over.
func TestATreeIsNotWrittenThroughALinkThatWasPlantedWhereItsFileGoes(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{fileX: "d1\n", fileAlerts: "a\n"})
	r := newRepo(t, f)
	real := r.fetchReal()
	root := r.run("rev-parse", "--verify", real+"^{tree}")
	other := r.run("rev-parse", "--verify", real+":grafana/alerts")
	victim := filepath.Join(gittest.TempDir(t), "victim")
	if err := os.WriteFile(victim, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(r.l.StateDir, "subtree-tree")); err != nil {
		t.Fatal(err)
	}

	if _, err := subtree.ReplaceDirectory(context.Background(), r.runner, r.l.StateDir, root, []string{"grafana", "dashboards"}, other); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(victim); err != nil || string(got) != "precious\n" {
		t.Errorf("the file that the link points to holds %q, %v", got, err)
	}
}
