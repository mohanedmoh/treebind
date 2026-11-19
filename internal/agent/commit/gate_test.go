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

package commit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// A commit is bounded by the breaker (docs/adr/0035): it asks before anything is written, and says that a commit was made
// once it is in the branch.

// gate is a breaker that answers as the test says and notes what it was asked.
type gate struct {
	allow, made error
	asked, told int
	// atTold is called when the agent says that a commit was made, with the context of the call.
	atTold func(ctx context.Context)
}

func (g *gate) Allow(context.Context) error {
	g.asked++
	return g.allow
}

func (g *gate) Made(ctx context.Context) error {
	g.told++
	if g.atTold != nil {
		g.atTold(ctx)
	}
	return g.made
}

func gated(g *gate) func(*Options) { return func(o *Options) { o.Gate = g } }

func TestACommitThatTheBreakerHoldsIsNotMadeAndWritesNothing(t *testing.T) {
	held := &breaker.HeldError{Count: 3, Limit: 3, Until: time.Unix(1_800_000_000, 0)}
	g := &gate{allow: held}
	w := newWorld(t, "", base, gated(g))
	w.write("left.txt", "staged\n")
	w.git("add", "--", "left.txt")
	w.write(fileNew, "new\n")
	before := w.head()

	_, err := w.committer.Commit(context.Background(), w.changes())

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != breaker.ReasonTooManyCommits || !errors.Is(err, held) {
		t.Fatalf("error = %v, want a TooManyCommits that wraps what the breaker said", err)
	}
	if failure.Message != held.Error() {
		t.Errorf("message = %q, want the words of the breaker: %q", failure.Message, held.Error())
	}
	if w.head() != before {
		t.Error("a commit was made although the breaker holds")
	}
	if staged := w.staged(); !slices.Equal(staged, []string{"left.txt"}) {
		t.Errorf("the index holds %q, want what it held before: a commit that is held writes nothing, not even a reset", staged)
	}
	if n := w.fenced.Load(); n != 0 {
		t.Errorf("the writer lock was checked %d times for a commit that is held, want none: nothing is written", n)
	}
	if _, err := os.Lstat(filepath.Join(w.l.StateDir, listFile)); err == nil {
		t.Error("the file of paths is left in the state directory")
	}
	if g.told != 0 {
		t.Errorf("the breaker was told of %d commits, want none", g.told)
	}

	// The files are still changes, and the next commit, when the breaker lets it, makes the commit of both.
	g.allow = nil
	if got := w.commit(); got.Outcome != Committed || got.Files != 2 {
		t.Errorf("the commit after the hold = %+v, want the two files committed", got)
	}
}

func TestTheBreakerIsAskedOnlyWhenThereIsSomethingToCommit(t *testing.T) {
	g := &gate{allow: &breaker.HeldError{Count: 1, Limit: 1}}
	w := newWorld(t, "", base, gated(g))

	if got, err := w.committer.Commit(context.Background(), nil); err != nil || got.Outcome != NothingToCommit {
		t.Errorf("Commit(nil) = %+v, %v, want NothingToCommit, even though the breaker holds", got, err)
	}
	w.write(fileNew, "new\n")
	listed := w.changes()
	if err := os.Remove(w.path(fileNew)); err != nil {
		t.Fatal(err)
	}
	if got, err := w.committer.Commit(context.Background(), listed); err != nil || got.Outcome != NothingToCommit || got.Gone != 1 {
		t.Errorf("Commit of a file that is gone = %+v, %v, want NothingToCommit", got, err)
	}
	if g.asked != 0 {
		t.Errorf("the breaker was asked %d times, want none: there was nothing to commit, so a hold has nothing to say", g.asked)
	}
}

func TestACommitThatWasMadeIsToldToTheBreakerOnceAfterItIsInTheBranchWithAContextThatAStopDoesNotCancel(t *testing.T) {
	g := &gate{}
	w := newWorld(t, "", base, gated(g))
	w.write(fileNew, "new\n")
	before := w.head()
	var headWhenTold string
	var deadline bool
	g.atTold = func(ctx context.Context) {
		headWhenTold = w.head()
		_, deadline = ctx.Deadline()
		if ctx.Err() != nil {
			t.Errorf("the context that the breaker is told with is done: %v", ctx.Err())
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	got, err := w.committer.Commit(ctx, w.changes())

	if err != nil || got.Outcome != Committed || got.Warning != "" {
		t.Fatalf("result = %+v, %v, want a commit with nothing to warn of", got, err)
	}
	if g.asked != 1 || g.told != 1 {
		t.Errorf("the breaker was asked %d times and told %d times, want once each", g.asked, g.told)
	}
	if headWhenTold == before || headWhenTold != got.Commit {
		t.Errorf("HEAD was %s when the breaker was told, want the new commit %s: it is told once the commit is in the branch", headWhenTold, got.Commit)
	}
	if !deadline {
		t.Error("the context that the breaker is told with has no time limit: it is the context of the commit, which a stop cancels, and the commit would not be counted")
	}
}

func TestACommitThatCameToNothingOrFailedIsNotToldToTheBreaker(t *testing.T) {
	t.Run("a file that was changed back", func(t *testing.T) {
		g := &gate{}
		w := newWorld(t, "", base, gated(g))
		w.write(fileA, "changed\n")
		listed := w.changes()
		w.write(fileA, base[fileA])

		got, err := w.committer.Commit(context.Background(), listed)

		if err != nil || got.Outcome != NothingToCommit {
			t.Fatalf("result = %+v, %v, want NothingToCommit", got, err)
		}
		if g.told != 0 {
			t.Errorf("the breaker was told of %d commits, want none: nothing was committed", g.told)
		}
	})
	t.Run("a commit that Git refused", func(t *testing.T) {
		g := &gate{}
		w := newWorld(t, "", base, gated(g))
		w.write(fileNew, "new\n")
		if err := os.WriteFile(filepath.Join(w.l.GitDir, "refs", "heads", branchMain+".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := w.committer.Commit(context.Background(), w.changes()); err == nil {
			t.Fatal("the commit worked, want it refused by the lock of the branch")
		}
		if g.told != 0 {
			t.Errorf("the breaker was told of %d commits, want none: nothing was committed", g.told)
		}
	})
}

func TestACommitThatCouldNotBeCountedStandsAndSaysSo(t *testing.T) {
	g := &gate{made: errors.New("the file of times is on a volume that is full")}
	w := newWorld(t, "", base, gated(g))
	w.write(fileNew, "new\n")

	got, err := w.committer.Commit(context.Background(), w.changes())

	if err != nil || got.Outcome != Committed || got.Commit == "" || got.Commit != w.head() {
		t.Fatalf("result = %+v, %v, want the commit that was made to stand", got, err)
	}
	for _, want := range []string{"not counted", "volume that is full"} {
		if !strings.Contains(got.Warning, want) {
			t.Errorf("warning = %q, want it to have %q", got.Warning, want)
		}
	}
}

func TestABreakerThatCannotCountIsAFailedCommitAndNotAHold(t *testing.T) {
	g := &gate{allow: errors.New("the times of the commits cannot be read: permission denied")}
	w := newWorld(t, "", base, gated(g))
	w.write(fileNew, "new\n")
	before := w.head()

	_, err := w.committer.Commit(context.Background(), w.changes())

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonCommitFailed || !errors.Is(err, g.allow) {
		t.Fatalf("error = %v, want a CommitFailed that wraps what the breaker said", err)
	}
	if !strings.Contains(failure.Message, "permission denied") || !strings.Contains(failure.Message, "cannot be counted") {
		t.Errorf("message = %q, want what is wrong and that the commits cannot be counted", failure.Message)
	}
	if w.head() != before || w.fenced.Load() != 0 {
		t.Error("something was written although the commits could not be counted")
	}
}

// The marker that a CI reads to leave a commit alone is a part of the template, as it is for a person (docs/echo-loops.md):
// there is no field for it, and the commit has it as it is written.
func TestATemplateMayCarryTheMarkerOfACIThatSkipsTheCommit(t *testing.T) {
	for _, marker := range []string{"[skip ci]", "[ci skip]", "[skip actions]"} {
		t.Run(marker, func(t *testing.T) {
			template := "treebind: sync {{.Count}} file(s) " + marker
			w := newWorld(t, "", base, func(o *Options) { o.MessageTemplate = template })
			w.write(fileNew, "new\n")

			w.commit()

			if got, want := w.git("log", "-1", "--format=%B"), "treebind: sync 1 file(s) "+marker; got != want {
				t.Errorf("the message is %q, want %q", got, want)
			}
		})
	}
}

// A stand-in for git that runs what it is asked, and then, at the subcommand that the step is, is the end of what Git can do: the
// command has run, and every command from then on fails without running, so that nothing the agent would do to clean up after
// itself hides what is left. With lost the command reports a failure as well, which is what an agent that is killed just after
// it is told, and without it the command reports what it did.
func killedAfter(t *testing.T, sub string, lost bool) (binary string, dead func() bool) {
	t.Helper()
	dir := gittest.TempDir(t)
	binary = filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
D='%s'
if [ -e "$D/dead" ]; then echo 'the agent was killed' >&2; exit 137; fi
hit=
for arg in "$@"; do
  if [ "$arg" = '%s' ]; then hit=yes; fi
done
'%s' "$@"
rc=$?
if [ -n "$hit" ]; then : > "$D/dead"; if [ -n '%s' ]; then exit 137; fi; fi
exit $rc
`, dir, sub, gittest.Binary(t), map[bool]string{true: "lost", false: ""}[lost])
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, func() bool { return gittest.Exists(filepath.Join(dir, "dead")) }
}

func TestACommitThatWasMadeIsCountedEvenWhenGitFailsAfterItAndTheIdCannotBeRead(t *testing.T) {
	// The commit is in the branch when the command that made it has run, and what Git does after it cannot take it out.
	g := &gate{}
	binary, dead := killedAfter(t, "commit", false)
	w := newWorld(t, "", base, gated(g), func(o *Options) { o.Git.Git = binary })
	w.write(fileNew, "new\n")
	before := w.head()

	_, err := w.committer.Commit(context.Background(), w.changes())

	if err == nil || !dead() {
		t.Fatalf("Commit = %v, dead = %t, want it to fail after the commit was made", err, dead())
	}
	if w.head() == before {
		t.Fatal("HEAD did not move: the step is not the one that this test is for")
	}
	if g.told != 1 {
		t.Errorf("the breaker was told of %d commits, want 1: the commit is in the branch, and counting it does not need Git", g.told)
	}
}

func TestAKillAfterTheCommitLeavesItUncountedAndTheNextPassCountsTheNextOneRight(t *testing.T) {
	// The agent is killed just after the commit, before it can tell the breaker: the file does not have it, so that the limit
	// is exceeded by one at most, and the next commit is counted as it would be.
	dir := t.TempDir()
	clock := time.Unix(1_800_000_000, 0)
	counting := func() *breaker.Breaker {
		b, err := breaker.New(breaker.Options{Dir: dir, Limit: 2, Now: func() time.Time { return clock }})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	binary, dead := killedAfter(t, "commit", true)
	killedRun := newWorld(t, "", base, func(o *Options) { o.Git.Git, o.Gate = binary, counting() })
	killedRun.write(fileNew, "new\n")
	listed := killedRun.changes()
	// The breaker is told after the command that made the commit, and the stand-in ends the agent at that command.
	if _, err := killedRun.committer.Commit(context.Background(), listed); err == nil || !dead() {
		t.Fatalf("Commit = %v, dead = %t, want the agent killed at the commit", err, dead())
	}
	if killedRun.head() == "" || len(killedRun.staged()) != 0 {
		t.Errorf("HEAD = %q and the index holds %q, want the commit made and nothing staged", killedRun.head(), killedRun.staged())
	}
	if changes := killedRun.changes(); len(changes) != 0 {
		t.Errorf("the scan lists %v after the kill, want nothing: the commit holds the file", changes)
	}
	if _, err := os.Stat(filepath.Join(dir, "commit-times")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file of the breaker is there after a kill that came before it was told: %v", err)
	}

	// The next pass is a new agent, with Git that works, and a breaker that reads the same file.
	fresh, err := New(Options{
		Layout: killedRun.l, Git: killedRun.f.GitOptions(), AuthorName: botName, AuthorEmail: botEmail, Gate: counting(),
	})
	if err != nil {
		t.Fatal(err)
	}
	killedRun.write(fileB, "b, again\n")
	got, err := fresh.Commit(context.Background(), killedRun.changes())
	if err != nil || got.Outcome != Committed || got.Warning != "" {
		t.Fatalf("the commit of the next pass = %+v, %v, want it made and counted", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "commit-times")); err != nil || strings.Count(string(data), "\n") != 1 {
		t.Errorf("the file of the breaker holds %q, %v, want one time: the commit that was killed was not counted, and this one is", data, err)
	}
}
