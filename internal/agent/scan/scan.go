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

// Package scan finds what the application changed in the synced directory since the commit the
// checkout is at, and keeps the files that the agent may sync (FR-3, docs/adr/0022).
//
// A scan is one git status through the Runner, whose list goes through the filter of package paths.
// It only reads: the Runner turns off the optional locks of Git (GIT_OPTIONAL_LOCKS=0), so that a
// scan does not refresh the index and write it back, which would be a write that the writer lock does
// not guard, and which a kill in the middle of it would leave a lock behind.
//
// A scan is the whole truth about the directory and the events of the file system are not: an event can
// be missed, on a network file system for another client's writes, and a scan never is. What a scan sees
// of each file is kept with it, so that the next scan can tell a file that was written again from one that
// was not.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
)

// ReasonScanFailed is the reason of a scan that could not be done. A Git directory that holds more than
// the agent put there is the reason of the checkout for that, as it is for a pull.
const ReasonScanFailed = "ScanFailed"

// Kind says what a change is, against the commit the checkout is at.
type Kind string

const (
	// Added is a file that the commit does not have and the directory does.
	Added Kind = "Added"
	// Modified is a file that both have, with other content or another mode.
	Modified Kind = "Modified"
	// Deleted is a file that the commit has and the directory does not.
	Deleted Kind = "Deleted"
	// TypeChanged is a file that both have as another kind of file: a regular file and a link.
	TypeChanged Kind = "TypeChanged"
)

// Seen is what a scan saw of a file when it looked at it. Two scans that see the same thing saw a file
// that was not written between them. The change time and the inode are in it besides the size and the
// modification time, because a program can set the modification time back, as rsync -t does, and
// cannot set the change time.
//
// Only the same volume's own times are compared with one another, between scans. A time that a file
// server reports is never compared with the agent's clock.
type Seen struct {
	// Present is false for a file that is not there.
	Present bool
	// Mode is the type and the permission bits. A link is seen as a link and is not followed.
	Mode fs.FileMode
	// Size is the length in bytes.
	Size int64
	// ModTime and ChangeTime are the modification and the change times, in nanoseconds.
	ModTime, ChangeTime int64
	// Inode is the number of the file, which a file that is replaced by another does not keep.
	Inode uint64
}

// Change is a file of the directory that differs from the commit the checkout is at, and that the filter
// keeps.
type Change struct {
	// Path is relative to the synced directory, with slashes, as Git writes it.
	Path string
	Kind Kind
	// Seen is what the scan saw of the file after Git listed it.
	Seen Seen
}

// Result is what a scan found.
type Result struct {
	// Changes are the files that the filter keeps, sorted by path.
	Changes []Change
	// Left counts the changes that the filter leaves out, by the rule that does. It is nil when there
	// are none. They are not synced and are not changes of the Treebind, and the count is for the log.
	Left map[paths.Reason]int
}

// Error is a scan that could not be done.
type Error struct {
	// Reason is a stable reason in the sense of docs/adr/0004.
	Reason string
	// Message says what is wrong and what to do about it, for people.
	Message string
	// Err is the error underneath, which may be nil.
	Err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Options say where to scan and which files count.
type Options struct {
	// Layout is where the checkout is.
	Layout layout.Layout
	// Git are the settings of the Runner. The directories and the look at the Git directory are taken
	// from Layout.
	Git git.Options
	// Filter says which files are synced. It is required.
	Filter *paths.Filter
}

// Scanner scans one checkout. It is safe for concurrent use as far as the Runner is, but a scan is one
// call of Git, and the agent makes one at a time.
type Scanner struct {
	o    Options
	git  *git.Runner
	args []string
	// lstat looks at a file. It is os.Lstat, and a test gives another to fail.
	lstat func(name string) (fs.FileInfo, error)
}

// New returns a Scanner. It refuses what cannot work: no filter.
func New(o Options) (*Scanner, error) {
	if o.Filter == nil {
		return nil, errors.New("scan: there is no filter to say which files are synced")
	}
	runner, err := git.New(checkout.RunnerOptions(o.Layout, o.Git))
	if err != nil {
		return nil, fmt.Errorf("scan: Git cannot be set up: %w", err)
	}
	return &Scanner{o: o, git: runner, args: arguments(o.Layout), lstat: os.Lstat}, nil
}

// statusArgs ask for every file that differs from the commit, one by one: --untracked-files=all lists the
// files of a directory that is new and not the directory, and --no-renames lists a file that moved as one
// removed and one added, which is what a commit of the files by name stages.
var statusArgs = []string{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames"}

// arguments returns the arguments of git for a scan of the layout. When the state directory is in the
// work tree, which it is when the whole claim is synced, Git would list every file of the Git directory
// as an untracked file at every scan, thousands of lines that the filter then leaves out, so the
// directory is excluded from the walk.
func arguments(l layout.Layout) []string {
	args := slices.Clone(statusArgs)
	if state := l.NeverSynced(); state != "" {
		args = append(args, "--", ":(exclude,top,literal)"+state)
	}
	return args
}

// modeAbsent is the mode that status gives for a file that is not there.
const modeAbsent = "000000"

// Scan lists the changes of the directory. A scan that could not be done is an *Error.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	out, err := s.git.Run(ctx, s.args...)
	if err != nil {
		return Result{}, s.fail("the changes of the directory cannot be listed: %v", err)
	}
	var (
		result   Result
		unmerged []string
	)
	for entry := range strings.SplitSeq(string(out.Stdout), "\x00") {
		path, kind, ok, merged := parseEntry(entry)
		switch {
		case merged:
			unmerged = append(unmerged, path)
			continue
		case !ok:
			continue
		}
		if reason := s.o.Filter.Reason(path); reason != paths.Synced {
			if result.Left == nil {
				result.Left = map[paths.Reason]int{}
			}
			result.Left[reason]++
			continue
		}
		seen, err := s.observe(path)
		if err != nil {
			return Result{}, s.fail("%s cannot be looked at: %v", path, err)
		}
		result.Changes = append(result.Changes, Change{Path: path, Kind: kind, Seen: seen})
	}
	if len(unmerged) > 0 {
		return Result{}, s.fail("%d paths are unmerged, which a merge that was not finished leaves: %s. The agent does not "+
			"guess which side is meant; finish or abort the merge in the Git directory of the checkout", len(unmerged), named(unmerged))
	}
	slices.SortFunc(result.Changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

// parseEntry reads one entry of git status --porcelain=v2 -z. ok is false for an entry that is not a change
// of a file: a header, an ignored file, or a file that is in neither the commit nor the directory, which is
// what is left of one that was staged and then removed. merged is true for an unmerged path.
func parseEntry(entry string) (path string, kind Kind, ok, merged bool) {
	switch {
	case strings.HasPrefix(entry, "? "):
		return entry[2:], Added, true, false
	case strings.HasPrefix(entry, "u "):
		// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
		if fields := strings.SplitN(entry, " ", 11); len(fields) == 11 {
			return fields[10], "", false, true
		}
	case strings.HasPrefix(entry, "1 "):
		// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>. The path is the rest, and can hold spaces.
		if fields := strings.SplitN(entry, " ", 9); len(fields) == 9 {
			kind, ok = kindOf(fields[3], fields[5])
			return fields[8], kind, ok, false
		}
	}
	return "", "", false, false
}

// kindOf says what a change is from the mode of the file in the commit and in the directory.
func kindOf(inCommit, inDirectory string) (Kind, bool) {
	switch {
	case inCommit == modeAbsent && inDirectory == modeAbsent:
		return "", false
	case inCommit == modeAbsent:
		return Added, true
	case inDirectory == modeAbsent:
		return Deleted, true
	case inCommit[:3] != inDirectory[:3]:
		// The first three digits of the octal mode are the type: 100 a file, 120 a link, 160 a repository.
		return TypeChanged, true
	}
	return Modified, true
}

// observe looks at a file as Git listed it, without following a link. A file that is not there, or whose
// directory is not, is seen absent: it was removed between the listing and the look.
func (s *Scanner) observe(path string) (Seen, error) {
	info, err := s.lstat(filepath.Join(s.o.Layout.WorkTree, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return Seen{}, nil
	}
	if err != nil {
		return Seen{}, err
	}
	seen := Seen{Present: true, Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime().UnixNano()}
	seen.ChangeTime, seen.Inode = identityOf(info)
	return seen, nil
}

// maxNamed is how many paths a message names.
const maxNamed = 3

// named lists the first paths, for a message.
func named(list []string) string {
	shown := list[:min(len(list), maxNamed)]
	text := strings.Join(shown, ", ")
	if len(list) > len(shown) {
		text += fmt.Sprintf(" and %d more", len(list)-len(shown))
	}
	return text
}

// fail returns an *Error. The last of the arguments that is an error is the one underneath. A refusal to
// run Git against a Git directory that holds more than the agent put there is the error of the checkout for
// that, because what to do about it is the same.
func (s *Scanner) fail(format string, args ...any) *Error {
	e := &Error{Reason: ReasonScanFailed, Message: fmt.Sprintf(format, args...)}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			e.Err = err
		}
	}
	if refusal, ok := checkout.Refusal(e.Err); ok {
		return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal.Err}
	}
	if errors.Is(e.Err, git.ErrOutputLimit) {
		e.Message = "the directory has so many changes that Treebind cannot list them at once"
	}
	return e
}
