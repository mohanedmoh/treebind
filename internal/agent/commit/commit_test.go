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

package commit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commitmsg"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
)

const (
	branchMain = "main"
	fileA      = "a.txt"
	fileB      = "b.txt"
	fileC      = "dir/c.txt"
	fileNew    = "new.txt"
	fileLeft   = "left.txt"

	botName  = "treebind-bot"
	botEmail = "bot@example.org"

	// whoFormat prints the author and the committer of a commit, each as a name and an email.
	whoFormat = "--format=%an|%ae|%cn|%ce"
)

// base is what the repository holds when a test begins.
var base = map[string]string{fileA: "a\n", fileB: "b\n", fileC: "c\n"}

// world is a checkout of a repository with the files, a scanner that finds the changes, and a committer.
type world struct {
	t         *testing.T
	f         *gittest.Fixture
	l         layout.Layout
	scanner   *scan.Scanner
	committer *Committer
	fenced    atomic.Int32
	fence     func(context.Context) error
}

func newWorld(t *testing.T, sub string, files map[string]string, change ...func(*Options)) *world {
	t.Helper()
	f := gittest.New(t)
	if files != nil {
		f.Publish(branchMain, files)
	}
	l := f.Layout(sub)
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	if err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	filter, err := paths.NewFilter(nil, nil, l.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := scan.New(scan.Options{Layout: l, Git: f.GitOptions(), Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, f: f, l: l, scanner: scanner}
	w.fence = func(context.Context) error { return nil }
	o := Options{
		Layout: l, Git: f.GitOptions(), AuthorName: botName, AuthorEmail: botEmail,
		Fence: func(ctx context.Context) error {
			w.fenced.Add(1)
			return w.fence(ctx)
		},
	}
	for _, c := range change {
		c(&o)
	}
	w.committer, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) write(name, content string) {
	w.t.Helper()
	gittest.Write(w.t, w.l, name, content)
}

func (w *world) path(name string) string { return filepath.Join(w.l.WorkTree, name) }

// git runs plain Git on the checkout, as a person would, and fails the test when it fails.
func (w *world) git(args ...string) string {
	w.t.Helper()
	return w.f.InCheckout(w.l, args...)
}

// changes is what a scan finds now, all of it: a test that commits does not wait for a quiet period.
func (w *world) changes() []scan.Change {
	w.t.Helper()
	got, err := w.scanner.Scan(context.Background())
	if err != nil {
		w.t.Fatalf("the scan failed: %v", err)
	}
	return got.Changes
}

// commit commits the changes that a scan finds now.
func (w *world) commit() Result {
	w.t.Helper()
	got, err := w.committer.Commit(context.Background(), w.changes())
	if err != nil {
		w.t.Fatalf("the commit failed: %v", err)
	}
	return got
}

// head is the commit that the checkout is at, or empty when there is none.
func (w *world) head() string {
	w.t.Helper()
	out, err := w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree}, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// committed is what the last commit changed: a status letter and a path, as "A new.txt", sorted.
func (w *world) committed() []string {
	w.t.Helper()
	out := w.git("diff-tree", "-r", "-z", "--name-status", "--no-renames", "--root", "--no-commit-id", "HEAD")
	fields := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var got []string
	for i := 0; i+1 < len(fields); i += 2 {
		got = append(got, fields[i]+" "+fields[i+1])
	}
	slices.Sort(got)
	return got
}

func (w *world) wantCommitted(want ...string) {
	w.t.Helper()
	slices.Sort(want)
	if got := w.committed(); !slices.Equal(got, want) {
		w.t.Errorf("the commit changed %q, want %q", got, want)
	}
}

// staged is what the index holds that the commit at HEAD does not, for a test of what is left behind.
func (w *world) staged() []string {
	w.t.Helper()
	out := w.git("diff-index", "--cached", "--name-only", "-z", "HEAD")
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(out, "\x00"), "\x00")
}

func TestCommitsEachKindOfChangeInOneCommitByTheIdentityWithTheRenderedMessage(t *testing.T) {
	w := newWorld(t, "", base)
	before := w.head()
	w.write(fileNew, "new\n")                        // Added
	w.write(fileA, "a, changed\n")                   // Modified
	if err := os.Remove(w.path(fileB)); err != nil { // Deleted
		t.Fatal(err)
	}
	if err := os.Remove(w.path(fileC)); err != nil { // TypeChanged: a file that becomes a link
		t.Fatal(err)
	}
	if err := os.Symlink(fileA, w.path(fileC)); err != nil {
		t.Fatal(err)
	}

	got := w.commit()

	if got.Outcome != Committed || got.Files != 4 || got.Gone != 0 {
		t.Errorf("result = %+v, want Committed, with 4 files and none gone", got)
	}
	if got.Commit != w.head() || got.Commit == before {
		t.Errorf("result.Commit = %s, HEAD = %s, before = %s, want the new HEAD", got.Commit, w.head(), before)
	}
	w.wantCommitted("A "+fileNew, "M "+fileA, "D "+fileB, "T "+fileC)
	if n := w.git("rev-list", "--count", before+"..HEAD"); n != "1" {
		t.Errorf("%s commits were made, want one", n)
	}
	if who := w.git("log", "-1", whoFormat); who != botName+"|"+botEmail+"|"+botName+"|"+botEmail {
		t.Errorf("the commit is by %q, want the identity as author and as committer", who)
	}
	if message := w.git("log", "-1", "--format=%B"); message != "treebind: sync 4 file(s)" {
		t.Errorf("the message is %q, want the default template with the count", message)
	}
	if left := w.changes(); len(left) != 0 {
		t.Errorf("a scan after the commit finds %+v, want nothing: the commit is what it compares with", left)
	}
	if staged := w.staged(); len(staged) != 0 {
		t.Errorf("the index holds %q beyond the commit", staged)
	}
}

func TestCommitsAChangeOfMode(t *testing.T) {
	w := newWorld(t, "", base)
	if err := os.Chmod(w.path(fileA), 0o755); err != nil {
		t.Fatal(err)
	}

	got := w.commit()

	if got.Outcome != Committed || got.Files != 1 {
		t.Errorf("result = %+v, want one file committed", got)
	}
	w.wantCommitted("M " + fileA)
	if mode := w.git("ls-tree", "HEAD", "--", fileA); !strings.HasPrefix(mode, "100755 ") {
		t.Errorf("the file is %q in the commit, want mode 100755", mode)
	}
}

func TestTheMessageIsTheTemplateRenderedAndKeptAsItIsWritten(t *testing.T) {
	// Blank lines one after the other, and spaces at the end of a line, are what the default cleanup of Git changes
	// in a message that is given on the command line. A line that starts with # is kept by it too, and is here for
	// the template of a person who writes one.
	const template = "# {{.Count}} file(s) of the application\n\n\n  indented line   \n\n\n\nlast line"
	w := newWorld(t, "", base, func(o *Options) { o.MessageTemplate = template })
	w.write(fileNew, "new\n")
	w.write(fileA, "changed\n")

	w.commit()

	want := "# 2 file(s) of the application\n\n\n  indented line   \n\n\n\nlast line"
	if got := w.git("log", "-1", "--format=%B"); got != want {
		t.Errorf("the message is %q, want %q", got, want)
	}
}

func TestOnlyTheChangesThatAreGivenAreCommitted(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")
	w.write(fileA, "changed\n")
	w.write(fileB, "changed too\n")
	var chosen []scan.Change
	for _, c := range w.changes() {
		if c.Path != fileB {
			chosen = append(chosen, c)
		}
	}

	got, err := w.committer.Commit(context.Background(), chosen)
	if err != nil {
		t.Fatal(err)
	}

	if got.Files != 2 {
		t.Errorf("files = %d, want 2", got.Files)
	}
	w.wantCommitted("A "+fileNew, "M "+fileA)
	left := w.changes()
	if len(left) != 1 || left[0].Path != fileB {
		t.Errorf("a scan finds %+v, want only %s, which was not given", left, fileB)
	}
}

func TestNamesThatAreNotPatternsAreStagedAsTheyAre(t *testing.T) {
	// Each of these means something as a pathspec. The commit gives Git literal paths, so it commits the file
	// that has the name and nothing that the name could match. The files that are not named are left.
	names := []string{
		"a[1].txt", "star*.txt", "q?.txt", "sp ace.txt", "new\nline.txt", "tab\there.txt", "back\\slash.txt",
		"-rf", "--force", ":(top)x", ":(exclude)y", ":(glob)**", ":!z", "café 田中.txt",
		"bytes-\xff\xfe-not-utf8.txt", "dir with space/inner name.txt", ".hidden", "ends with space ",
	}
	w := newWorld(t, "", base)
	want := make([]string, 0, len(names))
	for _, name := range names {
		w.write(name, "content of "+name+"\n")
		want = append(want, "A "+name)
	}
	// Files that the patterns above could match, and that are changed but not given.
	for _, name := range []string{"a1.txt", "starX.txt", "qZ.txt", "x", "y", "z"} {
		w.write(name, "not given\n")
	}
	var chosen []scan.Change
	for _, c := range w.changes() {
		if slices.Contains(names, c.Path) {
			chosen = append(chosen, c)
		}
	}
	if len(chosen) != len(names) {
		t.Fatalf("the scan lists %d of the %d names", len(chosen), len(names))
	}

	got, err := w.committer.Commit(context.Background(), chosen)
	if err != nil {
		t.Fatal(err)
	}

	if got.Files != len(names) {
		t.Errorf("files = %d, want %d", got.Files, len(names))
	}
	w.wantCommitted(want...)
	for _, name := range []string{"a1.txt", "starX.txt", "qZ.txt", "x", "y", "z"} {
		if !slices.ContainsFunc(w.changes(), func(c scan.Change) bool { return c.Path == name }) {
			t.Errorf("%s is not left as a change after the commit: a name that is a pattern took it in", name)
		}
	}
}

func TestMoreFilesThanTheArgumentsOfACommandAllow(t *testing.T) {
	// The names add up to about two megabytes, which is what the arguments of one command may hold at the most on Linux.
	// They are given to Git in a file.
	const count, width = 9000, 230
	w := newWorld(t, "", base)
	for i := range count {
		w.write(fmt.Sprintf("many/%05d-%s.txt", i, strings.Repeat("n", width)), "x\n")
	}

	got := w.commit()

	if got.Outcome != Committed || got.Files != count {
		t.Errorf("result = %+v, want %d files committed", got, count)
	}
	if n := w.git("rev-list", "--count", "HEAD"); n != "2" {
		t.Errorf("%s commits, want the first one and this", n)
	}
	if left := w.changes(); len(left) != 0 {
		t.Errorf("%d changes are left", len(left))
	}
}

func TestAPathThatWasRemovedBeforeTheCommitIsLeftOutAndTheRestIsCommitted(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")
	w.write("vanishes.txt", "short lived\n")
	w.write(fileA, "changed\n")
	listed := w.changes()
	if err := os.Remove(w.path("vanishes.txt")); err != nil {
		t.Fatal(err)
	}

	got, err := w.committer.Commit(context.Background(), listed)
	if err != nil {
		t.Fatalf("a path that is gone failed the commit of the rest: %v", err)
	}

	if got.Outcome != Committed || got.Files != 2 || got.Gone != 1 {
		t.Errorf("result = %+v, want 2 files committed and 1 gone", got)
	}
	w.wantCommitted("A "+fileNew, "M "+fileA)
}

func TestAFileOfTheCommitThatWasRemovedAfterTheScanIsCommittedAsRemoved(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileA, "changed\n")
	listed := w.changes()
	if err := os.Remove(w.path(fileA)); err != nil {
		t.Fatal(err)
	}

	got, err := w.committer.Commit(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}

	if got.Outcome != Committed || got.Files != 1 || got.Gone != 0 {
		t.Errorf("result = %+v, want the one file committed, and not counted as gone: it is a removal now", got)
	}
	w.wantCommitted("D " + fileA)
}

func TestNothingToCommitIsNotAnError(t *testing.T) {
	t.Run("a file that was changed back before the commit", func(t *testing.T) {
		w := newWorld(t, "", base)
		w.write(fileA, "changed\n")
		listed := w.changes()
		w.write(fileA, base[fileA])
		before := w.head()

		got, err := w.committer.Commit(context.Background(), listed)
		if err != nil {
			t.Fatal(err)
		}

		if got.Outcome != NothingToCommit || got.Files != 0 || got.Commit != "" {
			t.Errorf("result = %+v, want NothingToCommit", got)
		}
		if w.head() != before {
			t.Error("a commit was made for nothing")
		}
	})

	t.Run("no changes given", func(t *testing.T) {
		w := newWorld(t, "", base)
		got, err := w.committer.Commit(context.Background(), nil)
		if err != nil || got.Outcome != NothingToCommit {
			t.Errorf("Commit(nil) = %+v, %v, want NothingToCommit", got, err)
		}
		if n := w.fenced.Load(); n != 0 {
			t.Errorf("the writer lock was checked %d times for nothing to write", n)
		}
	})
}

func TestWhatAKilledRunLeftStagedIsNotCommitted(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileLeft, "staged by a run that was killed\n")
	w.git("add", "--", fileLeft)
	w.write(fileNew, "new\n")
	listed := []scan.Change{}
	for _, c := range w.changes() {
		if c.Path == fileNew {
			listed = append(listed, c)
		}
	}

	if _, err := w.committer.Commit(context.Background(), listed); err != nil {
		t.Fatal(err)
	}

	w.wantCommitted("A " + fileNew)
	if left := w.changes(); len(left) != 1 || left[0].Path != fileLeft {
		t.Errorf("a scan finds %+v, want left.txt as a change that is not committed", left)
	}
}

func TestTheFirstCommitOfARepositoryThatHasNone(t *testing.T) {
	w := newWorld(t, "", nil)
	if w.head() != "" {
		t.Fatal("the repository has a commit")
	}
	w.write(fileNew, "first\n")

	got := w.commit()

	if got.Outcome != Committed || got.Files != 1 || got.Commit == "" {
		t.Errorf("result = %+v, want the first commit", got)
	}
	if n := w.git("rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("%s commits, want 1", n)
	}
	w.wantCommitted("A " + fileNew)
}

func TestTheCountIsTheNumberOfFilesThatAreInTheCommit(t *testing.T) {
	w := newWorld(t, "", base, func(o *Options) { o.MessageTemplate = "{{.Count}}" })
	w.write(fileA, "changed\n")
	w.write(fileB, "changed\n")
	w.write(fileNew, "new\n")
	listed := w.changes()
	w.write(fileB, base[fileB]) // changed back, so it is not in the commit

	got, err := w.committer.Commit(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}

	if got.Files != 2 {
		t.Errorf("files = %d, want 2", got.Files)
	}
	if message := w.git("log", "-1", "--format=%B"); message != "2" {
		t.Errorf("the message is %q, want 2", message)
	}
}

func TestTheWriterLockIsCheckedBeforeAnythingIsWrittenAndNothingIsWrittenWithoutIt(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileLeft, "staged\n")
	w.git("add", "--", fileLeft)
	w.write(fileNew, "new\n")
	before := w.head()
	lost := errors.New("the writer lock is another agent's")
	w.fence = func(context.Context) error { return lost }

	_, err := w.committer.Commit(context.Background(), w.changes())

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed || !errors.Is(err, lost) {
		t.Fatalf("error = %v, want a CommitFailed that wraps what the fence said", err)
	}
	if !strings.Contains(failure.Message, "writer lock") {
		t.Errorf("message = %q, want it to say that the writer lock is the cause", failure.Message)
	}
	if w.head() != before {
		t.Error("a commit was made without the lock")
	}
	if staged := w.staged(); !slices.Equal(staged, []string{fileLeft}) {
		t.Errorf("the index holds %q, want what it held before: nothing was written without the lock, not even a reset", staged)
	}
	if n := w.fenced.Load(); n != 1 {
		t.Errorf("the writer lock was checked %d times, want once", n)
	}
	if _, err := os.Lstat(filepath.Join(w.l.StateDir, listFile)); err == nil {
		t.Error("the file of paths is left in the state directory")
	}
}

func TestACommitThatGitRefusesIsAFailureThatSaysWhatGitSaidAndLeavesNothingStaged(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")
	// A reference that is locked, as one is while another Git writes it: the commit cannot move the branch.
	lock := filepath.Join(w.l.GitDir, "refs", "heads", branchMain+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := w.head()

	_, err := w.committer.Commit(context.Background(), w.changes())

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed {
		t.Fatalf("error = %v, want a CommitFailed", err)
	}
	if !strings.Contains(failure.Message, branchMain+".lock") {
		t.Errorf("message = %q, want what Git said, which names the lock", failure.Message)
	}
	if w.head() != before {
		t.Error("HEAD moved")
	}
	if staged := w.staged(); len(staged) != 0 {
		t.Errorf("the index holds %q after the failure, want nothing: a pull would be blocked by it", staged)
	}
	if _, err := os.Lstat(filepath.Join(w.l.StateDir, listFile)); err == nil {
		t.Error("the file of paths is left in the state directory")
	}

	// The next try, once the lock is gone, commits what was not committed.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if got := w.commit(); got.Outcome != Committed || got.Files != 1 {
		t.Errorf("the second try = %+v, want the file committed", got)
	}
}

func TestAGitDirectoryThatHoldsMoreThanTheAgentPutThereIsRefusedWithTheReasonOfTheCheckoutAndThePull(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")
	changes := w.changes()
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "config"), []byte("[core]\n\tfsmonitor = /tmp/evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := w.committer.Commit(context.Background(), changes)

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != checkout.ReasonGitDirUnsafe {
		t.Fatalf("error = %v, want the reason %s", err, checkout.ReasonGitDirUnsafe)
	}
}

func TestACommitWithAContextThatIsDoneWritesNothing(t *testing.T) {
	w := newWorld(t, "", base)
	w.write(fileNew, "new\n")
	listed := w.changes()
	before := w.head()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := w.committer.Commit(ctx, listed); err == nil {
		t.Fatal("a commit with a context that is done succeeded")
	}
	if w.head() != before {
		t.Error("a commit was made")
	}
}

func TestACommitInASubPathCommitsThePathsOfTheSyncedDirectory(t *testing.T) {
	w := newWorld(t, "dashboards", base)
	w.write(fileNew, "new\n")
	w.write(fileC, "changed\n")

	got := w.commit()

	if got.Files != 2 {
		t.Errorf("files = %d, want 2", got.Files)
	}
	w.wantCommitted("A "+fileNew, "M "+fileC)
}

func TestNewChecksTheAuthorAndTheTemplateAndNamesWhatIsWrong(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout("")
	fence := func(context.Context) error { return nil }
	for name, o := range map[string]Options{
		"a name with an angle bracket":              {AuthorName: "bot <x>", AuthorEmail: botEmail},
		"an email that is not one":                  {AuthorName: botName, AuthorEmail: "not-an-email"},
		"both wrong":                                {AuthorName: "bot\nname", AuthorEmail: "Name <x@example.org>"},
		"a template that does not parse":            {AuthorName: botName, AuthorEmail: botEmail, MessageTemplate: "{{"},
		"a template with a field that is not there": {AuthorName: botName, AuthorEmail: botEmail, MessageTemplate: "{{.Nope}}"},
		"a template that renders nothing":           {AuthorName: botName, AuthorEmail: botEmail, MessageTemplate: "  "},
		"a name and no email":                       {AuthorName: botName},
		"an email and no name":                      {AuthorEmail: botEmail},
	} {
		t.Run(name, func(t *testing.T) {
			o.Layout, o.Git, o.Fence = l, f.GitOptions(), fence
			if _, err := New(o); err == nil {
				t.Error("New accepted it")
			} else if !strings.HasPrefix(err.Error(), "commit: ") {
				t.Errorf("error = %q, want it to begin with commit: ", err)
			}
		})
	}

	t.Run("both wrong are both named", func(t *testing.T) {
		_, err := New(Options{Layout: l, Git: f.GitOptions(), Fence: fence, AuthorName: "bot\nname", AuthorEmail: "Name <x@example.org>"})
		if err == nil || !strings.Contains(err.Error(), "name") || !strings.Contains(err.Error(), "email") {
			t.Errorf("error = %v, want it to name the name and the email", err)
		}
	})

	t.Run("a name without an email says that both are needed", func(t *testing.T) {
		_, err := New(Options{Layout: l, Git: f.GitOptions(), Fence: fence, AuthorName: botName})
		if err == nil || !strings.Contains(err.Error(), "a name and an email, or neither") {
			t.Errorf("error = %v, want it to say that the author needs a name and an email, or neither", err)
		}
	})

	t.Run("no fence", func(t *testing.T) {
		if _, err := New(Options{Layout: l, Git: f.GitOptions(), AuthorName: botName, AuthorEmail: botEmail}); err != nil {
			t.Errorf("New with no fence = %v, want it accepted: with no lock to hold there is nothing to check", err)
		}
	})
}

func TestWithNothingSetTheDefaultsAreTheAuthorAndTheTemplateOfTheAgent(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, base)
	l := f.Layout("")
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Layout: l, Git: f.GitOptions()})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, f: f, l: l, committer: c}
	filter, err := paths.NewFilter(nil, nil, l.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	w.scanner, err = scan.New(scan.Options{Layout: l, Git: f.GitOptions(), Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	w.write(fileNew, "new\n")

	w.commit()

	if who := w.git("log", "-1", whoFormat); who != commitmsg.DefaultAuthorName+"|"+commitmsg.DefaultAuthorEmail+"|"+commitmsg.DefaultAuthorName+"|"+commitmsg.DefaultAuthorEmail {
		t.Errorf("the commit is by %q, want the default author", who)
	}
	if message := w.git("log", "-1", "--format=%B"); message != "treebind: sync 1 file(s)" {
		t.Errorf("the message is %q, want the default template", message)
	}
}

// failsAfterStaging is a template that renders for one file, which is what the trial at the start renders, and
// fails for more, so that a commit fails after it has staged what it was given.
const failsAfterStaging = "{{if gt .Count 1}}{{.Nope}}{{end}}sync"

func TestAFailureAfterStagingPutsTheIndexBackSoThatNothingIsLeftStaged(t *testing.T) {
	w := newWorld(t, "", base, func(o *Options) { o.MessageTemplate = failsAfterStaging })
	w.write(fileNew, "new\n")
	w.write(fileA, "changed\n")
	before := w.head()

	_, err := w.committer.Commit(context.Background(), w.changes())

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed {
		t.Fatalf("error = %v, want a CommitFailed", err)
	}
	if w.head() != before {
		t.Error("HEAD moved")
	}
	if staged := w.staged(); len(staged) != 0 {
		t.Errorf("the index holds %q after the failure, want nothing: a pull would be blocked by it", staged)
	}
	if n := w.fenced.Load(); n != 2 {
		t.Errorf("the writer lock was checked %d times, want once before the commit and once before the reset", n)
	}
	if _, err := os.Lstat(filepath.Join(w.l.StateDir, listFile)); err == nil {
		t.Error("the file of paths is left in the state directory")
	}
}

func TestAFailureAfterStagingDoesNotPutTheIndexBackWithoutTheWriterLock(t *testing.T) {
	w := newWorld(t, "", base, func(o *Options) { o.MessageTemplate = failsAfterStaging })
	w.write(fileNew, "new\n")
	w.write(fileA, "changed\n")
	// The lock is held when the commit begins and is gone when it has to put the index back.
	w.fence = func(context.Context) error {
		if w.fenced.Load() > 1 {
			return errors.New("the writer lock is another agent's")
		}
		return nil
	}

	if _, err := w.committer.Commit(context.Background(), w.changes()); err == nil {
		t.Fatal("the commit did not fail")
	}

	if staged := w.staged(); len(staged) != 2 {
		t.Errorf("the index holds %q, want the two files that the commit staged: a reset without the lock is a write without it", staged)
	}
	if n := w.fenced.Load(); n != 2 {
		t.Errorf("the writer lock was checked %d times, want once before the commit and once before the reset", n)
	}
}

func TestAPathThatAScanCannotHaveListedIsRefusedBeforeAnythingIsRun(t *testing.T) {
	for name, p := range map[string]string{
		"empty":            "",
		"absolute":         "/etc/passwd",
		"above":            "../outside.txt",
		"a dot dot inside": "dir/../b.txt",
		"two slashes":      "dir//b.txt",
		"the directory":    ".",
		"a NUL":            "a\x00b.txt",
		"a trailing slash": "dir/",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, "", base)
			before := w.head()

			_, err := w.committer.Commit(context.Background(), []scan.Change{{Path: p, Kind: scan.Added}})

			var failure *Error
			if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed || !strings.Contains(failure.Message, "not a path of the synced directory") {
				t.Fatalf("error = %v, want a CommitFailed that says that the path is not one of the synced directory", err)
			}
			if n := w.fenced.Load(); n != 0 {
				t.Errorf("the writer lock was checked %d times for a path that is refused", n)
			}
			if w.head() != before {
				t.Error("a commit was made")
			}
		})
	}
}
