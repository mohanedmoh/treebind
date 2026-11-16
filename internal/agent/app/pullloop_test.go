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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
)

const (
	testInterval = 20 * time.Second
	otherCommit  = "fedcba9876543210fedcba9876543210fedcba98"

	// The files and the directory of the tests that pull from a real repository.
	jsonA, jsonB, jsonC, jsonD = "a.json", "b.json", "c.json", "d.json"
	subDir                     = "dashboards"
	reasonTooMany              = "TooManyDeletions"
)

// loopClock says how long every wait of the loop is, and lets it pass at once, so that no test waits.
type loopClock struct {
	mu     sync.Mutex
	asked  []time.Duration
	stalls int
}

// stallAfter makes the waits after the first n never pass, for a test that stops the agent while it waits.
func (c *loopClock) stallAfter(n int) *loopClock {
	c.stalls = n + 1
	return c
}

func (c *loopClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, d)
	ch := make(chan time.Time, 1)
	if c.stalls == 0 || len(c.asked) < c.stalls {
		ch <- time.Time{}
	}
	return ch
}

func (c *loopClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

// answer is what a pull of a test comes to.
type answer struct {
	result pull.Result
	err    error
}

// fakePuller is a puller that a test answers one pull at a time: a pull begins, which is said on began, and
// ends when the test gives it an answer.
type fakePuller struct {
	began   chan struct{}
	answers chan answer
}

func newFakePuller() *fakePuller {
	return &fakePuller{began: make(chan struct{}, 100), answers: make(chan answer, 100)}
}

func (f *fakePuller) Pull(ctx context.Context) (pull.Result, error) {
	f.began <- struct{}{}
	select {
	case a := <-f.answers:
		return a.result, a.err
	case <-ctx.Done():
		return pull.Result{}, ctx.Err()
	}
}

// answer gives the pull that has begun its answer, and waits for the loop to come to the next pull, which is
// when the answer is in the report.
func (f *fakePuller) answer(t *testing.T, a answer) {
	t.Helper()
	f.answers <- a
	f.awaitBegun(t)
}

func (f *fakePuller) awaitBegun(t *testing.T) {
	t.Helper()
	select {
	case <-f.began:
	case <-time.After(10 * time.Second):
		t.Fatal("no pull began")
	}
}

// withPuller gives the agent a puller, after a checkout that works, and returns the options that each
// pull is made with. The pulls of the test do not wait, and the jitter does not move the interval.
func withPuller(f *fakePuller, clock *loopClock) (func(*Options), <-chan pull.Options) {
	options := make(chan pull.Options, 10)
	return func(o *Options) {
		withRepository(cloned)(o)
		o.Config.SyncInterval = testInterval
		o.SyncClock = clock
		o.SyncRandom = func() float64 { return 0.5 }
		o.NewPuller = func(asked pull.Options) (Puller, error) {
			options <- asked
			return f, nil
		}
	}, options
}

func pullCheck(t *testing.T, r *running) (status.Check, bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckPull {
			return c, true
		}
	}
	return status.Check{}, false
}

func advanced(from, to string) answer {
	return answer{result: pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: from, To: to}}
}

func upToDate(commit string) answer {
	return answer{result: pull.Result{Outcome: pull.UpToDate, Branch: testBranch, From: commit, To: commit}}
}

func failedPull(reason, message string) answer {
	return answer{err: &pull.Error{Reason: reason, Message: message}}
}

func TestPullsAfterTheCheckoutAndSaysInTheReportHowEachPullWent(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, change)
	f.awaitBegun(t)

	f.answer(t, advanced(testCommit, otherCommit))

	got, found := pullCheck(t, r)
	want := "advanced the checkout on the branch main from 0123456789ab to fedcba987654"
	if !found || got.Outcome != outcomePassed || got.Reason != "" || got.Message != want {
		t.Errorf("pull = %+v, want it passed, with %q", got, want)
	}
	if names := checkNames(t, r); names != "Mount,Writable,Checkout,Pull" {
		t.Errorf("checks = %s, want the pull after the checkout", names)
	}

	f.answer(t, upToDate(otherCommit))

	got, _ = pullCheck(t, r)
	if got.Outcome != outcomePassed || got.Message != "the checkout is at the tip of the branch main (fedcba987654)" {
		t.Errorf("pull = %+v, want it passed, and at the tip", got)
	}

	f.answer(t, answer{result: pull.Result{Outcome: pull.Ahead, Branch: testBranch, From: otherCommit, To: otherCommit}})

	got, _ = pullCheck(t, r)
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "ahead of the branch main (fedcba987654)") ||
		!strings.Contains(got.Message, "nothing to pull") {
		t.Errorf("pull = %+v, want it passed, and ahead, with nothing to pull", got)
	}
}

func TestPullsIntoTheCheckoutThatWasMadeWithWhatTheSettingsSay(t *testing.T) {
	f := newFakePuller()
	change, made := withPuller(f, &loopClock{})
	start(t, change)

	options := <-made

	if options.URL != testURL || options.Layout.WorkTree != "/work/dashboards" ||
		options.Layout.GitDir != "/work/.treebind/git" || options.Git.SSH.IdentityFile != testKeyFile ||
		options.Git.SSH.KnownHostsFile != testKnownHosts {
		t.Errorf("options = %+v, want what the settings of the pod say", options)
	}
	// The setting says release, and the checkout followed main: the branch of the pull is the checkout's.
	if options.Branch != testBranch {
		t.Errorf("branch = %q, want %q, the branch that the checkout follows", options.Branch, testBranch)
	}
	if options.Fence == nil {
		t.Error("the pull has no fence: it would write without looking at the lock")
	}
}

func TestSaysThePhaseUntilTheFirstPullAndThatItRunsWhileItDoes(t *testing.T) {
	f := newFakePuller()
	// The first wait never passes, so that the report is read before the first pull.
	clock := (&loopClock{}).stallAfter(0)
	change, _ := withPuller(f, clock)
	r := start(t, change)

	eventually(t, "the first pull to be waited for", func() bool { return len(clock.waits()) == 1 })
	got, _ := pullCheck(t, r)
	phase := syncloop.Phase(testNS, testName, testInterval)
	want := fmt.Sprintf("waiting: the first pull is due in %s", phase.Round(time.Second))
	if got.Reason != preflight.ReasonNotChecked || got.Message != want {
		t.Errorf("pull = %+v, want it skipped with %q", got, want)
	}
	if waits := clock.waits(); len(waits) != 1 || waits[0] != phase {
		t.Errorf("waits = %v, want the phase of the Treebind, %v, before the first pull", waits, phase)
	}
}

func TestSaysTheFirstPullIsRunningWhileItDoes(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, change)
	f.awaitBegun(t)

	got, found := pullCheck(t, r)

	if !found || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.HasPrefix(got.Message, "running:") {
		t.Errorf("pull = %+v, want it skipped, and running: a pull of a large repository takes a while", got)
	}
}

func TestPullsAtTheIntervalAfterTheFirstPhase(t *testing.T) {
	f := newFakePuller()
	clock := &loopClock{}
	change, _ := withPuller(f, clock)
	start(t, change)
	f.awaitBegun(t)
	f.answer(t, upToDate(testCommit))
	f.answer(t, upToDate(testCommit))

	waits := clock.waits()

	if want := []time.Duration{syncloop.Phase(testNS, testName, testInterval), testInterval, testInterval}; !slices.Equal(waits[:3], want) {
		t.Errorf("waits = %v, want the phase and then the interval of the settings, with the source of the test not moving it", waits)
	}
}

func TestSaysAnIntervalThatIsNotSetIsAFailureOfThePull(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Config.SyncInterval = 0
	})

	eventually(t, "the failure of the loop", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	got, _ := pullCheck(t, r)
	if got.Reason != pull.ReasonPullFailed || !strings.Contains(got.Message, "interval") {
		t.Errorf("pull = %+v, want a failure that says what is wrong with the interval", got)
	}
	if !strings.Contains(r.logs.String(), "check=Pull outcome=Failed") {
		t.Errorf("the failure was not logged:\n%s", r.logs.String())
	}
}

func TestAPullThatFailsIsReportedWithItsReasonAndTheNextWaitGrows(t *testing.T) {
	f := newFakePuller()
	clock := &loopClock{}
	change, _ := withPuller(f, clock)
	r := start(t, change)
	f.awaitBegun(t)

	f.answer(t, failedPull(pull.ReasonFetchFailed, "the repository cannot be asked for the branch main: connection refused"))

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonFetchFailed || !strings.Contains(got.Message, "connection refused") {
		t.Errorf("pull = %+v, want the failure with its reason and what Git said", got)
	}
	f.answer(t, failedPull(pull.ReasonFetchFailed, "the repository cannot be asked for the branch main: connection refused"))
	f.answer(t, upToDate(testCommit))
	f.answer(t, upToDate(testCommit))

	waits := clock.waits()
	phase := syncloop.Phase(testNS, testName, testInterval)
	// With a source that says 0.5: the interval, then half way to twice and four times it, then the interval.
	if want := []time.Duration{phase, 30 * time.Second, 50 * time.Second, testInterval, testInterval}; !slices.Equal(waits[:5], want) {
		t.Errorf("waits = %v, want %v: the wait grows with each failure and goes back with the first success", waits, want)
	}
	if got, _ = pullCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("pull = %+v, want it passed once the repository answers", got)
	}
}

func TestAPullThatNeedsAPersonKeepsTheIntervalAndIsReportedUntilTheyAct(t *testing.T) {
	f := newFakePuller()
	clock := &loopClock{}
	change, _ := withPuller(f, clock)
	r := start(t, change)
	f.awaitBegun(t)

	blocked := failedPull(pull.ReasonBlocked, "the checkout was not updated, because 1 file(s) that the update changes have a local edit")
	f.answer(t, blocked)
	f.answer(t, blocked)
	f.answer(t, answer{err: &pull.Error{Reason: pull.ReasonDiverged, Message: "the checkout and the branch have gone different ways"}})

	waits := clock.waits()
	if want := []time.Duration{testInterval, testInterval, testInterval}; !slices.Equal(waits[1:4], want) {
		t.Errorf("waits = %v, want the interval after each of them: a slower poll does not put a file right", waits)
	}
	if got, _ := pullCheck(t, r); got.Reason != "Diverged" || got.Outcome != outcomeFailed {
		t.Errorf("pull = %+v, want the last of them, Diverged, in the report", got)
	}
}

func TestAPullThatTheCapRefusedKeepsTheIntervalAndIsLoggedOnce(t *testing.T) {
	f := newFakePuller()
	clock := &loopClock{}
	change, _ := withPuller(f, clock)
	r := start(t, change)
	f.awaitBegun(t)

	refused := failedPull(pull.ReasonTooManyDeletions, "the update would remove 101 file(s) from the directory, more than the 100 that Treebind removes in one update")
	for range 4 {
		f.answer(t, refused)
	}

	waits := clock.waits()
	if want := []time.Duration{testInterval, testInterval, testInterval, testInterval}; !slices.Equal(waits[1:5], want) {
		t.Errorf("waits = %v, want the interval after each: it needs a person, and a slower poll does not remove files", waits)
	}
	if got, _ := pullCheck(t, r); got.Reason != reasonTooMany || got.Outcome != outcomeFailed {
		t.Errorf("pull = %+v, want TooManyDeletions in the report", got)
	}
	if n := r.logs.count("reason=TooManyDeletions"); n != 1 {
		t.Errorf("a refusal that repeated 4 times was logged %d times, want once\n%s", n, r.logs.String())
	}
}

func TestLogsAPullOnlyWhenWhatItSaysChanges(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, change)
	f.awaitBegun(t)

	f.answer(t, advanced(testCommit, otherCommit))
	for range 5 {
		f.answer(t, upToDate(otherCommit))
	}
	fetchFailed := failedPull(pull.ReasonFetchFailed, "the repository cannot be asked")
	for range 4 {
		f.answer(t, fetchFailed)
	}
	f.answer(t, upToDate(otherCommit))

	logs := r.logs.String()
	if n := strings.Count(logs, "msg=pull "); n != 4 {
		t.Errorf("the pulls were logged %d times, want 4: the advance, being at the tip, the failure and being at the tip again\n%s", n, logs)
	}
	if n := r.logs.count("reason=FetchFailed"); n != 1 {
		t.Errorf("a failure that repeated 4 times was logged %d times, want once\n%s", n, logs)
	}
	if !strings.Contains(logs, "level=WARN msg=pull check=Pull outcome=Failed") || !strings.Contains(logs, "level=INFO msg=pull check=Pull outcome=Passed") {
		t.Errorf("a failure is a warning and a pull that went well is not\n%s", logs)
	}
}

func TestAPullThatCannotBeSetUpIsAFailureAndIsTriedAgain(t *testing.T) {
	f := newFakePuller()
	clock := &loopClock{}
	var made atomic.Int64
	r := start(t, func(o *Options) {
		change, _ := withPuller(f, clock)
		change(o)
		o.NewPuller = func(pull.Options) (Puller, error) {
			if made.Add(1) <= 2 {
				return nil, errors.New("git is not installed")
			}
			return f, nil
		}
	})
	eventually(t, "the failure in the report", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	f.awaitBegun(t)

	got, _ := pullCheck(t, r)
	if got.Outcome != outcomeFailed || got.Reason != pull.ReasonPullFailed || !strings.HasPrefix(got.Message, "the pull cannot be set up") {
		t.Errorf("pull = %+v, want the failure of the pull that could not be set up", got)
	}
	f.answer(t, upToDate(testCommit))
	if got, _ = pullCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("pull = %+v, want it passed once a puller could be made", got)
	}
	if made.Load() != 3 {
		t.Errorf("a puller was asked for %d times, want 3: two refusals and one that works, and then the same one is used", made.Load())
	}
	if want := []time.Duration{30 * time.Second, 50 * time.Second}; !slices.Equal(clock.waits()[1:3], want) {
		t.Errorf("waits = %v, want them to grow: a pull that cannot be set up is a failure", clock.waits())
	}
}

func TestTheMessageOfAPullThatCouldNotBeSetUpSaysSo(t *testing.T) {
	f := newFakePuller()
	r := start(t, func(o *Options) {
		change, _ := withPuller(f, &loopClock{})
		change(o)
		o.NewPuller = func(pull.Options) (Puller, error) { return nil, errors.New("git is not installed") }
	})

	eventually(t, "the failure in the report", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	got, _ := pullCheck(t, r)
	if got.Reason != pull.ReasonPullFailed || got.Message != "the pull cannot be set up: git is not installed" {
		t.Errorf("pull = %+v, want the reason PullFailed and a message that says what could not be done", got)
	}
}

func TestAnAgentThatIsToldToStopInTheMiddleOfAPullStopsAtOnceAndSaysNothingOfIt(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, change)
	f.awaitBegun(t)

	// The pull does not end, and the report is still served.
	if got, found := pullCheck(t, r); !found || got.Outcome != outcomeSkipped {
		t.Fatalf("pull = %+v, want a report while the pull runs", got)
	}
	r.cancel()
	err := r.waitStopped(t)

	if err != nil {
		t.Errorf("Run returned %v, want a clean stop", err)
	}
	if logs := r.logs.String(); strings.Contains(logs, "outcome=Failed") || strings.Contains(logs, "context canceled") {
		t.Errorf("a pull that was cut short by the stop was logged as a failure of the repository:\n%s", logs)
	}
}

func TestAnAgentThatIsToldToStopWhileItWaitsForTheNextPullStopsAtOnce(t *testing.T) {
	f := newFakePuller()
	// The phase passes and the next wait does not.
	clock := (&loopClock{}).stallAfter(1)
	change, _ := withPuller(f, clock)
	r := start(t, change)
	f.awaitBegun(t)
	f.answers <- upToDate(testCommit)
	eventually(t, "the next wait", func() bool { return len(clock.waits()) == 2 })

	r.cancel()

	if err := r.waitStopped(t); err != nil {
		t.Errorf("Run returned %v, want a clean stop", err)
	}
	if len(f.began) != 0 {
		t.Error("a pull began after the agent was told to stop")
	}
}

func TestDoesNotPullWhenNoPullerIsGivenOrNoCheckoutIsToBeMade(t *testing.T) {
	var asked atomic.Int64
	r := start(t, func(o *Options) {
		withRepository(cloned)(o)
		o.SyncClock = &loopClock{}
	})
	eventually(t, "the checkout", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
	if _, found := pullCheck(t, r); found {
		t.Error("a pull is reported, and no puller was given")
	}

	r = start(t, func(o *Options) {
		o.NewPuller = func(pull.Options) (Puller, error) {
			asked.Add(1)
			return nil, errors.New("no puller for a test with no checkout")
		}
	})
	eventually(t, "the volume checks", func() bool { return len(r.report(t).Checks) == 2 })
	if _, found := pullCheck(t, r); found || asked.Load() != 0 {
		t.Errorf("a pull is reported or a puller was asked for (%d times), and there is no checkout", asked.Load())
	}
}

func TestSaysThePullWasNotTriedWhileTheVolumeOrTheCheckoutHaveNotPassed(t *testing.T) {
	f := newFakePuller()
	inOrder := make(chan struct{})
	r := start(t, func(o *Options) {
		change, _ := withPuller(f, &loopClock{})
		change(o)
		o.Checkout = func(context.Context, checkout.Options) (checkout.Result, error) {
			return checkout.Result{}, &checkout.Error{Reason: checkout.ReasonFetchFailed, Message: "the repository cannot be asked"}
		}
		o.Preflight = func(context.Context, string, string) []preflight.Result {
			select {
			case <-inOrder:
				return passed
			default:
				return failed
			}
		}
	})

	eventually(t, "the pull to be skipped", func() bool {
		got, found := pullCheck(t, r)
		return found && got.Outcome == outcomeSkipped
	})
	got, _ := pullCheck(t, r)
	if got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "volume check failed") {
		t.Errorf("pull = %+v, want it skipped, because a volume check failed", got)
	}

	close(inOrder)
	eventually(t, "the checkout to fail", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomeFailed
	})
	got, found := pullCheck(t, r)
	if !found || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "checkout is not made") {
		t.Errorf("pull = %+v, want it skipped, because the checkout is not made", got)
	}
	if len(f.began) != 0 {
		t.Error("a pull began before the checkout was made")
	}
}

func TestKeepsTheRowOfTheCheckoutInTheReportWhileItRunsAfterTheLock(t *testing.T) {
	root, _ := claimWithState(t)
	running := make(chan struct{})
	r := start(t, takesTheLock(root, func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
		close(running)
		<-ctx.Done()
		return checkout.Result{}, ctx.Err()
	}))
	<-running

	got, found := checkoutCheck(t, r)

	if !found || got.Outcome != outcomeSkipped || !strings.HasPrefix(got.Message, "running:") {
		t.Errorf("checkout = %+v, want a row that says it is running: the lock is held and the checkout has not ended", got)
	}
}

func TestTheFenceOfThePullChecksTheLockThatTheAgentHolds(t *testing.T) {
	root, lockDir := claimWithState(t)
	clock := newFakeClock()
	f := newFakePuller()
	change, made := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		change(o)
		o.LockClock = clock
	})
	options := <-made
	f.awaitBegun(t)

	if err := options.Fence(context.Background()); err != nil {
		t.Errorf("Fence = %v, want nil: the agent holds the lock", err)
	}
	// The clock of this agent does not move, so it makes no heartbeat, and another agent sees the lock
	// unchanged for a whole TTL and takes it over.
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	err = options.Fence(context.Background())
	stopped := r.waitStopped(t)

	if !errors.Is(err, lock.ErrLost) {
		t.Errorf("Fence = %v, want the loss of the lock", err)
	}
	// Said again, it is still the loss, and it does not wait for Run, which is gone.
	for range 2 {
		if again := options.Fence(context.Background()); !errors.Is(again, lock.ErrLost) {
			t.Errorf("Fence = %v, want the loss of the lock again", again)
		}
	}
	if !errors.Is(stopped, ErrLockLost) || !errors.Is(stopped, lock.ErrLost) {
		t.Errorf("Run returned %v, want ErrLockLost: a pull that finds the lock gone stops the agent without waiting for the keeper", stopped)
	}
	if n := r.logs.count("the writer lock was lost to another agent"); n != 1 {
		t.Errorf("the loss was logged %d times, want once\n%s", n, r.logs.String())
	}
}

func TestSaysThePullWaitsForTheCheckoutWhileAnotherAgentHoldsTheLock(t *testing.T) {
	root, lockDir := claimWithState(t)
	other, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Release() })
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		change(o)
		o.LockTTL = time.Hour
	})

	eventually(t, "the wait in the report", func() bool {
		got, found := lockCheck(t, r)
		return found && got.Outcome == outcomeSkipped && strings.Contains(got.Message, "waiting")
	})

	got, found := pullCheck(t, r)
	if !found || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "checkout is not made yet") {
		t.Errorf("pull = %+v, want it skipped, because the checkout is not made yet", got)
	}
	if len(f.began) != 0 {
		t.Error("a pull began while another agent held the lock")
	}
}

func TestTheFenceSaysALostLockToRunAndNothingElse(t *testing.T) {
	_, lockDir := claimWithState(t)
	clock := newFakeClock()
	held, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: testPod, TTL: lockTTL, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{lost: make(chan error, 1)}
	a.held.Store(held)

	// The lock is held: nothing to say.
	if err := a.fence(context.Background()); err != nil || len(a.lost) != 0 {
		t.Fatalf("fence = %v, said %d times, want nil and nothing said", err, len(a.lost))
	}

	// The volume does not let the lock be checked: that is not the lock being lost to another agent.
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	if err := a.fence(context.Background()); err == nil || errors.Is(err, lock.ErrLost) || len(a.lost) != 0 {
		t.Fatalf("fence = %v, said %d times, want an error that is not the loss of the lock, and nothing said", err, len(a.lost))
	}
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.fence(context.Background()); err != nil || len(a.lost) != 0 {
		t.Fatalf("fence = %v, said %d times, want nil and nothing said once the volume lets it", err, len(a.lost))
	}

	// Another agent takes the lock: the loss is said, once, and saying it again does not wait for anyone.
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })
	for range 3 {
		if err := a.fence(context.Background()); !errors.Is(err, lock.ErrLost) {
			t.Fatalf("fence = %v, want the loss of the lock", err)
		}
	}
	if len(a.lost) != 1 {
		t.Errorf("the loss was said %d times, want once: one is enough, and a second does not wait", len(a.lost))
	}
}

func TestKeepsTheRowOfTheCheckoutInTheReportWhileItRunsWithNoLockToo(t *testing.T) {
	f := newFakePuller()
	running := make(chan struct{})
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Checkout = func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
			close(running)
			<-ctx.Done()
			return checkout.Result{}, ctx.Err()
		}
	})
	<-running

	got, found := checkoutCheck(t, r)
	next, nextFound := pullCheck(t, r)

	if !found || got.Outcome != outcomeSkipped || !strings.HasPrefix(got.Message, "running:") {
		t.Errorf("checkout = %+v, want a row that says it is running", got)
	}
	if !nextFound || next.Outcome != outcomeSkipped || !strings.Contains(next.Message, "checkout is not made yet") {
		t.Errorf("pull = %+v, want it skipped, because the checkout is not made yet", next)
	}
}

func TestTakesTheLockWithoutACheckoutAndReportsNoRowForTheCheckout(t *testing.T) {
	root, _ := claimWithState(t)
	r := start(t, takesTheLock(root, nil))
	eventually(t, "the lock in the report", func() bool {
		got, _ := lockCheck(t, r)
		return got.Outcome == outcomePassed
	})

	if names := checkNames(t, r); names != "Mount,Writable,Lock" {
		t.Errorf("checks = %s, want no row for a checkout that there is none of", names)
	}
}

func TestAnAgentThatLosesTheLockInTheMiddleOfAPullStopsThePullAndExitsWithTheLoss(t *testing.T) {
	root, lockDir := claimWithState(t)
	clock := newFakeClock()
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		takesTheLock(root, cloned)(o)
		change(o)
		o.LockClock = clock
	})
	f.awaitBegun(t)
	thief, err := lock.Acquire(context.Background(), lock.Config{Dir: lockDir, Holder: lockOther, TTL: lockTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thief.Release() })

	// The pull is running. Now the keeper makes its heartbeat, and finds that another holds the lock.
	clock.Advance(lockTTL)

	stopped := r.waitStopped(t)

	if !errors.Is(stopped, ErrLockLost) {
		t.Errorf("Run returned %v, want ErrLockLost", stopped)
	}
	if strings.Contains(r.logs.String(), "check=Pull outcome=Failed") {
		t.Errorf("a pull that was stopped by the loss was reported as a failure of the repository:\n%s", r.logs.String())
	}
}

func TestTheFenceOfAnAgentWithNoLockLetsTheUpdateThrough(t *testing.T) {
	f := newFakePuller()
	change, made := withPuller(f, &loopClock{})
	start(t, change)

	options := <-made

	if err := options.Fence(context.Background()); err != nil {
		t.Errorf("Fence = %v, want nil: there is no lock to look at", err)
	}
}

func TestOnlyAPullThatTheRepositoryOrTheVolumeCouldNotAnswerMakesTheNextWaitGrow(t *testing.T) {
	cases := map[string]bool{
		pull.ReasonFetchFailed:      true,
		pull.ReasonPullFailed:       true,
		pull.ReasonRefused:          true,
		pull.ReasonBlocked:          false,
		pull.ReasonTooManyDeletions: false,
		pull.ReasonDiverged:         false,
		pull.ReasonBranchGone:       false,
		pull.ReasonReservedPath:     false,
		pull.ReasonGitDirUnsafe:     false,
		"AReasonOfALaterRelease":    false,
	}
	for reason, want := range cases {
		if got := backsOff(reason); got != want {
			t.Errorf("backsOff(%s) = %v, want %v", reason, got, want)
		}
	}
}

func TestTurnsWhatAPullDidIntoAResultAndWhetherToWaitLonger(t *testing.T) {
	long := strings.Repeat("x", maxMessage+100)
	cases := []struct {
		name         string
		result       pull.Result
		err          error
		wantOutcome  preflight.Outcome
		wantReason   string
		wantMessage  string
		wantBacksOff bool
	}{
		{
			name:        "advanced a checkout that had no commit",
			result:      pull.Result{Outcome: pull.Advanced, Branch: testBranch, To: otherCommit},
			wantOutcome: preflight.Passed,
			wantMessage: "advanced the checkout on the branch main from no commit yet to fedcba987654",
		},
		{
			name:        "advanced a checkout that had a commit, which is kept",
			result:      pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: testCommit, To: otherCommit, Recovery: "refs/treebind/recovery/00000007"},
			wantOutcome: preflight.Passed,
			wantMessage: "advanced the checkout on the branch main from 0123456789ab to fedcba987654; the commit it was at is kept as refs/treebind/recovery/00000007",
		},
		{
			name: "advanced, with an old recovery ref that could not be removed",
			result: pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: testCommit, To: otherCommit,
				Recovery: "refs/treebind/recovery/00000007", Warning: "the old recovery ref refs/treebind/recovery/00000002 cannot be removed: locked"},
			wantOutcome: preflight.Passed,
			wantMessage: "advanced the checkout on the branch main from 0123456789ab to fedcba987654; the commit it was at is kept as " +
				"refs/treebind/recovery/00000007; the old recovery ref refs/treebind/recovery/00000002 cannot be removed: locked",
		},
		{
			name:        "at the tip with no commit, as an empty repository is",
			result:      pull.Result{Outcome: pull.UpToDate, Branch: "main"},
			wantOutcome: preflight.Passed,
			wantMessage: "the checkout is at the tip of the branch main (no commit yet)",
		},
		{
			name:         "an error of a pull",
			err:          &pull.Error{Reason: pull.ReasonFetchFailed, Message: "no route to host"},
			wantOutcome:  preflight.Failed,
			wantReason:   "FetchFailed",
			wantMessage:  "no route to host",
			wantBacksOff: true,
		},
		{
			name:        "an error of a pull that needs a person",
			err:         &pull.Error{Reason: pull.ReasonBlocked, Message: "a file has a local edit"},
			wantOutcome: preflight.Failed,
			wantReason:  "Blocked",
			wantMessage: "a file has a local edit",
		},
		{
			name:        "an update that removes too many files, which needs a person",
			err:         &pull.Error{Reason: pull.ReasonTooManyDeletions, Message: "the update would remove 101 file(s)"},
			wantOutcome: preflight.Failed,
			wantReason:  reasonTooMany,
			wantMessage: "the update would remove 101 file(s)",
		},
		{
			name:         "an error that wraps one of a pull",
			err:          fmt.Errorf("around: %w", &pull.Error{Reason: pull.ReasonRefused, Message: "not applied"}),
			wantOutcome:  preflight.Failed,
			wantReason:   "Refused",
			wantMessage:  "around: not applied",
			wantBacksOff: true,
		},
		{
			name:         "an error that is not one of a pull",
			err:          errors.New("something else"),
			wantOutcome:  preflight.Failed,
			wantReason:   "PullFailed",
			wantMessage:  "something else",
			wantBacksOff: true,
		},
	}
	for _, c := range cases {
		got, backs := pullResult(c.result, c.err)
		if got.Check != CheckPull || got.Outcome != c.wantOutcome || got.Reason != c.wantReason || got.Message != c.wantMessage || backs != c.wantBacksOff {
			t.Errorf("%s: pullResult = %+v, %v, want %s %q %q, %v", c.name, got, backs, c.wantOutcome, c.wantReason, c.wantMessage, c.wantBacksOff)
		}
	}

	got, _ := pullResult(pull.Result{}, errors.New(long))
	if len(got.Message) > maxMessage+3 || !strings.HasSuffix(got.Message, "...") {
		t.Errorf("a message of %d bytes was %d bytes in the report, want it cut", len(long), len(got.Message))
	}
}

// gatedClock lets the loop wait until the test says so, and says to the test that the loop is waiting, which
// is after the result of the pull that came before is in the report.
type gatedClock struct {
	asked chan gate
}

type gate struct {
	d  time.Duration
	ch chan time.Time
}

func newGatedClock() *gatedClock { return &gatedClock{asked: make(chan gate, 100)} }

func (c *gatedClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.asked <- gate{d: d, ch: ch}
	return ch
}

// wait returns the wait that the loop is in, once it is in one.
func (c *gatedClock) wait(t *testing.T) gate {
	t.Helper()
	select {
	case g := <-c.asked:
		return g
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not wait: it is stuck in a pull")
		return gate{}
	}
}

func (g gate) pass() { g.ch <- time.Time{} }

func TestAnUpdateOfTheRepositoryReachesTheCheckoutWhileAnEditOfTheApplicationIsKept(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n", jsonB: "b\n"})
	where := f.Layout(subDir)
	clock := newGatedClock()
	r := start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval = testInterval
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		// The checkout and the pulls are the ones of the packages, with the stand-in for ssh of the fixture.
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(options pull.Options) (Puller, error) {
			options.Git = f.GitOptions()
			return pull.New(options)
		}
	})
	check := func() status.Check {
		got, _ := pullCheck(t, r)
		return got
	}

	// The first pull, after the phase: nothing is new.
	first := clock.wait(t)
	first.pass()
	second := clock.wait(t)
	if got := check(); got.Outcome != outcomePassed || !strings.Contains(got.Message, "at the tip of the branch main") {
		t.Errorf("pull = %+v, want it passed, at the tip", got)
	}
	if second.d != testInterval {
		t.Errorf("the wait after the first pull is %v, want the interval", second.d)
	}

	// A commit in the repository reaches the work tree at the next pull.
	f.Publish(testBranch, map[string]string{jsonA: "a, changed\n", jsonC: "c\n"})
	second.pass()
	third := clock.wait(t)
	if got := check(); got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced the checkout on the branch main") {
		t.Errorf("pull = %+v, want it passed, advanced", got)
	}
	if gittest.Read(t, where, jsonA) != "a, changed\n" || gittest.Read(t, where, jsonC) != "c\n" {
		t.Error("the update of the repository is not in the work tree")
	}

	// An edit of the application that the next update would write over is kept, and said.
	gittest.Write(t, where, jsonB, "the application's own\n")
	f.Publish(testBranch, map[string]string{jsonB: "b, changed\n"})
	third.pass()
	fourth := clock.wait(t)
	got := check()
	if got.Outcome != outcomeFailed || got.Reason != "Blocked" || !strings.Contains(got.Message, jsonB) {
		t.Errorf("pull = %+v, want it blocked, with the file that is in the way", got)
	}
	if gittest.Read(t, where, jsonB) != "the application's own\n" {
		t.Error("the edit of the application was written over")
	}
	if fourth.d != testInterval {
		t.Errorf("the wait after Blocked is %v, want the interval: it needs a person, and not a slower poll", fourth.d)
	}

	// Once the file is as the repository has it, the pull goes on by itself.
	f.InCheckout(where, "checkout", "--", jsonB)
	fourth.pass()
	fifth := clock.wait(t)
	if got := check(); got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced") ||
		gittest.Read(t, where, jsonB) != "b, changed\n" {
		t.Errorf("pull = %+v, b.json = %q, want it advanced, with the file as the repository has it", got, gittest.Read(t, where, jsonB))
	}
	fifth.pass()
	clock.wait(t)
	if n := strings.Count(r.logs.String(), "msg=pull "); n != 5 {
		t.Errorf("the pulls were logged %d times, want 5: the tip, an advance, a block, an advance and the tip\n%s", n, r.logs.String())
	}
}

func TestAnUpdateThatRemovesTooManyFilesIsNotAppliedUntilThePersonHasRemovedThem(t *testing.T) {
	f := gittest.New(t)
	f.Publish(testBranch, map[string]string{jsonA: "a\n", jsonB: "b\n", jsonC: "c\n", jsonD: "d\n"})
	where := f.Layout(subDir)
	clock := newGatedClock()
	r := start(t, func(o *Options) {
		o.Config.MountPath, o.Config.SubPath = f.Claim, subDir
		o.Config.RepoURL, o.Config.RepoBranch = f.URL, testBranch
		o.Config.SyncInterval = testInterval
		o.SyncClock, o.SyncRandom = clock, func() float64 { return 0.5 }
		o.Checkout = func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			options.Git, options.FetchTimeout = f.GitOptions(), time.Minute
			return checkout.Open(ctx, options)
		}
		o.NewPuller = func(options pull.Options) (Puller, error) {
			options.Git = f.GitOptions()
			// A cap that a test can reach with a few files. The agent leaves it to the pull, which has its own.
			options.MaxDeletions = 2
			return pull.New(options)
		}
	})
	check := func() status.Check {
		got, _ := pullCheck(t, r)
		return got
	}
	// The first pull, after the phase: nothing is new.
	clock.wait(t).pass()
	waiting := clock.wait(t)

	// An update that removes three files, with the cap at two, is refused, and the files stay.
	f.Publish(testBranch, map[string]string{jsonA: "", jsonB: "", jsonC: ""})
	waiting.pass()
	waiting = clock.wait(t)
	got := check()
	if got.Outcome != outcomeFailed || got.Reason != reasonTooMany || !strings.Contains(got.Message, "3 file(s)") {
		t.Fatalf("pull = %+v, want it refused for 3 files", got)
	}
	if waiting.d != testInterval {
		t.Errorf("the wait after TooManyDeletions is %v, want the interval: it needs a person, and not a slower poll", waiting.d)
	}
	if !gittest.Exists(filepath.Join(where.WorkTree, jsonA)) {
		t.Error("a file is gone although the update was refused")
	}

	// It is tried again and says the same, and is logged once.
	waiting.pass()
	waiting = clock.wait(t)
	if n := r.logs.count("reason=TooManyDeletions"); n != 1 {
		t.Errorf("a refusal that was found twice was logged %d times, want once\n%s", n, r.logs.String())
	}

	// A person removes one of the files, so that the update removes two, which the cap allows.
	if err := os.Remove(filepath.Join(where.WorkTree, jsonA)); err != nil {
		t.Fatal(err)
	}
	waiting.pass()
	clock.wait(t)
	got = check()
	if got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced the checkout") ||
		!strings.Contains(got.Message, "the commit it was at is kept as refs/treebind/recovery/00000001") {
		t.Errorf("pull = %+v, want it advanced, with the recovery ref in the message", got)
	}
	if gittest.Exists(filepath.Join(where.WorkTree, jsonB)) || !gittest.Exists(filepath.Join(where.WorkTree, jsonD)) {
		t.Error("the files of the update are not as the update has them")
	}
}
