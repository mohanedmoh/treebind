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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

const (
	commitOne = "1111111111111111111111111111111111111111"
	commitTwo = "2222222222222222222222222222222222222222"
)

// journalWorld is a Puller on a claim, for the tests of the journal, which touch no Git.
func journalWorld(t *testing.T) *Puller {
	t.Helper()
	f := gittest.New(t)
	return puller(t, f, f.Layout(subPath))
}

func journalFile(p *Puller) string { return filepath.Join(p.o.Layout.StateDir, journalName) }

func TestTheJournalOfAnUpdateIsReadBackAsItWasWritten(t *testing.T) {
	for _, from := range []string{commitOne, ""} {
		p := journalWorld(t)
		if err := p.writeJournal(from, commitTwo); err != nil {
			t.Fatal(err)
		}

		got, present, problem := p.readJournal()

		if !present || problem != nil || got.From != from || got.To != commitTwo {
			t.Errorf("journal from %q = %+v, present %v, problem %v, want the commits it was given", from, got, present, problem)
		}
		info, err := os.Stat(journalFile(p))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("journal = %v, %v, want a file that only the agent can read", info, err)
		}
		if _, err := os.Lstat(journalFile(p) + ".new"); err == nil {
			t.Error("the file that the journal was written to is still there: it was to be renamed")
		}
	}
}

func TestThereIsNoJournalUnlessOneWasWritten(t *testing.T) {
	p := journalWorld(t)
	if got, present, problem := p.readJournal(); present || problem != nil {
		t.Errorf("journal = %+v, present %v, problem %v, want none", got, present, problem)
	}
	// A state directory that is not there has no journal either, and is no problem for the pull that has none.
	if err := os.RemoveAll(p.o.Layout.StateDir); err != nil {
		t.Fatal(err)
	}
	if _, present, problem := p.readJournal(); present || problem != nil {
		t.Errorf("present %v, problem %v, want none", present, problem)
	}
}

func TestAJournalThatIsTornOrNotOneOfOursIsAProblemAndNotAnUpdate(t *testing.T) {
	good := `{"version":1,"from":"` + commitOne + `","to":"` + commitTwo + `"}`
	for name, content := range map[string]string{
		stateEmpty:             "",
		"garbage":              "not json at all\n",
		"cut short":            good[:len(good)/2],
		"another version":      strings.Replace(good, `"version":1`, `"version":2`, 1),
		"an unknown field":     strings.Replace(good, `}`, `,"extra":1}`, 1),
		"a commit that is not": strings.Replace(good, commitTwo, "not-a-commit", 1),
		"no commit to go to":   `{"version":1,"from":"` + commitOne + `","to":""}`,
		"the same commit":      strings.ReplaceAll(good, commitTwo, commitOne),
		"more than one value":  good + good,
		"far too large":        good + strings.Repeat(" ", 1<<14),
	} {
		t.Run(name, func(t *testing.T) {
			p := journalWorld(t)
			if err := os.WriteFile(journalFile(p), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			_, present, problem := p.readJournal()

			if !present || problem == nil {
				t.Errorf("present %v, problem %v, want a journal that is there and cannot be used", present, problem)
			}
		})
	}
}

func TestTheJournalIsRemovedAndRemovingOneThatIsNotThereIsNoProblem(t *testing.T) {
	p := journalWorld(t)
	if err := p.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}
	if err := p.removeJournal(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(journalFile(p)); err == nil {
		t.Error("the journal is still there")
	}
	if err := p.removeJournal(); err != nil {
		t.Errorf("removing a journal that is not there = %v, want nothing", err)
	}
}

func TestAJournalIsWrittenOverTheOneThatWasLeft(t *testing.T) {
	p := journalWorld(t)
	if err := p.writeJournal(commitOne, commitTwo); err != nil {
		t.Fatal(err)
	}
	if err := p.writeJournal(commitTwo, commitOne); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := p.readJournal(); got.From != commitTwo || got.To != commitOne {
		t.Errorf("journal = %+v, want the last one", got)
	}
}
