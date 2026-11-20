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

package subtree

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// ErrBehind means that the snapshot at the tip of the checkout is not of the directory at the tip of the repository that was
// fetched: the repository changed the directory since, and the pull has to bring that in before anything is sent.
var ErrBehind = errors.New("the snapshot is not of the directory at the tip of the repository")

// Translation is what Translate made.
type Translation struct {
	// Tip is the commit of the repository that the head of the checkout is made again as. It is the tip that was fetched when
	// the checkout has nothing waiting.
	Tip string
	// Commits is how many commits of the repository were made.
	Commits int
}

// Translate makes the commits that the checkout has and the repository lacks again as commits of the repository, and returns the
// one that the head is. A commit of the repository has the parents of the commit it is made of, each as the commit of the
// repository it stands for, and the tree of the repository with the directory replaced by the tree of the commit: nothing outside
// the directory changes. The message is as it was, and the author and the committer are Options.Author.
//
// A commit that says it is a snapshot is not made again: it stands for the commit of the repository that it says it is of, and
// that is how a commit with an older snapshot for a parent, or a merge of the repository's branch, gets its parents. It is
// believed only when the directory at that commit is its tree.
//
// Nothing is written but the commits, which a push that does not happen leaves where they are, unreachable.
func (p *Projector) Translate(ctx context.Context, head string) (Translation, error) {
	held, err := p.held(ctx)
	if err != nil {
		return Translation{}, err
	}
	t := &translating{Projector: p, standsFor: map[string]string{}, trees: map[string]string{}}
	if held.commit != "" {
		real, err := p.commit(ctx, RealRef(p.o.Branch))
		if err != nil {
			return Translation{}, err
		}
		if real == "" {
			return Translation{}, fmt.Errorf("subtree: the branch %s of the repository has not been fetched", p.o.Branch)
		}
		if err := p.current(ctx, held.commit, real); err != nil {
			return Translation{}, err
		}
		t.standsFor[held.commit] = real
	}

	args := []string{"rev-list", "--topo-order", "--reverse", "--parents", head}
	if held.commit != "" {
		args = append(args, "^"+held.commit)
	}
	listed, err := p.run(ctx, args...)
	if err != nil {
		return Translation{}, err
	}
	var made int
	for line := range strings.Lines(listed) {
		fields := strings.Fields(line)
		commit, parents := fields[0], fields[1:]
		object, err := readCommit(ctx, p.git, commit)
		if err != nil {
			return Translation{}, err
		}
		if said, ok := parseSnapshot(object.message); ok {
			source, believed, err := p.believe(ctx, object.tree, said)
			if err != nil {
				return Translation{}, err
			}
			if believed {
				t.standsFor[commit] = source
				continue
			}
		}
		made++
		again, err := t.again(ctx, commit, object, parents)
		if err != nil {
			return Translation{}, err
		}
		t.standsFor[commit] = again
	}
	tip, ok := t.standsFor[head]
	if !ok {
		return Translation{}, fmt.Errorf("subtree: the commit %s is not one that the checkout is at or has waiting", short(head))
	}
	return Translation{Tip: tip, Commits: made}, nil
}

// translating is a Translate that is under way: the commit of the repository that each commit of the checkout stands for, and the tree
// of each commit of the repository that is known, so that a commit that is made on the one before it does not read that one again.
type translating struct {
	*Projector
	standsFor map[string]string
	trees     map[string]string
}

// commitObject is what Translate needs of a commit, read once: its tree and its message as they are stored.
type commitObject struct {
	tree, message string
}

// readCommit reads a commit through the Runner.
func readCommit(ctx context.Context, runner *git.Runner, commit string) (commitObject, error) {
	out, err := runner.Run(ctx, "cat-file", "commit", commit)
	if err != nil {
		return commitObject{}, err
	}
	header, message, _ := strings.Cut(string(out.Stdout), "\n\n")
	tree, ok := strings.CutPrefix(strings.SplitN(header, "\n", 2)[0], "tree ")
	if !ok {
		return commitObject{}, fmt.Errorf("the commit %s has no tree in what Git says of it", short(commit))
	}
	return commitObject{tree: tree, message: message}, nil
}

// current checks that the snapshot is of the directory at the commit of the repository.
func (p *Projector) current(ctx context.Context, snapshotCommit, real string) error {
	have, err := p.run(ctx, "rev-parse", "--verify", snapshotCommit+"^{tree}")
	if err != nil {
		return err
	}
	return p.sameDirectory(ctx, have, real)
}

// sameDirectory checks that the directory at the commit of the repository is the tree.
func (p *Projector) sameDirectory(ctx context.Context, tree, real string) error {
	dir, _, err := p.directory(ctx, real)
	if err != nil {
		return err
	}
	if dir != tree {
		return ErrBehind
	}
	return nil
}

// believe says whether what a commit says of itself is to be believed, and returns the commit of the repository that it is the
// snapshot of. A snapshot of another directory is a refusal, because nothing here makes one, and one that is not of the directory at
// the commit it names is not believed: it is a commit of the application whose message says so.
func (p *Projector) believe(ctx context.Context, tree string, said snapshot) (string, bool, error) {
	if said.path != p.o.Path {
		return "", false, Mismatch(p.o.Layout, said.path, p.o.Path)
	}
	if err := p.sameDirectory(ctx, tree, said.source); err != nil {
		if errors.Is(err, ErrBehind) {
			return "", false, nil
		}
		return "", false, err
	}
	return said.source, true, nil
}

// source returns the commit of the repository that a commit says it is a snapshot of, and false when it does not say so or is
// not believed.
func (p *Projector) source(ctx context.Context, commit string) (string, bool, error) {
	object, err := readCommit(ctx, p.git, commit)
	if err != nil {
		return "", false, err
	}
	said, ok := parseSnapshot(object.message)
	if !ok {
		return "", false, nil
	}
	return p.believe(ctx, object.tree, said)
}

// again makes a commit of the repository of a commit of the checkout, with the parents that stand for its own.
func (t *translating) again(ctx context.Context, commit string, object commitObject, parents []string) (string, error) {
	if t.author == nil {
		return "", errors.New("subtree: the commits of the agent have no valid author, and the Commit check says what is wrong with spec.commit")
	}
	base := emptyTree
	var realParents []string
	for i, parent := range parents {
		real, ok := t.standsFor[parent]
		if !ok {
			var err error
			if real, ok, err = t.source(ctx, parent); err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("subtree: the commit %s has the parent %s, which is neither a snapshot nor a commit that is waiting to be "+
					"sent, so there is no commit of the repository for it to stand on", short(commit), short(parent))
			}
		}
		if i == 0 {
			var err error
			if base, err = t.treeOfReal(ctx, real); err != nil {
				return "", err
			}
		}
		realParents = append(realParents, real)
	}
	tree, err := t.objects.replace(ctx, base, t.segments, object.tree)
	if err != nil {
		return "", fmt.Errorf("the commit %s cannot be made again on the repository: %w", short(commit), err)
	}
	// The message as it is stored: commit-tree adds a newline only when there is none, so a message that the agent made with the
	// verbatim cleanup is the same one.
	out, err := t.author.Run(ctx, commitTree(tree, object.message, realParents)...)
	if err != nil {
		return "", err
	}
	made := strings.TrimSpace(string(out.Stdout))
	t.trees[made] = tree
	return made, nil
}

// treeOfReal is the tree of a commit of the repository, which is known for the commits that this translation made.
func (t *translating) treeOfReal(ctx context.Context, real string) (string, error) {
	if tree, ok := t.trees[real]; ok {
		return tree, nil
	}
	tree, err := t.run(ctx, "rev-parse", "--verify", real+"^{tree}")
	if err != nil {
		return "", err
	}
	t.trees[real] = tree
	return tree, nil
}
