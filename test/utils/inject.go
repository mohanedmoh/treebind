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

package utils

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

// The functions below run the verbs of hack/inject.sh, which causes a failure in a container from the node
// that runs it, through a privileged pod with hostPID that it starts there when there is none. The script
// refuses a kubectl context that is not a kind cluster. Each returns what the script says it did: the pid,
// the ID of the container and its cgroup, for the log of the spec.

// KillContainer sends SIGKILL to the first process of a container of a pod, as an out-of-memory kill
// does. The kubelet starts the container again in the same pod.
func KillContainer(namespace, pod, container string) (string, error) {
	return inject("kill", namespace, pod, container)
}

// FreezeContainer freezes every process of a container of a pod with the freezer of cgroup v2, until
// ThawContainer lets them run again.
func FreezeContainer(namespace, pod, container string) (string, error) {
	return inject("freeze", namespace, pod, container)
}

// ThawContainer lets the processes of a container that FreezeContainer froze run again.
func ThawContainer(namespace, pod, container string) (string, error) {
	return inject("thaw", namespace, pod, container)
}

// TmpfsDir is where hack/inject.sh mounts the tmpfs of a name on a node, the directory that a PersistentVolume of
// type local points at.
func TmpfsDir(name string) string { return "/var/lib/treebind-inject/" + name }

// MountTmpfs mounts a tmpfs of a few mebibytes (at most 64) on a node, in the node's mount namespace at
// TmpfsDir(name). It is a volume that fills without filling the disk of the machine that runs the cluster: the
// claims of a storage class that makes directories, as local-path does on kind, are directories on that disk and
// have no size to fill.
func MountTmpfs(node, name string, mebibytes int) (string, error) {
	return injectVerb("tmpfs-mount", node, name, strconv.Itoa(mebibytes))
}

// FillTmpfs writes zeros into a file at the root of the tmpfs until at most the bytes given are free, rounded up to a
// block. It never writes more than the tmpfs holds, and refuses a directory that is not the mount point of a tmpfs.
func FillTmpfs(node, name string, bytesLeft int64) (string, error) {
	return injectVerb("tmpfs-fill", node, name, strconv.FormatInt(bytesLeft, 10))
}

// FreeTmpfs removes the file that FillTmpfs wrote. It is not an error that there is none.
func FreeTmpfs(node, name string) (string, error) { return injectVerb("tmpfs-free", node, name) }

// TmpfsStatus says how big the tmpfs is and how much of it is free, in the words "size=<bytes> available=<bytes>".
func TmpfsStatus(node, name string) (string, error) { return injectVerb("tmpfs-df", node, name) }

// UnmountTmpfs unmounts the tmpfs that MountTmpfs mounted. It is not an error that none is mounted.
func UnmountTmpfs(node, name string) (string, error) { return injectVerb("tmpfs-unmount", node, name) }

// RemoveInjector unmounts the tmpfs that MountTmpfs mounted, removes the privileged pods that the verbs start, with
// their namespace, and waits for them to be gone.
func RemoveInjector() error {
	_, err := Run(exec.Command("hack/inject.sh", "cleanup"))
	return err
}

// Watch is a verb of hack/inject.sh that watches for a moment that files mark and acts in it, and that runs until it
// has or its time is up.
type Watch struct {
	command string
	cmd     *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	output  strings.Builder
	err     error
}

// KillAt starts kill-at, which kills the first process of a container of a pod in the moment that the markers say,
// from the node, and returns once the script watches, so that what makes the moment can be done after it. A marker
// is +<path> for a file that has to be there and -<path> for one that must not, as the container sees them.
func KillAt(namespace, pod, container string, timeout time.Duration, markers ...string) (*Watch, error) {
	return startWatch(append([]string{"kill-at", namespace, pod, container, seconds(timeout)}, markers...))
}

// KillGitAt starts kill-git-at, which kills a Git that the first process of a container started, and whose arguments
// hold the word, in the moment that the markers say, from inside the container. It returns once the script watches.
func KillGitAt(
	namespace, pod, container string, timeout time.Duration, word string, markers ...string,
) (*Watch, error) {
	return startWatch(append([]string{"kill-git-at", namespace, pod, container, seconds(timeout), word}, markers...))
}

// Wait waits for the script to end, for up to limit, and returns what it said: what it saw and what it struck, or why
// it did not.
func (w *Watch) Wait(limit time.Duration) (string, error) {
	select {
	case <-w.done:
	case <-time.After(limit):
		// The script on the node ends by itself when its time is up, and lets the container go on.
		_ = w.cmd.Process.Kill()
		return w.said(), fmt.Errorf("%q did not end within %s", w.command, limit)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.output.String(), fmt.Errorf("%q failed: %w", w.command, w.err)
	}
	return w.output.String(), nil
}

func (w *Watch) said() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.String()
}

// startWatch starts the script and reads what it says, and returns once it has said that it watches, or has ended
// without saying so, or has not said so within a few minutes, which the privileged pod has to start in.
func startWatch(args []string) (*Watch, error) {
	cmd := exec.Command("hack/inject.sh", args...)
	dir, _ := GetProjectDir()
	cmd.Dir = dir
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	w := &Watch{command: strings.Join(cmd.Args, " "), cmd: cmd, done: make(chan struct{})}
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", w.command)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%q cannot be started: %w", w.command, err)
	}
	watching := make(chan struct{})
	read := make(chan struct{})
	go func() {
		defer close(read)
		said := false
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			w.mu.Lock()
			w.output.WriteString(scanner.Text() + "\n")
			w.mu.Unlock()
			if !said && strings.HasPrefix(scanner.Text(), "watching ") {
				said = true
				close(watching)
			}
		}
		_, _ = io.Copy(io.Discard, reader)
	}()
	go func() {
		err := cmd.Wait()
		_ = writer.Close()
		<-read
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
		close(w.done)
	}()
	select {
	case <-watching:
		return w, nil
	case <-w.done:
		return nil, fmt.Errorf("%q ended before it watched (%v); it said %q", w.command, w.err, w.said())
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("%q did not watch within 5m; it said %q", w.command, w.said())
	}
}

// seconds is a timeout in the whole seconds of the script, and at least one.
func seconds(timeout time.Duration) string {
	return strconv.Itoa(max(1, int(timeout/time.Second)))
}

func inject(verb, namespace, pod, container string) (string, error) {
	return injectVerb(verb, namespace, pod, container)
}

func injectVerb(args ...string) (string, error) {
	output, err := Run(exec.Command("hack/inject.sh", args...))
	return strings.TrimSpace(output), err
}
