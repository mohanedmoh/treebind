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

// Package checkout makes the checkout of the repository on the claim, or opens the one
// that is there (docs/adr/0017).
//
// It never writes over data it did not put there. A directory that is empty, or that
// matches the branch exactly, becomes the checkout; any other directory is refused and
// left as it is. A Git directory that is already there is opened only when it belongs to
// the repository and branch that the Treebind names.
//
// Everything goes through the Runner of internal/agent/git, so the protections of
// docs/adr/0013 apply to every command.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// DefaultFetchTimeout is how long the first fetch and the checkout may take. A large
// repository over a slow link needs far longer than the Runner's own timeout.
const DefaultFetchTimeout = 30 * time.Minute

// Outcome says what Open did.
type Outcome string

// The outcomes of a checkout that is usable. They are reasons in the sense of
// docs/adr/0004: users and tooling will match on them.
const (
	// Cloned means the branch was fetched into an empty directory and checked out.
	Cloned Outcome = "Cloned"
	// Adopted means the directory was not empty and matched the branch exactly, so it
	// became the checkout without a file being written.
	Adopted Outcome = "Adopted"
	// Reopened means a checkout from an earlier start was there and belongs to the
	// repository and branch.
	Reopened Outcome = "Reopened"
)

// The reasons of an Error. Each distinct cause has its own, and they are API like the
// outcomes.
const (
	// ReasonDirectoryNotEmpty means the directory holds files that are not the branch's,
	// and the agent does not write over data it did not put there. Nothing was touched.
	ReasonDirectoryNotEmpty = "DirectoryNotEmpty"
	// ReasonRepositoryMismatch means the Git directory that is there is of another
	// repository, or follows another branch than the one the Treebind names.
	ReasonRepositoryMismatch = "RepositoryMismatch"
	// ReasonBranchNotFound means the repository has no such branch, or no default branch
	// to follow.
	ReasonBranchNotFound = "BranchNotFound"
	// ReasonReservedPath means the branch has a file at the path where the agent keeps its
	// own state, and a checkout would write it there.
	ReasonReservedPath = "ReservedPath"
	// ReasonFetchFailed means a command that asks the repository failed: the host cannot
	// be reached, the key is refused, or the repository does not exist.
	ReasonFetchFailed = "FetchFailed"
	// ReasonCheckoutFailed means the agent could not make the checkout on the volume.
	ReasonCheckoutFailed = "CheckoutFailed"
)

// Error is a checkout that was refused or that failed. The message is for people and holds
// no credential: what Git wrote has the credentials of any URL removed (docs/adr/0013).
type Error struct {
	// Reason is one of the reasons above.
	Reason string
	// Message says what is wrong and what to do about it.
	Message string
	// Err is the error underneath, if any.
	Err error
}

// Error returns the message.
func (e *Error) Error() string { return e.Message }

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Options say what to check out and where.
type Options struct {
	// Layout is where the agent's files are. Its work tree is where the files go, and its
	// Git directory is where the history goes.
	Layout layout.Layout
	// URL is the address of the repository. It is given to every command that needs it and
	// is not stored in the config, so a change of it is picked up, and it never sits next to
	// a credential (NFR-3). A fetch still names it in the reflog line it writes, which holds
	// no credential, because the CRD does not admit one in a URL.
	URL string
	// Branch is the branch to follow, or empty for the default branch of the repository.
	Branch string
	// Git are the settings of the Runner: the git binary, ssh and the CA bundle. GitDir,
	// WorkTree and AllowGitDirInWorkTree are taken from Layout.
	Git git.Options
	// FetchTimeout is how long the first fetch and the checkout may take.
	// DefaultFetchTimeout when it is zero.
	FetchTimeout time.Duration
}

// Result is what Open made.
type Result struct {
	// Outcome says what Open did.
	Outcome Outcome
	// Branch is the branch that the checkout follows.
	Branch string
	// Commit is the commit the checkout is at, or empty when the branch has none yet: the
	// repository has no commits, and the first sync makes the first one.
	Commit string
}

// Test hook, replaced only by tests.
//
// beforeCheckout is called after the marker is written and before the first file is, which
// is where a crash would leave the directory half written.
var beforeCheckout func() error

// Open makes the checkout, or opens the one that is there, and returns what it did. A
// refusal or a failure is an *Error. Nothing is changed in the work tree unless the
// directory is empty, or has the files of an unfinished first checkout of the agent's own.
func Open(ctx context.Context, o Options) (Result, error) {
	if o.URL == "" {
		return Result{}, errors.New("checkout: the URL of the repository is not set")
	}
	if err := config.CheckBranch(o.Branch); err != nil {
		return Result{}, fmt.Errorf("checkout: the branch %q %w", o.Branch, err)
	}
	if o.FetchTimeout == 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	if err := o.Layout.MakeWorkTree(); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the synced directory cannot be made: %v", err)
	}

	options := o.Git
	options.GitDir = o.Layout.GitDir
	options.WorkTree = o.Layout.WorkTree
	options.AllowGitDirInWorkTree = o.Layout.GitDirInWorkTree
	runner, err := git.New(options)
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "Git cannot be set up: %v", err)
	}

	c := &checkout{Options: o, git: runner}
	return c.open(ctx)
}

// checkout is one call of Open.
type checkout struct {
	Options
	git *git.Runner
	// branch is the branch that is followed, once it is known.
	branch string
}

func (c *checkout) open(ctx context.Context) (Result, error) {
	remote, err := c.askRemote(ctx)
	if err != nil {
		return Result{}, err
	}
	c.branch = remote.branch
	if c.hasRepository() {
		return c.existing(ctx, remote)
	}
	return c.first(ctx, remote)
}

// existing handles a Git directory that holds a repository.
func (c *checkout) existing(ctx context.Context, remote remoteState) (Result, error) {
	// Empty when HEAD is not on a branch: the command answers with exit status 1.
	head, _, err := c.try(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the Git directory %s cannot be read: %v", c.Layout.GitDir, err)
	}
	_, born, err := c.try(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the checkout in %s cannot be read: %v", c.Layout.GitDir, err)
	}
	if born {
		if head != headsPrefix+c.branch {
			return Result{}, c.otherBranch(head)
		}
		return c.reopen(ctx, remote)
	}

	// HEAD has no commit: a first checkout that did not finish, or one that was refused.
	// The marker says whether the files in the directory are the agent's own.
	if head == headsPrefix+c.branch {
		if sha, ok := c.readMarker(); ok {
			return c.resume(ctx, sha, remote)
		}
	}
	// Only what the agent made is cleared, and a repository that has not a commit may still
	// be somebody's: it may hold a stash, notes or hooks.
	foreign, err := c.foreign(ctx)
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the checkout in %s cannot be read: %v", c.Layout.GitDir, err)
	}
	if foreign != "" {
		return Result{}, fail(ReasonRepositoryMismatch,
			"the Git directory %s holds %s, which Treebind did not make, and has no commit on the branch %s. It is left as "+
				"it is; empty it to start again", c.Layout.GitDir, foreign, c.branch)
	}
	if err := c.clearGitDir(); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the unfinished checkout in %s cannot be cleared: %v", c.Layout.GitDir, err)
	}
	return c.first(ctx, remote)
}

// otherBranch is the refusal of a checkout that follows another branch than the Treebind
// does, or none. Treebind does not switch it: it may hold changes.
func (c *checkout) otherBranch(head string) *Error {
	if head == "" {
		return fail(ReasonRepositoryMismatch,
			"the checkout in %s follows no branch, and the Treebind follows the branch %s. Treebind does not switch a "+
				"checkout that may hold changes. Empty %s to start again; commits that were not pushed are lost with it",
			c.Layout.GitDir, c.branch, c.Layout.GitDir)
	}
	followed := describeHead(head)
	return fail(ReasonRepositoryMismatch,
		"the checkout in %s follows the branch %s, and the Treebind follows the branch %s. Treebind does not switch a "+
			"checkout that may hold changes. Set repository.branch to %s to keep following it, or empty %s to start again "+
			"on %s; commits that were not pushed are lost with it",
		c.Layout.GitDir, followed, c.branch, followed, c.Layout.GitDir, c.branch)
}

// agentMade are the entries that the agent's own init, fetch and checkout leave at the top
// of a Git directory. A directory that has others was made by somebody else: a default
// template adds hooks, info and a description.
var agentMade = map[string]bool{
	"HEAD": true, "config": true, "objects": true, "refs": true, "logs": true, "index": true, "index.lock": true,
}

// foreign returns the name of something in the Git directory that the agent did not make, or
// an empty string when it all looks like its own: only the entries it makes, and no ref but
// the ones it fetches.
func (c *checkout) foreign(ctx context.Context) (string, error) {
	entries, err := os.ReadDir(c.Layout.GitDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !agentMade[entry.Name()] {
			return entry.Name(), nil
		}
	}
	out, err := c.git.Run(ctx, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return "", err
	}
	for ref := range strings.Lines(string(out.Stdout)) {
		if ref = strings.TrimSpace(ref); !strings.HasPrefix(ref, remotePrefix) {
			return ref, nil
		}
	}
	return "", nil
}

// first makes the checkout where there is none: the Git directory is empty.
func (c *checkout) first(ctx context.Context, remote remoteState) (Result, error) {
	empty, err := c.workTreeEmpty()
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the synced directory %s cannot be read: %v", c.Layout.WorkTree, err)
	}
	if _, err := c.git.Run(ctx, "init", "--quiet", "--template=", "--initial-branch="+c.branch); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the repository cannot be made in %s: %v", c.Layout.GitDir, err)
	}

	outcome := Cloned
	if !empty {
		outcome = Adopted
	}
	if remote.tip == "" {
		// The repository has no commits, so nothing can be overwritten, and the first sync
		// commits whatever is in the directory.
		return c.result(ctx, outcome), nil
	}

	tip, err := c.fetch(ctx)
	if err != nil {
		return Result{}, err
	}
	if err := c.checkReserved(ctx, tip); err != nil {
		return Result{}, c.refuse(err)
	}
	if empty {
		return c.populate(ctx, tip)
	}
	return c.adopt(ctx, tip)
}

// populate writes the files of the commit into the directory and makes the branch. The
// marker is written first and removed last, and the branch is made by the checkout
// itself, after the files, so a branch with a commit means a checkout that finished.
func (c *checkout) populate(ctx context.Context, tip string) (Result, error) {
	if err := c.writeMarker(tip); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the state directory %s cannot be written to: %v", c.Layout.StateDir, err)
	}
	if beforeCheckout != nil {
		if err := beforeCheckout(); err != nil {
			return Result{}, fail(ReasonCheckoutFailed, "%v", err)
		}
	}
	// --force: the files in the directory are the agent's own, from an unfinished attempt,
	// or there are none. -b and not -B: the branch has no commit here, and a branch that has
	// one is not to be moved.
	if _, err := c.git.RunTimeout(ctx, c.FetchTimeout, "checkout", "--quiet", "--force", "-b", c.branch, tip); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the files cannot be written to %s: %v", c.Layout.WorkTree, err)
	}
	c.removeMarker()
	return c.result(ctx, Cloned), nil
}

// adopt makes a directory that is not empty the checkout, if it matches the branch
// exactly: with the index set to the commit, no file is modified, missing or extra. An
// empty directory is not a file, holds no data and is left as it is. The files are not
// written to, and are compared as they are, with no filter and no conversion
// (docs/adr/0013).
func (c *checkout) adopt(ctx context.Context, tip string) (Result, error) {
	if _, err := c.git.RunTimeout(ctx, c.FetchTimeout, "read-tree", tip); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the files in %s cannot be compared: %v", c.Layout.WorkTree, err)
	}
	// --modified reports the files that are deleted too.
	args := []string{"ls-files", "-z", "--modified", "--others"}
	if c.Layout.GitDirInWorkTree {
		// The state directory is the agent's, not part of what is compared.
		args = append(args, "--exclude=/"+config.StateDirName)
	}
	out, err := c.git.RunTimeout(ctx, c.FetchTimeout, args...)
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the files in %s cannot be compared: %v", c.Layout.WorkTree, err)
	}
	if differing := splitZ(string(out.Stdout)); len(differing) > 0 {
		return Result{}, c.refuse(fail(ReasonDirectoryNotEmpty,
			"%s is not empty and does not match the branch %s: %s. Treebind does not write over data it did not put there. "+
				"Empty the directory, or make it hold exactly the files of the branch",
			c.Layout.WorkTree, c.branch, describeFiles(differing)))
	}
	if _, err := c.git.Run(ctx, "update-ref", headsPrefix+c.branch, tip); err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the branch %s cannot be made: %v", c.branch, err)
	}
	return c.result(ctx, Adopted), nil
}

// reopen opens a checkout from an earlier start, after checking that it shares history
// with the branch of the repository that the Treebind names. That tells a Git directory
// of the same repository, whatever address it is reached at, from one of another, which
// a sync that follows the remote could otherwise overwrite local work with.
func (c *checkout) reopen(ctx context.Context, remote remoteState) (Result, error) {
	c.removeMarker()
	if remote.tip == "" {
		// The repository has no commits: there is nothing to compare with.
		return c.result(ctx, Reopened), nil
	}
	if _, err := c.fetch(ctx); err != nil {
		return Result{}, err
	}
	_, related, err := c.try(ctx, "merge-base", headsPrefix+c.branch, remotePrefix+c.branch)
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the checkout in %s cannot be compared with the repository: %v", c.Layout.GitDir, err)
	}
	if !related {
		return Result{}, fail(ReasonRepositoryMismatch,
			"the checkout in %s has no history in common with the branch %s of the repository. It is of another repository, "+
				"and Treebind does not sync it with this one. Empty %s to start again, or set the URL back",
			c.Layout.GitDir, c.branch, c.Layout.GitDir)
	}
	return c.result(ctx, Reopened), nil
}

// resume finishes a first checkout that stopped half way, which the marker says the agent
// started in a directory that was empty.
func (c *checkout) resume(ctx context.Context, sha string, remote remoteState) (Result, error) {
	_, present, err := c.try(ctx, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return Result{}, fail(ReasonCheckoutFailed, "the checkout in %s cannot be read: %v", c.Layout.GitDir, err)
	}
	if !present {
		// The commit the marker names is gone, for example because the objects were lost. The
		// files are still the agent's own, so check out what the branch is at now.
		if remote.tip == "" {
			return Result{}, fail(ReasonCheckoutFailed,
				"the unfinished checkout in %s names a commit that is gone, and the repository has none to put in its place",
				c.Layout.GitDir)
		}
		tip, err := c.fetch(ctx)
		if err != nil {
			return Result{}, err
		}
		if err := c.checkReserved(ctx, tip); err != nil {
			return Result{}, c.refuse(err)
		}
		sha = tip
	}
	// A crash leaves the lock of the index. Only one agent writes here (NFR-1).
	if err := os.Remove(filepath.Join(c.Layout.GitDir, "index.lock")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Result{}, fail(ReasonCheckoutFailed, "the lock of the unfinished checkout cannot be removed: %v", err)
	}
	return c.populate(ctx, sha)
}

// refuse takes back what a checkout that was refused made, so that the next start looks at
// the directory afresh and the refusal does not hold a full history. It returns the
// refusal, with a word about it if taking it back failed.
func (c *checkout) refuse(refusal *Error) *Error {
	err := c.clearGitDir()
	if err == nil {
		return refusal
	}
	refused := *refusal
	refused.Message += fmt.Sprintf(" (what was fetched for it also cannot be removed from %s: %v)", c.Layout.GitDir, err)
	refused.Err = err
	return &refused
}

// result describes the checkout as it is.
func (c *checkout) result(ctx context.Context, outcome Outcome) Result {
	commit, _, _ := c.try(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	return Result{Outcome: outcome, Branch: c.branch, Commit: commit}
}

// fail returns an *Error. The last of the arguments that is an error is the one underneath.
func fail(reason, format string, args ...any) *Error {
	e := &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			e.Err = err
		}
	}
	return e
}

// describeHead says what a checkout follows, for a message.
func describeHead(head string) string {
	if head == "" {
		return "no branch"
	}
	return strings.TrimPrefix(head, headsPrefix)
}

// maxNamed is how many files a message names.
const maxNamed = 3

// describeFiles says how many files differ and names a few.
func describeFiles(files []string) string {
	named := files
	if len(named) > maxNamed {
		named = named[:maxNamed]
	}
	return fmt.Sprintf("%d file(s) differ, for example %s", len(files), strings.Join(named, ", "))
}

// splitZ splits the output of a command that ends each name with a NUL.
func splitZ(s string) []string {
	var names []string
	for name := range strings.SplitSeq(s, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
