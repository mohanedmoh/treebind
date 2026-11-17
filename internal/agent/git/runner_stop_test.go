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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A command that is stopped is told to end first, so that Git can remove its own lock files, and is
// killed only when it does not (docs/adr/0028). What Git does when it is told is held by the tests of
// killed_test.go on the real Git. These use a stand-in for it, to show the order and the times.

func TestACommandThatIsStoppedIsToldToEndFirst(t *testing.T) {
	root := tempDir(t)
	lock, noted, ready := filepath.Join(root, "lock"), filepath.Join(root, "noted"), filepath.Join(root, "ready")
	// The stand-in holds a "lock" and lets it go when it is told to end, which the shell of the image of the
	// agent can do with what it has built in: it has no rm.
	r := fakeGit(t, root, fmt.Sprintf(`trap 'echo term > "%[2]s"; echo released > "%[1]s"; exit 143' TERM
echo held > "%[1]s"
echo ready > "%[3]s"
while :; do :; done`, lock, noted, ready))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, cmdStatus)
		done <- err
	}()
	eventually(t, "the command to hold its lock", func() bool { _, err := os.Stat(ready); return err == nil })
	start := time.Now()
	cancel()

	err := <-done
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want a cancellation", err)
	}
	if _, statErr := os.Stat(noted); statErr != nil {
		t.Error("the command was not told to end: it was killed, and a command that is killed cannot remove its lock")
	}
	if held, _ := os.ReadFile(lock); string(held) != "released\n" {
		t.Errorf("the lock is %q, want it released: the command did not get to let it go", held)
	}
	if elapsed := time.Since(start); elapsed > termGrace {
		t.Errorf("the call took %s, and a command that ends when it is told to does not wait for the grace of %s", elapsed, termGrace)
	}
}

func TestACommandThatDoesNotEndWhenToldIsKilledAfterTheGraceWithWhatItStarted(t *testing.T) {
	root := tempDir(t)
	pidFile := filepath.Join(root, "pid")
	r := fakeGit(t, root, fmt.Sprintf("trap '' TERM\n(trap '' TERM; while :; do :; done) &\necho $! > '%s'\nwhile :; do :; done", pidFile))

	start := time.Now()
	_, err := r.RunTimeout(context.Background(), 200*time.Millisecond, cmdStatus)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want a timeout", err)
	}
	// It ignored the signal that tells it to end, so the call waited for the grace and then killed the group.
	if elapsed < termGrace {
		t.Errorf("the call took %s: it did not wait %s for a command to end before it killed it", elapsed, termGrace)
	}
	if limit := termGrace + killGrace + 5*time.Second; elapsed > limit {
		t.Errorf("the call took %s, want it over within %s", elapsed, limit)
	}
	eventually(t, "the child of the command to be killed", func() bool { return gone(readPID(t, pidFile)) })
}
