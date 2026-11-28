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

package layout

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The modes that the kubelet adds to a volume of a pod with an fsGroup (pkg/volume/volume_linux.go of Kubernetes).
const (
	kubeletRWMask   fs.FileMode = 0o660
	kubeletExecMask fs.FileMode = 0o110
)

// kubeletFSGroup does to the claim at root what the kubelet does to the volume of a pod with an fsGroup, as
// SetVolumeOwnership in pkg/volume/volume_linux.go of Kubernetes does it. With the policy OnRootMismatch it does
// nothing when the root already has the group, group read, write and execute, and setgid. Otherwise it walks every
// file without following a link, gives it the group, and adds group read and write, and to a directory group execute
// and setgid as well; a link keeps its mode. It reports whether it walked. The group is the test's own, the one group
// that a user who is not root may give a file.
func kubeletFSGroup(t *testing.T, root string, onRootMismatch bool) bool {
	t.Helper()
	gid := os.Getgid()
	if onRootMismatch {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		want := kubeletRWMask | kubeletExecMask
		if int(stat.Gid) == gid && info.Mode().Perm()&want == want && info.Mode()&fs.ModeSetgid != 0 {
			return false
		}
	}
	err := filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, -1, gid); err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		mask := kubeletRWMask
		if info.IsDir() {
			mask |= fs.ModeSetgid | kubeletExecMask
		}
		return os.Chmod(path, info.Mode()|mask)
	})
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func mustNewLayout(t *testing.T, root string) Layout {
	t.Helper()
	l, err := New(root, "")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The kubelet opens the agent's own directories to the group of the fsGroup when it walks a claim on which the agent
// has started before, and the agent refuses them then, as it refuses any private directory that the group may write:
// it cannot tell the kubelet from another user of the group. A walk before the first start, on a claim that has no
// state directory yet, does not stop it: what the agent makes after the walk is closed to the group, also under a
// root that is setgid. With OnRootMismatch the kubelet walks only while the root does not have the group, so the walk
// of the first start is the last one, and the agent starts again on the same claim.
func TestAnFSGroupWalkOfTheKubeletAndTheStateDirectory(t *testing.T) {
	t.Run("a claim on which the agent has started before is refused after a walk", func(t *testing.T) {
		root := t.TempDir()
		l := mustNewLayout(t, root)
		if err := l.Prepare(); err != nil {
			t.Fatal(err)
		}
		kubeletFSGroup(t, root, false)

		err := l.Prepare()
		var unsafe *UnsafeError
		if !errors.As(err, &unsafe) || unsafe.Path != l.StateDir || !strings.Contains(unsafe.Problem, "open to group") {
			t.Fatalf("Prepare() = %v, want the state directory refused as open to the group", err)
		}
	})

	t.Run("a walk before the first start leaves what the agent makes after it private", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "app.conf"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		kubeletFSGroup(t, root, false)
		l := mustNewLayout(t, root)
		if err := l.Prepare(); err != nil {
			t.Fatalf("Prepare() after a walk of a claim without a state directory = %v, want nil", err)
		}
		info, err := os.Lstat(l.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&fs.ModeSetgid == 0 || info.Mode().Perm() != privateMode {
			t.Errorf("the state directory under a setgid root has mode %v, want setgid and %04o", info.Mode(), privateMode)
		}
	})

	for _, policy := range []struct {
		name           string
		onRootMismatch bool
		wantWalk       bool
	}{
		{"Always walks again at the next start, and the agent refuses", false, true},
		{"OnRootMismatch does not walk again, and the agent starts", true, false},
	} {
		t.Run(policy.name, func(t *testing.T) {
			root := t.TempDir()
			kubeletFSGroup(t, root, policy.onRootMismatch)
			l := mustNewLayout(t, root)
			if err := l.Prepare(); err != nil {
				t.Fatal(err)
			}

			walked := kubeletFSGroup(t, root, policy.onRootMismatch)
			if walked != policy.wantWalk {
				t.Fatalf("the second mount walked = %v, want %v", walked, policy.wantWalk)
			}
			err := l.Prepare()
			if refused := errors.As(err, new(*UnsafeError)); refused != policy.wantWalk {
				t.Errorf("Prepare() at the second start = %v, want refused = %v", err, policy.wantWalk)
			}
		})
	}
}

// What the walk does to the files of the application: each gets group read and write, and none gets or loses an
// execute bit of its owner, which is the one bit of the mode that Git records (100755 or 100644), so Git sees no
// change of mode.
func TestAnFSGroupWalkKeepsTheExecuteBitsOfFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]fs.FileMode{"run.sh": 0o755, "app.conf": 0o644, "secret": 0o600, "read-only": 0o444}
	for name, mode := range files {
		file := filepath.Join(root, name)
		if err := os.WriteFile(file, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
	}
	kubeletFSGroup(t, root, false)

	for name, before := range files {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		after := info.Mode().Perm()
		if want := before | kubeletRWMask; after != want {
			t.Errorf("%s: mode %04o after the walk, want %04o", name, after, want)
		}
		if after&0o111 != before&0o111 {
			t.Errorf("%s: the execute bits went from %04o to %04o", name, before&0o111, after&0o111)
		}
	}
}
