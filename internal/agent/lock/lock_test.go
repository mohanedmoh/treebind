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

package lock

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testTTL      = 30 * time.Second
	testInterval = 10 * time.Second
	// patience is how long a test waits in real time for something that has to
	// happen before it gives up.
	patience = 20 * time.Second
)

// epochStart is where the fake clocks start.
var epochStart = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// fakeClock is a clock that moves only when a test moves it. Each agent in a
// test gets its own, the way each pod has its own.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	// parked gets a token when a goroutine starts to wait on the clock.
	parked chan struct{}
	// tick is how far the clock moves on with every reading, for a test in
	// which time passes while the code under test works.
	tick time.Duration
}

type fakeWaiter struct {
	due time.Time
	ch  chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, parked: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(c.tick)
	return now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waiters = append(c.waiters, fakeWaiter{due: c.now.Add(d), ch: ch})
	c.mu.Unlock()
	select {
	case c.parked <- struct{}{}:
	default:
	}
	return ch
}

// advance moves the clock and wakes the waiters whose time has come.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var still []fakeWaiter
	for _, w := range c.waiters {
		if w.due.After(c.now) {
			still = append(still, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = still
}

// jump sets the wall time of the clock, as an operator or NTP would.
func (c *fakeClock) jump(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = to
}

// waitParked waits until the goroutine that uses the clock waits on it, and
// reports false when that goroutine finished instead.
func (c *fakeClock) waitParked(t *testing.T, done <-chan struct{}) bool {
	t.Helper()
	select {
	case <-done:
		return false
	case <-c.parked:
		c.parked <- struct{}{}
		return true
	case <-time.After(patience):
		t.Fatal("nothing waited on the clock and nothing finished")
		return false
	}
}

// step advances the clock by one interval once the goroutine that uses it waits
// on it. It reports false, and leaves the clock alone, when that goroutine has
// finished.
func (c *fakeClock) step(t *testing.T, done <-chan struct{}) bool {
	t.Helper()
	if !c.waitParked(t, done) {
		return false
	}
	<-c.parked
	c.advance(testInterval)
	return true
}

// testConfig is the configuration of an agent with its own fake clock.
func testConfig(dir, holder string, clock Clock) Config {
	return Config{Dir: dir, Holder: holder, TTL: testTTL, Interval: testInterval, Clock: clock}
}

// lockDir is a lock directory that does not exist yet, in a real directory.
func lockDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "lock")
}

// soon is a context for an Acquire that has to return at once, so that one that
// waits instead fails its test and does not hang it.
func soon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), patience)
	t.Cleanup(cancel)
	return ctx
}

// mustAcquire takes a lock that is free. It fails the test when the lock is not
// handed over at once.
func mustAcquire(t *testing.T, cfg Config) *Lock {
	t.Helper()
	l, err := Acquire(soon(t), cfg)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return l
}

// attempt is an Acquire that runs in its own goroutine.
type attempt struct {
	done chan struct{}
	lock *Lock
	err  error
}

func acquireAsync(ctx context.Context, cfg Config) *attempt {
	a := &attempt{done: make(chan struct{})}
	go func() {
		defer close(a.done)
		a.lock, a.err = Acquire(ctx, cfg)
	}()
	return a
}

// wait waits for the attempt to return.
func (a *attempt) wait(t *testing.T) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(patience):
		t.Fatal("Acquire did not return")
	}
}

// waitUntilHeld steps the reader's clock until its attempt returns and reports
// how much of the reader's time that took.
func waitUntilHeld(t *testing.T, clock *fakeClock, a *attempt) time.Duration {
	t.Helper()
	start := clock.Now()
	for range 100 {
		if !clock.step(t, a.done) {
			if a.err != nil {
				t.Fatalf("Acquire: %v", a.err)
			}
			return clock.Now().Sub(start)
		}
	}
	t.Fatal("the lock was not acquired in 100 intervals")
	return 0
}

// slotsIn lists the slots in the directory in ascending order.
func slotsIn(t *testing.T, dir string) []uint64 {
	t.Helper()
	slots, err := listSlots(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(slots)
	return slots
}

// writeSlot puts a slot with any content into the directory, as another agent,
// a crash or a newer version could leave it.
func writeSlot(t *testing.T, dir string, n uint64, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slotPath(dir, n), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wantLost fails the test unless the error says the lock is lost.
func wantLost(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
}

func TestConfigIsChecked(t *testing.T) {
	const badInterval = "lock: the interval must be positive and at most half the TTL"
	valid := Config{Dir: "/volume/lock", Holder: "sync-0", TTL: 30 * time.Second}
	tests := []struct {
		name    string
		change  func(*Config)
		wantErr string
	}{
		{"the defaults", func(*Config) {}, ""},
		{"an interval of half the TTL", func(c *Config) { c.Interval = 15 * time.Second }, ""},
		{"a name of the greatest length", func(c *Config) { c.Holder = strings.Repeat("a", 256) }, ""},
		{"no directory", func(c *Config) { c.Dir = "" }, "lock: no directory is set"},
		{"no holder", func(c *Config) { c.Holder = "" }, "lock: the holder's name must have 1 to 256 bytes"},
		{"a name that is too long", func(c *Config) { c.Holder = strings.Repeat("a", 257) },
			"lock: the holder's name must have 1 to 256 bytes"},
		{"no TTL", func(c *Config) { c.TTL = 0 }, "lock: the TTL must be positive"},
		{"a negative TTL", func(c *Config) { c.TTL = -time.Second }, "lock: the TTL must be positive"},
		{"a TTL too short for an interval", func(c *Config) { c.TTL = 2 }, badInterval},
		{"a negative interval", func(c *Config) { c.Interval = -time.Second }, badInterval},
		{"an interval above half the TTL", func(c *Config) { c.Interval = 15*time.Second + 1 }, badInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.change(&cfg)

			got, err := cfg.checked()

			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("got error %v, want %q", err, test.wantErr)
				}
				if _, err := Acquire(soon(t), cfg); err == nil || err.Error() != test.wantErr {
					t.Errorf("Acquire returned %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Clock == nil {
				t.Error("no clock was set")
			}
			if cfg.Interval == 0 && got.Interval != 10*time.Second {
				t.Errorf("the interval is %s, want a third of the TTL", got.Interval)
			}
		})
	}
}

func TestAcquireTakesAFreeLock(t *testing.T) {
	t.Run("a lock nobody ever held is taken at once, and its record says who holds it", func(t *testing.T) {
		dir := lockDir(t)
		// The holder's clock is in another zone. The record is in UTC all the same.
		clock := newFakeClock(epochStart.In(time.FixedZone("east", 2*60*60)))

		l := mustAcquire(t, testConfig(dir, "sync-0/1f3a", clock))

		if l.Epoch() != 1 {
			t.Errorf("the first epoch is %d, want 1", l.Epoch())
		}
		slot := filepath.Join(dir, "slot-00000000000000000001")
		data, err := os.ReadFile(slot)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"version":1,"epoch":1,"holder":"sync-0/1f3a","time":"2026-11-13T09:00:00Z","released":false}` + "\n"
		if string(data) != want {
			t.Errorf("the record is %s, want %s", data, want)
		}
		// Agents of one group can all make slots, and nobody else changes one.
		mask := os.FileMode(syscall.Umask(0))
		syscall.Umask(int(mask))
		for path, want := range map[string]os.FileMode{dir: 0o775 &^ mask, slot: 0o644 &^ mask} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s has mode %v (%v), want %v", path, info.Mode().Perm(), err, want)
			}
		}
	})

	t.Run("a file with the name of slot zero is not a slot", func(t *testing.T) {
		dir := lockDir(t)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "slot-00000000000000000000"), []byte(`{"version":2}`), 0o644); err != nil {
			t.Fatal(err)
		}

		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))

		if l.Epoch() != 1 {
			t.Errorf("the first epoch is %d, want 1", l.Epoch())
		}
	})

	t.Run("the system clock is used when none is given", func(t *testing.T) {
		l := mustAcquire(t, Config{Dir: lockDir(t), Holder: "sync-0", TTL: time.Minute})

		if err := l.Check(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a directory whose parent is missing is not created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing", "lock")

		_, err := Acquire(soon(t), testConfig(dir, "sync-0", newFakeClock(epochStart)))

		if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "lock: create the lock directory: ") {
			t.Errorf("got %v, want the failure to create the directory", err)
		}
	})

	t.Run("a path that is a file fails", func(t *testing.T) {
		dir := lockDir(t)
		if err := os.WriteFile(dir, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Acquire(soon(t), testConfig(dir, "sync-0", newFakeClock(epochStart)))

		if err == nil || !strings.HasPrefix(err.Error(), "lock: list the lock directory: ") {
			t.Errorf("got %v, want the failure to list the directory", err)
		}
	})

	t.Run("a clock too far off to be written down fails", func(t *testing.T) {
		clock := newFakeClock(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))

		_, err := Acquire(soon(t), testConfig(lockDir(t), "sync-0", clock))

		if err == nil || !strings.HasPrefix(err.Error(), "lock: encode the record: ") {
			t.Errorf("got %v, want the failure to encode the record", err)
		}
	})

	t.Run("a context that has ended takes nothing", func(t *testing.T) {
		dir := lockDir(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := Acquire(ctx, testConfig(dir, "sync-0", newFakeClock(epochStart)))

		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want the context's error", err)
		}
		if slots := slotsIn(t, dir); len(slots) != 0 {
			t.Errorf("slots %v were made", slots)
		}
	})
}

// TestTakeoverIsTimedByTheReadersClock freezes a holder and lets a reader wait.
// The reader takes over after exactly one TTL of its own clock, whatever the
// holder's clock wrote into the record.
func TestTakeoverIsTimedByTheReadersClock(t *testing.T) {
	tests := []struct {
		name        string
		holderClock time.Time
	}{
		{"the holder's clock agrees with the reader's", epochStart},
		{"the holder's clock is twenty years ahead", epochStart.AddDate(20, 0, 0)},
		{"the holder's clock is twenty years behind", epochStart.AddDate(-20, 0, 0)},
		{"the holder's clock is one interval short of a TTL behind", epochStart.Add(testInterval - testTTL)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := lockDir(t)
			holder := mustAcquire(t, testConfig(dir, "old", newFakeClock(test.holderClock)))
			if err := holder.Check(); err != nil {
				t.Fatal(err)
			}

			readerClock := newFakeClock(epochStart)
			reader := acquireAsync(context.Background(), testConfig(dir, "new", readerClock))
			waited := waitUntilHeld(t, readerClock, reader)

			if waited != testTTL {
				t.Errorf("the reader took over after %s of its own time, want %s", waited, testTTL)
			}
			if reader.lock.Epoch() <= holder.Epoch() {
				t.Errorf("the new epoch %d is not above the old one %d", reader.lock.Epoch(), holder.Epoch())
			}
			// The holder wakes up. It has to find out at its next check.
			wantLost(t, holder.Check())
			wantLost(t, holder.Release())
			if err := reader.lock.Check(); err != nil {
				t.Errorf("the new holder's check failed: %v", err)
			}
		})
	}
}

// TestALiveHolderIsNotTakenOver lets a holder renew while a reader waits far
// longer than a TTL, with the holder's wall clock jumping about.
func TestALiveHolderIsNotTakenOver(t *testing.T) {
	dir := lockDir(t)
	holderClock := newFakeClock(epochStart)
	holder := mustAcquire(t, testConfig(dir, "holder", holderClock))

	readerClock := newFakeClock(epochStart)
	reader := acquireAsync(t.Context(), testConfig(dir, "reader", readerClock))

	// The holder renews every second interval of the reader, which is within the
	// TTL, and its wall clock is wrong in a new way every time.
	jumps := []time.Time{
		epochStart.AddDate(-30, 0, 0), epochStart.AddDate(30, 0, 0), epochStart.Add(-testTTL), epochStart,
	}
	for i := range 24 {
		if !readerClock.waitParked(t, reader.done) {
			t.Fatalf("the reader stopped waiting after %d intervals: lock %v, error %v", i, reader.lock, reader.err)
		}
		if i%2 == 0 {
			holderClock.jump(jumps[i/2%len(jumps)])
			if err := holder.Check(); err != nil {
				t.Fatalf("the holder's check failed after %d intervals: %v", i, err)
			}
		}
		readerClock.step(t, reader.done)
	}

	// The holder stops. The reader counts a full TTL from the last renewal it
	// saw, not from when it began to wait.
	lastSeen := readerClock.Now()
	waitUntilHeld(t, readerClock, reader)
	if got := readerClock.Now().Sub(lastSeen); got != testTTL-testInterval {
		// The last renewal was seen one interval before lastSeen.
		t.Errorf("the reader took over %s after the holder stopped, want %s", got, testTTL-testInterval)
	}
	wantLost(t, holder.Check())
}

// TestSlotsThatVanishStartTheWaitAnew empties the directory under a contender
// that is two thirds through its wait. What it knew is gone, so it waits a whole
// TTL again: a holder that is alive makes its next slot in that time.
func TestSlotsThatVanishStartTheWaitAnew(t *testing.T) {
	dir := lockDir(t)
	mustAcquire(t, testConfig(dir, "holder", newFakeClock(epochStart)))
	clock := newFakeClock(epochStart)
	reader := acquireAsync(context.Background(), testConfig(dir, "reader", clock))
	for range 2 {
		if !clock.step(t, reader.done) {
			t.Fatalf("the reader stopped waiting: lock %v, error %v", reader.lock, reader.err)
		}
	}
	clock.waitParked(t, reader.done)

	if err := os.Remove(slotPath(dir, 1)); err != nil {
		t.Fatal(err)
	}

	// The reader sees the empty directory at its next look, one interval on.
	if waited := waitUntilHeld(t, clock, reader); waited != testInterval+testTTL {
		t.Errorf("the reader took the lock %s after the slots vanished, want %s", waited, testInterval+testTTL)
	}
	if reader.lock.Epoch() != 1 {
		t.Errorf("the epoch is %d, want 1", reader.lock.Epoch())
	}
}

// TestRecordsThatCannotBeRead covers a highest slot whose record is missing or
// damaged. Nothing can be learned from it, so its holder is treated as one that
// may be alive: it is taken over after a TTL, not before, and it is no error.
func TestRecordsThatCannotBeRead(t *testing.T) {
	tests := []struct {
		name, content string
	}{
		{"an empty slot, as it is between the create and the write", ""},
		{"a torn record", `{"version":1,"epoch":7,"holder":"sync-0","time":"2026-11-13T0`},
		{"a torn record of a holder that released", `{"version":1,"epoch":7,"released":true`},
		{"garbage", "\x00\x00\x00\x7fELF"},
		{"a record with no version", `{"epoch":7,"holder":"sync-0","released":true}`},
		{"a record with version zero", `{"version":0,"released":true}`},
		{"something that is not an object", `null`},
		{"a record with a field of the wrong kind", `{"version":1,"epoch":"seven","released":true}`},
		{"a record that names two versions", `{"version":2,"version":"two"}`},
		{"more than a record", `{"version":1,"released":true}` + strings.Repeat(" ", maxRecord)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := lockDir(t)
			writeSlot(t, dir, 7, test.content)

			clock := newFakeClock(epochStart)
			reader := acquireAsync(context.Background(), testConfig(dir, "reader", clock))
			waited := waitUntilHeld(t, clock, reader)

			if waited != testTTL {
				t.Errorf("the reader took over after %s, want %s", waited, testTTL)
			}
			if reader.lock.Epoch() != 8 {
				t.Errorf("the epoch is %d, want 8", reader.lock.Epoch())
			}
		})
	}
}

func TestARecordOfANewerVersionIsNeverTakenOver(t *testing.T) {
	tests := []struct {
		name, content string
	}{
		{"a newer record", `{"version":2,"epoch":7,"holder":"sync-0","released":false}`},
		{"a newer record that looks released", `{"version":2,"released":true}`},
		{"a newer record whose fields changed their kind", `{"version":3,"epoch":"seven","released":"yes","time":7}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := lockDir(t)
			writeSlot(t, dir, 7, test.content)

			_, err := Acquire(soon(t), testConfig(dir, "reader", newFakeClock(epochStart)))

			if !errors.Is(err, ErrNewerVersion) {
				t.Fatalf("got %v, want ErrNewerVersion", err)
			}
			if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{7}) {
				t.Errorf("the slots are %v, want only the newer one", slots)
			}
		})
	}

	t.Run("a field this version does not know is ignored", func(t *testing.T) {
		dir := lockDir(t)
		writeSlot(t, dir, 7, `{"version":1,"epoch":3,"holder":"sync-0","released":true,"zone":"b"}`)

		l := mustAcquire(t, testConfig(dir, "reader", newFakeClock(epochStart)))

		if l.Epoch() != 8 {
			t.Errorf("the epoch is %d, want 8", l.Epoch())
		}
	})
}

func TestRelease(t *testing.T) {
	dir := lockDir(t)
	first := mustAcquire(t, testConfig(dir, "first", newFakeClock(epochStart)))
	if err := first.Check(); err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var rec record
	data, err := os.ReadFile(slotPath(dir, 3))
	if err != nil {
		t.Fatalf("the release made no slot: %v", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil || !rec.Released || rec.Holder != "first" || rec.Epoch != 1 {
		t.Errorf("the record of the release is %s (%v)", data, err)
	}
	// The next agent does not wait: its clock never moves.
	second := mustAcquire(t, testConfig(dir, "second", newFakeClock(epochStart)))
	if second.Epoch() != 4 {
		t.Errorf("the new epoch is %d, want 4", second.Epoch())
	}

	for name, call := range map[string]func() error{"Check": first.Check, "Release": first.Release} {
		if err := call(); !errors.Is(err, ErrReleased) {
			t.Errorf("%s after Release returned %v, want ErrReleased", name, err)
		}
	}
}

func TestEpochsRiseWithEveryTenure(t *testing.T) {
	dir := lockDir(t)
	var last uint64
	for tenure := range 6 {
		clock := newFakeClock(epochStart)
		a := acquireAsync(context.Background(), testConfig(dir, "agent", clock))
		waitUntilHeld(t, clock, a)
		l := a.lock

		if l.Epoch() <= last {
			t.Fatalf("tenure %d has epoch %d, which is not above %d", tenure, l.Epoch(), last)
		}
		last = l.Epoch()
		for range tenure {
			if err := l.Check(); err != nil {
				t.Fatal(err)
			}
		}
		if l.Epoch() != last {
			t.Fatalf("the epoch changed within a tenure, from %d to %d", last, l.Epoch())
		}
		// Every second holder stops cleanly and the others just stop.
		if tenure%2 == 0 {
			if err := l.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCheck(t *testing.T) {
	t.Run("each check makes a slot, and only the tenure's first and the highest are kept", func(t *testing.T) {
		dir := lockDir(t)
		writeSlot(t, dir, 3, `{"version":1,"released":true}`)
		writeSlot(t, dir, 2, "")
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{4}) {
			t.Fatalf("after the takeover the slots are %v, want [4]", slots)
		}

		for i := range 5 {
			if err := l.Check(); err != nil {
				t.Fatalf("check %d: %v", i, err)
			}
			if slots, want := slotsIn(t, dir), []uint64{4, 5 + uint64(i)}; !slices.Equal(slots, want) {
				t.Fatalf("after check %d the slots are %v, want %v", i, slots, want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
			t.Errorf("a file that is not a slot was touched: %v", err)
		}
	})

	t.Run("a slot made by someone else ends the lock for good", func(t *testing.T) {
		dir := lockDir(t)
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))
		writeSlot(t, dir, 2, "")

		wantLost(t, l.Check())

		// Whatever happens to the directory afterwards, a lost lock stays lost.
		if err := os.Remove(slotPath(dir, 2)); err != nil {
			t.Fatal(err)
		}
		wantLost(t, l.Check())
		wantLost(t, l.Release())
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{1}) {
			t.Errorf("the lost lock made slots: %v", slots)
		}
	})

	t.Run("a holder that slept through two tenures finds out from the listing", func(t *testing.T) {
		dir := lockDir(t)
		old := mustAcquire(t, testConfig(dir, "old", newFakeClock(epochStart)))

		clock := newFakeClock(epochStart)
		a := acquireAsync(context.Background(), testConfig(dir, "second", clock))
		waitUntilHeld(t, clock, a)
		if err := a.lock.Release(); err != nil {
			t.Fatal(err)
		}
		third := mustAcquire(t, testConfig(dir, "third", newFakeClock(epochStart)))
		// The slot that replaced the old holder is gone, so its exclusive create
		// succeeds and only the listing can tell it that it lost.
		if _, err := os.Stat(slotPath(dir, old.Epoch()+1)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the slot after the old holder's still exists: %v", err)
		}

		wantLost(t, old.Check())

		if err := third.Check(); err != nil {
			t.Errorf("the holder's check failed: %v", err)
		}
		if slots, want := slotsIn(t, dir), []uint64{third.Epoch(), third.Epoch() + 1}; !slices.Equal(slots, want) {
			t.Errorf("the slots are %v, want %v", slots, want)
		}
	})

	t.Run("a holder that slept through two tenures releases nothing", func(t *testing.T) {
		dir := lockDir(t)
		old := mustAcquire(t, testConfig(dir, "old", newFakeClock(epochStart)))
		clock := newFakeClock(epochStart)
		a := acquireAsync(t.Context(), testConfig(dir, "second", clock))
		waitUntilHeld(t, clock, a)
		if err := a.lock.Release(); err != nil {
			t.Fatal(err)
		}
		third := mustAcquire(t, testConfig(dir, "third", newFakeClock(epochStart)))

		wantLost(t, old.Release())

		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{third.Epoch()}) {
			t.Errorf("the slots are %v: the release of a lost lock must not make one", slots)
		}
		wantLost(t, old.Check())
	})

	t.Run("checks from several goroutines do not get in each other's way", func(t *testing.T) {
		const callers, checks = 8, 20
		dir := lockDir(t)
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))

		failures := make(chan error, callers*checks)
		var group sync.WaitGroup
		for range callers {
			group.Go(func() {
				for range checks {
					if err := l.Check(); err != nil {
						failures <- err
					}
				}
			})
		}
		group.Wait()
		close(failures)

		for err := range failures {
			t.Errorf("a check failed: %v", err)
		}
		if slots, want := slotsIn(t, dir), []uint64{1, 1 + callers*checks}; !slices.Equal(slots, want) {
			t.Errorf("the slots are %v, want %v", slots, want)
		}
	})
}

// TestCheckWhenTheVolumeFails covers a volume that refuses what the lock asks of
// it. The refusals are real: a directory that takes no files, a process that may
// write no data, a directory that cannot be opened.
func TestCheckWhenTheVolumeFails(t *testing.T) {
	t.Run("a failure of the volume is no loss, until it has lasted a TTL", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir := lockDir(t)
		clock := newFakeClock(epochStart)
		l := mustAcquire(t, testConfig(dir, "sync-0", clock))
		refuseWrites(t, dir)

		clock.advance(testTTL - 1)
		err := l.Check()
		if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrLost) {
			t.Fatalf("got %v, want the volume's error and no loss", err)
		}
		allowWrites(t, dir)
		if err := l.Check(); err != nil {
			t.Fatalf("the check after the volume recovered failed: %v", err)
		}
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{1, 2}) {
			t.Fatalf("the slots are %v: the failed check must not have used a number", slots)
		}

		// The TTL counts from the last renewal, not from the start of the tenure.
		refuseWrites(t, dir)
		clock.advance(testTTL - 1)
		if err := l.Check(); !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrLost) {
			t.Fatalf("got %v, want the volume's error and no loss", err)
		}
		clock.advance(1)
		err = l.Check()
		if !errors.Is(err, fs.ErrPermission) || !errors.Is(err, ErrLost) {
			t.Fatalf("got %v, want a loss that names the volume's error", err)
		}
		allowWrites(t, dir)
		wantLost(t, l.Check())
	})

	t.Run("the TTL counts from before the slot was made, not from when the check was done", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir := lockDir(t)
		clock := newFakeClock(epochStart)
		l := mustAcquire(t, testConfig(dir, "sync-0", clock))
		// The volume is slow: an interval passes between any two readings of the
		// clock, so the check that begins now is done two intervals later.
		clock.tick = testInterval
		if err := l.Check(); err != nil {
			t.Fatal(err)
		}
		clock.tick = 0
		if got := clock.Now().Sub(epochStart); got != 2*testInterval {
			t.Fatalf("the check took %s of the clock, want %s", got, 2*testInterval)
		}

		refuseWrites(t, dir)
		if err := l.Check(); !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrLost) {
			t.Fatalf("got %v, want the volume's error and no loss", err)
		}
		// A contender that saw the slot appear may take over a TTL after that.
		clock.advance(testTTL - 2*testInterval)
		wantLost(t, l.Check())
	})

	t.Run("a directory that cannot be synced fails the check", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root opens a directory whatever its mode")
		}
		dir := lockDir(t)
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))
		// Files can be made in the directory, but it cannot be opened.
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { allowWrites(t, dir) })

		err := l.Check()

		if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrLost) ||
			!strings.HasPrefix(err.Error(), "lock: write the record of slot 2: ") {
			t.Fatalf("got %v, want the failure to sync the directory and no loss", err)
		}
		allowWrites(t, dir)
		if err := l.Check(); err != nil {
			t.Fatalf("the check after the volume recovered failed: %v", err)
		}
	})

	t.Run("a release that cannot look at the directory releases nothing", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root opens a directory whatever its mode")
		}
		dir := lockDir(t)
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { allowWrites(t, dir) })

		err := l.Release()

		if !errors.Is(err, fs.ErrPermission) || !strings.HasPrefix(err.Error(), "lock: list the lock directory: ") {
			t.Fatalf("got %v, want the failure to list the directory", err)
		}
		allowWrites(t, dir)
		if err := l.Check(); err != nil {
			t.Fatalf("the lock is not held any more: %v", err)
		}
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{1, 2}) {
			t.Errorf("the slots are %v, want [1 2]", slots)
		}
	})
}

// TestARecordThatCannotBeWritten covers a volume that lets a slot be made and
// then takes no data for it, as a full one does. The slot counts all the same.
func TestARecordThatCannotBeWritten(t *testing.T) {
	t.Run("a slot whose record could not be written is still the holder's", func(t *testing.T) {
		dir := lockDir(t)
		l := mustAcquire(t, testConfig(dir, "sync-0", newFakeClock(epochStart)))

		allow := refuseFileData(t)
		err := l.Check()
		allow()

		if !errors.Is(err, syscall.EFBIG) || errors.Is(err, ErrLost) ||
			!strings.HasPrefix(err.Error(), "lock: write the record of slot 2: ") {
			t.Fatalf("got %v, want the failure to write the record and no loss", err)
		}
		if info, err := os.Stat(slotPath(dir, 2)); err != nil || info.Size() != 0 {
			t.Fatalf("slot 2 is not there and empty: %v", err)
		}
		// The slot was made, so the next check goes on from it.
		if err := l.Check(); err != nil {
			t.Fatalf("the check after the volume recovered failed: %v", err)
		}
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{1, 3}) {
			t.Errorf("the slots are %v, want [1 3]", slots)
		}
	})

	t.Run("a release whose record could not be written still ends the lock", func(t *testing.T) {
		dir := lockDir(t)
		holderClock := newFakeClock(epochStart)
		l := mustAcquire(t, testConfig(dir, "sync-0", holderClock))
		// The holder is late with its heartbeat, but its release makes the slot,
		// so the lock is released and not lost.
		holderClock.advance(testTTL)

		allow := refuseFileData(t)
		err := l.Release()
		allow()

		if !errors.Is(err, syscall.EFBIG) || errors.Is(err, ErrLost) {
			t.Fatalf("got %v, want the failure to write the record and no loss", err)
		}
		if err := l.Check(); !errors.Is(err, ErrReleased) {
			t.Errorf("Check after the release returned %v, want ErrReleased", err)
		}
		// Nobody can read that the lock was released, so the next agent waits.
		clock := newFakeClock(epochStart)
		next := acquireAsync(context.Background(), testConfig(dir, "next", clock))
		if waited := waitUntilHeld(t, clock, next); waited != testTTL {
			t.Errorf("the next agent took over after %s, want %s", waited, testTTL)
		}
	})

	t.Run("a tenure does not begin with a slot whose record could not be written", func(t *testing.T) {
		dir := lockDir(t)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		allow := refuseFileData(t)
		l, err := Acquire(soon(t), testConfig(dir, "sync-0", newFakeClock(epochStart)))
		allow()

		if l != nil || !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("got %v, %v; want no lock and the failure to write the record", l, err)
		}
	})
}

// refuseWrites makes the directory refuse new files, as a volume that turned
// read-only would, until the test ends.
func refuseWrites(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { allowWrites(t, dir) })
}

// refuseFileData makes the kernel refuse to put data into any file of this
// process, the way a full volume does: a file can still be created, but nothing
// can be written to it. The function it returns lifts the limit again.
func refuseFileData(t *testing.T) (allow func()) {
	t.Helper()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	// Without this the signal that comes with the refusal ends the process.
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: limit.Max}); err != nil {
		t.Fatal(err)
	}
	lifted := false
	allow = func() {
		if lifted {
			return
		}
		lifted = true
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
	t.Cleanup(allow)
	return allow
}

func allowWrites(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestClaim(t *testing.T) {
	t.Run("a slot that exists cannot be claimed", func(t *testing.T) {
		dir := lockDir(t)
		writeSlot(t, dir, 5, "")

		l, err := claim(testConfig(dir, "late", newFakeClock(epochStart)), 5)

		if l != nil || err != nil {
			t.Errorf("got %v, %v; want no lock and no error", l, err)
		}
	})

	t.Run("a slot below the highest gives no lock", func(t *testing.T) {
		dir := lockDir(t)
		writeSlot(t, dir, 5, "")

		l, err := claim(testConfig(dir, "late", newFakeClock(epochStart)), 3)

		if l != nil || err != nil {
			t.Errorf("got %v, %v; want no lock and no error", l, err)
		}
		if slots := slotsIn(t, dir); !slices.Equal(slots, []uint64{3, 5}) {
			t.Errorf("the slots are %v: the one who came late must not remove any", slots)
		}
	})

	t.Run("a failure of the volume is returned", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")

		l, err := claim(testConfig(dir, "sync-0", newFakeClock(epochStart)), 1)

		if l != nil || !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "lock: make slot 1: ") {
			t.Errorf("got %v, %v; want the failure to make the slot", l, err)
		}
	})
}

func TestKeep(t *testing.T) {
	// keep runs Keep for a new holder and returns what the test needs to steer it.
	keep := func(t *testing.T, ctx context.Context) (dir string, clock *fakeClock, l *Lock, done chan struct{}, result *error) {
		t.Helper()
		dir = lockDir(t)
		clock = newFakeClock(epochStart)
		l = mustAcquire(t, testConfig(dir, "sync-0", clock))
		done = make(chan struct{})
		result = new(error)
		go func() {
			defer close(done)
			*result = l.Keep(ctx)
		}()
		return dir, clock, l, done, result
	}
	finish := func(t *testing.T, done chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(patience):
			t.Fatal("Keep did not return")
		}
	}

	t.Run("it renews every interval and reports the loss", func(t *testing.T) {
		dir, clock, l, done, result := keep(t, context.Background())

		for i := range 5 {
			if !clock.step(t, done) {
				t.Fatalf("Keep returned %v after %d intervals", *result, i)
			}
			clock.waitParked(t, done)
			if top, want := highest(slotsIn(t, dir)), uint64(2+i); top != want {
				t.Fatalf("after %d intervals the highest slot is %d, want %d", i+1, top, want)
			}
		}

		writeSlot(t, dir, 7, `{"version":1,"epoch":7,"holder":"other","released":false}`)
		if clock.step(t, done) {
			clock.waitParked(t, done)
		}
		finish(t, done)
		wantLost(t, *result)
		wantLost(t, l.Check())
	})

	t.Run("it returns nil when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_, clock, l, done, result := keep(t, ctx)
		clock.waitParked(t, done)

		cancel()

		finish(t, done)
		if *result != nil {
			t.Errorf("Keep returned %v, want nil", *result)
		}
		if err := l.Check(); err != nil {
			t.Errorf("the lock is not held any more: %v", err)
		}
	})

	t.Run("it returns nil when the lock is released", func(t *testing.T) {
		_, clock, l, done, result := keep(t, context.Background())
		clock.waitParked(t, done)
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}

		clock.step(t, done)

		finish(t, done)
		if *result != nil {
			t.Errorf("Keep returned %v, want nil", *result)
		}
	})

	t.Run("it gives the lock up when the volume has refused a TTL long", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory whatever its mode")
		}
		dir, clock, _, done, result := keep(t, context.Background())
		clock.waitParked(t, done)
		refuseWrites(t, dir)

		start := clock.Now()
		for clock.step(t, done) {
		}

		if got := clock.Now().Sub(start); got != testTTL {
			t.Errorf("Keep gave up after %s, want %s", got, testTTL)
		}
		if !errors.Is(*result, ErrLost) || !errors.Is(*result, fs.ErrPermission) {
			t.Errorf("Keep returned %v, want a loss that names the volume's error", *result)
		}
		if !strings.HasPrefix((*result).Error(), "lock: lost to another holder: it could not be renewed for 30s: ") {
			t.Errorf("the error does not say what happened: %v", *result)
		}
	})
}

func TestAcquireStopsWaitingWhenTheContextEnds(t *testing.T) {
	dir := lockDir(t)
	holder := mustAcquire(t, testConfig(dir, "holder", newFakeClock(epochStart)))
	clock := newFakeClock(epochStart)
	ctx, cancel := context.WithCancel(context.Background())
	waiting := acquireAsync(ctx, testConfig(dir, "waiting", clock))
	clock.waitParked(t, waiting.done)

	cancel()

	waiting.wait(t)
	if waiting.lock != nil || !errors.Is(waiting.err, context.Canceled) {
		t.Errorf("got %v, %v; want no lock and the context's error", waiting.lock, waiting.err)
	}
	if err := holder.Check(); err != nil {
		t.Errorf("the holder's check failed: %v", err)
	}
}

func TestSlotNames(t *testing.T) {
	tests := []struct {
		name   string
		number uint64
		isSlot bool
	}{
		{"slot-00000000000000000001", 1, true},
		{"slot-00000000000000000042", 42, true},
		{"slot-18446744073709551615", 18446744073709551615, true},
		{"slot-00000000000000000000", 0, false},
		{"slot-42", 0, false},
		{"slot-000000000000000000042", 0, false},
		{"slot-0000000000000000004x", 0, false},
		{"slot-+0000000000000000042", 0, false},
		{"slot-18446744073709551616", 0, false},
		{"00000000000000000042", 0, false},
		{"slot-", 0, false},
		{"slot-00000000000000000042.tmp", 0, false},
		{"notes.txt", 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			n, ok := slotNumber(test.name)

			if n != test.number || ok != test.isSlot {
				t.Errorf("got %d, %v; want %d, %v", n, ok, test.number, test.isSlot)
			}
			if ok && slotName(n) != test.name {
				t.Errorf("slot %d is named %s", n, slotName(n))
			}
		})
	}
}
