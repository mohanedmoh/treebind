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
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

// The reasons of a checkout of a directory of the repository, which are the ones of package subtree.
const (
	// ReasonPathNotFound means the branch of the repository has no directory at repository.path, or has something there that is
	// not one. Nothing was written, and the next start looks again.
	ReasonPathNotFound = subtree.ReasonPathNotFound
	// ReasonPathMismatch means the checkout was made for another directory than the one the Treebind names now, or for the whole
	// repository. Nothing was changed (FR-18).
	ReasonPathMismatch = subtree.ReasonPathMismatch
)

// pathMarkerName is the file in the state directory that says which directory of the repository the checkout was made for, with
// the name in quotes. A checkout of the whole repository has none, so a checkout from before repository.path was read is one of
// those, and so is one that a Treebind with no path makes. It is not in the Git directory, which the agent keeps to what Git
// makes (docs/adr/0019), and it is not in a commit, because a repository that has no commit has no snapshot to say it in.
const pathMarkerName = "repository-path"

// writePathMarker records the directory the checkout is made for. It is written aside and renamed, so that the file is whole or
// not there.
func (c *checkout) writePathMarker() error {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	aside := pathMarkerName + ".tmp"
	if err := root.WriteFile(aside, []byte(strconv.Quote(c.Path)+"\n"), 0o600); err != nil {
		return err
	}
	return root.Rename(aside, pathMarkerName)
}

// madeFor returns the directory the checkout was made for, and an empty string for the whole repository, which is no marker.
func (c *checkout) madeFor() (string, error) {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(pathMarkerName)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	made, err := strconv.Unquote(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("%s is not what the agent writes: %w", pathMarkerName, err)
	}
	return made, nil
}

// removePathMarker removes the marker. One that is not there is as good as removed.
func (c *checkout) removePathMarker() {
	root, err := os.OpenRoot(c.Layout.StateDir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.Remove(pathMarkerName)
}

// judgePath is checkPath for a checkout that has a commit, and nothing for one that has none.
func (c *checkout) judgePath(ctx context.Context) (*Error, error) {
	_, born, err := c.try(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return nil, fail(ReasonCheckoutFailed, "the checkout in %s cannot be read: %v", c.Layout.GitDir, err)
	}
	if !born {
		return nil, nil
	}
	return c.checkPath(), nil
}

// checkPath refuses a checkout that was made for another directory than the one the Treebind names, or for the whole repository
// when it names one, or the other way round. It is judged by the marker, and so it holds whether the repository has a commit or
// not, and nothing is asked of the repository, and nothing is changed.
func (c *checkout) checkPath() *Error {
	made, err := c.madeFor()
	if err != nil {
		return fail(ReasonCheckoutFailed, "the directory that the checkout in %s was made for cannot be read: %v", c.Layout.GitDir, err)
	}
	if made == c.Path {
		return nil
	}
	refusal := subtree.Mismatch(c.Layout, made, c.Path)
	return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal}
}

// projected turns an error of the snapshots into the error of the checkout: a refusal keeps its reason, and any other is a
// failure of the first fetch's kind, which is a failure to make the checkout.
func projected(err error, branch string) *Error {
	if refusal, ok := errors.AsType[*subtree.Error](err); ok {
		return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal}
	}
	return fail(ReasonCheckoutFailed, "the files of the directory of the branch %s cannot be made: %v", branch, err)
}
