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

func TestMakeWorkTreeMakesTheSyncedDirectoryAndTheWayToIt(t *testing.T) {
	root, layout := claim(t, teamDashboards)

	if err := layout.MakeWorkTree(); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{teamDir, teamDashboards} {
		if info, err := os.Lstat(filepath.Join(root, dir)); err != nil || !info.IsDir() {
			t.Errorf("%s: %v, want a directory", dir, err)
		}
	}
	// The mode is open to the group, as far as the umask lets it be, so that the application
	// that shares the claim can write in them (R-12).
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	if got, want := modeOf(t, filepath.Join(root, teamDashboards)).Perm(), fs.FileMode(0o777)&^fs.FileMode(umask); got != want {
		t.Errorf("mode = %04o, want %04o", got, want)
	}
}

func TestMakeWorkTreeMakesNothingForTheWholeClaim(t *testing.T) {
	root, layout := claim(t, "")

	if err := layout.MakeWorkTree(); err != nil {
		t.Fatal(err)
	}

	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("the claim holds %d entries, want none: %v", len(entries), err)
	}
}

func TestMakeWorkTreeLeavesWhatIsThereAsItIs(t *testing.T) {
	root, layout := claim(t, teamDashboards)
	mustMkdir(t, filepath.Join(root, teamDashboards), 0o750)
	if err := os.Chmod(filepath.Join(root, teamDashboards), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, teamDashboards, "a.json"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := layout.MakeWorkTree(); err != nil {
		t.Fatal(err)
	}

	if got := modeOf(t, filepath.Join(root, teamDashboards)).Perm(); got != 0o750 {
		t.Errorf("mode = %04o, want the directory of the application left at 0750", got)
	}
	if _, err := os.Stat(filepath.Join(root, teamDashboards, "a.json")); err != nil {
		t.Error(err)
	}
}

func TestMakeWorkTreeRefusesALinkOrAFileInTheWay(t *testing.T) {
	tests := []struct {
		name    string
		subPath string
		setup   func(t *testing.T, root string)
		path    string
		problem string
	}{
		{"the synced directory is a link", dashboardsDir, func(t *testing.T, root string) {
			mustSymlink(t, t.TempDir(), filepath.Join(root, dashboardsDir))
		}, dashboardsDir, isALink},
		{"a directory on the way is a link", teamDashboards, func(t *testing.T, root string) {
			mustSymlink(t, t.TempDir(), filepath.Join(root, teamDir))
		}, teamDir, isALink},
		{"a link to the state directory", dashboardsDir, func(t *testing.T, root string) {
			mustSymlink(t, stateName, filepath.Join(root, dashboardsDir))
		}, dashboardsDir, isALink},
		{"a dangling link", dashboardsDir, func(t *testing.T, root string) {
			mustSymlink(t, filepath.Join(root, "gone"), filepath.Join(root, dashboardsDir))
		}, dashboardsDir, isALink},
		{"a file on the way", teamDashboards, func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, teamDir), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, teamDir, isNotADir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, layout := claim(t, tt.subPath)
			tt.setup(t, root)

			err := layout.MakeWorkTree()

			wantUnsafeOn(t, err, tt.path, tt.problem, true)
		})
	}
}

func TestMakeWorkTreeDoesNotMakeAnythingThroughALink(t *testing.T) {
	root, layout := claim(t, teamDashboards)
	outside := t.TempDir()
	mustSymlink(t, outside, filepath.Join(root, teamDir))

	if err := layout.MakeWorkTree(); err == nil {
		t.Fatal("a link on the way was followed")
	}

	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("the directory the link leads to holds %d entries, want none: %v", len(entries), err)
	}
}

func TestMakeWorkTreeSaysWhenTheClaimCannotBeUsedAndDoesNotCallItUnsafe(t *testing.T) {
	t.Run("the mount path does not exist", func(t *testing.T) {
		layout, err := New(filepath.Join(t.TempDir(), "missing"), dashboardsDir)
		if err != nil {
			t.Fatal(err)
		}

		err = layout.MakeWorkTree()

		var unsafe *UnsafeError
		if err == nil || errors.As(err, &unsafe) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want a plain error that wraps fs.ErrNotExist", err)
		}
	})
	t.Run("the claim cannot be written to", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory that is closed to it")
		}
		root, layout := claim(t, dashboardsDir)
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		err := layout.MakeWorkTree()

		var unsafe *UnsafeError
		if err == nil || errors.As(err, &unsafe) || !errors.Is(err, syscall.EACCES) {
			t.Fatalf("error = %v, want a plain error that wraps EACCES", err)
		}
		if want := filepath.Join(root, dashboardsDir); !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want the path of the directory it could not make, %s", err, want)
		}
	})
	t.Run("the claim cannot be searched", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory that is closed to it")
		}
		root, layout := claim(t, teamDashboards)
		mustMkdir(t, filepath.Join(root, teamDir), 0o755)
		if err := os.Chmod(root, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		err := layout.MakeWorkTree()

		if err == nil || !errors.Is(err, syscall.EACCES) {
			t.Fatalf("error = %v, want a plain error that wraps EACCES", err)
		}
	})
}
