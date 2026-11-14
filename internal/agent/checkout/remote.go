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

package checkout

import (
	"context"
	"errors"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
)

const (
	// refHead is the name of HEAD in the listing of ls-remote, and symrefPrefix starts the line
	// that says which ref it names.
	refHead      = "HEAD"
	symrefPrefix = "ref: "
	headsPrefix  = "refs/heads/"
	remotePrefix = "refs/remotes/origin/"
	// fallbackBranch is the branch of a repository that has no commits and names no
	// default branch.
	fallbackBranch = "main"
)

// remoteState is what the repository says about the branch.
type remoteState struct {
	// branch is the branch to follow: the one the Treebind names, or the default branch.
	branch string
	// tip is the commit the branch is at, or empty when it has no commit on the repository.
	tip string
}

// askRemote asks the repository for its default branch and for the tip of the branch.
// A branch that is not there is an error when the repository has commits, and not when it
// has none, because a repository that was just made is a good place to start.
func (c *checkout) askRemote(ctx context.Context) (remoteState, error) {
	patterns := []string{refHead}
	if c.Branch != "" {
		patterns = append(patterns, headsPrefix+c.Branch)
	}
	args := append([]string{"ls-remote", "--symref", "--", c.URL}, patterns...)
	out, err := c.git.Run(ctx, args...)
	if err != nil {
		return remoteState{}, fail(ReasonFetchFailed, "the repository cannot be asked for its branches: %v", err)
	}
	listing := parseListing(string(out.Stdout))

	branch := c.Branch
	if branch == "" {
		branch = listing.defaultBranch
	}
	if branch == "" {
		branch = fallbackBranch
	}
	// A branch that the Treebind names was checked when the checkout began. The default
	// branch is the repository's choice, and may be a name the agent does not take.
	if c.Branch == "" {
		if err := config.CheckBranch(branch); err != nil {
			return remoteState{}, fail(ReasonBranchNotFound, "the default branch %q of the repository cannot be followed: %v", branch, err)
		}
	}
	tip := listing.heads[branch]
	if tip == "" && c.Branch == "" && listing.defaultBranch != "" {
		// The default branch was found through HEAD, which is its tip.
		tip = listing.head
	}
	if tip != "" {
		return remoteState{branch: branch, tip: tip}, nil
	}

	// The branch has no commit. That is fine for a repository that has none at all, which is
	// a good place to start. It is not for one that has branches: the one that was named is
	// not among them, or HEAD names one that is not there.
	has, err := c.remoteHasBranches(ctx)
	if err != nil {
		return remoteState{}, err
	}
	if has && c.Branch != "" {
		return remoteState{}, fail(ReasonBranchNotFound,
			"the repository has no branch %s. Set repository.branch to a branch it has", c.Branch)
	}
	if has {
		return remoteState{}, fail(ReasonBranchNotFound,
			"the repository has no default branch to follow. Set repository.branch to the branch to sync")
	}
	return remoteState{branch: branch}, nil
}

// remoteHasBranches reports whether the repository has a branch, any branch.
func (c *checkout) remoteHasBranches(ctx context.Context) (bool, error) {
	out, err := c.git.Run(ctx, "ls-remote", "--heads", "--", c.URL)
	if err != nil {
		return false, fail(ReasonFetchFailed, "the repository cannot be asked for its branches: %v", err)
	}
	return len(strings.TrimSpace(string(out.Stdout))) > 0, nil
}

// fetch fetches the branch into refs/remotes/origin and returns the commit it is at. The
// URL is on the command line and not in the config; the tags and the FETCH_HEAD file, which
// names the URL, are left out.
func (c *checkout) fetch(ctx context.Context) (string, error) {
	refspec := "+" + headsPrefix + c.branch + ":" + remotePrefix + c.branch
	if _, err := c.git.RunTimeout(ctx, c.FetchTimeout, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--", c.URL, refspec); err != nil {
		return "", fail(ReasonFetchFailed, "the branch %s cannot be fetched: %v", c.branch, err)
	}
	out, err := c.git.Run(ctx, "rev-parse", "--verify", remotePrefix+c.branch+"^{commit}")
	if err != nil {
		return "", fail(ReasonFetchFailed, "the branch %s was fetched but cannot be read: %v", c.branch, err)
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// checkReserved refuses a commit that has a file at the root with the name of the agent's
// state directory, however it is spelled, when the state directory is inside the work
// tree. A checkout would write it there, and a Git directory with a config of the
// repository's choosing is the one thing the Runner cannot neutralise (docs/adr/0013). With
// a sub path the state directory is beside the work tree and the paths of the commit
// cannot reach it.
func (c *checkout) checkReserved(ctx context.Context, tip string) *Error {
	if !c.Layout.GitDirInWorkTree {
		return nil
	}
	out, err := c.git.Run(ctx, "ls-tree", "-z", "--name-only", tip)
	if err != nil {
		return fail(ReasonCheckoutFailed, "the files of the branch cannot be listed: %v", err)
	}
	for _, name := range splitZ(string(out.Stdout)) {
		if strings.EqualFold(name, config.StateDirName) {
			return fail(ReasonReservedPath,
				"the branch %s has a file named %s at its root, where Treebind keeps its own state when the whole claim is synced. "+
					"Use a subPath, or rename the file", c.branch, name)
		}
	}
	return nil
}

// try runs a command whose exit status 1 is an answer and not a failure, such as
// rev-parse --verify --quiet, and returns what it wrote and whether the answer was yes.
func (c *checkout) try(ctx context.Context, args ...string) (string, bool, error) {
	out, err := c.git.Run(ctx, args...)
	if err == nil {
		return strings.TrimSpace(string(out.Stdout)), true, nil
	}
	if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
		return "", false, nil
	}
	return "", false, err
}

// listing is what ls-remote printed.
type listing struct {
	// head is the commit of HEAD, and defaultBranch the branch it names.
	head, defaultBranch string
	// heads is the commit of each branch that was asked for.
	heads map[string]string
}

// parseListing reads the output of ls-remote --symref.
func parseListing(out string) listing {
	l := listing{heads: map[string]string{}}
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		value, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch {
		case name == refHead && strings.HasPrefix(value, symrefPrefix):
			// HEAD names a ref. Only a branch is a default branch to follow.
			if target := strings.TrimPrefix(value, symrefPrefix); strings.HasPrefix(target, headsPrefix) {
				l.defaultBranch = strings.TrimPrefix(target, headsPrefix)
			}
		case name == refHead:
			l.head = value
		case strings.HasPrefix(name, headsPrefix):
			l.heads[strings.TrimPrefix(name, headsPrefix)] = value
		}
	}
	return l
}
