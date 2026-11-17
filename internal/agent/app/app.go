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

// Package app runs the agent: it serves the status endpoint and the probes, it
// checks the volume until the checks pass, takes the writer lock of the volume,
// makes the checkout of the repository or opens the one that is there, and then
// pulls the commits that the repository gains, every interval, for as long as it runs.
//
// A failed check does not end the agent. It stays up, says what failed in its
// report, and checks again, so the operator can read the failure and the failure
// goes away when the volume is put right (docs/adr/0009 and 0011). The checkout is
// the same: a repository that cannot be reached, or a directory that is not empty,
// is reported and tried again, and the agent carries on once it is put right
// (docs/adr/0017). The writer lock is taken before anything writes, and an agent that
// loses it stops with ErrLockLost (docs/adr/0023). A pull that fails is reported and
// the loop goes on (docs/adr/0024).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/status"
	"github.com/mohanedmoh/treebind/internal/agent/syncloop"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

const (
	// DefaultRetryMin and DefaultRetryMax bound the wait between two runs of the
	// volume checks while one fails. The wait starts at the first and doubles at
	// every retry up to the second.
	DefaultRetryMin = 5 * time.Second
	DefaultRetryMax = time.Minute

	// refusedLogEvery is how often at most a refused request is logged. Anyone who
	// can reach the port can cause refusals.
	refusedLogEvery = 30 * time.Second

	// DefaultShutdownGrace is how long the requests that are being answered get when
	// the agent is told to stop, and then how long a volume check that is running
	// gets: each has the whole period.
	DefaultShutdownGrace = 5 * time.Second

	// maxReasonLog bounds the reason of a refusal that is logged.
	maxReasonLog = 200

	// CheckCheckout is the name of the check that says whether the checkout of the
	// repository is made. It is in the report after the checks of the volume.
	CheckCheckout = "Checkout"

	// shortCommit is how much of a commit id a message shows.
	shortCommit = 12

	// maxMessage is the most bytes of a message of the checkout that the report holds.
	maxMessage = 4096
)

// Options is what Run needs. Only Config and Verifier are required.
type Options struct {
	Config config.Config
	// Verifier decides which callers may read the report.
	Verifier status.Verifier
	// Preflight runs the volume checks on the mount path, for the sub path of the
	// Treebind. It defaults to preflight.Volume.
	Preflight func(ctx context.Context, path, subPath string) []preflight.Result
	// AcquireLock takes the writer lock of the volume, and waits for it while another agent holds
	// it. It is called once the volume checks pass and before anything is written, and the lock
	// is kept until the agent stops. It defaults to none, so that no lock is taken: the agent
	// starts with lock.Acquire.
	AcquireLock func(ctx context.Context, cfg lock.Config) (*lock.Lock, error)
	// LockTTL is how long another agent has to see no heartbeat before it takes the lock over.
	// DefaultLockTTL when it is zero. LockClock is the system clock when nil.
	LockTTL   time.Duration
	LockClock lock.Clock
	// Checkout makes the checkout of the repository, or opens the one that is there. It
	// is run once the volume checks pass, and again with a growing wait while it fails.
	// It defaults to none, so that nothing is fetched: the agent starts with checkout.Open.
	Checkout func(ctx context.Context, options checkout.Options) (checkout.Result, error)
	// NewPuller makes what pulls into the checkout, once the checkout is made, and the pulls go on
	// every Config.SyncInterval until the agent stops. It defaults to none, so that nothing is pulled:
	// the agent starts with pull.New. Both sync directions only pull for now.
	NewPuller func(options pull.Options) (Puller, error)
	// NewScanner makes what scans the synced directory for local changes, once the checkout is made, for a
	// Treebind that syncs in both directions. The scans go on beside the pulls, in the same loop, until the
	// agent stops. It defaults to none, so that nothing is scanned: the agent starts with scan.New.
	NewScanner func(options scan.Options) (Scanner, error)
	// NewWatcher makes what tells the agent that the directory may have changed, so that it scans sooner than
	// its interval. It defaults to none, and the scans are then on their interval alone: the agent starts
	// with watch.New.
	NewWatcher func(options watch.Options) (Watcher, error)
	// Settled is called with the changes that have settled, at the scan that finds them settled, and again at
	// each scan while they are still there. It is where the commit is to go (#36), and is none until then.
	Settled func(ctx context.Context, settled []scan.Change)
	// ScanInterval is the longest wait between two scans, and ScanGap the shortest time between the end of a
	// scan and the next that an event calls for. The defaults of package syncloop when they are zero.
	ScanInterval, ScanGap time.Duration
	// SyncClock and SyncRandom are where the loop gets its waits and its jitter, and are the system's
	// when nil. A clock for a loop with scans has Now as well (syncloop.WallClock).
	SyncClock  syncloop.Clock
	SyncRandom func() float64
	// Ready says whether the agent has synced. It defaults to never: there is no
	// sync yet, and a Treebind must not turn ready before anything syncs.
	Ready func() bool
	// Logger defaults to the default logger of log/slog.
	Logger *slog.Logger
	// Listener, when set, is served instead of listening on Config.StatusAddr.
	Listener net.Listener
	// RetryMin, RetryMax and ShutdownGrace default to the constants above.
	RetryMin, RetryMax, ShutdownGrace time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// agent is the state of a running agent.
type agent struct {
	Options
	startedAt time.Time
	// checks is the latest results of the checks, as a report carries them.
	checks atomic.Pointer[checked]
	refuse *rateLimit
	// holder names this start of the agent in the writer lock: the name of its pod, and a token
	// that differs between two starts of it.
	holder string
	// held is the writer lock once it is taken, which the fence of a pull checks.
	held atomic.Pointer[lock.Lock]
	// lost is where the keeper of the lock, or a pull that found it gone, says it was lost, and
	// background waits for the keeper to end.
	lost       chan error
	background sync.WaitGroup
	// idle is closed once what the agent writes with, the checkout and the pulls, has stopped. The
	// writer lock is given up after that and not before: a lock that is given up while Git is still
	// being stopped lets the next agent write beside it (NFR-1).
	idle chan struct{}
}

// checked is the results of one run of the volume checks and when it finished.
type checked struct {
	at     time.Time
	checks []status.Check
}

// Run serves the status endpoint and checks the volume until ctx is done, and then
// stops cleanly: requests that are being answered finish and the checks stop.
// It returns an error only when the endpoint cannot be served.
func Run(ctx context.Context, options Options) error {
	a := &agent{Options: options, lost: make(chan error, 1), idle: make(chan struct{})}
	a.fillDefaults()
	a.startedAt = a.Now()
	a.refuse = &rateLimit{every: refusedLogEvery, now: a.Now}

	listener := a.Listener
	if listener == nil {
		var err error
		if listener, err = net.Listen("tcp", a.Config.StatusAddr); err != nil {
			return fmt.Errorf("listen for the status endpoint: %w", err)
		}
	}

	handler := status.NewHandler(a.report, a.Verifier)
	handler.Rejected = a.logRefused
	server := status.Server(listener.Addr().String(), status.Routes(handler, a.Ready))

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	a.Logger.Info("serving the status endpoint", "address", listener.Addr().String(),
		"namespace", a.Config.Namespace, "name", a.Config.Name)

	var checking sync.WaitGroup
	checkCtx, stopChecking := context.WithCancel(ctx)
	defer stopChecking()
	checking.Go(func() { a.prepare(checkCtx) })

	var serveErr, lostErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	case lostErr = <-a.lost:
		a.Logger.Error("the writer lock was lost to another agent, and this one stops", "error", lostErr)
	}

	// The endpoint stops taking requests first. A volume check that does not return
	// is given the rest of the grace period and then left behind, so that a hung
	// mount cannot hold the agent past the time its pod gives it.
	stopChecking()
	serving, cancelServing := context.WithTimeout(context.Background(), a.ShutdownGrace)
	defer cancelServing()
	if err := server.Shutdown(serving); err != nil {
		a.Logger.Warn("the status endpoint did not stop cleanly", "error", err)
	}
	finished := make(chan struct{})
	go func() {
		checking.Wait()
		close(a.idle)
		a.background.Wait()
		close(finished)
	}()
	timer := time.NewTimer(a.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		a.Logger.Warn("a volume check did not stop in time and was left behind")
	}

	if lostErr != nil {
		return fmt.Errorf("%w: %w", ErrLockLost, lostErr)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve the status endpoint: %w", serveErr)
	}
	return nil
}

func (a *agent) fillDefaults() {
	if a.Preflight == nil {
		a.Preflight = preflight.Volume
	}
	if a.Ready == nil {
		a.Ready = func() bool { return false }
	}
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	if a.RetryMin <= 0 {
		a.RetryMin = DefaultRetryMin
	}
	if a.RetryMax < a.RetryMin {
		a.RetryMax = max(DefaultRetryMax, a.RetryMin)
	}
	if a.ShutdownGrace <= 0 {
		a.ShutdownGrace = DefaultShutdownGrace
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	a.fillLockDefaults()
}

// report is what the endpoint serves.
func (a *agent) report() status.Report {
	report := status.Report{
		Treebind:  status.Treebind{Namespace: a.Config.Namespace, Name: a.Config.Name},
		StartedAt: a.startedAt,
		Checks:    []status.Check{},
	}
	if latest := a.checks.Load(); latest != nil {
		report.CheckedAt, report.Checks = latest.at, latest.checks
	}
	return report
}

// progress is what the report holds of the steps that the agent takes one after another: the
// volume checks, the writer lock if there is one to take, the checkout if there is one to make, and
// the pulls if there are any to run.
type progress struct {
	volume                  []preflight.Result
	lock, checkout, pulling *preflight.Result
	// changes is the scan for local changes, which only a Treebind that syncs in both directions has.
	changes *preflight.Result
}

// prepare checks the volume, and once it is in order takes the writer lock, which is held
// until the agent stops, makes the checkout, and then pulls until the agent stops.
func (a *agent) prepare(ctx context.Context) {
	volume, ok := a.checkVolume(ctx)
	if !ok {
		return
	}
	p := &progress{volume: volume}
	if a.pullsFollow() {
		p.pulling = notTried(CheckPull, "not tried, because the checkout is not made yet")
	}
	if a.scansFollow() {
		p.changes = notTried(CheckChanges, "not tried, because the checkout is not made yet")
	}
	if a.AcquireLock != nil {
		if !a.takeLock(ctx, p) {
			return
		}
		if a.Checkout != nil {
			// The checkout runs for as long as the repository takes, and its row says so until its result is there.
			p.checkout = notTried(CheckCheckout, "running: its result is here when it ends")
			a.publish(p)
		}
	}
	made, ok := a.openCheckout(ctx, p)
	if ok && a.pullsFollow() {
		a.pullLoop(ctx, p, made)
	}
}

// checkVolume runs the volume checks, and runs them again with a growing wait while
// one fails. Once none fails the volume is in order, the loop ends and it returns the
// results of that run and true. It returns false when the agent is told to stop first.
func (a *agent) checkVolume(ctx context.Context) ([]preflight.Result, bool) {
	wait := a.RetryMin
	var logged []string
	for {
		results := a.Preflight(ctx, a.Config.MountPath, a.Config.SubPath)
		if ctx.Err() != nil {
			return nil, false
		}
		failed := preflight.AnyFailed(results)
		a.publish(a.waitingForTheVolume(results, failed))
		logged = a.logChanges("volume check", results, logged)

		if !failed {
			return results, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		wait = min(wait*2, a.RetryMax)
	}
}

// waitingForTheVolume is the progress while the volume checks run: skipped checks that say why the
// steps that come after have not been tried, for the steps that there are. With the volume in order the
// next step starts at once, and what it says stays in the report until its result is there, which for the
// lock of another agent and for a large repository takes a while.
func (a *agent) waitingForTheVolume(volume []preflight.Result, failed bool) *progress {
	p := &progress{volume: volume}
	later := "running: its result is here when it ends"
	switch {
	case failed:
		later = "not tried, because a volume check failed"
		if a.AcquireLock != nil {
			p.lock = notTried(CheckLock, later)
		}
	case a.AcquireLock != nil:
		p.lock = notTried(CheckLock, "waiting: the lock is being asked for, and another agent may hold it")
		later = "not tried, because the lock is not held yet"
	}
	if a.Checkout != nil {
		p.checkout = notTried(CheckCheckout, later)
	}
	if a.pullsFollow() {
		p.pulling = notTried(CheckPull, "not tried, because the checkout is not made yet")
		if failed {
			p.pulling = notTried(CheckPull, later)
		}
	}
	if a.scansFollow() {
		p.changes = notTried(CheckChanges, "not tried, because the checkout is not made yet")
		if failed {
			p.changes = notTried(CheckChanges, later)
		}
	}
	return p
}

// pullsFollow says whether the checkout is followed by pulls: there is a checkout to make and
// something to pull with.
func (a *agent) pullsFollow() bool { return a.Checkout != nil && a.NewPuller != nil }

// notTried is the result of a check that has not been tried, with the reason that says so.
func notTried(check, message string) *preflight.Result {
	return &preflight.Result{Check: check, Outcome: preflight.Skipped, Reason: preflight.ReasonNotChecked, Message: message}
}

// publish stores the report's checks: the volume checks, then the lock, the checkout and the pulls, for
// the steps that there are.
func (a *agent) publish(p *progress) {
	results := append([]preflight.Result(nil), p.volume...)
	for _, step := range []*preflight.Result{p.lock, p.checkout, p.pulling, p.changes} {
		if step != nil {
			results = append(results, *step)
		}
	}
	a.checks.Store(&checked{at: a.Now().UTC(), checks: status.ChecksFrom(results)})
}

// checkedOut is what the checkout was asked for and what it made, which the pulls that follow need.
type checkedOut struct {
	options checkout.Options
	made    checkout.Result
}

// openCheckout makes the checkout of the repository, or opens the one that is there, and
// does it again with a growing wait while it fails: the repository may not be reachable
// yet, or the directory may be emptied. Once it succeeds the loop ends, and it returns what
// it made and true. It returns false when there is no checkout to make, or when the agent is
// told to stop first.
func (a *agent) openCheckout(ctx context.Context, p *progress) (checkedOut, bool) {
	if a.Checkout == nil {
		return checkedOut{}, false
	}
	options, err := a.checkoutOptions()
	wait := a.RetryMin
	var logged []string
	for {
		var result preflight.Result
		var made checkout.Result
		if err != nil {
			result = failedCheckout(err)
		} else {
			result, made = a.runCheckout(ctx, options)
		}
		if ctx.Err() != nil {
			return checkedOut{}, false
		}
		p.checkout = &result
		a.publish(p)
		logged = a.logChanges("checkout", []preflight.Result{result}, logged)

		if result.Outcome == preflight.Passed {
			return checkedOut{options: options, made: made}, true
		}
		select {
		case <-ctx.Done():
			return checkedOut{}, false
		case <-time.After(wait):
		}
		wait = min(wait*2, a.RetryMax)
	}
}

// checkoutOptions says what to check out and where from the settings of the pod. The
// credentials are the files that the operator mounted, and never their content.
func (a *agent) checkoutOptions() (checkout.Options, error) {
	where, err := layout.New(a.Config.MountPath, a.Config.SubPath)
	if err != nil {
		return checkout.Options{}, err
	}
	return checkout.Options{
		Layout: where,
		URL:    a.Config.RepoURL,
		Branch: a.Config.RepoBranch,
		Git: git.Options{SSH: git.SSH{
			IdentityFile:   a.Config.SSHKeyFile,
			KnownHostsFile: a.Config.KnownHostsFile,
		}},
	}, nil
}

// runCheckout runs the checkout once and turns what came of it into a result of the check, and
// returns what it made.
func (a *agent) runCheckout(ctx context.Context, options checkout.Options) (preflight.Result, checkout.Result) {
	made, err := a.Checkout(ctx, options)
	if err != nil {
		return failedCheckout(err), checkout.Result{}
	}
	commit := shortOf(made.Commit)
	var message string
	switch made.Outcome {
	case checkout.Cloned:
		message = fmt.Sprintf("cloned the branch %s (%s) into %s", made.Branch, commit, options.Layout.WorkTree)
	case checkout.Adopted:
		message = fmt.Sprintf("%s holds exactly the files of the branch %s (%s) and is the checkout now",
			options.Layout.WorkTree, made.Branch, commit)
	default:
		message = fmt.Sprintf("reopened the checkout of the branch %s (%s) in %s", made.Branch, commit, options.Layout.WorkTree)
	}
	return preflight.Result{Check: CheckCheckout, Outcome: preflight.Passed, Message: message}, made
}

// shortOf is how much of a commit a message shows, or says that there is none.
func shortOf(commit string) string {
	if commit == "" {
		return "no commit yet"
	}
	return commit[:min(len(commit), shortCommit)]
}

// failedCheckout is the failed result for an error of the checkout: its own reason when
// the error has one, and the reason for a checkout that could not be made when it has not.
func failedCheckout(err error) preflight.Result {
	result := preflight.Result{
		Check: CheckCheckout, Outcome: preflight.Failed, Reason: checkout.ReasonCheckoutFailed, Message: shortened(err.Error()),
	}
	if refused, ok := errors.AsType[*checkout.Error](err); ok {
		result.Reason = refused.Reason
	}
	return result
}

// shortened cuts a message to maxMessage bytes, at a character. What Git wrote is already
// short and has the credentials of any URL removed, and this keeps the report small whatever
// else a message holds.
func shortened(message string) string {
	if len(message) <= maxMessage {
		return message
	}
	return strings.ToValidUTF8(message[:maxMessage], "") + "..."
}

// logChanges logs the results that differ from the ones last logged, so that a
// check that keeps failing is logged once and not at every retry. It returns what
// was logged.
func (a *agent) logChanges(what string, results []preflight.Result, before []string) []string {
	now := make([]string, len(results))
	for i, result := range results {
		now[i] = result.Check + "|" + string(result.Outcome) + "|" + result.Reason + "|" + result.Message
		if i < len(before) && before[i] == now[i] {
			continue
		}
		level := slog.LevelInfo
		if result.Outcome != preflight.Passed {
			level = slog.LevelWarn
		}
		a.Logger.Log(context.Background(), level, what,
			"check", result.Check, "outcome", string(result.Outcome), "reason", result.Reason, "message", result.Message)
	}
	return now
}

// logRefused logs that a caller was refused, at most once in refusedLogEvery. The
// verifier words its reasons without text from the token. The reason is cut short
// and slog quotes it, so it could not do harm if one did.
func (a *agent) logRefused(reason error) {
	if suppressed, ok := a.refuse.allow(); ok {
		text := reason.Error()
		if len(text) > maxReasonLog {
			text = strings.ToValidUTF8(text[:maxReasonLog], "") + "..."
		}
		a.Logger.Warn("a status request was refused", "reason", text, "alsoRefusedSince", suppressed)
	}
}

// rateLimit lets one event through in every period and counts the ones it holds
// back.
type rateLimit struct {
	every time.Duration
	now   func() time.Time

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// allow reports whether the event may be logged, and how many were held back since
// the last one that was.
func (r *rateLimit) allow() (suppressed int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.last.IsZero() && now.Sub(r.last) < r.every {
		r.suppressed++
		return 0, false
	}
	suppressed, r.suppressed, r.last = r.suppressed, 0, now
	return suppressed, true
}
