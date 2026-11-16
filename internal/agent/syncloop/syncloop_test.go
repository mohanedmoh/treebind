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

package syncloop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	interval = 30 * time.Second
	ten      = 10 * time.Minute
)

func TestJitterMovesTheIntervalByAFifthEitherWay(t *testing.T) {
	cases := []struct {
		random float64
		want   time.Duration
	}{
		{0, 24 * time.Second},
		{0.25, 27 * time.Second},
		{0.5, interval},
		{0.75, 33 * time.Second},
	}
	for _, c := range cases {
		if got := Jitter(interval, c.random); got != c.want {
			t.Errorf("Jitter(%v, %v) = %v, want %v", interval, c.random, got, c.want)
		}
	}
	if got := Jitter(interval, 0.999999); got <= 35*time.Second || got >= 36*time.Second {
		t.Errorf("Jitter(%v, just below 1) = %v, want just under 36s", interval, got)
	}
}

func TestBackoffDoublesTheCeilingUpToTheLongestWait(t *testing.T) {
	cases := []struct {
		failures int
		ceiling  time.Duration
	}{
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{4, 480 * time.Second},
		{5, ten},
		{6, ten},
		{1 << 30, ten},
	}
	for _, c := range cases {
		if got := Backoff(interval, ten, c.failures, 0); got != interval {
			t.Errorf("Backoff after %d failures with random 0 = %v, want the interval: never sooner than the interval", c.failures, got)
		}
		want := interval + (c.ceiling-interval)/2
		if got := Backoff(interval, ten, c.failures, 0.5); got != want {
			t.Errorf("Backoff after %d failures with random 0.5 = %v, want %v, half way to the ceiling %v", c.failures, got, want, c.ceiling)
		}
		got := Backoff(interval, ten, c.failures, 0.999999)
		if got >= c.ceiling || got < c.ceiling-time.Second {
			t.Errorf("Backoff after %d failures with random just below 1 = %v, want just under the ceiling %v", c.failures, got, c.ceiling)
		}
	}
}

func TestBackoffIsTheJitteredIntervalWhenTheIntervalIsAlreadyLongerThanTheLongestWait(t *testing.T) {
	long := 15 * time.Minute
	for _, random := range []float64{0, 0.25, 0.5, 0.999} {
		if got, want := Backoff(long, ten, 3, random), Jitter(long, random); got != want {
			t.Errorf("Backoff(%v, %v, 3, %v) = %v, want %v: no room to grow, and the jitter still spreads the agents", long, ten, random, got, want)
		}
	}
	if got := Backoff(long, ten, 3, 0); got >= long {
		t.Errorf("Backoff(%v, %v, 3, 0) = %v, want a wait that is moved by the jitter", long, ten, got)
	}
}

func TestBackoffStopsAtTheLongestWaitWhenTheDoublingWouldPassIt(t *testing.T) {
	six := 6 * time.Minute
	if got := Backoff(six, ten, 1, 0.5); got != 8*time.Minute {
		t.Errorf("Backoff(6m, 10m, 1, 0.5) = %v, want 8m, half way between the interval and the longest wait", got)
	}
}

func TestNextIsTheJitteredIntervalAfterASuccessAndTheBackoffAfterFailures(t *testing.T) {
	if got := Next(interval, ten, 0, 0.25); got != 27*time.Second {
		t.Errorf("Next after a success = %v, want the jittered interval 27s", got)
	}
	if got := Next(interval, ten, -1, 0.25); got != 27*time.Second {
		t.Errorf("Next with no failure = %v, want the jittered interval 27s", got)
	}
	if got := Next(interval, ten, 1, 0.5); got != 45*time.Second {
		t.Errorf("Next after one failure = %v, want 45s", got)
	}
	if got := Next(interval, ten, 2, 0.5); got != 75*time.Second {
		t.Errorf("Next after two failures = %v, want 75s", got)
	}
}

func TestPhaseIsAPointInTheIntervalThatEveryStartOfATreebindGets(t *testing.T) {
	first := Phase("apps", "dashboards", interval)
	if first < 0 || first >= interval {
		t.Errorf("Phase = %v, want a point in [0, %v)", first, interval)
	}
	if again := Phase("apps", "dashboards", interval); again != first {
		t.Errorf("Phase = %v and then %v, want the same at every start", first, again)
	}
	if other := Phase("apps", "dashboards", 10*interval); other >= 10*interval {
		t.Errorf("Phase = %v for an interval of %v, want it inside the interval", other, 10*interval)
	}
}

func TestPhaseSeparatesTheNamespaceFromTheName(t *testing.T) {
	if Phase("ab", "c", interval) == Phase("a", "bc", interval) {
		t.Error("the Treebinds ab/c and a/bc got one phase: the namespace and the name run together")
	}
	if Phase("a", "b", interval) == Phase("b", "a", interval) {
		t.Error("the Treebinds a/b and b/a got one phase")
	}
}

func TestPhaseSpreadsTheTreebindsOverTheInterval(t *testing.T) {
	const treebinds = 1000
	buckets := make([]int, 10)
	seen := map[time.Duration]bool{}
	for i := range treebinds {
		phase := Phase("apps", fmt.Sprintf("site-%d", i), interval)
		seen[phase] = true
		buckets[int(phase*10/interval)]++
	}
	if len(seen) < treebinds-5 {
		t.Errorf("%d Treebinds got %d phases, want almost as many as there are", treebinds, len(seen))
	}
	for i, n := range buckets {
		if n < 60 || n > 140 {
			t.Errorf("a tenth of the interval, number %d, got %d of %d Treebinds, want about %d", i, n, treebinds, treebinds/10)
		}
	}
}

func TestPhaseNeverReachesTheEndOfTheInterval(t *testing.T) {
	// With an interval of three nanoseconds there are three phases, and a hash gives each of them.
	seen := map[time.Duration]bool{}
	for i := range 200 {
		phase := Phase("apps", fmt.Sprintf("site-%d", i), 3)
		if phase < 0 || phase >= 3 {
			t.Fatalf("Phase = %v for an interval of 3ns, want 0, 1 or 2", phase)
		}
		seen[phase] = true
	}
	if len(seen) != 3 {
		t.Errorf("200 Treebinds got the phases %v, want all of 0, 1 and 2", seen)
	}
}

func TestPhaseOfAnIntervalThatIsNotPositiveIsNone(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := Phase("apps", "dashboards", d); got != 0 {
			t.Errorf("Phase for an interval of %v = %v, want none", d, got)
		}
	}
}

// recordingClock says how long every wait is, and lets it pass at once, so that no test waits.
type recordingClock struct {
	waits []time.Duration
	// onWait is called with each wait, before it passes.
	onWait func(d time.Duration)
	// never makes a wait that does not pass.
	never bool
}

func (c *recordingClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	if c.onWait != nil {
		c.onWait(d)
	}
	ch := make(chan time.Time, 1)
	if !c.never {
		ch <- time.Time{}
	}
	return ch
}

// script runs the pulls of a test: each says whether it failed, and when the last is done the context ends.
func script(cancel context.CancelFunc, failures ...bool) (func(context.Context) bool, *int) {
	pulls := 0
	return func(context.Context) bool {
		failed := failures[pulls]
		pulls++
		if pulls == len(failures) {
			cancel()
		}
		return failed
	}, &pulls
}

func TestRunWaitsThePhaseThenGrowsTheWaitAfterFailuresAndGoesBackToTheIntervalAfterASuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &recordingClock{}
	// Two failures, a success, a failure, and then the last pull, which stops the loop.
	pull, pulls := script(cancel, true, true, false, true, false)

	err := Run(ctx, Options{
		Interval: interval, Phase: 7 * time.Second, MaxWait: ten, Clock: clock,
		Random: func() float64 { return 0.5 }, Pull: pull,
	})

	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{
		7 * time.Second,  // the phase, before the first pull
		45 * time.Second, // after the first failure: between 30s and 60s
		75 * time.Second, // after the second: between 30s and 120s
		interval,         // after a success: the interval, which a random of 0.5 does not move
		45 * time.Second, // after a failure again: the count started again
	}
	if !slices.Equal(clock.waits, want) {
		t.Errorf("waits = %v, want %v", clock.waits, want)
	}
	if *pulls != 5 {
		t.Errorf("pulled %d times, want 5", *pulls)
	}
}

func TestRunDrawsEveryWaitFromTheRandomSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &recordingClock{}
	pull, _ := script(cancel, false, false, false)
	draws := []float64{0, 0.75}

	err := Run(ctx, Options{
		Interval: interval, Clock: clock, Pull: pull,
		Random: func() float64 { d := draws[0]; draws = draws[1:]; return d },
	})

	if err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{0, 24 * time.Second, 33 * time.Second}; !slices.Equal(clock.waits, want) {
		t.Errorf("waits = %v, want %v: no phase, and then the interval moved by what the source gave", clock.waits, want)
	}
}

func TestRunNeverWaitsLongerThanTheLongestWaitAndHasADefaultOfTenMinutes(t *testing.T) {
	for _, c := range []struct{ maxWait, want time.Duration }{{0, ten}, {time.Minute, time.Minute}} {
		ctx, cancel := context.WithCancel(context.Background())
		clock := &recordingClock{}
		failures := make([]bool, 30)
		for i := range failures {
			failures[i] = true
		}
		pull, _ := script(cancel, failures...)

		err := Run(ctx, Options{
			Interval: interval, MaxWait: c.maxWait, Clock: clock, Pull: pull, Random: func() float64 { return 0.999999 },
		})
		cancel()

		if err != nil {
			t.Fatal(err)
		}
		longest := slices.Max(clock.waits)
		if longest >= c.want || longest < c.want-time.Second {
			t.Errorf("with a longest wait of %v the longest wait was %v, want just under %v", c.maxWait, longest, c.want)
		}
	}
}

func TestRunStopsWhileItWaitsForTheNextPull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A wait that never passes. The loop is told to stop while it is in it.
	clock := &recordingClock{never: true, onWait: func(time.Duration) { cancel() }}
	var pulled bool

	err := Run(ctx, Options{Interval: interval, Clock: clock, Pull: func(context.Context) bool { pulled = true; return false }})

	if err != nil {
		t.Fatal(err)
	}
	if pulled {
		t.Error("it pulled after it was told to stop")
	}
}

func TestRunDoesNotPullWhenTheStopComesAsTheWaitPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		err := Run(ctx, Options{Interval: interval, Clock: &recordingClock{}, Pull: func(context.Context) bool {
			t.Error("a pull ran after the agent was told to stop")
			return false
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunStopsWhileAPullIsRunningAndDoesNotAskForAnotherWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &recordingClock{}
	running := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Interval: interval, Clock: clock, Pull: func(ctx context.Context) bool {
			close(running)
			<-ctx.Done()
			// A pull that was stopped says it failed, and that is no reason to wait for the next.
			return true
		}})
	}()
	<-running

	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(clock.waits) != 1 {
		t.Errorf("waits = %v, want only the one before the pull: a pull that was stopped is not followed by a wait", clock.waits)
	}
}

func TestRunUsesTheSystemClockWhenNoneIsGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pull, pulls := script(cancel, false)

	err := Run(ctx, Options{Interval: time.Hour, Pull: pull})

	if err != nil || *pulls != 1 {
		t.Errorf("Run = %v after %d pulls, want the first pull at once, with a phase of none, and a clean stop", err, *pulls)
	}
}

func TestRunWaitsOnTheSystemClockWhenNoneIsGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pull, _ := script(cancel, false, false, false)
	started := time.Now()

	err := Run(ctx, Options{Interval: 20 * time.Millisecond, Random: func() float64 { return 0 }, Pull: pull})

	// Two waits after the first pull, each at least 80% of the interval. The clock does not return before.
	if elapsed := time.Since(started); err != nil || elapsed < 32*time.Millisecond {
		t.Errorf("Run = %v after %v, want two waits of at least 16ms on the clock of the system", err, elapsed)
	}
}

func TestRunDrawsFromTheRandomSourceOfTheProcessWhenNoneIsGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &recordingClock{}
	pull, _ := script(cancel, make([]bool, 21)...)

	err := Run(ctx, Options{Interval: interval, Clock: clock, Pull: pull})

	if err != nil {
		t.Fatal(err)
	}
	distinct := map[time.Duration]bool{}
	for _, wait := range clock.waits[1:] {
		if wait < 24*time.Second || wait >= 36*time.Second {
			t.Errorf("a wait of %v, want one between 24s and 36s", wait)
		}
		distinct[wait] = true
	}
	if len(distinct) < 2 {
		t.Errorf("the waits were %v, want them to differ: the jitter comes from a source that varies", clock.waits)
	}
}

func TestRunRefusesOptionsThatCannotWork(t *testing.T) {
	pull := func(context.Context) bool { t.Error("a pull ran with options that cannot work"); return false }
	for name, o := range map[string]Options{
		"no interval":       {Pull: pull},
		"negative interval": {Interval: -time.Second, Pull: pull},
		"no pull":           {Interval: interval},
	} {
		err := Run(context.Background(), o)
		if err == nil || !strings.HasPrefix(err.Error(), "syncloop: ") {
			t.Errorf("%s: Run = %v, want an error", name, err)
		}
	}
}

// tickClock is a clock that moves only when a wait passes, by the length of the wait, so that no test waits
// and the loop sees time go by as it would. hook is called with each wait before it passes, and says whether
// it does: a test that sends a signal from it can keep the wait from passing, so that the signal is what the
// loop sees first.
type tickClock struct {
	now   time.Time
	waits []time.Duration
	hook  func(d time.Duration) (passes bool)
}

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTickClock() *tickClock { return &tickClock{now: epoch} }

func (c *tickClock) Now() time.Time { return c.now }

func (c *tickClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if c.hook == nil || c.hook(d) {
		c.now = c.now.Add(d)
		ch <- c.now
	}
	return ch
}

// elapsed is the time since the clock began, in whole seconds, which is how a turn is told.
func (c *tickClock) elapsed() int { return int(c.now.Sub(epoch) / time.Second) }

// turns records the pulls and the scans of a test in the order they happen, each with when, and ends the
// loop after the given number of them.
type turns struct {
	clock *tickClock
	log   []string
	stop  func()
	count int
}

func (r *turns) record(what string) {
	r.log = append(r.log, fmt.Sprintf("%s@%d", what, r.clock.elapsed()))
	if len(r.log) == r.count {
		r.stop()
	}
}

// options are the options of a loop with scans that records its turns. Each test changes what it needs.
func (r *turns) options(scans ...time.Duration) Options {
	i := 0
	return Options{
		Interval: time.Hour, Phase: time.Hour, ScanInterval: 30 * time.Second, ScanGap: 2 * time.Second, Clock: r.clock,
		Random: func() float64 { return 0.5 },
		Pull:   func(context.Context) bool { r.record("pull"); return false },
		Scan: func(context.Context, time.Time) time.Duration {
			var next time.Duration
			if i < len(scans) {
				next = scans[i]
			}
			i++
			r.record("scan")
			return next
		},
	}
}

func newTurns(count int) (*turns, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &turns{clock: newTickClock(), stop: cancel, count: count}, ctx
}

func wantTurns(t *testing.T, got *turns, want ...string) {
	t.Helper()
	if !slices.Equal(got.log, want) {
		t.Errorf("turns = %v, want %v", got.log, want)
	}
}

func TestWithScansTheLoopPullsAndScansEachOnItsOwnSchedule(t *testing.T) {
	r, ctx := newTurns(8)
	o := r.options()
	o.Interval, o.Phase, o.ScanInterval = interval, 7*time.Second, 10*time.Second

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// The first scan is at once, the first pull after the phase, a pull again after the interval, and a scan
	// every ten seconds in between.
	wantTurns(t, r, "scan@0", "pull@7", "scan@10", "scan@20", "scan@30", "pull@37", "scan@40", "scan@50")
}

func TestAScanThatSaysAFileSettlesSoonIsFollowedByAnotherAtThatTime(t *testing.T) {
	r, ctx := newTurns(4)
	// The first scan says that a file settles in 4s, the second that nothing waits, and the third that a
	// file settles in 100s, which is later than the interval, which then is the wait.
	o := r.options(4*time.Second, 0, 100*time.Second)

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	wantTurns(t, r, "scan@0", "scan@4", "scan@34", "scan@64")
}

func TestAScanThatTakesTimeIsFollowedByAnotherWhereTheFileSettlesAndNotLater(t *testing.T) {
	r, ctx := newTurns(2)
	o := r.options(10 * time.Second)
	inner := o.Scan
	o.Scan = func(c context.Context, now time.Time) time.Duration {
		next := inner(c, now)
		// The scan took four seconds, and the file settles ten seconds after the scan began.
		r.clock.now = r.clock.now.Add(4 * time.Second)
		return next
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// 10s after the first scan began, which is 6s after it ended. The wait before it is the first scan's.
	if got := r.clock.waits[1]; got != 6*time.Second {
		t.Errorf("the wait after the first scan = %v, want 6s", got)
	}
}

func TestAScanThatOutlastsTheSettleTimeIsFollowedByAnotherAfterTheGapAndNotAtOnce(t *testing.T) {
	r, ctx := newTurns(2)
	o := r.options(2 * time.Second)
	inner := o.Scan
	o.Scan = func(c context.Context, now time.Time) time.Duration {
		next := inner(c, now)
		r.clock.now = r.clock.now.Add(4 * time.Second) // the scan took 4s, and the file settled 2s after it began
		return next
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// The gap is 2s: scans of a large tree do not follow one another with nothing between them.
	wantTurns(t, r, "scan@0", "scan@6")
	if got := r.clock.waits[1]; got != 2*time.Second {
		t.Errorf("the wait after the first scan = %v, want the gap, 2s", got)
	}
}

func TestAnEventCallsForAScanAfterTheGapAndNoSoonerThanThat(t *testing.T) {
	r, ctx := newTurns(2)
	o := r.options()
	wake := make(chan struct{}, 1)
	o.Wake = wake
	r.clock.hook = func(d time.Duration) bool {
		if len(r.clock.waits) == 2 { // the wait after the first scan
			wake <- struct{}{}
			return false
		}
		return true
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	wantTurns(t, r, "scan@0", "scan@2")
	if want := []time.Duration{0, 30 * time.Second, 2 * time.Second}; !slices.Equal(r.clock.waits, want) {
		t.Errorf("waits = %v, want %v: none before the first scan, the scan interval, which an event cut short to the gap", r.clock.waits, want)
	}
}

func TestAnEventAfterALongQuietCallsForAScanAtOnce(t *testing.T) {
	r, ctx := newTurns(2)
	o := r.options()
	wake := make(chan struct{}, 1)
	o.Wake = wake
	r.clock.hook = func(d time.Duration) bool {
		if len(r.clock.waits) == 2 {
			r.clock.now = r.clock.now.Add(20 * time.Second) // 20s have passed since the last scan, more than the gap
			wake <- struct{}{}
			return false
		}
		return true
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	wantTurns(t, r, "scan@0", "scan@20")
	if got := r.clock.waits[2]; got != 0 {
		t.Errorf("wait after the event = %v, want none", got)
	}
}

func TestEventsThatComeWhileAScanRunsAreOneScanAfterIt(t *testing.T) {
	r, ctx := newTurns(3)
	o := r.options()
	wake := make(chan struct{}, 1)
	o.Wake = wake
	// A wait does not pass while a signal is waiting, so that the loop sees the signal first, as it does
	// when the signal came before the wait ended.
	r.clock.hook = func(time.Duration) bool { return len(wake) == 0 }
	inner := o.Scan
	first := true
	o.Scan = func(c context.Context, now time.Time) time.Duration {
		if first {
			first = false
			for range 5 {
				select { // as the watcher gives a signal: one is kept, and the rest are not
				case wake <- struct{}{}:
				default:
				}
			}
		}
		return inner(c, now)
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// One more scan after the gap for all the events, and then the interval.
	wantTurns(t, r, "scan@0", "scan@2", "scan@32")
}

func TestAClosedWakeChannelEndsTheEventsAndNotTheScans(t *testing.T) {
	// A loop that took the closed channel for an event at each turn would, with a clock whose waits pass at
	// once, sometimes take the wait and sometimes the event, so it is run many times, and what it asks of
	// the clock is what tells it: one wait for each turn.
	for range 30 {
		r, ctx := newTurns(3)
		o := r.options()
		wake := make(chan struct{})
		close(wake)
		o.Wake = wake

		done := make(chan error, 1)
		go func() { done <- Run(ctx, o) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the loop spins on a closed channel of events")
		}
		wantTurns(t, r, "scan@0", "scan@30", "scan@60")
		// A wait for each turn, and one more at the most: the turn that finds the channel closed.
		if n := len(r.clock.waits); n < 3 || n > 4 {
			t.Fatalf("waits = %v, want three or four: a wait for each turn, and none for the closed channel after it is seen", r.clock.waits)
		}
	}
}

func TestWhenBothTurnsAreOverdueTheOneThatWasDueFirstIsTaken(t *testing.T) {
	r, ctx := newTurns(3)
	// The first scan says that a file settles in 8s, and the first pull is due at 10s. The wait for the scan
	// overshoots by 5s, as a wait of a real clock can, so that at 13s both are due: the scan was due first.
	o := r.options(8 * time.Second)
	o.Phase = 10 * time.Second
	r.clock.hook = func(time.Duration) bool {
		if len(r.clock.waits) == 2 {
			r.clock.now = r.clock.now.Add(5 * time.Second)
		}
		return true
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	wantTurns(t, r, "scan@0", "scan@13", "pull@13")
}

func TestAPullAndAScanThatAreDueTogetherAreTakenPullFirst(t *testing.T) {
	r, ctx := newTurns(3)
	o := r.options()
	o.Phase = 0
	o.Interval = 45 * time.Second
	o.ScanInterval = time.Hour

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	wantTurns(t, r, "pull@0", "scan@0", "pull@45")
}

func TestPullsThatFailStillGrowTheirWaitBesideTheScans(t *testing.T) {
	r, ctx := newTurns(6)
	o := r.options()
	o.Phase = 0
	o.Interval = interval
	o.MaxWait = ten
	o.ScanInterval = time.Hour
	o.Pull = func(context.Context) bool { r.record("pull"); return true }

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// With a random of 0.5: after the first failure 45s, after the second 75s, after the third 135s, and after
	// the fourth 255s.
	wantTurns(t, r, "pull@0", "scan@0", "pull@45", "pull@120", "pull@255", "pull@510")
}

func TestAStopDuringAScanEndsTheLoopWithoutAnotherTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := newTickClock()
	var scans, pulls int
	err := Run(ctx, Options{
		Interval: interval, Clock: clock, ScanInterval: time.Second,
		Pull: func(context.Context) bool { pulls++; return false },
		Scan: func(context.Context, time.Time) time.Duration { scans++; cancel(); return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if scans != 1 || pulls != 1 {
		t.Errorf("scans = %d, pulls = %d, want 1 and 1: the pull that was due with it came first, and nothing came after the stop", scans, pulls)
	}
}

func TestScansNeedAClockThatSaysWhatTimeItIs(t *testing.T) {
	err := Run(context.Background(), Options{
		Interval: interval, Clock: &recordingClock{},
		Pull: func(context.Context) bool { return false },
		Scan: func(context.Context, time.Time) time.Duration { return 0 },
	})
	if err == nil || !strings.Contains(err.Error(), "Now") {
		t.Errorf("error = %v, want one that says that the clock needs Now", err)
	}
}

func TestTheScanIntervalAndTheGapHaveDefaults(t *testing.T) {
	r, ctx := newTurns(3)
	o := r.options()
	o.ScanInterval, o.ScanGap = 0, 0
	wake := make(chan struct{}, 1)
	o.Wake = wake
	r.clock.hook = func(time.Duration) bool {
		if len(r.clock.waits) == 3 { // the wait after the second scan
			wake <- struct{}{}
			return false
		}
		return true
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// scan@0, then the default interval twice, and an event just after the second scan gives the default gap.
	if want := []time.Duration{0, DefaultScanInterval, DefaultScanInterval, DefaultScanGap}; !slices.Equal(r.clock.waits, want) {
		t.Errorf("waits = %v, want %v", r.clock.waits, want)
	}
	if DefaultScanInterval != 30*time.Second || DefaultScanGap != time.Second {
		t.Errorf("defaults = %v and %v, want 30s and 1s", DefaultScanInterval, DefaultScanGap)
	}
}

func TestScansRunOnTheSystemClockWhenNoneIsGiven(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var starts []time.Time
	err := Run(ctx, Options{
		Interval: time.Hour, Phase: time.Hour, ScanInterval: 20 * time.Millisecond,
		Pull: func(context.Context) bool { return false },
		Scan: func(_ context.Context, now time.Time) time.Duration {
			starts = append(starts, now)
			if len(starts) == 3 {
				cancel()
			}
			return 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 || starts[2].Sub(starts[0]) < 40*time.Millisecond {
		t.Errorf("scans began at %v, want three of them 20ms apart on the system clock", starts)
	}
	if now := (systemClock{}).Now(); time.Since(now) > time.Second || time.Since(now) < 0 {
		t.Errorf("Now of the system clock = %v, want the time", now)
	}
}

func TestTheScanIntervalIsFromTheEndOfAScanAndAPullIsFromTheEndOfAPull(t *testing.T) {
	r, ctx := newTurns(4)
	o := r.options()
	o.Phase, o.Interval, o.ScanInterval = 0, 100*time.Second, time.Minute
	scan, pull := o.Scan, o.Pull
	o.Scan = func(c context.Context, now time.Time) time.Duration {
		next := scan(c, now)
		r.clock.now = r.clock.now.Add(4 * time.Second) // a scan takes 4s
		return next
	}
	o.Pull = func(c context.Context) bool {
		failed := pull(c)
		r.clock.now = r.clock.now.Add(10 * time.Second) // a pull takes 10s
		return failed
	}

	if err := Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	// pull@0 ends at 10, scan@10 ends at 14, and the next scan is the interval after that: 74. The next pull is
	// the interval after the end of the first, 110.
	wantTurns(t, r, "pull@0", "scan@10", "scan@74", "pull@110")
}
