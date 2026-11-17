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
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

// The pulls that do not pass: each reason of a pull, and an error that is not a pull's.
var unsuccessfulPulls = []struct {
	name   string
	answer answer
}{
	{pull.ReasonBlocked, failedPull(pull.ReasonBlocked, "a file has a local edit")},
	{pull.ReasonDiverged, failedPull(pull.ReasonDiverged, "the checkout and the branch went different ways")},
	{pull.ReasonBranchGone, failedPull(pull.ReasonBranchGone, "the repository has no such branch")},
	{pull.ReasonReservedPath, failedPull(pull.ReasonReservedPath, "the branch has a file where the state is")},
	{pull.ReasonGitDirUnsafe, failedPull(pull.ReasonGitDirUnsafe, "the Git directory holds more than the agent put there")},
	{pull.ReasonTooManyDeletions, failedPull(pull.ReasonTooManyDeletions, "the update removes too many files")},
	{pull.ReasonFetchFailed, failedPull(pull.ReasonFetchFailed, noRoute)},
	{pull.ReasonPullFailed, failedPull(pull.ReasonPullFailed, "the volume is read only")},
	{pull.ReasonRefused, failedPull(pull.ReasonRefused, "the lock cannot be checked")},
	{"an error that is not a pull's", answer{err: errors.New("something else")}},
}

// readyz is the answer of the readiness probe.
func (r *running) readyz(t *testing.T) int {
	t.Helper()
	code, body := r.get(t, status.ReadyPath)
	if code == http.StatusOK && body != "ready\n" {
		t.Errorf("readyz says %q with 200", body)
	}
	return code
}

func TestIsReadyOnceAPullHasPassedWhateverItDid(t *testing.T) {
	for name, passes := range map[string]answer{
		"at the tip":       upToDate(testCommit),
		"advanced":         advanced(otherCommit, testCommit),
		"ahead of the tip": {result: pull.Result{Outcome: pull.Ahead, Branch: testBranch, From: testCommit, To: testCommit}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePuller()
			change, _ := withPuller(f, &loopClock{})
			r := start(t, change)
			f.awaitBegun(t)

			if code := r.readyz(t); code != http.StatusServiceUnavailable {
				t.Errorf("readyz is %d while the first pull runs, want 503", code)
			}
			f.answer(t, passes)

			if code := r.readyz(t); code != http.StatusOK {
				t.Errorf("readyz is %d after a pull that passed, want 200", code)
			}
			if got, _ := pullCheck(t, r); got.Outcome != outcomePassed {
				t.Errorf("the Pull check is %+v when the agent is ready, want it passed", got)
			}
			if n := r.logs.count("the first pull passed, and the agent reports itself ready"); n != 1 {
				t.Errorf("the readiness was logged %d times, want once:\n%s", n, r.logs.String())
			}
		})
	}
}

func TestIsNotReadyUntilAPullHasPassedAndKeepsLookingAtEachPull(t *testing.T) {
	for _, tt := range unsuccessfulPulls {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePuller()
			change, _ := withPuller(f, &loopClock{})
			r := start(t, change)
			f.awaitBegun(t)

			f.answer(t, tt.answer)

			if code := r.readyz(t); code != http.StatusServiceUnavailable {
				t.Errorf("readyz is %d after a first pull that did not pass, want 503", code)
			}
			if got, _ := pullCheck(t, r); got.Outcome != outcomeFailed {
				t.Errorf("the Pull check is %+v, want it failed", got)
			}

			f.answer(t, upToDate(testCommit))

			if code := r.readyz(t); code != http.StatusOK {
				t.Errorf("readyz is %d once a pull has passed, want 200", code)
			}
		})
	}
}

func TestStaysReadyWhenALaterPullDoesNotPassAndTheReportSaysSo(t *testing.T) {
	for _, tt := range unsuccessfulPulls {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePuller()
			change, _ := withPuller(f, &loopClock{})
			r := start(t, change)
			f.awaitBegun(t)
			f.answer(t, upToDate(testCommit))

			f.answer(t, tt.answer)
			f.answer(t, tt.answer)

			if code := r.readyz(t); code != http.StatusOK {
				t.Errorf("readyz is %d after a pull that failed, want 200: the volume is as it was", code)
			}
			// The failure is not hidden by the readiness: it is in the check that the operator reads.
			if got, _ := pullCheck(t, r); got.Outcome != outcomeFailed {
				t.Errorf("the Pull check is %+v, want the failure", got)
			}

			f.answer(t, upToDate(testCommit))

			if code := r.readyz(t); code != http.StatusOK {
				t.Errorf("readyz is %d after the pull passed again, want 200", code)
			}
			if n := r.logs.count("the agent reports itself ready"); n != 1 {
				t.Errorf("the readiness was logged %d times, want once: it does not change", n)
			}
		})
	}
}

func TestIsNotReadyWhileTheFirstPullIsStillToBeDue(t *testing.T) {
	f := newFakePuller()
	// The wait for the first pull never ends, so the checkout is made and nothing is pulled.
	change, _ := withPuller(f, (&loopClock{}).stallAfter(0))
	r := start(t, change)

	eventually(t, "the checkout", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomePassed
	})

	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d after the checkout and before the first pull, want 503", code)
	}
	if code, _ := r.get(t, status.HealthPath); code != http.StatusOK {
		t.Errorf("healthz is %d", code)
	}
}

func TestIsNotReadyAfterACheckoutThatCouldNotBeMade(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	r := start(t, func(o *Options) {
		change(o)
		o.Checkout = func(context.Context, checkout.Options) (checkout.Result, error) {
			return checkout.Result{}, errors.New("the repository cannot be reached")
		}
	})

	eventually(t, "the checkout to fail", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d, want 503", code)
	}
}

// A repository with nothing in it has no branch, and the pull says so: it is the agent that cannot tell an
// empty repository from a branch that was named wrong. The first commit that the repository gets is what a pull
// finds, and the agent is ready after that pull. A later outage of the repository leaves it ready.
func TestARealRepositoryMakesTheAgentReadyAtItsFirstTipAndAnOutageDoesNotUndoIt(t *testing.T) {
	f := gittest.New(t)
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
			return pull.New(options)
		}
	})
	pulled := func(wait gate) gate {
		t.Helper()
		wait.pass()
		return clock.wait(t)
	}

	// No commit yet: the first pull finds no branch.
	wait := clock.wait(t)
	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d before any pull, want 503", code)
	}
	wait = pulled(wait)
	if got, _ := pullCheck(t, r); got.Outcome != outcomeFailed || got.Reason != pull.ReasonBranchGone {
		t.Errorf("pull = %+v, want BranchGone for a repository with no commit", got)
	}
	if code := r.readyz(t); code != http.StatusServiceUnavailable {
		t.Errorf("readyz is %d for a repository with no commit, want 503", code)
	}

	// The first commit comes, and the next pull brings it into the directory.
	f.Publish(testBranch, map[string]string{jsonA: "a\n"})
	wait = pulled(wait)
	if got, _ := pullCheck(t, r); got.Outcome != outcomePassed || !strings.Contains(got.Message, "advanced the checkout") {
		t.Errorf("pull = %+v, want it advanced", got)
	}
	if gittest.Read(t, where, jsonA) != "a\n" {
		t.Error("the commit is not in the directory")
	}
	if code := r.readyz(t); code != http.StatusOK {
		t.Errorf("readyz is %d after the first pull that passed, want 200", code)
	}

	// The repository goes away, and the directory is as it was.
	away := f.Bare + ".away"
	if err := os.Rename(f.Bare, away); err != nil {
		t.Fatal(err)
	}
	wait = pulled(wait)
	if got, _ := pullCheck(t, r); got.Outcome != outcomeFailed || got.Reason != pull.ReasonFetchFailed {
		t.Errorf("pull = %+v, want FetchFailed while the repository is away", got)
	}
	if code := r.readyz(t); code != http.StatusOK {
		t.Errorf("readyz is %d during an outage of the repository, want 200", code)
	}
	if err := os.Rename(away, f.Bare); err != nil {
		t.Fatal(err)
	}
	pulled(wait)
	if got, _ := pullCheck(t, r); got.Outcome != outcomePassed {
		t.Errorf("pull = %+v, want it to pass when the repository is back", got)
	}
	if code := r.readyz(t); code != http.StatusOK {
		t.Errorf("readyz is %d, want 200", code)
	}
}

// Both directions only pull for now, and the scans of a bidirectional Treebind are turns of the same loop: what
// they find, or that they fail, is not what the agent is ready for.
func TestABidirectionalAgentIsReadyAfterItsFirstPullWhateverTheScansSay(t *testing.T) {
	w := newWorld()
	for range 50 {
		w.scanner.answers <- scanAnswer{err: errors.New("git status failed")}
	}
	r := start(t, w.with(func(o *Options) { o.Config.SyncInterval = 4 * time.Second }))

	eventually(t, "the agent to be ready", func() bool {
		code, _ := r.get(t, status.ReadyPath)
		return code == http.StatusOK
	})

	if got, found := pullCheck(t, r); !found || got.Outcome != outcomePassed {
		t.Errorf("the Pull check is %+v when the agent is ready, want it passed", got)
	}
	if got, found := changesCheck(t, r); !found || got.Outcome != outcomeFailed {
		t.Errorf("the Changes check is %+v, want the failed scans, which do not stop the agent from being ready", got)
	}
}

// The report says that the pull passed before the probe says that the agent is ready. The agent reads its clock for
// the time of the report just before it stores it, so the probe is asked there: the report is not stored yet, and
// the agent has to be not ready.
func TestTheReportHoldsTheFirstPullBeforeTheProbeSaysReady(t *testing.T) {
	f := newFakePuller()
	change, _ := withPuller(f, &loopClock{})
	var (
		probing atomic.Bool
		url     atomic.Pointer[string]
		mu      sync.Mutex
		codes   []int
	)
	r := start(t, func(o *Options) {
		change(o)
		o.Now = func() time.Time {
			if base := url.Load(); base != nil && probing.Load() {
				response, err := http.Get(*base + status.ReadyPath)
				code := -1
				if err == nil {
					code = response.StatusCode
					_ = response.Body.Close()
				}
				mu.Lock()
				codes = append(codes, code)
				mu.Unlock()
			}
			return time.Now()
		}
	})
	base := r.url
	url.Store(&base)
	f.awaitBegun(t)

	probing.Store(true)
	f.answer(t, upToDate(testCommit))
	probing.Store(false)

	mu.Lock()
	defer mu.Unlock()
	if len(codes) == 0 {
		t.Fatal("the report was not stored while the test was asking: nothing was held")
	}
	for _, code := range codes {
		if code != http.StatusServiceUnavailable {
			t.Errorf("readyz was %d while the report of the pull was being made, want 503: the probe went first", code)
		}
	}
	if code := r.readyz(t); code != http.StatusOK {
		t.Errorf("readyz is %d after the pull, want 200", code)
	}
}
