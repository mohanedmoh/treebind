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

package paths

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run real Git, to hold what the filter and docs/adr/0022 say that Git does.

// Files that the tests write: one that git add can always stage, and one that the class
// of nameClass1 matches.
const (
	fileKeep  = "keep.txt"
	nameDigit = "a1.txt"
)

// commit commits what is staged, with an author given on the command line.
func commit(t *testing.T, o *gitOracle) {
	t.Helper()
	o.run(t, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "--quiet", "--message=one")
}

// writeFile writes a file under root, and the directories before it, with its name in it.
func writeFile(t *testing.T, root, name string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusPaths returns the paths that status lists: the changed files that Git tracks and
// the untracked ones, as the agent is to ask for them.
func statusPaths(t *testing.T, o *gitOracle) []string {
	t.Helper()
	var listed []string
	for _, entry := range files(o.run(t, "status", "--porcelain=v2", "-z", "--untracked-files=all")) {
		switch kind, rest, _ := strings.Cut(entry, " "); kind {
		case "?":
			listed = append(listed, rest)
		case "1":
			listed = append(listed, strings.SplitN(rest, " ", 8)[7])
		default:
			t.Fatalf("status printed %q, which this test does not read", entry)
		}
	}
	slices.Sort(listed)
	return listed
}

// One file whose name Git refuses makes git add -A stage nothing at all, so the filter
// has to leave out every such name. A name that only looks like one is staged.
func TestGitAddStagesNothingWithAGitPathInTheWorkTree(t *testing.T) {
	f := newTestFilter(t, nil, nil, Options{})
	for _, tt := range []struct {
		name    string
		refused bool
	}{
		{".GIT", true}, {"a/.Git/config", true}, {".git.", true}, {".git ", true}, {"GIT~1", true},
		{"a/git~1/x", true}, {".git::$INDEX_ALLOCATION", true}, {`.git\x`, true},
		{".gitx", false}, {".git.x", false}, {"git~2", false}, {"a.git", false}, {".gitmodules", false},
	} {
		o := newRepository(t)
		writeFile(t, o.workTree, fileKeep)
		writeFile(t, o.workTree, tt.name)
		_, err := o.try("add", "-A")
		staged := files(o.run(t, "diff", "--cached", "--name-only", "-z"))
		want := []string{fileKeep, tt.name}
		if tt.refused {
			want = nil
		}
		slices.Sort(want)
		if (err != nil) != tt.refused || !slices.Equal(staged, want) {
			t.Errorf("git add -A with %q: error %v, staged %q; want %q", tt.name, err, staged, want)
		}
		if got := f.Reason(tt.name) == GitPath; got != tt.refused {
			t.Errorf("Reason(%q) = %s, and Git refuses it = %v", tt.name, f.Reason(tt.name), tt.refused)
		}
	}
}

// A symbolic link named .gitmodules is refused as well, which the filter cannot see in
// a path. The stager has to leave such a link out (docs/adr/0022).
func TestGitAddStagesNothingWithALinkNamedGitmodules(t *testing.T) {
	o := newRepository(t)
	writeFile(t, o.workTree, fileKeep)
	if err := os.Symlink(fileKeep, filepath.Join(o.workTree, ".gitmodules")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.try("add", "-A"); err == nil {
		t.Errorf("git add -A takes a link named .gitmodules and staged %q", o.run(t, "diff", "--cached", "--name-only"))
	}
}

// Status lists a repository of its own with a slash at the end, and any other directory
// as its files.
func TestStatusListsANestedRepositoryWithASlash(t *testing.T) {
	o := newRepository(t)
	inner := &gitOracle{git: o.git, workTree: filepath.Join(o.workTree, "emb", "sub")}
	inner.gitDir = filepath.Join(inner.workTree, ".git")
	if err := os.MkdirAll(inner.workTree, 0o755); err != nil {
		t.Fatal(err)
	}
	inner.run(t, "init", "--quiet", "--template=")
	writeFile(t, inner.workTree, fileKeep)
	inner.run(t, "add", fileKeep)
	commit(t, inner)
	writeFile(t, o.workTree, "plain/d/f")

	listed := statusPaths(t, o)
	if want := []string{"emb/sub/", "plain/d/f"}; !slices.Equal(listed, want) {
		t.Fatalf("status lists %q, want %q", listed, want)
	}
	f := newTestFilter(t, nil, nil, Options{})
	if f.Reason(listed[0]) != NestedRepository || f.Reason(listed[1]) != Synced {
		t.Errorf("Reason(%q) = %s and Reason(%q) = %s", listed[0], f.Reason(listed[0]), listed[1], f.Reason(listed[1]))
	}
	o.run(t, "add", "-A")
	if staged := o.run(t, "ls-files", "--stage", "--", "emb/sub"); !strings.HasPrefix(staged, "160000 ") {
		t.Errorf("git add -A staged the nested repository as %q, want a gitlink", staged)
	}
}

// git add reads a path as a pattern, so the stager gives Git literal paths.
func TestGitAddReadsAPathAsAPattern(t *testing.T) {
	for _, tt := range []struct {
		arg  string
		want []string
	}{
		{nameClass1, []string{nameDigit, nameClass1}},
		{":(literal)" + nameClass1, []string{nameClass1}},
	} {
		o := newRepository(t)
		writeFile(t, o.workTree, nameClass1)
		writeFile(t, o.workTree, nameDigit)
		o.run(t, "add", "--", tt.arg)
		if staged := files(o.run(t, "diff", "--cached", "--name-only", "-z")); !slices.Equal(staged, tt.want) {
			t.Errorf("git add -- %q staged %q, want %q", tt.arg, staged, tt.want)
		}
	}
}

// When the whole claim is synced the Git directory is in the work tree, at
// .treebind/git, and status lists its files.
func TestStatusListsTheStateDirectoryInTheWorkTree(t *testing.T) {
	o := newRepository(t)
	in := &gitOracle{git: o.git, workTree: o.workTree, gitDir: filepath.Join(o.workTree, stateDir, "git")}
	if err := os.Mkdir(filepath.Dir(in.gitDir), 0o700); err != nil {
		t.Fatal(err)
	}
	in.run(t, "init", "--quiet", "--template=")
	listed := statusPaths(t, in)
	f := newTestFilter(t, nil, nil, Options{})
	if !slices.Contains(listed, stateDir+"/git/HEAD") {
		t.Fatalf("status lists %q, without the HEAD of the Git directory", listed)
	}
	for _, path := range listed {
		if f.Reason(path) != StateDirectory {
			t.Errorf("Reason(%q) = %s, want %s", path, f.Reason(path), StateDirectory)
		}
	}
}

// .gitignore hides an untracked file and not a tracked one that changed, so the filter
// has to leave out a change to a tracked file itself.
func TestGitignoreHidesOnlyUntrackedFiles(t *testing.T) {
	o := newRepository(t)
	if err := os.WriteFile(filepath.Join(o.workTree, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, o.workTree, "tracked.log")
	o.run(t, "add", "--force", ".gitignore", "tracked.log")
	commit(t, o)
	if err := os.WriteFile(filepath.Join(o.workTree, "tracked.log"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, o.workTree, "untracked.log")

	if listed := statusPaths(t, o); !slices.Equal(listed, []string{"tracked.log"}) {
		t.Errorf("status lists %q, want only the tracked file that changed", listed)
	}
}
