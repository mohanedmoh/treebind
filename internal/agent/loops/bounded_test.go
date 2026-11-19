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

package loops

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
)

// The loops that the agent cannot tell from the application's own work, or from a host that gives each commit back changed, are
// bounded by the breaker (docs/adr/0035): each loop is made to happen without it, and is then held to the limit of commits in
// an hour with it, and goes on when the hour has passed.

// hours is a clock that a test moves.
type hours struct{ now time.Time }

func (h *hours) Now() time.Time          { return h.now }
func (h *hours) advance(d time.Duration) { h.now = h.now.Add(d) }

// answering is the application of a node: when the file in has a number, the file out has the number after it.
func answering(t *testing.T, n *node, in, out string) {
	t.Helper()
	got, err := strconv.Atoi(strings.TrimSpace(gittest.Read(t, n.l, in)))
	if err != nil {
		t.Fatalf("%s: %s holds %q: %v", n.name, in, gittest.Read(t, n.l, in), err)
	}
	if want := fmt.Sprintf("%d\n", got+1); gittest.Read(t, n.l, out) != want {
		n.write(out, want)
	}
}

// pingPong makes two Treebinds whose applications answer each other, for rounds minutes, with the breaker of the limit, or none
// when the limit is zero. A round is a pull, the answer of the application, a commit and a push, one Treebind after the other.
func pingPong(t *testing.T, limit, rounds int) (a, b *node, clock *hours) {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, map[string]string{"ping.txt": "0\n", "pong.txt": "0\n"})
	a, b = newNode(t, f, "A", subPath, pull.FollowRemote), newNode(t, f, "B", subPath, pull.FollowRemote)
	clock = &hours{now: time.Unix(1_800_000_000, 0)}
	if limit > 0 {
		a.bounded(limit, clock.Now)
		b.bounded(limit, clock.Now)
	}
	a.write("ping.txt", "1\n")
	for range rounds {
		a.pullOutcome()
		answering(t, a, "ping.txt", "pong.txt")
		a.commit()
		a.push()
		b.pullOutcome()
		answering(t, b, "pong.txt", "ping.txt")
		b.commit()
		b.push()
		clock.advance(time.Minute)
	}
	return a, b, clock
}

func TestTwoApplicationsThatAnswerEachOthersFilesMakeACommitAtEveryTurnWithoutTheBreaker(t *testing.T) {
	a, b, _ := pingPong(t, 0, 24)

	// The loop is real: nothing in it is a mistake of the agent, and nothing in it stops.
	if a.commits+b.commits < 30 {
		t.Errorf("the Treebinds made %d and %d commits in 24 rounds, want a loop of commits: about one at every turn", a.commits, b.commits)
	}
}

func TestTwoApplicationsThatAnswerEachOthersFilesAreHeldToTheLimitOfTheHourAndGoOnWhenItHasPassed(t *testing.T) {
	const limit = 5
	a, b, clock := pingPong(t, limit, 24)

	if a.commits > limit || b.commits > limit || a.commits+b.commits < limit {
		t.Errorf("A made %d commits and B %d in 24 minutes, want at most the limit of %d each, and the limit reached by the one that held", a.commits, b.commits, limit)
	}
	// The Treebind that is held does not publish the file that the other would answer, so the other has nothing to answer and
	// does not reach the limit: the loop is held as a whole.
	if a.held+b.held == 0 {
		t.Error("the breaker held no commit, want the loop held: the applications still write when the limit is reached")
	}

	// The changes that were held are still on the volume, and are committed when an hour has passed since the commits.
	clock.advance(2 * time.Hour)
	before := a.commits + b.commits
	a.pullOutcome()
	a.commit()
	b.pullOutcome()
	b.commit()

	if a.commits+b.commits == before {
		t.Error("no commit was made an hour after the limit was reached: the changes that were held are not committed")
	}
	if a.commits > limit+1 || b.commits > limit+1 {
		t.Errorf("A has made %d commits and B %d, want the limit to hold in the hour that follows: the one commit that the hour allows", a.commits, b.commits)
	}
}

func TestAHostThatGivesBackEachCommitWithAnotherFileIsHeldToTheLimitOfTheHourInTheResolutions(t *testing.T) {
	// The files are not the same after the host has rewritten a commit, so the resolution is a commit, and it is made at every
	// pull for as long as the host does this. Only the breaker ends that: the trees are never the same.
	run := func(t *testing.T, limit int) (*node, *hours) {
		t.Helper()
		f := gittest.New(t)
		f.Publish(branchMain, map[string]string{"a.txt": "a\n"})
		f.RewriteWhatIsPushed(true)
		n := newNode(t, f, "A", subPath, pull.FollowLocal)
		clock := &hours{now: time.Unix(1_800_000_000, 0)}
		if limit > 0 {
			n.bounded(limit, clock.Now)
		}
		n.write("a.txt", "a, changed\n")
		for range 16 {
			n.commit()
			n.push()
			n.pullOutcome()
			clock.advance(time.Minute)
		}
		return n, clock
	}

	t.Run("without the breaker", func(t *testing.T) {
		n, _ := run(t, 0)
		if n.resolved < 12 {
			t.Errorf("the host's rewrites were resolved %d times in 16 pulls, want a loop: one at almost every pull", n.resolved)
		}
	})

	t.Run("with the breaker", func(t *testing.T) {
		const limit = 4
		n, clock := run(t, limit)

		if n.commits+n.resolved > limit {
			t.Errorf("the agent made %d commits and %d resolutions in 16 minutes, want %d at most together: the limit counts both", n.commits, n.resolved, limit)
		}
		if n.held == 0 {
			t.Error("the breaker held no resolution, want the loop held")
		}
		clock.advance(2 * time.Hour)
		before := n.resolved
		n.pullOutcome()
		if n.resolved == before {
			t.Error("no resolution was made an hour later: what was held is not resolved when the hour allows it")
		}
	})
}
