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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// Policy says which side wins when the checkout and the repository each have commits that the other lacks
// (docs/adr/0034). It is the conflict policy of the Treebind.
type Policy string

const (
	// FollowRemote makes the repository win: the checkout is moved to the repository's tip with the files that only the
	// application changed put on it, and the commits that the checkout had are kept under a ref of their own.
	FollowRemote Policy = config.ConflictFollowRemote
	// FollowLocal makes the volume win: the checkout is moved to a merge of its own commits and the repository's, in
	// which a file that both changed has the version of the volume, so that the next push is a fast-forward. When both
	// hold the same files there is nothing to merge, and it is moved to the repository's tip as followRemote is.
	FollowLocal Policy = config.ConflictFollowLocal
)

// ReasonConflictResolved is the reason of the check that says that a conflict was resolved. A resolution that could not be
// made is not one: it is a failed pull, with the reason of the update, and a message that says the conflict was not resolved
// (docs/adr/0034). It is API in the sense of docs/adr/0004.
const ReasonConflictResolved = "ConflictResolved"

const (
	// DefaultLocalRefs is how many of the refs that keep the commits of the checkout after a resolution are kept.
	DefaultLocalRefs = 20

	// localPrefix is the namespace of the refs that keep the commits a followRemote resolution left out of the branch. It
	// is beside the recovery refs and works as they do: loose, numbered, local, and never pushed, because the push
	// guard of the Runner refuses any destination that is not under refs/heads/.
	localPrefix = "refs/treebind/local/"

	// countTimeout is how long telling the breaker of a commit may take, when the agent was told to stop in the middle of the
	// pull: the checkout is at the commit whether it is counted or not.
	countTimeout = 5 * time.Second

	// indexBatch is how many files one command puts in the index, so that the command line stays short.
	indexBatch = 200
)

// Resolution says how a conflict was resolved. The counts are of files, and a file is a path: a rename is a delete and
// an add, and a file that only one side changed is not a conflict.
type Resolution struct {
	// Policy is the policy that was applied.
	Policy Policy
	// LocalCommits and RemoteCommits are how many commits each side had that the other lacked.
	LocalCommits, RemoteCommits int
	// Overridden is how many files both sides changed differently, for which the winner's version was taken whole.
	Overridden int
	// Carried is how many files only the side that lost changed, which were put on the winner's version.
	Carried int
	// Dropped names the files that only the side that lost changed and that could not be put on the winner's version,
	// because the winner has a file where they would need a directory, or a directory where they need a file. With
	// followRemote they are in the ref that keeps the commits of the checkout.
	Dropped []string
	// Commit is the commit that the resolution made. It is empty when the checkout was moved to the repository's tip,
	// because the application had changed nothing that the repository did not, or because both sides hold the same files.
	Commit string
	// Local is the ref that keeps the commits of the checkout. It is set when the checkout was moved to the repository's tip:
	// always with followRemote, and with followLocal when both sides hold the same files.
	Local string
}

// resolve resolves the conflict of a checkout at head and a repository at tip, which each have commits that the other
// lacks, by the policy (docs/adr/0034). It builds one commit, with the files of the side that wins and the files that
// only the other side changed, and then moves the checkout to it with the update that a pull makes, so that the lock
// is checked, the journal is written, the cap on deletions is applied and Git refuses to write over a local edit,
// all as for an update.
func (p *Puller) resolve(ctx context.Context, head, tip string, ours, theirs int) (Result, error) {
	result, err := p.resolveConflict(ctx, head, tip, ours, theirs)
	if failure, ok := errors.AsType[*Error](err); ok {
		failure.Message = fmt.Sprintf("the conflict between the checkout and the branch %s of the repository was not resolved: %s",
			p.o.Branch, failure.Message)
	}
	return result, err
}

func (p *Puller) resolveConflict(ctx context.Context, head, tip string, ours, theirs int) (Result, error) {
	// A resolution that has begun is not cut off by a stop, but one is not begun after it.
	if err := ctx.Err(); err != nil {
		return Result{}, p.fail(ReasonPullFailed, "the agent was told to stop: %v", err)
	}
	// The commit of a resolution needs an author, and a pull that has none says so before it writes anything.
	if p.o.Git.Identity == (git.Identity{}) {
		return Result{}, p.fail(ReasonPullFailed, "the commits of the agent have no valid author, and the Commit check says what is wrong with "+
			"spec.commit")
	}
	winner, loser := tip, head
	if p.o.Policy == FollowLocal {
		winner, loser = head, tip
	}
	found, err := p.compare(ctx, head, tip, winner, loser)
	if err != nil {
		return Result{}, err
	}
	res := Resolution{Policy: p.o.Policy, LocalCommits: ours, RemoteCommits: theirs, Overridden: found.overridden}

	// A resolution that makes a commit is bounded, and one that does not is not: the checkout moves to the tip and nothing is
	// made that the repository would have to take. It is asked before anything is written, a ref for the commits of the checkout
	// included, because one that is held at every interval would make a ref at each.
	if (p.o.Policy == FollowLocal && !found.sameTree) || len(found.carry) > 0 {
		if err := p.allowCommit(ctx); err != nil {
			return Result{}, err
		}
	}

	// The first write, and everything before it only read.
	if p.o.Fence != nil {
		if err := p.o.Fence(ctx); err != nil {
			return Result{}, p.fail(ReasonRefused, "the lock could not be checked: %v", err)
		}
	}
	// The checkout is moved to the tip, and not to a descendant of itself, when the repository wins and when both sides hold
	// the same files: a merge of two sides with the same tree is a commit that the repository would have to take for nothing,
	// and a host that rewrites what it is sent gives it back as another, so that the next pull makes one again (R-18).
	toTip := p.o.Policy == FollowRemote || found.sameTree
	var refs []recoveryRef
	if toTip {
		if res.Local, refs, err = p.keepRef(ctx, localPrefix, "local", head, "the resolution"); err != nil {
			return Result{}, err
		}
	}

	target := tip
	if (p.o.Policy == FollowLocal && !found.sameTree) || len(found.carry) > 0 {
		commit, dropped, err := p.commitOf(ctx, head, tip, winner, found.carry, res)
		if err != nil {
			return Result{}, err
		}
		res.Dropped = dropped
		// followRemote has no commit when nothing of what the checkout changed can be put on the tip: it would have the
		// tree of the tip, push nothing, and say that it kept files.
		if commit != "" {
			target, res.Commit = commit, commit
		}
	}
	res.Carried = len(found.carry) - len(res.Dropped)

	// A move to the tip is to a commit that is not a descendant of the checkout's, with a checkout that is refused for the same
	// things as a merge is. followLocal's commit is a descendant of the checkout's, and is a fast-forward.
	result, err := p.apply(ctx, head, target, move{switching: toTip})
	// The old refs go whether the move was made or not: a resolution that is refused at every interval, while the
	// application commits, makes a ref at each try, and none would go. Not after the lock was refused, which is a write.
	var warning string
	if failure, ok := errors.AsType[*Error](err); !ok || failure.Reason != ReasonRefused {
		warning = p.tidyRefs(ctx, refs, p.o.LocalRefs, "local")
	}
	if err != nil {
		return Result{}, err
	}
	if warning != "" {
		result.Warning = strings.Trim(result.Warning+"; "+warning, "; ")
	}
	result.Outcome, result.Resolution = Resolved, &res
	if res.Commit != "" {
		result.Warning = strings.Trim(result.Warning+"; "+p.countCommit(ctx), "; ")
	}
	return result, nil
}

// allowCommit asks the breaker whether a resolution may make a commit. A hold is the reason TooManyCommits, with the words of
// the breaker, and a count that cannot be made is a failed pull: what cannot be counted is not guessed.
func (p *Puller) allowCommit(ctx context.Context) error {
	if p.o.Gate == nil {
		return nil
	}
	err := p.o.Gate.Allow(ctx)
	if err == nil {
		return nil
	}
	if held, ok := errors.AsType[*breaker.HeldError](err); ok {
		return &Error{Reason: ReasonTooManyCommits, Message: held.Error(), Err: err}
	}
	return p.fail(ReasonPullFailed, "nothing was written, because the commits of the last hour cannot be counted: %v", err)
}

// countCommit tells the breaker that the resolution made a commit, and returns what to warn of when that did not work. The
// checkout is at the commit, so a stop that came in the middle of the pull does not cancel this: a short time is all it needs.
func (p *Puller) countCommit(ctx context.Context) string {
	if p.o.Gate == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), countTimeout)
	defer cancel()
	if err := p.o.Gate.Made(ctx); err != nil {
		return fmt.Sprintf("the commit of the resolution was not counted against the limit of commits in an hour, so the limit may be exceeded by it: %v", err)
	}
	return ""
}

// comparison is what the two sides did since the commit they share.
type comparison struct {
	// carry are the changes of the side that lost to files that the winner did not change.
	carry []touch
	// overridden is how many files both sides changed differently.
	overridden int
	// sameTree says that the checkout and the repository hold the same files, as one tree: there is nothing that a policy
	// has to decide, and no commit to make, whichever side wins.
	sameTree bool
}

// compare lists what each side changed since the commit that the two share, which is the empty tree for histories that
// share none, and tells the files that only the loser changed from the files that both changed. It only reads.
func (p *Puller) compare(ctx context.Context, head, tip, winner, loser string) (comparison, error) {
	base, err := p.try(ctx, "merge-base", head, tip)
	if err != nil {
		return comparison{}, p.fail(ReasonPullFailed, "the commit that the checkout and the repository share cannot be found: %v", err)
	}
	if base == "" {
		out, err := p.git.Run(ctx, "hash-object", "-t", "tree", "/dev/null")
		if err != nil {
			return comparison{}, p.fail(ReasonPullFailed, "the empty tree cannot be found: %v", err)
		}
		base = strings.TrimSpace(string(out.Stdout))
	}
	lost, err := p.rawDiff(ctx, base, loser)
	if err != nil {
		return comparison{}, p.fail(ReasonPullFailed, "what the checkout and the repository changed cannot be listed: %v", err)
	}
	won, err := p.rawDiff(ctx, base, winner)
	if err != nil {
		return comparison{}, p.fail(ReasonPullFailed, "what the checkout and the repository changed cannot be listed: %v", err)
	}
	byPath := make(map[string]touch, len(won))
	for _, change := range won {
		byPath[change.path] = change
	}
	same, err := p.sameTree(ctx, head, tip)
	if err != nil {
		return comparison{}, err
	}
	found := comparison{sameTree: same}
	for _, change := range lost {
		other, both := byPath[change.path]
		switch {
		case !both:
			found.carry = append(found.carry, change)
		case other.newMode != change.newMode || other.newID != change.newID:
			found.overridden++
		}
	}
	return found, nil
}

// sameTree says whether two commits have the same tree, which is the id of all of their files and modes. It only reads.
func (p *Puller) sameTree(ctx context.Context, a, b string) (bool, error) {
	out, err := p.git.Run(ctx, "rev-parse", a+"^{tree}", b+"^{tree}")
	if err != nil {
		return false, p.fail(ReasonPullFailed, "the trees of the checkout and of the repository cannot be compared: %v", err)
	}
	trees := strings.Fields(string(out.Stdout))
	if len(trees) != 2 {
		return false, p.fail(ReasonPullFailed, "the trees of the checkout and of the repository cannot be compared: Git gave %d ids", len(trees))
	}
	return trees[0] == trees[1], nil
}

// commitOf makes the commit that the checkout is moved to: the tree of the winner with the changes of the loser that
// the winner did not make, and the parents that the policy says. It returns the commit, which is empty for followRemote
// when none of the files could be carried, and the files it could not carry. It writes objects and, for a moment, the index, which it puts back before it returns, whatever happens
// to it; a stop does not cut it off, it has Drain to finish, and what an agent that is killed in the middle of it leaves
// is an index with files staged, which the next pull puts right (docs/adr/0028).
func (p *Puller) commitOf(ctx context.Context, head, tip, winner string, carry []touch, res Resolution) (string, []string, error) {
	running, finished := drained(ctx, p.o.Drain)
	defer finished()

	tree, dropped, err := p.treeOf(running, winner, carry)
	if err != nil {
		return "", nil, err
	}
	if p.o.Policy == FollowRemote && len(dropped) == len(carry) {
		// Nothing was put on the tip, and the tree is its own.
		return "", dropped, nil
	}
	parents := []string{tip}
	if p.o.Policy == FollowLocal {
		parents = []string{head, tip}
	}
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", p.message(tip, res, len(carry)-len(dropped)))
	out, err := p.git.Run(running, args...)
	if err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the commit of the resolution cannot be made: %v", err)
	}
	return strings.TrimSpace(string(out.Stdout)), dropped, nil
}

// message is the message of the commit of a resolution.
func (p *Puller) message(tip string, res Resolution, carried int) string {
	counts := fmt.Sprintf("The checkout had %d commit(s) that the branch %s of the repository did not, and the repository had %d "+
		"that the checkout did not.", res.LocalCommits, p.o.Branch, res.RemoteCommits)
	if p.o.Policy == FollowLocal {
		return fmt.Sprintf("treebind: merge the branch %s of the repository (%s)\n\n%s The volume wins (followLocal): for the %d "+
			"file(s) that both changed, its version is taken, and the %d file(s) that only the repository changed are kept.",
			p.o.Branch, short(tip), counts, res.Overridden, carried)
	}
	return fmt.Sprintf("treebind: keep the local changes on top of %s\n\n%s The repository wins (followRemote): for the %d "+
		"file(s) that both changed, its version is taken, and the %d file(s) that only the checkout changed are kept. "+
		"The commits of the checkout are kept in %s.", short(tip), counts, res.Overridden, carried, res.Local)
}

// treeOf writes the tree of the winner with the changes of the carry made on it, and returns its id and the files of the
// carry that could not be put on it. It makes the tree in the index, which is the one place that Git makes a tree
// from files that are not in the work tree, and the Runner takes no index of its own, and puts the index back to the commit
// the checkout is at.
func (p *Puller) treeOf(ctx context.Context, winner string, carry []touch) (tree string, dropped []string, err error) {
	if _, err := p.git.Run(ctx, "read-tree", winner); err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the tree of the resolution cannot be made: %v", err)
	}
	defer func() {
		if restored := p.restoreIndex(ctx); restored != nil && err == nil {
			tree, dropped, err = "", nil, restored
		}
	}()

	var removals, additions []touch
	for _, change := range carry {
		if change.newMode == "" {
			removals = append(removals, change)
		} else {
			additions = append(additions, change)
		}
	}
	// Removals first: a file that the loser deleted where it made a directory is out of the way of the files in it.
	for batch := range chunks(removals, indexBatch) {
		args := []string{"update-index", "--force-remove", "--"}
		for _, change := range batch {
			args = append(args, change.path)
		}
		if _, err := p.git.Run(ctx, args...); err != nil {
			return "", nil, p.fail(ReasonPullFailed, "the tree of the resolution cannot be made: %v", err)
		}
	}
	for batch := range chunks(additions, indexBatch) {
		skipped, err := p.addToIndex(ctx, batch)
		if err != nil {
			return "", nil, err
		}
		dropped = append(dropped, skipped...)
	}
	out, err := p.git.Run(ctx, "write-tree")
	if err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the tree of the resolution cannot be written: %v", err)
	}
	return strings.TrimSpace(string(out.Stdout)), dropped, nil
}

// addToIndex puts the files in the index. A batch is one command, which writes nothing when it fails. A batch that
// fails is tried a file at a time, to find the files that cannot go in because the index has a file where they need a
// directory or a directory where they need a file: those are returned, and any other failure is an error.
func (p *Puller) addToIndex(ctx context.Context, batch []touch) ([]string, error) {
	if err := p.updateIndex(ctx, batch); err == nil {
		return nil, nil
	}
	var dropped []string
	for _, change := range batch {
		err := p.updateIndex(ctx, []touch{change})
		if err == nil {
			continue
		}
		if clash, lookErr := p.clashes(ctx, change.path); lookErr != nil || !clash {
			return nil, p.fail(ReasonPullFailed, "the tree of the resolution cannot be made: %v", err)
		}
		dropped = append(dropped, change.path)
	}
	return dropped, nil
}

func (p *Puller) updateIndex(ctx context.Context, batch []touch) error {
	args := make([]string, 0, 2+2*len(batch))
	args = append(args, "update-index", "--add")
	for _, change := range batch {
		args = append(args, "--cacheinfo", change.newMode+","+change.newID+","+change.path)
	}
	_, err := p.git.Run(ctx, args...)
	return err
}

// clashes reports whether the index has a file named like a directory that the path is in, or has files under the
// path, which is a directory there.
func (p *Puller) clashes(ctx context.Context, path string) (bool, error) {
	specs := []string{":(literal)" + path}
	var directories []string
	for dir := path; ; {
		slash := strings.LastIndexByte(dir, '/')
		if slash < 0 {
			break
		}
		dir = dir[:slash]
		directories = append(directories, dir)
		specs = append(specs, ":(literal)"+dir)
	}
	listed, err := p.names(ctx, append([]string{"ls-files", "-z", "--cached", "--"}, specs...)...)
	if err != nil {
		return false, err
	}
	for _, name := range listed {
		if strings.HasPrefix(name, path+"/") || slices.Contains(directories, name) {
			return true, nil
		}
	}
	return false, nil
}

// restoreIndex puts the index back to the commit the checkout is at. It does not touch the work tree, so what the
// application changed stays a change of the application's, and the move that follows reads the state of the files
// itself: a merge and a checkout both refresh the index before they decide that a file is edited.
func (p *Puller) restoreIndex(ctx context.Context) error {
	if _, err := p.git.Run(ctx, "read-tree", "HEAD"); err != nil {
		return p.fail(ReasonPullFailed, "the index of the checkout in %s cannot be put back: %v", p.o.Layout.GitDir, err)
	}
	return nil
}
