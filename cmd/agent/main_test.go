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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/app"
	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/commit"
	"github.com/mohanedmoh/treebind/internal/agent/config"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/lock"
	"github.com/mohanedmoh/treebind/internal/agent/paths"
	"github.com/mohanedmoh/treebind/internal/agent/pull"
	"github.com/mohanedmoh/treebind/internal/agent/scan"
	"github.com/mohanedmoh/treebind/internal/agent/watch"
)

// The functions that the agent starts with, before init gives the tests others.
var (
	pullerOfThePackage  = newPuller
	scannerOfThePackage = newScanner
	watcherOfThePackage = newWatcher
	// committerOfThePackage is the function that the agent commits with.
	committerOfThePackage = newCommitter
)

// No test of the agent's main starts Git: the checkout and the pulls are the ones of the packages,
// which have their own tests, and here they do nothing.
func init() {
	newPuller = func(pull.Options) (app.Puller, error) {
		return nil, errors.New("the tests of the main pull nothing")
	}
	newScanner = func(scan.Options) (app.Scanner, error) {
		return nil, errors.New("the tests of the main scan nothing")
	}
	newWatcher = func(watch.Options) (app.Watcher, error) {
		return nil, errors.New("the tests of the main watch nothing")
	}
	newCommitter = func(commit.Options) (app.Committer, error) {
		return nil, errors.New("the tests of the main commit nothing")
	}
	openCheckout = func(context.Context, checkout.Options) (checkout.Result, error) {
		return checkout.Result{Outcome: checkout.Cloned, Branch: "main"}, nil
	}
	acquireLock = func(context.Context, lock.Config) (*lock.Lock, error) {
		return nil, errors.New("the tests of the main take no lock")
	}
}

// repoURL is an address that the tests of the main give the agent. Nothing asks it.
const (
	repoURL  = "https://example.com/a.git"
	otherURL = "https://example.com/b.git"

	// botEmail is the author of the commits of the tests of the main.
	botEmail = "bot@example.org"
)

func environment(extra map[string]string) func(string) string {
	env := map[string]string{
		config.EnvNamespace:      "apps",
		config.EnvName:           "dashboards",
		config.EnvStatusIssuer:   "https://kubernetes.default.svc.cluster.local",
		config.EnvStatusAudience: "treebind.example.org/apps/dashboards",
		config.EnvStatusSubject:  "system:serviceaccount:treebind-system:treebind-controller-manager",
		config.EnvStatusAddr:     "127.0.0.1:0",
		config.EnvMountPath:      "/work",
		config.EnvRepoURL:        "https://example.com/acme/dashboards.git",
		config.EnvSyncDirection:  config.DirectionBidirectional,
		config.EnvPodName:        "dashboards-sync-0",
	}
	maps.Copy(env, extra)
	return func(name string) string { return env[name] }
}

func TestRunRefusesAWrongConfiguration(t *testing.T) {
	var stderr bytes.Buffer

	code := run(context.Background(), func(string) string { return "" }, &stderr)

	if code != exitConfig {
		t.Errorf("exit code %d, want %d", code, exitConfig)
	}
	for _, want := range []string{
		"configuration is wrong", config.EnvNamespace, config.EnvStatusIssuer, config.EnvStatusSubject, config.EnvRepoURL,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the message does not say %q:\n%s", want, stderr.String())
		}
	}
}

// lockedBuffer is a destination that the agent writes to while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunStopsOnRequestWithCodeZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, environment(map[string]string{config.EnvMountPath: t.TempDir()}), &stderr)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "serving the status endpoint") {
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not start serving:\n%s", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()

	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code %d, want %d:\n%s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
}

func TestRunFailsWhenItCannotServe(t *testing.T) {
	var stderr bytes.Buffer

	code := run(context.Background(), environment(map[string]string{config.EnvStatusAddr: "256.0.0.1:1"}), &stderr)

	if code != exitFailed {
		t.Errorf("exit code %d, want %d:\n%s", code, exitFailed, stderr.String())
	}
}

func TestRunRefusesAnHTTPRepositoryAndSaysWhy(t *testing.T) {
	var stderr bytes.Buffer

	env := environment(map[string]string{config.EnvRepoURL: "http://git.internal/acme/dashboards.git"})

	code := run(context.Background(), env, &stderr)

	if code != exitConfig {
		t.Errorf("exit code %d, want %d: a restart does not make plain http safe", code, exitConfig)
	}
	for _, want := range []string{config.EnvRepoURL, "must not use http://", "NFR-15"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the message does not say %q:\n%s", want, stderr.String())
		}
	}
}

func TestTheAgentRunsTheCheckoutOfThePackage(t *testing.T) {
	var asked checkout.Options
	previous := openCheckout
	openCheckout = func(_ context.Context, options checkout.Options) (checkout.Result, error) {
		asked = options
		return checkout.Result{Outcome: checkout.Reopened}, nil
	}
	t.Cleanup(func() { openCheckout = previous })
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	made, err := options.Checkout(context.Background(), checkout.Options{URL: otherURL})

	if err != nil || made.Outcome != checkout.Reopened || asked.URL != otherURL {
		t.Errorf("Checkout = %+v, %v with %+v, want the function of the package, called with what it is given",
			made, err, asked)
	}
	if options.Config.RepoURL != repoURL {
		t.Errorf("the settings were not passed on: %+v", options.Config)
	}
}

func TestTheLossOfTheLockHasAnExitCodeOfItsOwn(t *testing.T) {
	lost := fmt.Errorf("%w: %w", app.ErrLockLost, lock.ErrLost)

	if got := exitCodeOf(lost); got != exitLockLost {
		t.Errorf("exit code %d, want %d: the pod did nothing wrong, and its restart waits for the lock", got, exitLockLost)
	}
	if got := exitCodeOf(errors.New("serve the status endpoint: closed")); got != exitFailed {
		t.Errorf("exit code %d, want %d for any other error", got, exitFailed)
	}
}

func TestTheAgentTakesTheLockOfThePackage(t *testing.T) {
	previous := acquireLock
	var asked lock.Config
	acquireLock = func(_ context.Context, cfg lock.Config) (*lock.Lock, error) {
		asked = cfg
		return nil, errors.New("no lock in a test")
	}
	t.Cleanup(func() { acquireLock = previous })
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	_, err := options.AcquireLock(context.Background(), lock.Config{Holder: "holder"})

	if err == nil || asked.Holder != "holder" {
		t.Errorf("AcquireLock asked for %+v, %v, want the function of the package, called with what it is given", asked, err)
	}
}

func TestTheAgentPullsWithTheFunctionOfTheMain(t *testing.T) {
	previous := newPuller
	t.Cleanup(func() { newPuller = previous })
	var asked pull.Options
	newPuller = func(o pull.Options) (app.Puller, error) {
		asked = o
		return nil, errors.New("no puller in a test")
	}
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	_, err := options.NewPuller(pull.Options{URL: otherURL})

	if err == nil || asked.URL != otherURL {
		t.Errorf("NewPuller asked for %+v, %v, want the function of the main, called with what it is given", asked, err)
	}
}

func TestThePullerOfThePackageRefusesWhatCannotWorkAndIsOneThatPullsOtherwise(t *testing.T) {
	if _, err := pullerOfThePackage(pull.Options{}); err == nil {
		t.Error("a puller was made for a repository with no address")
	}
	where, err := layout.New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	puller, err := pullerOfThePackage(pull.Options{Layout: where, URL: repoURL, Branch: "main"})
	if err != nil || puller == nil {
		t.Errorf("the puller = %v, %v, want one that can pull", puller, err)
	}
}

func TestTheAgentScansWithTheFunctionOfTheMain(t *testing.T) {
	previous := newScanner
	t.Cleanup(func() { newScanner = previous })
	var asked scan.Options
	newScanner = func(o scan.Options) (app.Scanner, error) {
		asked = o
		return nil, errors.New("no scanner in a test")
	}
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	_, err := options.NewScanner(scan.Options{Layout: layout.Layout{WorkTree: "/w"}})

	if err == nil || asked.Layout.WorkTree != "/w" {
		t.Errorf("NewScanner asked for %+v, %v, want the function of the main, called with what it is given", asked, err)
	}
}

func TestTheAgentCommitsWithTheFunctionOfTheMain(t *testing.T) {
	previous := newCommitter
	t.Cleanup(func() { newCommitter = previous })
	var asked commit.Options
	newCommitter = func(o commit.Options) (app.Committer, error) {
		asked = o
		return nil, errors.New("no committer in a test")
	}
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	_, err := options.NewCommitter(commit.Options{AuthorName: "bot", AuthorEmail: botEmail})

	if err == nil || asked.AuthorName != "bot" || asked.AuthorEmail != botEmail {
		t.Errorf("NewCommitter asked for %+v, %v, want the function of the main, called with what it is given", asked, err)
	}
}

func TestTheCommitterOfThePackageRefusesWhatCannotWorkAndIsOneThatWorksOtherwise(t *testing.T) {
	where, err := layout.New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	refused := commit.Options{Layout: where, AuthorName: "bot <x>", AuthorEmail: botEmail}
	if _, err := committerOfThePackage(refused); err == nil {
		t.Error("a committer was made for an author that Git would change")
	}
	committer, err := committerOfThePackage(commit.Options{Layout: where})
	if err != nil || committer == nil {
		t.Errorf("the committer = %v, %v, want one that can commit, with the defaults of the agent", committer, err)
	}
}

func TestTheAgentWatchesWithTheFunctionOfTheMain(t *testing.T) {
	previous := newWatcher
	t.Cleanup(func() { newWatcher = previous })
	var asked watch.Options
	newWatcher = func(o watch.Options) (app.Watcher, error) {
		asked = o
		return nil, errors.New("no watcher in a test")
	}
	options := agentOptions(config.Config{RepoURL: repoURL}, nil, nil)

	_, err := options.NewWatcher(watch.Options{Root: "/w"})

	if err == nil || asked.Root != "/w" {
		t.Errorf("NewWatcher asked for %+v, %v, want the function of the main, called with what it is given", asked, err)
	}
}

func TestTheScannerAndTheWatcherOfThePackagesRefuseWhatCannotWorkAndAreOnesThatWorkOtherwise(t *testing.T) {
	if _, err := scannerOfThePackage(scan.Options{}); err == nil {
		t.Error("a scanner was made with no filter")
	}
	where, err := layout.New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	filter, err := paths.NewFilter(nil, nil, where.NeverSynced(), paths.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := scannerOfThePackage(scan.Options{Layout: where, Filter: filter})
	if err != nil || scanner == nil {
		t.Errorf("the scanner = %v, %v, want one that can scan", scanner, err)
	}

	if _, err := watcherOfThePackage(watch.Options{Root: "relative"}); err == nil {
		t.Error("a watcher was made for a root that is not absolute")
	}
	watcher, err := watcherOfThePackage(watch.Options{Root: t.TempDir()})
	if err != nil || watcher == nil {
		t.Fatalf("the watcher = %v, %v, want one that watches", watcher, err)
	}
	if err := watcher.Close(); err != nil {
		t.Error(err)
	}
}
