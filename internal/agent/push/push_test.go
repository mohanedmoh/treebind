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

package push

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const (
	branchMain = "main"
	subDir     = "dashboards"
	fileA      = "a.json"
	fileB      = "b.json"

	// hookDeclines is a hook of the repository that declines a push with words of its own, as a host does. It uses
	// what a shell has built in, because the tests run in the image of the agent too.
	hookDeclines = "#!/bin/sh\necho 'GH006: Protected branch update failed for refs/heads/main.' >&2\n" +
		"echo 'Changes must be made through a pull request.' >&2\nexit 1\n"
)

// world is a checkout of a repository, and a Pusher for it.
type world struct {
	t      *testing.T
	f      *gittest.Fixture
	l      layout.Layout
	pusher *Pusher
	fenced atomic.Int32
	fence  func(context.Context) error
}

func newWorld(t *testing.T, files map[string]string, change ...func(*Options)) *world {
	t.Helper()
	f := gittest.New(t)
	if files != nil {
		f.Publish(branchMain, files)
	}
	l := f.Layout(subDir)
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	if err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	w := &world{t: t, f: f, l: l}
	w.fence = func(context.Context) error { return nil }
	o := Options{
		Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: time.Minute,
		Fence: func(ctx context.Context) error {
			w.fenced.Add(1)
			return w.fence(ctx)
		},
	}
	for _, c := range change {
		c(&o)
	}
	w.pusher, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// commit makes a commit in the checkout, as the commit step does, by plain Git.
func (w *world) commit(name, content string) string {
	w.t.Helper()
	gittest.Write(w.t, w.l, name, content)
	w.f.InCheckout(w.l, "add", "-A")
	w.f.InCheckout(w.l, "commit", "-q", "-m", "local "+name)
	return w.head()
}

func (w *world) head() string { return w.f.InCheckout(w.l, "rev-parse", "HEAD") }

// remoteTip is where the branch of the repository is, or empty when it has none.
func (w *world) remoteTip() string {
	w.t.Helper()
	out, err := w.f.PlainErr(w.f.Bare, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branchMain)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// tracking is the last tip that the checkout knows the repository to be at, or empty.
func (w *world) tracking() string {
	w.t.Helper()
	out, err := w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree},
		"rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branchMain)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (w *world) push() (Result, error) {
	w.t.Helper()
	return w.pusher.Push(context.Background())
}

func (w *world) mustPush() Result {
	w.t.Helper()
	got, err := w.push()
	if err != nil {
		w.t.Fatalf("the push failed: %v", err)
	}
	return got
}

func (w *world) wantFailure(err error, reason string) *Error {
	w.t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != reason {
		w.t.Fatalf("error = %v, want a %s", err, reason)
	}
	return failure
}

func (w *world) installHook(content string) {
	w.t.Helper()
	hook := filepath.Join(w.f.Bare, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte(content), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

var base = map[string]string{fileA: "a\n"}

func TestPushesTheCommitsThatTheRepositoryLacksAsAFastForward(t *testing.T) {
	w := newWorld(t, base)
	before := w.remoteTip()
	w.commit(fileA, "a, changed\n")
	tip := w.commit(fileB, "b\n")

	got := w.mustPush()

	if got.Outcome != Pushed || got.Commits != 2 || got.From != before || got.To != tip {
		t.Errorf("result = %+v, want Pushed, 2 commits, from %s to %s", got, before, tip)
	}
	if w.remoteTip() != tip {
		t.Errorf("the repository is at %s, want %s", w.remoteTip(), tip)
	}
	if w.tracking() != tip {
		t.Errorf("the checkout knows the repository to be at %s, want %s: a push to a URL leaves the tracking ref as it was, and the agent moves it", w.tracking(), tip)
	}
	if w.f.InCheckout(w.l, "rev-list", "--count", before+".."+tip) != "2" {
		t.Error("the commits that were pushed are not the two that the checkout made")
	}
}

func TestPushesTheFirstCommitToARepositoryThatHasNoBranch(t *testing.T) {
	w := newWorld(t, nil)
	if w.remoteTip() != "" {
		t.Fatal("the repository has a commit")
	}
	tip := w.commit(fileA, "first\n")

	got := w.mustPush()

	if got.Outcome != Pushed || got.Commits != 1 || got.From != "" || got.To != tip {
		t.Errorf("result = %+v, want Pushed, 1 commit, from nothing to %s", got, tip)
	}
	if w.remoteTip() != tip {
		t.Errorf("the repository is at %q, want the first commit %s", w.remoteTip(), tip)
	}
}

func TestSaysNothingToPushWhenTheCheckoutHasNoCommitOrTheRepositoryHasAllOfThem(t *testing.T) {
	t.Run("the repository has every commit", func(t *testing.T) {
		w := newWorld(t, base)
		sessions := w.f.Sessions()

		got := w.mustPush()

		if got.Outcome != NothingToPush || got.Commits != 0 {
			t.Errorf("result = %+v, want NothingToPush", got)
		}
		if n := w.f.Sessions() - sessions; n != 0 {
			t.Errorf("the repository was asked %d times for nothing to push", n)
		}
		if n := w.fenced.Load(); n != 0 {
			t.Errorf("the writer lock was checked %d times for nothing to push", n)
		}
	})

	t.Run("the checkout has no commit yet", func(t *testing.T) {
		w := newWorld(t, nil)
		sessions := w.f.Sessions()

		got := w.mustPush()

		if got.Outcome != NothingToPush {
			t.Errorf("result = %+v, want NothingToPush", got)
		}
		if n := w.f.Sessions() - sessions; n != 0 {
			t.Errorf("the repository was asked %d times for a checkout with no commit", n)
		}
	})

	t.Run("the repository is ahead of the checkout", func(t *testing.T) {
		w := newWorld(t, base)
		w.f.Publish(branchMain, map[string]string{fileB: "b\n"})
		w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
		sessions := w.f.Sessions()

		got := w.mustPush()

		if got.Outcome != NothingToPush {
			t.Errorf("result = %+v, want NothingToPush: the repository has all of what the checkout has", got)
		}
		if n := w.f.Sessions() - sessions; n != 0 {
			t.Errorf("the repository was asked %d times", n)
		}
	})

	t.Run("a push that Git calls up to date moves the tracking ref and pushes nothing", func(t *testing.T) {
		// The repository got the commit by another way, and the checkout does not know yet.
		w := newWorld(t, base)
		tip := w.commit(fileB, "b\n")
		w.f.Plain(w.f.Bare, "fetch", "-q", w.l.GitDir, "+refs/heads/main:refs/heads/main")
		if w.remoteTip() != tip {
			t.Fatal("the repository did not get the commit")
		}

		got := w.mustPush()

		if got.Outcome != NothingToPush || got.Commits != 0 || got.From != tip || got.To != tip {
			t.Errorf("result = %+v, want NothingToPush, with no commit sent, and the repository at %s", got, tip)
		}
		if w.tracking() != tip {
			t.Errorf("the tracking ref is %s, want %s", w.tracking(), tip)
		}
	})
}

func TestADivergedBranchIsNotForcedAndTheRepositoryIsAskedWhenItMayHaveMoved(t *testing.T) {
	w := newWorld(t, base)
	w.commit(fileB, "mine\n")
	// Another writer pushes to the branch. The checkout has not fetched it, so a push is sent and the host refuses.
	moved := w.f.Publish(branchMain, map[string]string{"other.json": "theirs\n"})

	_, err := w.push()

	failure := w.wantFailure(err, ReasonNonFastForward)
	if !strings.Contains(failure.Message, "not a fast-forward") {
		t.Errorf("message = %q, want it to say that the push is not a fast-forward", failure.Message)
	}
	if w.remoteTip() != moved {
		t.Errorf("the repository is at %s, want %s: the push was not forced", w.remoteTip(), moved)
	}
	if n := w.fenced.Load(); n != 1 {
		t.Errorf("the writer lock was checked %d times, want once before the push", n)
	}
}

func TestABranchThatIsKnownToHaveDivergedIsNotSentToTheHost(t *testing.T) {
	w := newWorld(t, base)
	w.commit(fileB, "mine\n")
	moved := w.f.Publish(branchMain, map[string]string{"other.json": "theirs\n"})
	// A pull fetched the new tip, so the checkout knows that the two have gone different ways.
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	sessions := w.f.Sessions()

	_, err := w.push()

	_ = w.wantFailure(err, ReasonNonFastForward)
	if n := w.f.Sessions() - sessions; n != 0 {
		t.Errorf("the repository was asked %d times for a push that cannot be a fast-forward: that is a load on a host for nothing", n)
	}
	if n := w.fenced.Load(); n != 0 {
		t.Errorf("the writer lock was checked %d times, want none: nothing is written", n)
	}
	if w.remoteTip() != moved {
		t.Error("the repository changed")
	}
}

func TestAHostThatDeclinesIsReportedWithItsOwnWordsAndNothingIsForced(t *testing.T) {
	w := newWorld(t, base)
	w.installHook(hookDeclines)
	before := w.remoteTip()
	w.commit(fileB, "b\n")
	trackedBefore := w.tracking()

	_, err := w.push()

	failure := w.wantFailure(err, ReasonPushRejected)
	for _, want := range []string{"pre-receive hook declined", "GH006: Protected branch update failed for refs/heads/main.", hostWords} {
		if !strings.Contains(failure.Message, want) {
			t.Errorf("message = %q, want it to hold %q", failure.Message, want)
		}
	}
	if w.remoteTip() != before {
		t.Error("the repository changed")
	}
	if w.tracking() != trackedBefore {
		t.Errorf("the tracking ref moved to %s after a push that was declined", w.tracking())
	}
}

func TestARepositoryThatDoesNotAnswerIsAPushFailed(t *testing.T) {
	w := newWorld(t, base)
	w.commit(fileB, "b\n")
	away := w.f.Bare + ".away"
	if err := os.Rename(w.f.Bare, away); err != nil {
		t.Fatal(err)
	}

	_, err := w.push()

	failure := w.wantFailure(err, ReasonPushFailed)
	if !strings.Contains(failure.Message, "cannot be pushed") {
		t.Errorf("message = %q, want it to say that the branch cannot be pushed", failure.Message)
	}
	if err := os.Rename(away, w.f.Bare); err != nil {
		t.Fatal(err)
	}
	// And the next try, with the repository back, pushes.
	if got := w.mustPush(); got.Outcome != Pushed {
		t.Errorf("the try after the outage = %+v, want Pushed", got)
	}
}

func TestNoCredentialOfTheURLIsInWhatIsReported(t *testing.T) {
	const secret = "hunter2-the-password"
	w := newWorld(t, base, func(o *Options) {
		o.URL = "ssh://git:" + secret + "@localhost" + strings.TrimPrefix(o.URL, "ssh://localhost")
	})
	w.installHook("#!/bin/sh\necho 'declined for ssh://git:" + secret + "@localhost/x.git' >&2\nexit 1\n")
	w.commit(fileB, "b\n")

	_, err := w.push()

	failure := w.wantFailure(err, ReasonPushRejected)
	if strings.Contains(failure.Error(), secret) || strings.Contains(failure.Message, secret) {
		t.Errorf("the credential is in the error: %q", failure.Message)
	}
}

func TestTheWriterLockIsCheckedOnceBeforeThePushAndANoPushIsMadeWithoutIt(t *testing.T) {
	w := newWorld(t, base)
	before := w.remoteTip()
	w.commit(fileB, "b\n")
	lost := errors.New("the writer lock is another agent's")
	w.fence = func(context.Context) error { return lost }
	sessions := w.f.Sessions()

	_, err := w.push()

	failure := w.wantFailure(err, ReasonPushFailed)
	if !errors.Is(err, lost) || !strings.Contains(failure.Message, "writer lock") {
		t.Errorf("error = %v, want it to wrap what the fence said and to name the writer lock", err)
	}
	if w.remoteTip() != before {
		t.Error("a push was made without the lock")
	}
	if n := w.f.Sessions() - sessions; n != 0 {
		t.Errorf("the repository was asked %d times without the lock", n)
	}
	if n := w.fenced.Load(); n != 1 {
		t.Errorf("the writer lock was checked %d times, want once", n)
	}
}

func TestTheTrackingRefIsNotMovedWithoutTheWriterLockAndThePushStillCounts(t *testing.T) {
	w := newWorld(t, base)
	known := w.tracking()
	tip := w.commit(fileB, "b\n")
	// The lock is held when the push is made and gone a moment later, when the tracking ref is to be moved.
	w.fence = func(context.Context) error {
		if w.fenced.Load() > 1 {
			return errors.New("the writer lock is another agent's")
		}
		return nil
	}

	got := w.mustPush()

	if got.Outcome != Pushed || got.To != tip || w.remoteTip() != tip {
		t.Fatalf("result = %+v, remote = %s, want the commit pushed: the repository took it", got, w.remoteTip())
	}
	if w.tracking() != known {
		t.Errorf("the tracking ref is %s, want %s: a write that is made without the lock is not made", w.tracking(), known)
	}
	if !strings.Contains(got.Warning, "writer lock") {
		t.Errorf("warning = %q, want it to say that the writer lock could not be checked", got.Warning)
	}
	if n := w.fenced.Load(); n != 2 {
		t.Errorf("the writer lock was checked %d times, want once before the push and once before the tracking ref", n)
	}
}

func TestTheTrackingRefIsMovedByAMoveThatAStopDoesNotCut(t *testing.T) {
	// The push has worked and the agent is told to stop before it has moved the tracking ref: the move is what
	// keeps the count of what waits true, and a short time is all it needs.
	w := newWorld(t, base)
	tip := w.commit(fileB, "b\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := w.pusher.moveTrackingRef(ctx, tip); err != nil {
		t.Fatalf("the move was cut short by a stop that came before it: %v", err)
	}
	if w.tracking() != tip {
		t.Errorf("the tracking ref is %s, want %s", w.tracking(), tip)
	}
}

func TestOnlyTheFlagsOfAPushThatWorkedAreASuccess(t *testing.T) {
	for flag, want := range map[byte]bool{
		'*': true,  // a new branch
		' ': true,  // a fast-forward
		'=': true,  // up to date
		'+': false, // an update by force, which the agent never asks for
		'-': false, // a deletion
		'!': false, // rejected
		'?': false, 0: false,
	} {
		if got := pushedFlag(flag); got != want {
			t.Errorf("pushedFlag(%q) = %v, want %v", flag, got, want)
		}
	}
}

func TestAPushWithAContextThatIsDoneSendsNothing(t *testing.T) {
	w := newWorld(t, base)
	before := w.remoteTip()
	w.commit(fileB, "b\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := w.pusher.Push(ctx); err == nil {
		t.Fatal("a push with a context that is done succeeded")
	}
	if w.remoteTip() != before {
		t.Error("the repository changed")
	}
}

func TestAStopInTheMiddleOfAPushStopsGitAndTheNextPushGoesThrough(t *testing.T) {
	w := newWorld(t, base)
	before := w.remoteTip()
	w.commit(fileB, "b\n")
	// A hook that takes its time, so that the push is still going when the stop comes.
	w.installHook("#!/bin/sh\nwhile :; do :; done\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.pusher.Push(ctx)
		done <- err
	}()
	eventually(t, "the push to begin", func() bool { return w.fenced.Load() == 1 })
	time.Sleep(300 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a push that was stopped succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the push did not stop")
	}
	if w.remoteTip() != before {
		t.Error("the repository changed")
	}
	w.installHook("#!/bin/sh\nexit 0\n")
	if got := w.mustPush(); got.Outcome != Pushed {
		t.Errorf("the push after the stop = %+v, want Pushed", got)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAPushThatWorkedButCouldNotMoveTheTrackingRefIsStillAPushAndSaysSo(t *testing.T) {
	w := newWorld(t, base)
	tip := w.commit(fileB, "b\n")
	lock := filepath.Join(w.l.GitDir, "refs", "remotes", "origin", branchMain+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := w.mustPush()

	if got.Outcome != Pushed || got.To != tip || w.remoteTip() != tip {
		t.Fatalf("result = %+v, remote = %s, want the commit pushed", got, w.remoteTip())
	}
	if !strings.Contains(got.Warning, "tracking") {
		t.Errorf("warning = %q, want it to say that the tracking ref could not be moved", got.Warning)
	}
}

func TestAGitDirectoryThatHoldsMoreThanTheAgentPutThereIsRefusedWithTheReasonOfTheCheckout(t *testing.T) {
	w := newWorld(t, base)
	w.commit(fileB, "b\n")
	if err := os.WriteFile(filepath.Join(w.l.GitDir, "config"), []byte("[core]\n\tfsmonitor = /tmp/evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := w.push()

	_ = w.wantFailure(err, checkout.ReasonGitDirUnsafe)
}

func TestABranchWithASlashIsPushedAsItIs(t *testing.T) {
	f := gittest.New(t)
	f.Publish("feature/x", base)
	l := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, "feature/x")); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{Layout: l, URL: f.URL, Branch: "feature/x", Git: f.GitOptions(), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, l, fileB, "b\n")
	f.InCheckout(l, "add", "-A")
	f.InCheckout(l, "commit", "-q", "-m", "local")

	got, err := p.Push(context.Background())

	if err != nil || got.Outcome != Pushed {
		t.Fatalf("Push = %+v, %v, want Pushed", got, err)
	}
	if tip := strings.TrimSpace(f.Plain(f.Bare, "rev-parse", "refs/heads/feature/x")); tip != f.InCheckout(l, "rev-parse", "HEAD") {
		t.Errorf("the branch of the repository is at %s", tip)
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout("")
	for name, o := range map[string]Options{
		"no URL":                              {Layout: l, Branch: branchMain, Git: f.GitOptions()},
		"no branch":                           {Layout: l, URL: f.URL, Git: f.GitOptions()},
		"a branch with a colon":               {Layout: l, URL: f.URL, Branch: "a:b", Git: f.GitOptions()},
		"a branch with a star":                {Layout: l, URL: f.URL, Branch: "a*", Git: f.GitOptions()},
		"a branch with a plus":                {Layout: l, URL: f.URL, Branch: "+a", Git: f.GitOptions()},
		"a branch that is a flag":             {Layout: l, URL: f.URL, Branch: "-f", Git: f.GitOptions()},
		"a branch with two dots":              {Layout: l, URL: f.URL, Branch: "a..b", Git: f.GitOptions()},
		"a negative timeout":                  {Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: -time.Second},
		"a branch that ends a ref":            {Layout: l, URL: f.URL, Branch: "a.lock", Git: f.GitOptions()},
		"a branch with a space":               {Layout: l, URL: f.URL, Branch: "a b", Git: f.GitOptions()},
		"a branch with a tilde":               {Layout: l, URL: f.URL, Branch: "a~1", Git: f.GitOptions()},
		"a branch with a caret":               {Layout: l, URL: f.URL, Branch: "a^", Git: f.GitOptions()},
		"a branch with a question mark":       {Layout: l, URL: f.URL, Branch: "a?", Git: f.GitOptions()},
		"a branch with a bracket":             {Layout: l, URL: f.URL, Branch: "a[1]", Git: f.GitOptions()},
		"a branch with a backslash":           {Layout: l, URL: f.URL, Branch: "a\\b", Git: f.GitOptions()},
		"a branch with a control character":   {Layout: l, URL: f.URL, Branch: "a\x01b", Git: f.GitOptions()},
		"a branch with a delete character":    {Layout: l, URL: f.URL, Branch: "a\x7fb", Git: f.GitOptions()},
		"a branch with a newline":             {Layout: l, URL: f.URL, Branch: "a\nb", Git: f.GitOptions()},
		"a branch with a start of a revision": {Layout: l, URL: f.URL, Branch: "a@{1}", Git: f.GitOptions()},
		"a branch with two slashes":           {Layout: l, URL: f.URL, Branch: "a//b", Git: f.GitOptions()},
		"a branch with a slash at the end":    {Layout: l, URL: f.URL, Branch: "a/", Git: f.GitOptions()},
		"a branch with a dot at the end":      {Layout: l, URL: f.URL, Branch: "a.", Git: f.GitOptions()},
		"a branch with a slash at the start":  {Layout: l, URL: f.URL, Branch: "/a", Git: f.GitOptions()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(o); err == nil {
				t.Error("New accepted it")
			} else if !strings.HasPrefix(err.Error(), "push: ") {
				t.Errorf("error = %q, want it to begin with push: ", err)
			}
		})
	}
	if _, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions()}); err != nil {
		t.Errorf("New with the defaults = %v, want it accepted: no fence, and the default timeout", err)
	}
}

// The Runner of the pusher is the one of the options it was given, so a watcher of the agent that is told of every
// Git command by those options is told of the push, with the timeout of the push.
func TestAPushIsToldToTheWatcherOfTheAgentLikeAnyGitCommand(t *testing.T) {
	var told []string
	f := gittest.New(t)
	f.Publish(branchMain, base)
	l := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	options := f.GitOptions()
	options.Running = func(timeout time.Duration) func() {
		told = append(told, "start "+timeout.String())
		return func() { told = append(told, "end") }
	}
	p, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: options, Timeout: 7 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, l, fileB, "b\n")
	f.InCheckout(l, "add", "-A")
	f.InCheckout(l, "commit", "-q", "-m", "local")

	if got, err := p.Push(context.Background()); err != nil || got.Outcome != Pushed {
		t.Fatalf("Push = %+v, %v", got, err)
	}

	// The push is the command with the timeout of the push, and it is ended before the next begins.
	var pushes int
	for i, event := range told {
		if event == "start 7m0s" {
			pushes++
			if i+1 >= len(told) || told[i+1] != "end" {
				t.Errorf("the push was not ended before another command began: %q", told)
			}
		}
	}
	if pushes != 1 {
		t.Errorf("the watcher was told of %d commands with the timeout of the push, want 1: %q", pushes, told)
	}
}

func TestWithNoTimeoutAPushMayTakeAsLongAsAFetch(t *testing.T) {
	var told []string
	f := gittest.New(t)
	f.Publish(branchMain, base)
	l := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil {
		t.Fatal(err)
	}
	options := f.GitOptions()
	options.Running = func(timeout time.Duration) func() {
		told = append(told, timeout.String())
		return func() {}
	}
	p, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: options})
	if err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, l, fileB, "b\n")
	f.InCheckout(l, "add", "-A")
	f.InCheckout(l, "commit", "-q", "-m", "local")

	if _, err := p.Push(context.Background()); err != nil {
		t.Fatal(err)
	}

	if want := DefaultTimeout.String(); !slices.Contains(told, want) || DefaultTimeout != 30*time.Minute {
		t.Errorf("the watcher was told %q, want a command of %s, the timeout of a fetch", told, want)
	}
}
