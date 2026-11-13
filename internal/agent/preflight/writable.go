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
	"os"
	"syscall"
)

const (
	// testFilePrefix starts the name of the hidden file the writable check
	// creates. Nothing else uses the prefix, so a file that a crash left behind
	// can be ignored or removed.
	testFilePrefix = ".treebind-preflight-"

	// docsURL is where a user finds what a volume has to allow.
	docsURL = "https://github.com/mohanedmoh/treebind/blob/main/docs/pod-security.md"
)

// checkWritable creates, writes to and removes a hidden file in the directory as
// the agent's own user, the way the agent will write the checkout. The file has a
// random name, so agents that start together do not meet on one file.
func checkWritable(dir string) Result {
	file, err := os.CreateTemp(dir, testFilePrefix+"*")
	if err != nil {
		return writeFailure(CheckWritable, dir, "create a file in", err)
	}
	name := file.Name()
	_, writeErr := file.WriteString("treebind preflight\n")
	// A network file system can report a refused write only when the file closes.
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return writeOutcome(dir, writeErr, closeErr, removeErr)
}

// writeOutcome turns the errors of the steps after the file was created into a
// result. The first step that failed decides it.
func writeOutcome(dir string, writeErr, closeErr, removeErr error) Result {
	for _, step := range []struct {
		action string
		err    error
	}{
		{"write to a file in", writeErr},
		{"close a file in", closeErr},
		{"remove a file from", removeErr},
	} {
		if step.err != nil {
			return writeFailure(CheckWritable, dir, step.action, step.err)
		}
	}
	return Result{
		Check:   CheckWritable,
		Outcome: Passed,
		Message: fmt.Sprintf("%s can be written to as %s", dir, identity()),
	}
}

// writeFailure turns the error of a write step into a result with the reason that
// tells a permission problem from a read-only volume and from any other failure.
// Only EACCES is a permission problem that the owner and mode of the directory
// explain. EPERM, which an immutable or append-only directory returns, is not
// cured by changing them, so it is reported as it is.
func writeFailure(check, dir, action string, err error) Result {
	switch {
	case errors.Is(err, syscall.EROFS):
		return Result{
			Check:   check,
			Outcome: Failed,
			Reason:  ReasonReadOnlyVolume,
			Message: dir + " is on a read-only volume: the claim may be mounted read-only, or the storage refuses " +
				"writes. Treebind needs a volume it can write to",
		}
	case errors.Is(err, syscall.EACCES):
		return Result{
			Check:   check,
			Outcome: Failed,
			Reason:  ReasonNotWritable,
			Message: fmt.Sprintf("this process (%s) cannot %s %s, which is %s. Make the directory writable for that "+
				"user or for a group it is in; see %s", identity(), action, dir, ownerOf(dir), docsURL),
		}
	default:
		return Result{
			Check:   check,
			Outcome: Failed,
			Reason:  ReasonWriteFailed,
			Message: fmt.Sprintf("could not %s %s: %v", action, dir, err),
		}
	}
}

// identity names the user and groups the agent runs as, the ones a permission
// check on the volume sees.
func identity() string {
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Sprintf("uid %d, gid %d", os.Geteuid(), os.Getegid())
	}
	return fmt.Sprintf("uid %d, gid %d, groups %v", os.Geteuid(), os.Getegid(), groups)
}

// ownerOf describes who owns the directory and what its mode allows.
func ownerOf(dir string) string {
	info, err := os.Stat(dir)
	if err != nil {
		return "of unknown ownership"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("set to mode %04o", info.Mode().Perm())
	}
	return fmt.Sprintf("owned by %d:%d with mode %04o", stat.Uid, stat.Gid, info.Mode().Perm())
}
