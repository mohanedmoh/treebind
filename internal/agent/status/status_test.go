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

package status

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

const (
	goodToken = "good.token.value"
)

// verifierFunc lets a function be the Verifier.
type verifierFunc func(token string) error

func (f verifierFunc) Verify(token string) error { return f(token) }

// onlyGoodToken accepts goodToken and gives every other token an error whose
// text must never reach a caller.
var onlyGoodToken = verifierFunc(func(token string) error {
	if token != goodToken {
		return errors.New("signature of key kid-1 does not match")
	}
	return nil
})

var (
	started  = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	reported = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
)

func sampleReport() Report {
	return Report{
		Treebind:  Treebind{Namespace: "apps", Name: "dashboards"},
		StartedAt: started,
		Checks: []Check{
			{Name: "Mount", Outcome: "Passed", Message: "/work is on the volume mounted at /work (ext4, read-write)"},
			{Name: "Writable", Outcome: "Failed", Reason: "NotWritable", Message: "uid 65532 cannot create a file in /work"},
		},
	}
}

func newHandler(verifier Verifier) *Handler {
	handler := NewHandler(sampleReport, verifier)
	handler.now = func() time.Time { return reported }
	return handler
}

// request sends one request to the handler.
func request(handler *Handler, method, path string, authorization ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for _, value := range authorization {
		r.Header.Add("Authorization", value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// The report is a contract with the operator. A change to this text is a change
// to the contract: add fields, and raise Version for anything else.
func TestReportFormat(t *testing.T) {
	const want = `{"version":1,"treebind":{"namespace":"apps","name":"dashboards"},` +
		`"startedAt":"2026-11-12T09:00:00Z","time":"2026-11-12T09:30:00Z","checks":[` +
		`{"name":"Mount","outcome":"Passed","message":"/work is on the volume mounted at /work (ext4, read-write)"},` +
		`{"name":"Writable","outcome":"Failed","reason":"NotWritable","message":"uid 65532 cannot create a file in /work"}]}`

	w := request(newHandler(onlyGoodToken), http.MethodGet, Path, "Bearer "+goodToken)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != want {
		t.Errorf("the report changed:\n got %s\nwant %s", got, want)
	}
}

func TestReportFormatAfterTheChecksRan(t *testing.T) {
	const want = `{"version":1,"treebind":{"namespace":"apps","name":"dashboards"},` +
		`"startedAt":"2026-11-12T09:00:00Z","time":"2026-11-12T09:30:00Z","checkedAt":"2026-11-12T09:10:00Z","checks":[]}`
	handler := NewHandler(func() Report {
		return Report{
			Treebind:  Treebind{Namespace: "apps", Name: "dashboards"},
			StartedAt: started,
			CheckedAt: time.Date(2026, 11, 12, 9, 10, 0, 0, time.UTC),
		}
	}, onlyGoodToken)
	handler.now = func() time.Time { return reported }

	w := request(handler, http.MethodGet, Path, "Bearer "+goodToken)

	if got := w.Body.String(); got != want {
		t.Errorf("the report changed:\n got %s\nwant %s", got, want)
	}
}

func TestHandlerServesTheReport(t *testing.T) {
	t.Run("an accepted GET gets the report with headers that keep it out of caches", func(t *testing.T) {
		w := request(newHandler(onlyGoodToken), http.MethodGet, Path, "Bearer "+goodToken)

		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", w.Code)
		}
		for header, want := range map[string]string{
			"Content-Type":           "application/json",
			"Cache-Control":          noStore,
			"X-Content-Type-Options": "nosniff",
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s is %q, want %q", header, got, want)
			}
		}
		var report Report
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatalf("the body is not a report: %v", err)
		}
		if report.Version != Version || !report.Time.Equal(reported) || !report.StartedAt.Equal(started) {
			t.Errorf("version %d, time %s, startedAt %s", report.Version, report.Time, report.StartedAt)
		}
	})

	t.Run("times are in UTC whatever the agent's time zone", func(t *testing.T) {
		zone := time.FixedZone("UTC+3", 3*60*60)
		handler := NewHandler(func() Report { return Report{StartedAt: started.In(zone)} }, onlyGoodToken)
		handler.now = func() time.Time { return reported.In(zone) }

		w := request(handler, http.MethodGet, Path, "Bearer "+goodToken)

		for _, want := range []string{`"startedAt":"2026-11-12T09:00:00Z"`, `"time":"2026-11-12T09:30:00Z"`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("the body %s does not contain %s", w.Body.String(), want)
			}
		}
	})

	t.Run("a report with no checks has an empty list, not null", func(t *testing.T) {
		handler := NewHandler(func() Report { return Report{} }, onlyGoodToken)

		w := request(handler, http.MethodGet, Path, "Bearer "+goodToken)

		if !strings.Contains(w.Body.String(), `"checks":[]`) {
			t.Errorf("the body is %s", w.Body.String())
		}
	})

	t.Run("the scheme is not case-sensitive and the token reaches the verifier as it is", func(t *testing.T) {
		var seen string
		handler := newHandler(verifierFunc(func(token string) error {
			seen = token
			return nil
		}))

		w := request(handler, http.MethodGet, Path, "bearer abc.def-_~+/=")

		if w.Code != http.StatusOK || seen != "abc.def-_~+/=" {
			t.Errorf("status %d, the verifier saw %q", w.Code, seen)
		}
	})

	t.Run("a HEAD gets the headers of the GET and no body", func(t *testing.T) {
		handler := newHandler(onlyGoodToken)
		get := request(handler, http.MethodGet, Path, "Bearer "+goodToken)

		head := request(handler, http.MethodHead, Path, "Bearer "+goodToken)

		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Errorf("status %d with %d bytes of body", head.Code, head.Body.Len())
		}
		if got, want := head.Header().Get("Content-Length"), strconv.Itoa(get.Body.Len()); got != want {
			t.Errorf("Content-Length is %q, want %q, the length of the GET body", got, want)
		}
	})
}

func TestHandlerRefusesCallersItDoesNotAccept(t *testing.T) {
	// A header that is not one well-formed bearer token is refused without asking
	// the verifier, so these run against a verifier that would accept anything.
	acceptsAnything := verifierFunc(func(string) error { return nil })

	tests := []struct {
		name          string
		verifier      Verifier
		authorization []string
	}{
		{"no Authorization header", acceptsAnything, nil},
		{"another scheme", acceptsAnything, []string{"Basic " + goodToken}},
		{"the scheme alone", acceptsAnything, []string{"Bearer"}},
		{"an empty token", acceptsAnything, []string{"Bearer "}},
		{"a token with a space in it", acceptsAnything, []string{"Bearer " + goodToken + " extra"}},
		{"a token with a tab in it", acceptsAnything, []string{"Bearer " + goodToken + "\textra"}},
		{"two Authorization headers", acceptsAnything, []string{"Bearer " + goodToken, "Bearer " + goodToken}},
		{"a token the verifier rejects", onlyGoodToken, []string{"Bearer forged.token.value"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := request(newHandler(test.verifier), http.MethodGet, Path, test.authorization...)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", w.Code)
			}
			if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate is %q", got)
			}
			if got := w.Header().Get("Cache-Control"); got != noStore {
				t.Errorf("Cache-Control is %q on a refusal", got)
			}
			// Every refusal reads the same, so a caller cannot tell a missing token
			// from a forged one, and nothing of the report or of the reason leaks.
			if got := w.Body.String(); got != "unauthorized\n" {
				t.Errorf("the body is %q", got)
			}
		})
	}
}

func TestHandlerIsReadOnly(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method+" from an accepted caller is not allowed", func(t *testing.T) {
			w := request(newHandler(onlyGoodToken), method, Path, "Bearer "+goodToken)

			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("status %d, Allow %q", w.Code, w.Header().Get("Allow"))
			}
			if got := w.Header().Get("Cache-Control"); got != noStore {
				t.Errorf("Cache-Control is %q", got)
			}
			if strings.Contains(w.Body.String(), "dashboards") {
				t.Errorf("the answer carries the report: %s", w.Body.String())
			}
		})
	}

	t.Run("a caller that is not accepted is refused before the method is looked at", func(t *testing.T) {
		w := request(newHandler(onlyGoodToken), http.MethodPost, Path)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", w.Code)
		}
	})

	// The path is taken as it was sent: nothing is cleaned or redirected, and a
	// stranger gets the same answer as the operator.
	for _, path := range []string{
		"/", "/status/", "/status/x", "/statusx", "/metrics", "//status", "/./status", "/status/../status", "/STATUS",
		"/%73tatus", "/status%2F",
	} {
		for name, authorization := range map[string][]string{
			"an accepted caller": {"Bearer " + goodToken},
			"a stranger":         nil,
		} {
			t.Run("nothing is served at "+path+" to "+name, func(t *testing.T) {
				w := request(newHandler(onlyGoodToken), http.MethodGet, path, authorization...)

				if w.Code != http.StatusNotFound {
					t.Errorf("status %d, want 404", w.Code)
				}
				if got := w.Header().Get("Cache-Control"); got != noStore {
					t.Errorf("Cache-Control is %q", got)
				}
				if strings.Contains(w.Body.String(), "dashboards") {
					t.Errorf("the answer carries the report: %s", w.Body.String())
				}
			})
		}
	}
}

func TestHandlerTurnsAwayRequestsBeyondTheCap(t *testing.T) {
	entered := make(chan struct{}, MaxInFlight)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	handler := newHandler(verifierFunc(func(string) error {
		entered <- struct{}{}
		<-release
		return nil
	}))

	done := make(chan int, MaxInFlight)
	for range MaxInFlight {
		go func() { done <- request(handler, http.MethodGet, Path, "Bearer "+goodToken).Code }()
	}
	for range MaxInFlight {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("fewer requests than the cap reached the verifier")
		}
	}

	// Every slot is taken by a request that is still being verified.
	w := request(handler, http.MethodGet, Path, "Bearer "+goodToken)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Errorf("status %d, Retry-After %q, want 503 and 1", w.Code, w.Header().Get("Retry-After"))
	}
	if got := w.Header().Get("Cache-Control"); got != noStore {
		t.Errorf("Cache-Control is %q when busy", got)
	}

	// A request without a well-formed token is refused without taking a slot, so
	// it gets its 401 even now.
	if w := request(handler, http.MethodGet, Path); w.Code != http.StatusUnauthorized {
		t.Errorf("a request without a token got %d while every slot was taken, want 401", w.Code)
	}

	releaseAll()
	for range MaxInFlight {
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Errorf("a request that held a slot got %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a request that held a slot did not finish")
		}
	}
	if w := request(handler, http.MethodGet, Path, "Bearer "+goodToken); w.Code != http.StatusOK {
		t.Errorf("status %d after the slots were freed, want 200", w.Code)
	}
}

func TestHandlerTellsTheAgentWhyItRefused(t *testing.T) {
	forged := errors.New("signature of key kid-1 does not match")
	tests := []struct {
		name          string
		authorization []string
		want          error
	}{
		{"a request without a bearer token", nil, errNoBearerToken},
		{"a token the verifier rejects", []string{"Bearer forged"}, forged},
		{"an accepted token", []string{"Bearer " + goodToken}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newHandler(verifierFunc(func(token string) error {
				if token != goodToken {
					return forged
				}
				return nil
			}))
			var reasons []error
			handler.Rejected = func(reason error) { reasons = append(reasons, reason) }

			request(handler, http.MethodGet, Path, test.authorization...)

			switch {
			case test.want == nil && len(reasons) != 0:
				t.Errorf("an accepted request was reported as refused: %v", reasons)
			case test.want != nil && (len(reasons) != 1 || !errors.Is(reasons[0], test.want)):
				t.Errorf("got the reasons %v, want %v once", reasons, test.want)
			}
		})
	}
}

func TestNewHandlerNeedsASourceAndAVerifier(t *testing.T) {
	for name, build := range map[string]func(){
		"no source":   func() { NewHandler(nil, onlyGoodToken) },
		"no verifier": func() { NewHandler(sampleReport, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewHandler did not panic")
				}
			}()
			build()
		})
	}
}

func TestChecksFrom(t *testing.T) {
	results := []preflight.Result{
		{Check: preflight.CheckMount, Outcome: preflight.Passed, Message: "mounted"},
		{Check: preflight.CheckWritable, Outcome: preflight.Failed, Reason: preflight.ReasonNotWritable, Message: "no"},
		{Check: "Other", Outcome: preflight.Skipped, Reason: preflight.ReasonNotChecked, Message: "skipped"},
	}
	want := []Check{
		{Name: "Mount", Outcome: "Passed", Message: "mounted"},
		{Name: "Writable", Outcome: "Failed", Reason: "NotWritable", Message: "no"},
		{Name: "Other", Outcome: "Skipped", Reason: "NotChecked", Message: "skipped"},
	}

	if got := ChecksFrom(results); !reflect.DeepEqual(got, want) {
		t.Errorf("ChecksFrom = %+v, want %+v", got, want)
	}
	if got := ChecksFrom(nil); got == nil || len(got) != 0 {
		t.Errorf("ChecksFrom(nil) = %#v, want an empty list", got)
	}
}

func TestServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := Server(listener.Addr().String(), newHandler(onlyGoodToken))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	get := func(t *testing.T, padding string) (int, string) {
		t.Helper()
		r, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+Path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+goodToken)
		r.Header.Set("X-Padding", padding)
		client := &http.Client{Timeout: 10 * time.Second}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}

	t.Run("serves the report over a real connection", func(t *testing.T) {
		code, body := get(t, "small")
		if code != http.StatusOK || !strings.Contains(body, `"name":"dashboards"`) {
			t.Errorf("status %d, body %s", code, body)
		}
	})

	// A bearer token is about 1 KiB, so the limit of 16 KiB is ample. The server
	// allows a little more than the limit before it cuts a request off.
	t.Run("cuts off a request with 32 KiB of headers", func(t *testing.T) {
		if code, _ := get(t, strings.Repeat("x", 32<<10)); code != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("status %d, want 431", code)
		}
	})

	t.Run("has a timeout for every phase of a request", func(t *testing.T) {
		for name, timeout := range map[string]time.Duration{
			"ReadHeaderTimeout": server.ReadHeaderTimeout,
			"ReadTimeout":       server.ReadTimeout,
			"WriteTimeout":      server.WriteTimeout,
			"IdleTimeout":       server.IdleTimeout,
		} {
			if timeout <= 0 || timeout > time.Minute {
				t.Errorf("%s is %s", name, timeout)
			}
		}
	})
}
