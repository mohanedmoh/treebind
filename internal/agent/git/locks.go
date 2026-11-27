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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrBusy means a Git command of this process is running on the Git directory, so no lock file or
// temporary file in it can be told from one that the command holds.
var ErrBusy = errors.New("a Git command is running on the Git directory")

// running counts the Git commands of this process that run, by Git directory. A command that holds a
// lock file is the reason a lock file is there, and a command is the only thing that this process
// starts that makes one, so a lock file is stale when none runs (docs/adr/0028). The mutex is held by
// RemoveStaleLocks for as long as it looks and removes, by RemoveStaleTempObjects while it removes, and by
// a command only while it counts itself, so that no command starts between the look at the count and the
// removal.
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

// The object store, the directory of its packs, and how the names of the temporary files that Git writes there begin, before
// it moves each to its name. They are the names that git prune takes for temporary files: tmp_obj_ in a directory of loose
// objects, and any tmp_ at the top of the store and among the packs. Among the packs a repack has one more: pack-objects moves
// the pack that it wrote to .tmp-<pid>-pack-<hash>, and the repack then moves it to pack-<hash>, the index last, and removes what
// it made redundant only after that (docs/adr/0043).
const (
	objectsDir       = "objects"
	packsDir         = "objects/pack"
	tempPrefix       = "tmp_"
	tempObjectPrefix = "tmp_obj_"
	repackTempPrefix = ".tmp-"
)

// unindexedPack matches the files of a pack that Git reads only through the index of the pack: the pack itself, and the reverse
// index, the bitmap and the times of a pack of unreachable objects beside it. One of them without its index is what a Git that
// was killed between moving the pack and moving its index leaves, a repack or the index-pack of a fetch, and Git never reads it.
// A .keep or a .promisor says something of a pack whether or not the pack is there, and is not one.
var unindexedPack = regexp.MustCompile(`^(pack-[0-9a-f]{40}(?:[0-9a-f]{24})?)\.(?:pack|rev|bitmap|mtimes)$`)

// looseDir reports whether a name at the top of the object store is that of a directory of loose objects: two hexadecimal
// digits.
func looseDir(name string) bool {
	return len(name) == 2 && strings.Trim(name, "0123456789abcdef") == ""
}

// StaleTempObjects lists what RemoveStaleTempObjects would remove, and removes nothing. It is for the caller that checks the
// writer lock before it removes anything, and only when there is something to remove.
func StaleTempObjects(gitDir string, olderThan time.Duration) ([]string, error) {
	root, err := os.OpenRoot(filepath.Clean(gitDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	found, failed := findTempObjects(root, gitDir, olderThan)
	return found, errors.Join(failed...)
}

// findTempObjects lists the temporary files of the object store, in order, that have not changed for olderThan, or all of them
// when it is not positive, and the errors of what it could not look at. WalkDir is not used: the directories of loose objects
// can hold many objects, and only the names are looked at.
func findTempObjects(root *os.Root, gitDir string, olderThan time.Duration) (found []string, failed []error) {
	now := time.Now()
	look := func(dir string, temporary func(names map[string]bool, name string) bool) []fs.DirEntry {
		entries, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failed = append(failed, fmt.Errorf("%s cannot be looked at: %w", filepath.Join(gitDir, filepath.FromSlash(dir)), err))
			}
			return nil
		}
		names := make(map[string]bool, len(entries))
		for _, entry := range entries {
			names[entry.Name()] = true
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !temporary(names, entry.Name()) {
				continue
			}
			if olderThan > 0 {
				info, err := entry.Info()
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						failed = append(failed, fmt.Errorf("%s cannot be looked at: %w",
							filepath.Join(gitDir, filepath.FromSlash(dir), entry.Name()), err))
					}
					continue
				}
				if now.Sub(info.ModTime()) < olderThan {
					continue
				}
			}
			found = append(found, path.Join(dir, entry.Name()))
		}
		return entries
	}
	prefixed := func(prefix string) func(map[string]bool, string) bool {
		return func(_ map[string]bool, name string) bool { return strings.HasPrefix(name, prefix) }
	}
	for _, entry := range look(objectsDir, prefixed(tempPrefix)) {
		// A link is not a directory here, so no link is followed.
		if entry.IsDir() && looseDir(entry.Name()) {
			look(path.Join(objectsDir, entry.Name()), prefixed(tempObjectPrefix))
		}
	}
	look(packsDir, func(names map[string]bool, name string) bool {
		if strings.HasPrefix(name, tempPrefix) || strings.HasPrefix(name, repackTempPrefix) {
			return true
		}
		m := unindexedPack.FindStringSubmatch(name)
		return m != nil && !names[m[1]+".idx"]
	})
	slices.Sort(found)
	return found, failed
}

// RemoveStaleTempObjects removes the temporary files that Git wrote in the object store of the Git directory and did not move
// to their names, because it failed in the middle of the write, as a fetch on a full volume does, or was killed, and the files of
// a pack that a killed repack or fetch left without its index, and returns their names relative to the Git directory, in order.
// Git does not remove them when it fails, and the next repack does not either, so they stay, and on a full volume they keep it
// full. None of them holds an object that Git reads: Git reads a pack through its index, and a repack removes what it made
// redundant only once its new pack has its index (docs/adr/0043). It removes only a regular file that has
// not changed for olderThan, or any when it is not positive, follows no link, and goes on after one that it cannot remove.
//
// It returns ErrBusy, and removes nothing, while a command of this process runs on the Git directory when the removal comes,
// as RemoveStaleLocks does. That no command of another process writes there is the caller's to make true: with the writer lock
// (NFR-1), or with an age that no command of an agent outlives.
//
// The look at the object store, which reads many directories and which a volume that does not answer holds, keeps no command
// from starting or ending: a file that a command made while it looked is that of a command that still runs at the removal, which
// is then refused, or of one that has ended, and then what it left is stale too, or gone.
func RemoveStaleTempObjects(gitDir string, olderThan time.Duration) ([]string, error) {
	gitDir = filepath.Clean(gitDir)
	root, err := os.OpenRoot(gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	found, failed := findTempObjects(root, gitDir, olderThan)
	if len(found) == 0 {
		return nil, errors.Join(failed...)
	}
	running.Lock()
	defer running.Unlock()
	if running.commands[gitDir] > 0 {
		return nil, ErrBusy
	}
	var removed []string
	for _, name := range found {
		switch err := root.Remove(filepath.FromSlash(name)); {
		case err == nil:
			removed = append(removed, name)
		case !errors.Is(err, fs.ErrNotExist):
			failed = append(failed, fmt.Errorf("the temporary file %s cannot be removed: %w", filepath.Join(gitDir, filepath.FromSlash(name)), err))
		}
	}
	return removed, errors.Join(failed...)
}
