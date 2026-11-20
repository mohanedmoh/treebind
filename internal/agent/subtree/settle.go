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
	"fmt"
)

// Settle puts the checkout at the commit that was sent, once the repository has taken it: head is the commit of the checkout that
// was made again as the commit pushed. It makes the snapshot of pushed, with the tree of head, on the snapshot that the checkout
// followed and not on head, so that every commit that the checkout holds beside its own is a snapshot, and moves, in this order:
// the ref of the snapshot, the branch of the checkout and the ref of the repository's tip. The files and the index are not
// touched: the snapshot has the tree that they are at.
//
// A stop or a kill between two of the three leaves what the pull puts right. The ref of the repository's tip is the last, so it
// is not the one that the repository has, and the pull fetches it and finds a directory that is the tree of the snapshot, which
// is then the snapshot of the tip. If the branch was not moved the checkout has a commit with the tree of the tip, and the
// resolution of two commits with the same tree moves it to the tip with no commit (docs/adr/0035).
//
// The fence is called immediately before the first write, and nothing is written when it returns an error.
func (p *Projector) Settle(ctx context.Context, head, pushed string, fence func(context.Context) error) error {
	tree, err := p.run(ctx, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return err
	}
	sent, _, err := p.directory(ctx, pushed)
	if err != nil {
		return err
	}
	if sent != tree {
		return fmt.Errorf("subtree: the commit %s that was sent does not have the files of the checkout, so the checkout is not put at it", short(pushed))
	}
	held, err := p.held(ctx)
	if err != nil {
		return err
	}
	known, err := p.commit(ctx, RealRef(p.o.Branch))
	if err != nil {
		return err
	}
	if fence != nil {
		if err := fence(ctx); err != nil {
			return err
		}
	}
	snapshotCommit, err := p.snapshot(ctx, tree, pushed, held.commit)
	if err != nil {
		return err
	}
	// The repository's tip is last: it is never ahead of the snapshot that is of it, because the pull fetches only when the
	// repository's tip is not the one that the checkout holds.
	for _, move := range [][]string{
		{TrackingRef(p.o.Branch), snapshotCommit, held.commit},
		{headsPrefix + p.o.Branch, snapshotCommit, head},
		{RealRef(p.o.Branch), pushed, known},
	} {
		if _, err := p.git.Run(ctx, "update-ref", move[0], move[1], move[2]); err != nil {
			return err
		}
	}
	return nil
}
