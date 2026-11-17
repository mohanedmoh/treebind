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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// The index is made again before a pull when it cannot be read, and reset when it holds what nothing here leaves
// staged (docs/adr/0028). The work tree is not touched, so an edit of the application stays an edit.

func indexPath(w *world) string { return filepath.Join(w.l.GitDir, "index") }

func TestAnIndexThatCannotBeReadIsMadeAgainAndTheEditsOfTheApplicationStayEdits(t *testing.T) {
	for name, damage := range map[string][]byte{
		"garbage":  []byte("this is not an index\n"),
		stateEmpty: {},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, subPath)
			gittest.Write(t, w.l, fileA, edited)
			if err := os.WriteFile(indexPath(w), damage, 0o600); err != nil {
				t.Fatal(err)
			}
			tip := w.f.Publish(branchMain, map[string]string{fileB: bChanged})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Advanced || got.To != tip {
				t.Fatalf("pull = %+v, %v, want the update applied", got, err)
			}
			if !strings.Contains(got.Repaired, "the index could not be read, and was made again from the commit the checkout is at") {
				t.Errorf("repaired = %q, want the index made again, and said", got.Repaired)
			}
			if gittest.Read(t, w.l, fileA) != edited || gittest.Read(t, w.l, fileB) != bChanged {
				t.Error("the edit of the application was lost, or the update was not applied")
			}
			// The edit is an edit: unstaged, and nothing else.
			if got := w.dirty(t); got != "M a.json" {
				t.Errorf("status = %q, want the edit of the application as an unstaged change", got)
			}
		})
	}
}

func TestAnIndexThatHasFilesStagedIsResetAndTheFilesStayEdits(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, fileA, edited)
	w.f.InCheckout(w.l, "add", "--", fileA)
	if got := w.dirty(t); got != "M  a.json" {
		t.Fatalf("status = %q, want a.json staged", got)
	}

	got, err := w.pull(t)

	if err != nil || got.Outcome != UpToDate {
		t.Fatalf("pull = %+v, %v, want nothing to apply", got, err)
	}
	if !strings.Contains(got.Repaired, "the index had files staged") {
		t.Errorf("repaired = %q, want the reset said", got.Repaired)
	}
	if status := w.dirty(t); status != "M a.json" || gittest.Read(t, w.l, fileA) != edited {
		t.Errorf("status = %q, a.json = %q, want the edit kept, and no longer staged", status, gittest.Read(t, w.l, fileA))
	}
}

func TestACheckoutWithNoCommitHasNothingStagedAndAnIndexThatCanBeRead(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, w *world){
		"a file staged": func(t *testing.T, w *world) { w.f.InCheckout(w.l, "add", "--", "mine.json") },
		"an index that cannot be read": func(t *testing.T, w *world) {
			if err := os.WriteFile(indexPath(w), []byte("garbage\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gittest.New(t)
			l := f.Layout(subPath)
			gittest.Write(t, l, "mine.json", mine)
			if _, err := checkoutOpen(t, f, l); err != nil {
				t.Fatal(err)
			}
			w := &world{f: f, l: l, puller: puller(t, f, l)}
			damage(t, w)

			// The repository has no branch yet, so the pull fails, and says what it put right all the same.
			_, err := w.pull(t)

			failure := errorOf(t, err, ReasonBranchGone)
			if !strings.Contains(failure.Repaired, "empty, as the checkout has no commit") {
				t.Errorf("repaired = %q, want the index emptied, said with the failure", failure.Repaired)
			}
			if out := w.f.InCheckout(w.l, "ls-files", "--cached"); out != "" || gittest.Read(t, l, "mine.json") != mine {
				t.Errorf("index holds %q, mine.json = %q, want an empty index, and the file of the application as it was", out, gittest.Read(t, l, "mine.json"))
			}
		})
	}
}

func TestAnIndexThatIsInOrderIsNotTouchedAndTheFenceIsNotAsked(t *testing.T) {
	var asked int
	w := newWorld(t, subPath, func(o *Options) { o.Fence = func(context.Context) error { asked++; return nil } })
	gittest.Write(t, w.l, fileA, edited)
	before, err := os.ReadFile(indexPath(w))
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.pull(t)

	if err != nil || got.Repaired != "" || asked != 0 {
		t.Errorf("pull = %+v, %v, asked the fence %d times, want nothing put right: an edit of the work tree is not a fault of the index", got, err, asked)
	}
	if after, _ := os.ReadFile(indexPath(w)); !bytes.Equal(before, after) {
		t.Error("an index that was in order was written")
	}
}

func TestAStopWhileTheIndexIsBeingLookedAtIsNotTakenForAnIndexThatCannotBeRead(t *testing.T) {
	// A command that was stopped says nothing about the index, and the index is not rebuilt on its word.
	root := gittest.TempDir(t)
	started := filepath.Join(root, "started")
	wrapper := filepath.Join(root, "git")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = diff-index ]; then
    echo started > '%s'
    while :; do :; done
  fi
done
exec '%s' "$@"
`, started, gittest.Binary(t))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, subPath, func(o *Options) { o.Git.Git = wrapper })
	before, err := os.ReadFile(indexPath(w))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err = w.puller.Pull(ctx)

	failure := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(failure.Message, "cannot be checked") || failure.Repaired != "" {
		t.Errorf("error = %q, repaired %q, want the check that was stopped said, and nothing made again", failure.Message, failure.Repaired)
	}
	if after, _ := os.ReadFile(indexPath(w)); !bytes.Equal(before, after) {
		t.Error("the index was written after a command that was stopped")
	}
}
