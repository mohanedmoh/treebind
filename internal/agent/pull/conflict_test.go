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
	"reflect"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// A checkout and a repository that each gained commits are resolved by the conflict policy of the Treebind
// (docs/adr/0034): the repository wins with followRemote, and the volume wins with followLocal. The unit is
// the file: a file that only one side changed takes that side's change, and a file that both changed takes the
// whole version of the side that wins. Every test is on a real repository.

const (
	localDir   = "refs/treebind/local/"
	firstLocal = localDir + "00000001"
	botName    = "treebind-bot"
	botEmail   = "treebind-bot@users.noreply.treebind.invalid"
	renamed    = "renamed.json"
	onlyHere   = "only-here.json"
	nested     = "x/y.json"
	// batchOfFiles is how many files one command puts in the index, as the test knows it and not as the code does, so that
	// a change of the code is a change that the test sees.
	batchOfFiles = 200
	aFile        = "a file\n"
	modeFile     = "100644"
	modeExec     = "100755"
)

// resolving is the option that gives the Puller a conflict policy and the author of the commits it makes.
func resolving(policy Policy) func(*Options) {
	return func(o *Options) {
		o.Policy = policy
		o.Git.Identity = git.Identity{Name: botName, Email: botEmail}
	}
}

// local commits what the application did in the checkout: the files are written, the names in remove are
// deleted, and one commit holds both. It returns the commit.
func (w *world) local(t *testing.T, set map[string]string, remove ...string) string {
	t.Helper()
	names := make([]string, 0, len(set)+len(remove))
	// The deletions are first: a file can be replaced with a directory of the same name.
	for _, name := range remove {
		drop(t, w.l, name)
		names = append(names, name)
	}
	for name, content := range set {
		gittest.Write(t, w.l, name, content)
		names = append(names, name)
	}
	w.f.InCheckout(w.l, append([]string{"add", "-A", "--"}, names...)...)
	w.f.InCheckout(w.l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "local")
	return w.head()
}

// check runs Git on the checkout and returns its error, for a command that is run for whether it succeeds.
func (w *world) check(args ...string) error {
	_, err := w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree}, args...)
	return err
}

// parents are the parents of a commit.
func (w *world) parents(t *testing.T, commit string) []string {
	t.Helper()
	return strings.Fields(w.f.InCheckout(w.l, "rev-list", "--parents", "-n", "1", commit))[1:]
}

// inTree is a file of a commit: its content without the last line break, and whether the commit has it.
func (w *world) inTree(commit, name string) (string, bool) {
	out, err := w.f.PlainErr(w.l.WorkTree, []string{"GIT_DIR=" + w.l.GitDir, "GIT_WORK_TREE=" + w.l.WorkTree}, "cat-file", "blob", commit+":"+name)
	return strings.TrimSpace(out), err == nil
}

// modeIn is the mode that a commit gives a file, or "" when it has none.
func (w *world) modeIn(t *testing.T, commit, name string) string {
	t.Helper()
	fields := strings.Fields(w.f.InCheckout(w.l, "ls-tree", commit, "--", name))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// replaceBranch replaces the branch of the repository with a history that has another root, with a.json and c.json.
func (w *world) replaceBranch(t *testing.T) {
	t.Helper()
	other := gittest.TempDir(t)
	w.f.Plain(other, "init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(other, fileA), []byte(aChanged), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, fileC), []byte(contentC), 0o644); err != nil {
		t.Fatal(err)
	}
	w.f.Plain(other, "add", "-A")
	w.f.Plain(other, "commit", "-q", "-m", "another root")
	w.f.Plain(other, "push", "-q", "--force", w.f.Bare, "HEAD:refs/heads/main")
}

// remoteRefs is every ref of the repository, with its commit.
func (w *world) remoteRefs(t *testing.T) string {
	t.Helper()
	return w.f.Plain(w.f.Bare, "for-each-ref", "--format=%(refname) %(objectname)")
}

// refsUnder lists the refs of the checkout under a prefix, by name.
func (w *world) refsUnder(t *testing.T, prefix string) []ref {
	t.Helper()
	var found []ref
	for line := range strings.Lines(w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname) %(objectname)", prefix)) {
		name, commit, _ := strings.Cut(strings.TrimSpace(line), " ")
		found = append(found, ref{name, commit})
	}
	return found
}

// wantLocalRefs fails the test unless the refs that keep the commits of the checkout are the ones given.
func (w *world) wantLocalRefs(t *testing.T, want ...ref) {
	t.Helper()
	if got := w.refsUnder(t, localDir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refs that keep the local commits = %v, want %v", got, want)
	}
}

// indexIsClean reports whether the index holds what the commit the checkout is at holds.
func (w *world) indexIsClean() bool {
	return w.check("diff-index", "--cached", "--quiet", "HEAD") == nil
}

// wantFiles fails the test unless every file is in the work tree with the content, and a file whose content
// is "" is not there.
func wantFiles(t *testing.T, w *world, want map[string]string) {
	t.Helper()
	for name, content := range want {
		data, err := os.ReadFile(filepath.Join(w.l.WorkTree, name))
		switch {
		case content == "" && err == nil:
			t.Errorf("%s is there with %q, want it gone", name, data)
		case content != "" && (err != nil || string(data) != content):
			t.Errorf("%s = %q, %v, want %q", name, data, err, content)
		}
	}
}

// wantResolution fails the test unless the result says that the conflict was resolved as given.
func wantResolution(t *testing.T, got Result, want Resolution) {
	t.Helper()
	if got.Resolution == nil {
		t.Fatalf("result = %+v, want a resolution", got)
	}
	if !reflect.DeepEqual(*got.Resolution, want) {
		t.Errorf("resolution = %+v, want %+v", *got.Resolution, want)
	}
}

// wantRemote fails the test unless the repository has the refs that it had.
func (w *world) wantRemote(t *testing.T, before string) {
	t.Helper()
	if after := w.remoteRefs(t); after != before {
		t.Errorf("the refs of the repository are now\n%s\nwant\n%s\nthe agent never writes to the repository when it pulls", after, before)
	}
}

func TestFollowRemoteTakesTheRepositorysVersionOfAFileBothSidesChangedAndKeepsTheCommitsOfTheCheckout(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub, resolving(FollowRemote))
		local := w.local(t, map[string]string{fileA: mine})
		tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
		remote := w.remoteRefs(t)

		got, err := w.pull(t)

		if err != nil || got.Outcome != Resolved || got.From != local || got.To != tip || got.Branch != branchMain || got.Recovery != "" {
			t.Fatalf("result = %+v, %v, want Resolved from %s to the tip %s, with no recovery ref", got, err, local, tip)
		}
		wantResolution(t, got, Resolution{Policy: FollowRemote, LocalCommits: 1, RemoteCommits: 1, Overridden: 1, Local: firstLocal})
		if w.head() != tip {
			t.Errorf("HEAD = %s, want the tip of the repository %s: nothing of the checkout was left to put on it", w.head(), tip)
		}
		wantFiles(t, w, map[string]string{fileA: aChanged, fileB: contentB})
		if dirty := w.dirty(t); dirty != "" {
			t.Errorf("the checkout has changes after the resolution: %s", dirty)
		}
		w.wantLocalRefs(t, ref{firstLocal, local})
		w.wantRefs(t)
		if content, ok := w.inTree(local, fileA); !ok || content != strings.TrimSpace(mine) {
			t.Errorf("the commit that the local ref keeps has a.json = %q, %v, want the application's version", content, ok)
		}
		w.wantRemote(t, remote)
		if !w.indexIsClean() {
			t.Error("the index holds files that are staged")
		}
	})
}

func TestFollowLocalKeepsTheVolumesVersionOfAFileBothSidesChangedAndTheHistoryOfTheRepository(t *testing.T) {
	eachLayout(t, func(t *testing.T, sub string) {
		w := newWorld(t, sub, resolving(FollowLocal))
		local := w.local(t, map[string]string{fileA: mine})
		tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged})
		remote := w.remoteRefs(t)

		got, err := w.pull(t)

		if err != nil || got.Outcome != Resolved || got.From != local || got.Recovery != "" {
			t.Fatalf("result = %+v, %v, want Resolved from %s", got, err, local)
		}
		head := w.head()
		if got.To != head || head == tip || head == local {
			t.Errorf("To = %s and HEAD = %s, want a new commit", got.To, head)
		}
		wantResolution(t, got, Resolution{Policy: FollowLocal, LocalCommits: 1, RemoteCommits: 1, Overridden: 1, Commit: head})
		if got, want := fmt.Sprint(w.parents(t, head)), fmt.Sprint([]string{local, tip}); got != want {
			t.Errorf("the parents of the new commit are %s, want %s: the history of both sides is kept", got, want)
		}
		wantFiles(t, w, map[string]string{fileA: mine, fileB: contentB})
		if dirty := w.dirty(t); dirty != "" {
			t.Errorf("the checkout has changes after the resolution: %s", dirty)
		}
		// The repository's tip is an ancestor of the new commit, so that what is pushed is a fast-forward.
		if err := w.check("merge-base", "--is-ancestor", tip, head); err != nil {
			t.Errorf("the tip of the repository is not an ancestor of the new commit: %v", err)
		}
		w.wantLocalRefs(t)
		w.wantRemote(t, remote)
	})
}

func TestEachPolicyPutsTheFilesThatOnlyTheOtherSideChangedOnTheSideThatWins(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			eachLayout(t, func(t *testing.T, sub string) {
				w := newWorld(t, sub, resolving(policy))
				local := w.local(t, map[string]string{fileA: mine})
				tip := w.f.Publish(branchMain, map[string]string{fileB: bChanged})
				remote := w.remoteRefs(t)

				got, err := w.pull(t)

				if err != nil || got.Outcome != Resolved {
					t.Fatalf("result = %+v, %v, want Resolved", got, err)
				}
				head := w.head()
				wantFiles(t, w, map[string]string{fileA: mine, fileB: bChanged})
				if dirty := w.dirty(t); dirty != "" {
					t.Errorf("the checkout has changes after the resolution: %s", dirty)
				}
				if a, _ := w.inTree(head, fileA); a != strings.TrimSpace(mine) {
					t.Errorf("a.json of the new commit = %q, want the application's", a)
				}
				if b, _ := w.inTree(head, fileB); b != strings.TrimSpace(bChanged) {
					t.Errorf("b.json of the new commit = %q, want the repository's", b)
				}
				switch policy {
				case FollowRemote:
					// One commit on top of the repository's tip: the history of the repository stays a line.
					if fmt.Sprint(w.parents(t, head)) != fmt.Sprint([]string{tip}) {
						t.Errorf("the parents of the new commit are %v, want only the tip %s", w.parents(t, head), tip)
					}
					wantResolution(t, got, Resolution{Policy: policy, LocalCommits: 1, RemoteCommits: 1, Carried: 1, Commit: head, Local: firstLocal})
					w.wantLocalRefs(t, ref{firstLocal, local})
				case FollowLocal:
					if fmt.Sprint(w.parents(t, head)) != fmt.Sprint([]string{local, tip}) {
						t.Errorf("the parents of the new commit are %v, want the checkout and the tip", w.parents(t, head))
					}
					wantResolution(t, got, Resolution{Policy: policy, LocalCommits: 1, RemoteCommits: 1, Carried: 1, Commit: head})
					w.wantLocalRefs(t)
				}
				w.wantRemote(t, remote)
			})
		})
	}
}

func TestTheCommitOfAResolutionIsByTheAuthorTheRunnerWasGiven(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.local(t, map[string]string{fileA: mine})
			w.f.Publish(branchMain, map[string]string{fileB: bChanged})

			if _, err := w.pull(t); err != nil {
				t.Fatal(err)
			}

			got := w.f.InCheckout(w.l, "log", "-1", "--format=%an <%ae> | %cn <%ce>")
			if want := fmt.Sprintf("%s <%s> | %s <%s>", botName, botEmail, botName, botEmail); got != want {
				t.Errorf("author and committer = %q, want %q", got, want)
			}
			if subject := w.f.InCheckout(w.l, "log", "-1", "--format=%s"); !strings.HasPrefix(subject, "treebind: ") {
				t.Errorf("subject = %q, want it to begin with treebind: as the commits of the agent do", subject)
			}
		})
	}
}

func TestADeleteIsAChangeLikeAnyOtherWhenBothSidesChangeTheSameFile(t *testing.T) {
	cases := []struct {
		name   string
		local  func(t *testing.T, w *world)
		remote map[string]string
		// want is the content of a.json after each policy: "" is not there.
		followRemote, followLocal string
	}{
		{
			name:         "the application deletes and the repository edits",
			local:        func(t *testing.T, w *world) { w.local(t, nil, fileA) },
			remote:       map[string]string{fileA: aChanged},
			followRemote: aChanged,
			followLocal:  "",
		},
		{
			name:         "the application edits and the repository deletes",
			local:        func(t *testing.T, w *world) { w.local(t, map[string]string{fileA: mine}) },
			remote:       map[string]string{fileA: ""},
			followRemote: "",
			followLocal:  mine,
		},
	}
	for _, c := range cases {
		for _, policy := range []Policy{FollowRemote, FollowLocal} {
			t.Run(c.name+", "+string(policy), func(t *testing.T) {
				w := newWorld(t, subPath, resolving(policy))
				c.local(t, w)
				w.f.Publish(branchMain, c.remote)

				got, err := w.pull(t)

				if err != nil || got.Outcome != Resolved {
					t.Fatalf("result = %+v, %v, want Resolved", got, err)
				}
				want := c.followRemote
				if policy == FollowLocal {
					want = c.followLocal
				}
				wantFiles(t, w, map[string]string{fileA: want, fileB: contentB})
				if got.Resolution.Overridden != 1 {
					t.Errorf("overridden = %d, want 1: a file that one side deleted and the other edited is a file both changed", got.Resolution.Overridden)
				}
				if dirty := w.dirty(t); dirty != "" {
					t.Errorf("the checkout has changes after the resolution: %s", dirty)
				}
			})
		}
	}
}

func TestARenameIsADeleteAndAnAddAndNotAHunkMergedIntoTheOtherName(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			gittest.Write(t, w.l, renamed, contentA)
			drop(t, w.l, fileA)
			w.f.InCheckout(w.l, "add", "-A", "--", renamed, fileA)
			w.f.InCheckout(w.l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "rename")
			w.f.Publish(branchMain, map[string]string{fileA: aChanged})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("result = %+v, %v, want Resolved", got, err)
			}
			if policy == FollowRemote {
				// a.json is the repository's, and the name the application gave the file is kept.
				wantFiles(t, w, map[string]string{fileA: aChanged, renamed: contentA})
				wantResolution(t, got, Resolution{Policy: policy, LocalCommits: 1, RemoteCommits: 1, Overridden: 1, Carried: 1,
					Commit: w.head(), Local: firstLocal})
			} else {
				wantFiles(t, w, map[string]string{fileA: "", renamed: contentA})
			}
		})
	}
}

// executable makes a file executable and commits it, as the application does with a script that it makes.
func (w *world) executable(t *testing.T, name string) {
	t.Helper()
	if err := os.Chmod(filepath.Join(w.l.WorkTree, name), 0o755); err != nil {
		t.Fatal(err)
	}
	w.f.InCheckout(w.l, "add", "--", name)
	w.f.InCheckout(w.l, "-c", "user.name=App", "-c", "user.email=app@example.com", "commit", "-q", "-m", "mode")
}

func TestAModeChangeThatOnlyOneSideMadeIsKeptWhicheverSideItIs(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.executable(t, fileB)
			w.f.Publish(branchMain, map[string]string{fileA: aChanged})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Carried != 1 {
				t.Fatalf("result = %+v, %v, want Resolved with one file carried", got, err)
			}
			if mode := w.modeIn(t, w.head(), fileB); mode != modeExec {
				t.Errorf("the mode of b.json = %s, want %s: a mode that only the application changed is kept", mode, modeExec)
			}
			info, err := os.Stat(filepath.Join(w.l.WorkTree, fileB))
			if err != nil || info.Mode()&0o111 == 0 {
				t.Errorf("b.json in the directory = %v, %v, want it executable", info, err)
			}
			wantFiles(t, w, map[string]string{fileA: aChanged, fileB: contentB})
		})
	}
}

func TestAModeChangeAndAnEditOfTheSameFileAreOneConflictAndTheWinnerTakesBoth(t *testing.T) {
	cases := []struct {
		policy  Policy
		mode    string
		content string
	}{
		{FollowRemote, modeFile, bChanged},
		{FollowLocal, modeExec, contentB},
	}
	for _, c := range cases {
		t.Run(string(c.policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(c.policy))
			w.executable(t, fileB)
			w.f.Publish(branchMain, map[string]string{fileB: bChanged})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Overridden != 1 {
				t.Fatalf("result = %+v, %v, want Resolved with one file overridden", got, err)
			}
			if mode := w.modeIn(t, w.head(), fileB); mode != c.mode {
				t.Errorf("the mode of b.json = %s, want %s: the winner's version is the whole of the file, the mode too", mode, c.mode)
			}
			wantFiles(t, w, map[string]string{fileB: c.content})
		})
	}
}

func TestACheckoutAndARepositoryThatMadeTheSameChangeAreResolvedWithNothingToOverride(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			local := w.local(t, map[string]string{fileA: aChanged})
			tip := w.f.Publish(branchMain, map[string]string{fileA: aChanged})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Overridden != 0 || got.Resolution.Carried != 0 {
				t.Fatalf("result = %+v, %v, want Resolved with nothing overridden and nothing carried", got, err)
			}
			wantFiles(t, w, map[string]string{fileA: aChanged})
			if policy == FollowRemote {
				if w.head() != tip {
					t.Errorf("HEAD = %s, want the tip %s: there is nothing of the checkout's to put on it", w.head(), tip)
				}
			} else if got, want := fmt.Sprint(w.parents(t, w.head())), fmt.Sprint([]string{local, tip}); got != want {
				t.Errorf("the parents are %s, want %s: the history of the repository has to be in the history of the branch", got, want)
			}
		})
	}
}

func TestAResolutionOfManyFilesIsMadeInSeveralCommands(t *testing.T) {
	const files, localOnly, remoteOnly, both = 450, 330, 100, 20
	name := func(i int) string { return fmt.Sprintf("many/f%03d.json", i) }
	base := map[string]string{}
	for i := range files {
		base[name(i)] = fmt.Sprintf("base %d\n", i)
	}
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			// The second command that puts files in the index leaves a mark: the files are not put in one command.
			marked := filepath.Join(gittest.TempDir(t), "marked")
			stand := makeTrap(t, trap{sub: subUpdateIndex, nth: 2, run: ": > '" + marked + "'"})
			w := newWorldOf(t, subPath, base, resolving(policy), func(o *Options) { o.Git.Git = stand.binary })
			mineAll, theirsAll := map[string]string{}, map[string]string{}
			want := map[string]string{}
			for i := range files {
				want[name(i)] = base[name(i)]
			}
			for i := range localOnly {
				mineAll[name(i)] = fmt.Sprintf("mine %d\n", i)
				want[name(i)] = mineAll[name(i)]
			}
			for i := files - remoteOnly; i < files; i++ {
				theirsAll[name(i)] = fmt.Sprintf("theirs %d\n", i)
				want[name(i)] = theirsAll[name(i)]
			}
			for i := localOnly; i < localOnly+both; i++ {
				mineAll[name(i)] = fmt.Sprintf("mine %d\n", i)
				theirsAll[name(i)] = fmt.Sprintf("theirs %d\n", i)
				want[name(i)] = mineAll[name(i)]
				if policy == FollowRemote {
					want[name(i)] = theirsAll[name(i)]
				}
			}
			w.local(t, mineAll)
			w.f.Publish(branchMain, theirsAll)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Overridden != both {
				t.Fatalf("result = %+v, %v, want Resolved with %d files overridden", got, err, both)
			}
			carried := localOnly
			if policy == FollowLocal {
				carried = remoteOnly
			}
			if got.Resolution.Carried != carried {
				t.Errorf("carried = %d, want %d", got.Resolution.Carried, carried)
			}
			wantFiles(t, w, want)
			if carried > batchOfFiles && !gittest.Exists(marked) {
				t.Errorf("%d files were put in the index with one command, want several of at most %d", carried, batchOfFiles)
			}
			if dirty := w.dirty(t); dirty != "" {
				t.Errorf("the checkout has changes after the resolution: %.200s", dirty)
			}
			// What the new commit holds is what the directory holds.
			for file, content := range want {
				if blob, _ := w.inTree(w.head(), file); blob != strings.TrimSpace(content) {
					t.Fatalf("%s of the new commit = %q, want %q", file, blob, content)
				}
			}
		})
	}
}

func TestAFileTheCheckoutAddedWhereTheRepositoryMadeAFileOfTheDirectoryIsLeftInTheLocalRef(t *testing.T) {
	// The repository adds a file named x, and the application adds x/y: both cannot be in one tree. The
	// repository wins, and the application's file is in the ref that keeps its commits and in the message.
	w := newWorld(t, subPath, resolving(FollowRemote))
	local := w.local(t, map[string]string{nested: mine, fileA: mine})
	w.f.Publish(branchMain, map[string]string{"x": aFile})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Resolved {
		t.Fatalf("result = %+v, %v, want Resolved", got, err)
	}
	wantFiles(t, w, map[string]string{"x": aFile, fileA: mine})
	if !reflect.DeepEqual(got.Resolution.Dropped, []string{nested}) || got.Resolution.Carried != 1 {
		t.Errorf("dropped = %v and carried = %d, want x/y.json dropped and a.json carried", got.Resolution.Dropped, got.Resolution.Carried)
	}
	if content, ok := w.inTree(local, nested); !ok || content != strings.TrimSpace(mine) {
		t.Errorf("the ref that keeps the local commits does not have x/y.json: %q, %v", content, ok)
	}
}

func TestAResolutionOfRepositoriesThatShareNoCommitTakesTheEmptyTreeAsTheBase(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			w.local(t, map[string]string{fileA: mine, onlyHere: mine})
			w.replaceBranch(t)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved {
				t.Fatalf("result = %+v, %v, want Resolved", got, err)
			}
			if policy == FollowRemote {
				wantFiles(t, w, map[string]string{fileA: aChanged, fileC: contentC, onlyHere: mine, fileB: contentB})
			} else {
				wantFiles(t, w, map[string]string{fileA: mine, fileC: contentC, onlyHere: mine, fileB: contentB})
			}
		})
	}
}

func TestACheckoutThatIsOnlyAheadOrAtTheTipIsNotResolvedWhateverThePolicy(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			local := w.local(t, map[string]string{fileA: mine})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Ahead || got.To != local || got.Resolution != nil {
				t.Fatalf("result = %+v, %v, want Ahead and no resolution", got, err)
			}
			w.wantLocalRefs(t)
		})
	}
}

func TestAPullerWithoutAPolicyStillSaysDivergedAndWritesNothing(t *testing.T) {
	w := newWorld(t, subPath)
	local := w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{fileA: aChanged})

	_, err := w.pull(t)

	wantReason(t, err, ReasonDiverged)
	if w.head() != local || gittest.Read(t, w.l, fileA) != mine || len(w.refsUnder(t, "refs/treebind/")) != 0 || !w.indexIsClean() {
		t.Error("a pull with no conflict policy changed the checkout, the refs or the index")
	}
}

func TestTheCheckoutIsAheadOfTheRepositoryAfterAFollowLocalResolutionAndHasNothingToPull(t *testing.T) {
	// followLocal: the new commit has the tip of the repository as an ancestor, so the next pull has nothing to do.
	w := newWorld(t, subPath, resolving(FollowLocal))
	w.local(t, map[string]string{fileA: mine})
	w.f.Publish(branchMain, map[string]string{fileB: bChanged})
	if _, err := w.pull(t); err != nil {
		t.Fatal(err)
	}

	got, err := w.pull(t)

	if err != nil || got.Outcome != Ahead {
		t.Fatalf("the pull after the resolution = %+v, %v, want Ahead: the commit is a fast-forward of the repository", got, err)
	}
}

func TestNewRefusesAPolicyThatIsNotOneOfTheTwoAndANegativeNumberOfLocalRefs(t *testing.T) {
	w := newWorld(t, subPath)
	for name, change := range map[string]func(*Options){
		"a policy that is not one": func(o *Options) { o.Policy = "merge" },
		"negative local refs":      func(o *Options) { o.LocalRefs = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			o := Options{Layout: w.l, URL: w.f.URL, Branch: branchMain, Git: w.f.GitOptions()}
			change(&o)
			if _, err := New(o); err == nil {
				t.Error("New accepted it")
			}
		})
	}
	if p := puller(t, w.f, w.l); p.o.Policy != "" || p.o.LocalRefs != DefaultLocalRefs {
		t.Errorf("policy = %q and local refs = %d, want none and %d", p.o.Policy, p.o.LocalRefs, DefaultLocalRefs)
	}
}

func TestAFileThatOnlyTheSideThatLostDeletedIsDeletedOnTheSideThatWins(t *testing.T) {
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(policy))
			// The loser deletes b.json, which the winner leaves alone, and the winner changes a.json.
			if policy == FollowRemote {
				w.local(t, nil, fileB)
				w.f.Publish(branchMain, map[string]string{fileA: aChanged})
			} else {
				w.local(t, map[string]string{fileA: mine})
				w.f.Publish(branchMain, map[string]string{fileB: ""})
			}

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Carried != 1 {
				t.Fatalf("result = %+v, %v, want Resolved with one file carried", got, err)
			}
			want := map[string]string{fileA: aChanged, fileB: ""}
			if policy == FollowLocal {
				want[fileA] = mine
			}
			wantFiles(t, w, want)
			if _, ok := w.inTree(w.head(), fileB); ok {
				t.Error("b.json is in the commit of the resolution")
			}
		})
	}
}

func TestAFileThatTheLoserReplacedWithADirectoryIsCarriedWhole(t *testing.T) {
	// The loser deletes the file x and adds x/y.json. The deletion is made first, so the file is not in the way.
	for _, policy := range []Policy{FollowRemote, FollowLocal} {
		t.Run(string(policy), func(t *testing.T) {
			w := newWorldOf(t, subPath, map[string]string{fileA: contentA, "x": aFile}, resolving(policy))
			if policy == FollowRemote {
				w.local(t, map[string]string{nested: mine}, "x")
				w.f.Publish(branchMain, map[string]string{fileA: aChanged})
			} else {
				w.local(t, map[string]string{fileA: mine})
				// Two commits, because a map is written in no order and the file has to go before the directory is made.
				w.f.Publish(branchMain, map[string]string{"x": ""})
				w.f.Publish(branchMain, map[string]string{nested: mine})
			}

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || len(got.Resolution.Dropped) != 0 || got.Resolution.Carried != 2 {
				t.Fatalf("result = %+v, %v, want Resolved with two files carried and none dropped", got, err)
			}
			wantFiles(t, w, map[string]string{nested: mine})
			if info, err := os.Stat(filepath.Join(w.l.WorkTree, "x")); err != nil || !info.IsDir() {
				t.Errorf("x = %v, %v, want a directory", info, err)
			}
		})
	}
}

func TestTheSameContentWithAnotherModeIsAConflictOfTheFile(t *testing.T) {
	cases := []struct {
		policy Policy
		mode   string
	}{
		{FollowRemote, modeFile},
		{FollowLocal, modeExec},
	}
	for _, c := range cases {
		t.Run(string(c.policy), func(t *testing.T) {
			w := newWorld(t, subPath, resolving(c.policy))
			gittest.Write(t, w.l, fileB, "same\n")
			w.executable(t, fileB)
			w.f.Publish(branchMain, map[string]string{fileB: "same\n"})

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.Resolution.Overridden != 1 {
				t.Fatalf("result = %+v, %v, want Resolved with one file overridden", got, err)
			}
			if mode := w.modeIn(t, w.head(), fileB); mode != c.mode {
				t.Errorf("the mode of b.json = %s, want %s", mode, c.mode)
			}
		})
	}
}

func TestAFileTheCheckoutAddedWhereTheRepositoryMadeADirectoryIsLeftInTheLocalRef(t *testing.T) {
	// The application adds a file named x, and the repository adds x/y.json.
	w := newWorld(t, subPath, resolving(FollowRemote))
	local := w.local(t, map[string]string{"x": aFile, fileA: mine})
	w.f.Publish(branchMain, map[string]string{nested: mine})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Resolved {
		t.Fatalf("result = %+v, %v, want Resolved", got, err)
	}
	wantFiles(t, w, map[string]string{nested: mine, fileA: mine})
	if !reflect.DeepEqual(got.Resolution.Dropped, []string{"x"}) || got.Resolution.Carried != 1 {
		t.Errorf("dropped = %v and carried = %d, want x dropped and a.json carried", got.Resolution.Dropped, got.Resolution.Carried)
	}
	if content, ok := w.inTree(local, "x"); !ok || content != strings.TrimSpace(aFile) {
		t.Errorf("the ref that keeps the local commits does not have the file x: %q, %v", content, ok)
	}
}

func TestTheResolutionSaysHowManyCommitsEachSideHad(t *testing.T) {
	w := newWorld(t, subPath, resolving(FollowRemote))
	w.local(t, map[string]string{fileA: mine})
	w.local(t, map[string]string{fileLocal: mine})
	w.f.Publish(branchMain, map[string]string{fileB: bChanged})
	w.f.Publish(branchMain, map[string]string{fileB: again})
	w.f.Publish(branchMain, map[string]string{fileD: contentD})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Resolved || got.Resolution.LocalCommits != 2 || got.Resolution.RemoteCommits != 3 {
		t.Fatalf("result = %+v, %v, want Resolved with 2 commits of the checkout and 3 of the repository", got, err)
	}
}

func TestWhenNothingThatTheCheckoutChangedCanBePutOnTheTipNoCommitIsMadeWithFollowRemote(t *testing.T) {
	cases := []struct {
		name          string
		local, remote map[string]string
		dropped       string
		wantFile      string
		wantContent   string
	}{
		{"the repository made a file where the application made a directory", map[string]string{nested: mine}, map[string]string{"x": aFile}, nested, "x", aFile},
		{"the repository made a directory where the application made a file", map[string]string{"x": aFile}, map[string]string{nested: mine}, "x", nested, mine},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, subPath, resolving(FollowRemote))
			local := w.local(t, c.local)
			tip := w.f.Publish(branchMain, c.remote)
			remote := w.remoteRefs(t)

			got, err := w.pull(t)

			if err != nil || got.Outcome != Resolved || got.To != tip {
				t.Fatalf("result = %+v, %v, want Resolved, at the tip %s", got, err, tip)
			}
			// The commit that would be made has the tree of the tip, so it would push nothing and say that it kept files.
			wantResolution(t, got, Resolution{Policy: FollowRemote, LocalCommits: 1, RemoteCommits: 1, Dropped: []string{c.dropped}, Local: firstLocal})
			if w.head() != tip {
				t.Errorf("HEAD = %s, want the tip %s: no commit is made when nothing survives", w.head(), tip)
			}
			wantFiles(t, w, map[string]string{c.wantFile: c.wantContent})
			if dirty := w.dirty(t); dirty != "" {
				t.Errorf("the checkout has changes after the resolution: %s", dirty)
			}
			w.wantLocalRefs(t, ref{firstLocal, local})
			w.wantRemote(t, remote)
		})
	}
}

func TestFollowLocalStillMakesTheMergeWhenNothingOfTheRepositoryCanBeCarried(t *testing.T) {
	// The history of the repository has to be in the branch, whatever is carried: the merge is the fast-forward.
	w := newWorld(t, subPath, resolving(FollowLocal))
	local := w.local(t, map[string]string{"x": aFile})
	tip := w.f.Publish(branchMain, map[string]string{nested: mine})

	got, err := w.pull(t)

	if err != nil || got.Outcome != Resolved || got.Resolution.Commit == "" || got.Resolution.Commit != w.head() {
		t.Fatalf("result = %+v, %v, want Resolved with the merge commit", got, err)
	}
	if fmt.Sprint(w.parents(t, w.head())) != fmt.Sprint([]string{local, tip}) || !reflect.DeepEqual(got.Resolution.Dropped, []string{nested}) {
		t.Errorf("parents = %v and dropped = %v, want the checkout and the tip, and %s dropped", w.parents(t, w.head()), got.Resolution.Dropped, nested)
	}
}
