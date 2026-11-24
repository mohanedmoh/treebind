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

package git

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// What an Error can be caused by, besides the failure of the command itself, which
// is an *exec.ExitError. Use errors.Is.
var (
	// ErrNotAllowed means the subcommand, or an option, is not one the agent may
	// use.
	ErrNotAllowed = errors.New("not allowed")
	// ErrTimeout means the command ran for longer than its timeout and was
	// stopped, together with everything it had started.
	ErrTimeout = errors.New("timed out")
	// ErrCanceled means the caller's context ended and the command was stopped.
	ErrCanceled = errors.New("canceled")
	// ErrOutputLimit means the command wrote more than the limit to its
	// standard output and was stopped.
	ErrOutputLimit = errors.New("output limit exceeded")
)

// Error is what a command that did not succeed returns. Its text is fixed: it
// names the subcommand and what happened, never the arguments, which can hold a
// URL, and ends with the last lines of the standard error with the credentials of
// any URL removed.
type Error struct {
	// Subcommand is the first argument.
	Subcommand string
	// Option is the option that was refused, with ErrNotAllowed.
	Option string
	// Refspec is the refspec of a push that was refused, with ErrNotAllowed.
	Refspec string
	// Cause is one of the errors above, the *exec.ExitError of a command that
	// failed, or the error that kept it from starting.
	Cause error
	// ExitCode is the exit status of a command that failed, and zero otherwise.
	ExitCode int
	// Errno is the error of the system that a command that failed names in its
	// standard error, of those that say the volume cannot be written to (see
	// VolumeRefused), and zero otherwise. errors.Is matches it.
	Errno syscall.Errno
	// Stderr is the last lines of the standard error, on one line, with the
	// credentials of any URL removed.
	Stderr string
	// Timeout and MaxOutput are the limits that applied, for the text.
	Timeout   time.Duration
	MaxOutput int64
}

// Error returns the text.
func (e *Error) Error() string {
	var text string
	switch {
	case errors.Is(e.Cause, ErrNotAllowed) && e.Option != "":
		return fmt.Sprintf("git: the option %s is not one the agent may pass to %s", quote(e.Option), e.Subcommand)
	case errors.Is(e.Cause, ErrNotAllowed) && e.Refspec != "":
		return fmt.Sprintf("git: the refspec %s is not one the agent may push: it has to name a branch, and nothing that forces, "+
			"deletes or matches many", quote(e.Refspec))
	case errors.Is(e.Cause, ErrNotAllowed):
		return fmt.Sprintf("git: the subcommand %s is not one the agent may run", quote(e.Subcommand))
	case errors.Is(e.Cause, ErrGitDirUntrusted):
		return fmt.Sprintf("git %s was not run: %v", e.Subcommand, e.Cause)
	case errors.Is(e.Cause, ErrTimeout):
		text = fmt.Sprintf("git %s did not finish within %s and was stopped", e.Subcommand, e.Timeout)
	case errors.Is(e.Cause, ErrCanceled):
		text = fmt.Sprintf("git %s was stopped because its caller gave up", e.Subcommand)
	case errors.Is(e.Cause, ErrOutputLimit):
		text = fmt.Sprintf("git %s wrote more than %d bytes to its output and was stopped", e.Subcommand, e.MaxOutput)
	case e.ExitCode != 0:
		text = fmt.Sprintf("git %s failed with exit status %d", e.Subcommand, e.ExitCode)
	default:
		text = fmt.Sprintf("git %s could not run: %v", e.Subcommand, e.Cause)
	}
	if e.Stderr != "" {
		text += ": " + e.Stderr
	}
	return text
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is makes errors.Is(err, syscall.ENOSPC) true of a command that said the volume
// was full, and the same for the other errors of volumeErrnos.
func (e *Error) Is(target error) bool { return e.Errno != 0 && target == e.Errno }

// volumeErrnos are the errors of the system that say that the volume cannot be
// written to: it is full, over its quota or read-only. Git names them in its
// standard error in the words of strerror, which LC_ALL=C (config.go) makes the
// ones that Go uses, but for the case of the first letter.
var volumeErrnos = []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT, syscall.EROFS}

// VolumeRefused reports whether err says that the volume could not be written to,
// because it is full, over its quota or read-only: an error of the system, or an
// Error of a command that said so.
func VolumeRefused(err error) bool {
	for _, errno := range volumeErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// volumeErrno is the one of volumeErrnos that the standard error of a command
// names, or zero. Only the lines in which Git says why it failed are read, the
// ones it starts with "error:" or "fatal:": a warning of ssh, such as one that
// it cannot make a directory in a home on a read-only file system, comes with
// every connection and says nothing of why the command failed. A line that Git
// starts with "remote:" was sent by the repository, and is of its volume and not
// of this one.
func volumeErrno(stderr string) syscall.Errno {
	for line := range strings.Lines(stderr) {
		if !strings.HasPrefix(line, "error:") && !strings.HasPrefix(line, "fatal:") {
			continue
		}
		line = strings.ToLower(line)
		for _, errno := range volumeErrnos {
			if strings.Contains(line, errno.Error()) {
				return errno
			}
		}
	}
	return 0
}

// quote shows an argument that is not a known subcommand, shortened, because a
// caller's mistake should not put a long string into a log.
func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return strconv.Quote(s)
}
