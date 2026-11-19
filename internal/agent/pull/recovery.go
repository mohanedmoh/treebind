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

// refsIn lists the agent's refs in a namespace of numbered refs, the oldest first. A ref with another name
// there is somebody's, and is not listed.
func (p *Puller) refsIn(ctx context.Context, prefix string) ([]recoveryRef, error) {
	out, err := p.git.Run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	var refs []recoveryRef
	for line := range strings.Lines(string(out.Stdout)) {
		commit, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.ParseUint(strings.TrimPrefix(name, prefix), 10, 32); err == nil {
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
	return p.keepRef(ctx, recoveryPrefix, "recovery", from, "the update")
}

// keepRef is keepRecovery for a namespace: it makes the numbered ref that keeps the commit, and what is then not done
// is called what, in the message of the failure.
func (p *Puller) keepRef(ctx context.Context, prefix, kind, from, what string) (string, []recoveryRef, error) {
	refs, err := p.refsIn(ctx, prefix)
	if err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the %s refs cannot be looked at, and %s was not applied: %v", kind, what, err)
	}
	if last := len(refs) - 1; last >= 0 && refs[last].commit == from {
		return refs[last].name, refs, nil
	}
	next := uint64(1)
	if last := len(refs) - 1; last >= 0 {
		next = refs[last].n + 1
	}
	name := fmt.Sprintf("%s%08d", prefix, next)
	// The empty old value makes Git refuse a ref that is already there.
	if _, err := p.git.Run(ctx, "update-ref", name, from, ""); err != nil {
		return "", nil, p.fail(ReasonPullFailed, "the %s ref %s cannot be written, and %s was not applied: %v", kind, name, what, err)
	}
	return name, append(refs, recoveryRef{name: name, commit: from, n: next}), nil
}

// tidyRecovery removes the refs past the number to keep, the oldest first, once the update is applied. It
// returns what it could not do, for the result, and not an error: the ref that keeps the commit is made, and
// an update that was applied is not a failure because an old ref stays. The next update tries again.
func (p *Puller) tidyRecovery(ctx context.Context, refs []recoveryRef) string {
	return p.tidyRefs(ctx, refs, p.o.RecoveryRefs, "recovery")
}

// tidyRefs removes the refs of a namespace past the number to keep, the oldest first.
func (p *Puller) tidyRefs(ctx context.Context, refs []recoveryRef, keep int, kind string) string {
	for _, old := range refs[:max(0, len(refs)-keep)] {
		if _, err := p.git.Run(ctx, "update-ref", "-d", old.name, old.commit); err != nil {
			return fmt.Sprintf("the old %s ref %s cannot be removed: %v", kind, old.name, err)
		}
	}
	return ""
}
