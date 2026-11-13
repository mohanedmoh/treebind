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
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVolume(t *testing.T) {
	t.Run("both checks run on a mounted, writable volume", func(t *testing.T) {
		dir := tempDir(t)

		results := volume(context.Background(), dir, mountTable(t, rootMountLine, mountLine(dir, "rw", fsExt4)))

		if len(results) != 2 {
			t.Fatalf("got %d results, want 2: %+v", len(results), results)
		}
		wantResult(t, results[0], CheckMount, Passed, "")
		wantResult(t, results[1], CheckWritable, Passed, "")
	})

	t.Run("a failed mount check skips the writable one and says which failure", func(t *testing.T) {
		results := volume(context.Background(), tempDir(t), mountTable(t, rootMountLine))

		if len(results) != 2 {
			t.Fatalf("got %d results, want 2: %+v", len(results), results)
		}
		wantResult(t, results[0], CheckMount, Failed, ReasonNotMounted)
		wantResult(t, results[1], CheckWritable, Skipped, ReasonNotChecked)
		if !strings.Contains(results[1].Message, ReasonNotMounted) {
			t.Errorf("the skipped check does not name the failure that caused it: %s", results[1].Message)
		}
	})

	t.Run("a mount check that cannot tell still lets the writable one run", func(t *testing.T) {
		results := volume(context.Background(), tempDir(t), noMountTable(t))

		wantResult(t, results[0], CheckMount, Skipped, ReasonNotChecked)
		wantResult(t, results[1], CheckWritable, Passed, "")
	})

	t.Run("a mounted volume that cannot be written to fails the writable check", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir := tempDir(t)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}

		results := volume(context.Background(), dir, mountTable(t, rootMountLine, mountLine(dir, "rw", fsExt4)))

		wantResult(t, results[0], CheckMount, Passed, "")
		wantResult(t, results[1], CheckWritable, Failed, ReasonNotWritable)
	})
}

func TestRun(t *testing.T) {
	t.Run("returns what the check returns", func(t *testing.T) {
		want := Result{Check: "X", Outcome: Passed, Message: "fine"}

		got := run(context.Background(), "X", func() Result { return want })

		if got != want {
			t.Errorf("run returned %+v, want %+v", got, want)
		}
	})

	t.Run("gives up on a volume that does not answer in time", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		started := time.Now()
		got := run(ctx, "X", func() Result {
			<-release
			return Result{Check: "X", Outcome: Passed, Message: "too late"}
		})

		wantResult(t, got, "X", Failed, ReasonTimedOut)
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("run waited %s for a check that hangs", elapsed)
		}
	})

	t.Run("keeps the result of a check that finished as the time ran out", func(t *testing.T) {
		want := Result{Check: "X", Outcome: Passed, Message: "just in time"}
		done := make(chan Result, 1)
		done <- want
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()

		// Both the result and the end of the context are ready. Whichever select
		// would pick, the finished check keeps its result.
		for range 100 {
			if got := await(ctx, "X", done); got != want {
				t.Fatalf("await returned %+v, want %+v", got, want)
			}
			done <- want
		}
	})

	t.Run("skips a check that is interrupted, which is not a failure of the volume", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		got := run(ctx, "X", func() Result {
			<-release
			return Result{Check: "X", Outcome: Passed, Message: "too late"}
		})

		wantResult(t, got, "X", Skipped, ReasonNotChecked)
	})
}

func TestAnyFailed(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    bool
	}{
		{"no results", nil, false},
		{"all passed", []Result{{Outcome: Passed}, {Outcome: Passed}}, false},
		{"one skipped", []Result{{Outcome: Passed}, {Outcome: Skipped}}, false},
		{"one failed", []Result{{Outcome: Passed}, {Outcome: Failed}}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AnyFailed(test.results); got != test.want {
				t.Errorf("AnyFailed = %v, want %v", got, test.want)
			}
		})
	}
}
