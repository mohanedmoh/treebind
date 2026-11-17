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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/gittest"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// What is done about a file that a cut-off update changed, for each state that the file can be in. A write that is
// cut off in the middle of a file cannot be made to happen on cue, even with a kill, because Git writes a file with
// one write, so the state of a file that is the start of the new one is put in place by hand. The checkout, the
// journal, the repair and the pull that follows are real, on a real repository.

const (
	longA     = "a2, a line that is longer than the one before\n"
	prefixA   = "a2, a line"
	newDFile  = "d, a file that the update adds\n"
	oldDFile  = "d, as the application had it\n"
	elsewhere = "target.txt"
	// A blob that a Runner with the small limit of the tests cannot hand over.
	bigBlob  = 2048
	bigLimit = 512
)

// plantedUpdate makes a checkout, publishes the update, fetches its commit as the pull that began it did, and writes the
// journal that the pull wrote, so that the work tree can be put in a state by hand.
func plantedUpdate(t *testing.T, sub string, files map[string]string, publish func(f *gittest.Fixture) string, change ...func(*Options)) *cut {
	t.Helper()
	w := newWorldOf(t, sub, files, change...)
	from := w.head()
	to := publish(w.f)
	w.f.InCheckout(w.l, "fetch", "--quiet", "--no-tags", "--", w.f.Bare, "+refs/heads/main:refs/remotes/origin/main")
	if err := w.puller.writeJournal(from, to); err != nil {
		t.Fatal(err)
	}
	return &cut{world: w, from: from, to: to}
}

func put(t *testing.T, l layout.Layout, name, content string) {
	t.Helper()
	gittest.Write(t, l, name, content)
}

func plantLink(t *testing.T, l layout.Layout, name, target string) {
	t.Helper()
	path := filepath.Join(l.WorkTree, name)
	_ = os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func drop(t *testing.T, l layout.Layout, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(l.WorkTree, name)); err != nil {
		t.Fatal(err)
	}
}

// stateCase is one state of one file of the update.
type stateCase struct {
	name string
	// plant puts the work tree in the state.
	plant func(t *testing.T, l layout.Layout)
	// want is the file after the repair: its content, or "" when it is not there, or "->target" for a link.
	want string
	// noted is what the note says of it, and notNoted what it does not say.
	noted, notNoted []string
	// next is what the next pull does: advance, or block.
	blocked bool
}

func runStateCases(t *testing.T, file string, setup func(t *testing.T) *cut, cases []stateCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := setup(t)
			c.plant(t, u.l)

			note, failure := u.puller.repair(context.Background(), u.head())

			if failure != nil {
				t.Fatal(failure)
			}
			if got := observed(u.l, file); got != c.want {
				t.Errorf("%s is %q after the repair, want %q", file, got, c.want)
			}
			for _, want := range c.noted {
				if !strings.Contains(note, want) {
					t.Errorf("note = %q, want it to say %q", note, want)
				}
			}
			for _, not := range c.notNoted {
				if strings.Contains(note, not) {
					t.Errorf("note = %q, want it not to say %q", note, not)
				}
			}
			if journalPresent(u.world) || u.head() != u.from {
				t.Errorf("journal present %v, HEAD %s, want the journal removed and HEAD where it was: a repair applies no update", journalPresent(u.world), u.head())
			}

			got, err := u.pull(t)
			if c.blocked {
				if e := errorOf(t, err, ReasonBlocked); !strings.Contains(e.Message, file) {
					t.Errorf("the pull was refused with %q, want it to name %s", e.Message, file)
				}
				return
			}
			if err != nil || got.Outcome != Advanced || got.To != u.to {
				t.Errorf("pull = %+v, %v, want the update applied", got, err)
			}
		})
	}
}

// observed says what is at the path: the content of a file, "" when there is nothing, and "->target" for a link.
func observed(l layout.Layout, name string) string {
	path := filepath.Join(l.WorkTree, name)
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return ""
	case info.Mode()&os.ModeSymlink != 0:
		target, _ := os.Readlink(path)
		return "->" + target
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestAFileThatTheUpdateChangedIsPutBackWhenItIsTheAgentsOwnAndLeftWhenItIsNot(t *testing.T) {
	setup := func(t *testing.T) *cut {
		return plantedUpdate(t, subPath, map[string]string{fileA: oldA, fileB: oldB},
			func(f *gittest.Fixture) string { return f.Publish(branchMain, map[string]string{fileA: longA}) })
	}
	runStateCases(t, fileA, setup, []stateCase{
		{name: "as it was", plant: func(*testing.T, layout.Layout) {}, want: oldA, noted: []string{"0 file(s) put back"}},
		{name: "as the update has it", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, longA) }, want: oldA,
			noted: []string{"1 file(s) put back"}},
		{name: "the start of the one the update has", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, prefixA) }, want: oldA,
			noted: []string{"1 file(s) put back"}},
		{name: "empty", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, "") }, want: oldA, noted: []string{"1 file(s) put back"}},
		{name: "not there", plant: func(t *testing.T, l layout.Layout) { drop(t, l, fileA) }, want: oldA, noted: []string{"1 file(s) put back"}},
		{name: "an edit of the application", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, edited) }, want: edited,
			noted: []string{"0 file(s) put back", "1 file(s) are left as they are", fileA}, blocked: true},
		{name: "the start of the one that was there", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, "a1") }, want: "a1",
			noted: []string{"are left as they are", fileA}, blocked: true},
		{name: "longer than the one the update has", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, longA+"and more\n") },
			want: longA + "and more\n", noted: []string{"are left as they are", fileA}, blocked: true},
		{name: "a link, where the commits have a file", plant: func(t *testing.T, l layout.Layout) { plantLink(t, l, fileA, elsewhere) },
			want: "->" + elsewhere, noted: []string{"are left as they are", fileA}, blocked: true},
	})
}

func TestAFileThatTheUpdateAddedIsRemovedWhenItIsTheAgentsOwnAndLeftWhenItIsNot(t *testing.T) {
	setup := func(t *testing.T) *cut {
		return plantedUpdate(t, subPath, map[string]string{fileA: oldA},
			func(f *gittest.Fixture) string { return f.Publish(branchMain, map[string]string{fileD: newDFile}) })
	}
	runStateCases(t, fileD, setup, []stateCase{
		{name: "not there", plant: func(*testing.T, layout.Layout) {}, want: "", noted: []string{"0 file(s) put back, 0 removed"}},
		{name: "as the update has it", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileD, newDFile) }, want: "",
			noted: []string{"0 file(s) put back, 1 removed"}},
		{name: "the start of the one the update has", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileD, newDFile[:10]) }, want: "",
			noted: []string{"1 removed"}},
		{name: "empty", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileD, "") }, want: "", noted: []string{"1 removed"}},
		{name: "a file of the application", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileD, oldDFile) }, want: oldDFile,
			noted: []string{"0 removed", "are left as they are", fileD}, blocked: true},
		{name: "a link, where the update adds a file", plant: func(t *testing.T, l layout.Layout) { plantLink(t, l, fileD, elsewhere) },
			want: "->" + elsewhere, noted: []string{"are left as they are", fileD}, blocked: true},
	})
}

func TestAFileThatTheUpdateDeletedIsLeftAsItIsAndTheNextPullDeletesIt(t *testing.T) {
	setup := func(t *testing.T) *cut {
		return plantedUpdate(t, subPath, map[string]string{fileA: oldA, fileE: oldE},
			func(f *gittest.Fixture) string { return f.Publish(branchMain, map[string]string{fileE: ""}) })
	}
	runStateCases(t, fileE, setup, []stateCase{
		{name: "as it was", plant: func(*testing.T, layout.Layout) {}, want: oldE, notNoted: []string{fileE}},
		{name: "already gone", plant: func(t *testing.T, l layout.Layout) { drop(t, l, fileE) }, want: "", notNoted: []string{fileE}},
		{name: "an edit of the application", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileE, edited) }, want: edited,
			notNoted: []string{fileE}, blocked: true},
	})
}

func TestALinkThatTheUpdateAddedIsRemovedWhenItPointsWhereTheUpdateSaysAndLeftWhenItDoesNot(t *testing.T) {
	setup := func(t *testing.T) *cut {
		return plantedUpdate(t, subPath, map[string]string{fileA: oldA},
			func(f *gittest.Fixture) string { return f.PublishLink(branchMain, "l.json", elsewhere) })
	}
	runStateCases(t, "l.json", setup, []stateCase{
		{name: "not there", plant: func(*testing.T, layout.Layout) {}, want: "", noted: []string{"0 removed"}},
		{name: "as the update has it", plant: func(t *testing.T, l layout.Layout) { plantLink(t, l, "l.json", elsewhere) }, want: "",
			noted: []string{"1 removed"}},
		{name: "pointing somewhere else", plant: func(t *testing.T, l layout.Layout) { plantLink(t, l, "l.json", "other.txt") }, want: "->other.txt",
			noted: []string{"are left as they are", "l.json"}, blocked: true},
		{name: "a file that holds the target", plant: func(t *testing.T, l layout.Layout) { put(t, l, "l.json", elsewhere) }, want: elsewhere,
			noted: []string{"are left as they are", "l.json"}, blocked: true},
	})
}

func TestABlobThatTheRunnerCannotHandOverIsRecognisedWhenWholeAndNeverAsAStart(t *testing.T) {
	big := strings.Repeat("x", bigBlob)
	setup := func(t *testing.T) *cut {
		return plantedUpdate(t, subPath, map[string]string{fileA: oldA},
			func(f *gittest.Fixture) string { return f.Publish(branchMain, map[string]string{fileA: big}) },
			func(o *Options) { o.Git.MaxOutput = bigLimit })
	}
	runStateCases(t, fileA, setup, []stateCase{
		{name: "whole", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, big) }, want: oldA, noted: []string{"1 file(s) put back"}},
		{name: "the start of it", plant: func(t *testing.T, l layout.Layout) { put(t, l, fileA, big[:1000]) }, want: big[:1000],
			noted: []string{"could not be compared and are left as they are", fileA}, blocked: true},
		{name: "as it was", plant: func(*testing.T, layout.Layout) {}, want: oldA, notNoted: []string{"could not be compared"}},
	})
}

func TestFilesWithNamesThatAreNotPlainArePutBackToo(t *testing.T) {
	names := []string{"sp ace/f g.json", "dir/ünï.json", "-dash.json", "[glob]*.json"}
	before, after := map[string]string{}, map[string]string{}
	for _, name := range names {
		before[name], after[name] = "old "+name+"\n", "new "+name+", longer\n"
	}
	u := plantedUpdate(t, subPath, before, func(f *gittest.Fixture) string { return f.Publish(branchMain, after) })
	for _, name := range names {
		put(t, u.l, name, after[name])
	}

	note, failure := u.puller.repair(context.Background(), u.head())

	if failure != nil || !strings.Contains(note, "4 file(s) put back") {
		t.Fatalf("repair = %q, %v, want the four files put back", note, failure)
	}
	for _, name := range names {
		if got := observed(u.l, name); got != before[name] {
			t.Errorf("%s is %q, want it as it was", name, got)
		}
	}
	if u.dirty(t) != "" {
		t.Errorf("status = %q, want a clean checkout", u.dirty(t))
	}
}

func TestAnUpdateThatChangesManyFilesPutsThemBackInSeveralCommands(t *testing.T) {
	before, after := map[string]string{}, map[string]string{}
	for i := range checkoutBatch + 20 {
		name := "many/f" + strings.Repeat("0", 3-len(strconv.Itoa(i))) + strconv.Itoa(i) + ".json"
		before[name], after[name] = "old\n", "new, longer\n"
	}
	u := plantedUpdate(t, subPath, before, func(f *gittest.Fixture) string { return f.Publish(branchMain, after) })
	for name, content := range after {
		put(t, u.l, name, content)
	}

	note, failure := u.puller.repair(context.Background(), u.head())

	if failure != nil || !strings.Contains(note, strconv.Itoa(checkoutBatch+20)+" file(s) put back") || u.dirty(t) != "" {
		t.Errorf("repair = %q, %v, status %q, want every file put back", note, failure, u.dirty(t))
	}
}
