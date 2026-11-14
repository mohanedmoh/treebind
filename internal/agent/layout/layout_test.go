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

package layout

import (
	"strings"
	"testing"
)

// within reports whether p is below dir. Both are clean absolute paths.
func within(p, dir string) bool {
	if dir == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

const (
	teamDashboards   = "team-a/dashboards"
	stateName        = ".treebind"
	dashboardsDir    = "dashboards"
	isALink          = "is a link"
	isNotADir        = "is not a directory"
	teamDir          = "team-a"
	claimRoot        = "/work"
	claimState       = "/work/.treebind"
	claimGit         = "/work/.treebind/git"
	oneSubPath       = "grafana"
	lookalike        = ".treebindx"
	nestedState      = "a/.treebind"
	mountPathProblem = "mount path"
)

func TestNewPlacesTheGitDirectoryBesideTheSyncedDirectory(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		subPath string
		want    Layout
	}{
		{
			name: "a sub path",
			root: claimRoot, subPath: oneSubPath,
			want: Layout{
				Root: claimRoot, SubPath: oneSubPath, WorkTree: "/work/grafana",
				StateDir: claimState, GitDir: claimGit,
			},
		},
		{
			name: "a nested sub path",
			root: "/data/claim", subPath: teamDashboards,
			want: Layout{
				Root: "/data/claim", SubPath: teamDashboards, WorkTree: "/data/claim/team-a/dashboards",
				StateDir: "/data/claim/.treebind", GitDir: "/data/claim/.treebind/git",
			},
		},
		{
			name: "no sub path: the whole claim is synced and the Git directory is inside it",
			root: claimRoot, subPath: "",
			want: Layout{
				Root: claimRoot, WorkTree: claimRoot, StateDir: claimState, GitDir: claimGit,
				GitDirInWorkTree: true,
			},
		},
		{
			name: "a claim mounted at the root of the file system",
			root: "/", subPath: oneSubPath,
			want: Layout{
				Root: "/", SubPath: oneSubPath, WorkTree: "/grafana", StateDir: "/.treebind", GitDir: "/.treebind/git",
			},
		},
		{
			name: "a sub path that merely looks like the state directory",
			root: claimRoot, subPath: lookalike,
			want: Layout{
				Root: claimRoot, SubPath: lookalike, WorkTree: "/work/.treebindx",
				StateDir: claimState, GitDir: claimGit,
			},
		},
		{
			name: "the state directory's name below the first segment",
			root: claimRoot, subPath: nestedState,
			want: Layout{
				Root: claimRoot, SubPath: nestedState, WorkTree: "/work/a/.treebind",
				StateDir: claimState, GitDir: claimGit,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.root, tt.subPath)

			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("layout = %+v\nwant     %+v", got, tt.want)
			}
		})
	}
}

// With a sub path the Git directory is beside the work tree, never in it, for every
// sub path that is allowed: that is the whole point of the layout. Without one it is
// in it, and is then the one thing that the sync must not stage.
func TestTheGitDirectoryIsOutsideTheWorkTreeExactlyWhenThereIsASubPath(t *testing.T) {
	for _, subPath := range []string{
		"a", "a/b", "a/b/c/d", lookalike, ".treebind-old", nestedState, ".config", "é", "a b", " a",
		"release-1.2", ".github", "git", ".gitignore", ".gitx",
	} {
		layout, err := New(claimRoot, subPath)
		if err != nil {
			t.Fatalf("%q: %v", subPath, err)
		}
		if layout.GitDirInWorkTree || within(layout.GitDir, layout.WorkTree) || within(layout.StateDir, layout.WorkTree) {
			t.Errorf("%q: the Git directory %s is inside the work tree %s", subPath, layout.GitDir, layout.WorkTree)
		}
		if within(layout.WorkTree, layout.StateDir) || layout.WorkTree == layout.StateDir {
			t.Errorf("%q: the work tree %s is inside the state directory %s", subPath, layout.WorkTree, layout.StateDir)
		}
		if !within(layout.GitDir, layout.Root) || !within(layout.WorkTree, layout.Root) {
			t.Errorf("%q: something is outside the claim: %+v", subPath, layout)
		}
		if layout.NeverSynced() != "" {
			t.Errorf("%q: %q is never synced, but the state directory is not in the work tree", subPath, layout.NeverSynced())
		}
	}

	whole, err := New(claimRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if !whole.GitDirInWorkTree || !within(whole.GitDir, whole.WorkTree) {
		t.Errorf("without a sub path the Git directory is in the work tree: %+v", whole)
	}
	if got := whole.NeverSynced(); got != stateName {
		t.Errorf("NeverSynced = %q, want .treebind, relative to the work tree", got)
	}
}

func TestNewRefusesWhatCannotBeALayout(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		subPath string
		want    string
	}{
		{"no root", "", oneSubPath, mountPathProblem},
		{"a relative root", "work", oneSubPath, mountPathProblem},
		{"a root that is not clean", "/work/../etc", "", mountPathProblem},
		{"a root with a slash at the end", "/work/", "", mountPathProblem},
		{"a root with a double slash", "//work", "", mountPathProblem},
		{"an absolute sub path", claimRoot, "/etc", "must be relative"},
		{"a sub path that climbs out", claimRoot, "../etc", "must not contain a '..'"},
		{"the state directory as the sub path", claimRoot, stateName, "must not start with '.treebind'"},
		{"a sub path inside the state directory", claimRoot, ".treebind/git", "must not start with '.treebind'"},
		{"a .git sub path", claimRoot, "a/.git", "must not contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.root, tt.subPath)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New(%q, %q) = %+v, %v, want a problem that says %q", tt.root, tt.subPath, got, err, tt.want)
			}
			if got != (Layout{}) {
				t.Errorf("a layout came back with the error: %+v", got)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	for _, tt := range []struct {
		path, dir string
		want      bool
	}{
		{"/work/a", claimRoot, true},
		{"/work/a/b", claimRoot, true},
		{claimRoot, claimRoot, false},
		{"/workshop", claimRoot, false},
		{claimRoot, "/work/a", false},
		{"/other/a", claimRoot, false},
		{"/a", "/", true},
		{"/", "/", false},
	} {
		if got := within(tt.path, tt.dir); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}
