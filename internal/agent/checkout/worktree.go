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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/config"
)

// markerName is the file in the state directory that says the agent began to write the
// files of a first checkout into a directory it found empty.
const markerName = "cloning"

// commitID is what a marker holds: the id of a commit, SHA-1 or SHA-256.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// hasRepository reports whether the Git directory holds a repository.
func (c *checkout) hasRepository() bool {
	_, err := os.Lstat(filepath.Join(c.Layout.GitDir, "HEAD"))
	return err == nil
}

// workTreeEmpty reports whether the synced directory has nothing in it, not counting the
// state directory when that is inside it.
func (c *checkout) workTreeEmpty() (bool, error) {
	entries, err := os.ReadDir(c.Layout.WorkTree)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if c.Layout.GitDirInWorkTree && entry.Name() == config.StateDirName {
			continue
		}
		return false, nil
	}
	return true, nil
}

// clearGitDir removes what is in the Git directory and the marker, and keeps the
// directory, which was made private and is the agent's own.
func (c *checkout) clearGitDir() error {
	root, err := os.OpenRoot(c.Layout.GitDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	c.removeMarker()
	return nil
}

// writeMarker records the commit that the first checkout is writing.
func (c *checkout) writeMarker(commit string) error {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(markerName, []byte(commit+"\n"), 0o600)
}

// readMarker returns the commit of the marker, and whether there is a usable one.
func (c *checkout) readMarker() (string, bool) {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return "", false
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(markerName)
	if err != nil {
		return "", false
	}
	commit := strings.TrimSpace(string(data))
	return commit, commitID.MatchString(commit)
}

// removeMarker removes the marker. One that is not there is as good as removed, and one
// that cannot be removed does no harm: it only matters on a branch that has no commit,
// and the next start tries again.
func (c *checkout) removeMarker() {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.Remove(markerName)
}
