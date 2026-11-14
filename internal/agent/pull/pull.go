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

// Package pull brings the commits that the repository has gained into the checkout (docs/adr/0020).
//
// A pull asks the repository for the tip of the branch first, and fetches only when it is not the
// commit that the checkout already knows, which is the cheap check that keeps many Treebinds from
// hammering one Git host (R-19). It then applies the commits as a fast-forward, with Git's own
// refusal to write over a file that has a local edit or an untracked file in the way, which is
// what FR-2 asks for: nothing the application has changed and not yet committed is overwritten.
// Commits on both sides are not merged here: that is the conflict policy of the Treebind.
//
// Everything goes through the Runner of internal/agent/git, so the protections of docs/adr/0013
// and the look at the Git directory of docs/adr/0019 apply to every command.
package pull

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// DefaultFetchTimeout is how long a fetch may take. It is the one of the first checkout: a
// repository that grew a lot since the last pull needs more than the Runner's own timeout.
const DefaultFetchTimeout = checkout.DefaultFetchTimeout

// Outcome says what a pull that worked did. The outcomes and the reasons below are API in the
// sense of docs/adr/0004: users and tooling will match on them.
type Outcome string

const (
	// UpToDate means the checkout is at the tip of the branch of the repository.
	UpToDate Outcome = "UpToDate"
	// Ahead means the checkout has commits that the repository does not have yet, and the
	// repository has nothing the checkout lacks. There is nothing to pull.
	Ahead Outcome = "Ahead"
	// Advanced means the checkout was fast-forwarded to a newer tip.
	Advanced Outcome = "Advanced"
)

// The reasons of an Error. Each distinct cause has its own.
const (
	// ReasonBlocked means the update changes a file that has a local edit, or that an untracked
	// file is in the way of. Nothing was written, and the next pull tries again.
	ReasonBlocked = "Blocked"
	// ReasonDiverged means the checkout and the repository each have commits that the other
	// lacks, so the update is not a fast-forward. Nothing was written. What wins is the conflict
	// policy of the Treebind.
	ReasonDiverged = "Diverged"
	// ReasonBranchGone means the repository has no such branch any more.
	ReasonBranchGone = "BranchGone"
	// ReasonFetchFailed means a command that asks the repository failed: the host cannot be
	// reached, the key is refused, or the repository does not exist.
	ReasonFetchFailed = "FetchFailed"
	// ReasonReservedPath means the update brings a file named like the state directory into a
	// work tree that holds it. It is the reason of the checkout for the same cause.
	ReasonReservedPath = checkout.ReasonReservedPath
	// ReasonGitDirUnsafe means the Git directory holds something the agent did not put there.
	// It is the reason of the checkout and of the preflight check for the same cause.
	ReasonGitDirUnsafe = checkout.ReasonGitDirUnsafe
	// ReasonRefused means the fence or the guard of the caller stopped the update. The error of
	// the caller is underneath: a lost lock is lock.ErrLost.
	ReasonRefused = "Refused"
	// ReasonPullFailed means the agent could not pull for another reason: Git failed on the
	// volume, or the checkout is not what a pull expects.
	ReasonPullFailed = "PullFailed"
)

// Error is a pull that could not be done. The message is for people and holds no credential:
// what Git wrote has the credentials of any URL removed (docs/adr/0013).
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

// Options say what to pull and where into.
type Options struct {
	// Layout is where the checkout is.
	Layout layout.Layout
	// URL is the address of the repository, given to every command that needs it and never stored.
	URL string
	// Branch is the branch the checkout follows: Result.Branch of the checkout.
	Branch string
	// Git are the settings of the Runner: the git binary, ssh and the CA bundle. The directories
	// and the look at the Git directory are taken from Layout.
	Git git.Options
	// FetchTimeout is how long a fetch may take. DefaultFetchTimeout when it is zero.
	FetchTimeout time.Duration

	// Fence is called immediately before an update is applied, and the update is not applied when
	// it returns an error. It is where the writer lock is checked (docs/adr/0012). It is not called
	// when there is nothing to apply.
	Fence func(ctx context.Context) error
	// Guard is called with the commit the checkout is at, empty when it has none, and the commit it
	// would move to, before the fence, and the update is not applied when it returns an error. It
	// is where a cap on deletions and a recovery ref go.
	Guard func(ctx context.Context, from, to string) error
}

// Result is what a pull that worked did.
type Result struct {
	// Outcome says what the pull did.
	Outcome Outcome
	// Branch is the branch the checkout follows.
	Branch string
	// From and To are the commits the checkout moved between when it was advanced, and From is
	// empty when the checkout had no commit. Otherwise both are the commit it is at.
	From, To string
}

// Puller pulls into one checkout. It is not safe for concurrent use: one pull runs at a time.
type Puller struct {
	// o is what New was given. It is not exported, because the Runner is made from it once and a
	// change of it afterwards would not reach the Runner.
	o   Options
	git *git.Runner
}

const (
	headsPrefix  = "refs/heads/"
	remotePrefix = "refs/remotes/origin/"
	// maxNamed is how many files a message names.
	maxNamed = 3
)

// New returns a Puller. It refuses what cannot work: no address, no branch, or a branch that the
// CRD would refuse.
func New(o Options) (*Puller, error) {
	if o.URL == "" {
		return nil, errors.New("pull: the URL of the repository is not set")
	}
	if o.Branch == "" {
		return nil, errors.New("pull: the branch is not set")
	}
	if err := config.CheckBranch(o.Branch); err != nil {
		return nil, fmt.Errorf("pull: the branch %q %w", o.Branch, err)
	}
	if o.FetchTimeout == 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	runner, err := git.New(checkout.RunnerOptions(o.Layout, o.Git))
	if err != nil {
		return nil, fmt.Errorf("pull: Git cannot be set up: %w", err)
	}
	return &Puller{o: o, git: runner}, nil
}

// Pull asks the repository for the tip of the branch, fetches it if it is new, and fast-forwards
// the checkout to it if it can. A pull that could not be done is an *Error.
func (p *Puller) Pull(ctx context.Context) (Result, error) {
	// A Git directory that holds something the agent did not put there is not run Git against.
	// The Runner looks before every command as well; this says it before the repository is asked.
	if err := git.VerifyGitDir(p.o.Layout.GitDir, p.o.Layout.WorkTree); err != nil {
		return Result{}, p.fail(ReasonPullFailed, "the Git directory cannot be looked at: %v", err)
	}
	head, err := p.head(ctx)
	if err != nil {
		return Result{}, err
	}

	remote, err := p.remoteTip(ctx)
	if err != nil {
		return Result{}, err
	}
	known, err := p.try(ctx, "rev-parse", "--verify", "--quiet", remotePrefix+p.o.Branch+"^{commit}")
	if err != nil {
		return Result{}, p.fail(ReasonPullFailed, "the checkout in %s cannot be read: %v", p.o.Layout.GitDir, err)
	}
	tip := known
	if remote != known {
		if tip, err = p.fetch(ctx); err != nil {
			return Result{}, err
		}
	}

	if head == tip {
		return Result{Outcome: UpToDate, Branch: p.o.Branch, From: head, To: tip}, nil
	}
	if head != "" {
		ours, theirs, err := p.sides(ctx, head, tip)
		if err != nil {
			return Result{}, err
		}
		switch {
		case ours > 0 && theirs == 0:
			return Result{Outcome: Ahead, Branch: p.o.Branch, From: head, To: head}, nil
		case ours > 0:
			return Result{}, p.fail(ReasonDiverged, "the checkout and the branch %s of the repository have gone different ways: "+
				"the checkout has %d commit(s) that the repository does not, and the repository has %d that the checkout does not. "+
				"Nothing was written. What wins is the conflict policy of the Treebind", p.o.Branch, ours, theirs)
		}
	}
	return p.fastForward(ctx, head, tip)
}

// head returns the commit the checkout is at, or an empty string when its branch has none yet, and
// refuses a checkout that is not on the branch.
func (p *Puller) head(ctx context.Context) (string, error) {
	// Empty when HEAD is not on a branch: the command answers with exit status 1.
	branch, err := p.try(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", p.fail(ReasonPullFailed, "the checkout in %s cannot be read: %v", p.o.Layout.GitDir, err)
	}
	if branch != headsPrefix+p.o.Branch {
		return "", p.fail(ReasonPullFailed, "the checkout in %s follows %s and not the branch %s, and a pull does not switch it",
			p.o.Layout.GitDir, describe(branch), p.o.Branch)
	}
	head, err := p.try(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", p.fail(ReasonPullFailed, "the checkout in %s cannot be read: %v", p.o.Layout.GitDir, err)
	}
	return head, nil
}

// remoteTip asks the repository for the commit its branch is at.
func (p *Puller) remoteTip(ctx context.Context) (string, error) {
	out, err := p.git.Run(ctx, "ls-remote", "--", p.o.URL, headsPrefix+p.o.Branch)
	if err != nil {
		return "", p.fail(ReasonFetchFailed, "the repository cannot be asked for the branch %s: %v", p.o.Branch, err)
	}
	for line := range strings.Lines(string(out.Stdout)) {
		if commit, name, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t"); ok && name == headsPrefix+p.o.Branch {
			return commit, nil
		}
	}
	return "", p.fail(ReasonBranchGone, "the repository has no branch %s any more. Set repository.branch to a branch it has, "+
		"or put the branch back", p.o.Branch)
}

// fetch fetches the branch into refs/remotes/origin, as the checkout does, and returns the commit
// it is at. The URL is on the command line, and the tags and FETCH_HEAD, which names it, are left out.
func (p *Puller) fetch(ctx context.Context) (string, error) {
	refspec := "+" + headsPrefix + p.o.Branch + ":" + remotePrefix + p.o.Branch
	if _, err := p.git.RunTimeout(ctx, p.o.FetchTimeout, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--", p.o.URL, refspec); err != nil {
		return "", p.fail(ReasonFetchFailed, "the branch %s cannot be fetched: %v", p.o.Branch, err)
	}
	out, err := p.git.Run(ctx, "rev-parse", "--verify", remotePrefix+p.o.Branch+"^{commit}")
	if err != nil {
		return "", p.fail(ReasonFetchFailed, "the branch %s was fetched but cannot be read: %v", p.o.Branch, err)
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// fastForward applies the commits up to tip to the checkout, which is at from, or has no commit
// when from is empty.
func (p *Puller) fastForward(ctx context.Context, from, to string) (Result, error) {
	if refused := checkout.CheckReserved(ctx, p.git, p.o.Layout, p.o.Branch, to); refused != nil {
		if refused.Reason == ReasonReservedPath {
			return Result{}, &Error{Reason: refused.Reason, Message: refused.Message, Err: refused.Err}
		}
		// A failure to list the files of the update. The reason of the checkout for it is not one of
		// a pull's, and a refusal of the Git directory keeps its own through fail.
		return Result{}, p.fail(ReasonPullFailed, "the files of the update cannot be listed: %v", refused.Err)
	}
	if p.o.Guard != nil {
		if err := p.o.Guard(ctx, from, to); err != nil {
			return Result{}, p.fail(ReasonRefused, "the update was not applied: %v", err)
		}
	}
	// Immediately before the write, so that the lock is checked as late as it can be.
	if p.o.Fence != nil {
		if err := p.o.Fence(ctx); err != nil {
			return Result{}, p.fail(ReasonRefused, "the update was not applied: %v", err)
		}
	}

	// --ff-only: Git applies the update, or writes nothing. It refuses when a file the update
	// changes has a local edit, and when an untracked file is in the way, and leaves every other
	// local edit as it is.
	_, err := p.git.RunTimeout(ctx, p.o.FetchTimeout, "merge", "--quiet", "--ff-only", to)
	if err != nil {
		return Result{}, p.mergeFailed(ctx, from, to, err)
	}
	return Result{Outcome: Advanced, Branch: p.o.Branch, From: from, To: to}, nil
}

// mergeFailed turns the failure of the fast-forward into its reason, and names the files that are
// in the way when Git refused for them.
func (p *Puller) mergeFailed(ctx context.Context, from, to string, err error) error {
	if gitErr, ok := errors.AsType[*git.Error](err); ok && strings.Contains(gitErr.Stderr, "would be overwritten") {
		blocked := p.inTheWay(ctx, from, to)
		what := "a file that the update changes has a local edit or an untracked file is in its way"
		if len(blocked) > 0 {
			what = fmt.Sprintf("%d file(s) that the update changes have a local edit or an untracked file in the way, for example %s",
				len(blocked), strings.Join(blocked[:min(len(blocked), maxNamed)], ", "))
		}
		return p.fail(ReasonBlocked, "the checkout was not updated, because %s. Treebind does not write over them; "+
			"revert or remove them, or let them be committed, and the next pull goes on", what)
	}
	return p.fail(ReasonPullFailed, "the checkout in %s cannot be updated: %v", p.o.Layout.WorkTree, err)
}

// inTheWay lists the files that the update changes and that have a local edit or are untracked, for
// the message. It is not what decides: Git decided. It returns nothing when it cannot tell.
func (p *Puller) inTheWay(ctx context.Context, from, to string) []string {
	var changed []string
	var err error
	if from == "" {
		changed, err = p.names(ctx, "ls-tree", "-r", "-z", "--name-only", to)
	} else {
		changed, err = p.names(ctx, "diff-tree", "-r", "-z", "--no-renames", "--name-only", from, to)
	}
	if err != nil {
		return nil
	}
	local, err := p.names(ctx, "ls-files", "-z", "--modified", "--others")
	if err != nil {
		return nil
	}
	edited := make(map[string]bool, len(local))
	for _, name := range local {
		edited[name] = true
	}
	var blocked []string
	for _, name := range changed {
		if edited[name] {
			blocked = append(blocked, name)
		}
	}
	return blocked
}

// sides counts the commits that the checkout has and the commit it is compared with does not, and
// the other way round. A failure to count is an error and not a count: two commits that could not be
// compared are not commits that went different ways, and what is done about those is not to be done
// on a guess.
func (p *Puller) sides(ctx context.Context, head, tip string) (ours, theirs int, err error) {
	out, err := p.git.Run(ctx, "rev-list", "--left-right", "--count", head+"..."+tip)
	if err != nil {
		return 0, 0, p.fail(ReasonPullFailed, "the commits %s and %s cannot be compared: %v", short(head), short(tip), err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out.Stdout)), "%d\t%d", &ours, &theirs); err != nil {
		return 0, 0, p.fail(ReasonPullFailed, "the commits %s and %s cannot be compared: Git counted %q", short(head), short(tip),
			strings.TrimSpace(string(out.Stdout)))
	}
	return ours, theirs, nil
}

// short is how much of a commit a message shows.
func short(commit string) string { return commit[:min(len(commit), 12)] }

// try runs a command whose exit status 1 is an answer of nothing and not a failure, such as
// rev-parse --verify --quiet, and returns what it wrote, which is empty for that answer.
func (p *Puller) try(ctx context.Context, args ...string) (string, error) {
	out, err := p.git.Run(ctx, args...)
	if err == nil {
		return strings.TrimSpace(string(out.Stdout)), nil
	}
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return "", nil
	}
	return "", err
}

// names runs a command that lists paths, each ended with a NUL.
func (p *Puller) names(ctx context.Context, args ...string) ([]string, error) {
	out, err := p.git.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range strings.SplitSeq(string(out.Stdout), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// fail returns an *Error. The last of the arguments that is an error is the one underneath. One
// that is a refusal to run Git against the Git directory is the error of the checkout for that.
func (p *Puller) fail(reason, format string, args ...any) *Error {
	e := &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			e.Err = err
		}
	}
	if refusal, ok := checkout.Refusal(e.Err); ok {
		return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal.Err}
	}
	return e
}

// describe says what a checkout follows, for a message.
func describe(branch string) string {
	if branch == "" {
		return "no branch"
	}
	return "the branch " + strings.TrimPrefix(branch, headsPrefix)
}
