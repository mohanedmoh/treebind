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

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/push"
)

const (
	testRepoPath = "grafana/dashboards"
	snapshotOne  = "1111111111111111111111111111111111111111"
	snapshotTwo  = "2222222222222222222222222222222222222222"
	commitOne    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitTwo    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// The directory of the repository that is the synced directory (docs/adr/0036) is what the checkout, the pull and the push are
// made for, and the push is told who the commits are by, which is the author of the agent's commits.
func TestTheCheckoutIsMadeForTheDirectoryThatTheSettingsSay(t *testing.T) {
	asked := make(chan checkout.Options, 4)
	open := withRepository(func(_ context.Context, o checkout.Options) (checkout.Result, error) {
		asked <- o
		return cloned(context.Background(), o)
	})
	start(t, func(o *Options) {
		open(o)
		o.Config.RepoPath = testRepoPath
	})

	if got := (<-asked).Path; got != testRepoPath {
		t.Errorf("the checkout is made for the path %q, want %q", got, testRepoPath)
	}
}

func TestThePullAndThePushAreMadeForTheDirectoryAndThePushHasTheAuthorOfTheCommits(t *testing.T) {
	w := newPushWorld()
	takePuller, pullOptions := pullOptionsOf()
	start(t, w.with(takePuller, func(o *Options) {
		o.Config.RepoPath = testRepoPath
		o.Config.CommitAuthorName, o.Config.CommitAuthorEmail = "Zoë", "zoe@example.org"
		// The first pull is due within an interval, and with a short one the scans that wait for it are few.
		o.Config.SyncInterval = testInterval
	}))

	if got := w.untilPulled(t, pullOptions).Path; got != testRepoPath {
		t.Errorf("the pull is made for the path %q, want %q", got, testRepoPath)
	}
	options := <-w.opts
	if options.Path != testRepoPath {
		t.Errorf("the push is made for the path %q, want %q", options.Path, testRepoPath)
	}
	if options.Author.Name != "Zoë" || options.Author.Email != "zoe@example.org" {
		t.Errorf("the push has the author %+v, want the one of the commits", options.Author)
	}
}

// The commits that a person can look up are the repository's: the checks name them, and not the snapshots that the checkout is at.
func TestTheChecksNameTheCommitsOfTheRepositoryAndNotTheSnapshots(t *testing.T) {
	advanced, _ := pullResult(pull.Result{Outcome: pull.Advanced, Branch: testBranch, From: snapshotOne, To: snapshotTwo, FromSource: commitOne, ToSource: commitTwo}, nil)
	for _, want := range []string{shortOf(commitOne), shortOf(commitTwo)} {
		if !strings.Contains(advanced.Message, want) {
			t.Errorf("advanced = %q, want it to name %s", advanced.Message, want)
		}
	}
	resolved, _ := pullResult(pull.Result{Outcome: pull.Resolved, Branch: testBranch, From: snapshotOne, To: snapshotTwo, FromSource: commitOne, ToSource: commitTwo,
		Resolution: &pull.Resolution{Policy: pull.FollowRemote}}, nil)
	if !strings.Contains(resolved.Message, shortOf(commitOne)) || !strings.Contains(resolved.Message, shortOf(commitTwo)) {
		t.Errorf("resolved = %q, want it to name the commits of the repository", resolved.Message)
	}
	for outcome, result := range map[string]pull.Result{
		"at the tip": {Outcome: pull.UpToDate, Branch: testBranch, From: snapshotTwo, To: snapshotTwo, ToSource: commitTwo},
		"ahead":      {Outcome: pull.Ahead, Branch: testBranch, From: snapshotTwo, To: snapshotTwo, ToSource: commitTwo},
	} {
		got, _ := pullResult(result, nil)
		if !strings.Contains(got.Message, shortOf(commitTwo)) {
			t.Errorf("%s = %q, want it to name the commit of the repository", outcome, got.Message)
		}
	}
	for _, snapshot := range []string{shortOf(snapshotOne), shortOf(snapshotTwo)} {
		if strings.Contains(advanced.Message, snapshot) || strings.Contains(resolved.Message, snapshot) {
			t.Errorf("a check names the snapshot %s", snapshot)
		}
	}

	pushed, _ := pushResult(push.Result{Outcome: push.Pushed, Branch: testBranch, From: snapshotOne, To: snapshotTwo, FromSource: commitOne, ToSource: commitTwo, Commits: 2}, nil)
	if want := "pushed 2 commits to the branch " + testBranch + ": " + shortOf(commitOne) + ".." + shortOf(commitTwo); pushed.Message != want {
		t.Errorf("pushed = %q, want %q", pushed.Message, want)
	}
	created, _ := pushResult(push.Result{Outcome: push.Pushed, Branch: testBranch, To: snapshotTwo, ToSource: commitTwo, Commits: 1}, nil)
	if !strings.Contains(created.Message, shortOf(commitTwo)) || strings.Contains(created.Message, shortOf(snapshotTwo)) {
		t.Errorf("created = %q, want it to name the commit of the repository", created.Message)
	}
}

func TestTheCheckoutCheckNamesTheCommitOfTheRepositoryForADirectory(t *testing.T) {
	r := start(t, withRepository(func(context.Context, checkout.Options) (checkout.Result, error) {
		return checkout.Result{Outcome: checkout.Cloned, Branch: testBranch, Commit: snapshotOne, Source: commitOne}, nil
	}))

	eventually(t, "the checkout in the report", func() bool {
		got, found := checkoutCheck(t, r)
		return found && got.Outcome == outcomePassed
	})

	got, _ := checkoutCheck(t, r)
	if !strings.Contains(got.Message, shortOf(commitOne)) || strings.Contains(got.Message, shortOf(snapshotOne)) {
		t.Errorf("checkout = %q, want it to name the commit of the repository and not the snapshot", got.Message)
	}
}
