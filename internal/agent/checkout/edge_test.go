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

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const notAnObject = "0123456789abcdef0123456789abcdef01234567"

func TestErrorHasTheMessageAndTheCauseUnderneath(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Open(ctx, f.options(l, branchMain))

	got := errorOf(t, err, ReasonFetchFailed)
	if err.Error() != got.Message || !errors.Is(err, git.ErrCanceled) {
		t.Errorf("error = %q, want the message, and the cause of the command to be reachable", err.Error())
	}
}

func TestOpenRefusesACheckoutThatIsNotOnABranch(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	f.inCheckout(l, "checkout", "-q", "--detach")

	_, err := open(t, f, l, branchMain)

	got := errorOf(t, err, ReasonRepositoryMismatch)
	if !strings.Contains(got.Message, "follows no branch") {
		t.Errorf("message = %q, want it to say the checkout follows no branch", got.Message)
	}
}

func TestOpenSaysWhenTheGitDirectoryIsNotARepository(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.WriteFile(filepath.Join(l.GitDir, "HEAD"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonCheckoutFailed)
}

func TestOpenReopensACheckoutWhenTheRepositoryHasNoCommitsNow(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	empty := newFixture(t)
	options := f.options(l, branchMain)
	options.URL = empty.url

	got, err := Open(context.Background(), options)

	if err != nil || got.Outcome != Reopened {
		t.Fatalf("result = %+v, %v, want Reopened: there is nothing to compare with, and the first sync pushes", got, err)
	}
}

func TestOpenChecksOutTheTipWhenTheMarkerNamesACommitThatIsGone(t *testing.T) {
	f := newFixture(t)
	tip := f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(notAnObject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, l, fileA, contentA)

	got, err := open(t, f, l, branchMain)

	// The files are still the agent's own, so the checkout is finished with what the branch
	// is at now.
	if err != nil || got.Outcome != Cloned || got.Commit != tip {
		t.Fatalf("result = %+v, %v, want Cloned at %s", got, err, tip)
	}
	wantFiles(t, l, tracked)
}

func TestOpenSaysWhenTheMarkerNamesACommitThatIsGoneAndTheRepositoryHasNone(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(notAnObject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := f.options(l, branchMain)
	options.URL = newFixture(t).url

	_, err := Open(context.Background(), options)

	wantReason(t, err, ReasonCheckoutFailed)
}

func TestOpenTakesAMarkerThatIsNotACommitForNoMarker(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	// A revision that Git can resolve, but that is not the id of a commit.
	if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(remotePrefix+branchMain+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, l, fileA, contentA)

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonDirectoryNotEmpty)
}

func TestOpenSaysWhenTheRepositoryHasNoDefaultBranchToFollow(t *testing.T) {
	f := newFixture(t)
	// HEAD is at a commit and on no branch, and the branch the agent would fall back to is
	// not there.
	tip := f.publish("other", tracked)
	f.plain(f.bare, "update-ref", "--no-deref", "HEAD", tip)
	l := f.layout(subPath)

	_, err := open(t, f, l, "")

	got := errorOf(t, err, ReasonBranchNotFound)
	if !strings.Contains(got.Message, "repository.branch") {
		t.Errorf("message = %q, want it to point to repository.branch", got.Message)
	}
}

func TestOpenDoesNotTakeARepositoryWhoseHeadIsDanglingForAnEmptyOne(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	// The default branch was renamed and HEAD was left pointing at the old name.
	f.plain(f.bare, "symbolic-ref", "HEAD", headsPrefix+"master")
	l := f.layout(subPath)

	_, err := open(t, f, l, "")

	got := errorOf(t, err, ReasonBranchNotFound)
	if !strings.Contains(got.Message, "repository.branch") {
		t.Errorf("message = %q, want it to point to repository.branch", got.Message)
	}
	if exists(filepath.Join(l.GitDir, "HEAD")) {
		t.Error("a repository was made for a branch that does not exist")
	}
}

func TestOpenRefusesADefaultBranchItCannotFollow(t *testing.T) {
	f := newFixture(t)
	f.publish("-bad", tracked)
	f.plain(f.bare, "symbolic-ref", "HEAD", headsPrefix+"-bad")
	l := f.layout(subPath)

	_, err := open(t, f, l, "")

	wantReason(t, err, ReasonBranchNotFound)
}

func TestOpenSaysWhenTheSyncedDirectoryCannotBeMade(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.Symlink(f.root, l.WorkTree); err != nil {
		t.Fatal(err)
	}

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonCheckoutFailed)
}

func TestOpenSaysWhenTheCheckoutCannotBeWrittenTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory that is closed to it")
	}
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.Chmod(l.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.StateDir, 0o700) })

	_, err := open(t, f, l, branchMain)

	// The marker cannot be written, so not a file is.
	wantReason(t, err, ReasonCheckoutFailed)
	if entries, readErr := os.ReadDir(l.WorkTree); readErr != nil || len(entries) != 0 {
		t.Errorf("the work tree holds %d entries, want none: %v", len(entries), readErr)
	}
}

func TestRefuseSaysSoWhenItCannotTakeBackWhatItMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a directory that is closed to it")
	}
	f := newFixture(t)
	l := f.layout(subPath)
	if err := os.Mkdir(filepath.Join(l.GitDir, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.GitDir, "objects", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(l.GitDir, "objects"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(l.GitDir, "objects"), 0o700) })
	c := &checkout{Options: f.options(l, branchMain)}

	err := c.refuse(fail(ReasonDirectoryNotEmpty, "the directory does not match"))

	// It is still the refusal, which is what the user has to act on, with a word about what
	// could not be taken back.
	got := errorOf(t, err, ReasonDirectoryNotEmpty)
	if !strings.Contains(got.Message, "does not match") || !strings.Contains(got.Message, "cannot be removed") {
		t.Errorf("message = %q, want the refusal and the failure to take back what was fetched", got.Message)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want the failure to take it back underneath", err)
	}
}

func TestParseListingReadsTheSymrefTheHeadAndTheBranches(t *testing.T) {
	out := "ref: refs/heads/trunk\tHEAD\n" +
		"1111111111111111111111111111111111111111\tHEAD\n" +
		"1111111111111111111111111111111111111111\trefs/heads/trunk\n" +
		"2222222222222222222222222222222222222222\trefs/heads/feature/x\n" +
		"3333333333333333333333333333333333333333\trefs/tags/v1\n" +
		"a line without a tab\n"

	got := parseListing(out)

	if got.defaultBranch != "trunk" || got.head != strings.Repeat("1", 40) {
		t.Errorf("listing = %+v, want the default branch trunk and its commit", got)
	}
	if len(got.heads) != 2 || got.heads["feature/x"] != strings.Repeat("2", 40) {
		t.Errorf("branches = %v, want the two branches and no tag", got.heads)
	}
	if empty := parseListing(""); empty.head != "" || empty.defaultBranch != "" || len(empty.heads) != 0 {
		t.Errorf("listing of nothing = %+v, want nothing", empty)
	}
}

func TestOpenDoesNotFetchTheTagsOfTheRepository(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	f.plain(f.scrat, "tag", "v1")
	f.plain(f.scrat, "push", "-q", f.bare, "v1")
	l := f.layout(subPath)

	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}

	if tags := f.inCheckout(l, "for-each-ref", "refs/tags"); tags != "" {
		t.Errorf("tags = %q, want none: a tag is a name that the repository chooses", tags)
	}
}

func TestOpenReopensACheckoutWhoseBranchWasRewrittenOnTheRepository(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	f.publish(branchMain, map[string]string{"second.txt": "2\n"})
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	// The history of the repository is rewritten: its last commit is replaced, and a branch
	// that cannot be fast-forwarded is still the branch.
	f.plain(f.scrat, "reset", "-q", "--hard", "HEAD~1")
	f.publish(branchMain, map[string]string{"third.txt": "3\n"})

	got, err := open(t, f, l, branchMain)

	if err != nil || got.Outcome != Reopened {
		t.Fatalf("result = %+v, %v, want Reopened: the history still shares its first commit", got, err)
	}
}

func TestOpenClearsWhatAnAttemptThatFailedLeftInTheGitDirectory(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	broken := f.options(l, branchMain)
	crash := errors.New("the pod was killed")
	beforeCheckout = func() error { return crash }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := Open(context.Background(), broken); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	// Without the marker, what the attempt made is not trusted, and is made again.
	if err := os.Remove(filepath.Join(l.StateDir, markerName)); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(l.GitDir, "objects", "leftover")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := open(t, f, l, branchMain)

	if err != nil || got.Outcome != Cloned {
		t.Fatalf("result = %+v, %v, want Cloned", got, err)
	}
	if exists(leftover) {
		t.Error("what the unfinished attempt left in the Git directory was not cleared")
	}
}

func TestOpenRemovesAMarkerThatAFinishedCheckoutLeftBehind(t *testing.T) {
	f := newFixture(t)
	tip := f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	// The pod was killed between the last file and the removal of the marker.
	if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(tip+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := open(t, f, l, branchMain)

	if err != nil || got.Outcome != Reopened {
		t.Fatalf("result = %+v, %v, want Reopened", got, err)
	}
	if exists(filepath.Join(l.StateDir, markerName)) {
		t.Error("the marker of a checkout that finished was kept")
	}
}

func TestOpenKeepsTheStateDirectoryOfTheApplicationOutOfWhatItIgnores(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	// With a sub path the state directory is beside the work tree, so a directory of this
	// name inside it is the application's, and the directory is not empty.
	write(t, l, stateName+"/notes.txt", mine)

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonDirectoryNotEmpty)
	wantFiles(t, l, map[string]string{stateName + "/notes.txt": mine})
}

func TestOpenDoesNotTakeAHeadThatNamesATagForADefaultBranch(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	f.plain(f.scrat, "tag", "v1")
	f.plain(f.scrat, "push", "-q", f.bare, "v1")
	f.plain(f.bare, "symbolic-ref", "HEAD", "refs/tags/v1")
	l := f.layout(subPath)

	_, err := open(t, f, l, "")

	wantReason(t, err, ReasonBranchNotFound)
}

func TestReadMarkerTakesTheIdOfACommitAndNothingElse(t *testing.T) {
	f := newFixture(t)
	l := f.layout(subPath)
	c := &checkout{Options: f.options(l, branchMain)}
	sha1, sha256 := strings.Repeat("a", 40), strings.Repeat("b", 64)
	tests := []struct {
		name, content string
		want          bool
	}{
		{"a SHA-1 id", sha1 + "\n", true},
		{"a SHA-256 id", sha256 + "\n", true},
		{"an id without the newline", sha1, true},
		{"an id with spaces around it", "  " + sha1 + " \n", true},
		{"one character too short", sha1[1:], false},
		{"one character too long", sha1 + "a", false},
		{"between the two lengths", strings.Repeat("a", 50), false},
		{"capitals", strings.ToUpper(sha1), false},
		{"a ref", remotePrefix + branchMain, false},
		{"nothing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, ok := c.readMarker()

			if ok != tt.want || (ok && got != strings.TrimSpace(tt.content)) {
				t.Errorf("readMarker() = %q, %v, want ok = %v", got, ok, tt.want)
			}
		})
	}
	t.Run("no marker", func(t *testing.T) {
		if err := os.Remove(filepath.Join(l.StateDir, markerName)); err != nil {
			t.Fatal(err)
		}
		if got, ok := c.readMarker(); ok {
			t.Errorf("readMarker() = %q, true, want no marker", got)
		}
	})
}

func TestOpenKeepsTheMarkerToItself(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })

	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}

	info, err := os.Stat(filepath.Join(l.StateDir, markerName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("marker: %v, mode %v, want a file of mode 0600 in the state directory", err, info)
	}
}

func TestOpenDoesNotClearAGitDirectoryThatItDidNotMake(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixture, l layout.Layout)
		want  string
	}{
		{"one made with the default template, which has hooks", func(t *testing.T, f *fixture, l layout.Layout) {
			f.inCheckout(l, "init", "-q", "--initial-branch="+branchMain)
		}, "branches|description|hooks|info"},
		{"a file that Git does not make", func(t *testing.T, f *fixture, l layout.Layout) {
			f.inCheckout(l, "init", "-q", "--template=", "--initial-branch="+branchMain)
			if err := os.WriteFile(filepath.Join(l.GitDir, "notes.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "notes.txt"},
		{"a ref that is not a branch or a tag", func(t *testing.T, f *fixture, l layout.Layout) {
			f.inCheckout(l, "init", "-q", "--template=", "--initial-branch="+branchMain)
			tree := f.inCheckout(l, "write-tree")
			commit := f.inCheckout(l, "commit-tree", tree, "-m", "theirs")
			f.inCheckout(l, "update-ref", "refs/stash", commit)
		}, "refs/stash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, tracked)
			l := f.layout(subPath)
			if err := os.MkdirAll(l.WorkTree, 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, f, l)

			_, err := open(t, f, l, branchMain)

			got := errorOf(t, err, ReasonRepositoryMismatch)
			// Which entry of a default template comes first depends on the release of Git.
			named := false
			for name := range strings.SplitSeq(tt.want, "|") {
				named = named || strings.Contains(got.Message, "holds "+name+",")
			}
			if !named || !strings.Contains(got.Message, "did not make") {
				t.Errorf("message = %q, want it to name %s and say Treebind did not make it", got.Message, tt.want)
			}
			if !exists(filepath.Join(l.GitDir, "HEAD")) {
				t.Error("the Git directory was cleared")
			}
		})
	}
}

func TestOpenDoesNotSwitchACheckoutWhenTheDefaultBranchIsRenamed(t *testing.T) {
	f := newFixture(t)
	f.plain(f.bare, "symbolic-ref", "HEAD", headsPrefix+"master")
	f.publish("master", tracked)
	l := f.layout(subPath)
	if got, err := open(t, f, l, ""); err != nil || got.Branch != "master" {
		t.Fatalf("result = %+v, %v, want the default branch master", got, err)
	}
	write(t, l, "local.txt", "local\n")
	f.plain(f.bare, "symbolic-ref", "HEAD", headsPrefix+branchMain)
	f.publish(branchMain, tracked)

	_, err := open(t, f, l, "")

	got := errorOf(t, err, ReasonRepositoryMismatch)
	// The way to keep following the branch comes before the way to start again.
	keep, again := strings.Index(got.Message, "Set repository.branch to master"), strings.Index(got.Message, "empty ")
	if keep < 0 || again < 0 || keep > again || !strings.Contains(got.Message, "not pushed are lost") {
		t.Errorf("message = %q, want how to keep following master, then how to start again and what it costs", got.Message)
	}
}

func TestOpenStartsAgainWhenTheDefaultBranchIsKnownOnlyAfterAnEmptyStart(t *testing.T) {
	f := newFixture(t)
	// An empty repository names no default branch, so the checkout is made on main and has no
	// commit.
	l := f.layout(subPath)
	if got, err := open(t, f, l, ""); err != nil || got.Branch != branchMain || got.Commit != "" {
		t.Fatalf("result = %+v, %v, want an empty checkout on %s", got, err, branchMain)
	}
	f.plain(f.bare, "symbolic-ref", "HEAD", headsPrefix+"master")
	tip := f.publish("master", tracked)

	got, err := open(t, f, l, "")

	if err != nil || got.Outcome != Cloned || got.Branch != "master" || got.Commit != tip {
		t.Fatalf("result = %+v, %v, want Cloned on master at %s: a checkout that holds nothing is not stuck on its old name", got, err, tip)
	}
}

func TestOpenDoesNotFinishAnotherBranchWithTheCommitOfTheMarker(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, map[string]string{fileA: "on main\n"})
	other := f.publish("other", map[string]string{fileA: onOther})
	l := f.layout(subPath)
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil

	// The Treebind now names another branch, so the commit of the marker is not the one.
	got, err := open(t, f, l, "other")

	if err != nil || got.Outcome != Cloned || got.Branch != "other" || got.Commit != other || readA(t, l) != onOther {
		t.Fatalf("result = %+v, %v, file %q, want Cloned on other at %s", got, err, readA(t, l), other)
	}
}

func TestOpenClearsWhatTheAgentLeftInTheGitDirectoryWhateverItIs(t *testing.T) {
	tests := []struct {
		name  string
		leave func(t *testing.T, f *fixture, l layout.Layout)
	}{
		{"the index", func(t *testing.T, f *fixture, l layout.Layout) {
			f.inCheckout(l, "read-tree", remotePrefix+branchMain)
		}},
		{"the lock of the index", func(t *testing.T, f *fixture, l layout.Layout) {
			if err := os.WriteFile(filepath.Join(l.GitDir, "index.lock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tip := f.publish(branchMain, tracked)
			l := f.layout(subPath)
			beforeCheckout = func() error { return errors.New("the pod was killed") }
			t.Cleanup(func() { beforeCheckout = nil })
			if _, err := open(t, f, l, branchMain); err == nil {
				t.Fatal("the simulated crash did not stop the checkout")
			}
			beforeCheckout = nil
			// What the agent leaves when it is killed while it compares a directory. There is
			// no marker, because no file was written.
			if err := os.Remove(filepath.Join(l.StateDir, markerName)); err != nil {
				t.Fatal(err)
			}
			tt.leave(t, f, l)

			got, err := open(t, f, l, branchMain)

			if err != nil || got.Outcome != Cloned || got.Commit != tip {
				t.Fatalf("result = %+v, %v, want Cloned at %s", got, err, tip)
			}
		})
	}
}

func TestOpenRefusesAReservedPathWhenItFinishesACheckoutWithTheTipOfToday(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout("")
	beforeCheckout = func() error { return errors.New("the pod was killed") }
	t.Cleanup(func() { beforeCheckout = nil })
	if _, err := open(t, f, l, branchMain); err == nil {
		t.Fatal("the simulated crash did not stop the checkout")
	}
	beforeCheckout = nil
	if err := os.WriteFile(filepath.Join(l.StateDir, markerName), []byte(notAnObject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The branch has moved on, and now has a file where the agent keeps its state.
	f.publish(branchMain, map[string]string{stateName + "/git/config": "[core]\n\tfsmonitor = /bin/sh\n"})

	_, err := open(t, f, l, branchMain)

	wantReason(t, err, ReasonReservedPath)
	if config, readErr := os.ReadFile(filepath.Join(l.GitDir, "config")); readErr == nil && strings.Contains(string(config), "fsmonitor") {
		t.Error("the config of the branch was written to the Git directory")
	}
}
