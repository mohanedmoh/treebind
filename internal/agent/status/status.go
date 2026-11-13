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

// Package status serves the agent's report to the operator. The agent holds no
// Kubernetes credentials, so this endpoint is how its state reaches the status of
// a Treebind: the operator polls it and writes what it reads.
//
// The endpoint is read-only and answers only a caller whose bearer token the
// Verifier accepts. The report is a contract with the operator (docs/adr/0011):
// within a Version fields are only added, and a reader ignores the ones it does
// not know.
package status

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

const (
	// Version is the version of the report format. It changes only when a field is
	// removed or changes its meaning.
	Version = 1

	// Path is the only path the endpoint serves.
	Path = "/status"

	// MaxInFlight bounds the tokens being verified and the reports being written
	// at once, so that a burst cannot grow the agent's memory without limit. A
	// request without a well-formed token is refused before it takes a slot. The
	// bound is not a defence against a flood: it holds only while verifying a
	// token is quick, which the Verifier has to see to, and keeping strangers away
	// from the port is the job of a NetworkPolicy.
	MaxInFlight = 32
)

// Report is what the agent tells the operator about itself.
type Report struct {
	// Version is the version of the format, Version.
	Version int `json:"version"`
	// Treebind names the Treebind the agent works for, so that a reader can tell
	// that it reached the agent it meant to.
	Treebind Treebind `json:"treebind"`
	// StartedAt is when the agent process started.
	StartedAt time.Time `json:"startedAt"`
	// Time is when the report was made, by the agent's clock.
	Time time.Time `json:"time"`
	// CheckedAt is when the checks last finished. It is absent until they first
	// have, and then Checks is empty because they are still to run, not because
	// nothing failed. A reader can also tell from it how old the results are.
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// Checks are the results of the preflight checks, in the order they ran.
	Checks []Check `json:"checks"`
}

// Treebind is the namespace and name of a Treebind.
type Treebind struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Check is the result of one preflight check, with the names, outcomes and
// reasons of docs/adr/0009. The message holds no credential.
type Check struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`
}

// Verifier decides whether a bearer token may read the report.
type Verifier interface {
	// Verify returns nil when the token is accepted. The error is for the
	// agent's own use and is never sent to the caller.
	Verify(token string) error
}

// Handler serves the report at Path.
type Handler struct {
	// Rejected, when set, is called with the reason each time a caller is
	// refused. The reason never reaches the caller, so this is how the agent
	// learns that the operator's token stopped being accepted. Anyone who can
	// reach the port can cause refusals, and the reason can carry text taken from
	// the token, so log it rarely and quoted. It is called from several
	// goroutines at once.
	Rejected func(reason error)

	source   func() Report
	verifier Verifier
	now      func() time.Time
	inFlight chan struct{}
}

// errNoBearerToken is the reason for refusing a request that carries no single,
// well-formed bearer token.
var errNoBearerToken = errors.New("the request has no single well-formed bearer token")

// NewHandler returns a handler that serves what source returns to callers the
// verifier accepts. It panics without a source or a verifier, at start and not
// at the first request. Both are called from several goroutines at once, and
// source has to return a report that nothing changes afterwards.
func NewHandler(source func() Report, verifier Verifier) *Handler {
	if source == nil || verifier == nil {
		panic("status: NewHandler needs a source and a verifier")
	}
	return &Handler{
		source:   source,
		verifier: verifier,
		now:      time.Now,
		inFlight: make(chan struct{}, MaxInFlight),
	}
}

// ServeHTTP answers a GET or HEAD at Path from an accepted caller with the
// report. A caller that is not accepted learns nothing but that: the answer is
// the same whether the token is missing, malformed, expired or signed by someone
// else. Any other path is not found, for anyone.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No answer of this endpoint may be stored, whatever it says.
	w.Header().Set("Cache-Control", noStore)

	// The path is compared as it was sent. Nothing is cleaned, decoded or
	// redirected, so "//status" and "/%73tatus" are other paths.
	if r.URL.EscapedPath() != Path {
		http.NotFound(w, r)
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		h.refuse(w, errNoBearerToken)
		return
	}

	select {
	case h.inFlight <- struct{}{}:
		defer func() { <-h.inFlight }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}

	if reason := h.verifier.Verify(token); reason != nil {
		h.refuse(w, reason)
		return
	}
	if !readOnlyMethod(w, r) {
		return
	}

	report := h.source()
	report.Version = Version
	report.StartedAt = report.StartedAt.UTC()
	report.Time = h.now().UTC()
	if report.Checks == nil {
		report.Checks = []Check{}
	}
	body, err := json.Marshal(report)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// refuse answers a caller that is not accepted and tells the agent why.
func (h *Handler) refuse(w http.ResponseWriter, reason error) {
	if h.Rejected != nil {
		h.Rejected(reason)
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// readOnlyMethod reports whether the request is a GET or a HEAD, and answers 405
// when it is not.
func readOnlyMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header. A
// request with no such header, with several, or with anything else in it has
// none.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// ChecksFrom turns the results of the preflight checks into the checks of a
// report. It is the one place where the two meet.
func ChecksFrom(results []preflight.Result) []Check {
	checks := make([]Check, 0, len(results))
	for _, result := range results {
		checks = append(checks, Check{
			Name:    result.Check,
			Outcome: string(result.Outcome),
			Reason:  result.Reason,
			Message: result.Message,
		})
	}
	return checks
}

// Server returns an HTTP server for the endpoint with limits that suit a small
// read-only answer: a caller that sends slowly, holds a connection open or sends
// large headers is cut off.
func Server(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
