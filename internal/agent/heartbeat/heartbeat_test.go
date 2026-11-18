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

package heartbeat

import (
	"sync"
	"testing"
	"time"
)

// clock is a clock that a test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestTheNumbersAreTheOnesTheDocumentationGives(t *testing.T) {
	if Work != 5*time.Minute {
		t.Errorf("Work = %v, want 5m: the time that the code between two waits or Git commands may take", Work)
	}
	if Grace != 30*time.Second {
		t.Errorf("Grace = %v, want 30s", Grace)
	}
}

func TestAMonitorThatIsNewPromisesWhatTheStartOfTheAgentNeeds(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	if !m.Alive() {
		t.Fatal("a new monitor is not alive: the agent would be restarted before it began")
	}
	c.advance(Work + Grace)
	if !m.Alive() {
		t.Errorf("not alive after %v, want alive until a promise of %v and the grace have passed", Work+Grace, Work)
	}
	c.advance(time.Nanosecond)
	if m.Alive() {
		t.Errorf("alive after %v and a nanosecond, want it not alive: nothing beat", Work+Grace)
	}
}

func TestABeatIsKeptUntilItsPromiseAndTheGraceHavePassed(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	m.Beat(2 * time.Minute)

	c.advance(2*time.Minute + Grace)
	if !m.Alive() || m.Late() != 0 {
		t.Errorf("at the end of the grace: Alive = %v, Late = %v, want alive and not late", m.Alive(), m.Late())
	}
	c.advance(time.Nanosecond)
	if m.Alive() {
		t.Error("a nanosecond after the grace the monitor is still alive")
	}
	if got := m.Late(); got != time.Nanosecond {
		t.Errorf("Late = %v, want 1ns, which is how long after the grace it is", got)
	}
	c.advance(time.Minute)
	if got, want := m.Late(), time.Minute+time.Nanosecond; got != want {
		t.Errorf("Late = %v, want %v", got, want)
	}
}

func TestABeatThatComesLateRevivesTheMonitor(t *testing.T) {
	// The kubelet counts misses, and a loop that was slow and went on is not stuck.
	c := newClock()
	m := New(c.Now)
	m.Beat(time.Second)
	c.advance(time.Hour)
	if m.Alive() {
		t.Fatal("alive an hour after a promise of a second")
	}
	m.Beat(time.Second)
	if !m.Alive() || m.Late() != 0 {
		t.Errorf("after a new beat: Alive = %v, Late = %v, want alive", m.Alive(), m.Late())
	}
}

func TestANewBeatReplacesThePromiseBeforeItWhateverItsLength(t *testing.T) {
	// A promise is how long until the next beat. After a long wait, the code that follows it promises
	// less, and a stuck loop has to be found by the short promise and not the long one before it.
	c := newClock()
	m := New(c.Now)
	m.Beat(24 * time.Hour)
	m.Beat(time.Second)
	c.advance(time.Second + Grace)
	if !m.Alive() {
		t.Fatal("not alive at the end of the new promise and the grace")
	}
	c.advance(time.Nanosecond)
	if m.Alive() {
		t.Error("still alive after the shorter promise: the longer one before it was kept")
	}
}

func TestAPromiseThatIsNotPositiveIsNoTime(t *testing.T) {
	for _, promise := range []time.Duration{0, -time.Hour} {
		c := newClock()
		m := New(c.Now)
		m.Beat(promise)
		c.advance(Grace)
		if !m.Alive() {
			t.Errorf("Beat(%v): not alive at the end of the grace", promise)
		}
		c.advance(time.Nanosecond)
		if m.Alive() {
			t.Errorf("Beat(%v): alive after the grace, want a promise of no time to give none", promise)
		}
	}
}

func TestAMonitorThatIsFinishedIsAliveForGood(t *testing.T) {
	// A loop that has nothing more to do, and has not stopped, is idle and not stuck.
	c := newClock()
	m := New(c.Now)
	m.Beat(time.Second)
	m.Finish()
	c.advance(1000 * time.Hour)
	if !m.Alive() || m.Late() != 0 {
		t.Errorf("after Finish: Alive = %v, Late = %v, want alive for good", m.Alive(), m.Late())
	}
	m.Beat(time.Second)
	c.advance(time.Hour)
	if !m.Alive() {
		t.Error("a beat after Finish made the monitor expire again")
	}
}

func TestAMonitorWithoutAClockUsesTheSystemClock(t *testing.T) {
	m := New(nil)
	if !m.Alive() {
		t.Error("a new monitor on the system clock is not alive")
	}
	m.Beat(time.Hour)
	if !m.Alive() {
		t.Error("a beat of an hour on the system clock is not alive")
	}
}

func TestMonitorIsSafeForConcurrentUse(t *testing.T) {
	c := newClock()
	m := New(c.Now)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				m.Beat(time.Second)
				_ = m.Alive()
				_ = m.Late()
				c.advance(time.Millisecond)
			}
		})
	}
	wg.Go(func() {
		for range 500 {
			_ = m.Alive()
		}
		m.Finish()
	})
	wg.Wait()
}
