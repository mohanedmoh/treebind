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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// The journal is written before the merge and removed after it (docs/adr/0028).

// beforeMerge is a stand-in for git that runs what the body says when it is asked to merge, and then is the real one.
func beforeMerge(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(gittest.TempDir(t), "git")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = merge ]; then
    %s
  fi
done
exec '%s' "$@"
`, body, gittest.Binary(t))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheJournalIsThereWhileTheMergeRunsAndIsGoneWhenItHasEnded(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		seen := filepath.Join(gittest.TempDir(t), "seen")
		w := newWorld(t, sub)
		from := w.head()
		journal := filepath.Join(w.l.StateDir, journalName)
		// Copied with what a shell has built in, because the image of the agent has no cp.
		w.puller = puller(t, w.f, w.l, func(o *Options) {
			o.Git.Git = beforeMerge(t, fmt.Sprintf(`while IFS= read -r line; do printf '%%s\n' "$line"; done < '%s' > '%s'`, journal, seen))
		})
		to := w.f.Publish(branchMain, map[string]string{fileA: aChanged})

		got, err := w.pull(t)

		if err != nil || got.Outcome != Advanced {
			t.Fatalf("pull = %+v, %v, want the update applied", got, err)
		}
		data, readErr := os.ReadFile(seen)
		if readErr != nil || !strings.Contains(string(data), `"from":"`+from+`"`) || !strings.Contains(string(data), `"to":"`+to+`"`) {
			t.Errorf("the merge saw the journal %q, %v, want one with the commits of the update", data, readErr)
		}
		if journalPresent(w) {
			t.Error("the journal is still there after the update was applied")
		}
	})
}

func TestTheJournalIsGoneWhenGitRefusedTheUpdateBeforeItWroteAFile(t *testing.T) {
	w := newWorld(t, subPath)
	gittest.Write(t, w.l, fileA, edited)
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	wantReason(t, err, ReasonBlocked)
	if journalPresent(w) {
		t.Error("the journal is there after a merge that wrote nothing: the next pull would look for files that were never written")
	}
}

func TestTheJournalStaysWhenTheMergeFailedForAnyOtherReason(t *testing.T) {
	// Git can fail in the middle of the files, for a full volume, and the files it wrote are half an update.
	w := newWorld(t, subPath)
	w.puller = puller(t, w.f, w.l, func(o *Options) {
		o.Git.Git = beforeMerge(t, `echo "error: unable to write file a.json" >&2; exit 1`)
	})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	wantReason(t, err, ReasonPullFailed)
	if !journalPresent(w) {
		t.Error("the journal was removed after a merge that failed: the files it wrote could not be put back")
	}
}

func TestAnUpdateIsNotAppliedWhenItsJournalCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a directory that it has no right to")
	}
	w := newWorld(t, subPath)
	before := w.head()
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})
	if err := os.Chmod(w.l.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.l.StateDir, 0o700) })

	_, err := w.pull(t)

	failure := errorOf(t, err, ReasonPullFailed)
	if !strings.Contains(failure.Message, "its journal cannot be written") {
		t.Errorf("error = %q, want it to say that the journal cannot be written", failure.Message)
	}
	if w.head() != before || gittest.Read(t, w.l, fileA) != contentA {
		t.Error("the update was applied without a journal")
	}
}
