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

package git

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What a Git command that is stopped in the middle of a write leaves behind (docs/adr/0028). The tests
// start the real Git, hold it inside a write, stop it in the way the Runner does, and look at the Git
// directory. They are run on the Git of the developer and on the one in the image of the agent.
//
// Git is held with what a shell has built in: a hook of reference-transaction that waits for a file that
// nobody opens, which holds the locks of the refs that the transaction updates, and a smudge filter that
// waits in the middle of the files an update writes, which holds the lock of the index. Both are set on
// the command line of the one command, and are not what the Runner runs: it takes them out.

// A stop is the signal a test sends to the process group of the held command.
type stop struct {
	name   string
	signal syscall.Signal
}

var (
	stopTerm = stop{"SIGTERM", syscall.SIGTERM}
	stopKill = stop{"SIGKILL", syscall.SIGKILL}
)

// held is a Git command that waits inside a write.
type held struct {
	t    *testing.T
	cmd  *exec.Cmd
	done chan struct{}
	out  *strings.Builder
	fifo string
}

// holdHook starts the command with a hook that waits when a transaction of refs is prepared and one of
// them is ref, and returns once it waits. The locks of the refs are held until it is stopped.
func (r *repo) holdHook(ref string, args ...string) *held {
	r.t.Helper()
	dir := filepath.Join(r.root, "hooks")
	r.scriptAt(filepath.Join(dir, "reference-transaction"), `if [ "$1" = prepared ]; then
  while read old new name; do
    if [ "$name" = "$HOLD_REF" ]; then
      echo waiting > "$HOLD_FLAG"
      read nothing < "$HOLD_FIFO"
    fi
  done
fi
exit 0`)
	return r.hold(ref, "", []string{"-c", "core.hooksPath=" + dir}, args)
}

// holdFiles starts the command with a smudge filter that waits when it is asked for the file, and returns
// once it waits. The command is in the middle of the files it writes, and holds the lock of the index.
func (r *repo) holdFiles(file string, args ...string) *held {
	r.t.Helper()
	smudge := r.script("smudge", `if [ "$1" = "$HOLD_FILE" ]; then
  echo waiting > "$HOLD_FLAG"
  read nothing < "$HOLD_FIFO"
fi
while IFS= read -r line; do printf '%s\n' "$line"; done`)
	attributes := filepath.Join(r.root, "attributes")
	if err := os.WriteFile(attributes, []byte("* filter=hold\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return r.hold("", file, []string{"-c", "core.attributesFile=" + attributes, "-c", "filter.hold.smudge=" + smudge + " %f"}, args)
}

func (r *repo) hold(ref, file string, settings, args []string) *held {
	r.t.Helper()
	fifo := filepath.Join(r.root, "fifo")
	flag := filepath.Join(r.root, "flag")
	_ = os.Remove(fifo)
	_ = os.Remove(flag)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		r.t.Fatal(err)
	}
	env := r.env("HOLD_REF="+ref, "HOLD_FILE="+file, "HOLD_FLAG="+flag, "HOLD_FIFO="+fifo)
	cmd := r.plainCmd(env, append(settings, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	h := &held{t: r.t, cmd: cmd, done: make(chan struct{}), out: &strings.Builder{}, fifo: fifo}
	cmd.Stdout, cmd.Stderr = h.out, h.out
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(h.done)
	}()
	r.t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(flag); err == nil {
			return h
		}
		select {
		case <-h.done:
			r.t.Fatalf("git %s ended before it waited: %s", strings.Join(args, " "), h.out)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("git %s did not wait: %s", strings.Join(args, " "), h.out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// release lets the held command go on, and waits for it to end.
func (h *held) release() {
	h.t.Helper()
	// Opening the file that the hook or the filter reads, and closing it, ends its wait.
	w, err := os.OpenFile(h.fifo, os.O_WRONLY, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = w.Close()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		h.t.Fatalf("the command did not end once it was released: %s", h.out)
	}
}

// stop sends the signal to the process group of the command and waits for it to end.
func (h *held) stop(s stop) {
	h.t.Helper()
	if err := syscall.Kill(-h.cmd.Process.Pid, s.signal); err != nil {
		h.t.Fatal(err)
	}
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		h.t.Fatalf("the command did not end after %s", s.name)
	}
}

// locksIn lists the lock files in the Git directory, relative to it.
func locksIn(t *testing.T, gitDir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(gitDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(d.Name(), ".lock") {
			rel, _ := filepath.Rel(gitDir, path)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}

// commit makes a commit with the files of the work tree, as the agent's own repository, which has no
// identity in its config, can.
func (r *repo) commit(message string) {
	r.t.Helper()
	r.plain(cmdAdd, "-A")
	r.plain("-c", "user.name=T", "-c", "user.email=t@example.com", cmdCommit, "-q", "-m", message)
}

// updatePending makes a branch "next" that is one commit ahead of main, with the files a, b and c
// changed and d added, and leaves the checkout on main, so that "merge --ff-only next" is an update.
func (r *repo) updatePending() {
	r.t.Helper()
	r.write("b.txt", "b1\n")
	r.write("c.txt", "c1\n")
	r.commit("second")
	r.write("a.txt", "a2\n")
	r.write("b.txt", "b2\n")
	r.write("c.txt", "c2\n")
	r.write("d.txt", "d2\n")
	r.commit("third")
	r.plain("branch", "next")
	r.plain("reset", "-q", "--hard", "HEAD~1")
}

// What each held command is, and the locks that it holds.
type heldCase struct {
	name string
	// hold starts the command and returns it, held.
	hold func(r *repo) *held
	// locks are the lock files that the command holds.
	locks []string
	// status is what the work tree looks like once it is stopped: the lines of status --porcelain.
	status string
}

const (
	lockOrigHead   = "ORIG_HEAD.lock"
	lockBranch     = "refs/heads/main.lock"
	lockHead       = "HEAD.lock"
	lockRecovery   = "refs/treebind/recovery/00000001.lock"
	lockRemoteRef  = "refs/remotes/origin/main.lock"
	lockPackedRefs = "packed-refs.lock"
	lockConfig     = "config.lock"
	refRecovery    = "refs/treebind/recovery/00000001"
	statusPartial  = " M a.txt\n D b.txt\n"
	statusStaged   = "M  a.txt\nM  b.txt\nM  c.txt\nA  d.txt\n"
)

// heldCases are the writes that the agent makes with Git, held in the middle: the update of a pull in
// each of its three steps, the ref that keeps the commit before it, its removal, and a fetch.
func heldCases() []heldCase {
	merge := []string{cmdMerge, "--quiet", "--ff-only", "--no-overwrite-ignore", "next"}
	return []heldCase{
		{"merge at the update of ORIG_HEAD", func(r *repo) *held { return r.holdHook("ORIG_HEAD", merge...) },
			[]string{lockOrigHead}, ""},
		{"merge in the middle of the files", func(r *repo) *held { return r.holdFiles("b.txt", merge...) },
			[]string{indexLock}, statusPartial},
		{"merge at the update of the branch", func(r *repo) *held { return r.holdHook("refs/heads/main", merge...) },
			[]string{lockHead, lockBranch}, statusStaged},
		{"update-ref of a new ref", func(r *repo) *held {
			return r.holdHook(refRecovery, "update-ref", refRecovery, refHead, "")
		}, []string{lockRecovery}, ""},
		{"update-ref -d of a ref", func(r *repo) *held {
			r.plain("update-ref", refRecovery, refHead)
			return r.holdHook(refRecovery, "update-ref", "-d", refRecovery)
		}, []string{lockPackedRefs, lockRecovery}, ""},
		{"fetch into a remote ref", func(r *repo) *held {
			origin := filepath.Join(r.root, "origin.git")
			// A repository of its own, so that the environment of the checkout is not given to it.
			if out, err := exec.Command(r.git, "init", "-q", "--bare", "--template=", origin).CombinedOutput(); err != nil {
				r.t.Fatalf("git init: %v: %s", err, out)
			}
			r.plain("push", "-q", origin, "next:main")
			return r.holdHook("refs/remotes/origin/main", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
				"--", origin, "+refs/heads/main:refs/remotes/origin/main")
		}, []string{lockRemoteRef}, ""},
	}
}

func TestAStoppedGitCommandLeavesItsLocksOnlyWhenItIsKilled(t *testing.T) {
	for _, c := range heldCases() {
		for _, s := range []stop{stopTerm, stopKill} {
			t.Run(c.name+" "+s.name, func(t *testing.T) {
				r := agentRepo(t)
				r.updatePending()
				h := c.hold(r)
				if got := locksIn(t, r.gitDir); !slices.Equal(got, c.locks) {
					t.Fatalf("the held command has the locks %v, want %v", got, c.locks)
				}
				h.stop(s)

				want := []string(nil)
				if s == stopKill {
					want = c.locks
				}
				if got := locksIn(t, r.gitDir); !slices.Equal(got, want) {
					t.Errorf("after %s the locks are %v, want %v", s.name, got, want)
				}
				// The work tree is what the command had written by then, whichever way it was stopped.
				if got := r.plain("status", "--porcelain"); got != c.status {
					t.Errorf("after %s status is %q, want %q", s.name, got, c.status)
				}
			})
		}
	}
}

func TestTheNextMergeGoesOnOnceTheLocksAreGoneUnlessTheFilesWereBeingWritten(t *testing.T) {
	for _, c := range heldCases()[:3] {
		t.Run(c.name, func(t *testing.T) {
			r := agentRepo(t)
			r.updatePending()
			c.hold(r).stop(stopKill)
			for _, lock := range locksIn(t, r.gitDir) {
				if err := os.Remove(filepath.Join(r.gitDir, lock)); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.plainErr(r.env(), cmdMerge, "--quiet", "--ff-only", "--no-overwrite-ignore", "next")
			if c.status == statusPartial {
				if err == nil || !strings.Contains(out, "would be overwritten") {
					t.Errorf("merge = %v, %q, want it refused for the files that were written", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("merge = %v, %q, want the update applied", err, out)
			}
			if head, next := r.plain("rev-parse", refHead), r.plain("rev-parse", "next"); head != next {
				t.Errorf("HEAD is %s, want %s", strings.TrimSpace(head), strings.TrimSpace(next))
			}
		})
	}
}

func TestAGitDirectoryWithTheLocksOfAKilledCommandIsStillAccepted(t *testing.T) {
	for _, c := range heldCases() {
		t.Run(c.name, func(t *testing.T) {
			r := agentRepo(t)
			r.updatePending()
			c.hold(r).stop(stopKill)
			if err := VerifyGitDir(r.gitDir, r.work); err != nil {
				t.Errorf("VerifyGitDir with the locks %v = %v, want the directory accepted: a lock that Git leaves is one it made", locksIn(t, r.gitDir), err)
			}
		})
	}
}

func TestAHalfWrittenUpdateIsPutBackByCheckingOutItsFilesAndTheNextMergeGoesOn(t *testing.T) {
	// What docs/git-directory.md tells a person to do for an update that was killed in the middle of the files.
	for _, s := range []stop{stopTerm, stopKill} {
		t.Run(s.name, func(t *testing.T) {
			r := agentRepo(t)
			r.updatePending()
			r.holdFiles("b.txt", cmdMerge, "--quiet", "--ff-only", "--no-overwrite-ignore", "next").stop(s)
			for _, lock := range locksIn(t, r.gitDir) {
				if err := os.Remove(filepath.Join(r.gitDir, lock)); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.plain("status", "--porcelain"); got != statusPartial {
				t.Fatalf("status = %q, want the update half written, %q", got, statusPartial)
			}

			r.plain(cmdCheckout, "--", "a.txt", "b.txt")
			if got := r.plain("status", "--porcelain"); got != "" {
				t.Fatalf("status = %q after the files were checked out, want a clean checkout", got)
			}
			if out, err := r.plainErr(r.env(), cmdMerge, "--quiet", "--ff-only", "--no-overwrite-ignore", "next"); err != nil {
				t.Fatalf("merge = %v, %q, want the update applied", err, out)
			}
			if head, next := r.plain("rev-parse", refHead), r.plain("rev-parse", "next"); head != next || r.plain("status", "--porcelain") != "" {
				t.Errorf("HEAD is %s, want %s, with a clean checkout", strings.TrimSpace(head), strings.TrimSpace(next))
			}
		})
	}
}

func TestALockFileThatIsRemovedUnderAGitCommandIsReplacedByWhatIsPutThere(t *testing.T) {
	// Why a lock file is not removed while a command may hold it (docs/adr/0028): the command does not look
	// again at what is under the name of its lock, and renames it over the file that the lock is for.
	r := agentRepo(t)
	r.updatePending()
	h := r.holdFiles("b.txt", cmdMerge, "--quiet", "--ff-only", "--no-overwrite-ignore", "next")
	lock := filepath.Join(r.gitDir, indexLock)
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("another index"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.release()

	index, err := os.ReadFile(filepath.Join(r.gitDir, "index"))
	if err != nil || string(index) != "another index" {
		t.Errorf("index = %q, %v, want what was put under the name of the lock, which the command renamed over it", index, err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the lock file is still there: the command did not rename it")
	}
}
