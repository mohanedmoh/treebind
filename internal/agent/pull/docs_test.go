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

package pull

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// The commands of docs/deletion-guard.md are what a person types, so they are run here as they are
// written, on a real repository, and the tests are the ones that say whether the page is true.

// guideBlock returns the shell block that follows the marker of the name in docs/deletion-guard.md.
func guideBlock(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "deletion-guard.md"))
	if err != nil {
		t.Fatal(err)
	}
	marker := "<!-- tested: " + name + " -->\n```sh\n"
	_, rest, found := strings.Cut(string(data), marker)
	if !found {
		t.Fatalf("docs/deletion-guard.md has no block marked %q", name)
	}
	block, _, found := strings.Cut(rest, "\n```")
	if !found {
		t.Fatalf("the block %q is not closed", name)
	}
	return block + "\n"
}

// runGuide runs the block of the page in a shell, for the checkout of the world, and returns what it wrote.
func (w *world) runGuide(t *testing.T, name string, extra ...string) string {
	t.Helper()
	for _, program := range []string{"sh", "xargs"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("%s is not here, and the commands of the page need it", program)
		}
	}
	sub := w.l.SubPath
	if sub == "" {
		sub = "."
	}
	cmd := exec.Command("sh", "-e", "-c", guideBlock(t, name))
	cmd.Dir = gittest.TempDir(t)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + cmd.Dir, "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"MOUNT=" + w.l.Root, "SUB=" + sub, "BRANCH=" + branchMain,
	}, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the block %q failed: %v\n%s", name, err, out)
	}
	return string(out)
}

func TestTheCommandsThatGetFilesBackDoSoAndChangeNothingElse(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorldOf(t, sub, five)
		w.f.Publish(branchMain, gone(fileA, fileB))
		if _, err := w.pull(t); err != nil {
			t.Fatal(err)
		}
		// Later updates that change a file the first did not, and add one.
		w.f.Publish(branchMain, map[string]string{fileE: "e, changed\n", "f.json": "f\n"})
		if _, err := w.pull(t); err != nil {
			t.Fatal(err)
		}

		out := w.runGuide(t, "restore-the-files-of-a-recovery-ref", "REF="+firstRef)

		for _, want := range []string{firstRef, secondRef, fileA + "\n" + fileB + "\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to hold %q: the refs and the files that go back", out, want)
			}
		}
		if gittest.Read(t, w.l, fileA) != contentA || gittest.Read(t, w.l, fileB) != contentB {
			t.Error("the files are not back as they were")
		}
		if gittest.Read(t, w.l, fileE) != "e, changed\n" || gittest.Read(t, w.l, "f.json") != "f\n" {
			t.Error("a file that the branch changed or added since was written over")
		}
		if dirty := w.dirty(t); dirty != "?? "+fileA+"; ?? "+fileB {
			t.Errorf("status = %q, want only the two files back, as files that the branch does not have", dirty)
		}
		if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
			t.Errorf("the Git directory is not what the agent makes after the commands: %v", err)
		}
		if gittest.Exists(filepath.Join(w.l.GitDir, "index.lock")) {
			t.Error("the commands left the lock of the index")
		}
		// What the page says about the files that are back.
		later := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
		_, err := w.pull(t)
		if wantReason(t, err, ReasonBlocked); w.head() == later {
			t.Error("an update that changes a file that is back was applied over it")
		}
	})
}

func TestARestoreOfADirectoryWouldRemoveTheFilesThatTheBranchAddedSince(t *testing.T) {
	// The page says not to give a directory to git restore, and this is why.
	w := newWorldOf(t, subPath, five)
	w.f.Publish(branchMain, gone(fileA))
	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}
	w.f.Publish(branchMain, map[string]string{"f.json": "f\n"})
	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}

	w.f.InCheckout(w.l, "restore", "--source="+firstRef, "--worktree", "--", ".")

	if !gittest.Exists(filepath.Join(w.l.WorkTree, fileA)) || gittest.Exists(filepath.Join(w.l.WorkTree, "f.json")) {
		t.Error("git restore of a directory from the ref did not bring the file back and remove the one the branch added")
	}
}

func TestTheCommandsThatLetAnUpdateThroughDoSo(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorldOf(t, sub, five, capped(2))
		tip := w.f.Publish(branchMain, gone(fileA, fileB, fileC, fileD))
		_, err := w.pull(t)
		wantReason(t, err, ReasonTooManyDeletions)

		listed := w.runGuide(t, "list-the-files-of-the-update")

		if want := strings.Join([]string{fileA, fileB, fileC, fileD}, "\n") + "\n"; listed != want {
			t.Errorf("the list = %q, want %q", listed, want)
		}
		w.runGuide(t, "remove-the-files-of-the-update")

		got, err := w.pull(t)

		if err != nil || got.Outcome != Advanced || got.To != tip {
			t.Fatalf("result = %+v, %v, want Advanced to %s: the update has nothing left to remove", got, err, tip)
		}
		if !gittest.Exists(filepath.Join(w.l.WorkTree, fileE)) {
			t.Error("a file that the update does not remove is gone")
		}
	})
}
