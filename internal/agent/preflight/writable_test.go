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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// workMountPoint is the directory the tests report on.
const workMountPoint = "/work"

// wantResult fails the test unless the result is for the check, with the outcome
// and the reason.
func wantResult(t *testing.T, got Result, check string, outcome Outcome, reason string) {
	t.Helper()
	if got.Check != check || got.Outcome != outcome || got.Reason != reason {
		t.Errorf("got %s %s %q (%s), want %s %s %q",
			got.Check, got.Outcome, got.Reason, got.Message, check, outcome, reason)
	}
	if got.Message == "" {
		t.Errorf("the %s result has no message", got.Check)
	}
}

func TestCheckWritable(t *testing.T) {
	t.Run("a writable directory passes and keeps nothing behind", func(t *testing.T) {
		dir := t.TempDir()

		result := checkWritable(dir)

		wantResult(t, result, CheckWritable, Passed, "")
		if want := fmt.Sprintf("uid %d", os.Geteuid()); !strings.Contains(result.Message, want) {
			t.Errorf("the message does not name the user (%s): %s", want, result.Message)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the check left %d entries in the directory", len(entries))
		}
	})

	t.Run("a directory the user cannot write to is not writable, and the message says why", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}

		result := checkWritable(dir)

		wantResult(t, result, CheckWritable, Failed, ReasonNotWritable)
		for _, want := range []string{
			fmt.Sprintf("uid %d", os.Geteuid()),
			fmt.Sprintf("gid %d", os.Getegid()),
			fmt.Sprintf("owned by %d:%d with mode 0555", os.Geteuid(), os.Getegid()),
			"create a file in",
			docsURL,
		} {
			if !strings.Contains(result.Message, want) {
				t.Errorf("the message does not contain %q: %s", want, result.Message)
			}
		}
	})

	t.Run("a directory that is missing fails with the system's error", func(t *testing.T) {
		result := checkWritable(filepath.Join(t.TempDir(), "missing"))

		wantResult(t, result, CheckWritable, Failed, ReasonWriteFailed)
		if !strings.Contains(result.Message, "no such file or directory") {
			t.Errorf("the message does not carry the error: %s", result.Message)
		}
	})
}

func TestWriteOutcome(t *testing.T) {
	full := &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	refused := &os.PathError{Op: "close", Path: "x", Err: syscall.EACCES}
	immutable := &os.PathError{Op: "remove", Path: "x", Err: syscall.EPERM}

	tests := []struct {
		name                          string
		writeErr, closeErr, removeErr error
		wantOutcome                   Outcome
		wantReason, wantAction        string
	}{
		{name: "no error", wantOutcome: Passed},
		{"a full volume fails the write", full, nil, nil, Failed, ReasonWriteFailed, "write to a file in"},
		{"a refusal that only shows when the file closes", nil, refused, nil, Failed, ReasonNotWritable, "close a file in"},
		{"a file that cannot be removed", nil, nil, immutable, Failed, ReasonWriteFailed, "remove a file from"},
		{"the first failing step decides", full, refused, immutable, Failed, ReasonWriteFailed, "write to a file in"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := writeOutcome(workMountPoint, test.writeErr, test.closeErr, test.removeErr)

			wantResult(t, result, CheckWritable, test.wantOutcome, test.wantReason)
			if !strings.Contains(result.Message, test.wantAction) {
				t.Errorf("the message %q does not name the step %q", result.Message, test.wantAction)
			}
		})
	}
}

func TestWriteFailure(t *testing.T) {
	const dir = workMountPoint
	pathError := func(errno syscall.Errno) error {
		return &os.PathError{Op: "open", Path: dir + "/.treebind-preflight-1", Err: errno}
	}

	tests := []struct {
		name       string
		err        error
		wantReason string
		wantInText string
	}{
		{"a read-only file system", pathError(syscall.EROFS), ReasonReadOnlyVolume, "read-only volume"},
		{"an access error", pathError(syscall.EACCES), ReasonNotWritable, "cannot create a file in /work"},
		{"an operation not permitted", pathError(syscall.EPERM), ReasonWriteFailed, "operation not permitted"},
		{"a full volume", pathError(syscall.ENOSPC), ReasonWriteFailed, "no space left on device"},
		{"a quota", pathError(syscall.EDQUOT), ReasonWriteFailed, "quota exceeded"},
		{"an input or output error", pathError(syscall.EIO), ReasonWriteFailed, "input/output error"},
		{"an error that is wrapped", fmt.Errorf("closing: %w", pathError(syscall.EROFS)), ReasonReadOnlyVolume, "read-only volume"},
		{"an error of another kind", errors.New("the disk is on fire"), ReasonWriteFailed, "the disk is on fire"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := writeFailure(dir, "create a file in", test.err)

			wantResult(t, result, CheckWritable, Failed, test.wantReason)
			if !strings.Contains(result.Message, test.wantInText) {
				t.Errorf("the message %q does not contain %q", result.Message, test.wantInText)
			}
		})
	}
}
