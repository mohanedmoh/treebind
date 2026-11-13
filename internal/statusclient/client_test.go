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

package statusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	testToken    = "operator-token-s3cret"
	testAudience = "treebind.example.org/apps/grafana"
	appsNS       = "apps"
	grafanaName  = "grafana"

	messageNotJSON          = "the sync agent's report is not JSON"
	messageNoAnswer         = "the sync pod did not answer in time"
	messageConnectionFailed = "the connection to the sync pod failed"
	opDial                  = "dial"
)

var testTarget = Target{Namespace: appsNS, Name: grafanaName, Audience: testAudience}

// fakeTokens is a TokenSource that hands out one token and records what it is
// asked.
type fakeTokens struct {
	mu        sync.Mutex
	token     string
	err       error
	asked     []string
	forgotten []string
	// refused are the tokens that were dropped, in the order of forgotten.
	refused []string
}

func (f *fakeTokens) Token(_ context.Context, audience string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, audience)
	return f.token, f.err
}

func (f *fakeTokens) Forget(audience, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, audience)
	f.refused = append(f.refused, token)
}

// goodReport is a report that the client accepts for testTarget.
func goodReport() status.Report {
	return status.Report{
		Version:   status.Version,
		Treebind:  status.Treebind{Namespace: testTarget.Namespace, Name: testTarget.Name},
		StartedAt: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC),
		Time:      time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		CheckedAt: time.Date(2026, 11, 13, 9, 59, 0, 0, time.UTC),
		Checks: []status.Check{
			{Name: "Mount", Outcome: "Passed", Message: "mounted"},
			{Name: "Writable", Outcome: "Failed", Reason: "ReadOnlyVolume", Message: "the volume refuses writes"},
		},
	}
}

// newTestClient returns a client that calls the server and uses the tokens.
func newTestClient(server *httptest.Server, tokens TokenSource) *Client {
	client := New(tokens)
	client.Address = func(Target) (string, error) { return strings.TrimPrefix(server.URL, "http://"), nil }
	return client
}

// answering returns a server that answers every request with the handler, and
// closes it with the test.
func answering(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func writeReport(w http.ResponseWriter, report status.Report) {
	body, _ := json.Marshal(report)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func TestFetchReadsTheReportWithTheOperatorsToken(t *testing.T) {
	var got *http.Request
	server := answering(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		writeReport(w, goodReport())
	})
	tokens := &fakeTokens{token: testToken}

	report, err := newTestClient(server, tokens).Fetch(context.Background(), testTarget)
	if err != nil {
		t.Fatal(err)
	}

	want := goodReport()
	if report.Version != want.Version || report.Treebind != want.Treebind || !report.Time.Equal(want.Time) ||
		!report.CheckedAt.Equal(want.CheckedAt) || len(report.Checks) != 2 || report.Checks[1] != want.Checks[1] {
		t.Errorf("report = %+v, want %+v", report, want)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/status" || got.URL.RawQuery != "" {
		t.Errorf("request = %s %s, want GET /status with no query", got.Method, got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the token as a bearer token", got.Header.Get("Authorization"))
	}
	if len(tokens.asked) != 1 || tokens.asked[0] != testAudience {
		t.Errorf("the token was asked for %v, want the audience of this Treebind, %s", tokens.asked, testAudience)
	}
	if len(tokens.forgotten) != 0 {
		t.Errorf("the token was dropped after a good answer: %v", tokens.forgotten)
	}
}

func TestFetchIgnoresFieldsOfTheReportThatItDoesNotKnow(t *testing.T) {
	server := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"version":1,"treebind":{"namespace":"apps","name":"grafana","uid":"x"},`+
			`"time":"2026-11-13T10:00:00Z","startedAt":"2026-11-13T09:00:00Z","checks":[],"sync":{"state":"idle"}}`)
	})

	report, err := newTestClient(server, &fakeTokens{token: testToken}).Fetch(context.Background(), testTarget)
	if err != nil || report.Version != 1 {
		t.Errorf("report = %+v, error = %v, want the report of version 1 read with the new fields ignored", report, err)
	}
}

func TestFetchSaysWhyAnAnswerIsNotAReport(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantKind    Kind
		wantMessage string
		wantDropped bool
	}{
		{
			name:     "the agent refuses the token",
			handler:  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) },
			wantKind: KindTokenRefused, wantMessage: "the sync agent refused the operator's token", wantDropped: true,
		},
		{
			name:     "the agent is busy",
			handler:  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", http.StatusServiceUnavailable) },
			wantKind: KindUnexpectedResponse, wantMessage: "the sync agent answered 503",
		},
		{
			name:     "another status",
			handler:  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusNotFound) },
			wantKind: KindUnexpectedResponse, wantMessage: "the sync agent answered 404",
		},
		{
			name:     "no content",
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			wantKind: KindUnexpectedResponse, wantMessage: "the sync agent answered 204",
		},
		{
			name:     "a body that is not JSON",
			handler:  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>login</html>")) },
			wantKind: KindReportUnreadable, wantMessage: messageNotJSON,
		},
		{
			name:     "an empty body",
			handler:  func(http.ResponseWriter, *http.Request) {},
			wantKind: KindReportUnreadable, wantMessage: messageNotJSON,
		},
		{
			name: "a report followed by more",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				body, _ := json.Marshal(goodReport())
				_, _ = w.Write(append(body, []byte(`{"again":1}`)...))
			},
			wantKind: KindReportUnreadable, wantMessage: messageNotJSON,
		},
		{
			name: "a report of a version the operator does not read",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				report := goodReport()
				report.Version = 2
				body, _ := json.Marshal(report)
				_, _ = w.Write(body)
			},
			wantKind: KindReportUnreadable, wantMessage: "the sync agent's report has version 2, and the operator reads version 1",
		},
		{
			name: "a report with no version",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"treebind":{"namespace":"apps","name":"grafana"}}`))
			},
			wantKind: KindReportUnreadable, wantMessage: "the sync agent's report has version 0, and the operator reads version 1",
		},
		{
			name: "the report of another Treebind in the same namespace",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				report := goodReport()
				report.Treebind.Name = "other"
				writeReport(w, report)
			},
			wantKind: KindReportUnreadable, wantMessage: "the report is the report of another Treebind",
		},
		{
			name: "the report of a Treebind in another namespace",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				report := goodReport()
				report.Treebind.Namespace = "elsewhere"
				writeReport(w, report)
			},
			wantKind: KindReportUnreadable, wantMessage: "the report is the report of another Treebind",
		},
		{
			name: "a report that is too large",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"version":1,"padding":"` + strings.Repeat("x", MaxReportBytes) + `"}`))
			},
			wantKind: KindReportUnreadable, wantMessage: "the sync agent's report is larger than 256 KiB",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := &fakeTokens{token: testToken}

			report, err := newTestClient(answering(t, tt.handler), tokens).Fetch(context.Background(), testTarget)

			fetchErr := wantFetchError(t, err, tt.wantKind, tt.wantMessage)
			if report != nil {
				t.Errorf("a report came back with the error: %+v", report)
			}
			assertNoSecret(t, fetchErr)
			if dropped := len(tokens.forgotten) == 1 && tokens.forgotten[0] == testAudience &&
				tokens.refused[0] == testToken; dropped != tt.wantDropped {
				t.Errorf("token dropped = %v (%v), want %v: a token that was refused must be asked for again, "+
					"and one that was not must be kept", dropped, tokens.forgotten, tt.wantDropped)
			}
		})
	}
}

func TestFetchAcceptsAReportJustUnderTheLimit(t *testing.T) {
	server := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(goodReport())
		body = body[:len(body)-1]
		padding := MaxReportBytes - len(body) - len(`,"p":""}`)
		_, _ = w.Write(append(body, []byte(`,"p":"`+strings.Repeat("x", padding)+`"}`)...))
	})

	if _, err := newTestClient(server, &fakeTokens{token: testToken}).Fetch(context.Background(), testTarget); err != nil {
		t.Errorf("a report of exactly %d bytes was refused: %v", MaxReportBytes, err)
	}
}

// wantFetchError checks that err is a *Error of the kind with the message, and
// returns it.
func wantFetchError(t *testing.T, err error, kind Kind, message string) *Error {
	t.Helper()
	var fetchErr *Error
	if !errors.As(err, &fetchErr) {
		t.Fatalf("error = %v (%T), want an *Error", err, err)
	}
	if fetchErr.Kind != kind || fetchErr.Message != message {
		t.Errorf("error = {%s, %q}, want {%s, %q}", fetchErr.Kind, fetchErr.Message, kind, message)
	}
	return fetchErr
}

// assertNoSecret checks that neither the text of the error nor its cause holds the
// token, which would put it in a condition or a log.
func assertNoSecret(t *testing.T, err *Error) {
	t.Helper()
	if strings.Contains(err.Message, testToken) || strings.Contains(err.Error(), testToken) {
		t.Errorf("the error holds the token: %s", err.Error())
	}
	if err.Err != nil && strings.Contains(err.Err.Error(), testToken) {
		t.Errorf("the cause of the error holds the token: %s", err.Err.Error())
	}
}

func TestFetchDoesNotFollowARedirect(t *testing.T) {
	var elsewhere, sawToken int
	other := answering(t, func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		if r.Header.Get("Authorization") != "" {
			sawToken++
		}
		writeReport(w, goodReport())
	})
	server := answering(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/status", http.StatusFound)
	})

	_, err := newTestClient(server, &fakeTokens{token: testToken}).Fetch(context.Background(), testTarget)

	assertNoSecret(t, wantFetchError(t, err, KindUnexpectedResponse, "the sync agent answered 302"))
	if elsewhere != 0 || sawToken != 0 {
		t.Errorf("the redirect was followed (%d requests, %d with the token): the token would go wherever "+
			"a pod that answers in the agent's place says", elsewhere, sawToken)
	}
}

func TestFetchSaysWhenTheConnectionFails(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close() // nothing listens there now

		client := New(&fakeTokens{token: testToken})
		client.Address = func(Target) (string, error) { return address, nil }

		_, err = client.Fetch(context.Background(), testTarget)

		fetchErr := wantFetchError(t, err, KindConnectionFailed, "the sync pod refused the connection")
		assertNoSecret(t, fetchErr)
		if !errors.Is(fetchErr, syscall.ECONNREFUSED) {
			t.Errorf("the cause is %v, want the refused connection kept for the log", fetchErr.Err)
		}
	})

	t.Run("no answer in time", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(release) }) // runs first, so that the server can close

		client := newTestClient(server, &fakeTokens{token: testToken})
		client.HTTP.Timeout = 50 * time.Millisecond

		_, err := client.Fetch(context.Background(), testTarget)

		assertNoSecret(t, wantFetchError(t, err, KindConnectionFailed, messageNoAnswer))
	})

	t.Run("the connection breaks while the report is read", func(t *testing.T) {
		server := answering(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "5000")
			_, _ = w.Write([]byte(`{"version":1,`))
			// The server ends the response here, which leaves the reader short of
			// the length it was told.
		})

		_, err := newTestClient(server, &fakeTokens{token: testToken}).Fetch(context.Background(), testTarget)

		assertNoSecret(t, wantFetchError(t, err, KindConnectionFailed, messageConnectionFailed))
	})
}

func TestFetchSaysWhenThePodCannotBeNamed(t *testing.T) {
	client := New(&fakeTokens{token: testToken})
	client.Address = func(Target) (string, error) { return "", errors.New("not a name") }

	_, err := client.Fetch(context.Background(), testTarget)

	if err == nil || !strings.Contains(err.Error(), "not a name") {
		t.Errorf("error = %v, want the reason the name was refused", err)
	}
	if _, ok := errors.AsType[*Error](err); ok {
		t.Errorf("error = %v, want a plain error: the name is a fact about the spec, not about the agent", err)
	}
}

func TestFetchSaysWhenThereIsNoTokenAndDoesNotCall(t *testing.T) {
	var calls int
	server := answering(t, func(http.ResponseWriter, *http.Request) { calls++ })
	cause := errors.New("serviceaccounts \"op\" is forbidden: the secret detail " + testToken)
	client := newTestClient(server, &fakeTokens{err: cause})

	_, err := client.Fetch(context.Background(), testTarget)

	fetchErr := wantFetchError(t, err, KindTokenUnavailable, "the operator could not get a token for the sync agent")
	if !errors.Is(err, cause) {
		t.Errorf("the cause of %v is lost, so the log cannot say why", err)
	}
	// The cause is for the log, and it is the API server's own text, so the
	// message that goes into a condition must not carry it.
	if strings.Contains(fetchErr.Message, "forbidden") {
		t.Errorf("the message carries the text of the cause: %q", fetchErr.Message)
	}
	if calls != 0 {
		t.Errorf("the agent was called %d times without a token", calls)
	}
}

func TestFetchStopsWhenTheCallerGivesUp(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestClient(server, &fakeTokens{token: testToken}).Fetch(ctx, testTarget)

	var fetchErr *Error
	if !errors.Is(err, context.Canceled) || errors.As(err, &fetchErr) {
		t.Errorf("error = %v, want the cancellation as it is: the manager stopping is not a failure of the agent", err)
	}
}

func TestClassifyTheFailuresOfTheNetwork(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantKind    Kind
		wantMessage string
	}{
		{
			"a name that does not exist",
			&net.OpError{Op: opDial, Err: &net.DNSError{Err: "no such host", Name: "x-sync-0.x-sync.apps.svc", IsNotFound: true}},
			KindNotRunning, "the name of the sync pod does not resolve",
		},
		{
			"a name server that does not answer",
			&net.OpError{Op: opDial, Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}},
			KindConnectionFailed, messageNoAnswer,
		},
		{
			"a name server that fails",
			&net.OpError{Op: opDial, Err: &net.DNSError{Err: "server misbehaving", IsTemporary: true}},
			KindConnectionFailed, messageConnectionFailed,
		},
		{
			"a refused connection",
			&net.OpError{Op: opDial, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
			KindConnectionFailed, "the sync pod refused the connection",
		},
		{
			"no route",
			&net.OpError{Op: opDial, Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)},
			KindConnectionFailed, messageConnectionFailed,
		},
		{
			"a connection that is reset",
			&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			KindConnectionFailed, messageConnectionFailed,
		},
		{"the time of the poll", context.DeadlineExceeded, KindConnectionFailed, messageNoAnswer},
		{"a timeout of the network", timeoutError{}, KindConnectionFailed, messageNoAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(fmt.Errorf("Get %q: %w", "http://x:8080/status", tt.err))
			if got.Kind != tt.wantKind || got.Message != tt.wantMessage {
				t.Errorf("classify = {%s, %q}, want {%s, %q}", got.Kind, got.Message, tt.wantKind, tt.wantMessage)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("the cause is lost: %v", got)
			}
		})
	}
}

// timeoutError is a network error that says it is a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "a timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTheHTTPClientIsBuiltToBeSafeToSendATokenWith(t *testing.T) {
	client := NewHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want an *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Error("the client uses a proxy: the token would go to whoever runs it, in the clear")
	}
	if client.CheckRedirect == nil {
		t.Fatal("the client follows redirects")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
	if !transport.DisableKeepAlives {
		t.Error("connections are kept, so one idle connection per agent would pile up")
	}
	if client.Timeout != Timeout {
		t.Errorf("timeout = %v, want %v", client.Timeout, Timeout)
	}
	if transport.ResponseHeaderTimeout <= 0 || transport.ResponseHeaderTimeout > Timeout {
		t.Errorf("response header timeout = %v, want it set and within %v", transport.ResponseHeaderTimeout, Timeout)
	}
	if transport.MaxResponseHeaderBytes <= 0 {
		t.Error("the size of the headers of an answer is not bounded")
	}
	if transport.ForceAttemptHTTP2 {
		t.Error("HTTP/2 is attempted: the agent speaks HTTP/1.1 and the call is plain")
	}
}

func TestAddressNamesThePodOfTheTreebindAndTheAgentsPort(t *testing.T) {
	got, err := AgentAddress(testTarget)
	if err != nil {
		t.Fatal(err)
	}
	want := "grafana-sync-0.grafana-sync.apps.svc:" + strconv.Itoa(Port)
	if got != want {
		t.Errorf("address = %s, want %s", got, want)
	}
	if Port != 8080 {
		t.Errorf("port = %d, want 8080, the port of the agent (docs/adr/0011)", Port)
	}
}

func TestAddressRefusesNamesThatCannotNameASyncPod(t *testing.T) {
	longest := strings.Repeat("a", 63-len("-sync"))
	tests := []struct {
		name   string
		target Target
		wantOK bool
	}{
		{"the longest name that fits", Target{Namespace: appsNS, Name: longest}, true},
		{"a name too long for the Service", Target{Namespace: appsNS, Name: longest + "a"}, false},
		{"a name with a dot", Target{Namespace: appsNS, Name: "a.b"}, false},
		// "a-" is not a label, but with the suffix it is: "a--sync".
		{"a name that ends with a dash", Target{Namespace: appsNS, Name: "a-"}, false},
		{"a name that starts with a dash", Target{Namespace: appsNS, Name: "-a"}, false},
		{"a name in capitals", Target{Namespace: appsNS, Name: "Grafana"}, false},
		{"an empty name", Target{Namespace: appsNS}, false},
		{"a namespace with a dot", Target{Namespace: "a.b", Name: grafanaName}, false},
		{"an empty namespace", Target{Name: grafanaName}, false},
		{"a name that would add a path", Target{Namespace: appsNS, Name: "a/b"}, false},
		{"a name that would add a port", Target{Namespace: appsNS, Name: "a:80"}, false},
		{"a name that would add credentials", Target{Namespace: appsNS, Name: "a@evil"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, err := AgentAddress(tt.target)
			if (err == nil) != tt.wantOK {
				t.Errorf("AgentAddress = %q, %v, want accepted = %v", address, err, tt.wantOK)
			}
			if err != nil && address != "" {
				t.Errorf("an address came back with the error: %q", address)
			}
		})
	}
}
