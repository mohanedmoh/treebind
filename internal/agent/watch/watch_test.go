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

package watch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// patient is how long a test waits for a signal that has to come.
	patient = 5 * time.Second
	// quietly is how long a test waits to see that a signal does not come. A signal that comes later than
	// this is one that no test can tell from one that does not come.
	quietly = 300 * time.Millisecond
)

func skipGit(dir string) bool {
	return dir == ".treebind" || filepath.Base(dir) == ".git"
}

func start(t *testing.T, root string) *Watcher {
	t.Helper()
	w, err := New(Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// signalled reports whether a signal comes within d.
func signalled(w *Watcher, d time.Duration) bool {
	select {
	case _, ok := <-w.Events():
		return ok
	case <-time.After(d):
		return false
	}
}

// drain takes the signals that are there, and the ones that come from what was done a moment ago.
func drain(w *Watcher) {
	for signalled(w, quietly) {
	}
}

func wantSignal(t *testing.T, w *Watcher, what string) {
	t.Helper()
	if !signalled(w, patient) {
		t.Fatalf("no signal for %s", what)
	}
}

func wantNoSignal(t *testing.T, w *Watcher, what string) {
	t.Helper()
	if signalled(w, quietly) {
		t.Fatalf("a signal for %s", what)
	}
}

func TestAnEventOfTheFileSystemIsASignal(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "existing.txt"), "1")
	write(t, filepath.Join(root, "sub", "inner.txt"), "1")
	w := start(t, root)
	drain(w)

	steps := []struct {
		what string
		do   func()
	}{
		{"a file that is created", func() { write(t, filepath.Join(root, "new.txt"), "1") }},
		{"a file that is written", func() { write(t, filepath.Join(root, "existing.txt"), "22") }},
		{"a file that is renamed", func() { rename(t, filepath.Join(root, "existing.txt"), filepath.Join(root, "renamed.txt")) }},
		{"a file whose mode changes", func() { chmod(t, filepath.Join(root, "renamed.txt"), 0o755) }},
		{"a file that is removed", func() { remove(t, filepath.Join(root, "renamed.txt")) }},
		{"a file in a directory", func() { write(t, filepath.Join(root, "sub", "inner.txt"), "22") }},
		{"a file that is created in a directory", func() { write(t, filepath.Join(root, "sub", "more.txt"), "1") }},
	}
	for _, step := range steps {
		step.do()
		wantSignal(t, w, step.what)
		drain(w)
	}
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func TestNothingHappeningIsNoSignal(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "1")
	w := start(t, root)
	drain(w)
	wantNoSignal(t, w, "nothing")
}

func TestDirectoriesThatAppearAreWatchedBeforeTheSignalOfTheirCreation(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)
	drain(w)

	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	wantSignal(t, w, "directories that are made")
	drain(w)

	// The watch of the deepest directory exists once the signal for it has been given, so that a file
	// written after the signal is not missed. The write is repeated, because the signals that were
	// given while the directories were made do not say which of them were watched yet.
	deadline := time.Now().Add(patient)
	for i := 0; ; i++ {
		write(t, filepath.Join(deep, "f.txt"), strings.Repeat("x", i+1))
		if signalled(w, 200*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no signal for a file in a directory that was made after the watch began")
		}
	}
}

func TestADirectoryThatIsMovedInWithFilesInItIsWatchedAndSignalled(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "moved", "deeper", "x.txt"), "1")
	w := start(t, root)
	drain(w)

	rename(t, filepath.Join(outside, "moved"), filepath.Join(root, "moved"))
	wantSignal(t, w, "a directory that is moved in")
	drain(w)

	deadline := time.Now().Add(patient)
	for i := 0; ; i++ {
		write(t, filepath.Join(root, "moved", "deeper", "y.txt"), strings.Repeat("y", i+1))
		if signalled(w, 200*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no signal for a file in a directory that was moved in with its directories")
		}
	}
}

func TestADirectoryThatIsRenamedIsWatchedUnderItsNewNameAfterARefresh(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "old", "x.txt"), "1")
	w := start(t, root)
	drain(w)

	rename(t, filepath.Join(root, "old"), filepath.Join(root, "new"))
	wantSignal(t, w, "a directory that is renamed")
	drain(w)
	// The kernel takes the watch of a directory that is renamed, and the one that was made for the new
	// name at the same moment can go with it. Refresh, which the agent calls at every scan, makes sure.
	w.Refresh()

	deadline := time.Now().Add(patient)
	for i := 0; ; i++ {
		write(t, filepath.Join(root, "new", "x.txt"), strings.Repeat("z", i+1))
		if signalled(w, 200*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no signal for a file in a directory under its new name")
		}
	}
}

func TestADirectoryThatIsRemovedAndMadeAgainIsWatchedAgain(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "d", "x.txt"), "1")
	w := start(t, root)
	drain(w)

	remove(t, filepath.Join(root, "d"))
	wantSignal(t, w, "a directory that is removed")
	drain(w)
	write(t, filepath.Join(root, "d", "x.txt"), "2")
	wantSignal(t, w, "a directory that is made again")
	drain(w)

	deadline := time.Now().Add(patient)
	for i := 0; ; i++ {
		write(t, filepath.Join(root, "d", "x.txt"), strings.Repeat("q", i+3))
		if signalled(w, 200*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no signal for a file in a directory that was removed and made again")
		}
	}
}

func TestDirectoriesThatAreSkippedAreNotWatched(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".treebind", "git", "objects", "x"), "1")
	write(t, filepath.Join(root, "sub", ".git", "HEAD"), "1")
	write(t, filepath.Join(root, "kept", "x.txt"), "1")
	w := start(t, root)
	drain(w)

	write(t, filepath.Join(root, ".treebind", "git", "objects", "y"), "1")
	write(t, filepath.Join(root, "sub", ".git", "HEAD"), "2")
	wantNoSignal(t, w, "files of the state directory and of a .git directory")

	// Nor when they appear later.
	write(t, filepath.Join(root, "other", ".git", "HEAD"), "1")
	wantSignal(t, w, "the directory other, which is not skipped, and is made")
	drain(w)
	write(t, filepath.Join(root, "other", ".git", "HEAD"), "2")
	wantNoSignal(t, w, "a .git directory that was made after the watch began")

	write(t, filepath.Join(root, "kept", "x.txt"), "2")
	wantSignal(t, w, "a file of a directory that is not skipped")
}

func TestALinkToADirectoryIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "x.txt"), "1")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	w := start(t, root)
	drain(w)

	write(t, filepath.Join(outside, "x.txt"), "2")
	wantNoSignal(t, w, "a file outside the directory, reached through a link")
	if got := w.Stats().Dirs; got != 1 {
		t.Errorf("directories watched = %d, want 1: only the root, and no link followed", got)
	}
}

func TestManyEventsAreFewSignalsAndNeverHoldBackTheWriter(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)
	drain(w)

	done := make(chan error, 1)
	go func() {
		for i := range 2000 {
			if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Repeat("x", i%7+1)), 0o644); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the writes were held back: a watcher that nobody reads blocks the program that writes")
	}
	wantSignal(t, w, "two thousand writes")
	got := 1
	for signalled(w, quietly) {
		got++
	}
	if got > 10 {
		t.Errorf("signals = %d for 2000 writes, want them to be few: the signals that nobody has read are one", got)
	}
}

func TestAClosedWatcherEndsItsSignalsAndCanBeClosedAgain(t *testing.T) {
	root := t.TempDir()
	w, err := New(Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The signals have ended when Close returns, and not only soon after.
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("a signal from a closed watcher")
		}
	default:
		t.Fatal("the signals of a closed watcher had not ended when Close returned")
	}
	if err := w.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

func TestCloseDoesNotReturnWhileTheLoopIsHandlingAnEvent(t *testing.T) {
	root := t.TempDir()
	fake := newFake(100, nil)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fake.mu.Lock()
	fake.gate, fake.entered = gate, entered
	fake.mu.Unlock()
	// The loop is in the middle of watching a new directory when Close is called.
	write(t, filepath.Join(root, "late", "x.txt"), "1")
	fake.events <- fsnotify.Event{Name: filepath.Join(root, "late"), Op: fsnotify.Create}
	select {
	case <-entered:
	case <-time.After(patient):
		t.Fatal("the loop did not begin to watch the new directory")
	}

	closed := make(chan struct{})
	go func() {
		_ = w.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while the loop was still handling an event: the signals of a closed watcher do not end with it")
	case <-time.After(quietly):
	}
	close(gate)
	select {
	case <-closed:
	case <-time.After(patient):
		t.Fatal("Close did not return after the loop ended")
	}
}

func TestNewRefusesARootThatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := New(Options{Root: filepath.Join(root, "missing"), Skip: skipGit}); err == nil {
		t.Error("New with a root that is not there succeeded")
	}
	file := filepath.Join(root, "file")
	write(t, file, "1")
	if _, err := New(Options{Root: file, Skip: skipGit}); err == nil {
		t.Error("New with a root that is a file succeeded")
	}
	if _, err := New(Options{Root: "relative/path", Skip: skipGit}); err == nil {
		t.Error("New with a root that is not absolute succeeded")
	}
	// A relative root that is there: the paths of the events would not be under it.
	t.Chdir(root)
	if err := os.Mkdir("here", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Root: "here", Skip: skipGit}); err == nil {
		t.Error("New with a relative root that is there succeeded")
	}
}

func TestTheSkipFunctionIsAskedWithPathsRelativeToTheRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "b", "x.txt"), "1")
	write(t, filepath.Join(root, "c", "y.txt"), "1")
	var mu sync.Mutex
	var asked []string
	w, err := New(Options{Root: root, Skip: func(dir string) bool {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, dir)
		return false
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"a", "a/b", "c"}
	if strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Errorf("asked about %q, want %q: the root is not asked about, and the paths have slashes and no root", asked, want)
	}
	if got := w.Stats().Dirs; got != 4 {
		t.Errorf("directories watched = %d, want 4 (the root, a, a/b and c)", got)
	}
}

// fakeSource stands in for the kernel's watches, for what cannot be made to happen on a real one: that
// the limit on watches is reached, and that events are lost.
type fakeSource struct {
	events chan fsnotify.Event
	errors chan error
	// addErr is returned by Add after the first okay watches.
	okay   int
	addErr error

	mu     sync.Mutex
	added  []string
	closed bool
	// gate, when it is set, holds Add until it is closed, and entered says that an Add is held.
	gate    chan struct{}
	entered chan struct{}
}

func newFake(okay int, addErr error) *fakeSource {
	return &fakeSource{events: make(chan fsnotify.Event, 8), errors: make(chan error, 8), okay: okay, addErr: addErr}
}

func (f *fakeSource) Add(name string) error {
	f.mu.Lock()
	gate, entered := f.gate, f.entered
	f.mu.Unlock()
	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.added, name) {
		return nil
	}
	if len(f.added) >= f.okay && f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, name)
	return nil
}

func (f *fakeSource) WatchList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added)
}

func (f *fakeSource) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeSource) Errors() <-chan error          { return f.errors }

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
		close(f.errors)
	}
	return nil
}

func TestTheLimitOnWatchesIsSaidAndWhatIsWatchedStillSignals(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		write(t, filepath.Join(root, name, "x.txt"), "1")
	}
	fake := newFake(2, &os.PathError{Op: "inotify_add_watch", Path: root, Err: syscall.ENOSPC})
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatalf("newWith: %v", err)
	}
	defer func() { _ = w.Close() }()

	s := w.Stats()
	if s.Dirs != 2 || s.Unwatched != 4 {
		t.Errorf("stats = %+v, want 2 directories watched (the root and a) and 4 that could not be", s)
	}
	if !strings.Contains(s.Reason, "fs.inotify.max_user_watches") {
		t.Errorf("reason = %q, want it to name fs.inotify.max_user_watches", s.Reason)
	}
	fake.events <- fsnotify.Event{Name: filepath.Join(root, "a", "x.txt"), Op: fsnotify.Write}
	wantSignal(t, w, "an event of a directory that is watched")
}

func TestAWatcherThatNobodyReadsStillWatchesTheDirectoriesThatAppear(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)
	// Many different events, and no one takes a signal. A watcher that waited for someone to take one
	// would stop where it is, with the watches of the directories that appear after it unmade.
	for i := range 50 {
		write(t, filepath.Join(root, strings.Repeat("f", i+1)), "x")
	}
	write(t, filepath.Join(root, "late", "x.txt"), "1")
	deadline := time.Now().Add(patient)
	for w.Stats().Dirs != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("directories watched = %d, want 2 (the root and late) although nobody took a signal", w.Stats().Dirs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestADirectoryThatCannotBeReadIsSaidAndTheRestIsWatched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "open", "x.txt"), "1")
	write(t, filepath.Join(root, "locked", "x.txt"), "1")
	if err := os.Chmod(filepath.Join(root, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })
	w := start(t, root)

	s := w.Stats()
	if s.Dirs != 2 || s.Unwatched != 1 || !strings.Contains(s.Reason, "permission denied") {
		t.Errorf("stats = %+v, want the root and open watched, and locked said to be refused", s)
	}
}

func TestAnotherFailureToWatchIsSaidWithWhatTheSystemSaid(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "x.txt"), "1")
	fake := newFake(1, &os.PathError{Op: "inotify_add_watch", Path: root, Err: syscall.EACCES})
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	s := w.Stats()
	if s.Unwatched != 1 || !strings.Contains(s.Reason, "permission denied") || strings.Contains(s.Reason, "max_user_watches") {
		t.Errorf("stats = %+v, want one directory that could not be watched, with what the system said", s)
	}
}

func TestLostEventsAreASignalToLookAtEverythingAndAreCounted(t *testing.T) {
	root := t.TempDir()
	fake := newFake(100, nil)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	fake.errors <- fsnotify.ErrEventOverflow
	wantSignal(t, w, "an overflow of the queue of events")
	fake.errors <- errors.New("the watcher failed")
	wantSignal(t, w, "an error of the watcher")
	if got := w.Stats().Lost; got != 2 {
		t.Errorf("lost = %d, want 2: an overflow and an error are both events that may be lost", got)
	}
}

func TestRefreshWatchesTheDirectoriesThatHaveNoWatchAndSaysNothing(t *testing.T) {
	root := t.TempDir()
	fake := newFake(100, nil)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	write(t, filepath.Join(root, "late", "x.txt"), "1")
	write(t, filepath.Join(root, ".git", "HEAD"), "1")
	w.Refresh()
	if got := w.Stats().Dirs; got != 2 {
		t.Errorf("directories watched = %d, want 2: the root and late, and not .git", got)
	}
	wantNoSignal(t, w, "a refresh, which is not an event")
}

func TestADirectoryThatCouldNotBeWatchedAndIsRemovedIsNotCountedAnyMore(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "short-lived", "x.txt"), "1")
	fake := newFake(1, syscall.ENOSPC)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if s := w.Stats(); s.Unwatched != 1 {
		t.Fatalf("stats = %+v, want the directory to be said not to be watched", s)
	}

	remove(t, filepath.Join(root, "short-lived"))
	w.Refresh()

	if s := w.Stats(); s.Unwatched != 0 || s.Reason != "" {
		t.Errorf("stats = %+v, want nothing said of a directory that is gone: it would stay for as long as the agent runs", s)
	}
}

func TestAfterLostEventsEveryDirectoryIsLookedAtAgainForNewOnes(t *testing.T) {
	root := t.TempDir()
	fake := newFake(100, nil)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	// A directory made while the queue overflowed: its creation was never told.
	write(t, filepath.Join(root, "late", "deep", "x.txt"), "1")
	if got := w.Stats().Dirs; got != 1 {
		t.Fatalf("directories watched = %d, want 1 before the overflow", got)
	}
	fake.errors <- fsnotify.ErrEventOverflow
	wantSignal(t, w, "an overflow")
	if got := w.Stats().Dirs; got != 3 {
		t.Errorf("directories watched = %d, want 3: the root, late and late/deep, which an overflow may have hidden", got)
	}
}

func TestAWatchThatCouldNotBeMadeIsNotCountedTwiceAndIsTriedAgainAfterLostEvents(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "x.txt"), "1")
	write(t, filepath.Join(root, "b", "x.txt"), "1")
	fake := newFake(1, syscall.ENOSPC)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if s := w.Stats(); s.Unwatched != 2 || s.Reason == "" {
		t.Fatalf("stats = %+v, want 2 directories that could not be watched", s)
	}

	fake.mu.Lock()
	fake.okay = 100 // room again, as when another program has let go of its watches
	fake.mu.Unlock()
	fake.errors <- fsnotify.ErrEventOverflow
	wantSignal(t, w, "an overflow")
	if s := w.Stats(); s.Unwatched != 0 || s.Reason != "" || s.Dirs != 3 {
		t.Errorf("stats = %+v, want every directory watched and nothing said about a limit that is not there", s)
	}
}

func TestAnEventOfASkippedDirectoryIsNotASignal(t *testing.T) {
	root := t.TempDir()
	fake := newFake(100, nil)
	w, err := newWith(fake, Options{Root: root, Skip: skipGit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	fake.events <- fsnotify.Event{Name: filepath.Join(root, ".git"), Op: fsnotify.Create}
	fake.events <- fsnotify.Event{Name: filepath.Join(root, ".treebind"), Op: fsnotify.Create}
	wantNoSignal(t, w, "the creation of directories that are skipped")
	fake.events <- fsnotify.Event{Name: filepath.Join(root, "x.txt"), Op: fsnotify.Create}
	wantSignal(t, w, "the creation of a file")
}
