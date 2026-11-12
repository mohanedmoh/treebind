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
	"testing"
	"time"
)

func TestVolume(t *testing.T) {
	t.Run("a writable directory passes", func(t *testing.T) {
		results := Volume(context.Background(), t.TempDir())

		if len(results) != 1 {
			t.Fatalf("got %d results, want 1: %+v", len(results), results)
		}
		wantResult(t, results[0], CheckWritable, Passed, "")
	})

	t.Run("a directory that cannot be written to fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}

		results := Volume(context.Background(), dir)

		if len(results) != 1 {
			t.Fatalf("got %d results, want 1: %+v", len(results), results)
		}
		wantResult(t, results[0], CheckWritable, Failed, ReasonNotWritable)
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
