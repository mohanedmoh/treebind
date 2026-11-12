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

// Package preflight checks, once at start, that the volume the agent writes to
// can hold a checkout.
//
// A check never refuses a setup that works. One that cannot tell is skipped, and
// that is not a failure.
//
// The reasons of the results are the ones the status poller will use as the
// reasons of conditions, so they stay stable.
package preflight

import (
	"context"
	"errors"
	"time"
)

// Timeout is how long the checks may take altogether. A volume that does not
// answer in that time, a network file system that is down for one, fails the
// check with TimedOut instead of leaving the agent hanging. Volume applies it.
const Timeout = 30 * time.Second

// Outcome says how a check ended.
type Outcome string

const (
	// Passed means the check ran and found nothing wrong.
	Passed Outcome = "Passed"
	// Failed means the check ran and found a problem. Reason and Message say
	// which.
	Failed Outcome = "Failed"
	// Skipped means the check could not tell, because it could not run or one it
	// depends on failed. It is not a failure.
	Skipped Outcome = "Skipped"
)

// The names of the checks, in the order they run.
const (
	CheckWritable = "Writable"
)

// The reasons of a result that did not pass. Each distinct cause has its own, and
// a reason is never reused for another cause.
const (
	// ReasonNotWritable means the agent's user lacks the permission to create a
	// file in the directory.
	ReasonNotWritable = "NotWritable"
	// ReasonReadOnlyVolume means the volume refuses writes.
	ReasonReadOnlyVolume = "ReadOnlyVolume"
	// ReasonWriteFailed means writing the test file failed for another reason,
	// such as a full volume or an I/O error.
	ReasonWriteFailed = "WriteFailed"
	// ReasonTimedOut means the volume did not answer in time.
	ReasonTimedOut = "TimedOut"
	// ReasonNotChecked means the check was skipped. The message says why.
	ReasonNotChecked = "NotChecked"
)

// Result is the outcome of one check.
type Result struct {
	// Check is the name of the check.
	Check string
	// Outcome says whether the check passed, failed or was skipped.
	Outcome Outcome
	// Reason is empty when the check passed. Otherwise it is one of the reasons
	// above.
	Reason string
	// Message tells a reader what was found and what to do about it. It holds no
	// credential and names no storage server.
	Message string
}

// Volume runs the checks on the directory where the agent writes, in order, and
// returns one result for each. The checks share Timeout, whatever the context
// allows.
func Volume(ctx context.Context, path string) []Result {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	return []Result{run(ctx, CheckWritable, func() Result { return checkWritable(path) })}
}

// run executes a check and gives up on it when the context ends. A call that
// hangs on a dead network file system cannot be cancelled, so it keeps its
// goroutine until the call returns or the process exits, and nothing waits for it.
func run(ctx context.Context, check string, fn func() Result) Result {
	done := make(chan Result, 1)
	go func() { done <- fn() }()
	return await(ctx, check, done)
}

// await returns the result of a check, or the outcome of the context ending
// first. A check that finished at the moment the context ended keeps its result.
func await(ctx context.Context, check string, done <-chan Result) Result {
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
	}

	select {
	case result := <-done:
		return result
	default:
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{
			Check:   check,
			Outcome: Failed,
			Reason:  ReasonTimedOut,
			Message: "the volume did not answer in time. A network file system whose server is down or " +
				"unreachable from the node looks like this; check that the storage is reachable",
		}
	}
	return Result{Check: check, Outcome: Skipped, Reason: ReasonNotChecked, Message: "the check was interrupted"}
}
