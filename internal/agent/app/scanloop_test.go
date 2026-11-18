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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

const (
	scanEvery = 3 * time.Second
	scanGap   = 2 * time.Second
	settleFor = 5 * time.Second
	fileOne   = "one.json"
	fileTwo   = "two.json"

	// Where the claim of the tests is mounted, the synced directory of the one with a sub path, and the Git
	// directory.
	claimOfTheTests    = "/work"
	workTreeOfTheTests = "/work/dashboards"
	gitDirOfTheTests   = "/work/.treebind/git"
)

// tickWall is a clock that moves only when a wait passes, by the length of the wait, so that no test
// waits. A wait does not pass while hold says so, which is how a test lets the loop see a signal first.
type tickWall struct {
	mu   sync.Mutex
	now  time.Time
	hold func() bool
}

var wallStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTickWall() *tickWall { return &tickWall{now: wallStart} }

func (c *tickWall) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *tickWall) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if c.hold == nil || !c.hold() {
		c.now = c.now.Add(d)
		ch <- c.now
	}
	return ch
}

// seconds is the time on the clock since it began.
func (c *tickWall) seconds() time.Duration { return c.Now().Sub(wallStart) }

// fakeScanner is a scanner that a test answers one scan at a time: a scan begins, which is said on began
// with the time on the clock, and ends when the test gives it an answer.
type fakeScanner struct {
	clock   *tickWall
	began   chan time.Duration
	answers chan scanAnswer
}

type scanAnswer struct {
	result scan.Result
	err    error
}

func newFakeScanner(clock *tickWall) *fakeScanner {
	return &fakeScanner{clock: clock, began: make(chan time.Duration, 100), answers: make(chan scanAnswer, 100)}
}

func (f *fakeScanner) Scan(ctx context.Context) (scan.Result, error) {
	f.began <- f.clock.seconds()
	select {
	case a := <-f.answers:
		return a.result, a.err
	case <-ctx.Done():
		return scan.Result{}, ctx.Err()
	}
}

// awaitBegun waits for a scan to begin, and returns when, on the clock.
func (f *fakeScanner) awaitBegun(t *testing.T) time.Duration {
	t.Helper()
	select {
	case at := <-f.began:
		return at
	case <-time.After(10 * time.Second):
		t.Fatal("no scan began")
		return 0
	}
}

// answer gives the scan that has begun its answer, and waits for the next to begin, which is when the
// answer is in the report. It returns when the next began.
func (f *fakeScanner) answer(t *testing.T, a scanAnswer) time.Duration {
	t.Helper()
	f.answers <- a
	return f.awaitBegun(t)
}

func changed(path string, kind scan.Kind, size int64) scan.Change {
	return scan.Change{Path: path, Kind: kind, Seen: scan.Seen{Present: true, Size: size}}
}

func found(changes ...scan.Change) scanAnswer {
	return scanAnswer{result: scan.Result{Changes: changes}}
}

// fakeWatcher stands in for the watcher of the directory.
type fakeWatcher struct {
	events    chan struct{}
	stats     watch.Stats
	refreshes atomic.Int32
	closed    atomic.Bool
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{events: make(chan struct{}, 1)} }

func (w *fakeWatcher) Events() <-chan struct{} { return w.events }
func (w *fakeWatcher) Refresh()                { w.refreshes.Add(1) }
func (w *fakeWatcher) Stats() watch.Stats      { return w.stats }
func (w *fakeWatcher) Close() error            { w.closed.Store(true); return nil }

// autoPuller is a puller that finds nothing new, at once.
type autoPuller struct{}

func (autoPuller) Pull(context.Context) (pull.Result, error) {
	return pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: testCommit, To: testCommit}, nil
}

// world is what a test of the scans has: the clock, the scanner and the watcher, and the options that
// give them to the agent.
type world struct {
	clock    *tickWall
	scanner  *fakeScanner
	watcher  *fakeWatcher
	settled  chan []scan.Change
	scanOpts chan scan.Options
	watchOpt chan watch.Options
	made     atomic.Int32
	// committer is what commits the settled changes, and commitOpt gets the options that it was made with.
	committer *fakeCommitter
	commitOpt chan commit.Options
	// commitErr, when it is set, is why the committer cannot be made.
	commitErr error
}

// fakeCommitter says each call on settled, and answers with what the test says: a commit of all the files that
// it was given, until the test gives it another answer.
type fakeCommitter struct {
	settled chan<- []scan.Change
	calls   atomic.Int32
	// answer, when it is set, is the answer to the call with the number n, from 1.
	answer func(n int, changes []scan.Change) (commit.Result, error)
}

func (f *fakeCommitter) Commit(ctx context.Context, changes []scan.Change) (commit.Result, error) {
	f.settled <- changes
	n := int(f.calls.Add(1))
	if f.answer != nil {
		return f.answer(n, changes)
	}
	return commit.Result{Outcome: commit.Committed, Commit: testCommit, Files: len(changes)}, nil
}

func newWorld() *world {
	clock := newTickWall()
	w := &world{
		clock: clock, scanner: newFakeScanner(clock), watcher: newFakeWatcher(),
		settled: make(chan []scan.Change, 100), scanOpts: make(chan scan.Options, 10), watchOpt: make(chan watch.Options, 10),
		commitOpt: make(chan commit.Options, 10),
	}
	w.committer = &fakeCommitter{settled: w.settled}
	return w
}

// with gives the agent a bidirectional Treebind, a checkout that works, pulls that find nothing, and the
// scanner and the watcher of the world.
func (w *world) with(more ...func(*Options)) func(*Options) {
	return func(o *Options) {
		withRepository(cloned)(o)
		o.Config.SyncInterval = time.Hour
		o.Config.SettleTime = settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.SyncClock, o.SyncRandom = w.clock, func() float64 { return 0.5 }
		o.ScanInterval, o.ScanGap = scanEvery, scanGap
		o.NewPuller = func(pull.Options) (Puller, error) { return autoPuller{}, nil }
		o.NewScanner = func(options scan.Options) (Scanner, error) {
			w.made.Add(1)
			w.scanOpts <- options
			return w.scanner, nil
		}
		o.NewWatcher = func(options watch.Options) (Watcher, error) {
			w.watchOpt <- options
			return w.watcher, nil
		}
		o.NewCommitter = func(options commit.Options) (Committer, error) {
			w.commitOpt <- options
			if w.commitErr != nil {
				return nil, w.commitErr
			}
			return w.committer, nil
		}
		for _, m := range more {
			m(o)
		}
	}
}

func changesCheck(t *testing.T, r *running) (status.Check, bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckChanges {
			return c, true
		}
	}
	return status.Check{}, false
}

// next answers the scan that has begun and returns when the next began, which is when the answer is in the
// report.
func (w *world) next(t *testing.T, a scanAnswer) time.Duration { return w.scanner.answer(t, a) }

func TestScansABidirectionalTreebindAfterTheCheckoutAndSaysInTheReportWhatItFound(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, found(changed(fileOne, scan.Modified, 1), changed(fileTwo, scan.Added, 2)))

	got, ok := changesCheck(t, r)
	if !ok || got.Outcome != outcomePassed || got.Reason != "" {
		t.Fatalf("changes = %+v, want it passed", got)
	}
	for _, want := range []string{"2 files changed on the volume", "0 settled", "2 waiting"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want it to say %q", got.Message, want)
		}
	}
	if strings.Contains(got.Message, "does not commit") {
		t.Errorf("message = %q, want it not to say that the agent does not commit: it does", got.Message)
	}
	if names := checkNames(t, r); names != "Mount,Writable,Checkout,Pull,Changes,Commit" {
		t.Errorf("checks = %s, want the changes after the pull, and the commit after the changes", names)
	}
}

func TestSaysThereAreNoChangesAndOneFileInTheSingular(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	w.next(t, scanAnswer{})
	if got, _ := changesCheck(t, r); got.Outcome != outcomePassed || got.Message != "no local changes" {
		t.Errorf("changes = %+v, want it passed, with no local changes and nothing else to say", got)
	}
	w.next(t, found(changed(fileOne, scan.Added, 1)))
	if got, _ := changesCheck(t, r); !strings.HasPrefix(got.Message, "1 file changed on the volume") {
		t.Errorf("message = %q, want one file in the singular", got.Message)
	}
}

func TestDoesNotScanForAPullTreebindOrWithNoScannerGiven(t *testing.T) {
	t.Run("the direction is pull", func(t *testing.T) {
		w := newWorld()
		r := start(t, w.with(func(o *Options) { o.Config.SyncDirection = config.DirectionPull }))
		eventually(t, "the checkout to pass", func() bool {
			got, found := checkoutCheck(t, r)
			return found && got.Outcome == outcomePassed
		})
		eventually(t, "the pull to pass", func() bool {
			got, found := pullCheck(t, r)
			return found && got.Outcome == outcomePassed
		})
		if _, found := changesCheck(t, r); found || w.made.Load() != 0 || len(w.watchOpt) != 0 {
			t.Errorf("a pull Treebind has a Changes check = %v, %d scanners, %d watchers: it has none", found, w.made.Load(), len(w.watchOpt))
		}
	})
	t.Run("no scanner is given", func(t *testing.T) {
		w := newWorld()
		r := start(t, w.with(func(o *Options) { o.NewScanner = nil }))
		eventually(t, "the pull to pass", func() bool {
			got, found := pullCheck(t, r)
			return found && got.Outcome == outcomePassed
		})
		if _, found := changesCheck(t, r); found || len(w.watchOpt) != 0 {
			t.Error("an agent with no scanner has a Changes check or a watcher")
		}
	})
}

func TestAFileSettlesWhenTheScansSeeItUnchangedForTheSettleTime(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	same := found(changed(fileOne, scan.Modified, 1))
	at := w.scanner.awaitBegun(t) // 0s

	// 0s: the file is seen. The next scan is due at the interval, 3s, which is before it settles.
	if got := w.next(t, same); got != scanEvery {
		t.Errorf("the second scan began at %v, want the interval %v", got, scanEvery)
	}
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "0 settled, 1 waiting") {
		t.Errorf("at %v: %q, want one that waits", at, got.Message)
	}
	if len(w.settled) != 0 {
		t.Fatal("the file was handed on before it settled")
	}

	// 3s: seen again, 3s of 5s. The scan says that it settles in 2s, and that is when the next one is.
	if got := w.next(t, same); got != settleFor {
		t.Errorf("the third scan began at %v, want %v, when the file settles", got, settleFor)
	}
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "0 settled, 1 waiting") {
		t.Errorf("message = %q, want it still waiting", got.Message)
	}

	// 5s: unchanged for the settle time.
	w.next(t, same)
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "1 settled, 0 waiting") {
		t.Errorf("message = %q, want it settled", got.Message)
	}
	select {
	case got := <-w.settled:
		if len(got) != 1 || got[0].Path != fileOne {
			t.Errorf("handed on %v, want %s", got, fileOne)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the settled file was not handed on")
	}
}

func TestAFileThatIsWrittenAgainStartsItsQuietPeriodAgain(t *testing.T) {
	w := newWorld()
	start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, found(changed(fileOne, scan.Modified, 1)))
	w.next(t, found(changed(fileOne, scan.Modified, 2))) // written again, seen at 3s
	w.next(t, found(changed(fileOne, scan.Modified, 2))) // seen at 6s: three of the five seconds of quiet
	if len(w.settled) != 0 {
		t.Error("a file that was written again two seconds ago was handed on")
	}
}

func TestAScanThatFailsIsReportedWithItsReasonAndTheNextOneComesAtTheInterval(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)

	second := w.next(t, scanAnswer{err: &scan.Error{Reason: scan.ReasonScanFailed, Message: "the changes of the directory cannot be listed: boom"}})

	got, _ := changesCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != scan.ReasonScanFailed || !strings.Contains(got.Message, "boom") {
		t.Errorf("changes = %+v, want it failed with ScanFailed and what went wrong", got)
	}
	if second != scanEvery {
		t.Errorf("the scan after a failure began at %v, want the interval %v: a scan is local, and has no backoff", second, scanEvery)
	}

	w.next(t, scanAnswer{})
	if got, _ := changesCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("changes = %+v, want it passed once a scan works", got)
	}
}

func TestAScanThatIsRefusedBecauseOfTheGitDirectoryHasTheReasonOfTheCheckout(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{err: &scan.Error{Reason: checkout.ReasonGitDirUnsafe, Message: "config holds a setting"}})
	if got, _ := changesCheck(t, r); got.Reason != checkout.ReasonGitDirUnsafe {
		t.Errorf("reason = %q, want %q", got.Reason, checkout.ReasonGitDirUnsafe)
	}
}

func TestAScanThatFailsStillSaysThatThereAreNoEvents(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) { o.NewWatcher = nil }))
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{err: &scan.Error{Reason: scan.ReasonScanFailed, Message: "boom"}})
	got, _ := changesCheck(t, r)
	if got.Outcome != outcomeFailed || !strings.Contains(got.Message, "boom") || !strings.Contains(got.Message, "no events of the file system") {
		t.Errorf("changes = %+v, want it failed, with why, and with the events being off: a failed check is where a person looks", got)
	}
}

func TestAScanThatFailsWithAnErrorOfNoReasonIsAScanThatFailed(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{err: errors.New("something else")})
	if got, _ := changesCheck(t, r); got.Reason != scan.ReasonScanFailed || !strings.Contains(got.Message, "something else") {
		t.Errorf("changes = %+v, want ScanFailed with the message", got)
	}
}

func TestSaysWhatTheFilterLeftOutByReasonInOrder(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{result: scan.Result{
		Changes: []scan.Change{changed(fileOne, scan.Added, 1)},
		Left:    map[paths.Reason]int{paths.Secret: 1, paths.Excluded: 2},
	}})
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "left out: Excluded 2, Secret 1") {
		t.Errorf("message = %q, want what the filter left out, by reason, in order", got.Message)
	}
}

func TestLogsAScanOnlyWhenWhatItSaysChanges(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	same := found(changed(fileOne, scan.Modified, 1))
	w.next(t, same)
	w.next(t, same) // the same, with a file that has not settled yet: the message holds no time
	w.next(t, scanAnswer{})
	w.scanner.answers <- scanAnswer{}
	w.scanner.awaitBegun(t)
	// A change of the message: one file, then none.
	if n := strings.Count(r.logs.String(), "msg=scan "); n != 2 {
		t.Errorf("the scans were logged %d times, want 2: the change and the end of it\n%s", n, r.logs.String())
	}
}

func TestAnEventOfTheWatcherCallsForAScanAtTheGapAndNotAtTheInterval(t *testing.T) {
	w := newWorld()
	// A wait does not pass while an event is waiting, so that the loop sees the event first, as it does when
	// the event came before the wait ended.
	w.clock.hold = func() bool { return len(w.watcher.events) > 0 }
	start(t, w.with())
	w.scanner.awaitBegun(t)
	w.watcher.events <- struct{}{} // while the first scan runs

	second := w.next(t, scanAnswer{})

	if second != scanGap {
		t.Errorf("the scan after the event began at %v, want the gap %v after the first, and not the interval %v", second, scanGap, scanEvery)
	}
}

func TestTheWatcherIsGivenTheWorkTreeAndAWayToSkipGitAndTheStateDirectory(t *testing.T) {
	w := newWorld()
	start(t, w.with(func(o *Options) { o.Config.MountPath, o.Config.SubPath = claimOfTheTests, "" }))
	var got watch.Options
	select {
	case got = <-w.watchOpt:
	case <-time.After(10 * time.Second):
		t.Fatal("no watcher was made")
	}
	if got.Root != claimOfTheTests {
		t.Errorf("root = %q, want the work tree %s", got.Root, claimOfTheTests)
	}
	for dir, want := range map[string]bool{".git": true, "a/.git": true, ".treebind": true, ".treebind/git": true, "a": false, "a/b": false} {
		if got.Skip == nil || got.Skip(dir) != want {
			t.Errorf("Skip(%q) = %v, want %v: the state directory is in the work tree when the whole claim is synced", dir, got.Skip != nil && got.Skip(dir), want)
		}
	}
}

func TestTheWatcherOfATreebindWithASubPathDoesNotSkipADirectoryNamedLikeTheStateDirectory(t *testing.T) {
	w := newWorld()
	start(t, w.with(func(o *Options) { o.Config.MountPath, o.Config.SubPath = claimOfTheTests, "data" }))
	got := <-w.watchOpt
	if got.Root != "/work/data" {
		t.Errorf("root = %q, want /work/data", got.Root)
	}
	if got.Skip(".treebind") {
		t.Error(".treebind is skipped, and the state directory is beside the synced directory and not in it")
	}
}

func TestTheDirectoriesAreRefreshedAtTheScansOfTheIntervalAndNotAtThoseOfEvents(t *testing.T) {
	w := newWorld()
	w.clock.hold = func() bool { return len(w.watcher.events) > 0 }
	start(t, w.with())
	w.scanner.awaitBegun(t) // 0s: the first scan, which refreshes
	if got := w.watcher.refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want 1 before the first scan", got)
	}

	w.watcher.events <- struct{}{}    // an event while the first scan runs
	w.scanner.answer(t, scanAnswer{}) // 2s, the gap: a scan that an event called for
	if got := w.watcher.refreshes.Load(); got != 1 {
		t.Errorf("refreshes = %d, want 1: a scan that an event called for, 2s after the last refresh, is not one that walks every directory", got)
	}
	at := w.scanner.answer(t, scanAnswer{}) // 5s: the interval after the one at 2s
	if got := w.watcher.refreshes.Load(); got != 2 || at != 5*time.Second {
		t.Errorf("refreshes = %d at %v, want 2 at 5s: the interval has passed since the last refresh", got, at)
	}
}

func TestAWatcherThatCannotBeMadeLeavesTheScansOnTheirIntervalAndSaysSo(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.NewWatcher = func(watch.Options) (Watcher, error) { return nil, errors.New("too many instances") }
	}))
	w.scanner.awaitBegun(t)
	second := w.next(t, scanAnswer{})
	got, _ := changesCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "no events of the file system") ||
		!strings.Contains(got.Message, "too many instances") || !strings.Contains(got.Message, scanEvery.String()) {
		t.Errorf("changes = %+v, want it passed, with no events, why, and the interval", got)
	}
	if second != scanEvery {
		t.Errorf("the second scan began at %v, want the interval", second)
	}
}

func TestAgentWithNoWatcherFunctionScansOnItsIntervalAndSaysSo(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) { o.NewWatcher = nil }))
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "no events of the file system") {
		t.Errorf("message = %q, want it to say that there are no events", got.Message)
	}
}

func TestTheDefaultScanIntervalIsTheOneOfTheLoop(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) { o.NewWatcher = nil; o.ScanInterval = 0 }))
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "every 30s") {
		t.Errorf("message = %q, want the default interval of 30s", got.Message)
	}
}

func TestTheFilterKnowsTheStateDirectoryWhenTheWholeClaimIsSynced(t *testing.T) {
	w := newWorld()
	start(t, w.with(func(o *Options) { o.Config.SubPath = "" }))
	got := <-w.scanOpts
	if got.Filter.Reason(".treebind/git/config") != paths.StateDirectory {
		t.Errorf("Reason of a file of the state directory = %v, want %v: it is in the work tree", got.Filter.Reason(".treebind/git/config"), paths.StateDirectory)
	}
}

func TestAWatcherThatCameWithAnErrorIsNotUsed(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.NewWatcher = func(watch.Options) (Watcher, error) { return w.watcher, errors.New("half made") }
	}))
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})
	if w.watcher.refreshes.Load() != 0 {
		t.Error("a watcher that was returned with an error was used")
	}
	if got, _ := changesCheck(t, r); !strings.Contains(got.Message, "no events of the file system (half made)") {
		t.Errorf("message = %q, want it to say that there are no events, and why", got.Message)
	}
}

func TestSaysTheScansWereNotTriedWhileAVolumeCheckFails(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.Preflight = func(context.Context, string, string) []preflight.Result { return failed }
	}))
	eventually(t, "the changes to be skipped", func() bool {
		got, found := changesCheck(t, r)
		return found && got.Outcome == outcomeSkipped
	})
	got, _ := changesCheck(t, r)
	if got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "volume check failed") {
		t.Errorf("changes = %+v, want it skipped, because a volume check failed", got)
	}
	if len(w.scanner.began) != 0 {
		t.Error("a scan began while a volume check failed")
	}
}

func TestSaysWhatTheWatcherCouldNotWatchAndThatEventsWereLost(t *testing.T) {
	w := newWorld()
	w.watcher.stats = watch.Stats{
		Dirs: 100, Unwatched: 2, Reason: "the limit of the kernel on watches (fs.inotify.max_user_watches) is reached", Lost: 1,
	}
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	w.next(t, scanAnswer{})
	got, _ := changesCheck(t, r)
	for _, want := range []string{"2 directories cannot be watched", "fs.inotify.max_user_watches", "events were lost"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want it to say %q", got.Message, want)
		}
	}
	if strings.Contains(got.Message, "100") {
		t.Errorf("message = %q, want no count of the directories that are watched: it would change with every directory", got.Message)
	}
}

func TestTheWatcherIsClosedWhenTheAgentStops(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	r.cancel()
	_ = r.waitStopped(t)
	if !w.watcher.closed.Load() {
		t.Error("the watcher was not closed when the agent stopped")
	}
}

func TestAScanThatIsCutShortByAStopSaysNothing(t *testing.T) {
	w := newWorld()
	r := start(t, w.with())
	w.scanner.awaitBegun(t)
	r.cancel()
	_ = r.waitStopped(t)
	if strings.Contains(r.logs.String(), "msg=scan ") {
		t.Errorf("a scan that was cut short was logged:\n%s", r.logs.String())
	}
}

func TestSaysTheScansWereNotTriedUntilTheCheckoutIsMadeAndThatTheFirstIsRunning(t *testing.T) {
	w := newWorld()
	inOrder := make(chan struct{})
	r := start(t, w.with(func(o *Options) {
		o.Checkout = func(context.Context, checkout.Options) (checkout.Result, error) {
			select {
			case <-inOrder:
				return checkout.Result{Outcome: checkout.Cloned, Branch: testBranch, Commit: testCommit}, nil
			default:
				return checkout.Result{}, &checkout.Error{Reason: checkout.ReasonFetchFailed, Message: "the repository cannot be asked"}
			}
		}
	}))
	eventually(t, "the changes to be skipped", func() bool {
		got, found := changesCheck(t, r)
		return found && got.Outcome == outcomeSkipped
	})
	got, _ := changesCheck(t, r)
	if got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "checkout is not made") {
		t.Errorf("changes = %+v, want it skipped, because the checkout is not made", got)
	}

	close(inOrder)
	w.scanner.awaitBegun(t) // the first scan is running
	got, _ = changesCheck(t, r)
	if got.Outcome != outcomeSkipped || !strings.Contains(got.Message, "running") {
		t.Errorf("changes = %+v, want it to say that the first scan is running", got)
	}
}

func TestAScannerThatCannotBeMadeIsAFailureOfTheChangesAndThePullsGoOn(t *testing.T) {
	w := newWorld()
	r := start(t, w.with(func(o *Options) {
		o.NewScanner = func(scan.Options) (Scanner, error) { return nil, errors.New("no filter") }
	}))
	eventually(t, "the changes to fail", func() bool {
		got, found := changesCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})
	got, _ := changesCheck(t, r)
	if got.Reason != scan.ReasonScanFailed || !strings.Contains(got.Message, "cannot be set up") || !strings.Contains(got.Message, "no filter") {
		t.Errorf("changes = %+v, want ScanFailed, saying that the scan cannot be set up and why", got)
	}
	eventually(t, "the pull to pass", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomePassed
	})
}

func TestTheScannerIsMadeWithTheLayoutTheGitSettingsAndAFilterOfTheSpec(t *testing.T) {
	w := newWorld()
	start(t, w.with(func(o *Options) {
		o.Config.PathsInclude = []string{"*.json"}
		o.Config.PathsExclude = []string{"build/"}
	}))
	var got scan.Options
	select {
	case got = <-w.scanOpts:
	case <-time.After(10 * time.Second):
		t.Fatal("no scanner was made")
	}
	if got.Layout.WorkTree != workTreeOfTheTests || got.Layout.GitDir != gitDirOfTheTests {
		t.Errorf("layout = %+v, want the one of the claim and the sub path", got.Layout)
	}
	for path, want := range map[string]paths.Reason{
		"a.json": paths.Synced, "a.txt": paths.NotIncluded, "build/a.json": paths.Excluded, ".env": paths.Secret,
	} {
		if got.Filter == nil || got.Filter.Reason(path) != want {
			t.Errorf("the filter does not give %v for %q: it is not the one that spec.paths says", want, path)
		}
	}
}

// The real scanner, on a real repository: what the application writes is a change, and what the agent
// never syncs is left out and counted.
func TestScansTheRealDirectoryAndSeesWhatTheApplicationWrote(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n", jsonB: "b\n"})
	where := f.Layout(subDir)
	clock := newTickWall()
	r := start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval, o.Config.SettleTime = time.Hour, settleFor
		o.Config.SyncDirection = config.DirectionBidirectional
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		o.ScanInterval = scanEvery
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(pull.Options) (Puller, error) { return autoPuller{}, nil }
		o.NewScanner = func(options scan.Options) (Scanner, error) {
			options.Git = f.GitOptions()
			return scan.New(options)
		}
	})

	eventually(t, "the first scan to say that nothing changed", func() bool {
		got, found := changesCheck(t, r)
		return found && got.Outcome == outcomePassed && strings.HasPrefix(got.Message, "no local changes")
	})
	gittest.Write(t, where, jsonA, "a, changed by the application\n")
	gittest.Write(t, where, "new.json", "new\n")
	gittest.Write(t, where, ".env", "TOKEN=1\n")
	eventually(t, "the scan to see the application's files", func() bool {
		got, _ := changesCheck(t, r)
		return strings.HasPrefix(got.Message, "2 files changed on the volume")
	})
	got, _ := changesCheck(t, r)
	if !strings.Contains(got.Message, "left out: Secret 1") {
		t.Errorf("message = %q, want the secret to be left out and counted", got.Message)
	}
}
