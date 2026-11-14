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
package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

var tracked = map[string]string{fileA: contentA, fileB: contentB}

func open(t *testing.T, f *fixture, l layout.Layout, branch string) (Result, error) {
	t.Helper()
	return Open(context.Background(), f.options(l, branch))
}

func TestOpenClonesIntoAnEmptyDirectory(t *testing.T) {
	for _, sub := range []string{subPath, "team/" + subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			f := newFixture(t)
			tip := f.publish(branchMain, tracked)
			l := f.layout(sub)

			got, err := open(t, f, l, branchMain)

			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != Cloned || got.Branch != branchMain || got.Commit != tip {
				t.Errorf("result = %+v, want Cloned on %s at %s", got, branchMain, tip)
			}
			wantFiles(t, l, tracked)
			if branch := f.inCheckout(l, "symbolic-ref", "HEAD"); branch != headsPrefix+branchMain {
				t.Errorf("HEAD = %s, want it on %s", branch, branchMain)
			}
			if status := f.inCheckout(l, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, fileA) {
				t.Errorf("status = %q, want the clone clean", status)
			}
			if exists(filepath.Join(l.StateDir, markerName)) {
				t.Error("the marker of the first checkout is still there after it finished")
			}
		})
	}
}

func TestOpenMakesTheSyncedDirectoryAndKeepsTheUrlOutOfTheConfig(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if exists(l.WorkTree) {
		t.Fatal("the synced directory was there before the test")
	}

	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}

	// The address is not in the config, where a credential would sit next to it, nor in
	// FETCH_HEAD, and there is no remote for a config of the repository to redefine (NFR-3).
	config, err := os.ReadFile(filepath.Join(l.GitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), f.bare) || strings.Contains(string(config), "remote") {
		t.Errorf("the config names the repository or a remote:\n%s", config)
	}
	if exists(filepath.Join(l.GitDir, "FETCH_HEAD")) {
		t.Error("FETCH_HEAD, which names the address of the repository, was written")
	}
}

func TestOpenFollowsTheDefaultBranchWhenNoneIsNamed(t *testing.T) {
	f := newFixture(t)
	f.plain(f.root, "-C", f.bare, "symbolic-ref", "HEAD", headsPrefix+branchTrunk)
	tip := f.publish(branchTrunk, tracked)
	l := f.layout(subPath)

	got, err := open(t, f, l, "")

	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != branchTrunk || got.Outcome != Cloned || got.Commit != tip {
		t.Errorf("result = %+v, want Cloned on %s at %s", got, branchTrunk, tip)
	}
}

func TestOpenFollowsTheBranchThatIsNamed(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, map[string]string{fileA: "on main\n"})
	other := f.publish("other", map[string]string{fileA: onOther})
	l := f.layout(subPath)

	got, err := open(t, f, l, "other")

	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "other" || got.Commit != other || readA(t, l) != onOther {
		t.Errorf("result = %+v, file = %q, want the branch other", got, readA(t, l))
	}
}

func TestOpenAdoptsADirectoryThatMatchesTheBranchExactly(t *testing.T) {
	for _, sub := range []string{subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			f := newFixture(t)
			tip := f.publish(branchMain, tracked)
			l := f.layout(sub)
			write(t, l, fileA, contentA)
			write(t, l, fileB, contentB)
			before, err := os.Stat(filepath.Join(l.WorkTree, fileA))
			if err != nil {
				t.Fatal(err)
			}

			got, err := open(t, f, l, branchMain)

			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != Adopted || got.Commit != tip {
				t.Errorf("result = %+v, want Adopted at %s", got, tip)
			}
			after, err := os.Stat(filepath.Join(l.WorkTree, fileA))
			if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Errorf("the file was written to: %v", err)
			}
			if status := f.inCheckout(l, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, fileA) || strings.Contains(status, fileB) {
				t.Errorf("status = %q, want the adopted directory clean", status)
			}
		})
	}
}

func TestOpenRefusesADirectoryThatDoesNotMatchAndLeavesItAsItIs(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"a file that differs", map[string]string{fileA: "changed\n", fileB: contentB}},
		{"a file that is missing", map[string]string{fileA: contentA}},
		{"a file that is extra", map[string]string{fileA: contentA, fileB: contentB, "extra.txt": mine}},
		{"another set of files", map[string]string{"notes.txt": mine}},
	}
	for _, tt := range tests {
		for _, sub := range []string{subPath, ""} {
			t.Run(tt.name+", sub path "+sub, func(t *testing.T) {
				f := newFixture(t)
				f.publish(branchMain, tracked)
				l := f.layout(sub)
				for name, content := range tt.files {
					write(t, l, name, content)
				}

				_, err := open(t, f, l, branchMain)

				got := errorOf(t, err, ReasonDirectoryNotEmpty)
				if !strings.Contains(got.Message, l.WorkTree) || !strings.Contains(got.Message, "file(s) differ") {
					t.Errorf("message = %q, want the directory and how many files differ", got.Message)
				}
				wantFiles(t, l, tt.files)
				entries, readErr := os.ReadDir(l.GitDir)
				if readErr != nil || len(entries) != 0 {
					t.Errorf("the Git directory holds %d entries after the refusal, want none: %v", len(entries), readErr)
				}
			})
		}
	}
}

func TestOpenNamesAFewOfTheFilesThatDiffer(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	for _, name := range []string{"1.txt", "2.txt", "3.txt", "4.txt", "5.txt"} {
		write(t, l, name, "x\n")
	}

	_, err := open(t, f, l, branchMain)

	got := errorOf(t, err, ReasonDirectoryNotEmpty)
	// Both files of the branch are missing too, so seven differ.
	if !strings.Contains(got.Message, "7 file(s) differ") || strings.Count(got.Message, ".txt")+strings.Count(got.Message, ".json") != maxNamed {
		t.Errorf("message = %q, want the count and %d names", got.Message, maxNamed)
	}
}

func TestOpenJudgesARefusedDirectoryAfreshOnTheNextStart(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	write(t, l, "notes.txt", mine)
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("a directory that does not match was taken")
	}
	if err := os.Remove(filepath.Join(l.WorkTree, "notes.txt")); err != nil {
		t.Fatal(err)
	}

	got, err := open(t, f, l, branchMain)

	if err != nil || got.Outcome != Cloned {
		t.Fatalf("result = %+v, %v, want Cloned once the directory was emptied", got, err)
	}
}

func TestOpenReopensTheCheckoutOfAnEarlierStart(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	// Local work, and a branch of the repository that moved on, are neither of them a
	// reason to refuse or to touch anything.
	write(t, l, fileA, "edited here\n")
	write(t, l, "new.txt", "made here\n")
	f.publish(branchMain, map[string]string{"remote.txt": "made there\n"})
	head := f.inCheckout(l, "rev-parse", "HEAD")

	got, err := open(t, f, l, branchMain)

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Reopened || got.Commit != head {
		t.Errorf("result = %+v, want Reopened at %s", got, head)
	}
	wantFiles(t, l, map[string]string{fileA: "edited here\n", fileB: contentB, "new.txt": "made here\n"})
}

func TestOpenReopensACheckoutThatHasCommitsOfItsOwn(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	write(t, l, "local.txt", "local\n")
	f.inCheckout(l, "add", "local.txt")
	f.inCheckout(l, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "local")
	local := f.inCheckout(l, "rev-parse", "HEAD")

	got, err := open(t, f, l, branchMain)

	if err != nil || got.Outcome != Reopened || got.Commit != local {
		t.Fatalf("result = %+v, %v, want Reopened at %s", got, err, local)
	}
}

func TestOpenRefusesTheGitDirectoryOfAnotherRepository(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	write(t, l, fileA, "local work\n")
	// The Treebind now names a repository that shares no history with the one that was
	// checked out.
	other := newFixture(t)
	other.url = "ssh://localhost" + other.bare
	other.publish(branchMain, map[string]string{"unrelated.txt": "x\n"})
	options := f.options(l, branchMain)
	options.URL = other.url

	_, err := Open(context.Background(), options)

	got := errorOf(t, err, ReasonRepositoryMismatch)
	if !strings.Contains(got.Message, "no history in common") {
		t.Errorf("message = %q, want it to say the history is not shared", got.Message)
	}
	if readA(t, l) != "local work\n" {
		t.Error("the local work was touched")
	}
}

func TestOpenReopensTheSameRepositoryAtAnotherAddress(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	options := f.options(l, branchMain)
	options.URL = "ssh://localhost" + f.bare + "/"

	got, err := Open(context.Background(), options)

	if err != nil || got.Outcome != Reopened {
		t.Fatalf("result = %+v, %v, want Reopened: the history is the same", got, err)
	}
}

func TestOpenRefusesACheckoutOfAnotherBranch(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	f.publish("other", tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	write(t, l, fileA, "local work\n")

	_, err := open(t, f, l, "other")

	got := errorOf(t, err, ReasonRepositoryMismatch)
	// It says which branch the checkout follows, and how to keep following it, before it says
	// how to start again, which loses what was not pushed.
	if !strings.Contains(got.Message, "follows the branch "+branchMain) || !strings.Contains(got.Message, "set repository.branch to "+branchMain) &&
		!strings.Contains(got.Message, "Set repository.branch to "+branchMain) {
		t.Errorf("message = %q, want the branch it follows and how to keep following it", got.Message)
	}
	if readA(t, l) != "local work\n" {
		t.Error("the local work was touched")
	}
}

func TestOpenRefusesABranchTheRepositoryDoesNotHave(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)

	_, err := open(t, f, l, "missing")

	got := errorOf(t, err, ReasonBranchNotFound)
	if !strings.Contains(got.Message, "missing") {
		t.Errorf("message = %q, want it to name the branch", got.Message)
	}
	if exists(filepath.Join(l.GitDir, "HEAD")) {
		t.Error("a repository was made for a branch that does not exist")
	}
}

func TestOpenStartsOnARepositoryWithoutCommits(t *testing.T) {
	t.Run("an empty directory", func(t *testing.T) {
		f := newFixture(t)
		l := f.layout(subPath)

		got, err := open(t, f, l, branchMain)

		if err != nil || got.Outcome != Cloned || got.Commit != "" || got.Branch != branchMain {
			t.Fatalf("result = %+v, %v, want Cloned on %s with no commit", got, err, branchMain)
		}
	})
	t.Run("a directory with files, which the first sync will commit", func(t *testing.T) {
		f := newFixture(t)
		l := f.layout(subPath)
		write(t, l, "mine.txt", mine)

		got, err := open(t, f, l, "")

		if err != nil || got.Outcome != Adopted || got.Commit != "" || got.Branch != branchMain {
			t.Fatalf("result = %+v, %v, want Adopted on %s with no commit", got, err, branchMain)
		}
		wantFiles(t, l, map[string]string{"mine.txt": mine})
		if branch := f.inCheckout(l, "symbolic-ref", "HEAD"); branch != headsPrefix+branchMain {
			t.Errorf("HEAD = %s, want %s%s", branch, headsPrefix, branchMain)
		}
	})
	t.Run("again on the next start", func(t *testing.T) {
		f := newFixture(t)
		l := f.layout(subPath)
		if _, err := open(t, f, l, branchMain); err != nil {
			t.Fatal(err)
		}

		got, err := open(t, f, l, branchMain)

		if err != nil || got.Commit != "" {
			t.Fatalf("result = %+v, %v, want a checkout that still has no commit", got, err)
		}
	})
}

func TestOpenFinishesAFirstCheckoutThatStoppedHalfWay(t *testing.T) {
	f := newFixture(t)
	tip := f.publish(branchMain, tracked)
	l := f.layout(subPath)
	crash := errors.New("the pod was killed")
	beforeCheckout = func() error { return crash }
	t.Cleanup(func() { beforeCheckout = nil })

	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	// The checkout had written one file and held the lock of the index.
	write(t, l, fileA, contentA)
	if err := os.WriteFile(filepath.Join(l.GitDir, "index.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.publish(branchMain, map[string]string{"later.txt": "after the crash\n"})

	got, err := open(t, f, l, branchMain)

	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Cloned || got.Commit != tip {
		t.Errorf("result = %+v, want Cloned at %s, the commit the first checkout began with", got, tip)
	}
	wantFiles(t, l, tracked)
	if exists(filepath.Join(l.StateDir, markerName)) {
		t.Error("the marker is still there after the checkout finished")
	}
}

func TestOpenJudgesAnUnfinishedCheckoutWithoutAMarkerAsAFirstStart(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	// Without the marker the files cannot be told from somebody else's.
	if err := os.Remove(filepath.Join(l.StateDir, markerName)); err != nil {
		t.Fatal(err)
	}
	write(t, l, fileA, contentA)

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonDirectoryNotEmpty)
	wantFiles(t, l, map[string]string{fileA: contentA})
}

func TestOpenDoesNotClearAGitDirectoryThatHoldsABranch(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.MkdirAll(l.WorkTree, 0o755); err != nil {
		t.Fatal(err)
	}
	// Somebody's repository, made as the agent makes its own, with no template: HEAD is on a
	// branch without a commit, and another branch has one.
	f.inCheckout(l, "init", "-q", "--template=", "--initial-branch="+branchMain)
	f.inCheckout(l, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "theirs")
	f.inCheckout(l, "branch", "-m", "elsewhere")
	f.inCheckout(l, "symbolic-ref", "HEAD", headsPrefix+branchMain)

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonRepositoryMismatch)
	if got := f.inCheckout(l, "for-each-ref", "--format=%(refname)", "refs/heads"); got != headsPrefix+"elsewhere" {
		t.Errorf("branches = %q, want the branch of the repository to be left", got)
	}
}

func TestOpenRefusesABranchWithAFileWhereTheAgentKeepsItsState(t *testing.T) {
	for _, name := range []string{stateName, ".TreeBind"} {
		t.Run(name+" with the whole claim", func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, map[string]string{name + "/git/config": "[core]\n\tfsmonitor = /bin/sh\n"})
			l := f.layout("")

			_, err := open(t, f, l, branchMain)

			got := errorOf(t, err, ReasonReservedPath)
			if !strings.Contains(got.Message, name) || !strings.Contains(got.Message, "subPath") {
				t.Errorf("message = %q, want the name and the advice to use a subPath", got.Message)
			}
			if exists(filepath.Join(l.GitDir, "config")) {
				t.Error("the Git directory was not cleared after the refusal")
			}
			config, readErr := os.ReadFile(filepath.Join(l.GitDir, "config"))
			if readErr == nil && strings.Contains(string(config), "fsmonitor") {
				t.Error("the config of the branch was written to the Git directory")
			}
		})
	}
	t.Run("a sub path, where the paths of the branch cannot reach the state directory", func(t *testing.T) {
		f := newFixture(t)
		f.publish(branchMain, map[string]string{stateName + "/notes.txt": "n\n"})
		l := f.layout(subPath)

		got, err := open(t, f, l, branchMain)

		if err != nil || got.Outcome != Cloned {
			t.Fatalf("result = %+v, %v, want the branch cloned: the state directory is beside the work tree", got, err)
		}
		wantFiles(t, l, map[string]string{stateName + "/notes.txt": "n\n"})
	})
}

func TestOpenSaysWhenTheRepositoryCannotBeReached(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	options := f.options(l, branchMain)
	options.URL = "ssh://localhost" + filepath.Join(f.root, "missing.git")

	_, err := Open(context.Background(), options)

	got := errorOf(t, err, ReasonFetchFailed)
	if got.Err == nil {
		t.Error("the error of the command is not kept")
	}
	// The next start, with the repository back, is a first start.
	if got, err := open(t, f, l, branchMain); err != nil || got.Outcome != Cloned {
		t.Fatalf("result = %+v, %v, want Cloned once the repository can be reached", got, err)
	}
}

func TestOpenRefusesWhatItCannotUse(t *testing.T) {
	f := newFixture(t)
	l := f.layout(subPath)
	for name, change := range map[string]struct {
		mutate func(*Options)
		say    string
	}{
		"no URL":      {func(o *Options) { o.URL = "" }, "URL"},
		"a branch":    {func(o *Options) { o.Branch = "-x" }, "must not start with '-'"},
		"ssh by name": {func(o *Options) { o.Git.SSH.Program = "relative/ssh" }, "SSH.Program"},
	} {
		t.Run(name, func(t *testing.T) {
			options := f.options(l, branchMain)
			change.mutate(&options)

			_, err := Open(context.Background(), options)

			if err == nil || !strings.Contains(err.Error(), change.say) {
				t.Fatalf("error = %v, want one that says %q", err, change.say)
			}
		})
	}
}
