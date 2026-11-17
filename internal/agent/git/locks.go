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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrBusy means a Git command of this process is running on the Git directory, so no lock file in
// it can be told from one that the command holds.
var ErrBusy = errors.New("a Git command is running on the Git directory")

// running counts the Git commands of this process that run, by Git directory. A command that holds a
// lock file is the reason a lock file is there, and a command is the only thing that this process
// starts that makes one, so a lock file is stale when none runs (docs/adr/0028). The mutex is held by
// RemoveStaleLocks for as long as it looks and removes, and by a command only while it counts itself,
// so that no command starts between the look at the count and the removal.
var running = struct {
	sync.Mutex
	commands map[string]int
}{commands: map[string]int{}}

// duringRemoval is called by a test between the look for lock files and their removal, while
// RemoveStaleLocks holds off the commands that start.
var duringRemoval func()

// begin counts a command that runs on the Git directory, and returns the function that says it ended.
func begin(gitDir string) (end func()) {
	running.Lock()
	running.commands[gitDir]++
	running.Unlock()
	return func() {
		running.Lock()
		defer running.Unlock()
		if running.commands[gitDir]--; running.commands[gitDir] <= 0 {
			delete(running.commands, gitDir)
		}
	}
}

// packedRefs is locked by a command that removes a ref, whether or not there are packed refs. The agent
// never makes any, and its check of the Git directory does not accept the file.
const packedRefs = "packed-refs"

// topLockName reports whether a name at the top of the Git directory is the lock file of a file that the
// agent's commands write there: the files that it makes, and packed-refs. A name of another kind that
// ends in .lock is not one of Git's, and is left to the check of the Git directory.
func topLockName(name string) bool {
	base, isLock := strings.CutSuffix(name, lockSuffix)
	if !isLock {
		return false
	}
	if base == packedRefs {
		return true
	}
	kind, makes := gitDirEntries[base]
	return makes && kind == kindFile
}

// StaleLocks lists what RemoveStaleLocks would remove, and removes nothing. It is for the caller that
// has to check the writer lock before it removes anything, and only when there is something to remove.
func StaleLocks(gitDir string) ([]string, error) {
	root, err := os.OpenRoot(filepath.Clean(gitDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	found, failed := findLocks(root, gitDir)
	return found, errors.Join(failed...)
}

// findLocks lists the lock files in the Git directory, in order, and the errors of what it could not
// look at.
func findLocks(root *os.Root, gitDir string) (found []string, failed []error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, []error{err}
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && topLockName(entry.Name()) {
			found = append(found, entry.Name())
		}
	}
	// WalkDir does not follow a link, so a link to a directory is not entered.
	walked := fs.WalkDir(root.FS(), "refs", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failed = append(failed, fmt.Errorf("%s cannot be looked at: %w", filepath.Join(gitDir, filepath.FromSlash(name)), err))
			}
			return nil
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), lockSuffix) {
			found = append(found, name)
		}
		return nil
	})
	if walked != nil {
		failed = append(failed, walked)
	}
	slices.Sort(found)
	return found, failed
}

// RemoveStaleLocks removes the lock files that Git made in the Git directory and did not remove,
// because it was killed in the middle of a write: the lock of a file at the top of the Git directory
// and every lock under refs, and returns their names relative to the Git directory, in order. It
// removes only a regular file: a link or a directory with such a name is not one that Git made, and no
// link is followed. It goes on after one that it cannot remove, and returns what it could not as an
// error with the ones that it did.
//
// It is for an agent that holds the writer lock of the volume (NFR-1), which is why no other process
// writes here, and that runs one Git command at a time. It returns ErrBusy, and removes nothing, while
// a command of this process runs on the Git directory, and a command that starts while it works waits
// for it. Whoever calls it with the lock to make that true is the caller (docs/adr/0028).
func RemoveStaleLocks(gitDir string) ([]string, error) {
	gitDir = filepath.Clean(gitDir)
	running.Lock()
	defer running.Unlock()
	if running.commands[gitDir] > 0 {
		return nil, ErrBusy
	}

	root, err := os.OpenRoot(gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	found, failed := findLocks(root, gitDir)
	if duringRemoval != nil {
		duringRemoval()
	}
	var removed []string
	for _, name := range found {
		switch err := root.Remove(filepath.FromSlash(name)); {
		case err == nil:
			removed = append(removed, name)
		case !errors.Is(err, fs.ErrNotExist):
			failed = append(failed, fmt.Errorf("the lock file %s cannot be removed: %w", filepath.Join(gitDir, filepath.FromSlash(name)), err))
		}
	}
	return removed, errors.Join(failed...)
}
