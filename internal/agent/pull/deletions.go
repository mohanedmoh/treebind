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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// approve ends the message of every refusal of the cap: what a person does about it.
const approve = "Nothing was written. If the update is meant, remove those files yourself, and the next pull goes on"

// checkDeletions refuses an update that would remove more files from the directory than MaxDeletions,
// and one that would remove any file and leave the branch with none (docs/adr/0026). It only reads.
//
// What counts is the files of the commit the checkout is at that the commit it moves to does not have,
// and that are in the directory now: a file that the application removed itself is not removed by the
// update, and a person who means a big deletion removes the files, after which the update has few
// left to remove. A file that moved is a file removed and another added.
func (p *Puller) checkDeletions(ctx context.Context, from, to string) error {
	if from == "" {
		// A checkout with no commit has nothing for an update to remove.
		return nil
	}
	listed, err := p.names(ctx, "diff-tree", "-r", "-z", "--no-renames", "--diff-filter=D", "--name-only", from, to)
	if errors.Is(err, git.ErrOutputLimit) {
		return p.fail(ReasonTooManyDeletions, "the update would remove so many files that Treebind cannot list them, and more than "+
			"the %d that it removes in one update. "+approve, p.o.MaxDeletions)
	}
	if err != nil {
		return p.fail(ReasonPullFailed, "the files that the update removes cannot be listed: %v", err)
	}
	var present []string
	for _, name := range listed {
		// Only a file that is not there is not counted. One that cannot be looked at may be, and an update that
		// is refused for it is tried again at the next interval, where one that is applied is not undone.
		_, err := os.Lstat(filepath.Join(p.o.Layout.WorkTree, filepath.FromSlash(name)))
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			present = append(present, name)
		}
	}
	if len(present) > p.o.MaxDeletions {
		return p.fail(ReasonTooManyDeletions, "the update would remove %d file(s) from the directory, more than the %d that Treebind "+
			"removes in one update, for example %s. "+approve, len(present), p.o.MaxDeletions, named(present))
	}
	if len(present) == 0 {
		return nil
	}
	left, err := p.names(ctx, "ls-tree", "-z", "--name-only", to)
	if err != nil {
		return p.fail(ReasonPullFailed, "the files of the branch cannot be listed: %v", err)
	}
	if len(left) == 0 {
		return p.fail(ReasonTooManyDeletions, "the update would remove %d file(s) from the directory and leave the branch with no file "+
			"at all, for example %s. "+approve, len(present), named(present))
	}
	return nil
}

// named is the first few of the files, for a message.
func named(files []string) string {
	return strings.Join(files[:min(len(files), maxNamed)], ", ")
}
