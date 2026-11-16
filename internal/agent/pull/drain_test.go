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
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// A stop does not cut off an update that is writing the files, which leaves them half written
// (docs/adr/0028): the merge has Drain to finish, and is stopped after that.

// slowMerge is what stands in for a merge that takes long to write its files: a git that, when it is
// asked to merge, says so and waits for a file to appear, and then is the real one. It uses only
// what a shell has built in, because the image of the agent has no sleep.
func slowMerge(t *testing.T, started, release string) string {
	t.Helper()
	path := filepath.Join(gittest.TempDir(t), "git")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = merge ]; then
    echo started > '%s'
    while [ ! -e '%s' ]; do :; done
  fi
done
exec '%s' "$@"
`, started, release, gittest.Binary(t))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// pullInBackground starts a pull and returns where its end is.
func pullInBackground(ctx context.Context, w *world) <-chan pulled {
	done := make(chan pulled, 1)
	go func() {
		result, err := w.puller.Pull(ctx)
		done <- pulled{result, err}
	}()
	return done
}

type pulled struct {
	result Result
	err    error
}

func waitFor(t *testing.T, what, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAStopLetsTheUpdateThatIsWritingTheFilesFinish(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		root := gittest.TempDir(t)
		started, release := filepath.Join(root, "started"), filepath.Join(root, "release")
		w := newWorld(t, sub, func(o *Options) {
			o.Git.Git = slowMerge(t, started, release)
			o.Drain = time.Minute
		})
		tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged, fileC: contentC})

		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		done := pullInBackground(ctx, w)
		waitFor(t, "the merge to start", started)
		stop()

		select {
		case got := <-done:
			t.Fatalf("the pull ended at the stop, %+v, %v, and the merge was cut off in the middle of the files", got.result, got.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.err != nil || got.result.Outcome != Advanced || w.head() != tip {
			t.Fatalf("pull = %+v, %v, HEAD %s, want the update applied to %s", got.result, got.err, w.head(), tip)
		}
		if gittest.Read(t, w.l, fileA) != aChanged || gittest.Read(t, w.l, fileC) != contentC || w.dirty(t) != "" {
			t.Errorf("the files are not the update's, or the checkout is not clean: %q", w.dirty(t))
		}
	})
}

func TestAStopThatLastsPastTheDrainStopsTheUpdate(t *testing.T) {
	root := gittest.TempDir(t)
	started, release := filepath.Join(root, "started"), filepath.Join(root, "release")
	w := newWorld(t, subPath, func(o *Options) {
		o.Git.Git = slowMerge(t, started, release)
		o.Drain = 300 * time.Millisecond
	})
	before := w.head()
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := pullInBackground(ctx, w)
	waitFor(t, "the merge to start", started)
	begun := time.Now()
	stop()

	var got pulled
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the merge was not stopped once the drain had passed")
	}
	if elapsed := time.Since(begun); elapsed < 250*time.Millisecond {
		t.Errorf("the merge was stopped %s after the stop, before the drain of 300ms had passed", elapsed)
	}
	wantReason(t, got.err, ReasonPullFailed)
	if w.head() != before {
		t.Errorf("HEAD moved to %s although the merge was stopped", w.head())
	}
}

func TestNoUpdateBeginsOnceTheAgentWasToldToStop(t *testing.T) {
	// A checkout with no commit has no commit to keep a ref for, so the merge is the first command after the
	// fence, and the only thing between the stop and the merge is the pull itself.
	root := gittest.TempDir(t)
	started, release := filepath.Join(root, "started"), filepath.Join(root, "release")
	f := gittest.New(t)
	l := f.Layout(subPath)
	gittest.Write(t, l, "mine.json", mine)
	if made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain)); err != nil || made.Commit != "" {
		t.Fatalf("checkout = %+v, %v, want one with no commit", made, err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	p := puller(t, f, l, func(o *Options) {
		o.Git.Git = slowMerge(t, started, release)
		o.Drain = time.Minute
		// The stop comes after the checks of the update and before its first write.
		o.Fence = func(context.Context) error { stop(); return nil }
	})
	f.Publish(branchMain, versionOne)

	_, err := p.Pull(ctx)

	wantReason(t, err, ReasonPullFailed)
	if _, statErr := os.Stat(started); statErr == nil {
		t.Error("a merge began after the agent was told to stop")
	}
	if gittest.Read(t, l, "mine.json") != mine || gittest.Exists(filepath.Join(l.WorkTree, fileA)) {
		t.Error("the directory changed")
	}
}

func TestNewFillsInTheDrainAndRefusesANegativeOne(t *testing.T) {
	w := newWorld(t, subPath)
	if w.puller.o.Drain != DefaultDrain {
		t.Errorf("Drain = %s, want %s", w.puller.o.Drain, DefaultDrain)
	}
	o := Options{Layout: w.l, URL: w.f.URL, Branch: branchMain, Git: w.f.GitOptions(), Drain: -time.Second}
	if _, err := New(o); err == nil {
		t.Error("New took a negative drain")
	}
}
