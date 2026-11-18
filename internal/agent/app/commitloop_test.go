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

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	authorOfTheTests = "treebind-bot"
	emailOfTheTests  = "bot@example.org"
	templateOfTests  = "treebind: sync {{.Count}} file(s)"

	// messageBoom is the message of a commit of the tests that fails.
	messageBoom = "the changes of the directory cannot be committed: boom"
)

func commitCheck(t *testing.T, r *running) (status.Check, bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckCommit {
			return c, true
		}
	}
	return status.Check{}, false
}

// settle takes one file through the scans that make it settle, which are three, and returns once the scan that
// finds it settled is in the report, with the commit that follows it.
func (w *world) settle(t *testing.T, change scan.Change) {
	t.Helper()
	w.scanner.awaitBegun(t)
	for range 3 {
		w.next(t, found(change))
	}
}

// handedOn is what the committer was given by its next call.
func (w *world) handedOn(t *testing.T) []scan.Change {
	t.Helper()
	select {
	case got := <-w.settled:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("the committer was not called")
		return nil
	}
}

func TestCommitsTheFilesThatHaveSettledAndSaysSoInTheReport(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	if got := w.handedOn(t); len(got) != 1 || got[0].Path != fileOne {
		t.Errorf("the committer was given %v, want %s", got, fileOne)
	}
	got, ok := commitCheck(t, r)
	if !ok || got.Outcome != outcomePassed || got.Reason != "" {
		t.Fatalf("commit = %+v, want it passed", got)
	}
	if want := "committed 1 file as " + shortOf(testCommit); got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	if names := checkNames(t, r); !strings.HasSuffix(names, ",Changes,Commit") {
		t.Errorf("checks = %s, want the commit after the changes", names)
	}
}

func TestOnlyTheFilesThatHaveSettledAreHandedToTheCommitter(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	// One file stays as it is, and the other is written at every scan.
	for size := int64(1); size <= 3; size++ {
		w.next(t, found(changed(fileOne, scan.Modified, 1), changed(fileTwo, scan.Added, size)))
	}

	got := w.handedOn(t)
	if len(got) != 1 || got[0].Path != fileOne {
		t.Fatalf("the committer was given %v, want only %s, which settled", got, fileOne)
	}
	if check, _ := changesCheck(t, r); !strings.Contains(check.Message, "1 settled, 1 waiting") {
		t.Errorf("changes = %q, want one settled and one waiting", check.Message)
	}
}

func TestTheCommitCheckSaysItIsNotTriedUntilAFileSettles(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, found(changed(fileOne, scan.Modified, 1))) // seen, not settled

	got, ok := commitCheck(t, r)
	if !ok || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "no file has settled yet") {
		t.Errorf("commit = %+v, want it skipped, because no file has settled yet", got)
	}
	if n := w.committer.calls.Load(); n != 0 {
		t.Errorf("the committer was called %d times before a file settled", n)
	}
}

func TestSaysTheCommitWasNotTriedWhileTheCheckoutIsNotMade(t *testing.T) {
	w := newWorld()
	started := make(chan struct{})
	r := start(t, w.with(func(o *Options) {
		o.Checkout = func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
			close(started)
			<-ctx.Done()
			return checkout.Result{}, ctx.Err()
		}
	}))
	<-started

	eventually(t, "the commit row", func() bool {
		got, ok := commitCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "checkout is not made yet")
	})
}

func TestSaysNothingToCommitWhenTheFilesThatSettledCameToNothing(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{Outcome: commit.NothingToCommit}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "nothing to commit") {
		t.Errorf("commit = %+v, want it passed, with nothing to commit", got)
	}
}

func TestSaysHowManyFilesWereGoneBeforeTheyCouldBeCommitted(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: 3, Gone: 2}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if want := "committed 3 files as " + shortOf(testCommit) + "; 2 files that had settled were gone before they could be committed"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
}

func TestSaysOneFileThatWasGoneInTheSingular(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: 1, Gone: 1}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if want := "committed 1 file as " + shortOf(testCommit) + "; 1 file that had settled was gone before it could be committed"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
}

func TestSaysTheCommitWasNotTriedWhileAVolumeCheckFails(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.Preflight = func(context.Context, string, string) []preflight.Result { return failed }
	}))

	eventually(t, "the commit row", func() bool {
		got, ok := commitCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "a volume check failed")
	})
}

func TestSaysTheCommitWasNotTriedWhenTheScanCannotBeSetUp(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.NewScanner = func(scan.Options) (Scanner, error) { return nil, errors.New("the scanner cannot be made") }
	}))

	eventually(t, "the commit row", func() bool {
		got, ok := commitCheck(t, r)
		return ok && got.Outcome == outcomeSkipped && got.Reason == preflight.ReasonNotChecked && strings.Contains(got.Message, "the scan cannot be set up")
	})
	if n := len(w.commitOpt); n != 0 {
		t.Errorf("%d committers were made for scans that cannot be set up", n)
	}
}

func TestACommitThatFailsIsReportedWithItsReasonAndTriedAgainAtTheNextScan(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(n int, _ []scan.Change) (commit.Result, error) {
		if n == 1 {
			return commit.Result{}, &commit.Error{Reason: commit.ReasonCommitFailed, Message: "the changes of the directory cannot be committed: git commit failed"}
		}
		return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: 1}, nil
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != commit.ReasonCommitFailed || !strings.Contains(got.Message, "git commit failed") {
		t.Fatalf("commit = %+v, want it failed, with CommitFailed and what Git said", got)
	}
	if check, _ := changesCheck(t, r); check.Outcome != outcomePassed {
		t.Errorf("changes = %+v, want the scans to pass while the commit fails", check)
	}

	// The file is still there, so the next scan hands it over again.
	w.next(t, found(changed(fileOne, scan.Modified, 1)))

	if got, _ := commitCheck(t, r); got.Outcome != outcomePassed || !strings.Contains(got.Message, "committed 1 file") {
		t.Errorf("commit = %+v, want the second try to have passed", got)
	}
	if n := w.committer.calls.Load(); n != 2 {
		t.Errorf("the committer was called %d times, want 2", n)
	}
}

func TestAFailedCommitIsTakenOutOfTheReportWhenTheScansFindNothingMoreToCommit(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{}, &commit.Error{Reason: commit.ReasonCommitFailed, Message: messageBoom}
	}
	r := start(t, w.with())
	w.settle(t, changed(fileOne, scan.Modified, 1))
	if got, _ := commitCheck(t, r); got.Outcome != outcomeFailed {
		t.Fatalf("commit = %+v, want it failed", got)
	}

	// The application puts the file back, so the scans find nothing, and the failure is about nothing that is there.
	w.next(t, scanAnswer{})

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "no file has settled yet") {
		t.Errorf("commit = %+v, want it skipped, because no file has settled yet", got)
	}
}

func TestTheSameFailureThatComesBackAfterItWasTakenOutIsLoggedAgain(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{}, &commit.Error{Reason: commit.ReasonCommitFailed, Message: messageBoom}
	}
	r := start(t, w.with())
	w.settle(t, changed(fileOne, scan.Modified, 1))
	w.next(t, scanAnswer{}) // nothing is left, and the failure leaves the report

	// The file comes back, settles, and fails in the same words.
	for range 3 {
		w.next(t, found(changed(fileOne, scan.Modified, 1)))
	}

	if n := r.logs.count("check=Commit outcome=Failed"); n != 2 {
		t.Errorf("the failure was logged %d times, want twice: it went, and it came back\n%s", n, r.logs.String())
	}
}

func TestAFailedCommitStaysWhileFilesAreStillThere(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{}, &commit.Error{Reason: commit.ReasonCommitFailed, Message: messageBoom}
	}
	r := start(t, w.with())
	w.settle(t, changed(fileOne, scan.Modified, 1))

	// Another file is written, and has not settled: the volume is as it was, and the commit that failed is not forgotten.
	w.next(t, found(changed(fileTwo, scan.Added, 7)))

	if got, _ := commitCheck(t, r); got.Outcome != outcomeFailed || got.Reason != commit.ReasonCommitFailed {
		t.Errorf("commit = %+v, want the failure to stay while there are changes", got)
	}
}

func TestACommitThatPassedStaysInTheReportWhenTheScansFindNothing(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.settle(t, changed(fileOne, scan.Modified, 1))

	w.next(t, scanAnswer{}) // the commit made them go, and the scan finds nothing

	if got, _ := commitCheck(t, r); got.Outcome != outcomePassed || !strings.HasPrefix(got.Message, "committed 1 file as ") {
		t.Errorf("commit = %+v, want the last commit to stay: it says what was committed last", got)
	}
}

func TestACommitterThatCannotBeMadeStaysFailedWhenTheScansFindNothing(t *testing.T) {
	w := newWorld()
	w.commitErr = errors.New("commit: the author name is empty")
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, scanAnswer{})
	w.next(t, scanAnswer{})

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeFailed || !strings.Contains(got.Message, "the commit cannot be set up") {
		t.Errorf("commit = %+v, want the failure to stay: it is about the settings and not about the files", got)
	}
	if !strings.Contains(got.Message, "not committed until") {
		t.Errorf("message = %q, want it to say that the settled files wait for the settings to be put right", got.Message)
	}
}

func TestAGitDirectoryThatIsNotTrustedKeepsItsOwnReasonInTheCommitCheck(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{}, &commit.Error{Reason: checkout.ReasonGitDirUnsafe, Message: "the Git directory holds more than the agent put there"}
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	if got, _ := commitCheck(t, r); got.Outcome != outcomeFailed || got.Reason != checkout.ReasonGitDirUnsafe {
		t.Errorf("commit = %+v, want it failed with %s, as the checkout and the pull say it", got, checkout.ReasonGitDirUnsafe)
	}
}

func TestAnErrorThatIsNotOfTheCommitPackageIsACommitFailed(t *testing.T) {
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{}, errors.New("the volume is read only")
	}
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != commit.ReasonCommitFailed || !strings.Contains(got.Message, "the volume is read only") {
		t.Errorf("commit = %+v, want a CommitFailed that says why", got)
	}
}

func TestACommitterThatCannotBeMadeIsAFailedCheckAndTheScansGoOn(t *testing.T) {
	w := newWorld()
	w.commitErr = errors.New(`commit: the author name "bot <x>" must not contain '<' or '>'`)
	r := start(t, w.with())

	w.settle(t, changed(fileOne, scan.Modified, 1))

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != commit.ReasonCommitFailed {
		t.Fatalf("commit = %+v, want it failed", got)
	}
	for _, want := range []string{"the commit cannot be set up", "the author name"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want it to say %q", got.Message, want)
		}
	}
	if check, _ := changesCheck(t, r); check.Outcome != outcomePassed || !strings.Contains(check.Message, "1 settled") {
		t.Errorf("changes = %+v, want the scans to go on", check)
	}
	if n := w.committer.calls.Load(); n != 0 {
		t.Errorf("a committer that was not made was called %d times", n)
	}
}

func TestTheCommitterIsMadeWithTheSettingsOfTheSpecAndTheFenceOfTheAgent(t *testing.T) {
	w := newWorld()
	start(t, w.with(func(o *Options) {
		o.Config.CommitAuthorName, o.Config.CommitAuthorEmail, o.Config.CommitMessageTemplate = authorOfTheTests, emailOfTheTests, templateOfTests
	}))

	var options commit.Options
	select {
	case options = <-w.commitOpt:
	case <-time.After(10 * time.Second):
		t.Fatal("no committer was made")
	}

	if options.AuthorName != authorOfTheTests || options.AuthorEmail != emailOfTheTests || options.MessageTemplate != templateOfTests {
		t.Errorf("the committer has %q <%s> and %q, want the settings of the spec", options.AuthorName, options.AuthorEmail, options.MessageTemplate)
	}
	if options.Layout.WorkTree != workTreeOfTheTests || options.Layout.GitDir != gitDirOfTheTests {
		t.Errorf("the layout is %q and %q, want the work tree and the Git directory of the checkout", options.Layout.WorkTree, options.Layout.GitDir)
	}
	if options.Git.SSH.IdentityFile != testKeyFile {
		t.Errorf("the Git options have the key %q, want the one of the spec: %q", options.Git.SSH.IdentityFile, testKeyFile)
	}
	if options.Fence == nil {
		t.Fatal("the committer has no fence: it would write without the writer lock")
	}
	if err := options.Fence(context.Background()); err != nil {
		t.Errorf("the fence says %v for an agent that has no lock to lose", err)
	}
}

func TestTheFenceOfTheCommitterIsTheOneOfTheLockThatTheAgentHolds(t *testing.T) {
	root, lockDir := claimWithState(t)
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		takesTheLock(root, nil)(o)
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		// The clock of this agent does not move, so it makes no heartbeat, and another agent sees the lock
		// unchanged for a whole TTL and takes it over.
		o.LockClock = newFakeClock()
	}))
	options := <-w.commitOpt
	if err := options.Fence(context.Background()); err != nil {
		t.Fatalf("the fence says %v while the agent holds the lock", err)
	}

	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	if err := options.Fence(context.Background()); !errors.Is(err, lock.ErrLost) {
		t.Errorf("the fence of the committer says %v, want the loss of the lock that another agent took", err)
	}
	if stopped := r.waitStopped(t); !errors.Is(stopped, ErrLockLost) {
		t.Errorf("Run returned %v, want ErrLockLost: a commit that finds the lock gone stops the agent", stopped)
	}
}

func TestMakesNoCommitterAndNoCheckForAPullTreebindOrWhenNoneIsGiven(t *testing.T) {
	t.Run("the direction is pull", func(t *testing.T) {
		w := newWorld()
		r := start(t, w.with(func(o *Options) { o.Config.SyncDirection = config.DirectionPull }))
		eventually(t, "the pull to pass", func() bool {
			got, found := pullCheck(t, r)
			return found && got.Outcome == outcomePassed
		})
		if _, found := commitCheck(t, r); found || len(w.commitOpt) != 0 {
			t.Errorf("a pull Treebind has a Commit check = %v and %d committers: it has none", found, len(w.commitOpt))
		}
	})
	t.Run("no committer is given", func(t *testing.T) {
		w := newWorld()
		r := start(t, w.with(func(o *Options) { o.NewCommitter = nil }))
		w.scanner.awaitBegun(t)
		w.next(t, scanAnswer{})
		if _, found := commitCheck(t, r); found {
			t.Error("an agent with no committer has a Commit check")
		}
	})
}

func TestRemovesTheLockFilesAStoppedGitLeftBeforeACommitAndSaysSo(t *testing.T) {
	root, _ := claimWithState(t)
	gitDir := filepath.Join(root, ".treebind", "git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(gitDir, "index.lock")
	w := newWorld()
	r := start(t, w.with(func(o *Options) { o.Config.MountPath = root }))
	// The checkout and the pulls clear what they find before they run, so the file is put there once the first
	// scan has begun, which is after the checkout, and the pull is due long after the file settles.
	w.scanner.awaitBegun(t)
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w.next(t, found(changed(fileOne, scan.Modified, 1)))
	}

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "index.lock") {
		t.Errorf("commit = %+v, want it passed, and to say which lock file it removed", got)
	}
	if gittest.Exists(stale) {
		t.Error("the lock file is still there when the commit was made")
	}
}

// blockingCommitter does not return until the agent is told to stop.
type blockingCommitter struct{ began chan struct{} }

func (b blockingCommitter) Commit(ctx context.Context, _ []scan.Change) (commit.Result, error) {
	close(b.began)
	<-ctx.Done()
	return commit.Result{}, ctx.Err()
}

func TestAStopInTheMiddleOfACommitSaysNothingOfItAndTheAgentStops(t *testing.T) {
	w := newWorld()
	blocking := blockingCommitter{began: make(chan struct{})}
	r := start(t, w.with(func(o *Options) {
		o.NewCommitter = func(commit.Options) (Committer, error) { return blocking, nil }
	}))
	// The third answer is the scan at which the file settles, and the commit that follows it does not return.
	for range 3 {
		w.scanner.answers <- found(changed(fileOne, scan.Modified, 1))
	}
	select {
	case <-blocking.began:
	case <-time.After(10 * time.Second):
		t.Fatal("the commit did not begin")
	}

	r.cancel()

	if err := r.waitStopped(t); err != nil {
		t.Errorf("Run returned %v, want nil for a stop", err)
	}
	if n := r.logs.count("check=Commit outcome=Failed"); n != 0 {
		t.Errorf("a commit that was cut short by a stop was logged as a failure %d times:\n%s", n, r.logs.String())
	}
}

// The whole of it on a real repository: the application writes a file, it settles, and the commit is in the
// checkout, by the author and with the message of the spec.
func TestCommitsWhatTheApplicationWritesOnARealRepository(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n"})
	where := f.Layout(subDir)
	clock := newTickWall()
	r := start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.Config.CommitAuthorName, o.Config.CommitAuthorEmail, o.Config.CommitMessageTemplate = authorOfTheTests, emailOfTheTests, templateOfTests
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		o.ScanInterval, o.ScanGap = scanEvery, scanGap
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(options pull.Options) (Puller, error) {
			options.Git = f.GitOptions()
			return pull.New(options)
		}
		o.NewScanner = func(options scan.Options) (Scanner, error) {
			options.Git = f.GitOptions()
			return scan.New(options)
		}
		o.NewCommitter = func(options commit.Options) (Committer, error) {
			options.Git = f.GitOptions()
			return commit.New(options)
		}
	})
	eventually(t, "the checkout", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomePassed
	})
	before := f.InCheckout(where, "rev-parse", "HEAD")

	gittest.Write(t, where, jsonB, "written by the application\n")
	if err := os.Remove(filepath.Join(where.WorkTree, jsonA)); err != nil {
		t.Fatal(err)
	}

	// Each file settles at a scan of its own, so the two changes may be in one commit or in two. What holds is that
	// every commit is by the author of the spec with the message of its template, and that together they have both.
	eventually(t, "the commits of both changes", func() bool {
		got, found := commitCheck(t, r)
		changes, _ := changesCheck(t, r)
		return found && got.Outcome == outcomePassed && strings.HasPrefix(got.Message, "committed ") &&
			changes.Message != "" && strings.HasPrefix(changes.Message, "no local changes")
	})
	subjects := f.InCheckout(where, "log", before+"..HEAD", "--format=%an|%ae|%cn|%ce|%s")
	if subjects == "" {
		t.Fatal("HEAD did not move")
	}
	for line := range strings.SplitSeq(subjects, "\n") {
		if !strings.HasPrefix(line, "treebind-bot|bot@example.org|treebind-bot|bot@example.org|treebind: sync ") || !strings.HasSuffix(line, " file(s)") {
			t.Errorf("a commit of the agent is %q, want the author and the message of the spec", line)
		}
	}
	if names := f.InCheckout(where, "diff", "--name-status", before, "HEAD"); !strings.Contains(names, "A\t"+jsonB) || !strings.Contains(names, "D\t"+jsonA) {
		t.Errorf("the commits changed %q, want %s added and %s deleted", names, jsonB, jsonA)
	}
	eventually(t, "the scan that finds nothing left", func() bool {
		got, found := changesCheck(t, r)
		return found && got.Outcome == outcomePassed && strings.HasPrefix(got.Message, "no local changes")
	})
}
