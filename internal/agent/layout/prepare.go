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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/config"
)

// privateMode is the mode of the directories that the agent makes for its own state.
// Nothing in them is for group or others, and the umask of the agent (0002) leaves
// it as it is.
const privateMode fs.FileMode = 0o700

// UnsafeError says that a directory the agent would put its Git directory in, or
// would sync, is not what it should be: it is a link, it is not a directory, it
// belongs to another user or it is open to group or others. Git is not run against
// it. Any other error from Prepare is about the claim and not about the directory:
// it is missing, cannot be written to, or does not answer.
type UnsafeError struct {
	// Path is the directory, as the agent names it.
	Path string
	// Problem says what is wrong with it.
	Problem string
	// OnTheWay is true for a directory on the way to the synced directory, which the
	// application owns, and false for the state directory and the Git directory, which the
	// agent owns. What puts it right is not the same.
	OnTheWay bool
}

func (e *UnsafeError) Error() string { return e.Path + " " + e.Problem }

// Test hooks, replaced only by tests.
var (
	// effectiveUID is the user the agent runs as.
	effectiveUID = os.Geteuid
	// afterLookedAt is called after a directory was looked at and before it is opened,
	// which is where an application that could write the claim root would put a link.
	afterLookedAt func(path string)
)

// Prepare makes the state directory and the Git directory, and checks that they and
// the way to the synced directory are what they should be. It makes nothing else: the
// synced directory is made by the clone, with the rules for one that is not empty.
//
// A directory that it makes is mode 0700. One that is already there is left as it
// is, and has to be a real directory, not a link, owned by the user the agent runs
// as and closed to group and others, or the call returns an *UnsafeError. A link on
// the way to the synced directory is refused too, because a link there could point the
// work tree into the state directory.
//
// The state directory and the Git directory are looked at and opened without
// following a link, and what was looked at has to be what was opened. That holds
// against an application that swaps a link in at the wrong moment, if it can write the
// claim root, which an application that mounts only the sub path cannot
// (docs/adr/0016). The way to the synced directory is looked at once, here: whoever
// opens the synced directory later, the clone, has to open it without following a link
// too. It is a check of the directories and not of what is in the Git directory: the
// config in it is looked at when Git is run.
func (l Layout) Prepare() error {
	root, err := os.OpenRoot(l.Root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := l.checkWayToWorkTree(root); err != nil {
		return atPath(l.Root, err)
	}

	state, err := l.privateDir(root, l.Root, config.StateDirName)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	git, err := l.privateDir(state, l.StateDir, GitDirName)
	if err != nil {
		return err
	}
	return git.Close()
}

// checkWayToWorkTree refuses a link or a file on the way to the synced directory. A
// part that is not there yet is fine, and so are the owner and the mode of one that
// is: the application owns that directory.
func (l Layout) checkWayToWorkTree(root *os.Root) error {
	if l.SubPath == "" {
		return nil
	}
	parts := strings.Split(l.SubPath, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if problem := notADirectory(info); problem != "" {
			return &UnsafeError{Path: filepath.Join(l.Root, filepath.FromSlash(name)), Problem: problem, OnTheWay: true}
		}
	}
	return nil
}

// workTreeMode is the mode of the synced directory and of the directories on the way to
// it that the agent makes. With the umask of the agent (0002) they are 0775, so that
// the application that shares the claim, as another user in the same group, can write
// in them (R-12).
const workTreeMode fs.FileMode = 0o777

// MakeWorkTree makes the synced directory, and the directories on the way to it, that
// are not there yet. The claim itself is there already. Each part is made before it is
// looked at, and none is followed if it is a link, so a link that appeared where a
// directory was to be made is refused as Prepare refuses one. What is there is left as
// it is: the application owns that directory, and its owner and mode are its business.
func (l Layout) MakeWorkTree() error {
	if l.SubPath == "" {
		return nil
	}
	root, err := os.OpenRoot(l.Root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	parts := strings.Split(l.SubPath, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		if err := root.Mkdir(name, workTreeMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return atPath(l.Root, err)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return atPath(l.Root, err)
		}
		if problem := notADirectory(info); problem != "" {
			return &UnsafeError{Path: filepath.Join(l.Root, filepath.FromSlash(name)), Problem: problem, OnTheWay: true}
		}
	}
	return nil
}

// privateDir makes the directory in parent if it is not there, checks it, and returns
// it opened. parentPath is where parent is, for the words of an error.
func (l Layout) privateDir(parent *os.Root, parentPath, name string) (*os.Root, error) {
	dir, err := openPrivateDir(parent, parentPath, name)
	if err != nil {
		return nil, atPath(parentPath, err)
	}
	return dir, nil
}

// openPrivateDir is privateDir with the errors of the Root as they come, which name a
// path relative to the parent.
func openPrivateDir(parent *os.Root, parentPath, name string) (*os.Root, error) {
	path := filepath.Join(parentPath, name)

	// Make it first and look at it after, whoever made it. Making something that is there,
	// a link included, fails without following it, so there is no look before the make
	// for anyone to get in between. On a read-only volume a directory that is there still
	// reports that it is.
	if err := parent.Mkdir(name, privateMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if problem := notAPrivateDirectory(info); problem != "" {
		return nil, &UnsafeError{Path: path, Problem: problem}
	}

	if afterLookedAt != nil {
		afterLookedAt(path)
	}

	dir, err := parent.OpenRoot(name)
	if err != nil {
		// A link put in the place of the directory is refused by the open or followed by
		// it. Either way, say that it changed, if it did, and not that the open failed.
		if again, lookErr := parent.Lstat(name); lookErr == nil && !os.SameFile(info, again) {
			return nil, &UnsafeError{Path: path, Problem: "changed while it was being looked at"}
		}
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if !os.SameFile(info, opened) {
		_ = dir.Close()
		return nil, &UnsafeError{Path: path, Problem: "changed while it was being looked at"}
	}
	return dir, nil
}

// atPath makes the path of an error from a Root, which is relative to the directory
// that was opened, the path the agent knows it by, so that a message names the directory
// that failed and not a name that means nothing outside of the call.
func atPath(base string, err error) error {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		return err
	}
	return &fs.PathError{Op: pathErr.Op, Path: filepath.Join(base, pathErr.Path), Err: pathErr.Err}
}

// notADirectory returns what is wrong when the file is a link or not a directory, and
// an empty string when it is a directory.
func notADirectory(info fs.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "is a link, and the agent does not follow links in the claim"
	case !info.IsDir():
		return "is not a directory"
	}
	return ""
}

// notAPrivateDirectory returns what is wrong when the file is not a directory that
// belongs to the agent's user and is closed to group and others, and an empty string
// when it is one.
func notAPrivateDirectory(info fs.FileInfo) string {
	if problem := notADirectory(info); problem != "" {
		return problem
	}
	owner, known := ownerOf(info)
	if !known {
		return "has an owner that cannot be told, so it cannot be trusted"
	}
	if me := effectiveUID(); owner != me {
		return fmt.Sprintf("belongs to another user (uid %d, and the agent is uid %d), so it cannot be trusted", owner, me)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("is open to group or others (mode %04o), and the agent keeps it closed (0700)", info.Mode().Perm())
	}
	return ""
}

// ownerOf returns the user that owns the file, and false when the file info does not
// carry it.
func ownerOf(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
