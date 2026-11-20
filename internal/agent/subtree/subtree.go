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

// Package subtree lets a checkout sync one directory of the repository as the root of the synced directory
// (docs/adr/0036): spec.repository.path.
//
// Git has no way to write the files of a directory of a commit at the root of a work tree and keep the commit that is checked
// out. What the agent checks out instead is a snapshot: a commit whose tree is the directory of a commit of the repository,
// made with commit-tree. The snapshot branch is the one that the checkout, the pull, the scan, the commit and the filter work
// on, as they are: the files of the volume are the tree of the snapshot, and every rule that is about the files of the
// branch is about the files of the directory.
//
// The repository's own branch is fetched as it always was, into the ref RealRef, and a snapshot of its directory is made when
// the directory differs from the one of the snapshot that TrackingRef holds. A commit that the agent makes on the snapshot
// branch is made again as a commit of the repository, on the commit it is of, with the directory replaced and every other
// file as the repository has it: a commit of the agent cannot change a file outside the directory.
//
// Every command goes through the Runner of internal/agent/git, so the protections of docs/adr/0013 and the look at the Git
// directory of docs/adr/0019 apply to every command.
package subtree

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// The reasons of an Error. They are API in the sense of docs/adr/0004: users and tooling will match on them.
const (
	// ReasonPathNotFound means the branch of the repository has no directory at repository.path, or has something there that is
	// not one. Nothing was written.
	ReasonPathNotFound = "PathNotFound"
	// ReasonPathMismatch means the checkout was made for another directory than the one the Treebind names now, or for the whole
	// repository, and a checkout is not changed to the other: it holds files that the other would write over or remove. Nothing
	// was written (FR-18).
	ReasonPathMismatch = "PathMismatch"
)

// Error is a refusal that has a reason of its own. The message is for people.
type Error struct {
	// Reason is one of the reasons above.
	Reason string
	// Message says what is wrong and what to do about it.
	Message string
}

// Error returns the message.
func (e *Error) Error() string { return e.Message }

// docsURL is where a person finds what repository.path does and how to put right a checkout that was made for another one.
const docsURL = "https://github.com/mohanedmoh/treebind/blob/main/docs/repository-path.md"

// Mismatch is the refusal of a checkout that was made for the directory made of the repository, which is empty for the whole
// repository, when the Treebind now names the directory want, which is empty too for the whole repository. Both ways out are in
// the message, and the first is the one that changes nothing.
func Mismatch(l layout.Layout, made, want string) *Error {
	describe := func(dir string) string {
		if dir == "" {
			return "the whole repository"
		}
		return fmt.Sprintf("the directory %q of the repository", dir)
	}
	keep := "remove repository.path to go on with the whole repository"
	if made != "" {
		keep = fmt.Sprintf("set repository.path to %q to go on with that directory", made)
	}
	return &Error{
		Reason: ReasonPathMismatch,
		Message: fmt.Sprintf("the checkout in %s was made for %s, and the Treebind now syncs %s. A checkout is not changed to the other: "+
			"it holds files that the other would write over or remove. Nothing was changed. To keep what is there, %s. To start "+
			"again with %s, empty the synced directory %s and the Git directory %s, which loses the commits that were not pushed; "+
			"see %s", l.GitDir, describe(made), describe(want), keep, describe(want), l.WorkTree, l.GitDir, docsURL),
	}
}

// SnapshotIdentity is who makes the snapshots. A snapshot never leaves the checkout, and a Runner cannot make a commit with no
// identity, which is what the pull of a Treebind that only pulls has. It is not the identity of the commits that the agent makes
// for the application's changes, which a Treebind chooses and which the pull checks before a resolution (docs/adr/0034).
var SnapshotIdentity = git.Identity{Name: "treebind-snapshot", Email: "treebind-snapshot@treebind.invalid"}

// Options say which directory of which branch, and where the checkout is.
type Options struct {
	// Layout is where the checkout is.
	Layout layout.Layout
	// Git are the settings of the Runner: the git binary, ssh and the CA bundle. The directories and the look at the Git
	// directory are taken from Layout, and the identity is not: the snapshots have their own, and Author is for the rest.
	Git git.Options
	// Path is the directory of the repository, as repository.path says it, and is not empty.
	Path string
	// Branch is the branch that the checkout follows.
	Branch string
	// Author is the author and the committer of the commits that Translate makes of the commits of the agent: the identity of
	// spec.commit, which the application's commits have. It may be empty for a Treebind that never commits.
	Author git.Identity
}

// Projector makes the snapshots of one directory of one branch. It is not safe for concurrent use: the agent does one thing at a
// time in a checkout.
type Projector struct {
	o   Options
	git *git.Runner
	// author makes the commits of the repository, with the author of the agent's commits, and is nil when there is none.
	author   *git.Runner
	objects  objects
	segments []string
}

// New returns a Projector. It refuses what cannot work: a path that the CRD would refuse, or none, and no branch.
func New(o Options) (*Projector, error) {
	if o.Path == "" {
		return nil, errors.New("subtree: the path is not set")
	}
	if err := config.CheckRepoPath(o.Path); err != nil {
		return nil, fmt.Errorf("subtree: the path %q %w", o.Path, err)
	}
	if o.Branch == "" {
		return nil, errors.New("subtree: the branch is not set")
	}
	options := o.Git
	options.Identity = SnapshotIdentity
	runner, err := git.New(o.Layout.RunnerOptions(options))
	if err != nil {
		return nil, fmt.Errorf("subtree: Git cannot be set up: %w", err)
	}
	p := &Projector{
		o: o, git: runner, segments: strings.Split(o.Path, "/"),
		objects: objects{git: runner, scratch: o.Layout.StateDir},
	}
	if o.Author != (git.Identity{}) {
		options.Identity = o.Author
		if p.author, err = git.New(o.Layout.RunnerOptions(options)); err != nil {
			return nil, fmt.Errorf("subtree: Git cannot be set up: %w", err)
		}
	}
	return p, nil
}

// Projection is what Project found and made.
type Projection struct {
	// Snapshot is the commit that TrackingRef holds now: the snapshot of the directory at Real.
	Snapshot string
	// Real is the tip of the branch of the repository that Snapshot is the directory of, as it was fetched.
	Real string
	// Made is true when a snapshot was made, and false when the one that was there is of the directory as it is.
	Made bool
}

// Project makes the snapshot of the directory at the tip that was fetched into FetchRef, when the snapshot at TrackingRef is not
// of it, and moves TrackingRef to it, and then RealRef to the tip. A change of the repository outside the directory makes no
// snapshot and moves RealRef only. A projection that fails moves nothing: RealRef is never ahead of the snapshot, so the pull, which
// fetches only when the repository's tip is not the one that RealRef holds, finds the update again.
func (p *Projector) Project(ctx context.Context) (Projection, error) {
	real, err := p.commit(ctx, FetchRef(p.o.Branch))
	if err != nil {
		return Projection{}, err
	}
	if real == "" {
		return Projection{}, fmt.Errorf("subtree: the branch %s of the repository has not been fetched", p.o.Branch)
	}
	held, err := p.held(ctx)
	if err != nil {
		return Projection{}, err
	}
	tree, found, err := p.directory(ctx, real)
	if err != nil {
		return Projection{}, err
	}
	if held.commit == "" && !found {
		return Projection{}, &Error{Reason: ReasonPathNotFound, Message: fmt.Sprintf("the branch %s of the repository has no "+
			"directory %q at %s. Check repository.path. A directory that is to come is made in the repository first, so that a "+
			"mistake in the name does not make the agent commit the application's files into a new directory of it; see %s",
			p.o.Branch, p.o.Path, short(real), docsURL)}
	}
	if held.commit != "" {
		heldTree, err := p.run(ctx, "rev-parse", "--verify", held.commit+"^{tree}")
		if err != nil {
			return Projection{}, err
		}
		if heldTree == tree {
			return Projection{Snapshot: held.commit, Real: real}, p.holdReal(ctx, real)
		}
	}
	parent := held.commit
	if parent != "" {
		// The new snapshot continues the old one only when the repository's branch continues the commit that the old one is of.
		// A branch that was rewritten has no history in common with it, and the snapshot has none either.
		continues, err := p.isAncestor(ctx, held.source, real)
		if err != nil {
			return Projection{}, err
		}
		if !continues {
			parent = ""
		}
	}
	made, err := p.snapshot(ctx, tree, real, parent)
	if err != nil {
		return Projection{}, err
	}
	if _, err := p.git.Run(ctx, "update-ref", TrackingRef(p.o.Branch), made, held.commit); err != nil {
		return Projection{}, err
	}
	return Projection{Snapshot: made, Real: real, Made: true}, p.holdReal(ctx, real)
}

// holdReal moves RealRef to the tip that the snapshot at TrackingRef is the directory of. It is the last write of a projection, and
// is not made when the ref is there already.
func (p *Projector) holdReal(ctx context.Context, real string) error {
	held, err := p.commit(ctx, RealRef(p.o.Branch))
	if err != nil || held == real {
		return err
	}
	_, err = p.git.Run(ctx, "update-ref", RealRef(p.o.Branch), real, held)
	return err
}

// snapshot makes the commit that is the snapshot of the directory at a commit of the repository: the tree of the directory, the
// message that says where it is from, and the snapshot it continues, if there is one.
func (p *Projector) snapshot(ctx context.Context, tree, real, parent string) (string, error) {
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	return p.run(ctx, commitTree(tree, snapshotMessage(real, p.o.Path), parents)...)
}

// commitTree is the command that makes a commit of a tree, with the parents and the message as they are given.
func commitTree(tree, message string, parents []string) []string {
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	return append(args, "-m", message)
}

// heldSnapshot is the snapshot that TrackingRef holds, and what it says of itself. Both are empty when the ref is not there.
type heldSnapshot struct {
	commit string
	source string
}

// held reads the snapshot at TrackingRef, and refuses it when it is not one of this directory: a commit with no trailers is the
// tip of a checkout of the whole repository, and another path is a checkout for another directory.
func (p *Projector) held(ctx context.Context) (heldSnapshot, error) {
	commit, err := p.commit(ctx, TrackingRef(p.o.Branch))
	if err != nil || commit == "" {
		return heldSnapshot{}, err
	}
	// A commit that is not a snapshot says no directory, which is not the one of the Treebind, whose path is not empty.
	made, _, err := says(ctx, p.git, commit)
	if err != nil {
		return heldSnapshot{}, err
	}
	if made.path != p.o.Path {
		return heldSnapshot{}, Mismatch(p.o.Layout, made.path, p.o.Path)
	}
	return heldSnapshot{commit: commit, source: made.source}, nil
}

// says reads what a commit says of itself through the Runner, and whether it is a snapshot at all.
func says(ctx context.Context, runner *git.Runner, commit string) (snapshot, bool, error) {
	object, err := readCommit(ctx, runner, commit)
	if err != nil {
		return snapshot{}, false, err
	}
	read, ok := parseSnapshot(object.message)
	return read, ok, nil
}

// Source returns the commit of the repository that a snapshot of the directory is of, for a message that names it, and false when
// the commit is not one: it is believed only when the directory at that commit is its tree.
func (p *Projector) Source(ctx context.Context, commit string) (string, bool, error) {
	return p.source(ctx, commit)
}

// directory returns the tree of the directory at a commit of the repository. found is false when the commit has nothing at the
// path, and an entry there that is not a directory is a refusal.
func (p *Projector) directory(ctx context.Context, real string) (tree string, found bool, err error) {
	out, err := p.git.Run(ctx, "rev-parse", "--verify", "--quiet", real+":"+p.o.Path)
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return emptyTree, false, nil
	}
	if err != nil {
		return "", false, err
	}
	tree = strings.TrimSpace(string(out.Stdout))
	kind, err := p.run(ctx, "cat-file", "-t", tree)
	if err != nil {
		return "", false, err
	}
	if kind != "tree" {
		what := "a file or a link"
		if kind == "commit" {
			what = "a submodule"
		}
		return "", false, &Error{Reason: ReasonPathNotFound, Message: fmt.Sprintf("%q is %s at %s of the branch %s of the repository, "+
			"not a directory. Check repository.path; see %s", p.o.Path, what, short(real), p.o.Branch, docsURL)}
	}
	return tree, true, nil
}

// isAncestor reports whether a is a commit that b has in its history, or is. Git answers with exit status 1 when it is not.
func (p *Projector) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := p.git.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// run runs a subcommand and returns what it wrote, without the newline at the end.
func (p *Projector) run(ctx context.Context, args ...string) (string, error) {
	out, err := p.git.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// commit returns the commit that a ref names, or an empty string when the ref is not there: the command answers with exit
// status 1.
func (p *Projector) commit(ctx context.Context, ref string) (string, error) {
	out, err := p.git.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}
