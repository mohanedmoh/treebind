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

package checkout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/subtree"
)

const (
	repoPath    = "grafana/dashboards"
	otherPath   = "grafana/alerts"
	fileOutside = "other/z.txt"

	changedContent = "changed\n"

	wholeRepository = "whole repository"
)

// repositoryWithDirectories has the directory of the tests, another one and files outside both, as a repository does.
var repositoryWithDirectories = map[string]string{
	repoPath + "/" + fileA: contentA,
	repoPath + "/" + fileB: contentB,
	otherPath + "/c.json":  "c\n",
	fileOutside:            "z\n",
	"README.md":            "readme\n",
}

// the files that the directory of the tests has, as the synced directory holds them
var directoryFiles = map[string]string{fileA: contentA, fileB: contentB}

func (f *fixture) pathOptions(l layout.Layout, path string) Options {
	o := f.options(l, branchMain)
	o.Path = path
	return o
}

func openPath(t *testing.T, f *fixture, l layout.Layout, path string) (Result, error) {
	t.Helper()
	return Open(t.Context(), f.pathOptions(l, path))
}

// The directory of the repository is the synced directory: its files are at the root of it, and nothing else of the repository is on
// the volume. The branch of the checkout is a snapshot, the repository's tip is kept as it is, and the checkout says which
// directory it was made for.
func TestOpenClonesTheDirectoryOfTheRepositoryIntoTheRootOfTheSyncedDirectory(t *testing.T) {
	for _, sub := range []string{subPath, "team/" + subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			f := newFixture(t)
			tip := f.publish(branchMain, repositoryWithDirectories)
			l := f.layout(sub)

			got, err := openPath(t, f, l, repoPath)

			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != Cloned || got.Branch != branchMain {
				t.Errorf("result = %+v, want Cloned on %s", got, branchMain)
			}
			wantFiles(t, l, directoryFiles)
			if head := f.inCheckout(l, "rev-parse", "HEAD^{tree}"); head != strings.TrimSpace(f.plain(f.scrat, "rev-parse", tip+":"+repoPath)) {
				t.Errorf("the tree of the checkout is %s, not the one of the directory", head)
			}
			if got.Commit != f.inCheckout(l, "rev-parse", "HEAD") || got.Commit == tip {
				t.Errorf("commit = %s, want the snapshot and not the repository's %s", got.Commit, tip)
			}
			if real := f.inCheckout(l, "rev-parse", subtree.RealRef(branchMain)); real != tip {
				t.Errorf("the repository's tip is kept as %s, want %s", real, tip)
			}
			if status := f.inCheckout(l, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, fileA) {
				t.Errorf("status = %q, want the clone clean", status)
			}
			if made, err := os.ReadFile(filepath.Join(l.StateDir, pathMarkerName)); err != nil || !strings.Contains(string(made), repoPath) {
				t.Errorf("the directory the checkout was made for is %q, %v; want %s", made, err, repoPath)
			}
		})
	}
}

func TestATreebindWithNoPathLeavesNoMarkerOfOne(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if _, err := open(t, f, l, branchMain); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(l.StateDir, pathMarkerName)) {
		t.Error("a checkout of the whole repository has the marker of a directory")
	}
	if refs := f.inCheckout(l, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "treebind") {
		t.Errorf("a checkout of the whole repository has refs of the snapshots: %s", refs)
	}
}

func TestOpenAdoptsAVolumeThatHoldsExactlyTheFilesOfTheDirectory(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	for name, content := range directoryFiles {
		write(t, l, name, content)
	}

	got, err := openPath(t, f, l, repoPath)

	if err != nil || got.Outcome != Adopted {
		t.Fatalf("Open = %+v, %v; want Adopted", got, err)
	}
	wantFiles(t, l, directoryFiles)
}

// A volume that holds the whole repository, which is what a Treebind that sets the path has today, does not match the directory:
// every file stays where it is, and the refusal is the one of every directory that holds more than the branch.
func TestOpenRefusesAVolumeThatHoldsTheWholeRepositoryAndLeavesItAsItIs(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	for name, content := range repositoryWithDirectories {
		write(t, l, name, content)
	}

	_, err := openPath(t, f, l, repoPath)

	wantReason(t, err, ReasonDirectoryNotEmpty)
	wantFiles(t, l, repositoryWithDirectories)
	if entries, readErr := os.ReadDir(l.GitDir); readErr != nil || len(entries) != 0 {
		t.Errorf("the Git directory holds %d entries after the refusal, want none: %v", len(entries), readErr)
	}
	if exists(filepath.Join(l.StateDir, pathMarkerName)) {
		t.Error("a refused first checkout left the marker of a directory")
	}
}

// A directory that the repository does not have, or has as a file, is refused with a reason of its own, and a start that is
// refused leaves nothing: the next one judges the volume afresh.
func TestOpenRefusesADirectoryThatTheBranchDoesNotHaveAndLeavesNothing(t *testing.T) {
	for _, path := range []string{"grafana/nope", "README.md", repoPath + "/" + fileA} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, repositoryWithDirectories)
			l := f.layout(subPath)

			_, err := openPath(t, f, l, path)

			got := errorOf(t, err, ReasonPathNotFound)
			if !strings.Contains(got.Message, path) {
				t.Errorf("message = %q, want it to name %s", got.Message, path)
			}
			wantFiles(t, l, nil)
			if entries, readErr := os.ReadDir(l.GitDir); readErr != nil || len(entries) != 0 {
				t.Errorf("the Git directory holds %d entries after the refusal, want none: %v", len(entries), readErr)
			}
			if exists(filepath.Join(l.StateDir, pathMarkerName)) {
				t.Error("a refused first checkout left the marker of a directory")
			}
		})
	}
}

func TestOpenReopensTheCheckoutOfADirectoryAndFollowsWhatTheRepositoryChangesInIt(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	first, err := openPath(t, f, l, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	f.publish(branchMain, map[string]string{fileOutside: changedContent})

	again, err := openPath(t, f, l, repoPath)

	if err != nil || again.Outcome != Reopened || again.Commit != first.Commit {
		t.Fatalf("Open = %+v, %v; want Reopened at %s", again, err, first.Commit)
	}
	wantFiles(t, l, directoryFiles)
}

// A checkout is made for one directory, or for the whole repository, and a Treebind that names another is refused with every file
// where it is, whichever way the two differ, and whether the repository had a commit at the time or none.
func TestOpenRefusesACheckoutMadeForAnotherDirectoryOrForNone(t *testing.T) {
	tests := []struct {
		name       string
		made, want string
		wantInText []string
		empty      bool
	}{
		{"the whole repository, and now a directory", "", repoPath, []string{wholeRepository, repoPath, "remove repository.path"}, false},
		{"a directory, and now the whole repository", repoPath, "", []string{repoPath, wholeRepository}, false},
		{"a directory, and now another", repoPath, otherPath, []string{repoPath, otherPath}, false},
		{"the whole repository, which had no commit but has one of the application's, and now a directory", "", repoPath, []string{wholeRepository, repoPath}, true},
		{"a directory of a repository that had no commit but has one of the application's, and now another", repoPath, otherPath, []string{repoPath, otherPath}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if !tt.empty {
				f.publish(branchMain, repositoryWithDirectories)
			}
			l := f.layout(subPath)
			if _, err := openPath(t, f, l, tt.made); err != nil {
				t.Fatal(err)
			}
			if !tt.empty {
				want := directoryFiles
				if tt.made == "" {
					want = repositoryWithDirectories
				}
				wantFiles(t, l, want)
			} else {
				// the application's first commit, which a repository that had none takes: the checkout has a commit now, and it is
				// of the directory that it was made for
				write(t, l, "mine.json", mine)
				f.inCheckout(l, "add", "-A")
				f.inCheckout(l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "first")
			}
			before := f.inCheckout(l, "for-each-ref")

			_, err := openPath(t, f, l, tt.want)

			got := errorOf(t, err, ReasonPathMismatch)
			for _, text := range tt.wantInText {
				if !strings.Contains(got.Message, text) {
					t.Errorf("message = %q, want it to say %q", got.Message, text)
				}
			}
			if after := f.inCheckout(l, "for-each-ref"); after != before {
				t.Errorf("the refusal changed the refs:\n%s\n%s", before, after)
			}
		})
	}
}

// A directory of the repository that holds the name of the state directory is the case that the whole claim being synced makes a
// refusal: the file would be written into the state directory. The repository's own root is not the directory, so a file of
// that name there is of no concern, and a path under that name is a directory like another.
func TestOpenJudgesTheStateDirectoryByTheDirectoryAndNotByTheRoot(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, map[string]string{"cfg/" + stateName + "/x": "1\n", stateName + "/y": "2\n", "cfg/a.json": contentA, "keep/" + fileA: contentA, stateName + "/cfg/" + fileA: contentA})
	tests := []struct {
		name, path string
		reason     string
		files      map[string]string
	}{
		{"a directory that holds it", "cfg", ReasonReservedPath, nil},
		{"the root holds it and the directory does not", "keep", "", map[string]string{fileA: contentA}},
		{"a directory under it", stateName + "/cfg", "", map[string]string{fileA: contentA}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := layout.New(tempDir(t), "")
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Prepare(); err != nil {
				t.Fatal(err)
			}
			_, err = openPath(t, f, l, tt.path)
			if tt.reason != "" {
				wantReason(t, err, tt.reason)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantFiles(t, l, tt.files)
		})
	}
}

// A first checkout that stopped after it had fetched and before it wrote a file has the refs of the snapshots in its Git directory,
// which are the agent's own: the next start judges the directory afresh and does not take them for somebody else's.
func TestOpenFinishesAFirstCheckoutOfADirectoryThatStoppedAfterTheFetch(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	if _, err := openPath(t, f, l, repoPath); err != nil {
		t.Fatal(err)
	}
	// the branch is not made yet: what a checkout that was stopped before its last step leaves
	f.inCheckout(l, "update-ref", "-d", headsPrefix+branchMain)

	got, err := openPath(t, f, l, repoPath)

	if err != nil || got.Outcome != Adopted {
		t.Fatalf("Open = %+v, %v; want Adopted: the files are the ones of the directory", got, err)
	}
	wantFiles(t, l, directoryFiles)
}

// The way out that the refusal names, emptying the synced directory and the Git directory, starts a first checkout again. What the
// checkout was made for is what the Treebind names now, whichever way it was before, and the next start reopens it.
func TestAFirstCheckoutAfterStartingAgainIsMadeForWhatTheTreebindNamesNow(t *testing.T) {
	for _, tt := range []struct{ name, made, now string }{
		{"a directory, and now the whole repository", repoPath, ""},
		{"the whole repository, and now a directory", "", repoPath},
		{"a directory, and now another", repoPath, otherPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, repositoryWithDirectories)
			l := f.layout(subPath)
			if _, err := openPath(t, f, l, tt.made); err != nil {
				t.Fatal(err)
			}
			wantReason(t, errOf(openPath(t, f, l, tt.now)), ReasonPathMismatch)

			// what the message says to do
			for _, dir := range []string{l.WorkTree, l.GitDir} {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.Prepare(); err != nil {
				t.Fatal(err)
			}
			if got, err := openPath(t, f, l, tt.now); err != nil || got.Outcome != Cloned {
				t.Fatalf("Open after starting again = %+v, %v; want Cloned", got, err)
			}

			if got, err := openPath(t, f, l, tt.now); err != nil || got.Outcome != Reopened {
				t.Errorf("the next Open = %+v, %v; want the checkout reopened for %q", got, err, tt.now)
			}
		})
	}
}

// errOf returns the error of a call that also returns a result.
func errOf(_ Result, err error) error { return err }

// What a checkout was made for is judged before the repository is asked anything: a Treebind whose repository cannot be reached
// is told that its checkout is for another directory and not that the host is down, and nothing goes over the network for a
// checkout that is refused.
func TestAMismatchIsToldWithoutAskingTheRepository(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	if _, err := openPath(t, f, l, repoPath); err != nil {
		t.Fatal(err)
	}
	options := f.pathOptions(l, otherPath)
	options.URL = "ssh://localhost" + f.root + "/gone.git"

	_, err := Open(t.Context(), options)

	wantReason(t, err, ReasonPathMismatch)
}

// The commit that a person can look up is the repository's: for a directory the result names it beside the snapshot that the
// checkout is at, and names none when the checkout is at a commit of its own.
func TestTheResultNamesTheCommitOfTheRepositoryTheSnapshotIsOf(t *testing.T) {
	f := newFixture(t)
	tip := f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)

	cloned, err := openPath(t, f, l, repoPath)
	if err != nil || cloned.Source != tip || cloned.Commit == tip {
		t.Fatalf("Open = %+v, %v; want Cloned at a snapshot of %s", cloned, err, tip)
	}
	reopened, err := openPath(t, f, l, repoPath)
	if err != nil || reopened.Source != tip {
		t.Errorf("reopened = %+v, %v; want the source %s", reopened, err, tip)
	}

	// a commit of the application on top of the snapshot is no snapshot, and the repository has no commit that it is the files of
	write(t, l, "mine.json", mine)
	f.inCheckout(l, "add", "-A")
	f.inCheckout(l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "local")
	if again, err := openPath(t, f, l, repoPath); err != nil || again.Source != "" {
		t.Errorf("a checkout at a commit of its own = %+v, %v; want no source", again, err)
	}
}

func TestTheResultOfTheWholeRepositoryNamesNoSource(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	got, err := open(t, f, f.layout(subPath), branchMain)
	if err != nil || got.Source != "" {
		t.Errorf("Open = %+v, %v; want no source: the commit is the repository's", got, err)
	}
}

// A Git directory that was made and has no commit is a first checkout that did not finish, whatever it was made for: the volume is
// judged afresh, as the first checkout judges every volume, and the agent does not refuse it for a marker that was never written.
func TestAnUnfinishedFirstCheckoutIsNotRefusedForAMarkerThatWasNeverWritten(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	// what a start leaves that was stopped after the Git directory was made and before the file that says what it is for
	if err := l.MakeWorkTree(); err != nil {
		t.Fatal(err)
	}
	f.inCheckout(l, "init", "-q", "--template=", "--initial-branch="+branchMain)

	got, err := openPath(t, f, l, repoPath)

	if err != nil || got.Outcome != Cloned {
		t.Fatalf("Open = %+v, %v; want the first checkout made", got, err)
	}
	wantFiles(t, l, directoryFiles)
}

// A checkout of a repository that has no commit, and that the application has not committed to yet, holds nothing that a directory
// would write over: another directory is the first checkout again.
func TestACheckoutThatHasNoCommitYetIsMadeAgainForAnotherDirectory(t *testing.T) {
	f := newFixture(t)
	l := f.layout(subPath)
	if got, err := openPath(t, f, l, repoPath); err != nil || got.Commit != "" {
		t.Fatalf("Open = %+v, %v; want a checkout with no commit", got, err)
	}

	got, err := openPath(t, f, l, otherPath)

	if err != nil || got.Outcome != Adopted && got.Outcome != Cloned {
		t.Fatalf("Open = %+v, %v; want the first checkout again, for the directory that the Treebind names now", got, err)
	}
	if made, err := os.ReadFile(filepath.Join(l.StateDir, pathMarkerName)); err != nil || !strings.Contains(string(made), otherPath) {
		t.Errorf("the directory the checkout was made for is %q, %v; want %s", made, err, otherPath)
	}
}

// A change that the repository makes outside the directory moves the tip and no snapshot: the checkout is still at the snapshot it
// was, which is of the directory at the new tip too, and the result names the tip, which is the commit that a person looks up.
func TestTheResultNamesTheTipOfTheRepositoryWhenOnlyAFileOutsideTheDirectoryChanged(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, repositoryWithDirectories)
	l := f.layout(subPath)
	first, err := openPath(t, f, l, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	tip := f.publish(branchMain, map[string]string{fileOutside: changedContent})

	again, err := openPath(t, f, l, repoPath)

	if err != nil || again.Commit != first.Commit || again.Source != tip {
		t.Fatalf("Open = %+v, %v; want the same snapshot, %s, and the new tip %s as the source", again, err, first.Commit, tip)
	}
}
