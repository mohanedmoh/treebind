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

// Package commit commits the changes that a scan found settled (docs/adr/0031). It is given the changes and
// makes one commit of them, by the one identity of the agent and with the message of the template, and it
// writes nothing else.
//
// The steps are few, and each is there for something that was tried against Git:
//
//   - The index is first made equal to HEAD (reset), so that whatever a run that was killed left staged, and
//     whatever the filter no longer keeps, does not ride along in the commit.
//   - The paths are staged as literal paths, from a file, because a name is not a pattern (a[1].txt must not
//     take in a1.txt), and a list of thousands of names does not fit in the arguments of a command. The Runner
//     has no standard input, so the file is in the state directory of the agent, which the scan leaves out.
//   - A path that was added and is gone by now is left out, because Git refuses the whole list for one that
//     matches nothing. What changes after the scan is for the next scan.
//   - The files that are in the commit are the ones that the index differs by from HEAD, which is how many
//     files the message says and also how nothing to commit is told: a file that was changed back is no change.
//   - The message is kept verbatim, so that it is stored as the template rendered it: the default cleanup of Git
//     collapses blank lines that come one after the other and strips the spaces at the end of a line.
//
// The writer lock is checked once, immediately before the first write. A commit that fails leaves the index as
// HEAD, so that a pull is not blocked by what it staged.
package commit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commitmsg"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

const (
	// ReasonCommitFailed is the reason of a commit that could not be made: Git said no, the writer lock could not
	// be checked, or the volume refused a write. The files are still there, and the next scan finds them again. A
	// Git directory that holds more than the agent put there has its own reason, the one of the checkout.
	ReasonCommitFailed = "CommitFailed"

	// listFile is the file in the state directory that holds the paths of a commit for Git to read.
	listFile = "commit-paths"

	// literal is the magic of a pathspec that makes the rest of it a path and not a pattern.
	literal = ":(literal)"

	// emptyTree is the tree that a commit that has no parent is compared with. The agent's repositories use SHA-1.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	// undoTimeout is how long putting the index back may take after a commit that failed, when the agent was
	// told to stop in the middle of it.
	undoTimeout = 5 * time.Second

	// countTimeout is how long telling the breaker of a commit may take, when the agent was told to stop in the middle of
	// the commit: a short time is all it needs, and the commit is in the branch whether it is counted or not.
	countTimeout = 5 * time.Second
)

// Outcome says what a call of Commit came to.
type Outcome string

const (
	// Committed means a commit was made.
	Committed Outcome = "Committed"
	// NothingToCommit means the changes that were given came to nothing: they were all gone, or were changed
	// back, so that the index equals HEAD. It is not a failure, and nothing was committed.
	NothingToCommit Outcome = "NothingToCommit"
)

// Result is what a commit did.
type Result struct {
	Outcome Outcome
	// Commit is the id of the commit that was made, and is empty when none was.
	Commit string
	// Files is how many files the commit holds.
	Files int
	// Gone is how many of the changes that were given were left out because the file was gone by then.
	Gone int
	// Warning says what did not work after a commit that did: it could not be counted by the breaker, so that the limit may be
	// exceeded by it. It is empty when there is nothing to say.
	Warning string
}

// Error is a commit that could not be made.
type Error struct {
	// Reason is a stable reason in the sense of docs/adr/0004.
	Reason string
	// Message says what is wrong, for people.
	Message string
	// Err is the error underneath, which may be nil.
	Err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Options say where to commit and who commits.
type Options struct {
	// Layout is where the checkout is.
	Layout layout.Layout
	// Git are the settings of the Runner. The directories and the look at the Git directory are taken from
	// Layout, and the identity from the author below.
	Git git.Options
	// AuthorName and AuthorEmail are the author and the committer of every commit. Both are given or neither:
	// with neither, the defaults of package commitmsg are the author.
	AuthorName, AuthorEmail string
	// MessageTemplate is the template of the message of a commit. commitmsg.DefaultTemplate when it is empty.
	MessageTemplate string
	// Fence is called immediately before the first write, and nothing is written when it returns an error. It is
	// the check that the agent still holds the writer lock (docs/adr/0023). With none, nothing is checked.
	Fence func(ctx context.Context) error
	// Gate is the breaker that bounds the commits of an hour (docs/adr/0035). It is asked before anything is written, and
	// told when a commit was made. With none, nothing is bounded.
	Gate breaker.Gate
}

// Committer commits into one checkout. It is not safe for concurrent use: the agent commits one at a time,
// in the same loop as its pulls and its scans.
type Committer struct {
	o   Options
	git *git.Runner
}

// New checks the author and the template, and makes a Committer. A wrong author or template is an error that
// names it, so that it is a failed check at the start of the agent and not a commit that fails every time.
func New(o Options) (*Committer, error) {
	if (o.AuthorName == "") != (o.AuthorEmail == "") {
		return nil, errors.New("commit: the author needs a name and an email, or neither")
	}
	if o.AuthorName == "" {
		o.AuthorName, o.AuthorEmail = commitmsg.DefaultAuthorName, commitmsg.DefaultAuthorEmail
	}
	if err := commitmsg.ValidateAuthor(o.AuthorName, o.AuthorEmail); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if o.MessageTemplate == "" {
		o.MessageTemplate = commitmsg.DefaultTemplate
	}
	if _, err := commitmsg.Render(o.MessageTemplate, commitmsg.Data{Count: 1}); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	options := o.Git
	options.Identity = git.Identity{Name: o.AuthorName, Email: o.AuthorEmail}
	runner, err := git.New(checkout.RunnerOptions(o.Layout, options))
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &Committer{o: o, git: runner}, nil
}

// Commit commits the changes, which are the paths of a scan. It makes one commit, or none when they come to
// nothing. A commit that could not be made is an *Error.
func (c *Committer) Commit(ctx context.Context, changes []scan.Change) (Result, error) {
	list, gone, err := c.pathsOf(changes)
	if err != nil {
		return Result{}, c.fail(err, "the changes cannot be committed: %v", err)
	}
	if len(list) == 0 {
		return Result{Outcome: NothingToCommit, Gone: gone}, nil
	}
	if err := c.allow(ctx); err != nil {
		return Result{}, err
	}
	if c.o.Fence != nil {
		if err := c.o.Fence(ctx); err != nil {
			return Result{}, c.fail(err, "nothing was committed, because the writer lock could not be checked: %v", err)
		}
	}
	result, err := c.write(ctx, list)
	if err != nil {
		c.undo(ctx)
		return Result{}, c.fail(err, "the changes of the directory cannot be committed: %v", err)
	}
	result.Gone = gone
	if result.Outcome == Committed {
		result.Warning = c.count(ctx)
	}
	return result, nil
}

// allow asks the breaker whether a commit may be made. A hold is the reason TooManyCommits, with the words of the breaker, and
// a count that cannot be made is a failed commit: what cannot be counted is not guessed.
func (c *Committer) allow(ctx context.Context) error {
	if c.o.Gate == nil {
		return nil
	}
	err := c.o.Gate.Allow(ctx)
	if err == nil {
		return nil
	}
	if held, ok := errors.AsType[*breaker.HeldError](err); ok {
		return &Error{Reason: breaker.ReasonTooManyCommits, Message: held.Error(), Err: err}
	}
	return c.fail(err, "nothing was committed, because the commits of the last hour cannot be counted: %v", err)
}

// count tells the breaker that a commit was made, and returns what to warn of when that did not work. The commit is in the
// branch, so a stop that came in the middle of it does not cancel this: a short time is all it needs.
func (c *Committer) count(ctx context.Context) string {
	if c.o.Gate == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), countTimeout)
	defer cancel()
	if err := c.o.Gate.Made(ctx); err != nil {
		return fmt.Sprintf("the commit was not counted against the limit of commits in an hour, so the limit may be exceeded by it: %v", err)
	}
	return ""
}

// pathsOf is the literal pathspecs of the changes, and how many were left out because the file is gone. A path
// that is not a clean path of the synced directory is refused, because it is not what a scan lists.
func (c *Committer) pathsOf(changes []scan.Change) (list []string, gone int, err error) {
	for _, change := range changes {
		p := change.Path
		if p == "" || strings.ContainsRune(p, 0) || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
			return nil, 0, fmt.Errorf("the path %q is not a path of the synced directory", p)
		}
		if change.Kind == scan.Added {
			// A file that was added and is gone has nothing in the index to match, and Git refuses the whole list
			// for it. A file that the commit has is matched by the index even when it is gone: it is a removal.
			if _, err := os.Lstat(filepath.Join(c.o.Layout.WorkTree, filepath.FromSlash(p))); errors.Is(err, fs.ErrNotExist) {
				gone++
				continue
			}
		}
		list = append(list, literal+p)
	}
	return list, gone, nil
}

// write stages the paths and commits them, and returns what it made.
func (c *Committer) write(ctx context.Context, list []string) (Result, error) {
	if _, err := c.git.Run(ctx, "reset", "--quiet"); err != nil {
		return Result{}, err
	}
	if err := c.stage(ctx, list); err != nil {
		return Result{}, err
	}
	against, err := c.parent(ctx)
	if err != nil {
		return Result{}, err
	}
	out, err := c.git.Run(ctx, "diff-index", "--cached", "--name-only", "-z", "--no-renames", against)
	if err != nil {
		return Result{}, err
	}
	files := strings.Count(string(out.Stdout), "\x00")
	if files == 0 {
		return Result{Outcome: NothingToCommit}, nil
	}
	message, err := commitmsg.Render(c.o.MessageTemplate, commitmsg.Data{Count: files})
	if err != nil {
		return Result{}, err
	}
	if _, err := c.git.Run(ctx, "commit", "--quiet", "--cleanup=verbatim", "--message="+message); err != nil {
		return Result{}, err
	}
	head, err := c.git.Run(ctx, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: Committed, Commit: strings.TrimSpace(string(head.Stdout)), Files: files}, nil
}

// stage gives Git the paths in a file of the state directory, which is removed when Git has read it.
func (c *Committer) stage(ctx context.Context, list []string) error {
	file := filepath.Join(c.o.Layout.StateDir, listFile)
	// A file that a run that was killed left is not read: it is replaced.
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the file of paths cannot be replaced: %w", err)
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the file of paths cannot be made: %w", err)
	}
	defer func() { _ = os.Remove(file) }()
	if _, err := f.WriteString(strings.Join(list, "\x00") + "\x00"); err != nil {
		_ = f.Close()
		return fmt.Errorf("the file of paths cannot be written: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("the file of paths cannot be written: %w", err)
	}
	_, err = c.git.Run(ctx, "add", "--all", "--pathspec-from-file="+file, "--pathspec-file-nul")
	return err
}

// parent is what the index is compared with: HEAD, or the empty tree when the branch has no commit yet.
func (c *Committer) parent(ctx context.Context) (string, error) {
	_, err := c.git.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err == nil {
		return "HEAD", nil
	}
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return emptyTree, nil
	}
	return "", err
}

// undo puts the index back as HEAD after a commit that failed, so that what it staged does not block a pull. It
// checks the writer lock again, and does nothing without it, and it is not given up with a stop that came in the
// middle: a short time is all it needs. A failure of it is not told, because the next commit begins with the same
// reset.
func (c *Committer) undo(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	if c.o.Fence != nil && c.o.Fence(ctx) != nil {
		return
	}
	_, _ = c.git.Run(ctx, "reset", "--quiet")
}

// fail is the error of a commit. A Git directory that holds more than the agent put there has the reason of the
// checkout and the pull.
func (c *Committer) fail(err error, format string, args ...any) *Error {
	if refusal, ok := checkout.Refusal(err); ok {
		return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal.Err}
	}
	return &Error{Reason: ReasonCommitFailed, Message: fmt.Sprintf(format, args...), Err: err}
}
