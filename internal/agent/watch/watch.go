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

// Package watch tells the agent that something in the synced directory may have changed, so that it
// scans sooner than its interval (FR-3, R-11).
//
// An event never says what changed and is not trusted to say that nothing did. The watcher gives one kind
// of signal, "look at the directory", and the scan of package scan decides. A missed event, which is what
// R-11 is about, delays a commit by up to one scan interval and never loses it. On a network file system
// another client's writes make no event at all, and there the scan is the only thing that notices them.
//
// There is one inotify watch for each directory, because inotify does not look into the directories of the
// one it watches. A directory that appears is watched before the signal for it is given, so that a file
// that is written after the signal is not missed, and the files that were made in it before it was watched
// are the scan's. The watcher says when it cannot do all of it: when the limit of the kernel on watches
// is reached, when a directory cannot be watched, and when events were lost. In each case the answer is a
// scan, and Stats says what the report should say. A directory that is renamed can lose its watch without
// a word, which Refresh, called at every scan, puts right.
package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// Options say what to watch.
type Options struct {
	// Root is the synced directory. It is absolute, and it is a directory.
	Root string
	// Skip says whether a directory, given relative to Root with slashes, and everything below it is not
	// watched: the state directory and the directories of Git. It is also asked about the path of an event,
	// which is never a path below one that it skipped. It is not asked about Root. A nil Skip skips nothing.
	Skip func(dir string) bool
}

// Stats say how much of the directory is watched, for the report.
type Stats struct {
	// Dirs is how many directories are watched.
	Dirs int
	// Unwatched is how many directories could not be, and Reason says why one of them could not: the limit
	// of the kernel on watches, or what the system said. Reason is empty when Unwatched is zero.
	Unwatched int
	Reason    string
	// Lost is how many times events may have been lost since the watcher began.
	Lost int
}

// source is what the kernel's watches are used through. The fsnotify.Watcher is one; a test gives another
// to make happen what cannot be made to on a real one: the limit on watches, and a queue that overflows.
type source interface {
	Add(name string) error
	WatchList() []string
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

type kernel struct{ *fsnotify.Watcher }

func (k kernel) Events() <-chan fsnotify.Event { return k.Watcher.Events }
func (k kernel) Errors() <-chan error          { return k.Watcher.Errors }

// Watcher watches a directory. Close it to stop.
type Watcher struct {
	root string
	skip func(dir string) bool
	src  source
	out  chan struct{}
	wg   sync.WaitGroup

	mu        sync.Mutex
	unwatched map[string]struct{}
	reason    string
	lost      int
}

// New watches the directory and returns at once, with the directories that exist watched. An error is a
// watcher that cannot be made at all, which is the limit on instances of the kernel (fs.inotify.max_user_instances)
// or a root that is not a directory: the agent then scans only.
func New(o Options) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: the watcher of the kernel cannot be made: %w", err)
	}
	return newWith(kernel{fw}, o)
}

func newWith(src source, o Options) (*Watcher, error) {
	root, err := realDirectory(o.Root)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	w := &Watcher{
		root: root, skip: o.Skip, src: src, unwatched: map[string]struct{}{}, out: make(chan struct{}, 1),
	}
	w.addTree(root)
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// realDirectory returns the root with its links resolved, so that the paths of events are under it.
func realDirectory(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("watch: the root %q is not an absolute path", root)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("watch: the root cannot be looked at: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("watch: the root cannot be looked at: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("watch: the root %q is not a directory", root)
	}
	return real, nil
}

// Events delivers a signal when something in the directory may have changed. Signals that nobody has taken
// are one, so a program that writes a great deal makes a few. The channel is closed when the watcher is.
func (w *Watcher) Events() <-chan struct{} { return w.out }

// Refresh watches the directories that have no watch: a directory whose creation was not told, one that was
// moved in or renamed, and one that could not be watched while the kernel's limit was reached. The kernel
// takes the watch of a directory that is renamed, and the watch that is made for its new name at the
// same moment can go with it, so that nothing tells the watcher that the directory is not watched. The
// agent calls Refresh at every scan, which makes what a rename costs the same as what a missed event
// costs: up to one scan interval. It does not signal.
func (w *Watcher) Refresh() {
	// What could not be watched is counted again from nothing: a directory that was removed since is not one
	// that cannot be watched, and the walk notes the ones that still cannot.
	w.mu.Lock()
	w.unwatched = map[string]struct{}{}
	w.reason = ""
	w.mu.Unlock()
	w.addTree(w.root)
}

// Stats returns how much is watched.
func (w *Watcher) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Stats{Dirs: len(w.src.WatchList()), Unwatched: len(w.unwatched), Reason: w.reason, Lost: w.lost}
}

// Close stops the watcher and ends its signals, which it has when it returns. It can be called again.
func (w *Watcher) Close() error {
	err := w.src.Close()
	w.wg.Wait()
	return err
}

func (w *Watcher) run() {
	defer w.wg.Done()
	defer close(w.out)
	for {
		select {
		case event, ok := <-w.src.Events():
			if !ok {
				return
			}
			w.handle(event)
		case err, ok := <-w.src.Errors():
			if !ok {
				return
			}
			w.lose(err)
		}
	}
}

// handle gives the signal for an event, after the watches that it makes necessary.
func (w *Watcher) handle(event fsnotify.Event) {
	if dir, ok := w.relative(event.Name); ok && w.skipped(dir) {
		return
	}
	if event.Has(fsnotify.Create) {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			w.addTree(event.Name)
		}
	}
	w.signal()
}

// lose is a signal to look at everything: the events that were lost may have been the creation of a
// directory, which then has no watch, so every directory is looked at again, and the ones that could not be
// watched are tried again.
func (w *Watcher) lose(error) {
	w.mu.Lock()
	w.lost++
	w.mu.Unlock()
	w.Refresh()
	w.signal()
}

func (w *Watcher) signal() {
	select {
	case w.out <- struct{}{}:
	default:
	}
}

// relative returns the path of an event relative to the root, with slashes, and false for a path that is not
// below it.
func (w *Watcher) relative(path string) (string, bool) {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == "." || rel == ".." || len(rel) > 2 && rel[:3] == "../" {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (w *Watcher) skipped(dir string) bool { return w.skip != nil && w.skip(dir) }

// addTree watches a directory and every directory below it that is not skipped. A link is not followed.
// Directories that cannot be watched are noted, and are tried again by the next call that reaches them.
func (w *Watcher) addTree(top string) {
	_ = filepath.WalkDir(top, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory whose entries cannot be read has had its watch refused, which is noted, or is gone.
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if dir, ok := w.relative(path); ok && w.skipped(dir) {
			return filepath.SkipDir
		}
		w.noted(path, w.src.Add(path))
		return nil
	})
}

// noted records the result of watching a directory.
func (w *Watcher) noted(path string, err error) {
	if err != nil {
		w.unwatch(path, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.unwatched, path)
	if len(w.unwatched) == 0 {
		w.reason = ""
	}
}

func (w *Watcher) unwatch(path string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reason = why(err)
	w.unwatched[path] = struct{}{}
}

// why says what stopped a directory from being watched, in the words that a person looks for.
func why(err error) string {
	if errors.Is(err, syscall.ENOSPC) {
		return "the limit of the kernel on watches (fs.inotify.max_user_watches) is reached"
	}
	return err.Error()
}
