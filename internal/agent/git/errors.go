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
	// Cause is one of the errors above, the *exec.ExitError of a command that
	// failed, or the error that kept it from starting.
	Cause error
	// ExitCode is the exit status of a command that failed, and zero otherwise.
	ExitCode int
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
	case errors.Is(e.Cause, ErrNotAllowed):
		return fmt.Sprintf("git: the subcommand %s is not one the agent may run", quote(e.Subcommand))
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

// quote shows an argument that is not a known subcommand, shortened, because a
// caller's mistake should not put a long string into a log.
func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return strconv.Quote(s)
}
