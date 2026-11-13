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

package preflight

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// gitDirDocsURL is where a user finds how the agent keeps its Git directory and what
// the application has to mount.
const gitDirDocsURL = "https://github.com/mohanedmoh/treebind/blob/main/docs/git-directory.md"

// checkGitDir makes the directories where the agent keeps its Git directory and checks
// that they are the agent's own and private (docs/adr/0016). A directory that is a link,
// belongs to another user or is open to group or others is refused, because the Git
// directory is where a hook or a config key would run code with the agent's credentials.
// A claim that cannot be written to fails like the writable check says it does.
func checkGitDir(path, subPath string) Result {
	where, err := layout.New(path, subPath)
	if err != nil {
		// The settings were checked when the agent started, so this is not expected. The
		// check protects, so it refuses and does not skip.
		return Result{
			Check:   CheckGitDir,
			Outcome: Failed,
			Reason:  ReasonGitDirUnsafe,
			Message: fmt.Sprintf("the layout of the claim cannot be worked out: %v", err),
		}
	}

	var unsafe *layout.UnsafeError
	switch err := where.Prepare(); {
	case err == nil:
		return Result{
			Check:   CheckGitDir,
			Outcome: Passed,
			Message: passedMessage(where),
		}
	case errors.As(err, &unsafe):
		return Result{
			Check:   CheckGitDir,
			Outcome: Failed,
			Reason:  ReasonGitDirUnsafe,
			Message: fmt.Sprintf("%s. Git is not run against it. %s; see %s", unsafe, remedy(unsafe), gitDirDocsURL),
		}
	default:
		return writeFailure(CheckGitDir, failedIn(where, err), "make the Git directory in", err)
	}
}

// remedy says what puts an unsafe directory right. The agent owns the state directory and
// the Git directory, so they have to be its own and private. The application owns the way
// to the synced directory, so only a link or a file there is in the way.
func remedy(unsafe *layout.UnsafeError) string {
	if unsafe.OnTheWay {
		return "Replace it with a real directory"
	}
	return fmt.Sprintf("Remove it, or make it a directory of %s with mode 0700", identity())
}

// failedIn is the directory a failure to make or look at something happened in. It is
// the one the error names, which is the claim itself when the claim cannot be written
// to, and not always the state directory.
func failedIn(l layout.Layout, err error) string {
	pathErr, ok := errors.AsType[*fs.PathError](err)
	if !ok || !filepath.IsAbs(pathErr.Path) || pathErr.Path == l.Root {
		return l.Root
	}
	return filepath.Dir(pathErr.Path)
}

// passedMessage says where the Git directory is, and, when it is in the synced
// directory, that the application can see the directory it is in.
func passedMessage(l layout.Layout) string {
	message := fmt.Sprintf("the Git directory %s is private to the agent (%s)", l.GitDir, identity())
	if l.GitDirInWorkTree {
		message += ". It is inside the synced directory, so the application can see where it is. " +
			"Give the Treebind a subPath and mount only that in the application; see " + gitDirDocsURL
	}
	return message
}
