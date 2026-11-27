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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// A journal that stays keeps the commit step from committing (docs/adr/0028), so a pull that cannot remove it has not put
// the update right, and says so as a failure: the Synced condition shows it, which the note of a passed check does not.

// refuseRemovals makes the state directory refuse the removal of the journal, as one that is read only does.
func refuseRemovals(t *testing.T, w *world) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a directory that it has no right to")
	}
	if err := os.Chmod(w.l.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.l.StateDir, 0o700) })
}

func wantJournalFailure(t *testing.T, err error) *Error {
	t.Helper()
	failure := errorOf(t, err, ReasonPullFailed)
	for _, want := range []string{"journal", "cannot be removed", "no commit is made"} {
		if !strings.Contains(failure.Message, want) {
			t.Errorf("error = %q, want it to say %q", failure.Message, want)
		}
	}
	return failure
}

func TestAPullFailsWhenTheJournalOfAnUpdateThatWasCutOffCannotBeRemoved(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		c := cutOff(t, sub, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })
		refuseRemovals(t, c.world)

		_, err := c.pull(t)

		failure := wantJournalFailure(t, err)
		// What the repair did before it met the journal is said, so that the check tells all of it.
		if !strings.Contains(failure.Repaired, "2 file(s) put back") {
			t.Errorf("repaired = %q, want the files that were put back said", failure.Repaired)
		}
		if !journalPresent(c.world) {
			t.Error("the journal is gone, although the state directory refuses the removal")
		}
	})
}

func TestAPullFailsWhenTheJournalOfAnUpdateThatHadFinishedCannotBeRemoved(t *testing.T) {
	w := newWorld(t, subPath)
	if err := w.puller.writeJournal(commitOne, w.head()); err != nil {
		t.Fatal(err)
	}
	refuseRemovals(t, w)

	_, err := w.pull(t)

	_ = wantJournalFailure(t, err)
}

func TestAPullFailsWhenAJournalThatCannotBeUsedCannotBeRemoved(t *testing.T) {
	w := newWorld(t, subPath)
	// A directory with a file in it is a journal that cannot be used, and one that Remove refuses.
	journal := journalFile(w.puller)
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journal, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := w.pull(t)

	_ = wantJournalFailure(t, err)
}

func TestRecoverPutsAnUpdateThatWasCutOffRightAndAppliesNothing(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		c := cutOff(t, sub, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })

		note, err := c.puller.Recover(context.Background())

		if err != nil {
			t.Fatalf("Recover = %v, want the update put right", err)
		}
		if !strings.Contains(note, noteUn) || !strings.Contains(note, "2 file(s) put back, 0 removed") {
			t.Errorf("note = %q, want the update undone, with 2 files put back", note)
		}
		if got, want := c.files(t), map[string]string{fileA: oldA, fileB: oldB, fileC: oldC}; !sameFiles(got, want) {
			t.Errorf("the files are %v, want %v", got, want)
		}
		if c.head() != c.from || journalPresent(c.world) {
			t.Errorf("HEAD = %s, journal present %v, want the commit before and no journal: Recover applies no update", c.head(), journalPresent(c.world))
		}
	})
}

func TestRecoverDoesNothingWhenNoUpdateWasCutOff(t *testing.T) {
	w := newWorld(t, subPath)

	note, err := w.puller.Recover(context.Background())

	if note != "" || err != nil {
		t.Errorf("Recover = %q, %v, want nothing said and no error", note, err)
	}
}

func TestRecoverFailsWhenTheJournalCannotBeRemoved(t *testing.T) {
	c := cutOff(t, subPath, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })
	refuseRemovals(t, c.world)

	note, err := c.puller.Recover(context.Background())

	_ = wantJournalFailure(t, err)
	if !strings.Contains(note, "2 file(s) put back") {
		t.Errorf("note = %q, want what was put back said, also when the journal stays", note)
	}
}

// The failure of a pull says that it is of an update that was cut off, which is what the agent puts right at once. A repair that
// fails is not: it would fail the same way again.
func TestOnlyTheFailureOfAnUpdateThatWasBegunSaysThatItWasCutOff(t *testing.T) {
	c := cutOff(t, subPath, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })
	if failure := errorOf(t, c.killed, ReasonPullFailed); !failure.Cut {
		t.Errorf("the pull whose update was killed does not say that it was cut off: %+v", failure)
	}
	refuseRemovals(t, c.world)

	_, err := c.pull(t)

	if failure := wantJournalFailure(t, err); failure.Cut {
		t.Errorf("the pull whose repair could not remove the journal says that an update was cut off: %+v", failure)
	}
}

// Recover writes nothing when the fence of the writer lock refuses, as the repair of a pull does not.
func TestRecoverWritesNothingWhenTheFenceRefuses(t *testing.T) {
	c := cutOff(t, subPath, func(f *gittest.Fixture) *gittest.HeldMerge { return f.HoldMerge(fileB) })
	refused := errors.New("the lock is another agent's")
	c.puller = puller(t, c.f, c.l, func(o *Options) {
		o.Fence = func(context.Context) error { return refused }
	})
	before := c.files(t)

	_, err := c.puller.Recover(context.Background())

	failure := errorOf(t, err, ReasonRefused)
	if !errors.Is(failure, refused) {
		t.Errorf("error = %v, want the refusal of the fence underneath", failure)
	}
	if !journalPresent(c.world) || !sameFiles(c.files(t), before) {
		t.Errorf("journal present %v, files %v, want nothing written: the fence refused", journalPresent(c.world), c.files(t))
	}
}
