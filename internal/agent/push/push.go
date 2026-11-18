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

// Package push sends the commits that the checkout has and the repository lacks to the branch that the checkout
// follows (docs/adr/0033). It pushes as a fast-forward and never forces: the Runner refuses a force, a delete
// and every refspec that is not a branch, before Git starts, and a push that the repository refuses is
// reported, with the words of the host, and not worked around.
//
// What it asks Git is small, and each step is there for something that was tried against it:
//
//   - What is waiting is counted from the commits that the checkout has and the last fetched tip does not
//     (refs/remotes/origin/<branch>), and nothing is sent when there are none, or when the checkout has no commit.
//   - A push is not sent when the checkout and the last fetched tip have gone different ways: the host would
//     refuse it, and a host that is asked for a push that cannot work is a load for nothing (R-19).
//   - `git push --porcelain` writes one line for the branch, which Git words itself: a flag, and in brackets
//     what happened. Only that is read. `[rejected] (fetch first)` and `(non-fast-forward)` are the repository
//     having moved, and any other rejection is the host's decision, whose words are kept as they are.
//   - A push to a URL does not move refs/remotes/origin/<branch>, as one to a named remote does, so the agent
//     moves it after a push that worked, and the count of what is waiting stays true.
//
// The writer lock is checked immediately before the push (docs/adr/0023), and nothing is sent without it.
package push

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

const (
	// ReasonNonFastForward means that the repository has commits on the branch that the checkout lacks, so that
	// the push is not a fast-forward. It is not forced. A person, or the conflict policy (#38), puts it right.
	ReasonNonFastForward = "NonFastForward"
	// ReasonPushRejected means that the repository refused the push: a protected branch, a hook, a rule, a file
	// that is too large, a secret that it found. The message has its words. A person puts it right.
	ReasonPushRejected = "PushRejected"
	// ReasonPushFailed means that the push could not be made: the repository did not answer, the key was
	// refused, the volume refused, or the writer lock could not be checked. The agent tries again with a growing
	// wait.
	ReasonPushFailed = "PushFailed"

	// DefaultTimeout is how long a push may take. It is the one of a fetch: a repository over a slow link takes
	// far longer than the Runner's own timeout.
	DefaultTimeout = checkout.DefaultFetchTimeout

	// maxHostText is the most bytes of what the host said that a message keeps.
	maxHostText = 2048

	headsPrefix  = "refs/heads/"
	remotePrefix = "refs/remotes/origin/"

	// updateTimeout is how long moving the tracking ref after a push that worked may take, when the agent was told
	// to stop in the middle of the push: a short time is all it needs.
	updateTimeout = 5 * time.Second
)

// Outcome says what a call of Push came to.
type Outcome string

const (
	// Pushed means commits were sent and the repository took them.
	Pushed Outcome = "Pushed"
	// NothingToPush means the repository has every commit that the checkout has, or the checkout has none.
	NothingToPush Outcome = "NothingToPush"
)

// Result is what a push did.
type Result struct {
	Outcome Outcome
	Branch  string
	// From is the tip that the repository was last known to be at, and is empty when it had no branch. To is the
	// tip that it is at now.
	From, To string
	// Commits is how many commits were sent.
	Commits int
	// Warning says what did not work after a push that did: the tracking ref could not be moved.
	Warning string
}

// Error is a push that could not be made.
type Error struct {
	// Reason is a stable reason in the sense of docs/adr/0004.
	Reason string
	// Message says what is wrong, for people. It may hold the words of the host.
	Message string
	// Err is the error underneath, which may be nil.
	Err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Options say where to push from, and to.
type Options struct {
	// Layout is where the checkout is.
	Layout layout.Layout
	// URL is the repository, and Branch the branch of it that the checkout follows, which is the branch that is
	// pushed.
	URL, Branch string
	// Git are the settings of the Runner. The directories and the look at the Git directory are taken from Layout.
	Git git.Options
	// Timeout is how long a push may take. DefaultTimeout when it is zero.
	Timeout time.Duration
	// Fence is called immediately before the push, and nothing is sent when it returns an error. It is the check
	// that the agent still holds the writer lock (docs/adr/0023). With none, nothing is checked.
	Fence func(ctx context.Context) error
}

// Pusher pushes one checkout. It is not safe for concurrent use: the agent pushes one at a time, in the same loop
// as its pulls and its scans.
type Pusher struct {
	o   Options
	git *git.Runner
}

// New checks the options and makes a Pusher.
func New(o Options) (*Pusher, error) {
	switch {
	case o.URL == "":
		return nil, errors.New("push: the repository has no address")
	case o.Timeout < 0:
		return nil, errors.New("push: the timeout must not be negative")
	}
	if err := checkBranch(o.Branch); err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	runner, err := git.New(checkout.RunnerOptions(o.Layout, o.Git))
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	return &Pusher{o: o, git: runner}, nil
}

// checkBranch refuses a name that is not a branch of the shape that Git takes, or that would be read as more
// than one: the refspec is made of it, and the Runner holds the refspec to a branch as well.
func checkBranch(name string) error {
	switch {
	case name == "":
		return errors.New("the branch has no name")
	case strings.HasPrefix(name, "-") || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "/"):
		return fmt.Errorf("the branch %q must not start with -, + or /", name)
	case strings.ContainsAny(name, " ~^:?*[\\") || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return fmt.Errorf("the branch %q has a character that a branch name does not", name)
	case strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//"):
		return fmt.Errorf("the branch %q is not a name of a branch", name)
	case strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock"):
		return fmt.Errorf("the branch %q must not end with /, . or .lock", name)
	}
	return nil
}

// Push sends the commits that the repository lacks. It makes at most one push, and none when there is nothing to
// send. A push that could not be made is an *Error.
func (p *Pusher) Push(ctx context.Context) (Result, error) {
	branch := p.o.Branch
	local, born, err := p.try(ctx, "rev-parse", "--verify", "--quiet", headsPrefix+branch+"^{commit}")
	if err != nil {
		return Result{}, p.fail(err, "the branch %s of the checkout cannot be read: %v", branch, err)
	}
	if !born {
		return Result{Outcome: NothingToPush, Branch: branch}, nil
	}
	known, has, err := p.try(ctx, "rev-parse", "--verify", "--quiet", remotePrefix+branch+"^{commit}")
	if err != nil {
		return Result{}, p.fail(err, "the last known tip of the branch %s cannot be read: %v", branch, err)
	}
	waiting, err := p.waiting(ctx, known, has, local)
	if err != nil {
		return Result{}, p.fail(err, "the commits that wait to be pushed cannot be counted: %v", err)
	}
	if waiting == 0 {
		return Result{Outcome: NothingToPush, Branch: branch, From: known, To: known}, nil
	}
	if has {
		ancestor, err := p.isAncestor(ctx, known, local)
		if err != nil {
			return Result{}, p.fail(err, "the branch %s of the checkout cannot be compared with the repository's: %v", branch, err)
		}
		if !ancestor {
			return Result{}, &Error{Reason: ReasonNonFastForward, Message: fmt.Sprintf("the checkout and the branch %s of the repository have gone "+
				"different ways: the checkout has commits that the repository lacks, and the repository has commits that the checkout lacks, "+
				"so the push would not be a fast-forward, and it is not sent. The agent does not force it", branch)}
		}
	}
	if p.o.Fence != nil {
		if err := p.o.Fence(ctx); err != nil {
			return Result{}, &Error{Reason: ReasonPushFailed, Err: err,
				Message: fmt.Sprintf("nothing was pushed, because the writer lock could not be checked: %v", err)}
		}
	}

	refspec := headsPrefix + branch + ":" + headsPrefix + branch
	out, err := p.git.RunTimeout(ctx, p.o.Timeout, "push", "--porcelain", "--no-verify", "--", p.o.URL, refspec)
	status := parsePorcelain(string(out.Stdout), headsPrefix+branch)
	switch {
	case status.found && status.flag == '!':
		reason, message := rejection(status, out.Stderr, branch)
		return Result{}, &Error{Reason: reason, Message: message, Err: err}
	case err != nil:
		return Result{}, p.fail(err, "the branch %s cannot be pushed: %v", branch, err)
	case status.found && status.flag == '+':
		// Nothing in the agent asks for this: the Runner refuses a force. It is not counted as a push that worked.
		return Result{}, &Error{Reason: ReasonPushFailed, Message: fmt.Sprintf("the branch %s of the repository was updated by force, "+
			"which the agent never asks for, so the push is not counted as one that worked", branch)}
	case !status.found || !pushedFlag(status.flag):
		return Result{}, &Error{Reason: ReasonPushFailed, Message: fmt.Sprintf("Git pushed the branch %s and said nothing that the agent can read about it", branch)}
	}

	result := Result{Outcome: Pushed, Branch: branch, From: known, To: local, Commits: waiting}
	if status.flag == '=' {
		// The repository had the commits already, from another way, and the checkout did not know.
		result.Outcome, result.Commits, result.From = NothingToPush, 0, local
	}
	if err := p.moveTrackingRef(ctx, local); err != nil {
		result.Warning = fmt.Sprintf("the repository took the push, but the checkout's note of where the repository is (the tracking ref %s) "+
			"could not be moved, so the commits are counted as waiting until the next pull moves it: %v", remotePrefix+branch, err)
	}
	return result, nil
}

// try runs a command that says no with exit status 1, as rev-parse --verify --quiet does, and returns what it
// wrote, and whether it said yes.
func (p *Pusher) try(ctx context.Context, args ...string) (string, bool, error) {
	out, err := p.git.Run(ctx, args...)
	if err != nil {
		if gitErr, ok := errors.AsType[*git.Error](err); ok && gitErr.ExitCode == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(out.Stdout)), true, nil
}

// waiting counts the commits of the branch that the last fetched tip does not have, or all of them when there is
// no such tip.
func (p *Pusher) waiting(ctx context.Context, known string, has bool, local string) (int, error) {
	args := []string{"rev-list", "--count", local}
	if has {
		args = []string{"rev-list", "--count", known + ".." + local}
	}
	out, err := p.git.Run(ctx, args...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out.Stdout)))
	if err != nil {
		return 0, fmt.Errorf("git counted %q", strings.TrimSpace(string(out.Stdout)))
	}
	return n, nil
}

// isAncestor says whether the commit is one that the branch has, which is what makes a push of the branch a
// fast-forward.
func (p *Pusher) isAncestor(ctx context.Context, commit, branch string) (bool, error) {
	_, found, err := p.try(ctx, "merge-base", "--is-ancestor", commit, branch)
	return found, err
}

// moveTrackingRef records that the repository is at the commit, as a push to a named remote does. It is a write, so
// it checks the writer lock again, and does nothing without it. It is not given up by a stop that came in the middle
// of the push that it follows.
func (p *Pusher) moveTrackingRef(ctx context.Context, commit string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateTimeout)
	defer cancel()
	if p.o.Fence != nil {
		if err := p.o.Fence(ctx); err != nil {
			return fmt.Errorf("the writer lock could not be checked: %w", err)
		}
	}
	_, err := p.git.Run(ctx, "update-ref", remotePrefix+p.o.Branch, commit)
	return err
}

// pushedFlag says whether the flag of a ref in the output of `git push --porcelain` is one of a push that worked: a
// new branch, a fast-forward, or a branch that the repository had already. An update by force is not, because
// the agent never asks for one.
func pushedFlag(flag byte) bool { return strings.ContainsRune("* =", rune(flag)) }

// fail is the error of a push that did not get to the host. A Git directory that holds more than the agent put
// there has the reason of the checkout and the pull.
func (p *Pusher) fail(err error, format string, args ...any) *Error {
	if refusal, ok := checkout.Refusal(err); ok {
		return &Error{Reason: refusal.Reason, Message: refusal.Message, Err: refusal.Err}
	}
	return &Error{Reason: ReasonPushFailed, Message: fmt.Sprintf(format, args...), Err: err}
}

// refStatus is what `git push --porcelain` said of one ref.
type refStatus struct {
	found bool
	// flag is the first column: '*' a new branch, ' ' a fast-forward, '+' a forced update, '-' a deletion,
	// '=' up to date and '!' rejected.
	flag byte
	// summary is the third column, and reason what is in parentheses at the end of it.
	summary, reason string
}

// parsePorcelain finds the line of a ref in the standard output of `git push --porcelain`: a flag, a tab, the
// refspec, a tab and a summary. A line that has not that shape is not read.
func parsePorcelain(stdout, ref string) refStatus {
	for line := range strings.SplitSeq(stdout, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || len(fields[0]) != 1 {
			continue
		}
		if _, to, ok := strings.Cut(fields[1], ":"); !ok || to != ref {
			continue
		}
		status := refStatus{found: true, flag: fields[0][0], summary: strings.TrimSpace(fields[2])}
		if strings.HasSuffix(status.summary, ")") {
			if open := strings.LastIndex(status.summary, "("); open >= 0 {
				status.reason = status.summary[open+1 : len(status.summary)-1]
			}
		}
		return status
	}
	return refStatus{}
}

// rejection says why a ref was rejected. Only what Git words itself decides the reason: the repository having
// moved is one, and every other rejection is the decision of the host, whose words are kept.
func rejection(status refStatus, stderr, branch string) (reason, message string) {
	if strings.HasPrefix(status.summary, "[rejected]") && (status.reason == "fetch first" || status.reason == "non-fast-forward") {
		return ReasonNonFastForward, fmt.Sprintf("the branch %s of the repository has commits that the checkout does not have, so the push is "+
			"not a fast-forward. The agent does not force it", branch)
	}
	message = fmt.Sprintf("the repository refused the push of the branch %s", branch)
	if status.reason != "" {
		message += " (" + status.reason + ")"
	} else if status.summary != "" {
		message += " " + status.summary
	}
	if said := hostText(stderr); said != "" {
		message += ". It said: " + said
	}
	return ReasonPushRejected, message
}

// hostText is what the host said, which Git writes to standard error behind "remote:": the lines in order, in
// one line, with the spaces collapsed, the credentials of any URL removed and at most maxHostText bytes. What
// Git itself writes there, such as the hints, is left out.
func hostText(stderr string) string {
	var said []string
	for line := range strings.SplitSeq(stderr, "\n") {
		text, ok := strings.CutPrefix(line, "remote:")
		if !ok {
			continue
		}
		if text = strings.Join(strings.Fields(text), " "); text != "" {
			said = append(said, text)
		}
	}
	text := git.Redact(strings.Join(said, " "))
	if len(text) > maxHostText {
		text = strings.ToValidUTF8(text[:maxHostText], "")
	}
	return text
}
