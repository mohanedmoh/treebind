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

package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/gittest"
)

// The commands of docs/recovering-from-a-conflict-or-a-rejected-push.md for a push that the host refused are run here as they
// are written, on a real repository whose host refuses a push with a hook, as those of docs/deletion-guard.md are. The commands
// for a conflict are tried in package pull, which has the conflicts.

const (
	recoveryPage = "recovering-from-a-conflict-or-a-rejected-push.md"

	bigFile = "big.bin"

	// hookRefusesBigFile is a hook of the repository that declines a push in which any commit adds the file, as a host does that
	// reads every commit it is sent, so that a commit that removes the file later does not help. It uses what a shell has built in
	// and Git, because the tests run in the image of the agent too.
	hookRefusesBigFile = "#!/bin/sh\nwhile read old new ref; do\n" +
		"  for name in $(git log --diff-filter=A --name-only --format= \"$old..$new\"); do\n" +
		"    case \"$name\" in " + bigFile + ") echo 'error: " + bigFile + " is larger than the limit of this repository' >&2; exit 1;; esac\n" +
		"  done\ndone\n"
)

// newWorldIn is newWorld for a layout with the sub path, which is none when it is empty.
func newWorldIn(t *testing.T, sub string, files map[string]string) *world {
	t.Helper()
	f := gittest.New(t)
	f.Publish(branchMain, files)
	l := f.Layout(sub)
	made, err := checkout.Open(context.Background(), f.CheckoutOptions(l, branchMain))
	if err != nil || made.Outcome != checkout.Cloned {
		t.Fatalf("the checkout was not made: %+v, %v", made, err)
	}
	pusher, err := New(Options{Layout: l, URL: f.URL, Branch: branchMain, Git: f.GitOptions(), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, f: f, l: l, pusher: pusher, fence: func(context.Context) error { return nil }}
}

// commitPaths commits the paths as the commit step does, which is by name: the layout that syncs the whole claim has the state
// directory inside the work tree, which a commit of everything would take.
func (w *world) commitPaths(subject string, paths ...string) string {
	w.t.Helper()
	w.f.InCheckout(w.l, append([]string{"add", "-A", "--"}, paths...)...)
	w.f.InCheckout(w.l, "commit", "-q", "-m", subject)
	return w.head()
}

func (w *world) runPage(name string, programs []string) string {
	w.t.Helper()
	return w.f.RunBlock(w.t, w.l, recoveryPage, name, branchMain, programs)
}

// theLogLine is what the log of the agent has for a check, as the page shows it: the line that logChanges writes, less its time.
func theLogLine(t *testing.T) string {
	t.Helper()
	line := gittest.Output(t, recoveryPage, "the-words-of-a-host")
	_, rest, found := strings.Cut(line, " level=WARN msg=push check=Push outcome=Failed reason=PushRejected message=\"")
	if !found {
		t.Fatalf("the page shows %q, want a line of the log for a push that was rejected", line)
	}
	return strings.TrimSuffix(strings.TrimSpace(rest), "\"")
}

func TestThePageShowsTheWordsOfTheHostAsTheAgentSaysThem(t *testing.T) {
	w := newWorld(t, base)
	w.installHook(hookDeclines)
	w.commit(fileB, "b\n")

	_, err := w.push()

	failure := w.wantFailure(err, ReasonPushRejected)
	if shown := theLogLine(t); shown != failure.Message {
		t.Errorf("the page shows the message %q, the agent says %q", shown, failure.Message)
	}
}

func TestTheCommandsForARefusedPushShowWhatWaitsAndTakeItOffTheBranch(t *testing.T) {
	for _, sub := range []string{subDir, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			w := newWorldIn(t, sub, base)
			w.installHook(hookRefusesBigFile)
			before := w.tracking()
			gittest.Write(t, w.l, bigFile, strings.Repeat("x", 5000))
			w.commitPaths("local "+bigFile, bigFile)
			gittest.Write(t, w.l, fileB, "b\n")
			w.commitPaths("local "+fileB, fileB)
			_, err := w.push()
			failure := w.wantFailure(err, ReasonPushRejected)
			if !strings.Contains(failure.Message, "It said: error: "+bigFile+" is larger than the limit of this repository") {
				t.Fatalf("message = %q, want the words of the host", failure.Message)
			}
			// The file is taken out of the volume, and the scan commits that. The host reads every commit it is sent, so the push
			// is refused again, which is why the page says that a later commit that removes the file does not help.
			if err := os.Remove(filepath.Join(w.l.WorkTree, bigFile)); err != nil {
				t.Fatal(err)
			}
			waiting := w.commitPaths("remove "+bigFile, bigFile)
			_, err = w.push()
			_ = w.wantFailure(err, ReasonPushRejected)

			seen := w.runPage("see-what-waits-to-be-pushed", []string{"sort", "tail"})
			counted := w.runPage("compare-with-the-repository", nil)

			for _, want := range []string{
				"local " + bigFile + "\n", "local " + fileB + "\n", "remove " + bigFile + "\n", // the commits that wait
				" " + bigFile + " |", " " + fileB + " |", // and the files they changed
				"5000 blob " + bigFile + "\n", // the largest object last, with its size
			} {
				if !strings.Contains(seen, want) {
					t.Errorf("output = %q, want it to hold %q", seen, want)
				}
			}
			if !strings.HasSuffix(seen, "5000 blob "+bigFile+"\n") {
				t.Errorf("output = %q, want the largest object last", seen)
			}
			if !strings.HasPrefix(counted, "3\t0\n") {
				t.Errorf("counts = %q, want 3 commits that only your directory has and none that only the repository has", counted)
			}
			if w.tracking() != before || w.head() != waiting {
				t.Error("a command that only reads moved a ref")
			}

			// What the page says to do about it, the file being out of the volume: the commits off the branch.
			w.runPage("drop-the-commits-that-wait", []string{"date"})

			if w.head() != before {
				t.Errorf("HEAD = %s, want %s: the branch is back at what the repository has", w.head(), before)
			}
			if gittest.Read(t, w.l, fileB) != "b\n" || gittest.Exists(filepath.Join(w.l.WorkTree, bigFile)) {
				t.Error("the files are not as they were on the volume")
			}
			status := w.f.InCheckout(w.l, "status", "--porcelain", "--untracked-files=all", "--", fileB, bigFile)
			if status != "?? "+fileB {
				t.Errorf("status = %q, want the file that was committed an untracked file now, for the scan to find", status)
			}
			kept := strings.Fields(w.f.InCheckout(w.l, "for-each-ref", "--format=%(refname) %(objectname)", "refs/treebind/recovery/"))
			if len(kept) != 2 || !strings.HasPrefix(kept[0], "refs/treebind/recovery/by-hand-") || kept[1] != waiting {
				t.Errorf("refs = %q, want one of your own that keeps %s", kept, waiting)
			}
			if err := git.VerifyGitDir(w.l.GitDir, w.l.WorkTree); err != nil {
				t.Errorf("the Git directory is not what the agent makes after the commands: %v", err)
			}
			if gittest.Exists(filepath.Join(w.l.GitDir, "index.lock")) {
				t.Error("the commands left the lock of the index")
			}

			// The push says that there is nothing to push, and the commit of what is left, as the scan makes it, goes through.
			if got := w.mustPush(); got.Outcome != NothingToPush {
				t.Errorf("result = %+v, want NothingToPush", got)
			}
			tip := w.commitPaths("local "+fileB, fileB)
			if got := w.mustPush(); got.Outcome != Pushed || got.To != tip || w.remoteTip() != tip {
				t.Errorf("result = %+v, repository at %s, want the commit of the file that was left pushed", got, w.remoteTip())
			}
		})
	}
}

func TestARepositoryThatMovedShowsInTheCountsOnlyAfterAFetch(t *testing.T) {
	w := newWorld(t, base)
	before := w.tracking()
	w.commit(fileB, "b\n")
	w.f.Publish(branchMain, map[string]string{fileA: "a, changed\n"})

	_, err := w.push()

	_ = w.wantFailure(err, ReasonNonFastForward)
	// The host refused the push, and the page says that this does not move the ref that the counts are made against.
	if counted := w.runPage("compare-with-the-repository", nil); w.tracking() != before || !strings.HasPrefix(counted, "1\t0\n") {
		t.Errorf("tracking = %s, counts = %q, want the ref where it was and 1 commit that only your directory has", w.tracking(), counted)
	}
	// What the pull that follows does first.
	w.f.InCheckout(w.l, "fetch", "-q", w.f.Bare, "+refs/heads/"+branchMain+":refs/remotes/origin/"+branchMain)
	if counted := w.runPage("compare-with-the-repository", nil); !strings.HasPrefix(counted, "1\t1\n") {
		t.Errorf("counts = %q, want 1 commit on each side once the repository's is fetched", counted)
	}
}
