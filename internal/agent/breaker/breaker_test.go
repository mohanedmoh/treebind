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

package breaker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// epoch is the time of the tests, which is far from any real clock and is a whole second.
var epoch = time.Unix(1_800_000_000, 0)

// clock is a clock that a test moves.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// world is a state directory, a clock, and a breaker that has a limit of three.
type world struct {
	dir     string
	clock   *clock
	fenced  int
	fence   error
	breaker *Breaker
}

func newWorld(t *testing.T, change ...func(*Options)) *world {
	t.Helper()
	w := &world{dir: t.TempDir(), clock: &clock{now: epoch}}
	o := Options{
		Dir: w.dir, Limit: 3, Now: w.clock.Now,
		Fence: func(context.Context) error {
			w.fenced++
			return w.fence
		},
	}
	for _, c := range change {
		c(&o)
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	w.breaker = b
	return w
}

// journal is what the file holds, or "" when there is none.
func (w *world) journal(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, journalFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// seed writes the file, with the times given as seconds from the epoch.
func (w *world) seed(t *testing.T, seconds ...int64) {
	t.Helper()
	var b strings.Builder
	for _, s := range seconds {
		b.WriteString(strconv.FormatInt(epoch.Unix()+s, 10) + "\n")
	}
	if err := os.WriteFile(filepath.Join(w.dir, journalFile), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// made records a commit at the time of the clock.
func (w *world) made(t *testing.T) {
	t.Helper()
	if err := w.breaker.Made(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// held returns the error of a commit that is not allowed, and fails the test for any other error.
func (w *world) held(t *testing.T) *HeldError {
	t.Helper()
	err := w.breaker.Allow(context.Background())
	held, ok := errors.AsType[*HeldError](err)
	if !ok {
		t.Fatalf("Allow = %v, want a *HeldError", err)
	}
	return held
}

func (w *world) allowed(t *testing.T) {
	t.Helper()
	if err := w.breaker.Allow(context.Background()); err != nil {
		t.Fatalf("Allow = %v, want a commit to be allowed", err)
	}
}

func TestNewRefusesWhatCannotWorkAndHasTheDefaultLimitForNone(t *testing.T) {
	dir := t.TempDir()
	for name, o := range map[string]Options{
		"no directory":                         {Limit: 3},
		"a negative limit":                     {Dir: dir, Limit: -1},
		"a limit over the most the API admits": {Dir: dir, Limit: MaxLimit + 1},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("New with %s succeeded, want an error", name)
		}
	}
	b, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if b.Limit() != DefaultLimit {
		t.Errorf("a breaker with no limit has %d, want the default %d", b.Limit(), DefaultLimit)
	}
	if _, err := New(Options{Dir: dir, Limit: MaxLimit}); err != nil {
		t.Errorf("New with the most the API admits = %v, want it taken", err)
	}
	if _, err := New(Options{Dir: dir, Limit: MinLimit}); err != nil {
		t.Errorf("New with the least the API admits = %v, want it taken", err)
	}
}

func TestACommitIsAllowedUntilTheLimitIsReachedAndThenHeld(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= 3; i++ {
		w.allowed(t)
		w.made(t)
		w.clock.advance(time.Minute)
	}

	held := w.held(t)

	if held.Count != 3 || held.Limit != 3 {
		t.Errorf("held = %+v, want 3 commits against a limit of 3", held)
	}
	// The oldest commit was made at the epoch, and the next can be made when it is an hour old.
	if want := epoch.Add(time.Hour); !held.Until.Equal(want) {
		t.Errorf("Until = %v, want %v: an hour after the oldest commit of the hour", held.Until, want)
	}
}

func TestACommitIsAllowedAgainWhenTheOldestOfTheHourIsAnHourOld(t *testing.T) {
	w := newWorld(t)
	w.seed(t, 0, 60, 120)
	w.clock.advance(time.Hour - time.Second)
	if held := w.held(t); held.Count != 3 {
		t.Fatalf("held = %+v, want 3 commits a second before the oldest is an hour old", held)
	}

	w.clock.advance(time.Second)

	w.allowed(t)
	w.made(t)
	if held := w.held(t); held.Count != 3 || !held.Until.Equal(epoch.Add(60*time.Second+time.Hour)) {
		t.Errorf("held = %+v, want 3 commits, the oldest of which was made a minute after the epoch", held)
	}
}

func TestMoreCommitsThanTheLimitHoldUntilEnoughOfThemAreOld(t *testing.T) {
	// A limit that was lowered after more commits were made than it allows: every commit that is over is to be an hour old.
	w := newWorld(t)
	w.seed(t, 0, 10, 20, 30, 40)

	held := w.held(t)

	if held.Count != 5 || !held.Until.Equal(epoch.Add(20*time.Second+time.Hour)) {
		t.Errorf("held = %+v, want 5 commits and a wait until the third is an hour old: only then are there fewer than 3", held)
	}
}

func TestACheckReadsAndNeverWritesOrAsksTheFence(t *testing.T) {
	w := newWorld(t)
	w.allowed(t)
	if got := w.journal(t); got != "" {
		t.Errorf("the file holds %q after a check, want no file", got)
	}
	w.seed(t, 0, 1, 2)
	before := w.journal(t)

	w.held(t)

	if w.journal(t) != before || w.fenced != 0 {
		t.Errorf("a check changed the file or asked the fence (%d times), want neither", w.fenced)
	}
}

func TestACommitThatWasMadeIsCountedByABreakerThatIsMadeAfterARestart(t *testing.T) {
	w := newWorld(t)
	w.made(t)
	w.made(t)
	w.made(t)

	again, err := New(Options{Dir: w.dir, Limit: 3, Now: w.clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	if err := again.Allow(context.Background()); err == nil {
		t.Error("a breaker made after a restart allows a commit, want it held: the commits of the hour are in the file")
	}
}

func TestRecordingWritesTheFenceFirstAndLeavesAFileOfTheCommitsOfTheHourOnly(t *testing.T) {
	w := newWorld(t)
	w.seed(t, -7200, -3600, -3594, 10)
	w.clock.advance(5 * time.Second)

	w.made(t)

	// The times that are an hour old or more are gone, so that the file does not grow for as long as the agent runs.
	want := strconv.FormatInt(epoch.Unix()-3594, 10) + "\n" + strconv.FormatInt(epoch.Unix()+5, 10) + "\n" + strconv.FormatInt(epoch.Unix()+10, 10) + "\n"
	if got := w.journal(t); got != want {
		t.Errorf("the file holds %q, want %q", got, want)
	}
	if w.fenced != 1 {
		t.Errorf("the fence was asked %d times, want once before the write", w.fenced)
	}
	if entries, _ := os.ReadDir(w.dir); len(entries) != 1 {
		t.Errorf("the state directory holds %d files, want only the file: no file that was written aside is left", len(entries))
	}
}

func TestAFenceThatRefusesIsANoteOfNothingAndAnError(t *testing.T) {
	w := newWorld(t)
	w.seed(t, 0)
	before := w.journal(t)
	w.fence = errors.New("the lock is lost")

	err := w.breaker.Made(context.Background())

	if err == nil || !strings.Contains(err.Error(), "the lock is lost") {
		t.Errorf("Made = %v, want an error that has the words of the fence", err)
	}
	if w.journal(t) != before {
		t.Errorf("the file was written although the fence refused")
	}
}

func TestLinesThatAreNotATimeAreSkippedAndATimeMoreThanAnHourAheadIsDropped(t *testing.T) {
	w := newWorld(t)
	data := "not a time\n\n" + strconv.FormatInt(epoch.Unix()+int64(time.Hour/time.Second)+1, 10) + "\n" + // more than an hour ahead
		strconv.FormatInt(epoch.Unix()+int64(time.Hour/time.Second), 10) + "\n" + // exactly an hour ahead
		strconv.FormatInt(epoch.Unix()+int64(time.Hour/time.Second)-1, 10) + "\n" + // a clock that was set back counts
		"12 34\n" + strconv.FormatInt(epoch.Unix()-1, 10) + "\n"
	if err := os.WriteFile(filepath.Join(w.dir, journalFile), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	w.made(t)

	held := w.held(t)

	// Counted: the time that is just under an hour ahead, the time just before now, and the commit that was made.
	if held.Count != 3 {
		t.Errorf("held = %+v, want 3 commits: lines that are not a time and a time more than an hour ahead are not commits", held)
	}
	if got := w.journal(t); strings.Contains(got, "not a time") || strings.Contains(got, "12 34") {
		t.Errorf("the file still holds what is not a time after a record: %q", got)
	}
}

func TestAFileThatCannotBeReadIsAnErrorAndNotAPermissionNorAHold(t *testing.T) {
	w := newWorld(t)
	if err := os.Mkdir(filepath.Join(w.dir, journalFile), 0o700); err != nil {
		t.Fatal(err)
	}

	err := w.breaker.Allow(context.Background())

	if err == nil {
		t.Fatal("Allow = nil, want an error: what the agent cannot read it cannot count")
	}
	if _, held := errors.AsType[*HeldError](err); held {
		t.Errorf("Allow = %v, want an error that says the file cannot be read, and not a hold", err)
	}
	if err := w.breaker.Made(context.Background()); err == nil {
		t.Error("Made = nil, want an error for a file that cannot be read")
	}
}

func TestAWriteThatFailsLeavesTheFileAsItWasAndTheNextRecordPutsItRight(t *testing.T) {
	w := newWorld(t)
	w.seed(t, 0)
	before := w.journal(t)
	// The file that is written aside cannot be made when something else is where it goes.
	aside := filepath.Join(w.dir, journalFile+".tmp")
	if err := os.Mkdir(aside, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := w.breaker.Made(context.Background()); err == nil {
		t.Fatal("Made = nil, want an error for a file that cannot be written")
	}
	if w.journal(t) != before {
		t.Errorf("the file is %q after a write that failed, want %q", w.journal(t), before)
	}
	if err := os.Remove(aside); err != nil {
		t.Fatal(err)
	}

	w.made(t)

	if got := strings.Count(w.journal(t), "\n"); got != 2 {
		t.Errorf("the file has %d times after the record that worked, want 2: the one that was there and the commit", got)
	}
}

func TestAFileThatWasWrittenAsideByAKilledAgentIsReplacedAndNotRead(t *testing.T) {
	w := newWorld(t)
	w.seed(t, 0)
	// A kill between the write of the file aside and the rename leaves this.
	aside := filepath.Join(w.dir, journalFile+".tmp")
	if err := os.WriteFile(aside, []byte(strconv.FormatInt(epoch.Unix(), 10)+"\n"+strconv.FormatInt(epoch.Unix(), 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if held := w.breaker.Allow(context.Background()); held != nil {
		t.Fatalf("Allow = %v, want a commit to be allowed: the file that was written aside is not the journal", held)
	}

	w.made(t)

	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file written aside is still there after a record: %v", err)
	}
	if got := strings.Count(w.journal(t), "\n"); got != 2 {
		t.Errorf("the file has %d times, want 2: the one that was there and the commit", got)
	}
}

func TestTheMessageOfAHoldSaysTheLimitAndWhatWaitsAndHasNoTime(t *testing.T) {
	held := &HeldError{Count: 60, Limit: 60, Until: epoch}

	got := held.Error()

	for _, want := range []string{"60 commits", "last hour", "limit", "spec.commit.maxPerHour", "stay on the volume"} {
		if !strings.Contains(got, want) {
			t.Errorf("the message %q has no %q", got, want)
		}
	}
	if strings.Contains(got, strconv.FormatInt(epoch.Unix(), 10)) || strings.Contains(got, epoch.Format(time.RFC3339)) || strings.Contains(got, "UTC") {
		t.Errorf("the message %q has a time, so that a hold that is the same would be a new line of the log at every scan", got)
	}
	one := &HeldError{Count: 1, Limit: 1}
	if !strings.Contains(one.Error(), "1 commit ") {
		t.Errorf("the message %q has no \"1 commit\" for a count of one", one.Error())
	}
}
