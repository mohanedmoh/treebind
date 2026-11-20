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

	"github.com/mohanedmoh/treebind/internal/agent/git"
)

// What the tests of package subtree_test, which make their repositories with the fixtures of the agent and so cannot be in this
// package (the fixtures use the checkout, which uses this package), need of what is not exported.
const (
	EmptyTree     = emptyTree
	TrailerSource = trailerSource
)

// SnapshotMessage and Short are snapshotMessage and short.
var (
	SnapshotMessage = snapshotMessage
	Short           = short
)

// ReplaceDirectory is objects.replace: the tree of root with the directory that the segments name replaced by the tree sub.
func ReplaceDirectory(ctx context.Context, runner *git.Runner, scratch, root string, segments []string, sub string) (string, error) {
	return objects{git: runner, scratch: scratch}.replace(ctx, root, segments, sub)
}
