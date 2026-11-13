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

// Package layout says where the agent's files are on the claim (docs/adr/0016).
//
// The sync pod mounts the whole claim. The agent keeps its own state in the
// directory .treebind at the root of the claim, the Git directory in .treebind/git,
// and syncs the directory that target.subPath names, or the whole claim when there
// is none. With a sub path, .treebind is beside the synced directory and not in it,
// so an application that mounts only the sub path never sees the Git directory, its
// config or its hooks (R-17). Without one it is inside the synced directory, where
// the application can see it, and the layout says so.
//
// The package only works out paths. Making the directories and checking that they
// are what they should be is separate.
package layout

import (
	"fmt"
	"path"

	"github.com/mohanedmoh/treebind/internal/agent/config"
)

// GitDirName is the Git directory inside the state directory.
const GitDirName = "git"

// Layout is where the agent's files are. Every path is absolute.
type Layout struct {
	// Root is the claim, where the sync pod mounts it.
	Root string
	// SubPath is the directory of the claim that is synced, relative to Root, or empty
	// when the whole claim is.
	SubPath string
	// WorkTree is the directory that is synced with the repository.
	WorkTree string
	// StateDir is the directory of the agent's own state, at the root of the claim.
	StateDir string
	// GitDir is the Git directory.
	GitDir string
	// GitDirInWorkTree is true when the whole claim is synced, which puts the state
	// directory, and so the Git directory, inside the work tree.
	GitDirInWorkTree bool
}

// New returns the layout for a claim mounted at root, with the sub path of the
// Treebind. It refuses a root that is not an absolute path in its shortest form, and
// a sub path that the CRD would refuse.
func New(root, subPath string) (Layout, error) {
	if !path.IsAbs(root) || path.Clean(root) != root {
		return Layout{}, fmt.Errorf("the mount path %q must be an absolute path in its shortest form", root)
	}
	if err := config.CheckSubPath(subPath); err != nil {
		return Layout{}, fmt.Errorf("the sub path %q %w", subPath, err)
	}

	layout := Layout{Root: root, SubPath: subPath, WorkTree: root, GitDirInWorkTree: subPath == ""}
	if subPath != "" {
		layout.WorkTree = path.Join(root, subPath)
	}
	layout.StateDir = path.Join(root, config.StateDirName)
	layout.GitDir = path.Join(layout.StateDir, GitDirName)
	return layout, nil
}

// NeverSynced returns the path, relative to the work tree, that the sync must never
// stage, or an empty string when there is none. It is the state directory when that
// is inside the work tree: the repository does not hold the agent's Git directory.
func (l Layout) NeverSynced() string {
	if l.GitDirInWorkTree {
		return config.StateDirName
	}
	return ""
}
