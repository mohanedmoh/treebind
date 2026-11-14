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
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testBranch = "main"

	// The outcomes of a check, as the report says them.
	outcomePassed  = "Passed"
	outcomeFailed  = "Failed"
	outcomeSkipped = "Skipped"
)

// withRepository gives the agent a repository to sync with, and the checkout the test
// replaces.
func withRepository(open func(context.Context, checkout.Options) (checkout.Result, error)) func(*Options) {
	return func(o *Options) {
		o.Config.RepoURL = "ssh://git@example.com/acme/dashboards.git"
		o.Config.RepoBranch = "release"
		o.Config.SubPath = "dashboards"
		o.Config.SSHKeyFile = "/etc/treebind/ssh/id_ed25519"
		o.Config.KnownHostsFile = "/etc/treebind/known-hosts/known_hosts"
		o.Checkout = open
	}
}

func cloned(context.Context, checkout.Options) (checkout.Result, error) {
	return checkout.Result{Outcome: checkout.Cloned, Branch: testBranch, Commit: testCommit}, nil
}

func checkoutCheck(t *testing.T, r *running) (check struct{ Outcome, Reason, Message string }, found bool) {
	t.Helper()
	for _, c := range r.report(t).Checks {
		if c.Name == CheckCheckout {
			return struct{ Outcome, Reason, Message string }{c.Outcome, c.Reason, c.Message}, true
		}
	}
	return check, false
}

func TestMakesTheCheckoutOnceTheVolumeIsInOrderAndReportsIt(t *testing.T) {
	asked := make(chan checkout.Options, 4)
	r := start(t, withRepository(func(_ context.Context, o checkout.Options) (checkout.Result, error) {
		asked <- o
		return cloned(context.Background(), o)
	}))

	eventually(t, "the checkout in the report", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomePassed
	})

	got, _ := checkoutCheck(t, r)
	if got.Reason != "" || !strings.Contains(got.Message, "cloned the branch main (0123456789ab) into /work/dashboards") {
		t.Errorf("checkout = %+v, want it cloned, with the branch, a short commit and the directory", got)
	}
	names := make([]string, 0, 3)
	for _, c := range r.report(t).Checks {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "Mount,Writable,Checkout" {
		t.Errorf("checks = %v, want the volume checks and then the checkout", names)
	}
	options := <-asked
	if options.URL != "ssh://git@example.com/acme/dashboards.git" || options.Branch != "release" ||
		options.Layout.WorkTree != "/work/dashboards" || options.Layout.GitDir != "/work/.treebind/git" ||
		options.Git.SSH.IdentityFile != "/etc/treebind/ssh/id_ed25519" ||
		options.Git.SSH.KnownHostsFile != "/etc/treebind/known-hosts/known_hosts" {
		t.Errorf("options = %+v, want what the settings of the pod say", options)
	}
	if code, _ := r.get(t, "/readyz"); code == 200 {
		t.Error("the agent is ready after a checkout: it has not synced")
	}
}

func TestMakesNoCheckoutWhenNoneIsGiven(t *testing.T) {
	r := start(t, func(o *Options) { o.Config.RepoURL = "https://example.com/a.git" })

	eventually(t, "the volume checks in the report", func() bool { return len(r.report(t).Checks) == 2 })
	if _, found := checkoutCheck(t, r); found {
		t.Error("a checkout is reported, and none was asked for")
	}
}

func TestDoesNotTryTheCheckoutWhileAVolumeCheckFails(t *testing.T) {
	var volume, opened atomic.Int64
	r := start(t, func(o *Options) {
		withRepository(func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return cloned(ctx, options)
		})(o)
		o.Preflight = func(context.Context, string, string) []preflight.Result {
			if volume.Add(1) <= 3 {
				return failed
			}
			return passed
		}
	})

	eventually(t, "the checkout to be skipped", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomeSkipped
	})
	got, _ := checkoutCheck(t, r)
	if got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "volume check failed") {
		t.Errorf("checkout = %+v, want it skipped with the reason that says why", got)
	}
	if opened.Load() != 0 {
		t.Errorf("the checkout was tried %d times while a volume check failed", opened.Load())
	}
	eventually(t, "the checkout to be made once the volume is in order", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
}

func TestKeepsTryingTheCheckoutWhileItFailsAndStopsOnceItWorks(t *testing.T) {
	var calls atomic.Int64
	r := start(t, withRepository(func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		if calls.Add(1) <= 3 {
			return checkout.Result{}, &checkout.Error{Reason: checkout.ReasonFetchFailed, Message: "the repository cannot be asked for its branches"}
		}
		return cloned(ctx, o)
	}))

	eventually(t, "the failure in the report", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})
	if got, _ := checkoutCheck(t, r); got.Reason != "FetchFailed" || !strings.Contains(got.Message, "cannot be asked") {
		t.Errorf("checkout = %+v, want the reason and the message of the failure", got)
	}
	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
	settled := calls.Load()
	// Nothing else is going to happen, so a short wait is all there is to see.
	eventually(t, "the loop to have ended", func() bool { return calls.Load() == settled })
	if settled != 4 {
		t.Errorf("the checkout ran %d times, want 4: three failures and a success", settled)
	}
}

func TestSaysAFailureOfTheCheckoutWithItsOwnReasonAndAPlainOneWithTheGeneralReason(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"a refusal", &checkout.Error{Reason: checkout.ReasonDirectoryNotEmpty, Message: "the directory is not empty"}, "DirectoryNotEmpty"},
		{"a refusal that wraps another error", &checkout.Error{Reason: checkout.ReasonRepositoryMismatch, Message: "mismatch", Err: errors.New("x")}, "RepositoryMismatch"},
		{"a plain error", errors.New("the URL of the repository is not set"), checkout.ReasonCheckoutFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := start(t, withRepository(func(context.Context, checkout.Options) (checkout.Result, error) {
				return checkout.Result{}, tt.err
			}))

			eventually(t, "the failure in the report", func() bool {
				got, found := checkoutCheck(t, r)
				return found && got.Outcome == outcomeFailed
			})

			got, _ := checkoutCheck(t, r)
			if got.Reason != tt.wantReason || got.Message != tt.err.Error() {
				t.Errorf("checkout = %+v, want the reason %s and the message %q", got, tt.wantReason, tt.err.Error())
			}
		})
	}
}

func TestSaysWhatKindOfCheckoutItMade(t *testing.T) {
	tests := []struct {
		name   string
		result checkout.Result
		want   string
	}{
		{"adopted", checkout.Result{Outcome: checkout.Adopted, Branch: testBranch, Commit: testCommit},
			"/work/dashboards holds exactly the files of the branch main (0123456789ab) and is the checkout now"},
		{"reopened", checkout.Result{Outcome: checkout.Reopened, Branch: testBranch, Commit: testCommit},
			"reopened the checkout of the branch main (0123456789ab) in /work/dashboards"},
		{"cloned into a repository with no commits", checkout.Result{Outcome: checkout.Cloned, Branch: testBranch},
			"cloned the branch main (no commit yet) into /work/dashboards"},
		{"a commit shorter than the cut", checkout.Result{Outcome: checkout.Reopened, Branch: testBranch, Commit: "abc"},
			"reopened the checkout of the branch main (abc) in /work/dashboards"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := start(t, withRepository(func(context.Context, checkout.Options) (checkout.Result, error) { return tt.result, nil }))

			eventually(t, "the checkout in the report", func() bool {
				got, _ := checkoutCheck(t, r)
				return got.Outcome == outcomePassed
			})

			if got, _ := checkoutCheck(t, r); got.Message != tt.want {
				t.Errorf("message = %q, want %q", got.Message, tt.want)
			}
		})
	}
}

func TestReportsACheckoutThatCannotBeSetUp(t *testing.T) {
	var opened atomic.Int64
	r := start(t, func(o *Options) {
		withRepository(func(ctx context.Context, options checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return cloned(ctx, options)
		})(o)
		// A mount path that the config would have refused: the layout cannot be worked out.
		o.Config = config.Config{Namespace: testNS, Name: testName, MountPath: "work", RepoURL: o.Config.RepoURL}
	})

	eventually(t, "the failure in the report", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomeFailed
	})

	got, _ := checkoutCheck(t, r)
	if got.Reason != checkout.ReasonCheckoutFailed || !strings.Contains(got.Message, "mount path") {
		t.Errorf("checkout = %+v, want a failure that says what is wrong with the mount path", got)
	}
	if opened.Load() != 0 {
		t.Error("the checkout was run with a layout that could not be worked out")
	}
}

func TestLogsTheCheckoutWhenItChanges(t *testing.T) {
	var calls atomic.Int64
	r := start(t, withRepository(func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		if calls.Add(1) <= 3 {
			return checkout.Result{}, &checkout.Error{Reason: checkout.ReasonFetchFailed, Message: "the host cannot be reached"}
		}
		return cloned(ctx, o)
	}))

	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})

	if n := r.logs.count(`msg=checkout check=Checkout outcome=Failed reason=FetchFailed`); n != 1 {
		t.Errorf("the failure was logged %d times, want once: it did not change\n%s", n, r.logs.String())
	}
	if n := r.logs.count(`msg=checkout check=Checkout outcome=Passed`); n != 1 {
		t.Errorf("the success was logged %d times, want once\n%s", n, r.logs.String())
	}
}

func TestStopsWhileTheCheckoutIsRunning(t *testing.T) {
	started := make(chan struct{})
	var stoppedByContext atomic.Bool
	r := start(t, withRepository(func(ctx context.Context, _ checkout.Options) (checkout.Result, error) {
		close(started)
		<-ctx.Done()
		stoppedByContext.Store(true)
		return checkout.Result{}, ctx.Err()
	}))
	<-started

	r.cancel()

	if err := r.waitStopped(t); err != nil {
		t.Errorf("Run returned %v, want it to stop cleanly", err)
	}
	if !stoppedByContext.Load() {
		t.Error("the checkout was not told to stop: a clone of a large repository would hold the pod for its whole period")
	}
	if n := r.logs.count("outcome=Failed"); n != 0 {
		t.Errorf("a checkout that was stopped is logged as a failure\n%s", r.logs.String())
	}
}

func TestMakesNoCheckoutWhenToldToStopWhileAVolumeCheckFails(t *testing.T) {
	var opened atomic.Int64
	asked := make(chan struct{}, 1)
	r := start(t, func(o *Options) {
		withRepository(func(context.Context, checkout.Options) (checkout.Result, error) {
			opened.Add(1)
			return checkout.Result{}, errors.New("the checkout was run")
		})(o)
		o.Preflight = func(context.Context, string, string) []preflight.Result {
			select {
			case asked <- struct{}{}:
			default:
			}
			return failed
		}
	})
	<-asked

	r.cancel()

	if err := r.waitStopped(t); err != nil {
		t.Fatal(err)
	}
	if opened.Load() != 0 {
		t.Errorf("the checkout was run %d times by an agent that was told to stop before the volume was in order", opened.Load())
	}
}

func TestDoesNotMakeTheCheckoutAgainOnceItWorked(t *testing.T) {
	var calls atomic.Int64
	again := make(chan struct{})
	r := start(t, withRepository(func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		if calls.Add(1) == 2 {
			close(again)
		}
		return cloned(ctx, o)
	}))
	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})

	// The wait between two tries is a few milliseconds here, so a loop that went on would have
	// tried again many times in this time.
	select {
	case <-again:
		t.Error("the checkout was made again after it worked")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSaysThatTheCheckoutIsRunningWhileItRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	r := start(t, withRepository(func(ctx context.Context, o checkout.Options) (checkout.Result, error) {
		close(started)
		<-release
		return cloned(ctx, o)
	}))
	<-started

	got, found := checkoutCheck(t, r)

	if !found || got.Outcome != outcomeSkipped || got.Reason != preflight.ReasonNotChecked || !strings.Contains(got.Message, "running") {
		t.Errorf("checkout = %+v, want it skipped, and the message to say that it is running: the volume checks passed", got)
	}
	close(release)
	eventually(t, "the checkout to be made", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomePassed
	})
}

func TestCutsALongMessageOfTheCheckoutShort(t *testing.T) {
	long := strings.Repeat("日", maxMessage/2)
	r := start(t, withRepository(func(context.Context, checkout.Options) (checkout.Result, error) {
		return checkout.Result{}, &checkout.Error{Reason: checkout.ReasonFetchFailed, Message: long}
	}))
	eventually(t, "the failure in the report", func() bool {
		got, _ := checkoutCheck(t, r)
		return got.Outcome == outcomeFailed
	})

	got, _ := checkoutCheck(t, r)

	if len(got.Message) > maxMessage+3 || !strings.HasSuffix(got.Message, "...") || !utf8.ValidString(got.Message) {
		t.Errorf("message is %d bytes, valid UTF-8 = %v, want it cut at %d bytes at a character, with ...",
			len(got.Message), utf8.ValidString(got.Message), maxMessage)
	}
}
