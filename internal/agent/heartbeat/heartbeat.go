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

// Package heartbeat tells whether the loop of the agent is stuck (docs/adr/0030). The loop beats, and
// each beat says how long it is until the next. The agent is alive until that time and a grace have
// passed, and the liveness probe answers from that, so the kubelet restarts a pod whose loop has gone
// quiet for longer than it said it would.
//
// A single deadline for the whole loop cannot work. The interval of a Treebind has no maximum, a pull
// that failed waits up to ten minutes, a fetch of a large repository is allowed half an hour, and the
// agent waits for the writer lock for as long as another agent holds it, which is not a fault that a
// restart puts right. So a beat carries its own promise: before a wait, the length of the wait; during
// a Git command, the timeout of that command; and in the code between them, Work.
//
// The package knows nothing of Git, of the loop or of HTTP, and takes its time from a function, so that
// a test moves it by hand.
package heartbeat

import (
	"sync"
	"time"
)

const (
	// Work is how long the code between two waits or two Git commands may take, which is the promise of a
	// beat that comes from the code and not from a wait or a command. It reads and writes the volume a
	// few times, and a volume that is slow takes minutes and a volume that does not answer takes for ever,
	// which is what a restart is for.
	Work = 5 * time.Minute

	// Grace is how long a promise is kept after it ran out. A timer that fires late, a process that was not
	// scheduled for a while and a Git command that is being stopped after its timeout (SIGTERM, then
	// SIGKILL after three seconds, and ten seconds of drain for an update that is being applied) all take
	// less than that. With a probe that runs every ten seconds and fails after three misses, the restart
	// comes twenty to thirty seconds after the grace.
	Grace = 30 * time.Second
)

// Monitor is the beats of one loop, and says whether it is late. It is safe for concurrent use: the loop
// beats, and the requests of the kubelet ask.
type Monitor struct {
	now func() time.Time

	mu       sync.Mutex
	due      time.Time
	finished bool
}

// New makes a monitor that takes the time from now, which is the system's time when it is nil. It begins
// with a promise of Work, for the start of the agent.
func New(now func() time.Time) *Monitor {
	if now == nil {
		now = time.Now
	}
	m := &Monitor{now: now}
	m.Beat(Work)
	return m
}

// Beat says that the next beat comes within promise from now. It replaces the promise before it, long or
// short: a beat is how long until the next one, and after a wait the code that follows has its own, shorter
// promise, which is the one to hold it to. A promise that is not positive is no time at all.
func (m *Monitor) Beat(promise time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.due = m.now().Add(max(promise, 0))
}

// Finish says that the loop has nothing more to do and has not stopped: a Treebind whose settings no wait
// puts right, or one that has no pulls. Such an agent is idle, and a restart would only bring it back to
// the same place, so it is alive for good.
func (m *Monitor) Finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished = true
}

// Alive says whether the loop is within its promise and the grace.
func (m *Monitor) Alive() bool { return m.Late() == 0 }

// Late is how long after the promise and the grace it is, or zero while the loop is within them.
func (m *Monitor) Late() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished {
		return 0
	}
	return max(m.now().Sub(m.due.Add(Grace)), 0)
}
