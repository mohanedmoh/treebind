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

package pull

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const (
	branchMain = "main"
	subPath    = "dashboards"
	fileA      = "a.json"
	fileB      = "b.json"
	fileC      = "c.json"
	contentA   = "a\n"
	contentB   = "b\n"
	contentC   = "c\n"
	mine       = "mine\n"
	aChanged   = "a, changed\n"
	bChanged   = "b, changed\n"
	edited     = "edited here\n"
)

var versionOne = map[string]string{fileA: contentA, fileB: contentB}

// world is a repository, a checkout that was made on it, and a Puller for the checkout.
type world struct {
	f      *gittest.Fixture
	l      layout.Layout
	puller *Puller
	tip    string
}

// newWorld publishes version one, makes the checkout of a layout the way the agent does, and
// returns a Puller. The options can be changed first.
func newWorld(t *testing.T, sub string, change ...func(*Options)) *world {
	t.Helper()
	return newWorldOf(t, sub, versionOne, change...)
}

// newWorldOf is newWorld for a repository that starts with the files.
func newWorldOf(t *testing.T, sub string, files map[string]string, change ...func(*Options)) *world {
	t.Helper()
	f := gittest.New(t)
	tip := f.Publish(branchMain, files)
	l := f.Layout(sub)
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	if err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	return &world{f: f, l: l, puller: puller(t, f, l, change...), tip: tip}
}

func puller(t *testing.T, f *gittest.Fixture, l layout.Layout, change ...func(*Options)) *Puller {
	t.Helper()
	o := Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions()}
	for _, c := range change {
		c(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *world) pull(t *testing.T) (Result, error) {
	t.Helper()
	return w.puller.Pull(context.Background())
}

// wantReason fails the test unless err is an *Error with the reason.
func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	_ = errorOf(t, err, reason)
}

// errorOf fails the test unless err is an *Error with the reason, and returns it.
func errorOf(t *testing.T, err error, reason string) *Error {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) {
		t.Fatalf("error = %v (%T), want an *Error with the reason %s", err, err, reason)
	}
	if got.Reason != reason {
		t.Fatalf("reason = %s (%s), want %s", got.Reason, got.Message, reason)
	}
	return got
}

// dirty is what git status says about the checkout, not counting the state directory, which is
// inside the work tree when the whole claim is synced and is never part of the repository.
func (w *world) dirty(t *testing.T) string {
	t.Helper()
	var lines []string
	for line := range strings.Lines(w.f.InCheckout(w.l, "status", "--porcelain", "--untracked-files=all")) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "?? .treebind/") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "; ")
}

func (w *world) head() string { return w.f.InCheckout(w.l, "rev-parse", "HEAD") }

func eachLayout(t *testing.T, run func(t *testing.T, sub string)) {
	t.Helper()
	for _, sub := range []string{subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) { run(t, sub) })
	}
}

func TestPullDoesNothingWhenTheCheckoutIsAtTheTipAndDoesNotFetch(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		before := w.f.Sessions()

		got, err := w.pull(t)

		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != UpToDate || got.Branch != branchMain || got.From != w.tip || got.To != w.tip {
			t.Errorf("result = %+v, want UpToDate at %s", got, w.tip)
		}
		// One session, the question of what the tip is, and no fetch: R-19.
		if sessions := w.f.Sessions() - before; sessions != 1 {
			t.Errorf("the repository was reached %d times, want once, with no fetch", sessions)
		}
	})
}

func TestPullFastForwardsTheCheckoutToTheNewTip(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileB: "", fileC: contentC})
		before := w.f.Sessions()

		got, err := w.pull(t)

		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != Advanced || got.From != w.tip || got.To != tip || w.head() != tip {
			t.Errorf("result = %+v, HEAD = %s, want Advanced from %s to %s", got, w.head(), w.tip, tip)
		}
		if gittest.Read(t, w.l, fileA) != aChanged || gittest.Read(t, w.l, fileC) != contentC || gittest.Exists(filepath.Join(w.l.WorkTree, fileB)) {
			t.Error("the files of the work tree are not the files of the new tip")
		}
		if dirty := w.dirty(t); dirty != "" {
			t.Errorf("status = %q, want the checkout clean", dirty)
		}
		// The question and the fetch.
		if sessions := w.f.Sessions() - before; sessions != 2 {
			t.Errorf("the repository was reached %d times, want twice: the question and the fetch", sessions)
		}
	})
}

func TestPullKeepsALocalEditToAFileTheUpdateDoesNotTouch(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		gittest.Write(t, w.l, fileA, edited)
		gittest.Write(t, w.l, "new-here.json", mine)
		tip := w.f.Publish(branchMain, map[string]string{fileB: bChanged})

		got, err := w.pull(t)

		if err != nil || got.Outcome != Advanced || got.To != tip {
			t.Fatalf("result = %+v, %v, want Advanced to %s", got, err, tip)
		}
		if gittest.Read(t, w.l, fileA) != edited || gittest.Read(t, w.l, "new-here.json") != mine || gittest.Read(t, w.l, fileB) != bChanged {
			t.Error("the update did not keep what was edited and add what changed")
		}
	})
}

func TestPullDoesNotWriteOverALocalEditAndSaysWhichFiles(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub)
		gittest.Write(t, w.l, fileA, edited)
		gittest.Write(t, w.l, fileB, "edited here too\n")
		w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileB: bChanged})

		_, err := w.pull(t)

		got := errorOf(t, err, ReasonBlocked)
		if !strings.Contains(got.Message, "2 file(s)") || !strings.Contains(got.Message, fileA) || !strings.Contains(got.Message, fileB) {
			t.Errorf("message = %q, want the count and the files", got.Message)
		}
		if gittest.Read(t, w.l, fileA) != edited || gittest.Read(t, w.l, fileB) != "edited here too\n" || w.head() != w.tip {
			t.Error("a file was written to, or the checkout moved, although the pull was refused")
		}
		// Once the edits are given up, the next pull goes on.
		gittest.Write(t, w.l, fileA, contentA)
		gittest.Write(t, w.l, fileB, contentB)
		if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
			t.Fatalf("result = %+v, %v, want Advanced once the files are as they were", got, err)
		}
	})
}

func TestPullDoesNotWriteOverAFileThatTheUpdateDeletesEither(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, fileB, edited)
	w.f.Publish(branchMain, map[string]string{fileB: ""})

	_, err := w.pull(t)

	wantReason(t, err, ReasonBlocked)
	if gittest.Read(t, w.l, fileB) != edited {
		t.Error("the file with a local edit was deleted")
	}
}

func TestPullDoesNotWriteOverAnUntrackedFileInTheWay(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, fileC, mine)
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonBlocked)
	if !strings.Contains(got.Message, fileC) {
		t.Errorf("message = %q, want the file in the way", got.Message)
	}
	if gittest.Read(t, w.l, fileC) != mine {
		t.Error("the file of the application was written over")
	}
}

func TestPullLeavesACheckoutThatHasCommitsOfItsOwnAlone(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, "local.json", mine)
	w.f.InCheckout(w.l, "add", "local.json")
	w.f.InCheckout(w.l, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "local")
	local := w.head()

	got, err := w.pull(t)

	if err != nil || got.Outcome != Ahead || got.From != local || got.To != local {
		t.Fatalf("result = %+v, %v, want Ahead at %s", got, err, local)
	}
}

func TestPullDoesNotMergeCommitsOnBothSidesAndSaysHowMany(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, "local.json", mine)
	w.f.InCheckout(w.l, "add", "local.json")
	w.f.InCheckout(w.l, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "local")
	local := w.head()
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonDiverged)
	if !strings.Contains(got.Message, "the checkout has 1 commit(s)") || !strings.Contains(got.Message, "the repository has 2 that") {
		t.Errorf("message = %q, want the counts of commits on each side", got.Message)
	}
	if w.head() != local || gittest.Exists(filepath.Join(w.l.WorkTree, fileC)) {
		t.Error("the checkout was changed by a pull that was refused")
	}
}

func TestPullDoesNotFollowABranchThatWasRewritten(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("result = %+v, %v, want the first update applied", got, err)
	}
	// The repository's last commit is replaced: the checkout has a commit that is not on the branch.
	w.f.Rewrite()
	w.f.Publish(branchMain, map[string]string{fileA: "rewritten\n"})

	_, err := w.pull(t)

	wantReason(t, err, ReasonDiverged)
	if gittest.Read(t, w.l, fileA) != contentA {
		t.Error("a force-pushed branch was written to the checkout")
	}
}

func TestPullSaysWhenTheBranchIsGone(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.Plain(w.f.Bare, "update-ref", "-d", "refs/heads/main")

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonBranchGone)
	if !strings.Contains(got.Message, "no branch main any more") {
		t.Errorf("message = %q, want it to name the branch", got.Message)
	}
	if w.head() != w.tip {
		t.Error("the checkout moved")
	}
}

func TestPullSaysWhenTheRepositoryCannotBeReached(t *testing.T) {
	w := newWorld(t, subPath)
	p := puller(t, w.f, w.l, func(o *Options) { o.URL = "ssh://localhost" + filepath.Join(filepath.Dir(w.f.Bare), "missing.git") })

	_, err := p.Pull(context.Background())

	got := errorOf(t, err, ReasonFetchFailed)
	if got.Err == nil {
		t.Error("the error of the command is not kept")
	}
	// The next pull, with the repository back, works.
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	if got, err := w.pull(t); err != nil || got.Outcome != Advanced {
		t.Fatalf("result = %+v, %v, want Advanced once the repository can be reached", got, err)
	}
}

func TestPullFastForwardsACheckoutThatHasNoCommitYet(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	// An empty repository, and a directory with a file of the application's own: the checkout is
	// adopted, with its branch unborn.
	gittest.Write(t, l, "mine.json", mine)
	if made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil || made.Commit != "" {
		t.Fatalf("checkout = %+v, %v, want one with no commit", made, err)
	}
	p := puller(t, f, l)
	tip := f.Publish(branchMain, versionOne)

	got, err := p.Pull(context.Background())

	if err != nil || got.Outcome != Advanced || got.From != "" || got.To != tip {
		t.Fatalf("result = %+v, %v, want Advanced from nothing to %s", got, err, tip)
	}
	if gittest.Read(t, l, "mine.json") != mine || gittest.Read(t, l, fileA) != contentA {
		t.Error("the file of the application is gone, or the files of the branch are not there")
	}
}

func TestPullDoesNotWriteOverAFileOfTheApplicationInACheckoutThatHasNoCommitYet(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	gittest.Write(t, l, fileA, mine)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	p := puller(t, f, l)
	f.Publish(branchMain, versionOne)

	_, err := p.Pull(context.Background())

	got := errorOf(t, err, ReasonBlocked)
	if !strings.Contains(got.Message, fileA) || gittest.Read(t, l, fileA) != mine {
		t.Errorf("message = %q, file = %q, want the file named and left as it was", got.Message, gittest.Read(t, l, fileA))
	}
}

func TestPullRefusesAnUpdateThatWritesIntoTheStateDirectory(t *testing.T) {
	w := newWorld(t, "")
	w.f.Publish(branchMain, map[string]string{".treebind/git/config": "[core]\n\tfsmonitor = /bin/sh\n"})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonReservedPath)
	if !strings.Contains(got.Message, ".treebind") {
		t.Errorf("message = %q, want the name", got.Message)
	}
	if config, readErr := os.ReadFile(filepath.Join(w.l.GitDir, "config")); readErr == nil && strings.Contains(string(config), "fsmonitor") {
		t.Error("the config of the branch was written into the Git directory")
	}
	if w.head() != w.tip {
		t.Error("the checkout moved")
	}
}

func TestPullAllowsADirectoryNamedLikeTheStateDirectoryWithASubPath(t *testing.T) {
	w := newWorld(t, subPath)
	tip := w.f.Publish(branchMain, map[string]string{".treebind/notes.json": contentC})

	got, err := w.pull(t)

	if err != nil || got.To != tip || gittest.Read(t, w.l, ".treebind/notes.json") != contentC {
		t.Fatalf("result = %+v, %v, want it applied: the state directory is beside the work tree", got, err)
	}
}

func TestPullRunsNoGitAgainstAGitDirectoryThatHoldsMoreThanTheAgentPutThere(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := w.f.Sessions()

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonGitDirUnsafe)
	if !strings.Contains(got.Message, "notes.txt") || w.f.Sessions() != before {
		t.Errorf("message = %q, reached %d times, want the file named and the repository not asked", got.Message, w.f.Sessions()-before)
	}
}

func TestPullChecksTheFenceBeforeItWritesAndNotWhenThereIsNothingToWrite(t *testing.T) {
	var fenced int
	w := newWorld(t, subPath, func(o *Options) {
		o.Fence = func(context.Context) error { fenced++; return nil }
	})
	if _, err := w.pull(t); err != nil || fenced != 0 {
		t.Fatalf("a pull with nothing to write called the fence %d times, err = %v", fenced, err)
	}
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}

	if fenced != 1 {
		t.Errorf("fence called %d times, want once", fenced)
	}
}

func TestPullDoesNotWriteWhenTheFenceRefuses(t *testing.T) {
	lost := errors.New("the lock is lost")
	w := newWorld(t, subPath, func(o *Options) { o.Fence = func(context.Context) error { return lost } })
	w.f.Publish(branchMain, map[string]string{fileC: contentC})

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonRefused)
	if !errors.Is(err, lost) || !strings.Contains(got.Message, "the lock is lost") {
		t.Errorf("error = %v, want the refusal of the caller underneath and in the message", err)
	}
	if w.head() != w.tip || gittest.Exists(filepath.Join(w.l.WorkTree, fileC)) {
		t.Error("the checkout was written to although the pull was refused")
	}
}

func TestPullRefusesACheckoutThatIsNotOnTheBranch(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.InCheckout(w.l, "checkout", "-q", "--detach")

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "no branch") {
		t.Errorf("message = %q, want it to say the checkout follows no branch", got.Message)
	}
}

func TestPullCountsACheckoutOnAnotherBranchAsOne(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.InCheckout(w.l, "checkout", "-q", "-b", "other")

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "the branch other and not the branch main") {
		t.Errorf("message = %q, want both branches named", got.Message)
	}
}

func TestPullSaysWhenTheGitDirectoryCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory that is closed to it")
	}
	w := newWorld(t, subPath)
	if err := os.Chmod(w.l.GitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.l.GitDir, 0o700) })

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "cannot be looked at") || !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want the failure to look at the directory", err)
	}
}

func TestErrorHasTheMessageAndTheCauseUnderneath(t *testing.T) {
	cause := errors.New("underneath")
	e := &Error{Reason: ReasonPullFailed, Message: "the message", Err: cause}

	if e.Error() != "the message" || !errors.Is(e, cause) {
		t.Errorf("error = %q, want the message, and the cause to be reachable", e.Error())
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)
	tests := []struct {
		name   string
		change func(*Options)
		say    string
	}{
		{"no address", func(o *Options) { o.URL = "" }, "URL"},
		{"no branch", func(o *Options) { o.Branch = "" }, "branch is not set"},
		{"a branch that is refused", func(o *Options) { o.Branch = "-x" }, "must not start with '-'"},
		{"a Runner that cannot be set up", func(o *Options) { o.Git.SSH.Program = "relative/ssh" }, "SSH.Program"},
		{"a cap that is negative", func(o *Options) { o.MaxDeletions = -1 }, "must not be negative"},
		{"a number of refs that is negative", func(o *Options) { o.RecoveryRefs = -1 }, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions()}
			tt.change(&o)

			_, err := New(o)

			if err == nil || !strings.Contains(err.Error(), tt.say) {
				t.Errorf("error = %v, want one that says %q", err, tt.say)
			}
		})
	}
}

func TestPullFetchesNeitherTagsNorFetchHead(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	w.f.Plain(w.f.Bare, "tag", "v1", "main")

	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}

	if tags := w.f.InCheckout(w.l, "for-each-ref", "refs/tags"); tags != "" {
		t.Errorf("tags = %q, want none: a tag is a name that the repository chooses", tags)
	}
	if gittest.Exists(filepath.Join(w.l.GitDir, "FETCH_HEAD")) {
		t.Error("FETCH_HEAD, which names the address of the repository, was written")
	}
	if config, err := os.ReadFile(filepath.Join(w.l.GitDir, "config")); err != nil || strings.Contains(string(config), w.f.Bare) {
		t.Errorf("the address of the repository is in the config: %v", err)
	}
}

func TestPullFollowsTheBranchThatTheCheckoutFollows(t *testing.T) {
	f := gittest.New(t)
	f.Publish(branchMain, versionOne)
	other := f.Publish("other", map[string]string{fileA: "on other\n"})
	l := f.Layout(subPath)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, "other")); err != nil {
		t.Fatal(err)
	}
	p := puller(t, f, l, func(o *Options) { o.Branch = "other" })
	// The scratch repository's history is one line, so the commit of main that holds the file c.json
	// is made after the one of the branch that is pulled.
	tip := f.Publish("other", map[string]string{fileB: "b on other\n"})
	f.Publish(branchMain, map[string]string{fileC: contentC})
	_ = other

	got, err := p.Pull(context.Background())

	if err != nil || got.Outcome != Advanced || got.To != tip || got.Branch != "other" {
		t.Fatalf("result = %+v, %v, want Advanced to %s on other", got, err, tip)
	}
	if gittest.Exists(filepath.Join(l.WorkTree, fileC)) {
		t.Error("a file of another branch was written")
	}
}

func TestNewFillsInTheTimeoutOfAFetch(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subPath)

	p, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions()})

	if err != nil || p.o.FetchTimeout != DefaultFetchTimeout {
		t.Errorf("timeout = %v, %v, want %v: a fetch of a large repository needs more than the Runner's own timeout", p.o.FetchTimeout, err, DefaultFetchTimeout)
	}
}

func TestPullTakesTheBranchAndNotARefThatEndsLikeIt(t *testing.T) {
	w := newWorld(t, subPath)
	tip := w.f.Publish(branchMain, map[string]string{fileC: contentC})
	// ls-remote matches a pattern against the end of a name, so this ref comes back as well, and
	// before the branch.
	w.f.Plain(w.f.Bare, "update-ref", "refs/a/refs/heads/main", w.tip)

	got, err := w.pull(t)

	if err != nil || got.Outcome != Advanced || got.To != tip {
		t.Fatalf("result = %+v, %v, want Advanced to %s, the tip of the branch", got, err, tip)
	}
}

func TestPullNamesThreeOfTheFilesInTheWay(t *testing.T) {
	w := newWorld(t, subPath)
	changes := map[string]string{}
	for _, name := range []string{"1.json", "2.json", "3.json", "4.json", "5.json"} {
		gittest.Write(t, w.l, name, mine)
		changes[name] = contentC
	}
	w.f.Publish(branchMain, changes)

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonBlocked)
	if !strings.Contains(got.Message, "5 file(s)") || strings.Count(got.Message, ".json") != maxNamed {
		t.Errorf("message = %q, want the count and %d names", got.Message, maxNamed)
	}
}

func TestPullSaysWhenTheCheckoutCannotBeRead(t *testing.T) {
	w := newWorld(t, subPath)
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "HEAD"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "cannot be read") {
		t.Errorf("message = %q, want it to say the checkout cannot be read, and not that it follows no branch", got.Message)
	}
}

func TestPullDoesNotTakeACommitItCannotCompareForOneThatWentAnotherWay(t *testing.T) {
	w := newWorld(t, subPath)
	w.f.Publish(branchMain, map[string]string{fileC: contentC})
	tip := w.f.Publish(branchMain, map[string]string{fileA: "a, again\n"})
	// Both commits are fetched, and then the one between the checkout and the tip is lost: the
	// history from the tip cannot be walked down to the checkout.
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	middle := w.f.InCheckout(w.l, "rev-parse", tip+"^")
	if err := os.Remove(filepath.Join(w.l.GitDir, "objects", middle[:2], middle[2:])); err != nil {
		t.Skipf("the commit is not a loose object, so the test cannot lose it: %v", err)
	}

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, " and "+tip[:12]+" cannot be compared") || got.Err == nil {
		t.Errorf("error = %v, want a failure to compare, with the error of Git underneath, naming the commits in short, and not Diverged", err)
	}
}

func TestPullGivesAFailureToLookAtTheUpdateTheReasonOfAPullAndNotOfTheCheckout(t *testing.T) {
	w := newWorld(t, "")
	tip := w.f.Publish(branchMain, map[string]string{fileC: contentC})
	// The commit is fetched, so that the pull asks no more of the repository, and then the tree of
	// it is lost: listing the files of the update fails.
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	tree := w.f.InCheckout(w.l, "rev-parse", tip+"^{tree}")
	if err := os.Remove(filepath.Join(w.l.GitDir, "objects", tree[:2], tree[2:])); err != nil {
		t.Skipf("the tree is not a loose object, so the test cannot lose it: %v", err)
	}

	_, err := w.pull(t)

	got := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(got.Message, "cannot be listed") || got.Err == nil {
		t.Errorf("message = %q, error underneath = %v, want it to say the files of the update cannot be listed, with the cause", got.Message, got.Err)
	}
}

func TestPullDoesNotWriteOverAFileOfTheApplicationThatAGitignoreHides(t *testing.T) {
	// A .env that the application keeps out of the repository is its own. Git writes over such a file when
	// an update adds one at the same path, unless it is told not to.
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{".gitignore": ".env\n", fileA: contentA})
	l := f.Layout(subPath)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	p := puller(t, f, l)
	gittest.Write(t, l, ".env", "SECRET=mine\n")
	theirs := f.PublishIgnored(branchMain, map[string]string{".env": "SECRET=theirs\n"})

	_, err := p.Pull(context.Background())

	got := errorOf(t, err, ReasonBlocked)
	if !strings.Contains(got.Message, ".env") || gittest.Read(t, l, ".env") != "SECRET=mine\n" {
		t.Errorf("message = %q, .env = %q, want the file named and left as it was", got.Message, gittest.Read(t, l, ".env"))
	}
	if head := f.InCheckout(l, "rev-parse", "HEAD"); head == theirs {
		t.Error("the checkout moved to a commit that holds a file of the application's")
	}
}
