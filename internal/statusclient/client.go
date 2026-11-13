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

// Package statusclient is the operator's side of the agent's status endpoint
// (docs/adr/0011, docs/adr/0015). It asks one agent for its report with a token of
// the operator's own service account, and says why when it cannot get one.
//
// Every failure of a poll is an *Error with a Kind and a Message. The message is a
// fixed sentence for its cause and is meant for a condition of the Treebind, which
// everyone who can read the Treebind reads: it holds no token, no text of a network
// error and nothing taken from the agent's answer. The technical cause is kept in
// Err for the log.
package statusclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/mohanedmoh/treebind/internal/agent/status"
)

const (
	// Port is the port the agent serves its status on (docs/adr/0011).
	Port = 8080

	// MaxReportBytes is the largest report that is read. A report holds the results
	// of a few checks, so a larger answer is something else.
	MaxReportBytes = 256 << 10

	// Timeout is the most one poll takes, from the first connection attempt to the
	// last byte of the report.
	Timeout = 5 * time.Second

	// dialTimeout bounds the connection. Most of Timeout is left for the answer.
	dialTimeout = 2 * time.Second

	// maxHeaderBytes bounds the headers of an answer.
	maxHeaderBytes = 16 << 10

	// podSuffix is added to the name of a Treebind to name its StatefulSet, and its
	// Service, and the first and only pod is that name and -0.
	podSuffix = "-sync"
)

// Kind says why a poll failed, in the terms of the AgentUnreachable condition.
type Kind string

const (
	// KindNotRunning means the name of the sync pod does not resolve, which is a
	// pod that does not exist yet and not a failure of the agent.
	KindNotRunning Kind = "NotRunning"
	// KindConnectionFailed means the connection was refused or nothing answered.
	KindConnectionFailed Kind = "ConnectionFailed"
	// KindTokenRefused means the agent answered 401.
	KindTokenRefused Kind = "TokenRefused"
	// KindUnexpectedResponse means the agent answered with another status.
	KindUnexpectedResponse Kind = "UnexpectedResponse"
	// KindReportUnreadable means the agent answered 200 with something that is not a
	// report the operator can use.
	KindReportUnreadable Kind = "ReportUnreadable"
	// KindTokenUnavailable means the operator could not get a token, which is not the
	// agent's fault.
	KindTokenUnavailable Kind = "TokenUnavailable"
)

// Error is a failed poll.
type Error struct {
	// Kind is the cause in the terms of the condition.
	Kind Kind
	// Message is a fixed sentence for the cause. It is safe to put in a condition.
	Message string
	// Err is the technical cause, for the log. It is never put in a condition.
	Err error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Target names the agent of one Treebind and the token it accepts.
type Target struct {
	Namespace, Name string
	// Audience is the aud claim of the token the agent accepts.
	Audience string
}

// TokenSource gives the operator's token for an audience.
type TokenSource interface {
	// Token returns a token of the operator's service account for the audience.
	Token(ctx context.Context, audience string) (string, error)
	// Forget drops the given token if it is the one kept for the audience, so that
	// the next call asks for a new one. It is called when the agent refused the
	// token.
	Forget(audience, token string)
}

// Client polls agents.
type Client struct {
	// Tokens gives the token to call an agent with.
	Tokens TokenSource
	// HTTP makes the calls. New sets it to NewHTTPClient().
	HTTP *http.Client
	// Address returns the host and port of the agent of a Treebind. New sets it to
	// AgentAddress. A test points it at a server of its own.
	Address func(Target) (string, error)
}

// New returns a client that gets its tokens from tokens.
func New(tokens TokenSource) *Client {
	return &Client{Tokens: tokens, HTTP: NewHTTPClient(), Address: AgentAddress}
}

// NewHTTPClient returns an HTTP client for the calls to the agents. It is built so
// that the token goes only to the address it is aimed at: it uses no proxy, so
// that a variable of the operator's environment cannot send it somewhere else, and
// it follows no redirect, so that a pod that answers in an agent's place cannot
// send it onwards. The call is plain HTTP on the pod network, which ADR 0011
// accepts: the token is good for the report of one Treebind and for minutes. The
// client keeps no connection, so that one idle connection per agent does not pile
// up, and it has a time limit for the connection, for the headers and for the whole
// call.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: Timeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: dialTimeout}).DialContext,
			DisableKeepAlives:      true,
			ForceAttemptHTTP2:      false,
			ResponseHeaderTimeout:  Timeout - dialTimeout,
			MaxResponseHeaderBytes: maxHeaderBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// AgentAddress returns the host and port of the agent of a Treebind: the name of the
// first pod of its StatefulSet behind its headless Service, with the agent's port. It
// refuses a name that could not be a part of that host, so that what a Treebind is
// called cannot make the operator call something else.
func AgentAddress(target Target) (string, error) {
	for _, label := range []string{target.Namespace, target.Name, target.Name + podSuffix} {
		if problems := validation.IsDNS1123Label(label); len(problems) > 0 {
			return "", fmt.Errorf("the Treebind %q in %q cannot name a sync pod: %q is not a DNS label: %s",
				target.Name, target.Namespace, label, problems[0])
		}
	}
	service := target.Name + podSuffix
	host := service + "-0." + service + "." + target.Namespace + ".svc"
	return net.JoinHostPort(host, strconv.Itoa(Port)), nil
}

// Fetch asks the agent of the Treebind for its report.
//
// A failure of the poll is an *Error. The cancellation of ctx is returned as it is,
// because the manager stopping says nothing about the agent. An address that cannot
// name a pod is a plain error, because it is a fact about the spec.
func (c *Client) Fetch(ctx context.Context, target Target) (*status.Report, error) {
	address, err := c.Address(target)
	if err != nil {
		return nil, err
	}
	token, err := c.Tokens.Token(ctx, target.Audience)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Kind: KindTokenUnavailable, Message: "the operator could not get a token for the sync agent", Err: err}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+status.Path, nil)
	if err != nil {
		return nil, fmt.Errorf("build the request for the sync agent: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")

	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, c.failed(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusUnauthorized:
		c.Tokens.Forget(target.Audience, token)
		return nil, &Error{Kind: KindTokenRefused, Message: "the sync agent refused the operator's token"}
	case response.StatusCode != http.StatusOK:
		return nil, &Error{
			Kind:    KindUnexpectedResponse,
			Message: fmt.Sprintf("the sync agent answered %d", response.StatusCode),
		}
	}
	return c.readReport(ctx, response.Body, target)
}

// readReport reads and checks the report in an answer.
func (c *Client) readReport(ctx context.Context, body io.Reader, target Target) (*status.Report, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxReportBytes+1))
	if err != nil {
		return nil, c.failed(ctx, err)
	}
	if len(data) > MaxReportBytes {
		return nil, &Error{
			Kind:    KindReportUnreadable,
			Message: fmt.Sprintf("the sync agent's report is larger than %d KiB", MaxReportBytes>>10),
		}
	}

	var report status.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, &Error{Kind: KindReportUnreadable, Message: "the sync agent's report is not JSON", Err: err}
	}
	if report.Version != status.Version {
		return nil, &Error{
			Kind: KindReportUnreadable,
			Message: fmt.Sprintf("the sync agent's report has version %d, and the operator reads version %d",
				report.Version, status.Version),
		}
	}
	if report.Treebind.Namespace != target.Namespace || report.Treebind.Name != target.Name {
		return nil, &Error{Kind: KindReportUnreadable, Message: "the report is the report of another Treebind"}
	}
	return &report, nil
}

// failed turns an error of the network into the error of the poll. A caller that
// gave up is not a failure of the agent.
func (c *Client) failed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return classify(err)
}

// classify says in the terms of the condition why a call did not get an answer.
func classify(err error) *Error {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return &Error{Kind: KindNotRunning, Message: "the name of the sync pod does not resolve", Err: err}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &Error{Kind: KindConnectionFailed, Message: "the sync pod refused the connection", Err: err}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return &Error{Kind: KindConnectionFailed, Message: "the sync pod did not answer in time", Err: err}
	default:
		return &Error{Kind: KindConnectionFailed, Message: "the connection to the sync pod failed", Err: err}
	}
}
