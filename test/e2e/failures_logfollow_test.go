//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"os/exec"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// logFollower keeps the log of the container of an agent that is about to end, by reading it with kubectl logs -f from the start of
// the container until the container ends.
//
// The specs that end the agent (a kill, a freeze that ends in a lost lock, a full volume) check what it said before it ended. They
// cannot read that from the pod afterwards with kubectl logs --previous, because the container is dead then, and the kubelet
// removes dead containers by its own garbage collection, and their logs with them: in one run of CI it removed the killed agent
// less than a second after the restart, and the log came back as "unable to retrieve container logs for containerd://...", which
// kubectl prints as if it were the log and exits with 0. A stream that is open before the container ends has the whole log whatever
// the kubelet does after.
type logFollower struct {
	command *exec.Cmd
	// mu guards text, which the process that kubectl runs in writes to from another goroutine.
	mu   sync.Mutex
	text bytes.Buffer
	// ended is closed when kubectl has exited, which it does when the container ends or the follower is stopped.
	ended chan struct{}
}

// Write keeps what kubectl printed. It is the stdout and the stderr of the process, which is why it is guarded.
func (f *logFollower) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text.Write(p)
}

// followAgentLog starts to keep the log of the container of the agent of the Treebind that runs, from its start. The follower is
// stopped when the spec ends, if the container did not end before.
func followAgentLog(t syncTarget) *logFollower {
	f := &logFollower{ended: make(chan struct{})}
	f.command = exec.Command("kubectl", "logs", "-f", t.syncPodName(), "-n", t.namespace, "-c", failureAgentContainer)
	f.command.Stdout = f
	f.command.Stderr = f
	Expect(f.command.Start()).To(Succeed(), "Failed to follow the log of the agent")
	go func() {
		_ = f.command.Wait()
		close(f.ended)
	}()
	DeferCleanup(f.stop)
	return f
}

// stop ends kubectl if it still runs.
func (f *logFollower) stop() {
	select {
	case <-f.ended:
	default:
		_ = f.command.Process.Kill()
		<-f.ended
	}
}

// logOfEndedContainer waits for the container to end, which ends the stream, and returns the whole of its log.
func (f *logFollower) logOfEndedContainer(g Gomega) string {
	select {
	case <-f.ended:
	case <-time.After(logFollowerGrace):
		g.Expect(false).To(BeTrue(), "the stream of the log of the agent did not end within %s of the container ending", logFollowerGrace)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text.String()
}

// logFollowerGrace is how long the stream may take to end after the container did: it ends with the container, and the wait is for a
// kubectl that is slow to notice.
const logFollowerGrace = 2 * time.Minute
