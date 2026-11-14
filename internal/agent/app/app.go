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

// Package app runs the agent: it serves the status endpoint and the probes, and
// it checks the volume until the checks pass.
//
// A failed check does not end the agent. It stays up, says what failed in its
// report, and checks again, so the operator can read the failure and the failure
// goes away when the volume is put right (docs/adr/0009 and 0011).
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

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
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
)

// Options is what Run needs. Only Config and Verifier are required.
type Options struct {
	Config config.Config
	// Verifier decides which callers may read the report.
	Verifier status.Verifier
	// Preflight runs the volume checks on the mount path, for the sub path of the
	// Treebind. It defaults to preflight.Volume.
	Preflight func(ctx context.Context, path, subPath string) []preflight.Result
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
	// checks is the latest results of the volume checks, as a report carries them.
	checks atomic.Pointer[checked]
	refuse *rateLimit
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
	a := &agent{Options: options}
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
	checking.Go(func() { a.checkVolume(checkCtx) })

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
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
		close(finished)
	}()
	timer := time.NewTimer(a.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		a.Logger.Warn("a volume check did not stop in time and was left behind")
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

// checkVolume runs the volume checks, and runs them again with a growing wait while
// one fails. Once none fails the volume is in order and the loop ends.
func (a *agent) checkVolume(ctx context.Context) {
	wait := a.RetryMin
	var logged []string
	for {
		results := a.Preflight(ctx, a.Config.MountPath, a.Config.SubPath)
		if ctx.Err() != nil {
			return
		}
		a.checks.Store(&checked{at: a.Now().UTC(), checks: status.ChecksFrom(results)})
		logged = a.logChanges(results, logged)

		if !preflight.AnyFailed(results) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, a.RetryMax)
	}
}

// logChanges logs the results that differ from the ones last logged, so that a
// check that keeps failing is logged once and not at every retry. It returns what
// was logged.
func (a *agent) logChanges(results []preflight.Result, before []string) []string {
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
		a.Logger.Log(context.Background(), level, "volume check",
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
