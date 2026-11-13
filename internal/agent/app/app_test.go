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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	goodToken = "good.token.value"
	testNS    = "apps"
	testName  = "dashboards"
)

type verifierFunc func(string) error

func (f verifierFunc) Verify(token string) error { return f(token) }

var onlyGoodToken = verifierFunc(func(token string) error {
	if token != goodToken {
		return errors.New("the signature does not verify")
	}
	return nil
})

// safeBuffer is a log destination that several goroutines write to.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) count(substring string) int {
	return strings.Count(b.String(), substring)
}

// running is an agent that is serving on a free port.
type running struct {
	url     string
	logs    *safeBuffer
	cancel  context.CancelFunc
	stopped chan struct{}
	err     error
}

// waitStopped waits for Run to return, and returns what it returned.
func (r *running) waitStopped(t *testing.T) error {
	t.Helper()
	select {
	case <-r.stopped:
		return r.err
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
		return nil
	}
}

var passed = []preflight.Result{
	{Check: preflight.CheckMount, Outcome: preflight.Passed, Message: "mounted"},
	{Check: preflight.CheckWritable, Outcome: preflight.Passed, Message: "writable"},
}

var failed = []preflight.Result{
	{Check: preflight.CheckMount, Outcome: preflight.Passed, Message: "mounted"},
	{Check: preflight.CheckWritable, Outcome: preflight.Failed, Reason: preflight.ReasonNotWritable, Message: "uid 65532 cannot write"},
}

func start(t *testing.T, change func(*Options)) *running {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &safeBuffer{}
	options := Options{
		Config:   config.Config{Namespace: testNS, Name: testName, MountPath: "/work"},
		Verifier: onlyGoodToken,
		Preflight: func(context.Context, string) []preflight.Result {
			return passed
		},
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
		Listener: listener,
		RetryMin: time.Millisecond,
		RetryMax: 5 * time.Millisecond,
	}
	if change != nil {
		change(&options)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &running{url: "http://" + listener.Addr().String(), logs: logs, cancel: cancel, stopped: make(chan struct{})}
	go func() {
		r.err = Run(ctx, options)
		close(r.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		_ = r.waitStopped(t)
	})
	return r
}

func (r *running) get(t *testing.T, path string, authorization ...string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, r.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range authorization {
		request.Header.Add("Authorization", value)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// report reads the report as the operator does.
func (r *running) report(t *testing.T) status.Report {
	t.Helper()
	code, body := r.get(t, status.Path, "Bearer "+goodToken)
	if code != http.StatusOK {
		t.Fatalf("the report was refused: %d %s", code, body)
	}
	var report status.Report
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("not a report: %v: %s", err, body)
	}
	return report
}

// eventually waits for a condition that the agent brings about by itself.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestServesTheReport(t *testing.T) {
	// A clock that moves on at every look, so that the times in the report say
	// which came first.
	var looks atomic.Int64
	base := time.Now()
	r := start(t, func(o *Options) {
		o.Now = func() time.Time { return base.Add(time.Duration(looks.Add(1)) * time.Second) }
	})

	report := r.report(t)

	if report.Version != status.Version || report.Treebind != (status.Treebind{Namespace: testNS, Name: testName}) {
		t.Errorf("the report names %+v at version %d", report.Treebind, report.Version)
	}
	if report.StartedAt.IsZero() {
		t.Errorf("startedAt is not set")
	}
	eventually(t, "the checks in the report", func() bool { return len(r.report(t).Checks) == 2 })
	report = r.report(t)
	if !report.CheckedAt.After(report.StartedAt) {
		t.Errorf("checkedAt %s is not after startedAt %s: the checks finished after the agent started", report.CheckedAt, report.StartedAt)
	}
	checks := report.Checks
	if checks[0].Name != "Mount" || checks[0].Outcome != "Passed" || checks[1].Name != "Writable" {
		t.Errorf("the checks are %+v", checks)
	}
}

func TestTheReportSaysThatTheChecksAreStillToRun(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := start(t, func(o *Options) {
		o.Preflight = func(ctx context.Context, _ string) []preflight.Result {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return passed
		}
	})
	<-entered

	report := r.report(t)

	if len(report.Checks) != 0 || !report.CheckedAt.IsZero() {
		t.Errorf("before the checks finished the report has %d checks, checked at %s", len(report.Checks), report.CheckedAt)
	}
	code, body := r.get(t, status.Path, "Bearer "+goodToken)
	if code != http.StatusOK || strings.Contains(body, "checkedAt") || !strings.Contains(body, `"checks":[]`) {
		t.Errorf("the body should say that no checks have finished: %s", body)
	}

	close(release)
	eventually(t, "the checks to finish", func() bool { return !r.report(t).CheckedAt.IsZero() })
}

func TestRefusesStrangers(t *testing.T) {
	r := start(t, nil)

	for _, authorization := range [][]string{nil, {"Bearer forged"}, {"Basic " + goodToken}} {
		if code, body := r.get(t, status.Path, authorization...); code != http.StatusUnauthorized || body != "unauthorized\n" {
			t.Errorf("%v got %d %q, want 401", authorization, code, body)
		}
	}
}

func TestKeepsCheckingWhileACheckFailsAndStopsOnceTheyPass(t *testing.T) {
	var calls atomic.Int64
	r := start(t, func(o *Options) {
		o.Preflight = func(context.Context, string) []preflight.Result {
			if calls.Add(1) <= 4 {
				return failed
			}
			return passed
		}
	})

	eventually(t, "the failure in the report", func() bool {
		checks := r.report(t).Checks
		return len(checks) == 2 && checks[1].Outcome == "Failed"
	})
	if checks := r.report(t).Checks; checks[1].Reason != "NotWritable" || !strings.Contains(checks[1].Message, "cannot write") {
		t.Errorf("the failure is %+v", checks[1])
	}

	eventually(t, "the checks to pass", func() bool {
		checks := r.report(t).Checks
		return len(checks) == 2 && checks[1].Outcome == "Passed"
	})
	settled := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != settled {
		t.Errorf("the checks ran again after they passed: %d then %d", settled, calls.Load())
	}
	if settled != 5 {
		t.Errorf("the checks ran %d times, want 5: four failures and a pass", settled)
	}
}

func TestLogsAFailureOnceAndTheChangeWhenItEnds(t *testing.T) {
	var calls atomic.Int64
	r := start(t, func(o *Options) {
		o.Preflight = func(context.Context, string) []preflight.Result {
			if calls.Add(1) <= 6 {
				return failed
			}
			return passed
		}
	})

	eventually(t, "the checks to pass", func() bool { return calls.Load() >= 7 })
	eventually(t, "the pass to be logged", func() bool { return r.logs.count("outcome=Passed") >= 2 })

	if n := r.logs.count("outcome=Failed"); n != 1 {
		t.Errorf("a failure that repeated 6 times was logged %d times, want once:\n%s", n, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "cannot write") {
		t.Errorf("the failure was not logged with its message:\n%s", r.logs.String())
	}
}

func TestProbes(t *testing.T) {
	t.Run("alive at once, never ready, because nothing syncs yet", func(t *testing.T) {
		r := start(t, nil)

		if code, _ := r.get(t, status.HealthPath); code != http.StatusOK {
			t.Errorf("healthz is %d", code)
		}
		if code, _ := r.get(t, status.ReadyPath); code != http.StatusServiceUnavailable {
			t.Errorf("readyz is %d, want 503", code)
		}
	})

	t.Run("ready when the agent says it has synced", func(t *testing.T) {
		var synced atomic.Bool
		r := start(t, func(o *Options) { o.Ready = synced.Load })

		if code, _ := r.get(t, status.ReadyPath); code != http.StatusServiceUnavailable {
			t.Fatalf("readyz is %d before the sync", code)
		}
		synced.Store(true)

		if code, _ := r.get(t, status.ReadyPath); code != http.StatusOK {
			t.Errorf("readyz is %d after the sync, want 200", code)
		}
	})

	t.Run("a failed check does not make it not alive", func(t *testing.T) {
		r := start(t, func(o *Options) {
			o.Preflight = func(context.Context, string) []preflight.Result { return failed }
		})

		eventually(t, "the failure", func() bool { return len(r.report(t).Checks) == 2 })

		if code, _ := r.get(t, status.HealthPath); code != http.StatusOK {
			t.Errorf("healthz is %d while a check fails: the kubelet would restart a pod that is only waiting", code)
		}
	})
}

func TestLogsRefusalsRarelyAndCountsTheRest(t *testing.T) {
	var now atomic.Pointer[time.Time]
	clock := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	now.Store(&clock)
	r := start(t, func(o *Options) { o.Now = func() time.Time { return *now.Load() } })

	for range 40 {
		r.get(t, status.Path, "Bearer forged")
	}
	if n := r.logs.count("a status request was refused"); n != 1 {
		t.Errorf("40 refusals were logged %d times, want once:\n%s", n, r.logs.String())
	}

	later := clock.Add(refusedLogEvery + time.Second)
	now.Store(&later)
	r.get(t, status.Path, "Bearer forged")

	if n := r.logs.count("a status request was refused"); n != 2 {
		t.Errorf("after the period the refusals were logged %d times, want 2", n)
	}
	if !strings.Contains(r.logs.String(), "alsoRefusedSince=39") {
		t.Errorf("the 39 refusals that were held back are not counted:\n%s", r.logs.String())
	}
	if strings.Contains(r.logs.String(), "forged") {
		t.Errorf("the log carries text from a token:\n%s", r.logs.String())
	}
}

func TestLongRefusalReasonsAreCutShort(t *testing.T) {
	long := strings.Repeat("x", 5000)
	r := start(t, func(o *Options) {
		o.Verifier = verifierFunc(func(string) error { return errors.New(long) })
	})

	r.get(t, status.Path, "Bearer anything")

	logged := r.logs.String()
	if strings.Contains(logged, strings.Repeat("x", maxReasonLog+1)) || !strings.Contains(logged, strings.Repeat("x", maxReasonLog)+"...") {
		t.Errorf("the reason was not cut at %d characters:\n%.400s", maxReasonLog, logged)
	}
}

func TestStopsOnRequest(t *testing.T) {
	t.Run("returns without an error and stops serving", func(t *testing.T) {
		r := start(t, nil)
		if code, _ := r.get(t, status.HealthPath); code != http.StatusOK {
			t.Fatalf("healthz is %d", code)
		}

		r.cancel()

		if err := r.waitStopped(t); err != nil {
			t.Errorf("Run returned %v on request", err)
		}
		if _, err := (&http.Client{Timeout: time.Second}).Get(r.url + status.HealthPath); err == nil {
			t.Error("the endpoint still answers after the agent stopped")
		}
	})

	t.Run("stops while a volume check is running", func(t *testing.T) {
		entered := make(chan struct{})
		r := start(t, func(o *Options) {
			o.Preflight = func(ctx context.Context, _ string) []preflight.Result {
				close(entered)
				<-ctx.Done()
				return failed
			}
		})
		<-entered

		r.cancel()

		_ = r.waitStopped(t)
	})

	t.Run("does not return while a volume check is still running", func(t *testing.T) {
		entered := make(chan struct{})
		var finished atomic.Bool
		r := start(t, func(o *Options) {
			o.Preflight = func(ctx context.Context, _ string) []preflight.Result {
				close(entered)
				<-ctx.Done()
				time.Sleep(100 * time.Millisecond) // a check that takes a moment to wind down
				finished.Store(true)
				return failed
			}
		})
		<-entered

		r.cancel()
		_ = r.waitStopped(t)

		if !finished.Load() {
			t.Error("Run returned while a volume check was still running")
		}
	})

	t.Run("does not wait for a volume check that never returns", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		r := start(t, func(o *Options) {
			o.ShutdownGrace = 200 * time.Millisecond
			o.Preflight = func(context.Context, string) []preflight.Result {
				close(entered)
				<-release // a call that ignores the context, as one stuck on a dead mount does
				return failed
			}
		})
		<-entered

		started := time.Now()
		r.cancel()
		err := r.waitStopped(t)

		if err != nil {
			t.Errorf("Run returned %v", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("Run took %s to stop with a check that never returns", elapsed)
		}
		if !strings.Contains(r.logs.String(), "did not stop in time") {
			t.Errorf("the check that was left behind is not logged:\n%s", r.logs.String())
		}
	})

	t.Run("gives a check that is winding down its own time after a slow request", func(t *testing.T) {
		// The request is answered 400 ms after the stop, so the server is done then,
		// and the check needs until 900 ms: after the time the server had (600 ms),
		// within the time the check has once the server is done.
		entered := make(chan struct{})
		var finished atomic.Bool
		unblock := make(chan struct{})
		verifying := make(chan struct{})
		r := start(t, func(o *Options) {
			o.ShutdownGrace = 600 * time.Millisecond
			o.Verifier = verifierFunc(func(string) error {
				close(verifying)
				<-unblock
				return nil
			})
			o.Preflight = func(ctx context.Context, _ string) []preflight.Result {
				close(entered)
				<-ctx.Done()
				time.Sleep(900 * time.Millisecond)
				finished.Store(true)
				return failed
			}
		})
		<-entered
		go func() { r.get(t, status.Path, "Bearer "+goodToken) }()
		<-verifying

		r.cancel()
		time.Sleep(400 * time.Millisecond)
		close(unblock)
		_ = r.waitStopped(t)

		if !finished.Load() {
			t.Error("the check was not given its own time after the server was done")
		}
		if strings.Contains(r.logs.String(), "did not stop in time") {
			t.Errorf("the check was reported as left behind although it had time:\n%s", r.logs.String())
		}
	})

	t.Run("stops serving before it waits for a check that is winding down", func(t *testing.T) {
		entered := make(chan struct{})
		r := start(t, func(o *Options) {
			o.Preflight = func(ctx context.Context, _ string) []preflight.Result {
				close(entered)
				<-ctx.Done()
				time.Sleep(300 * time.Millisecond)
				return failed
			}
		})
		<-entered

		r.cancel()
		time.Sleep(100 * time.Millisecond) // the check is still winding down

		if _, err := (&http.Client{Timeout: time.Second}).Get(r.url + status.HealthPath); err == nil {
			t.Error("the endpoint still took requests while the agent was stopping")
		}
		_ = r.waitStopped(t)
	})

	t.Run("stops while it waits to check again", func(t *testing.T) {
		var calls atomic.Int64
		r := start(t, func(o *Options) {
			o.RetryMin, o.RetryMax = time.Hour, time.Hour
			o.Preflight = func(context.Context, string) []preflight.Result {
				calls.Add(1)
				return failed
			}
		})
		eventually(t, "the first check", func() bool { return calls.Load() == 1 })

		r.cancel()

		_ = r.waitStopped(t)
	})
}

func TestRunCannotServe(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	err = Run(context.Background(), Options{
		Config:   config.Config{StatusAddr: taken.Addr().String()},
		Verifier: onlyGoodToken,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err == nil || !strings.Contains(err.Error(), "listen for the status endpoint") {
		t.Errorf("got %v, want a failure to listen", err)
	}
}

func TestRunReturnsAnErrorWhenServingFailsAfterItStarted(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- Run(context.Background(), Options{
			Config:   config.Config{Namespace: testNS, Name: testName},
			Verifier: onlyGoodToken,
			Listener: listener,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Preflight: func(ctx context.Context, _ string) []preflight.Result {
				<-ctx.Done()
				return nil
			},
		})
	}()
	time.Sleep(50 * time.Millisecond)

	_ = listener.Close() // what the endpoint stands on goes away

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "serve the status endpoint") {
			t.Errorf("got %v, want a failure to serve", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept running without an endpoint")
	}
}

func TestRateLimit(t *testing.T) {
	var clock time.Time
	limit := &rateLimit{every: time.Minute, now: func() time.Time { return clock }}
	clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	if suppressed, ok := limit.allow(); !ok || suppressed != 0 {
		t.Fatalf("the first event: %d, %v", suppressed, ok)
	}
	for range 3 {
		if _, ok := limit.allow(); ok {
			t.Error("an event within the period was allowed")
		}
	}
	clock = clock.Add(time.Minute)
	if suppressed, ok := limit.allow(); !ok || suppressed != 3 {
		t.Errorf("after the period: %d held back, %v, want 3 and true", suppressed, ok)
	}
}
