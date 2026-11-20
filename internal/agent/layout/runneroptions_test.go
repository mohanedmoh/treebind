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
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// The Runner of the agent is made from the layout in one place, so that every step that runs Git looks at the Git directory
// before every command, and puts the Git directory where the layout says.
func TestRunnerOptionsPutTheDirectoriesOfTheLayoutInTheSettingsOfTheRunner(t *testing.T) {
	given := git.Options{Git: "/usr/bin/git", Timeout: 7 * time.Second, Identity: git.Identity{Name: "n", Email: "e@x"}}
	tests := []struct {
		name     string
		subPath  string
		inWorkTr bool
	}{
		{"a sub path: the Git directory is beside the work tree", oneSubPath, false},
		{"no sub path: the Git directory is in the work tree", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := New(claimRoot, tt.subPath)
			if err != nil {
				t.Fatal(err)
			}
			got := l.RunnerOptions(given)
			if got.GitDir != l.GitDir || got.WorkTree != l.WorkTree {
				t.Errorf("directories = %q, %q; want %q, %q", got.GitDir, got.WorkTree, l.GitDir, l.WorkTree)
			}
			if got.AllowGitDirInWorkTree != tt.inWorkTr {
				t.Errorf("AllowGitDirInWorkTree = %v, want %v", got.AllowGitDirInWorkTree, tt.inWorkTr)
			}
			if !got.VerifyGitDir {
				t.Error("the Git directory is not looked at before every command")
			}
			if got.Git != given.Git || got.Timeout != given.Timeout || got.Identity != given.Identity {
				t.Errorf("what the caller gave was changed: %+v", got)
			}
		})
	}
}
