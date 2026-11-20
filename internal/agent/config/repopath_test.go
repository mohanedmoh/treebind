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

package config

import (
	"strings"
	"testing"
)

// Paths that the rules of a directory refuse or take for a reason of their own.
const (
	twoByteLetters  = "é/ü"
	withBackslash   = `a\b`
	withDoubleSlash = "a//b"

	spacedPath          = " a b "
	parentSegmentInside = "a/../b"
	stateNamedPath      = ".treebind/cfg"
)

// What CheckRepoPath says. The rules are the ones that the CRD puts on repository.path, which are the ones of target.subPath
// except for the state directory: a directory of the repository is the root of the synced directory and cannot reach the
// state directory, so stateNamedPath is a path like any other.
func TestCheckRepoPath(t *testing.T) {
	long := strings.Repeat("a", MaxRepoPathLength)
	tests := []struct {
		name string
		dir  string
		want string // "" means accepted, else what the problem says
	}{
		{"none", "", ""},
		{"a directory", "grafana", ""},
		{"nested directories", "grafana/dashboards", ""},
		{"a dot inside a name", "release-1.2", ""},
		{"a dot-prefixed name that is not .git", ".github/dashboards", ""},
		{"the state directory's name, which is a directory of the repository like another", stateNamedPath, ""},
		{"a name with a space", "my dashboards", ""},
		{"a letter of two bytes in a directory name", twoByteLetters, ""},
		{"the longest path", long, ""},
		{"the longest path of letters of two bytes", strings.Repeat("é", MaxRepoPathLength), ""},
		{"a path one character too long", long + "a", "must be at most 1024 characters"},
		{"an absolute path", "/data", "must be relative"},
		{"a dot", ".", "must not be '.'"},
		{"a backslash in the path", withBackslash, "must not contain a backslash"},
		{"a double slash inside the path", withDoubleSlash, "must not contain '//'"},
		{"a parent segment", "../etc", badSegment},
		{"a parent segment inside", parentSegmentInside, badSegment},
		{"a dot segment", "a/./b", badSegment},
		{"a .git segment", "a/.git/b", badSegment},
		{"a .git directory", ".git", badSegment},
		{"a slash at the end", "a/", "must not end with '/'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRepoPath(tt.dir)

			switch {
			case tt.want == "" && err != nil:
				t.Errorf("CheckRepoPath(%q) = %v, want it accepted", tt.dir, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("CheckRepoPath(%q) = %v, want a problem that says %q", tt.dir, err, tt.want)
			}
		})
	}
}
