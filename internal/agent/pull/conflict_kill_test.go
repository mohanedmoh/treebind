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

package pull

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// An agent that is killed at any step of a resolution leaves a checkout that the next pull puts right and resolves
// (docs/adr/0028, docs/adr/0034). The step is made to happen on cue: a stand-in for git runs the command that the
// step is, and from there on every command fails without running, so that nothing the agent would have done to
// clean up after itself can hide what a kill leaves. The checkout, the index, the journal, the repair and the pull
// that follows are real, on a real repository.

const (
	subReadTree    = "read-tree"
	subUpdateIndex = "update-index"
	fileLocal      = "c.json"
	again          = "b, once more\n"
)

// trap is a stand-in for git that does something when it is asked to run a subcommand.
type trap struct {
	// sub is the subcommand, and nth which run of it is the trap, counting from one.
	sub string
	nth int
	// after runs the command first, and then what run says. Otherwise run is first.
	after bool
	// run is what a shell runs at the trap. It has what a shell has built in, because the image of the agent has
	// little else, and it runs git by the path that is given to it.
	run string
	// fails makes the command fail, without running it, and without the agent being killed: the later commands run.
	fails bool
	// dies means that the agent is killed at the trap: the command is not run when the trap is before it, and
	// reports a failure when it is after it, and every command from then on fails without running.
	dies bool
}

// trapped is the stand-in for git and the directory where it keeps what it knows.
type trapped struct {
	binary, dir string
}

// killed reports whether the agent was killed at the trap.
func (g trapped) killed() bool { return gittest.Exists(filepath.Join(g.dir, "dead")) }

func makeTrap(t *testing.T, tr trap) trapped {
	t.Helper()
	dir := gittest.TempDir(t)
	binary := filepath.Join(dir, "git")
	real := gittest.Binary(t)
	run := tr.run
	if run == "" {
		run = ":"
	}
	var at string
	switch {
	case tr.fails:
		at = `echo 'broken on purpose' >&2; exit 3`
	case tr.after && tr.dies:
		at = fmt.Sprintf("'%s' \"$@\"; : > \"$D/dead\"; exit 137", real)
	case tr.after:
		at = fmt.Sprintf("'%s' \"$@\"; rc=$?; %s; exit $rc", real, run)
	case tr.dies:
		at = run + `; : > "$D/dead"; exit 137`
	default:
		at = run
	}
	script := fmt.Sprintf(`#!/bin/sh
D='%s'
if [ -e "$D/dead" ]; then echo 'the agent was killed' >&2; exit 137; fi
hit=
for arg in "$@"; do
  if [ "$arg" = '%s' ]; then hit=yes; fi
done
if [ -n "$hit" ]; then
  n=0
  if [ -e "$D/count" ]; then read n < "$D/count"; fi
  n=$((n + 1))
  echo $n > "$D/count"
  if [ "$n" = %d ]; then
    %s
  fi
fi
exec '%s' "$@"
`, dir, tr.sub, tr.nth, at, real)
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return trapped{binary: binary, dir: dir}
}

// moveOf is the subcommand that moves the checkout to the commit of a resolution.
func moveOf(policy Policy) string {
	if policy == FollowRemote {
		return "checkout"
	}
	return "merge"
}

// cutScenario is a checkout and a repository whose resolution builds a commit with both policies: the application
// changed a.json and made c.json, and the repository changed a.json and b.json.
type cutScenario struct {
	*world
	policy Policy
	local  string
	tip    string
	// final is what the directory holds when the resolution is done.
	final map[string]string
}

func newCutScenario(t *testing.T, policy Policy) *cutScenario {
	t.Helper()
	w := newWorld(t, subPath, resolving(policy))
	c := &cutScenario{world: w, policy: policy}
	c.local = w.local(t, map[string]string{fileA: mine, fileLocal: mine})
	c.tip = w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileB: bChanged})
	c.final = map[string]string{fileA: aChanged, fileB: bChanged, fileLocal: mine}
	if policy == FollowLocal {
		c.final[fileA] = mine
	}
	return c
}

// restart is what the next start of the agent does: it takes the locks that a killed Git left away and has a Puller
// of its own, with the real git.
func (c *cutScenario) restart(t *testing.T) {
	t.Helper()
	if _, err := git.RemoveStaleLocks(c.l.GitDir); err != nil {
		t.Fatal(err)
	}
	c.puller = puller(t, c.f, c.l, resolving(c.policy))
}

// wantResolvedAfter fails the test unless the checkout is as a resolution makes it, with a clean index, no journal and
// the refs that the policy keeps.
func (c *cutScenario) wantResolved(t *testing.T) {
	t.Helper()
	wantFiles(t, c.world, c.final)
	if dirty := c.dirty(t); dirty != "" {
		t.Errorf("the checkout has changes after the next pull: %s", dirty)
	}
	if !c.indexIsClean() {
		t.Error("the index holds files that are staged after the next pull")
	}
	if journalPresent(c.world) {
		t.Error("the journal is still there after the next pull")
	}
	if c.policy == FollowRemote {
		c.wantLocalRefs(t, ref{firstLocal, c.local})
	} else {
		c.wantLocalRefs(t)
	}
}

func TestAKillAtAnyStepOfAResolutionIsPutRightAndResolvedByTheNextPull(t *testing.T) {
	staged := "had files staged"
	steps := []struct {
		name string
		// only is the policy the step is of, and empty for both.
		only Policy
		trap func(policy Policy) trap
		// noted is what the next pull says it put right, for each policy, and empty when there was nothing.
		noted map[Policy]string
		// outcome is what the next pull does: it resolves, or it finds the checkout already resolved and ahead.
		outcome Outcome
	}{
		{"before the ref that keeps the local commits", FollowRemote,
			func(Policy) trap { return trap{sub: "update-ref", nth: 1, dies: true} }, nil, Resolved},
		{"after the ref that keeps the local commits", FollowRemote,
			func(Policy) trap { return trap{sub: "update-ref", nth: 1, after: true, dies: true} }, nil, Resolved},
		// With followLocal the tree that is read is the one of the checkout, so the index holds what it holds.
		{"after the index was read from the tree of the winner", "",
			func(Policy) trap { return trap{sub: subReadTree, nth: 1, after: true, dies: true} },
			map[Policy]string{FollowRemote: staged}, Resolved},
		{"after the files of the loser were put in the index", "",
			func(Policy) trap { return trap{sub: subUpdateIndex, nth: 1, after: true, dies: true} },
			map[Policy]string{FollowRemote: staged, FollowLocal: staged}, Resolved},
		{"after the tree was written", "",
			func(Policy) trap { return trap{sub: "write-tree", nth: 1, after: true, dies: true} },
			map[Policy]string{FollowRemote: staged, FollowLocal: staged}, Resolved},
		{"after the index was put back", "",
			func(Policy) trap { return trap{sub: subReadTree, nth: 2, after: true, dies: true} }, nil, Resolved},
		{"after the commit was made", "",
			func(Policy) trap { return trap{sub: "commit-tree", nth: 1, after: true, dies: true} }, nil, Resolved},
		{"before the move", "",
			func(p Policy) trap { return trap{sub: moveOf(p), nth: 1, dies: true} },
			map[Policy]string{FollowRemote: noteUn, FollowLocal: noteUn}, Resolved},
		{"after the move, before the journal is removed", "",
			func(p Policy) trap { return trap{sub: moveOf(p), nth: 1, after: true, dies: true} },
			map[Policy]string{FollowRemote: "had finished", FollowLocal: "had finished"}, Ahead},
	}
	for _, step := range steps {
		for _, policy := range []Policy{FollowRemote, FollowLocal} {
			if step.only != "" && step.only != policy {
				continue
			}
			t.Run(fmt.Sprintf("%s, %s", policy, step.name), func(t *testing.T) {
				c := newCutScenario(t, policy)
				remote := c.remoteRefs(t)
				stand := makeTrap(t, step.trap(policy))
				c.puller = puller(t, c.f, c.l, resolving(policy), func(o *Options) { o.Git.Git = stand.binary })

				_, err := c.pull(t)

				if err == nil || !stand.killed() {
					t.Fatalf("the pull that was killed = %v, killed = %v, want it to fail where the agent was killed", err, stand.killed())
				}
				c.restart(t)

				got, err := c.pull(t)

				if err != nil || got.Outcome != step.outcome {
					t.Fatalf("the next pull = %+v, %v, want %s", got, err, step.outcome)
				}
				noted := step.noted[policy]
				if noted == "" && got.Repaired != "" {
					t.Errorf("the next pull put right %q, want nothing to put right", got.Repaired)
				}
				if !strings.Contains(got.Repaired, noted) {
					t.Errorf("the next pull put right %q, want it to say %q", got.Repaired, noted)
				}
				c.wantResolved(t)
				c.wantRemote(t, remote)
			})
		}
	}
}

func TestAKillInTheMiddleOfTheFilesOfAResolutionIsUndoneAndTheNextPullResolves(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			c := newCutScenario(t, policy)
			remote := c.remoteRefs(t)
			var held *gittest.HeldMerge
			if policy == FollowRemote {
				held = c.f.HoldCheckout(fileB)
			} else {
				held = c.f.HoldMerge(fileB)
			}
			killed := puller(t, c.f, c.l, resolving(policy), func(o *Options) { o.Git.Git = held.Binary })
			done := make(chan error, 1)
			go func() {
				_, err := killed.Pull(context.Background())
				done <- err
			}()
			held.WaitHeld()
			held.Kill()
			if err := <-done; err == nil {
				t.Fatal("the pull of the move that was killed did not fail")
			}
			if !journalPresent(c.world) {
				t.Fatal("the journal is not there although the files were being written")
			}
			c.restart(t)

			got, err := c.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("the next pull = %+v, %v, want Resolved", got, err)
			}
			if !strings.Contains(got.Repaired, noteUn) {
				t.Errorf("the next pull put right %q, want it to say that the update was undone", got.Repaired)
			}
			c.wantResolved(t)
			c.wantRemote(t, remote)
		})
	}
}

func TestAStopBetweenTheCommitAndTheMoveLeavesTheCheckoutAsItWasAndTheNextPullResolves(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			c := newCutScenario(t, policy)
			started, release := filepath.Join(gittest.TempDir(t), "started"), filepath.Join(gittest.TempDir(t), "release")
			hold := fmt.Sprintf(`echo started > '%s'; while [ ! -e '%s' ]; do :; done`, started, release)
			stand := makeTrap(t, trap{sub: subUpdateIndex, nth: 1, run: hold})
			c.puller = puller(t, c.f, c.l, resolving(policy), func(o *Options) { o.Git.Git = stand.binary })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ended := pullInBackground(ctx, c.world)
			waitFor(t, "the files of the loser to be put in the index", started)

			cancel()
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			result := <-ended

			// The tree and the commit are made, because a stop lets a write that has begun finish, and the index is put
			// back, which a command that was cut off with the stop could not have done, and the checkout is not moved,
			// because a move does not begin once the agent was told to stop.
			wantReason(t, result.err, ReasonPullFailed)
			c.wantNotMoved(t, c.local)
			c.restart(t)
			got, err := c.pull(t)
			if err != nil || got.Outcome != Resolved {
				t.Fatalf("the next pull = %+v, %v, want Resolved", got, err)
			}
			c.wantResolved(t)
		})
	}
}

func TestAnIndexThatCannotBePutBackFailsTheResolutionAndTheNextPullPutsItRight(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			c := newCutScenario(t, policy)
			stand := makeTrap(t, trap{sub: subReadTree, nth: 2, fails: true})
			c.puller = puller(t, c.f, c.l, resolving(policy), func(o *Options) { o.Git.Git = stand.binary })

			_, err := c.pull(t)

			got := errorOf(t, err, ReasonPullFailed)
			if !strings.Contains(got.Message, "cannot be put back") {
				t.Errorf("message = %q, want it to say that the index cannot be put back", got.Message)
			}
			if c.head() != c.local {
				t.Error("the checkout was moved although its index could not be put back")
			}
			c.restart(t)

			if resolved, err := c.pull(t); err != nil || resolved.Outcome != Resolved {
				t.Fatalf("the next pull = %+v, %v, want Resolved", resolved, err)
			}
			c.wantResolved(t)
		})
	}
}

func TestARepositoryThatMovesAgainWhileTheCheckoutIsResolvedIsResolvedAgainAtTheNextPull(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			c := newCutScenario(t, policy)
			first := c.tip
			// The repository's next commit is made on another branch, and moved onto the branch when the agent is about
			// to move the checkout.
			second := c.f.Publish("spare", map[string]string{fileB: again})
			publish := fmt.Sprintf(`'%s' --git-dir='%s' update-ref refs/heads/main %s`, gittest.Binary(t), c.f.Bare, second)
			stand := makeTrap(t, trap{sub: moveOf(policy), nth: 1, run: publish})
			c.puller = puller(t, c.f, c.l, resolving(policy), func(o *Options) { o.Git.Git = stand.binary })

			got, err := c.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("the pull = %+v, %v, want Resolved against the tip that it fetched", got, err)
			}
			head := c.head()
			if parents := c.parents(t, head); policy == FollowRemote && fmt.Sprint(parents) != fmt.Sprint([]string{first}) {
				t.Errorf("the parents of the commit = %v, want only the tip that was fetched, %s", parents, first)
			}
			remote := c.remoteRefs(t)
			if !strings.Contains(remote, "refs/heads/main "+second) {
				t.Fatalf("the repository is %s, want its branch at the second commit %s", remote, second)
			}
			c.puller = puller(t, c.f, c.l, resolving(policy))

			got, err = c.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("the next pull = %+v, %v, want Resolved again", got, err)
			}
			c.final[fileB] = again
			wantFiles(t, c.world, c.final)
			if err := c.check("merge-base", "--is-ancestor", second, c.head()); err != nil {
				t.Errorf("the second commit of the repository is not in the history of the checkout: %v", err)
			}
			if dirty := c.dirty(t); dirty != "" {
				t.Errorf("the checkout has changes: %s", dirty)
			}
			c.wantRemote(t, remote)
			if policy == FollowRemote {
				// The commit that the first resolution made is kept too, as the local commits are.
				if refs := c.refsUnder(t, localDir); len(refs) != 2 || refs[0].commit != c.local || refs[1].commit != head {
					t.Errorf("local refs = %v, want the commit of the checkout and the commit of the first resolution", refs)
				}
			}
		})
	}
}

func TestTheTrapOnlyFiresOnTheRunItIsFor(t *testing.T) {
	// A guard of the stand-in the tests above rely on: a trap for the second run of a command is not fired by the first.
	stand := makeTrap(t, trap{sub: "rev-parse", nth: 2, dies: true})
	w := newWorld(t, subPath)
	run := puller(t, w.f, w.l, func(o *Options) { o.Git.Git = stand.binary })
	if _, err := run.git.Run(context.Background(), "rev-parse", "HEAD"); err != nil || stand.killed() {
		t.Fatalf("the first run failed or killed: %v, %v", err, stand.killed())
	}
	if _, err := run.git.Run(context.Background(), "rev-parse", "HEAD"); err == nil || !stand.killed() {
		t.Fatalf("the second run = %v, killed = %v, want a failure and the agent killed", err, stand.killed())
	}
	if _, err := run.git.Run(context.Background(), "version"); err == nil {
		t.Error("a command after the kill ran")
	}
}

func TestACommandThatFailsWhileTheTwoSidesAreComparedFailsTheResolutionBeforeItWritesAnything(t *testing.T) {
	// A failure of the command that finds the commit the two sides share is not "they share none", which would
	// take the empty tree as the base and carry every file of the loser over the winner's. The same goes for each of the
	// two lists of what a side changed, and for the command that finds the empty tree for sides that share none.
	for _, tt := range []struct {
		name, sub string
		nth       int
		noBase    bool
		says      string
	}{
		{"the commit they share", "merge-base", 1, false, "the commit that the checkout and the repository share cannot be found"},
		{"what the loser changed", "diff-tree", 1, false, "cannot be listed"},
		{"what the winner changed", "diff-tree", 2, false, "cannot be listed"},
		{"the empty tree", "hash-object", 1, true, "the empty tree cannot be found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newCutScenario(t, FollowRemote)
			if tt.noBase {
				c.replaceBranch(t)
			}
			stand := makeTrap(t, trap{sub: tt.sub, nth: tt.nth, fails: true})
			c.puller = puller(t, c.f, c.l, resolving(FollowRemote), func(o *Options) { o.Git.Git = stand.binary })

			_, err := c.pull(t)

			got := errorOf(t, err, ReasonPullFailed)
			if !strings.Contains(got.Message, "was not resolved") || !strings.Contains(got.Message, tt.says) {
				t.Errorf("message = %q, want it to say that the conflict was not resolved and %q", got.Message, tt.says)
			}
			c.wantNotMoved(t, c.local)
			c.wantLocalRefs(t)
		})
	}
}
