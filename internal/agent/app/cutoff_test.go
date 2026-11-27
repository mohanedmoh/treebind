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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
)

// An update that was cut off in the middle of its files leaves files that the scans take for changes of the application, and the
// commit step commits none of them until the pull has put the update right, whatever the phase of the first pull, the settle time
// or the repository (docs/adr/0028, #232).

const (
	// jsonAdded is a file that the update adds, between jsonA and jsonB in the order in which Git writes the files, and jsonApp a
	// file that the application writes after the update was cut off.
	jsonAdded = "ab.json"
	jsonApp   = "app.json"
	// updatedB is what the update makes of jsonB, and halfB the start of it, which is what a write that was cut off leaves.
	updatedB = "b, updated\n"
	halfB    = "b, upd"
	// updatedC is what the update makes of jsonC.
	updatedC = "c, updated\n"
	// cutInterval is the interval of the loops of these tests: a pull that fails waits one and a half of it for the next, and the
	// settle time and the scans come many times in that.
	cutInterval = time.Minute
)

var (
	// cutBase is what the repository holds before the update, and cutUpdate what the update makes of it.
	cutBase   = map[string]string{jsonA: "a\n", jsonB: "b\n", jsonC: "c\n"}
	cutUpdate = map[string]string{jsonA: updatedA, jsonAdded: "ab\n", jsonB: updatedB, jsonC: updatedC}
)

// gatedWall is a clock of a loop with scans that the test moves: each wait of the loop is a gate that the test passes, and the time
// on the clock moves to the end of the wait when it does. The loop asks for its next wait once the turn before it is in the report.
type gatedWall struct {
	mu    sync.Mutex
	now   time.Time
	asked chan wallGate
}

type wallGate struct {
	c  *gatedWall
	d  time.Duration
	at time.Time
	ch chan time.Time
}

func newGatedWall() *gatedWall { return &gatedWall{now: wallStart, asked: make(chan wallGate, 100)} }

func (c *gatedWall) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gatedWall) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.asked <- wallGate{c: c, d: d, at: c.Now().Add(d), ch: ch}
	return ch
}

// wait returns the wait that the loop is in, once it is in one.
func (c *gatedWall) wait(t *testing.T) wallGate {
	t.Helper()
	select {
	case g := <-c.asked:
		return g
	case <-time.After(20 * time.Second):
		t.Fatal("the loop did not wait: it is stuck in a turn")
		return wallGate{}
	}
}

func (g wallGate) pass() {
	g.c.mu.Lock()
	g.c.now = g.at
	g.c.mu.Unlock()
	g.ch <- g.at
}

// passUntil lets the waits of the loop pass one at a time, from the one it is in, until done holds after a turn, and returns the
// wait that the loop is in then.
func (c *gatedWall) passUntil(t *testing.T, what string, g wallGate, done func() bool) wallGate {
	t.Helper()
	for range 300 {
		g.pass()
		g = c.wait(t)
		if done() {
			return g
		}
	}
	t.Fatalf("%s did not come in 300 turns of the loop", what)
	return g
}

// cutOffBeforeStart makes the checkout at the files of cutBase, as an agent that ran before did, and applies the update of
// cutUpdate with a Git that is killed in the middle of the files, at jsonB, as a kernel that is short of memory does. It returns the
// layout, the commit the checkout is at and the commit the update went to. What the update leaves is jsonA and jsonAdded written
// whole, jsonB not there and jsonC as it was, the journal and the lock of the index; jsonB is then given the start of its new
// content by hand, because Git writes a file with one write and the state of a file that is the start of the new one cannot be had
// on cue.
func cutOffBeforeStart(t *testing.T, f *gittest.Fixture) (where layout.Layout, before, tip string) {
	t.Helper()
	before = f.Publish(testBranch, cutBase)
	where = f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(where, testBranch)); err != nil {
		t.Fatal(err)
	}
	held := f.HoldMerge(jsonB)
	tip = f.Publish(testBranch, cutUpdate)
	options := pull.Options{Layout: where, URL: f.URL, Branch: testBranch, Git: f.GitOptions()}
	options.Git.Git = held.Binary
	killed, err := pull.New(options)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := killed.Pull(context.Background())
		done <- err
	}()
	held.WaitHeld()
	held.Kill()
	if err := <-done; err == nil {
		t.Fatal("the pull of the merge that was killed did not fail")
	}
	expectCutOff(t, where)
	return where, before, tip
}

// expectCutOff checks that an update of cutUpdate was cut off where the tests hold it, and writes the start of jsonB.
func expectCutOff(t *testing.T, where layout.Layout) {
	t.Helper()
	if gittest.Read(t, where, jsonA) != updatedA || gittest.Read(t, where, jsonAdded) != "ab\n" ||
		gittest.Exists(filepath.Join(where.WorkTree, jsonB)) || gittest.Read(t, where, jsonC) != "c\n" {
		t.Fatal("the update was not cut off in the middle of the files: the test does not hold the merge where it means to")
	}
	if cut, err := pull.UpdateCutOff(where); err != nil || !cut {
		t.Fatalf("UpdateCutOff = %v, %v after the merge was killed, want the journal there", cut, err)
	}
	gittest.Write(t, where, jsonB, halfB)
}

// expectWhole checks that every file is what the update made of it.
func expectWhole(t *testing.T, where layout.Layout) {
	t.Helper()
	for name, want := range cutUpdate {
		if got := gittest.Read(t, where, name); got != want {
			t.Errorf("%s is %q, want %q, which the update makes of it", name, got, want)
		}
	}
}

// bothWays starts the agent on the claim of the fixture as a Treebind that syncs in both directions, with the checkout, the pulls,
// the scans and the commits of the packages, no watcher, and the clock of the test. newPuller makes the puller from the options
// that the agent gives, which have the stand-in for ssh of the fixture.
func bothWays(t *testing.T, f *gittest.Fixture, clock *gatedWall, newPuller func(pull.Options) (Puller, error)) *running {
	t.Helper()
	return start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval, o.Config.SettleTime = cutInterval, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.Config.CommitAuthorName, o.Config.CommitAuthorEmail, o.Config.CommitMessageTemplate = authorOfTheTests, emailOfTheTests, templateOfTests
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		o.ScanInterval, o.ScanGap = scanEvery, scanGap
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(options pull.Options) (Puller, error) {
			// The author of the commit of a resolution is the agent's.
			identity := options.Git.Identity
			options.Git = f.GitOptions()
			options.Git.Identity = identity
			return newPuller(options)
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
}

func realPuller(options pull.Options) (Puller, error) { return pull.New(options) }

// untilCommitted lets the loop go on until the Commit check says what came of a commit, which has to be the commit of one file, and
// returns the wait that the loop is in then.
func untilCommitted(t *testing.T, clock *gatedWall, r *running, g wallGate) wallGate {
	t.Helper()
	g = clock.passUntil(t, "a commit", g, func() bool {
		got, _ := commitCheck(t, r)
		return got.Outcome != outcomeSkipped
	})
	if got, _ := commitCheck(t, r); got.Outcome != outcomePassed || !strings.HasPrefix(got.Message, "committed 1 file as ") {
		t.Fatalf("commit = %+v, want the application's file committed, alone", got)
	}
	return g
}

// expectOnlyTheApplicationsFileCommitted checks that the agent made one commit, on the commit given, and that it holds the file
// of the application and nothing of the update.
func expectOnlyTheApplicationsFileCommitted(t *testing.T, f *gittest.Fixture, where layout.Layout, on string) {
	t.Helper()
	if n := f.InCheckout(where, "rev-list", "--count", on+"..HEAD"); n != "1" {
		t.Fatalf("the agent made %s commit(s) on %s, want one", n, shortOf(on))
	}
	if parent := f.InCheckout(where, "rev-parse", "HEAD^"); parent != on {
		t.Errorf("the commit of the agent is on %s, want it on %s", shortOf(parent), shortOf(on))
	}
	if files := f.InCheckout(where, "diff-tree", "-r", "--no-commit-id", "--name-only", "HEAD"); files != jsonApp {
		t.Errorf("the commit of the agent holds %q, want only %s, the application's", files, jsonApp)
	}
}

func TestAStartThatFindsAnUpdateCutOffPullsBeforeItScansAndCommitsOnlyWhatTheApplicationWrote(t *testing.T) {
	f := gittest.New(t)
	where, _, tip := cutOffBeforeStart(t, f)
	clock := newGatedWall()
	r := bothWays(t, f, clock, realPuller)

	// The phase of the first pull is not waited for: it is due at once, with the first scan, and is taken first.
	first := clock.wait(t)
	if got, _ := pullCheck(t, r); first.d != 0 || !strings.Contains(got.Message, "the first pull is due at once, because an update") {
		t.Fatalf("the first wait is %v with the pull saying %q, want the first pull at once, saying why", first.d, got.Message)
	}
	// The application writes a file after the start.
	gittest.Write(t, where, jsonApp, "the application's\n")

	first.pass()
	g := clock.wait(t)
	got, _ := pullCheck(t, r)
	for _, want := range []string{
		"advanced the checkout", "was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed",
	} {
		if got.Outcome != outcomePassed || !strings.Contains(got.Message, want) {
			t.Errorf("pull = %+v, want it passed, with %q said", got, want)
		}
	}
	if scanned, _ := changesCheck(t, r); !strings.HasPrefix(scanned.Message, "running:") {
		t.Errorf("changes = %+v after the first turn, want no scan yet: the pull comes first", scanned)
	}
	expectWhole(t, where)
	if head := f.InCheckout(where, "rev-parse", "HEAD"); head != tip {
		t.Errorf("the checkout is at %s, want the commit of the update, %s", shortOf(head), shortOf(tip))
	}

	// What the application wrote settles and is committed, alone.
	untilCommitted(t, clock, r, g)
	expectOnlyTheApplicationsFileCommitted(t, f, where, tip)
}

// cutOnce is a puller whose first pull has its merge held in the middle of the files, for the test to kill, and whose pulls after
// it are the real ones. It says on began that its first pull began.
type cutOnce struct {
	first, rest Puller
	began       chan struct{}
	once        sync.Once
}

func (p *cutOnce) Pull(ctx context.Context) (pull.Result, error) {
	called := false
	p.once.Do(func() { called = true })
	if called {
		p.began <- struct{}{}
		return p.first.Pull(ctx)
	}
	return p.rest.Pull(ctx)
}

// Recover is the repair of the real puller, which is the one that puts an update right.
func (p *cutOnce) Recover(ctx context.Context) (string, error) {
	if repairer, ok := p.rest.(Repairer); ok {
		return repairer.Recover(ctx)
	}
	return "", nil
}

func TestAnUpdateThatFailsWhileTheAgentRunsIsPutRightAtOnceAndTheCommitsDoNotWaitForTheNextPull(t *testing.T) {
	f := gittest.New(t)
	before := f.Publish(testBranch, cutBase)
	where := f.Layout(subDir)
	if _, err := checkout.Open(context.Background(), f.CheckoutOptions(where, testBranch)); err != nil {
		t.Fatal(err)
	}
	held := f.HoldMerge(jsonB)
	tip := f.Publish(testBranch, cutUpdate)
	clock := newGatedWall()
	began := make(chan struct{}, 1)
	r := bothWays(t, f, clock, func(options pull.Options) (Puller, error) {
		rest, err := pull.New(options)
		if err != nil {
			return nil, err
		}
		options.Git.Git = held.Binary
		first, err := pull.New(options)
		if err != nil {
			return nil, err
		}
		return &cutOnce{first: first, rest: rest, began: began}, nil
	})

	// The scans go on until the first pull, which is held in the middle of the files of the update and killed there, as a kernel
	// that is short of memory kills the Git of a running agent.
	g := clock.wait(t)
	for turns := 0; ; turns++ {
		if turns == 300 {
			t.Fatal("the first pull did not come in 300 turns of the loop")
		}
		g.pass()
		select {
		case g = <-clock.asked:
			continue
		case <-began:
		case <-time.After(20 * time.Second):
			t.Fatal("the loop did not wait and did not pull")
		}
		break
	}
	held.WaitHeld()
	held.Kill()
	g = clock.wait(t)
	cutAt := clock.Now()

	// The pull failed, and the agent put the update right before it waited for the next one: what the update wrote is back as it
	// was, and the journal is gone, which is what the commit step waits for.
	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonPullFailed ||
		!strings.Contains(got.Message, "was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed") {
		t.Fatalf("pull = %+v, want it failed, with the update undone at once", got)
	}
	if cut, err := pull.UpdateCutOff(where); err != nil || cut {
		t.Fatalf("UpdateCutOff = %v, %v after the pull that failed, want the journal gone", cut, err)
	}
	for name, want := range cutBase {
		if got := gittest.Read(t, where, name); got != want {
			t.Errorf("%s is %q after the undo, want %q, as the commit before the update has it", name, got, want)
		}
	}
	if gittest.Exists(filepath.Join(where.WorkTree, jsonAdded)) {
		t.Errorf("%s, which the update added, is still there", jsonAdded)
	}

	// What the application writes after that is committed within the settle time and the next scan, long before the next pull, and
	// none of the files of the update is in the commit.
	gittest.Write(t, where, jsonApp, "the application's\n")
	g = untilCommitted(t, clock, r, g)
	if waited := clock.Now().Sub(cutAt); waited >= cutInterval {
		t.Errorf("the commit came %v after the cut, want it before the pull of the next interval: it waited for the pull", waited)
	}
	expectOnlyTheApplicationsFileCommitted(t, f, where, before)

	// The next pull applies the update on top of the application's commit, and both are kept.
	clock.passUntil(t, "the pull that applies the update", g, func() bool {
		got, _ := pullCheck(t, r)
		return got.Outcome == outcomePassed
	})
	expectWhole(t, where)
	if gittest.Read(t, where, jsonApp) != "the application's\n" {
		t.Error("the application's file was lost")
	}
	// The commit of the update is under the checkout: it fails the test when it is not.
	f.InCheckout(where, "merge-base", "--is-ancestor", tip, "HEAD")
}

func TestAStartThatFindsAnUpdateCutOffAndCannotReachTheRepositoryCommitsNoneOfTheFilesOfTheUpdate(t *testing.T) {
	f := gittest.New(t)
	where, before, tip := cutOffBeforeStart(t, f)
	clock := newGatedWall()
	r := bothWays(t, f, clock, realPuller)
	first := clock.wait(t)

	// The repository goes away after the checkout, before the first pull.
	away := f.Bare + ".away"
	if err := os.Rename(f.Bare, away); err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, where, jsonApp, "the application's\n")
	first.pass()
	g := clock.wait(t)

	// The pull puts the update right before it asks the repository, and fails for the repository.
	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonFetchFailed ||
		!strings.Contains(got.Message, "was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed") {
		t.Fatalf("pull = %+v, want FetchFailed, with the update undone", got)
	}
	for name, want := range cutBase {
		if got := gittest.Read(t, where, name); got != want {
			t.Errorf("%s is %q after the undo, want %q, as the commit before the update has it", name, got, want)
		}
	}
	if gittest.Exists(filepath.Join(where.WorkTree, jsonAdded)) {
		t.Errorf("%s, which the update added, is still there", jsonAdded)
	}

	// What the application wrote is committed while the repository is away, and nothing of the update is.
	g = untilCommitted(t, clock, r, g)
	expectOnlyTheApplicationsFileCommitted(t, f, where, before)

	// When the repository is back, the update and the application's file meet as two sides, and both are kept.
	if err := os.Rename(away, f.Bare); err != nil {
		t.Fatal(err)
	}
	clock.passUntil(t, "the pull after the repository is back", g, func() bool {
		got, _ := pullCheck(t, r)
		return got.Outcome == outcomePassed
	})
	expectWhole(t, where)
	if gittest.Read(t, where, jsonApp) != "the application's\n" {
		t.Error("the application's file was lost")
	}
	// The commit of the update is under the checkout: it fails the test when it is not.
	f.InCheckout(where, "merge-base", "--is-ancestor", tip, "HEAD")
}

// The Commit check while the commit step waits for an update that was cut off, with a committer that says so: a check that is
// skipped, logged once, and that says what the commit waits for only while the journal is there.
func TestTheCommitCheckSaysThatItWaitsForAnUpdateThatWasCutOffOnlyWhileTheJournalIsThere(t *testing.T) {
	claim := gittest.TempDir(t)
	journal := filepath.Join(claim, config.StateDirName, "updating")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newWorld()
	w.committer.answer = func(int, []scan.Change) (commit.Result, error) {
		return commit.Result{Outcome: commit.Deferred}, nil
	}
	r := start(t, w.with(func(o *Options) { o.Config.MountPath = claim }))
	w.settle(t, changed(fileOne, scan.Modified, 1))
	w.handedOn(t)

	got, _ := commitCheck(t, r)
	if got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || got.Message != commitWaitsForRepair {
		t.Fatalf("commit = %+v, want it skipped, waiting for the update to be put right", got)
	}
	// The files have settled and are given again at each scan, and the wait is in the log once.
	w.next(t, found(changed(fileOne, scan.Modified, 1)))
	w.handedOn(t)
	if n := r.logs.count("msg=commit check=Commit outcome=Skipped reason=NotChecked"); n != 1 {
		t.Errorf("the wait was logged %d times, want once:\n%s", n, r.logs.String())
	}

	// A scan that finds nothing settled leaves the wait while the journal is there, and ends it once it is gone.
	w.next(t, found())
	if got, _ := commitCheck(t, r); got.Message != commitWaitsForRepair {
		t.Errorf("commit = %+v with the journal there, want it still waiting", got)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	w.next(t, found(changed(fileTwo, scan.Added, 1)))
	if got, _ := commitCheck(t, r); got.Outcome != outcomeSkipped || got.Message != commitNotSettled {
		t.Errorf("commit = %+v once the journal is gone, want it saying that no file has settled", got)
	}
	if n := w.committer.calls.Load(); n != 2 {
		t.Errorf("the committer was called %d times, want twice: at the two scans that found files settled", n)
	}
}

// The first pull of a start that finds no journal waits for its phase, as it always did.
func TestTheFirstPullOfAStartThatFindsNoUpdateCutOffWaitsForItsPhase(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, cutBase)
	f.Layout(subDir)
	clock := newGatedWall()
	r := bothWays(t, f, clock, realPuller)

	first := clock.wait(t)
	if got, _ := pullCheck(t, r); !strings.Contains(got.Message, "waiting: the first pull is due in ") {
		t.Errorf("pull = %+v, want the first pull due after the phase", got)
	}
	// The first scan comes at once, and the first pull after the phase.
	clock.passUntil(t, "the first pull", first, func() bool {
		got, _ := pullCheck(t, r)
		return got.Outcome == outcomePassed
	})
	phase := syncloop.Phase(testNS, testName, cutInterval)
	if phase < scanEvery {
		t.Fatalf("the phase of the tests is %v, want one longer than a scan interval, for the test to tell", phase)
	}
	if pulled := clock.Now().Sub(wallStart); pulled != phase {
		t.Errorf("the first pull came %v after the start, want it after its phase, %v", pulled, phase)
	}
}

// firstPullDue is the phase of the first pull, and at once only when the journal of an update that was cut off is in the state
// directory. A state directory that cannot be looked at is not taken for one: the phase stays, and the Pull check does not say that
// an update was cut off.
func TestTheFirstPullIsDueAtOnceOnlyWhenTheJournalIsThere(t *testing.T) {
	const phase = 3900 * time.Millisecond
	state := filepath.Join(gittest.TempDir(t), config.StateDirName)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	l := layout.Layout{StateDir: state}

	if got, said := firstPullDue(l, phase); got != phase || !strings.Contains(said, "due in 4s") {
		t.Errorf("with no journal the first pull is due after %s and says %q, want the phase and the time of it", got, said)
	}

	if err := os.WriteFile(filepath.Join(state, "updating"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, said := firstPullDue(l, phase); got != 0 || !strings.Contains(said, "at once") || !strings.Contains(said, "cut off") {
		t.Errorf("with a journal the first pull is due after %s and says %q, want it at once and why", got, said)
	}

	if os.Geteuid() == 0 {
		t.Skip("root can look at a directory that has no permissions")
	}
	if err := os.Chmod(state, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })
	if got, said := firstPullDue(l, phase); got != phase || strings.Contains(said, "cut off") {
		t.Errorf("with a state directory that cannot be looked at the first pull is due after %s and says %q, want the phase and no "+
			"update that was cut off", got, said)
	}
}
