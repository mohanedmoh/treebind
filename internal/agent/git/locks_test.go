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

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// plant makes a file in a directory, and the directories above it.
func plant(t *testing.T, dir, rel string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemoveStaleLocksRemovesTheLocksThatGitMakes(t *testing.T) {
	gitDir := tempDir(t)
	locks := []string{
		lockHead, lockOrigHead, lockConfig, indexLock, lockPackedRefs, "FETCH_HEAD.lock",
		lockBranch, "refs/heads/feature/deep.lock", lockRemoteRef,
		"refs/treebind/recovery/00000001.lock",
	}
	for _, lock := range locks {
		plant(t, gitDir, lock)
	}
	got, err := RemoveStaleLocks(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(locks))
	if !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	for _, lock := range locks {
		if exists(filepath.Join(gitDir, lock)) {
			t.Errorf("%s is still there", lock)
		}
	}
	if again, err := RemoveStaleLocks(gitDir); err != nil || len(again) != 0 {
		t.Errorf("a second call removed %v, %v, want nothing", again, err)
	}
}

func TestRemoveStaleLocksLeavesWhatIsNotALockOfGit(t *testing.T) {
	gitDir := tempDir(t)
	keep := []string{
		refHead, "index", configFile, "packed-refs", "ORIG_HEAD", "description",
		"refs/heads/main", "refs/heads/lock", "refs/heads/notalock.locked", "logs/refs/heads/main",
		"objects/ab/cdef", "objects/pack/tmp_pack_x",
		// Names that Git does not lock, at the top of the Git directory and in the directories that are
		// not refs. The agent does not make them, and the check of the Git directory says what they are.
		"unknown.lock", "hooks/pre-commit.lock", "objects/info/commit-graphs/commit-graph-chain.lock", "logs/HEAD.lock",
	}
	for _, name := range keep {
		plant(t, gitDir, name)
	}
	got, err := RemoveStaleLocks(gitDir)
	if err != nil || len(got) != 0 {
		t.Fatalf("removed %v, %v, want nothing", got, err)
	}
	for _, name := range keep {
		if !exists(filepath.Join(gitDir, name)) {
			t.Errorf("%s was removed", name)
		}
	}
}

func TestRemoveStaleLocksFollowsNoLinkAndRemovesNoDirectory(t *testing.T) {
	gitDir, elsewhere := tempDir(t), tempDir(t)
	outside := plant(t, elsewhere, "keep.lock")
	outsideInRefs := plant(t, elsewhere, "inside.lock")

	// A link named like a lock is not one that Git made, and a link to a directory is not a directory of refs.
	mustSymlink(t, outside, filepath.Join(gitDir, "index.lock"))
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, elsewhere, filepath.Join(gitDir, "refs", "linked"))
	mustSymlink(t, outside, filepath.Join(gitDir, "refs", "alias.lock"))
	// A directory is not a lock file either.
	for _, dir := range []string{"HEAD.lock", "refs/heads/x.lock"} {
		if err := os.MkdirAll(filepath.Join(gitDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := RemoveStaleLocks(gitDir)
	if err != nil || len(got) != 0 {
		t.Fatalf("removed %v, %v, want nothing", got, err)
	}
	for _, path := range []string{
		outside, outsideInRefs, filepath.Join(gitDir, "index.lock"), filepath.Join(gitDir, "refs", "linked"),
		filepath.Join(gitDir, "refs", "alias.lock"), filepath.Join(gitDir, "HEAD.lock"), filepath.Join(gitDir, "refs", "heads", "x.lock"),
	} {
		if !exists(path) {
			t.Errorf("%s was removed", path)
		}
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveStaleLocksWithNoGitDirectoryRemovesNothing(t *testing.T) {
	got, err := RemoveStaleLocks(filepath.Join(tempDir(t), "absent"))
	if err != nil || len(got) != 0 {
		t.Errorf("removed %v, %v, want nothing and no error", got, err)
	}
}

func TestRemoveStaleLocksSaysWhatItCouldNotRemoveAndGoesOn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file in a directory that it cannot write")
	}
	gitDir := tempDir(t)
	plant(t, gitDir, "index.lock")
	plant(t, gitDir, "refs/heads/main.lock")
	plant(t, gitDir, "refs/remotes/origin/main.lock")
	locked := filepath.Join(gitDir, "refs", "heads")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got, err := RemoveStaleLocks(gitDir)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want one that says what was refused", err)
	}
	if want := []string{indexLock, lockRemoteRef}; !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v: the others are removed although one could not be", got, want)
	}
	if !exists(filepath.Join(gitDir, "refs/heads/main.lock")) {
		t.Error("the lock in the directory that cannot be written is gone")
	}
}

func TestRemoveStaleLocksRefusesWhileACommandRuns(t *testing.T) {
	root := tempDir(t)
	ready := filepath.Join(root, "ready")
	r := fakeGit(t, root, fmt.Sprintf("echo ready > '%s'\nwhile :; do :; done", ready))
	gitDir := r.gitDir
	lock := plant(t, gitDir, "index.lock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(ctx, cmdStatus)
	}()
	eventually(t, "the command to run", func() bool { return exists(ready) })

	got, err := RemoveStaleLocks(gitDir)
	if !errors.Is(err, ErrBusy) || len(got) != 0 {
		t.Errorf("removed %v, %v, want nothing and ErrBusy: the lock may be the command's own", got, err)
	}
	if !exists(lock) {
		t.Fatal("the lock of the command that runs was removed")
	}

	// Another Git directory is not affected by it.
	if _, err := RemoveStaleLocks(tempDir(t)); err != nil {
		t.Errorf("a command on one Git directory stops the removal on another: %v", err)
	}

	cancel()
	<-done
	got, err = RemoveStaleLocks(gitDir)
	if err != nil || !slices.Equal(got, []string{"index.lock"}) {
		t.Errorf("once the command has ended: removed %v, %v, want the lock removed", got, err)
	}
}

func TestACommandThatStartsWaitsForARemovalThatIsRunning(t *testing.T) {
	// The check that nothing runs and the removal are one step as far as a command that starts is
	// concerned: it does not start in between, and then it would hold a lock that is removed under it.
	root := tempDir(t)
	started := filepath.Join(root, "started")
	r := fakeGit(t, root, fmt.Sprintf("echo started > '%s'", started))
	plant(t, r.gitDir, "index.lock")

	ended := make(chan struct{})
	duringRemoval = func() {
		go func() {
			defer close(ended)
			_, _ = r.Run(context.Background(), cmdStatus)
		}()
		time.Sleep(300 * time.Millisecond)
		if exists(started) {
			t.Error("a command started while the locks were being removed")
		}
	}
	t.Cleanup(func() { duringRemoval = nil })

	if _, err := RemoveStaleLocks(r.gitDir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the removal did not call the hook that the test set, or the command did not end")
	}
	if !exists(started) {
		t.Error("the command did not run once the removal was over")
	}
}

func TestStaleLocksListsWhatWouldBeRemovedAndRemovesNothing(t *testing.T) {
	gitDir := tempDir(t)
	for _, name := range []string{indexLock, lockBranch, "refs/heads/main", "unknown.lock"} {
		plant(t, gitDir, name)
	}

	got, err := StaleLocks(gitDir)

	if want := []string{indexLock, lockBranch}; err != nil || !slices.Equal(got, want) {
		t.Errorf("StaleLocks = %v, %v, want %v", got, err, want)
	}
	for _, name := range []string{"index.lock", "refs/heads/main.lock"} {
		if !exists(filepath.Join(gitDir, name)) {
			t.Errorf("%s was removed by a call that only looks", name)
		}
	}
	if got, err := StaleLocks(filepath.Join(gitDir, "absent")); err != nil || len(got) != 0 {
		t.Errorf("StaleLocks of a directory that is not there = %v, %v, want nothing", got, err)
	}
}

func TestRemoveStaleLocksGoesOnWhenALockIsGoneByTheTimeItRemovesIt(t *testing.T) {
	gitDir := tempDir(t)
	plant(t, gitDir, indexLock)
	plant(t, gitDir, lockBranch)
	// Something else removed one between the look and the removal, as a Git that was only slow does.
	duringRemoval = func() {
		if err := os.Remove(filepath.Join(gitDir, indexLock)); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { duringRemoval = nil })

	got, err := RemoveStaleLocks(gitDir)

	if err != nil || !slices.Equal(got, []string{lockBranch}) {
		t.Errorf("removed %v, %v, want the one that was still there, and no error for the one that was not", got, err)
	}
}

func TestRemoveStaleLocksAndStaleLocksSayWhenTheGitDirectoryCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory that it has no right to")
	}
	gitDir := tempDir(t)
	plant(t, gitDir, indexLock)
	if err := os.Chmod(gitDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	if got, err := RemoveStaleLocks(gitDir); err == nil || len(got) != 0 {
		t.Errorf("RemoveStaleLocks = %v, %v, want nothing removed and the error", got, err)
	}
	if got, err := StaleLocks(gitDir); err == nil || len(got) != 0 {
		t.Errorf("StaleLocks = %v, %v, want nothing listed and the error", got, err)
	}
}
