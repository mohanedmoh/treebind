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
	"context"
	"crypto/sha1" //nolint:gosec // the id of a Git object is a SHA-1 of it, and this is not used for security
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// repair puts right what a pull that was cut off left, before the next one begins (docs/adr/0028). It reads first, and
// writes only when there is something to put right, after the fence has checked the writer lock. It never applies an
// update: it puts the checkout back as it was before the update began, and the pull that follows applies the update with
// every check it has. It returns what it did, in words for the Pull check, also when it fails.
func (p *Puller) repair(ctx context.Context, head string) (string, *Error) {
	state, err := p.indexState(ctx, head)
	if err != nil {
		return "", p.fail(ReasonPullFailed, "the index of the checkout in %s cannot be checked: %v", p.o.Layout.GitDir, err)
	}
	found, present, problem := p.readJournal()
	if state == indexClean && !present {
		return "", nil
	}
	// Both write, so the lock is checked as late as it can be, and not at all when there is nothing to put right.
	if p.o.Fence != nil {
		if err := p.o.Fence(ctx); err != nil {
			return "", p.fail(ReasonRefused, "the checkout was not put right, because the lock could not be checked: %v", err)
		}
	}
	var notes []string
	if state != indexClean {
		note, failure := p.resetIndex(ctx, head, state)
		if failure != nil {
			return "", failure
		}
		notes = append(notes, note)
	}
	if present {
		note, failure := p.recoverUpdate(ctx, head, found, problem)
		if note != "" {
			notes = append(notes, note)
		}
		if failure != nil {
			return strings.Join(notes, "; "), failure
		}
	}
	return strings.Join(notes, "; "), nil
}

// indexStatus says what is wrong with the index, if anything.
type indexStatus int

const (
	// indexClean is an index that can be read and holds what the commit that the checkout is at holds.
	indexClean indexStatus = iota
	// indexStaged is an index that holds something else: a file that is staged, which nothing here stages for long.
	indexStaged
	// indexUnreadable is an index that Git cannot read.
	indexUnreadable
)

// indexState looks at the index with one command that reads it and writes nothing. An error is only for a command that
// was stopped, which says nothing about the index.
func (p *Puller) indexState(ctx context.Context, head string) (indexStatus, error) {
	var err error
	if head == "" {
		// A checkout with no commit has no file in its index: anything there is staged.
		var out git.Result
		out, err = p.git.Run(ctx, "ls-files", "-z", "--cached")
		switch {
		case err == nil && len(out.Stdout) == 0:
			return indexClean, nil
		case err == nil, errors.Is(err, git.ErrOutputLimit):
			return indexStaged, nil
		}
	} else {
		// Exit status 1 is the answer that the index differs from the commit, and any other failure is Git not reading it.
		_, err = p.git.Run(ctx, "diff-index", "--cached", "--quiet", "HEAD")
		if err == nil {
			return indexClean, nil
		}
		if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
			return indexStaged, nil
		}
	}
	if errors.Is(err, git.ErrCanceled) || errors.Is(err, git.ErrTimeout) {
		return indexClean, err
	}
	return indexUnreadable, nil
}

// resetIndex makes the index what the commit that the checkout is at holds, which is nothing when it has none. It does
// not touch the work tree, so what the application changed stays a change of the application's.
func (p *Puller) resetIndex(ctx context.Context, head string, state indexStatus) (string, *Error) {
	args := []string{"read-tree", "HEAD"}
	what := "the commit the checkout is at (" + short(head) + ")"
	if head == "" {
		args, what = []string{"read-tree", "--empty"}, "empty, as the checkout has no commit"
	}
	if _, err := p.git.Run(ctx, args...); err != nil {
		return "", p.fail(ReasonPullFailed, "the index of the checkout in %s cannot be made again: %v", p.o.Layout.GitDir, err)
	}
	if state == indexUnreadable {
		return "the index could not be read, and was made again from " + what, nil
	}
	return "the index had files staged, which nothing here stages for long, and was reset to " + what, nil
}

// recoverUpdate deals with the journal of an update that was begun. Whatever it finds, it removes the journal, unless
// something could not be done that the next pull is to try again.
func (p *Puller) recoverUpdate(ctx context.Context, head string, j journal, problem error) (string, *Error) {
	switch {
	case problem != nil:
		note := fmt.Sprintf("a journal of an update was found that cannot be used, and was ignored: %v", problem)
		if err := p.removeJournal(); err != nil {
			note += fmt.Sprintf(" (and it cannot be removed: %v)", err)
		}
		return note, nil
	case head == j.To:
		return fmt.Sprintf("the update to %s had finished, and its journal was removed%s", short(j.To), p.journalRemoved()), nil
	case head != j.From:
		return fmt.Sprintf("the journal of the update from %s to %s was removed: the checkout is at %s, which is neither%s",
			commitName(j.From), short(j.To), commitName(head), p.journalRemoved()), nil
	}
	return p.undo(ctx, j)
}

// journalRemoved removes the journal and says so when it could not.
func (p *Puller) journalRemoved() string {
	if err := p.removeJournal(); err != nil {
		return fmt.Sprintf(" (the journal cannot be removed: %v)", err)
	}
	return ""
}

// commitName says a commit for a message, or that there is none.
func commitName(commit string) string {
	if commit == "" {
		return "no commit"
	}
	return short(commit)
}

// touch is one file that an update changes: its mode and id in the commit before and in the commit after, which are
// empty for a commit that has no such file.
type touch struct {
	path             string
	oldMode, newMode string
	oldID, newID     string
}

// What is done about a file that the update changes, once what is there is known.
type action int

const (
	leave action = iota
	putBack
	removeIt
	// keep is a file that is neither as the update nor as the commit before has it, so it is the application's.
	keep
	// unclear is a file that could not be compared.
	unclear
)

// undo puts the files that a cut-off update changed back as the commit before has them, as far as they are the
// agent's own, and removes the journal. A file is the agent's own when it is as the commit before had it, as the commit
// after has it, or the start of that, which is what a write that was cut off leaves.
func (p *Puller) undo(ctx context.Context, j journal) (string, *Error) {
	touched, err := p.touched(ctx, j)
	if err != nil {
		note := fmt.Sprintf("the files of the update from %s to %s that was cut off cannot be listed (%v), so none was put back, "+
			"and the next pull is Blocked by any file that it cannot write over", commitName(j.From), short(j.To), err)
		_ = p.removeJournal()
		return note, nil
	}
	work, err := os.OpenRoot(p.o.Layout.WorkTree)
	if err != nil {
		return "", p.fail(ReasonPullFailed, "the synced directory %s cannot be looked at: %v", p.o.Layout.WorkTree, err)
	}
	defer func() { _ = work.Close() }()

	var restore, remove, kept, unsure []string
	for _, t := range touched {
		switch p.decide(ctx, work, t) {
		case putBack:
			restore = append(restore, t.path)
		case removeIt:
			remove = append(remove, t.path)
		case keep:
			kept = append(kept, t.path)
		case unclear:
			unsure = append(unsure, t.path)
		case leave:
		}
	}
	for batch := range chunks(restore, checkoutBatch) {
		args := append([]string{"checkout-index", "-f", "-u", "--"}, batch...)
		if _, err := p.git.Run(ctx, args...); err != nil {
			return "", p.fail(ReasonPullFailed, "the files of the update that was cut off cannot be put back: %v", err)
		}
	}
	for _, path := range remove {
		if err := work.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", p.fail(ReasonPullFailed, "the file %s of the update that was cut off cannot be removed: %v", path, err)
		}
	}

	note := fmt.Sprintf("the update from %s to %s was cut off, and was undone, and the next pull applies it again: %d file(s) put back, %d removed",
		commitName(j.From), short(j.To), len(restore), len(remove))
	if len(kept) > 0 {
		note += fmt.Sprintf("; %d file(s) are left as they are, because they are neither as the update nor as the commit before has them, "+
			"and the next pull is Blocked by them: %s", len(kept), NameSome(kept))
	}
	if len(unsure) > 0 {
		note += fmt.Sprintf("; %d file(s) could not be compared and are left as they are: %s", len(unsure), NameSome(unsure))
	}
	return note + p.journalRemoved(), nil
}

// checkoutBatch is how many files one command puts back, so that the command line stays short.
const checkoutBatch = 500

// chunks splits the names into batches.
func chunks[T any](names []T, size int) func(yield func([]T) bool) {
	return func(yield func([]T) bool) {
		for len(names) > 0 {
			n := min(size, len(names))
			if !yield(names[:n]) {
				return
			}
			names = names[n:]
		}
	}
}

// NameSome names the first few of the files, and how many more there are, for a message. The agent says the files of a
// resolution that were left out with it.
func NameSome(names []string) string {
	shown := names[:min(len(names), maxNamed)]
	text := strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// touched lists the files that the update changes: for a checkout that had a commit what differs between it and the
// commit after, and for one that had none every file of the commit after.
func (p *Puller) touched(ctx context.Context, j journal) ([]touch, error) {
	if j.From == "" {
		out, err := p.git.Run(ctx, "ls-tree", "-r", "-z", j.To)
		if err != nil {
			return nil, err
		}
		var files []touch
		for entry := range strings.SplitSeq(string(out.Stdout), "\x00") {
			// <mode> SP <type> SP <id> TAB <path>
			meta, path, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 {
				continue
			}
			files = append(files, touch{path: path, newMode: fields[0], newID: fields[2]})
		}
		return files, nil
	}
	return p.rawDiff(ctx, j.From, j.To)
}

// rawDiff lists what differs between two trees, a file at a time: a rename is a delete and an add.
func (p *Puller) rawDiff(ctx context.Context, from, to string) ([]touch, error) {
	out, err := p.git.Run(ctx, "diff-tree", "-r", "--no-renames", "-z", "--raw", "--no-abbrev", from, to)
	if err != nil {
		return nil, err
	}
	var files []touch
	fields := strings.Split(string(out.Stdout), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// :<old mode> SP <new mode> SP <old id> SP <new id> SP <status>, and then the path.
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			continue
		}
		files = append(files, touch{
			path: fields[i+1], oldMode: noMode(meta[0]), newMode: noMode(meta[1]), oldID: meta[2], newID: meta[3],
		})
	}
	return files, nil
}

// noMode turns the mode that Git gives a file that is not there into an empty one.
func noMode(mode string) string {
	if mode == "000000" {
		return ""
	}
	return mode
}

const (
	modeLink    = "120000"
	modeGitlink = "160000"
)

// decide says what to do about one file that the update changes, from what is in the work tree now.
//
// A file that the update added is removed when it is the whole of the file or the start of it, and is left when it is
// anything else, which is a file of the application's. A file that the update changed is put back when it is not there,
// or is the whole of the file that the update writes or the start of it, and is left alone when it is as it was, and
// when it is anything else. A file that the update deleted is left as it is: it is gone, or it is as it was, and the next
// pull deletes it. A link is the whole of a link or nothing: it is compared by what it points to, and a link where the
// commits have a file is anything else.
func (p *Puller) decide(ctx context.Context, work *os.Root, t touch) action {
	if t.newMode == "" {
		return leave
	}
	if t.oldMode == modeGitlink || t.newMode == modeGitlink {
		return unclear
	}
	found, err := look(work, t.path)
	if err != nil {
		return unclear
	}
	if found.kind == absent {
		if t.oldMode == "" {
			return leave
		}
		return putBack
	}
	if found.kind == other {
		return keep
	}
	asNew, err := found.is(t.newMode, t.newID)
	if err != nil {
		return unclear
	}
	if asNew {
		return undoWritten(t)
	}
	if t.oldMode != "" {
		asOld, err := found.is(t.oldMode, t.oldID)
		if err != nil {
			return unclear
		}
		if asOld {
			return leave
		}
	}
	switch p.startOf(ctx, found, t) {
	case yes:
		return undoWritten(t)
	case cannotTell:
		return unclear
	case no:
	}
	return keep
}

// undoWritten is what is done about a file that the update has written, whole or in part: it is removed when the update
// added it, and put back when the update changed it.
func undoWritten(t touch) action {
	if t.oldMode == "" {
		return removeIt
	}
	return putBack
}

// kind is what is at a path in the work tree.
type kind int

const (
	absent kind = iota
	regular
	link
	other
)

// found is what the work tree has at a path.
type found struct {
	root *os.Root
	path string
	kind kind
	size int64
	// target is where a link points.
	target string
}

// look says what is at the path, following no link.
func look(root *os.Root, path string) (found, error) {
	info, err := root.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return found{kind: absent}, nil
	case err != nil:
		return found{}, err
	}
	f := found{root: root, path: path, size: info.Size()}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		f.kind = link
		f.target, err = root.Readlink(path)
		return f, err
	case info.Mode().IsRegular():
		f.kind = regular
	default:
		f.kind = other
	}
	return f, nil
}

// is reports whether what is there is the whole of the file that a commit has, with the mode and the id that
// it names. It compares the id that Git would give the content, so that a file of any size is compared with nothing
// held in memory and no command run.
func (f found) is(mode, id string) (bool, error) {
	var sum hash.Hash
	switch len(id) {
	case sha1Hex:
		sum = sha1.New() //nolint:gosec // the id of a Git object, not a protection
	case sha256Hex:
		sum = sha256.New()
	default:
		return false, errors.New("not the id of an object")
	}
	switch {
	case mode == modeLink && f.kind == link:
		_, _ = fmt.Fprintf(sum, "blob %d\x00%s", len(f.target), f.target)
	case mode != modeLink && f.kind == regular:
		file, err := f.root.Open(f.path)
		if err != nil {
			return false, err
		}
		defer func() { _ = file.Close() }()
		_, _ = fmt.Fprintf(sum, "blob %d\x00", f.size)
		if n, err := io.Copy(sum, io.LimitReader(file, f.size+1)); err != nil || n != f.size {
			// It changed while it was read, or cannot be read: it is not what is compared with.
			return false, err
		}
	default:
		return false, nil
	}
	return hex.EncodeToString(sum.Sum(nil)) == id, nil
}

const (
	sha1Hex   = 40
	sha256Hex = 64
)

// what startOf can say.
type answer int

const (
	no answer = iota
	yes
	cannotTell
)

// startOf reports whether what is there is the start of the file that the update writes, which is what a write that
// was cut off leaves, and an empty file is the start of any. A link is made whole or not at all. The file that
// the update writes has to be read to tell, and one that the Runner cannot hand over, because it is longer than what
// a command may write, cannot be told: that is cannotTell.
func (p *Puller) startOf(ctx context.Context, f found, t touch) answer {
	if f.kind != regular || t.newMode == modeLink {
		return no
	}
	out, err := p.git.Run(ctx, "cat-file", "blob", t.newID)
	if err != nil {
		return cannotTell
	}
	blob := out.Stdout
	if f.size >= int64(len(blob)) {
		return no
	}
	file, err := f.root.Open(f.path)
	if err != nil {
		return cannotTell
	}
	defer func() { _ = file.Close() }()
	start := make([]byte, f.size)
	if _, err := io.ReadFull(file, start); err != nil {
		return cannotTell
	}
	if string(blob[:f.size]) == string(start) {
		return yes
	}
	return no
}
