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

package git

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// runningLog is what the hook Options.Running was told, in order: a command that starts, with the longest it
// may take, and a command that has ended.
type runningLog struct {
	mu     sync.Mutex
	events []string
}

func (l *runningLog) running(timeout time.Duration) func() {
	l.add(fmt.Sprintf("start %s", timeout))
	return func() { l.add("end") }
}

func (l *runningLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *runningLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (l *runningLog) want(t *testing.T, want ...string) {
	t.Helper()
	if got := l.snapshot(); !slices.Equal(got, want) {
		t.Errorf("the hook was told %q, want %q", got, want)
	}
}

func TestRunningIsToldWhenARealCommandStartsAndEndsWithTheRunnersTimeout(t *testing.T) {
	r := newRepo(t, outsideWork)
	log := &runningLog{}
	runner := r.runner(func(o *Options) { o.Timeout = 90 * time.Second; o.Running = log.running })

	if _, err := r.run(runner, cmdRevParse, refHead); err != nil {
		t.Fatal(err)
	}

	log.want(t, "start 1m30s", "end")
}

func TestRunningIsToldTheTimeoutOfTheCallAndTheDefaultWhenNeitherSaysOne(t *testing.T) {
	r := newRepo(t, outsideWork)
	log := &runningLog{}
	runner := r.runner(func(o *Options) { o.Timeout = 0; o.Running = log.running })

	if _, err := r.run(runner, cmdRevParse, refHead); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTimeout(context.Background(), 7*time.Second, cmdRevParse, refHead); err != nil {
		t.Fatal(err)
	}

	log.want(t, "start "+DefaultTimeout.String(), "end", "start 7s", "end")
}

func TestRunningIsToldWhenACommandFailsToo(t *testing.T) {
	r := newRepo(t, outsideWork)
	log := &runningLog{}
	runner := r.runner(func(o *Options) { o.Running = log.running })

	if _, err := r.run(runner, cmdRevParse, "--verify", "refs/heads/does-not-exist"); err == nil {
		t.Fatal("a command for a branch that is not there did not fail")
	}

	log.want(t, "start 1m0s", "end")
}

func TestRunningIsToldWhenACommandIsStoppedAtItsTimeoutAndTheEndIsBeforeTheCallReturns(t *testing.T) {
	log := &runningLog{}
	r := fakeGit(t, tempDir(t), "while :; do :; done", func(o *Options) { o.Running = log.running })

	_, err := r.RunTimeout(context.Background(), 200*time.Millisecond, cmdStatus)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want a timeout", err)
	}

	// The end is told once the command has been stopped, and not when the timeout came: until then it was
	// running, and the promise that was made with it has not been kept or broken yet.
	log.want(t, "start 200ms", "end")
}

func TestRunningIsToldBeforeTheGitDirectoryIsLookedAt(t *testing.T) {
	// A volume that does not answer holds the look at the Git directory, and the beat that says how long the
	// command may take is what a watcher of the agent holds that look to.
	r := agentRepo(t)
	r.write("new.txt", "new\n")
	writeInGitDir(t, r, "info/attributes", "* filter=x\n")
	log := &runningLog{}
	runner := r.runner(func(o *Options) { o.VerifyGitDir = true; o.Running = log.running })

	_, err := r.run(runner, cmdAdd, "new.txt")

	if !errors.Is(err, ErrGitDirUntrusted) {
		t.Fatalf("got %v, want the Git directory refused", err)
	}
	log.want(t, "start 1m0s", "end")
}

func TestRunningIsNotToldOfACommandThatIsNotRunBecauseItIsNotAllowed(t *testing.T) {
	r := newRepo(t, outsideWork)
	log := &runningLog{}
	runner := r.runner(func(o *Options) { o.Running = log.running })

	if _, err := r.run(runner, "gc"); err == nil {
		t.Fatal("a command that is not on the list was run")
	}

	log.want(t)
}

func TestARunnerWorksWhenTheHookReturnsNoEnd(t *testing.T) {
	r := newRepo(t, outsideWork)
	runner := r.runner(func(o *Options) { o.Running = func(time.Duration) func() { return nil } })
	if _, err := r.run(runner, cmdRevParse, refHead); err != nil {
		t.Fatal(err)
	}
}
