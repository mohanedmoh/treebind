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

package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

func TestCheckGitDirPassesOnAClaimItCanMakeTheDirectoriesIn(t *testing.T) {
	dir := tempDir(t)

	result := checkGitDir(dir, "grafana")

	wantResult(t, result, CheckGitDir, Passed, "")
	if !strings.Contains(result.Message, filepath.Join(dir, ".treebind", "git")) {
		t.Errorf("the message does not say where the Git directory is: %s", result.Message)
	}
	if strings.Contains(result.Message, "application can see") {
		t.Errorf("a Git directory beside the synced directory is described as visible: %s", result.Message)
	}
	if info, err := os.Lstat(filepath.Join(dir, ".treebind", "git")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the Git directory was not made private: %v, %v", info, err)
	}
}

func TestCheckGitDirSaysWhenTheApplicationCanSeeTheGitDirectory(t *testing.T) {
	result := checkGitDir(tempDir(t), "")

	wantResult(t, result, CheckGitDir, Passed, "")
	for _, want := range []string{"application can see", "subPath", docsPage} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("the message of a Git directory inside the synced directory does not say %q: %s", want, result.Message)
		}
	}
}

func TestCheckGitDirRefusesADirectoryThatIsNotTheAgents(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"a link", func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".treebind")); err != nil {
				t.Fatal(err)
			}
		}, "is a link"},
		{"a directory open to others", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, ".treebind"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, ".treebind"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "open to group or others"},
		{"a file", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".treebind"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tempDir(t)
			tt.setup(t, dir)

			result := checkGitDir(dir, "grafana")

			wantResult(t, result, CheckGitDir, Failed, ReasonGitDirUnsafe)
			for _, want := range []string{filepath.Join(dir, ".treebind"), tt.want, notRunHere, docsPage, "uid", "with mode 0700"} {
				if !strings.Contains(result.Message, want) {
					t.Errorf("the message does not say %q: %s", want, result.Message)
				}
			}
		})
	}
}

func TestCheckGitDirRefusesALinkOnTheWayToTheSyncedDirectory(t *testing.T) {
	dir := tempDir(t)
	if err := os.Symlink(".treebind", filepath.Join(dir, "grafana")); err != nil {
		t.Fatal(err)
	}

	result := checkGitDir(dir, "grafana")

	wantResult(t, result, CheckGitDir, Failed, ReasonGitDirUnsafe)
	if !strings.Contains(result.Message, filepath.Join(dir, "grafana")+" is a link") {
		t.Errorf("the message does not name the link: %s", result.Message)
	}
	if !strings.Contains(result.Message, "Replace it with a real directory") || strings.Contains(result.Message, "mode 0700") {
		t.Errorf("the message gives the advice for the agent's own directories, which the application's directory is not: %s", result.Message)
	}
}

func TestCheckGitDirFailsClosedWhenTheLayoutCannotBeWorkedOut(t *testing.T) {
	result := checkGitDir(tempDir(t), ".treebind")

	wantResult(t, result, CheckGitDir, Failed, ReasonGitDirUnsafe)
	if !strings.Contains(result.Message, "cannot be worked out") {
		t.Errorf("the message does not say why: %s", result.Message)
	}
}

func TestCheckGitDirNamesTheDirectoryThatCannotBeWrittenTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its mode")
	}
	t.Run("the claim itself", func(t *testing.T) {
		dir := tempDir(t)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}

		result := checkGitDir(dir, "grafana")

		wantResult(t, result, CheckGitDir, Failed, ReasonNotWritable)
		if !strings.Contains(result.Message, "make the Git directory in "+dir+",") {
			t.Errorf("the message does not name the claim as the directory that cannot be written to: %s", result.Message)
		}
		if strings.Contains(result.Message, filepath.Join(dir, ".treebind")+",") {
			t.Errorf("the message names the state directory, which does not exist, as the one that failed: %s", result.Message)
		}
	})
	t.Run("a claim that is not there", func(t *testing.T) {
		dir := filepath.Join(tempDir(t), "gone")

		result := checkGitDir(dir, "grafana")

		wantResult(t, result, CheckGitDir, Failed, ReasonWriteFailed)
		if !strings.Contains(result.Message, "make the Git directory in "+dir+":") {
			t.Errorf("the message does not name the claim that is not there: %s", result.Message)
		}
	})
	t.Run("the state directory", func(t *testing.T) {
		dir := tempDir(t)
		state := filepath.Join(dir, ".treebind")
		if err := os.Mkdir(state, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

		result := checkGitDir(dir, "grafana")

		wantResult(t, result, CheckGitDir, Failed, ReasonNotWritable)
		if !strings.Contains(result.Message, "make the Git directory in "+state+",") {
			t.Errorf("the message does not name the state directory: %s", result.Message)
		}
	})
}

func TestWriteFailureNamesTheCheckThatFailed(t *testing.T) {
	for _, check := range []string{CheckWritable, CheckGitDir} {
		for _, err := range []error{syscall.EROFS, syscall.EACCES, syscall.ENOSPC} {
			if got := writeFailure(check, t.TempDir(), "make a directory in", err).Check; got != check {
				t.Errorf("a failure of the %s check is reported as the %s check (%v)", check, got, err)
			}
		}
	}
}

// The names are API: a user or a tool matches on them (docs/adr/0009).
func TestTheNamesOfTheGitDirCheckAreFixed(t *testing.T) {
	if CheckGitDir != "GitDir" || ReasonGitDirUnsafe != "GitDirUnsafe" {
		t.Errorf("the check is %q and its reason %q, want GitDir and GitDirUnsafe", CheckGitDir, ReasonGitDirUnsafe)
	}
}

func TestGitDirReasonIsItsOwn(t *testing.T) {
	for _, other := range []string{
		ReasonMountPathInvalid, ReasonNotMounted, ReasonNotWritable, ReasonReadOnlyVolume,
		ReasonWriteFailed, ReasonTimedOut, ReasonNotChecked,
	} {
		if other == ReasonGitDirUnsafe {
			t.Errorf("%q is used for two causes (docs/adr/0009)", other)
		}
	}
}

// Names that the tests of what is in the Git directory use again and again.
const (
	subGrafana = "grafana"
	gitConfig  = "config"
	notRunHere = "Git is not run against it"
	docsPage   = "git-directory.md"
)

// fillGitDir writes what the agent's checkout leaves in the Git directory: HEAD, the config
// that init writes, with the work tree of the layout, and the directories of the objects and
// the refs.
func fillGitDir(t *testing.T, where layout.Layout) {
	t.Helper()
	for _, dir := range []string{"objects", filepath.Join("refs", "heads")} {
		if err := os.MkdirAll(filepath.Join(where.GitDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeIn(t, where, "HEAD", "ref: refs/heads/main\n", 0o600)
	writeIn(t, where, gitConfig, "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"+
		"\tlogallrefupdates = true\n\tworktree = "+where.WorkTree+"\n", 0o600)
}

// writeIn writes a file in the Git directory, as an application that can write there would.
func writeIn(t *testing.T, where layout.Layout, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(where.GitDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// layoutOf returns the layout of a claim, with the directories made as the check makes them.
func layoutOf(t *testing.T, dir, subPath string) layout.Layout {
	t.Helper()
	where, err := layout.New(dir, subPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := where.Prepare(); err != nil {
		t.Fatal(err)
	}
	return where
}

func TestCheckGitDirPassesOnAGitDirectoryThatIsEmptyOrHoldsWhatTheAgentMakes(t *testing.T) {
	for _, subPath := range []string{subGrafana, ""} {
		t.Run("sub path "+subPath, func(t *testing.T) {
			dir := tempDir(t)
			where := layoutOf(t, dir, subPath)

			// Empty, as the first start makes it.
			wantResult(t, checkGitDir(dir, subPath), CheckGitDir, Passed, "")

			fillGitDir(t, where)

			result := checkGitDir(dir, subPath)

			wantResult(t, result, CheckGitDir, Passed, "")
			if result.Message != passedMessage(where) {
				t.Errorf("message = %q, want the one of a Git directory that is private: %q", result.Message, passedMessage(where))
			}
		})
	}
}

func TestCheckGitDirRefusesAGitDirectoryThatHoldsWhatTheAgentDidNotPutThere(t *testing.T) {
	tests := []struct {
		name string
		// plant puts it in the Git directory, and returns what puts it right.
		plant func(t *testing.T, where layout.Layout) (undo func())
		// path is the file that the message names, and problem what it says about it.
		path, problem string
	}{
		{"a hook", func(t *testing.T, where layout.Layout) func() {
			writeIn(t, where, filepath.Join("hooks", "post-checkout"), "#!/bin/sh\n", 0o700)
			return func() { remove(t, filepath.Join(where.GitDir, "hooks", "post-checkout")) }
		}, "hooks", "holds post-checkout"},
		{"a config key", func(t *testing.T, where layout.Layout) func() {
			config, err := os.ReadFile(filepath.Join(where.GitDir, gitConfig))
			if err != nil {
				t.Fatal(err)
			}
			writeIn(t, where, gitConfig, string(config)+"\tsshCommand = /tmp/evil\n", 0o600)
			return func() { writeIn(t, where, gitConfig, string(config), 0o600) }
		}, gitConfig, "line 7 sets core.sshcommand"},
		{"info/attributes", func(t *testing.T, where layout.Layout) func() {
			writeIn(t, where, filepath.Join("info", "attributes"), "* filter=x\n", 0o600)
			return func() { remove(t, filepath.Join(where.GitDir, "info", "attributes")) }
		}, "info", "holds attributes"},
		{"a file at the top that the agent does not make", func(t *testing.T, where layout.Layout) func() {
			writeIn(t, where, "notes.txt", "x\n", 0o600)
			return func() { remove(t, filepath.Join(where.GitDir, "notes.txt")) }
		}, "notes.txt", "is not a file that the agent makes"},
	}
	for _, tt := range tests {
		for _, subPath := range []string{subGrafana, ""} {
			t.Run(tt.name+", sub path "+subPath, func(t *testing.T) {
				dir := tempDir(t)
				where := layoutOf(t, dir, subPath)
				fillGitDir(t, where)
				undo := tt.plant(t, where)

				result := checkGitDir(dir, subPath)

				wantResult(t, result, CheckGitDir, Failed, ReasonGitDirUnsafe)
				for _, want := range []string{
					filepath.Join(where.GitDir, tt.path) + " " + tt.problem, notRunHere, "Remove the file or the setting",
					"empty " + where.GitDir, docsPage,
				} {
					if !strings.Contains(result.Message, want) {
						t.Errorf("the message does not say %q: %s", want, result.Message)
					}
				}

				// The agent checks again, and carries on once it is put right.
				undo()
				wantResult(t, checkGitDir(dir, subPath), CheckGitDir, Passed, "")
			})
		}
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestCheckGitDirSaysWhenItCannotLookInTheGitDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	dir := tempDir(t)
	where := layoutOf(t, dir, subGrafana)
	fillGitDir(t, where)
	hooks := filepath.Join(where.GitDir, "hooks")
	if err := os.Mkdir(hooks, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooks, 0o700) })

	result := checkGitDir(dir, subGrafana)

	// The agent is shut out of a directory of its own. It cannot tell what is in it, so Git is
	// not run against it, and the advice is to make it readable, not writable.
	wantResult(t, result, CheckGitDir, Failed, ReasonGitDirUnsafe)
	for _, want := range []string{"cannot look at what is in " + where.GitDir + ":", "Git is not run", "Make it readable"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("the message does not say %q: %s", want, result.Message)
		}
	}
}
