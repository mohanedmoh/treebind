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

package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// Held is a Git command that a test started and that waits inside a write, holding the lock files
// of it, until the test kills it: what a Git that is killed in the middle of a write leaves behind
// is a lock file that nothing removes (docs/adr/0028).
//
// Git is held with what a shell has built in, because the image of the agent has no sleep: a hook
// of reference-transaction, or a filter, that waits for a file that nobody opens. They are set on
// the command line of this one command, and are not what the Runner runs: it takes them out.
type Held struct {
	t    *testing.T
	cmd  *exec.Cmd
	done chan struct{}
	out  *strings.Builder
}

// HoldRef starts plain Git on the checkout of a layout, with the arguments, and returns once it
// waits with the locks of the refs that it updates held, at the transaction that updates ref.
func (f *Fixture) HoldRef(l layout.Layout, ref string, args ...string) *Held {
	f.T.Helper()
	dir := filepath.Join(f.root, "hold-hooks")
	f.script(filepath.Join(dir, "reference-transaction"), `if [ "$1" = prepared ]; then
  while read old new name; do
    if [ "$name" = "$HOLD_NAME" ]; then
      echo waiting > "$HOLD_FLAG"
      read nothing < "$HOLD_FIFO"
    fi
  done
fi
exit 0`)
	return f.hold(l, ref, []string{"-c", "core.hooksPath=" + dir}, args)
}

// HoldIndex starts plain Git to add the file to the index of the checkout of a layout, and returns
// once it waits with the lock of the index held, in the middle of reading the file. The file has to
// be one that has changed.
func (f *Fixture) HoldIndex(l layout.Layout, file string) *Held {
	f.T.Helper()
	clean := filepath.Join(f.root, "hold-clean")
	f.script(clean, `if [ "$1" = "$HOLD_NAME" ]; then
  echo waiting > "$HOLD_FLAG"
  read nothing < "$HOLD_FIFO"
fi
while IFS= read -r line; do printf '%s\n' "$line"; done`)
	attributes := filepath.Join(f.root, "hold-attributes")
	if err := os.WriteFile(attributes, []byte("* filter=hold\n"), 0o644); err != nil {
		f.T.Fatal(err)
	}
	settings := []string{"-c", "core.attributesFile=" + attributes, "-c", "filter.hold.clean=" + clean + " %f"}
	return f.hold(l, file, settings, []string{"add", "--", file})
}

func (f *Fixture) script(path, body string) {
	f.T.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.T.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		f.T.Fatal(err)
	}
}

func (f *Fixture) hold(l layout.Layout, name string, settings, args []string) *Held {
	f.T.Helper()
	fifo, flag := filepath.Join(f.root, "hold-fifo"), filepath.Join(f.root, "hold-flag")
	_ = os.Remove(fifo)
	_ = os.Remove(flag)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		f.T.Fatal(err)
	}
	cmd := exec.Command(f.git, append(settings, args...)...)
	cmd.Dir = l.WorkTree
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_DIR=" + l.GitDir, "GIT_WORK_TREE=" + l.WorkTree,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"HOLD_NAME=" + name, "HOLD_FLAG=" + flag, "HOLD_FIFO=" + fifo,
	}
	// A group of its own, so that the hook or the filter that Git started is killed with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	h := &Held{t: f.T, cmd: cmd, done: make(chan struct{}), out: &strings.Builder{}}
	cmd.Stdout, cmd.Stderr = h.out, h.out
	if err := cmd.Start(); err != nil {
		f.T.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(h.done)
	}()
	f.T.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(flag); err == nil {
			return h
		}
		select {
		case <-h.done:
			f.T.Fatalf("git %s ended before it waited: %s", strings.Join(args, " "), h.out)
		default:
		}
		if time.Now().After(deadline) {
			f.T.Fatalf("git %s did not wait: %s", strings.Join(args, " "), h.out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Kill kills the command and everything it started with SIGKILL, so that it cannot remove the lock
// files it holds, and waits for it to end.
func (h *Held) Kill() {
	h.t.Helper()
	if err := syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		h.t.Fatal("the command did not end after SIGKILL")
	}
}

// HeldMerge is a stand-in for the git binary that holds the merge of an update in the middle of its
// writes, and that a test kills there. It runs the real Git, with a filter or a hook added to the merge
// that waits at the point that the test names. A Runner that is given Binary as its git runs it as it
// runs any other, and what is added comes after the settings that take filters and hooks out.
type HeldMerge struct {
	t *testing.T
	// Binary is the path of the stand-in.
	Binary string
	flag   string
	pidOf  string
}

// HoldMerge returns a stand-in that holds every merge it runs in the middle of the files that it writes:
// when the merge asks for the file, it has written the files before that one and not the ones after it,
// and it holds the lock of the index.
func (f *Fixture) HoldMerge(file string) *HeldMerge {
	f.T.Helper()
	dir := filepath.Join(f.root, "hold-merge")
	smudge := filepath.Join(dir, "smudge")
	attributes := filepath.Join(dir, "attributes")
	f.script(smudge, `if [ "$1" = "$HOLD_NAME" ]; then
  echo waiting > "$HOLD_FLAG"
  read nothing < "$HOLD_FIFO"
fi
while IFS= read -r line; do printf '%s\n' "$line"; done`)
	if err := os.WriteFile(attributes, []byte("* filter=hold\n"), 0o644); err != nil {
		f.T.Fatal(err)
	}
	return f.holdMerge(dir, file, "core.attributesFile", attributes, "-c", "filter.hold.smudge="+smudge+" %f")
}

// HoldMergeAtRef returns a stand-in that holds every merge it runs at the update of the ref, after it has
// written the files and the index, and holds the locks of the refs.
func (f *Fixture) HoldMergeAtRef(ref string) *HeldMerge {
	f.T.Helper()
	dir := filepath.Join(f.root, "hold-merge")
	f.script(filepath.Join(dir, "hooks", "reference-transaction"), `if [ "$1" = prepared ]; then
  while read old new name; do
    if [ "$name" = "$HOLD_NAME" ]; then
      echo waiting > "$HOLD_FLAG"
      read nothing < "$HOLD_FIFO"
    fi
  done
fi
exit 0`)
	return f.holdMerge(dir, ref, "core.hooksPath", filepath.Join(dir, "hooks"))
}

// holdMerge makes the stand-in. The arguments of the merge are rewritten, with what a shell has built in:
// the setting that takes something away, which is named, is given the file that has what is to be held, and
// what follows it is put in front of the arguments.
func (f *Fixture) holdMerge(dir, name, setting, value string, more ...string) *HeldMerge {
	h := &HeldMerge{t: f.T, Binary: filepath.Join(dir, "git"), flag: filepath.Join(dir, "flag"), pidOf: filepath.Join(dir, "pid")}
	fifo := filepath.Join(dir, "fifo")
	_ = os.Remove(fifo)
	_ = os.Remove(h.flag)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		f.T.Fatal(err)
	}
	extra := ""
	for _, arg := range more {
		extra += " '" + arg + "'"
	}
	f.script(h.Binary, `merging=
for arg in "$@"; do
  if [ "$arg" = merge ]; then merging=yes; fi
done
if [ -z "$merging" ]; then exec '`+f.git+`' "$@"; fi
echo $$ > '`+h.pidOf+`'
count=$#
seen=0
while [ "$seen" -lt "$count" ]; do
  arg=$1
  shift
  if [ "$arg" = `+setting+`=/dev/null ]; then arg=`+setting+`=`+value+`; fi
  set -- "$@" "$arg"
  seen=$((seen + 1))
done
export HOLD_NAME='`+name+`' HOLD_FLAG='`+h.flag+`' HOLD_FIFO='`+fifo+`'
exec '`+f.git+`'`+extra+` "$@"`)
	return h
}

// WaitHeld waits until a merge is held.
func (h *HeldMerge) WaitHeld() {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(h.flag); err == nil {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("no merge was held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Kill kills the held merge and what it started with SIGKILL, as an out-of-memory kill does, so that it
// leaves what it had written and the locks that it held.
func (h *HeldMerge) Kill() {
	h.t.Helper()
	data, err := os.ReadFile(h.pidOf)
	if err != nil {
		h.t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		h.t.Fatal(err)
	}
	// The Runner gives the command a group of its own, whose number is the one of the command.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
}
