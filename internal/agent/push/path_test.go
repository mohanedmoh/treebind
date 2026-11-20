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

package push

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

const (
	dirPath     = "grafana/dashboards"
	fileOutside = "other/z.txt"
)

const upstreamChange = "changed upstream\n"

var (
	botAuthor = git.Identity{Name: "treebind-bot", Email: "treebind-bot@treebind.invalid"}

	// the files of a repository that has the directory of the tests, another one and files outside both
	repoFiles = map[string]string{
		dirPath + "/" + fileA: "a\n",
		dirPath + "/" + fileB: "b\n",
		"grafana/alerts/x":    "x\n",
		fileOutside:           "z\n",
		"README.md":           "readme\n",
	}
)

// directory is a repository, the checkout of one directory of it, a Pusher for the checkout and a Puller that resolves a
// conflict by the policy.
type directory struct {
	*world
	puller  *pull.Puller
	options Options
}

func newDirectory(t *testing.T, policy pull.Policy, change ...func(*Options)) *directory {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, repoFiles)
	l := f.Layout(subDir)
	opened := f.CheckoutOptions(l, branchMain)
	opened.Path = dirPath
	if made, err := checkout.Open(context.Background(), opened); err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	w := &world{t: t, f: f, l: l}
	w.fence = func(context.Context) error { return nil }
	o := Options{
		Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: time.Minute, Path: dirPath, Author: botAuthor,
		Fence: func(ctx context.Context) error {
			w.fenced.Add(1)
			return w.fence(ctx)
		},
	}
	for _, c := range change {
		c(&o)
	}
	var err error
	if w.pusher, err = New(o); err != nil {
		t.Fatal(err)
	}
	signed := f.GitOptions()
	signed.Identity = botAuthor
	puller, err := pull.New(pull.Options{Layout: l, URL: f.URL, Branch: branchMain, Path: dirPath, Git: signed, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return &directory{world: w, puller: puller, options: o}
}

// restart is the agent after a kill: a Pusher with the Git of the agent again, and the fence of the test.
func (d *directory) restart() {
	d.t.Helper()
	o := d.options
	o.Git = d.f.GitOptions()
	var err error
	if d.pusher, err = New(o); err != nil {
		d.t.Fatal(err)
	}
}

func (d *directory) pull() (pull.Result, error) { return d.puller.Pull(context.Background()) }

// real is the tree of a commit of the repository's branch, and the files that it has under a directory.
func (d *directory) names(commit, dir string) string {
	d.t.Helper()
	return d.f.Plain(d.f.Bare, "ls-tree", "-r", "--name-only", commit+":"+dir)
}

// A commit of the application on the directory is sent as a commit of the repository, on the tip of the repository, which has the
// files of the directory as the commit has them and every other file as it was; the checkout is put at it with nothing left to
// send, and a pull that follows has nothing to do and fetches nothing.
func TestACommitOnTheDirectoryIsPushedAsACommitOfTheRepositoryAndTheCheckoutIsPutAtIt(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	oldTip := d.remoteTip()
	d.commit(fileA, "changed\n")
	d.commit("new/deep.json", "n\n")

	got, err := d.push()

	if err != nil || got.Outcome != Pushed || got.Commits != 2 {
		t.Fatalf("Push = %+v, %v; want two commits pushed", got, err)
	}
	tip := d.remoteTip()
	if tip == oldTip || got.ToSource != tip || got.FromSource != oldTip {
		t.Errorf("the repository is at %s, the result says from %s to %s; want from %s to the tip", tip, got.FromSource, got.ToSource, oldTip)
	}
	if outside := d.f.Plain(d.f.Bare, "diff-tree", "-r", "--name-only", oldTip, tip); strings.Contains(outside, fileOutside) ||
		strings.Contains(outside, "README") || strings.Contains(outside, "alerts") {
		t.Errorf("the push changed files outside the directory:\n%s", outside)
	}
	if names := d.names(tip, dirPath); !strings.Contains(names, "new/deep.json") {
		t.Errorf("the directory in the repository has:\n%s", names)
	}
	if count := strings.TrimSpace(d.f.Plain(d.f.Bare, "rev-list", "--count", oldTip+".."+tip)); count != "2" {
		t.Errorf("%s commits were sent, want 2, one for each commit of the application", count)
	}

	// settled: nothing waits, the checkout is at a snapshot of the tip, and the next pull has nothing to do
	if again, err := d.push(); err != nil || again.Outcome != NothingToPush {
		t.Errorf("the next push = %+v, %v; want NothingToPush", again, err)
	}
	before := d.f.Sessions()
	pulled, err := d.pull()
	if err != nil || pulled.Outcome != pull.UpToDate || pulled.ToSource != tip {
		t.Fatalf("Pull = %+v, %v; want UpToDate at %s", pulled, err, tip)
	}
	if asked := d.f.Sessions() - before; asked != 1 {
		t.Errorf("the pull reached the repository %d times, want 1: a question", asked)
	}
	if d.head() != d.f.InCheckout(d.l, "rev-parse", subtree.TrackingRef(branchMain)) {
		t.Error("the checkout is not at the snapshot of the tip")
	}
}

// What the repository changes outside the directory never stands in the way of a push: the commit is made on the tip that was
// fetched, and a push that the host takes for a tip that moved is sent again on the new one after a pull, with no conflict.
func TestAChangeOutsideTheDirectoryMakesAPushThatIsNotAFastForwardAndNoConflict(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	d.commit(fileA, "changed\n")
	d.f.Publish(branchMain, map[string]string{fileOutside: upstreamChange})

	_, err := d.push()
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonNonFastForward {
		t.Fatalf("err = %v, want %s: the host has a tip that the checkout has not seen", err, ReasonNonFastForward)
	}

	if pulled, err := d.pull(); err != nil || pulled.Outcome != pull.Ahead {
		t.Fatalf("Pull = %+v, %v; want Ahead: nothing came in that the volume does not have", pulled, err)
	}
	got, err := d.push()
	if err != nil || got.Outcome != Pushed {
		t.Fatalf("Push = %+v, %v; want it pushed on the new tip", got, err)
	}
	if content := d.f.Plain(d.f.Bare, "show", "main:"+fileOutside); content != upstreamChange {
		t.Errorf("the change outside the directory was undone: %q", content)
	}
	if content := d.f.Plain(d.f.Bare, "show", "main:"+dirPath+"/"+fileA); content != "changed\n" {
		t.Errorf("the commit of the application is not in the repository: %q", content)
	}
}

// The agent is killed after each write that the settling of a push makes. What is left is put right by the next pull, whichever
// policy the Treebind has: the volume is as it was, nothing is sent again, and the checkout is at a snapshot of the tip.
func TestAKillAfterTheEndOfAPushAndBeforeTheCheckoutIsSettledLeavesWhatThePullPutsRight(t *testing.T) {
	for _, policy := range []pull.Policy{pull.FollowRemote, pull.FollowLocal} {
		for kill := 1; kill <= 3; kill++ {
			t.Run(string(policy)+" after write "+string(rune('0'+kill)), func(t *testing.T) {
				killed := gittest.KilledAfter(t, "update-ref", kill)
				d := newDirectory(t, policy, func(o *Options) { o.Git.Git = killed })
				d.commit(fileA, "changed\n")
				before := gittest.Read(t, d.l, fileA)

				_, err := d.push()
				if (err == nil) != (kill == 3) {
					// The push itself worked; what the agent could not do afterwards is a warning of the result and no error.
					t.Logf("push: %v", err)
				}
				tip := d.remoteTip()
				if tip == "" || strings.TrimSpace(d.f.Plain(d.f.Bare, "show", "main:"+dirPath+"/"+fileA)) != "changed" {
					t.Fatal("the push did not reach the repository")
				}
				commits := strings.TrimSpace(d.f.Plain(d.f.Bare, "rev-list", "--count", "main"))

				// the next pass, with the Git of the agent again
				d.restart()
				if pulled, err := d.pull(); err != nil {
					t.Fatalf("the pull after the kill failed: %v (outcome %s)", err, pulled.Outcome)
				}
				if again, err := d.push(); err != nil || again.Outcome != NothingToPush {
					t.Fatalf("the push after the kill = %+v, %v; want nothing to send", again, err)
				}
				if got := strings.TrimSpace(d.f.Plain(d.f.Bare, "rev-list", "--count", "main")); got != commits {
					t.Errorf("the repository has %s commits, want the %s that it had: nothing is sent again", got, commits)
				}
				if gittest.Read(t, d.l, fileA) != before {
					t.Error("the volume changed")
				}
				if d.head() != d.f.InCheckout(d.l, "rev-parse", subtree.TrackingRef(branchMain)) {
					t.Error("the checkout is not at the snapshot at the tip")
				}
				if got := d.f.InCheckout(d.l, "rev-parse", subtree.RealRef(branchMain)); got != tip {
					t.Errorf("the checkout knows the repository to be at %s, want %s", got, tip)
				}
			})
		}
	}
}

// A repository that declines a push with its own words is a push that was not made: neither the tip of the repository nor the
// snapshot is moved, and the commits wait to be sent as they did.
func TestAPushThatTheRepositoryDeclinesMovesNothingOfTheCheckout(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	d.installHook(hookDeclines)
	head := d.commit(fileA, "changed\n")
	real, snapshot := d.f.InCheckout(d.l, "rev-parse", subtree.RealRef(branchMain)), d.tracking()

	_, err := d.push()

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonPushRejected || !strings.Contains(failure.Message, "Protected branch") {
		t.Fatalf("err = %v, want %s with the words of the host", err, ReasonPushRejected)
	}
	if now := d.f.InCheckout(d.l, "rev-parse", subtree.RealRef(branchMain)); now != real || d.tracking() != snapshot || d.head() != head {
		t.Error("a push that was declined moved a ref of the checkout")
	}
}

// A lock that cannot be checked sends nothing, and the commits wait.
func TestAFenceThatRefusesSendsNothingForADirectory(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	d.commit(fileA, "changed\n")
	tip := d.remoteTip()
	d.fence = func(context.Context) error { return errors.New("the lock is lost") }

	_, err := d.push()

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonPushFailed || !strings.Contains(failure.Message, "writer lock") {
		t.Fatalf("err = %v, want %s that says the writer lock could not be checked", err, ReasonPushFailed)
	}
	if d.remoteTip() != tip {
		t.Error("something was sent")
	}
}

func TestACommitThatWaitsNeedsTheAuthorOfTheAgentAndSendsNothingWithout(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote, func(o *Options) { o.Author = git.Identity{} })
	d.commit(fileA, "changed\n")
	tip := d.remoteTip()

	_, err := d.push()

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonPushFailed || !strings.Contains(failure.Message, "no valid author") {
		t.Fatalf("err = %v, want %s that says there is no valid author", err, ReasonPushFailed)
	}
	if d.remoteTip() != tip {
		t.Error("something was sent")
	}
}

// A stop that comes in the middle of a push that worked does not cut the settling: it is what keeps the commits from being
// counted as waiting, and a short time is all it needs.
func TestTheCheckoutIsSettledByAMoveThatAStopDoesNotCut(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	head := d.commit(fileA, "changed\n")
	translated, err := d.pusher.projector.Translate(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	d.f.Plain(d.l.GitDir, "push", "-q", d.f.Bare, translated.Tip+":refs/heads/"+branchMain)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.pusher.settle(ctx, head, translated.Tip); err != nil {
		t.Fatalf("the settling was cut short by a stop that came before it: %v", err)
	}
	if d.head() != d.tracking() || d.head() == head {
		t.Error("the checkout was not put at the snapshot")
	}
}

// A repository that has no commit takes the first commit of the application as a commit that has no parent and has the directory
// in it, and nothing else.
func TestTheFirstPushIntoARepositoryWithNoCommitMakesTheDirectoryAndNothingElse(t *testing.T) {
	f := gittest.New(t)
	l := f.Layout(subDir)
	opened := f.CheckoutOptions(l, branchMain)
	opened.Path = dirPath
	if _, err := checkout.Open(context.Background(), opened); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, f: f, l: l}
	var err error
	w.pusher, err = New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: time.Minute, Path: dirPath, Author: botAuthor})
	if err != nil {
		t.Fatal(err)
	}
	w.commit(fileA, "first\n")

	got, err := w.push()

	if err != nil || got.Outcome != Pushed || got.Commits != 1 {
		t.Fatalf("Push = %+v, %v", got, err)
	}
	if names := f.Plain(f.Bare, "ls-tree", "-r", "--name-only", "main"); strings.TrimSpace(names) != dirPath+"/"+fileA {
		t.Errorf("the repository has %q, want only the directory with the file", names)
	}
	if again, err := w.push(); err != nil || again.Outcome != NothingToPush {
		t.Errorf("the next push = %+v, %v; want NothingToPush", again, err)
	}
}

// A repository that changed the directory, fetched and not yet made a snapshot of, is one that the checkout is behind: nothing is
// sent, and the reason is the one of a push that is not a fast-forward, which the next pull puts right.
func TestACheckoutThatIsBehindTheDirectoryOfTheRepositoryIsToldToPullFirst(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	d.commit(fileA, "changed\n")
	tip := d.remoteTip()
	d.f.Publish(branchMain, map[string]string{dirPath + "/" + fileB: upstreamChange})
	d.f.Plain(d.l.GitDir, "fetch", "-q", d.f.Bare, "+refs/heads/"+branchMain+":"+subtree.RealRef(branchMain))
	sessions := d.f.Sessions()

	_, err := d.push()

	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != ReasonNonFastForward || !strings.Contains(failure.Message, "next pull brings them in") {
		t.Fatalf("err = %v, want %s that says the next pull brings the changes in", err, ReasonNonFastForward)
	}
	if d.remoteTip() == tip || d.f.Sessions() != sessions {
		t.Error("the agent reached the repository for a push that cannot be sent")
	}
}

// Commits that wait and are all snapshots are no commits of the application: nothing is made again and the repository is not
// asked for a push for them.
func TestCommitsThatAreOnlySnapshotsAreNotPushed(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	tree := d.f.InCheckout(d.l, "rev-parse", "HEAD^{tree}")
	real := d.f.InCheckout(d.l, "rev-parse", subtree.RealRef(branchMain))
	again := d.f.InCheckout(d.l, "commit-tree", tree, "-p", "HEAD", "-m", "a snapshot\n\nTreebind-Source: "+real+"\nTreebind-Path: "+strconv.Quote(dirPath)+"\n")
	d.f.InCheckout(d.l, "update-ref", "refs/heads/"+branchMain, again)
	sessions := d.f.Sessions()

	got, err := d.push()

	if err != nil || got.Outcome != NothingToPush {
		t.Fatalf("Push = %+v, %v; want NothingToPush", got, err)
	}
	if d.f.Sessions() != sessions {
		t.Error("the repository was asked for a push that has nothing in it")
	}
}

// The lock is checked before the checkout is put at the snapshot, as it is before every write, and a lock that cannot be checked
// is a push that worked and a warning that says so: the pull puts the checkout right.
func TestAFenceThatRefusesAfterThePushLeavesTheCheckoutForThePull(t *testing.T) {
	d := newDirectory(t, pull.FollowRemote)
	head := d.commit(fileA, "changed\n")
	calls := 0
	d.fence = func(context.Context) error {
		if calls++; calls > 1 {
			return errors.New("the lock is lost")
		}
		return nil
	}

	got, err := d.push()

	if err != nil || got.Outcome != Pushed || !strings.Contains(got.Warning, "the lock is lost") {
		t.Fatalf("Push = %+v, %v; want a push that worked and a warning that names the lock", got, err)
	}
	if d.head() != head {
		t.Error("the checkout was moved without the lock")
	}
}
