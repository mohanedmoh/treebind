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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// recoveryPrefix is the namespace of the refs that keep the commit the checkout was at before an update
// (docs/adr/0026). They are loose refs: the agent never packs refs, and git.VerifyGitDir refuses a
// packed-refs file.
const recoveryPrefix = "refs/treebind/recovery/"

// recoveryRef is one of the agent's refs in that namespace: a name that is a number, of at most 32 bits, and
// nothing else. A name that is bigger is somebody's, so that a ref of a person cannot make the next number wrap.
// The counter itself is good for four billion updates.
type recoveryRef struct {
	name, commit string
	n            uint64
}

// recoveryRefs lists the agent's refs in the namespace, the oldest first. A ref with another name
// there is somebody's, and is not listed.
func (p *Puller) recoveryRefs(ctx context.Context) ([]recoveryRef, error) {
	out, err := p.git.Run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", recoveryPrefix)
	if err != nil {
		return nil, err
	}
	var refs []recoveryRef
	for line := range strings.Lines(string(out.Stdout)) {
		commit, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.ParseUint(strings.TrimPrefix(name, recoveryPrefix), 10, 32); err == nil {
			refs = append(refs, recoveryRef{name: name, commit: commit, n: n})
		}
	}
	// By the number, and not the name: "9" and "00000010" are names that sort the other way round.
	slices.SortFunc(refs, func(a, b recoveryRef) int { return cmp.Or(cmp.Compare(a.n, b.n), strings.Compare(a.name, b.name)) })
	return refs, nil
}

// keepRecovery makes the ref that keeps the commit the checkout is at, and returns its name and the refs of the
// agent, the new one last. The newest ref is not made again while it already keeps that commit: an update that
// is tried at every interval, because a file is in its way, would otherwise push every older ref out. It is a
// write, so it is made after the fence and before the update. A failure of any of it is a failure of the pull:
// an update is not applied that cannot be undone.
func (p *Puller) keepRecovery(ctx context.Context, from string) (string, []recoveryRef, error) {
	refs, err := p.recoveryRefs(ctx)
	if err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the recovery refs cannot be looked at, and the update was not applied: %v", err)
	}
	if last := len(refs) - 1; last >= 0 && refs[last].commit == from {
		return refs[last].name, refs, nil
	}
	next := uint64(1)
	if last := len(refs) - 1; last >= 0 {
		next = refs[last].n + 1
	}
	name := fmt.Sprintf("%s%08d", recoveryPrefix, next)
	// The empty old value makes Git refuse a ref that is already there.
	if _, err := p.git.Run(ctx, "update-ref", name, from, ""); err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the recovery ref %s cannot be written, and the update was not applied: %v", name, err)
	}
	return name, append(refs, recoveryRef{name: name, commit: from, n: next}), nil
}

// tidyRecovery removes the refs past the number to keep, the oldest first, once the update is applied. It
// returns what it could not do, for the result, and not an error: the ref that keeps the commit is made, and
// an update that was applied is not a failure because an old ref stays. The next update tries again.
func (p *Puller) tidyRecovery(ctx context.Context, refs []recoveryRef) string {
	for _, old := range refs[:max(0, len(refs)-p.o.RecoveryRefs)] {
		if _, err := p.git.Run(ctx, "update-ref", "-d", old.name, old.commit); err != nil {
			return fmt.Sprintf("the old recovery ref %s cannot be removed: %v", old.name, err)
		}
	}
	return ""
}
