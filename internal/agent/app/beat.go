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
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/heartbeat"
)

// stuckLogEvery is how often at most a late loop is logged. The kubelet asks every ten seconds.
const stuckLogEvery = time.Minute

// The loop of the agent says how long it is until it is heard from again, and /healthz answers from that
// (docs/adr/0030). It says it at every place where it can spend a long time:
//
//   - before a wait, the length of the wait (beat);
//   - during a Git command, the timeout of the command, which the Runner says through running;
//   - in the code between the waits and the commands, the time that code may take (working).
//
// The loop is the only one that beats. The keeper of the writer lock does not: a keeper that cannot write
// to the volume loses the lock in thirty seconds and ends the agent, which is not what a probe is for.

// beat says that the loop is heard from again within promise, which is the length of the wait that it is
// about to make.
func (a *agent) beat(promise time.Duration) { a.beats.Beat(promise) }

// working says that the code of the loop runs, between two waits or two Git commands, and is heard from again
// within the time that such code may take.
func (a *agent) working() { a.beats.Beat(heartbeat.Work) }

// running is the hook of the Runner of Git (git.Options.Running): a command begins and may take its timeout,
// and when it has ended the code that follows has its own promise.
func (a *agent) running(timeout time.Duration) func() {
	a.beat(timeout)
	return a.working
}

// alive is the answer of the liveness probe: the loop is within what it promised, and the grace. A loop
// that is late is logged, so that the person who finds the pod restarted finds out why.
func (a *agent) alive() bool {
	late := a.beats.Late()
	if late == 0 {
		return true
	}
	if suppressed, ok := a.stuck.allow(); ok {
		a.Logger.Warn("the loop of the agent did not beat within what it promised, and the liveness probe fails",
			"late", late.Round(time.Second).String(), "alsoFailedSince", suppressed)
	}
	return false
}
