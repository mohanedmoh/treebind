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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/breaker"
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
