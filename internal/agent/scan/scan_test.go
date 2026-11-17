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

package scan

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
)

const (
	branchMain = "main"
	fileA      = "a.txt"
	fileB      = "b.txt"
	fileC      = "dir/c.txt"
	fileNew    = "new.txt"
	// pause is longer than the granularity of the file times of the kernel, which is a few
	// milliseconds, so that two writes in a test do not get the same time.
	pause = 30 * time.Millisecond
)

// world is a checkout of a repository with the files, a filter, and a Scanner.
type world struct {
	t       *testing.T
	f       *gittest.Fixture
	l       layout.Layout
	scanner *Scanner
}

type filters struct{ include, exclude []string }

func newWorld(t *testing.T, sub string, files map[string]string, with ...func(*filters)) *world {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, files)
	l := f.Layout(sub)
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	if err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	return &world{t: t, f: f, l: l, scanner: scannerOf(t, f, l, with...)}
}

func scannerOf(t *testing.T, f *gittest.Fixture, l layout.Layout, with ...func(*filters)) *Scanner {
	t.Helper()
	var fl filters
	for _, w := range with {
		w(&fl)
	}
	filter, err := paths.NewFilter(fl.include, fl.exclude, l.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Layout: l, Git: f.GitOptions(), Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) write(name, content string) {
	w.t.Helper()
	gittest.Write(w.t, w.l, name, content)
}

func (w *world) path(name string) string { return filepath.Join(w.l.WorkTree, name) }

func (w *world) scan() Result {
	w.t.Helper()
	got, err := w.scanner.Scan(context.Background())
	if err != nil {
		w.t.Fatalf("the scan failed: %v", err)
	}
	return got
}

// plain runs Git on the checkout without the Runner, as a person would, and returns what it said and
// whether it failed. The identity is given on the command line, because the checkout has none.
func (w *world) plain(args ...string) (string, error) {
	w.t.Helper()
	args = append([]string{"-c", "user.name=t", "-c", "user.email=t@example.org"}, args...)
	return w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree}, args...)
}

func (w *world) mustPlain(args ...string) {
	w.t.Helper()
	if out, err := w.plain(args...); err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// changes returns the changes by path, and fails the test when a path is there twice.
func changes(t *testing.T, got Result) map[string]Change {
	t.Helper()
	by := map[string]Change{}
	for _, c := range got.Changes {
		if _, dup := by[c.Path]; dup {
			t.Errorf("%q is listed twice", c.Path)
		}
		by[c.Path] = c
	}
	return by
}

func kinds(t *testing.T, got Result) map[string]Kind {
	t.Helper()
	by := map[string]Kind{}
	for path, c := range changes(t, got) {
		by[path] = c.Kind
	}
	return by
}

func wantKinds(t *testing.T, got Result, want map[string]Kind) {
	t.Helper()
	have := kinds(t, got)
	if len(have) != len(want) {
		t.Errorf("changes = %v, want %v", have, want)
		return
	}
	for path, kind := range want {
		if have[path] != kind {
			t.Errorf("changes = %v, want %v", have, want)
			return
		}
	}
}

var base = map[string]string{fileA: "one\n", fileB: "two\n", fileC: "three\n"}

func TestACleanCheckoutHasNoChanges(t *testing.T) {
	w := newWorld(t, "", base)
	got := w.scan()
	if len(got.Changes) != 0 || len(got.Left) != 0 {
		t.Errorf("a clean checkout: changes = %v, left out = %v, want none", got.Changes, got.Left)
	}
}

func TestEachKindOfChangeIsTold(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")               // added
	w.write("dir/deeper/d.txt", "deeper\n") // added, in a directory that is new
	w.write(fileA, "changed\n")             // modified
	if err := os.Remove(w.path(fileB)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(w.path(fileC), 0o755); err != nil { // mode only
		t.Fatal(err)
	}
	got := w.scan()
	wantKinds(t, got, map[string]Kind{
		fileNew:            Added,
		"dir/deeper/d.txt": Added,
		fileA:              Modified,
		fileB:              Deleted,
		fileC:              Modified,
	})
	if c := changes(t, got)[fileC]; c.Seen.Mode&0o111 == 0 {
		t.Errorf("the mode that was seen = %v, want the executable bits", c.Seen.Mode)
	}
}

func TestAFileThatBecomesALinkIsATypeChange(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.Remove(w.path(fileA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fileB, w.path(fileA)); err != nil {
		t.Fatal(err)
	}
	got := w.scan()
	wantKinds(t, got, map[string]Kind{fileA: TypeChanged})
	if c := changes(t, got)[fileA]; c.Seen.Mode&os.ModeSymlink == 0 {
		t.Errorf("seen mode = %v, want a link: a link is not followed", c.Seen.Mode)
	}
}

func TestAMovedFileIsOneRemovedAndOneAdded(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.Rename(w.path(fileA), w.path("moved.txt")); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, w.scan(), map[string]Kind{fileA: Deleted, "moved.txt": Added})
}

func TestAMoveThatWasStagedIsStillOneRemovedAndOneAdded(t *testing.T) {
	// A commit that was killed after it staged the files leaves a move in the index, which Git lists as
	// a rename, an entry of another kind, unless it is told not to detect them.
	w := newWorld(t, "", base)
	if err := os.Rename(w.path(fileA), w.path("moved.txt")); err != nil {
		t.Fatal(err)
	}
	w.mustPlain("add", "-A")
	wantKinds(t, w.scan(), map[string]Kind{fileA: Deleted, "moved.txt": Added})
}

func TestPathsComeBackAsTheyAre(t *testing.T) {
	w := newWorld(t, "", base)
	names := []string{"with space.txt", "new\nline.txt", "bytes\xff\xfe.txt", "dir/ünï.txt", `back\slash.txt`, "quote\".txt", "-dash.txt"}
	for _, name := range names {
		w.write(name, "x\n")
	}
	got := kinds(t, w.scan())
	for _, name := range names {
		if got[name] != Added {
			t.Errorf("%q is not listed as added, the changes are %q", name, got)
		}
	}
	if len(got) != len(names) {
		t.Errorf("changes = %q, want %q", got, names)
	}
}

func TestChangesAreInTheOrderOfTheirPaths(t *testing.T) {
	w := newWorld(t, "", base)
	// Git lists the files that it tracks before the ones that it does not, so the order of its list is not
	// the order of the paths.
	w.write(fileB, "changed\n")
	for _, name := range []string{"z.txt", "m/b.txt", "a.txt.new", "m/a.txt"} {
		w.write(name, "x\n")
	}
	found := w.scan().Changes
	order := make([]string, 0, len(found))
	for _, c := range found {
		order = append(order, c.Path)
	}
	if !slices.IsSorted(order) {
		t.Errorf("order = %q, want it sorted", order)
	}
}

func TestWhatTheFilterLeavesOutIsCountedAndNotListed(t *testing.T) {
	w := newWorld(t, "", map[string]string{fileA: "one\n", "build/out.bin": "1\n", ".gitignore": "*.log\n"},
		func(f *filters) { f.exclude = []string{"build/"} })
	w.write(".env", "TOKEN=1\n")    // secret by its name
	w.write("notes.swp", "swap\n")  // transient
	w.write("build/out.bin", "2\n") // tracked, changed, and excluded
	w.write("build/new.bin", "3\n") // untracked and excluded
	w.write("x.log", "hidden\n")    // hidden by .gitignore: Git does not list it
	w.write("kept.txt", "kept\n")   // synced
	if err := os.MkdirAll(w.path("vendor/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A repository of its own. It is made with no GIT_DIR in the environment, which would make git init
	// act on the Git directory of the checkout.
	w.f.Plain(w.path("vendor/lib"), "init", "-q")
	w.write("vendor/lib/x.txt", "x\n")

	got := w.scan()
	wantKinds(t, got, map[string]Kind{"kept.txt": Added})
	want := map[paths.Reason]int{paths.Secret: 1, paths.Transient: 1, paths.Excluded: 2, paths.NestedRepository: 1}
	if len(got.Left) != len(want) {
		t.Fatalf("left out = %v, want %v", got.Left, want)
	}
	for reason, n := range want {
		if got.Left[reason] != n {
			t.Errorf("left out = %v, want %v", got.Left, want)
			break
		}
	}
}

func TestTheStateDirectoryInTheWorkTreeIsNotWalked(t *testing.T) {
	// With the whole claim synced the Git directory is in the work tree, and Git lists every file of it as
	// an untracked file unless it is told not to. A limit on the output that a hundred files under the
	// state directory would pass, and one change would not, shows that it is not listed.
	f := gittest.New(t)
	f.Publish(branchMain, base)
	l := f.Layout("")
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		gittest.Write(t, l, filepath.Join(".treebind", "lock", strings.Repeat("s", 40)+string(rune('a'+i%26))+string(rune('a'+i/26))), "{}")
	}
	g := f.GitOptions()
	g.MaxOutput = 400
	filter, err := paths.NewFilter(nil, nil, l.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Layout: l, Git: g, Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, l, fileNew, "x\n")
	got, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("the scan failed: %v", err)
	}
	wantKinds(t, got, map[string]Kind{fileNew: Added})
	if len(got.Left) != 0 {
		t.Errorf("left out = %v, want nothing: the state directory is not listed", got.Left)
	}
}

func TestPathsAreRelativeToTheSyncedDirectory(t *testing.T) {
	w := newWorld(t, "data", base)
	w.write(fileNew, "x\n")
	w.write(fileA, "changed\n")
	wantKinds(t, w.scan(), map[string]Kind{fileNew: Added, fileA: Modified})
}

func TestWhatWasStagedIsStillWhatChanged(t *testing.T) {
	w := newWorld(t, "", base)
	w.write("staged.txt", "x\n")
	w.write("ghost.txt", "x\n")
	w.mustPlain("add", "staged.txt", "ghost.txt")
	if err := os.Remove(w.path("ghost.txt")); err != nil {
		t.Fatal(err)
	}
	// A commit that was killed after it staged files leaves them in the index. One that is in the
	// index and on the disk is a file that is not in the commit; one that is in neither is nothing.
	wantKinds(t, w.scan(), map[string]Kind{"staged.txt": Added})
}

func TestWhatTheAgentSawChangesWhenTheFileDoes(t *testing.T) {
	w := newWorld(t, "", base)
	w.write("f.txt", "aaaa")
	first := changes(t, w.scan())["f.txt"].Seen
	if !first.Present || first.Size != 4 {
		t.Fatalf("seen = %+v, want a present file of 4 bytes", first)
	}
	if again := changes(t, w.scan())["f.txt"].Seen; again != first {
		t.Errorf("seen twice = %+v and %+v, want the same for a file that did not change", first, again)
	}

	// The same size and the same modification time, as rsync -t or touch -r leave a file: the change
	// time of the file still moves.
	info, err := os.Stat(w.path("f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(pause)
	w.write("f.txt", "bbbb")
	if err := os.Chtimes(w.path("f.txt"), time.Time{}, info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if second := changes(t, w.scan())["f.txt"].Seen; second == first {
		t.Errorf("seen after a rewrite with the same size and time = %+v, want it to differ", second)
	}
}

func TestAFileBelowADirectoryThatBecameAFileIsSeenAbsent(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.RemoveAll(w.path("dir")); err != nil {
		t.Fatal(err)
	}
	w.write("dir", "now a file\n")
	got := w.scan()
	wantKinds(t, got, map[string]Kind{fileC: Deleted, "dir": Added})
	if c := changes(t, got)[fileC]; c.Seen.Present {
		t.Errorf("seen = %+v, want a file below a file to be absent", c.Seen)
	}
}

func TestAFileThatCannotBeLookedAtFailsTheScanAndSaysWhich(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "x\n")
	w.scanner.lstat = func(string) (fs.FileInfo, error) { return nil, syscall.EIO }
	_, err := w.scanner.Scan(context.Background())
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonScanFailed || !errors.Is(err, syscall.EIO) {
		t.Fatalf("error = %v, want a scan that failed because of the look at a file", err)
	}
	if !strings.Contains(failure.Message, "new.txt cannot be looked at") {
		t.Errorf("message = %q, want it to name the file", failure.Message)
	}
}

func TestADeletedFileIsSeenAbsent(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.Remove(w.path(fileA)); err != nil {
		t.Fatal(err)
	}
	c := changes(t, w.scan())[fileA]
	if c.Kind != Deleted || c.Seen.Present {
		t.Errorf("change = %+v, want a deletion that sees nothing", c)
	}
}

func TestAScanWritesNothing(t *testing.T) {
	w := newWorld(t, "", base)
	// Stale stat data in the index is what makes a plain status write it back: every file is touched.
	time.Sleep(pause)
	later := time.Now().Add(time.Hour)
	for _, name := range []string{fileA, fileB, fileC} {
		if err := os.Chtimes(w.path(name), later, later); err != nil {
			t.Fatal(err)
		}
	}
	index := filepath.Join(w.l.GitDir, "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}

	w.scan()

	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Errorf("the scan rewrote the index")
	}
	if _, err := os.Lstat(index + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("index.lock after a scan: %v, want it not to exist", err)
	}

	// The control: without the setting a plain status does write the index, so the test above can
	// fail.
	w.mustPlain("status", "--porcelain")
	if control, err := os.ReadFile(index); err != nil || bytes.Equal(control, before) {
		t.Errorf("a plain git status left the index as it was (%v): the test does not tell a scan that writes", err)
	}
}

func TestAStaleIndexLockDoesNotStopAScan(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "index.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.write(fileNew, "x\n")
	wantKinds(t, w.scan(), map[string]Kind{fileNew: Added})
}

func TestAnUnfinishedMergeIsAFailureAndNotAGuess(t *testing.T) {
	files := map[string]string{fileA: "1\n", fileB: "2\n", "c.txt": "3\n", "d.txt": "4\n", "e.txt": "5\n"}
	w := newWorld(t, "", files)
	w.mustPlain("checkout", "-q", "-b", "other")
	for name := range files {
		w.write(name, "other\n")
	}
	w.mustPlain("commit", "-q", "-am", "other")
	w.mustPlain("checkout", "-q", branchMain)
	for name := range files {
		w.write(name, "main\n")
	}
	w.mustPlain("commit", "-q", "-am", "main")
	if out, err := w.plain("merge", "other"); err == nil {
		t.Fatalf("the merge did not conflict:\n%s", out)
	}

	_, err := w.scanner.Scan(context.Background())
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonScanFailed {
		t.Fatalf("error = %v, want a scan that failed", err)
	}
	// Five paths are unmerged, and the message names three of them, in order, and counts the rest.
	for _, want := range []string{"5 paths are unmerged", "a.txt, b.txt, c.txt and 2 more"} {
		if !strings.Contains(failure.Message, want) {
			t.Errorf("message = %q, want it to say %q", failure.Message, want)
		}
	}
}

func TestAListThatIsTooLongIsAFailureThatSaysSo(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, base)
	l := f.Layout("")
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		gittest.Write(t, l, filepath.Join("many", strings.Repeat("n", 20)+string(rune('a'+i%26))+string(rune('a'+i/26))), "x\n")
	}
	filter, err := paths.NewFilter(nil, nil, l.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	g := f.GitOptions()
	g.MaxOutput = 256
	s, err := New(Options{Layout: l, Git: g, Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Scan(context.Background())
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonScanFailed || !errors.Is(err, git.ErrOutputLimit) {
		t.Fatalf("error = %v, want a scan that failed with the output limit", err)
	}
	if !strings.Contains(failure.Message, "so many") {
		t.Errorf("message = %q, want it to say that there are too many changes to list", failure.Message)
	}
}

func TestAScanStopsWhenItsContextIsDone(t *testing.T) {
	w := newWorld(t, "", base)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.scanner.Scan(ctx); err == nil {
		t.Error("a scan with a context that is done succeeded")
	}
}

func TestAGitDirectoryThatHoldsMoreThanTheAgentPutThereIsRefused(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "config"), []byte("[core]\n\tfsmonitor = /tmp/evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := w.scanner.Scan(context.Background())
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want an *Error", err)
	}
	if failure.Reason != checkout.ReasonGitDirUnsafe {
		t.Errorf("reason = %s, want %s as for the checkout and the pull", failure.Reason, checkout.ReasonGitDirUnsafe)
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout("")
	if _, err := New(Options{Layout: l, Git: f.GitOptions()}); err == nil {
		t.Error("New with no filter succeeded")
	}
}
