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

// Package breaker bounds the commits that the agent makes in an hour (R-18, docs/adr/0035). A loop that the agent cannot
// tell from the application's own work, an application that answers what a pull brought and a Treebind that answers it,
// makes a commit at every turn; the agent does not know that it is a loop, and can only stop making commits for as long as
// there have been too many.
//
// The commits are counted in a small file of the state directory, with the time of each, and not read from the history of
// the branch: a host that rewrites what it is sent, and a resolution that moves the branch off the commits of the checkout,
// take commits out of the history, and the refs that keep them are few. A file also survives the restart of the pod, so
// that a restart is not a new budget for the loop.
//
// Asking is a read and writes nothing, so that a commit that is held costs the volume nothing at every scan. Telling that
// a commit was made is the one write: it is made after the commit is in the branch, behind the writer-lock fence, to a file
// that is written aside and renamed, so that a kill leaves the old file or the new one. A kill between the commit and the
// write leaves one commit that is not counted, and the limit is exceeded by that one at most, for each kill.
package breaker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// ReasonTooManyCommits is the reason of a step that is not made because the agent has made as many commits in the last
	// hour as it may. It is API in the sense of docs/adr/0004.
	ReasonTooManyCommits = "TooManyCommits"

	// DefaultLimit is how many commits the agent makes in an hour when the Treebind does not say, and MinLimit and MaxLimit
	// are the least and the most that spec.commit.maxPerHour admits. A test holds them to what the API server does.
	DefaultLimit = 60
	MinLimit     = 1
	MaxLimit     = 10000

	// Window is how long a commit counts for.
	Window = time.Hour

	// journalFile is the file of the state directory that holds the times of the commits, one in seconds since the epoch on
	// each line. A file of this name with ".tmp" is where the new one is written before it is put in its place.
	journalFile = "commit-times"

	// maxJournalBytes is the largest file that is read. The most it holds is a line for each of the commits that the limit
	// allows, which is far under this, and a file that is larger is not one that the agent wrote.
	maxJournalBytes = 1 << 20
)

// HeldError is a commit that is not made because there have been too many: Count commits in the last hour, which is the
// Limit. It is the error of Allow.
type HeldError struct {
	// Count is how many commits of the last hour there are, and Limit is how many there may be.
	Count, Limit int
	// Until is the time at which one of them is an hour old and a commit is allowed again. It is for tests and for logs of
	// the agent that are not the check: the message has no time, so that a hold that is the same is not a new line of a log.
	Until time.Time
}

// Error says what is held and what waits, without a time.
func (e *HeldError) Error() string {
	return fmt.Sprintf("the agent made %s in the last hour and its limit is %d (spec.commit.maxPerHour), so no commit is made until "+
		"enough of them are more than an hour old; the changes stay on the volume. A loop, an application that answers what a pull "+
		"brings or two Treebinds that undo each other's files, is what this is for, and the limit can be raised when the "+
		"commits are expected", plural(e.Count, "commit", "commits"), e.Limit)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// Gate is what a step that makes commits asks before it writes anything, and tells when it has made one. The Breaker is
// one.
type Gate interface {
	// Allow returns nil when a commit may be made, a *HeldError when it may not, and another error when the commits cannot
	// be counted.
	Allow(ctx context.Context) error
	// Made says that a commit was made, now.
	Made(ctx context.Context) error
}

var _ Gate = (*Breaker)(nil)

// Refusing is a Gate for an agent that could not make its Breaker: it allows no commit, and says why with the error that it was
// given. What cannot be bounded is not run without a bound, and the step that asks fails with the cause, as it does for a file
// of times that cannot be read. No commit is made, so there is none to tell of.
func Refusing(err error) Gate { return refusing{err} }

type refusing struct{ err error }

func (r refusing) Allow(context.Context) error { return r.err }
func (refusing) Made(context.Context) error    { return nil }

// CountTimeout is how long telling the gate of a commit may take after a stop came in the middle of the step that made it: a short
// time is all it needs.
const CountTimeout = 5 * time.Second

// Tell says to the gate that a commit was made, and returns what to warn of when that did not work, in words that are the same for
// every step that makes commits. The commit is in the branch whether it is counted or not, so a stop that came in the middle of
// the step does not cancel this, and it has CountTimeout. With no gate there is nothing to tell.
func Tell(ctx context.Context, gate Gate) string {
	if gate == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CountTimeout)
	defer cancel()
	if err := gate.Made(ctx); err != nil {
		return fmt.Sprintf("the commit was not counted against the limit of commits in an hour, so the limit may be exceeded by it: %v", err)
	}
	return ""
}

// Options say where the commits are counted and how many there may be.
type Options struct {
	// Dir is the state directory of the agent, which holds the file.
	Dir string
	// Limit is how many commits there may be in an hour. DefaultLimit when it is zero.
	Limit int
	// Now is the clock. time.Now when it is nil. The times in the file are of this clock, and are kept across restarts, so
	// it is a clock of the wall and not one that only goes forward.
	Now func() time.Time
	// Fence is called immediately before the file is written, and nothing is written when it returns an error. It is the
	// check that the agent still holds the writer lock (docs/adr/0023). With none, nothing is checked.
	Fence func(ctx context.Context) error
}

// Breaker counts the commits of one agent. It is not safe for concurrent use: the agent makes its commits one at a time, in
// one loop.
type Breaker struct {
	o Options
	// rename puts the file that was written aside in its place. It is os.Rename, and a test gives another to fail.
	rename func(oldpath, newpath string) error
}

// New returns a Breaker. It refuses what cannot work: no directory, and a limit that is not one that the API admits.
func New(o Options) (*Breaker, error) {
	if o.Dir == "" {
		return nil, errors.New("breaker: the state directory is not set")
	}
	if o.Limit == 0 {
		o.Limit = DefaultLimit
	}
	if o.Limit < MinLimit || o.Limit > MaxLimit {
		return nil, fmt.Errorf("breaker: the limit of commits in an hour is %d, want from %d to %d", o.Limit, MinLimit, MaxLimit)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Breaker{o: o, rename: os.Rename}, nil
}

// Limit is how many commits there may be in an hour.
func (b *Breaker) Limit() int { return b.o.Limit }

// Allow says whether a commit may be made now. It only reads. A file that cannot be read is an error that is not a hold: what
// the agent cannot count it does not guess, and the step that asked fails with the words of the volume.
func (b *Breaker) Allow(context.Context) error {
	times, err := b.read(b.o.Now())
	if err != nil {
		return err
	}
	if len(times) < b.o.Limit {
		return nil
	}
	// Held until enough of them are an hour old that there are fewer than the limit: the oldest ones, in order.
	return &HeldError{Count: len(times), Limit: b.o.Limit, Until: times[len(times)-b.o.Limit].Add(Window)}
}

// Made says that a commit was made now. It checks the writer lock, and writes nothing without it. The commit stands whatever
// this says: it is an error that tells the caller that the commit was not counted.
func (b *Breaker) Made(ctx context.Context) error {
	now := b.o.Now()
	times, err := b.read(now)
	if err != nil {
		return err
	}
	if b.o.Fence != nil {
		if err := b.o.Fence(ctx); err != nil {
			return fmt.Errorf("the commit was not counted, because the writer lock could not be checked: %w", err)
		}
	}
	return b.write(append(times, now))
}

// path is where the file is.
func (b *Breaker) path() string { return filepath.Join(b.o.Dir, journalFile) }

// read returns the times of the commits that count at now, oldest first: those that are less than an hour old, and those
// that are ahead of the clock by less than an hour, which a clock that was set back makes. A time that is further ahead is
// not one that was written by this agent, and a line that is not a time is skipped. A file that is not there is no commits.
func (b *Breaker) read(now time.Time) ([]time.Time, error) {
	f, err := os.Open(b.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the times of the commits cannot be read: %w", err)
	}
	defer func() { _ = f.Close() }()
	// A file that is larger than the agent ever writes is not read in part: the start of it would be counted as the whole.
	if info, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("the times of the commits cannot be read: %w", err)
	} else if info.Size() > maxJournalBytes {
		return nil, fmt.Errorf("the file of the times of the commits is larger than the agent writes (%d bytes, at most %d): it is not one that "+
			"the agent wrote, and the commits cannot be counted from it", info.Size(), maxJournalBytes)
	}
	var times []time.Time
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		seconds, err := strconv.ParseInt(strings.TrimSpace(scanner.Text()), 10, 64)
		if err != nil {
			continue
		}
		at := time.Unix(seconds, 0)
		if age := now.Sub(at); age < Window && age > -Window {
			times = append(times, at)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("the times of the commits cannot be read: %w", err)
	}
	slices.SortFunc(times, func(a, c time.Time) int { return a.Compare(c) })
	return times, nil
}

// write puts the times in the file: it is written aside and renamed, so that the file is the old one or the new one.
func (b *Breaker) write(times []time.Time) (err error) {
	var out strings.Builder
	slices.SortFunc(times, func(a, c time.Time) int { return a.Compare(c) })
	for _, at := range times {
		out.WriteString(strconv.FormatInt(at.Unix(), 10))
		out.WriteByte('\n')
	}
	aside := b.path() + ".tmp"
	// A file that a killed agent left is replaced, and is never read.
	f, err := os.OpenFile(aside, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("the times of the commits cannot be written: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(aside)
		}
	}()
	if _, err = f.WriteString(out.String()); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("the times of the commits cannot be written: %w", err)
	}
	if err = b.rename(aside, b.path()); err != nil {
		return fmt.Errorf("the times of the commits cannot be written: %w", err)
	}
	return nil
}
