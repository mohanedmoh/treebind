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

// claim returns a directory that stands for the claim, and the layout for a sub path.
func claim(t *testing.T, subPath string) (string, Layout) {
	t.Helper()
	root := t.TempDir()
	layout, err := New(root, subPath)
	if err != nil {
		t.Fatal(err)
	}
	return root, layout
}

func mustMkdir(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// wantUnsafe checks that err says a directory is not what it should be, and that the
// text names the path and the problem.
func wantUnsafe(t *testing.T, err error, pathPart, problem string) {
	t.Helper()
	wantUnsafeOn(t, err, pathPart, problem, false)
}

// wantUnsafeOn is wantUnsafe for a directory that is, or is not, on the way to the synced
// directory.
func wantUnsafeOn(t *testing.T, err error, pathPart, problem string, onTheWay bool) {
	t.Helper()
	var unsafe *UnsafeError
	if !errors.As(err, &unsafe) {
		t.Fatalf("error = %v (%T), want an *UnsafeError", err, err)
	}
	if unsafe.OnTheWay != onTheWay {
		t.Errorf("OnTheWay = %t, want %t, for %s", unsafe.OnTheWay, onTheWay, unsafe.Path)
	}
	if !strings.Contains(unsafe.Path, pathPart) || !strings.Contains(unsafe.Problem, problem) {
		t.Errorf("error = {%s: %s}, want the path to hold %q and the problem to say %q", unsafe.Path, unsafe.Problem, pathPart, problem)
	}
	if !strings.Contains(err.Error(), unsafe.Path) || !strings.Contains(err.Error(), unsafe.Problem) {
		t.Errorf("the text of the error, %q, leaves out the path or the problem", err.Error())
	}
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestPrepareMakesTheStateAndGitDirectoriesPrivate(t *testing.T) {
	for _, subPath := range []string{"", "grafana", teamDashboards} {
		t.Run("sub path "+subPath, func(t *testing.T) {
			root, layout := claim(t, subPath)

			if err := layout.Prepare(); err != nil {
				t.Fatal(err)
			}

			for _, dir := range []string{filepath.Join(root, stateName), filepath.Join(root, stateName, "git")} {
				mode := modeOf(t, dir)
				if !mode.IsDir() || mode.Perm() != 0o700 {
					t.Errorf("%s has mode %v, want a directory with 0700", dir, mode)
				}
			}
			if layout.GitDir != filepath.Join(root, stateName, "git") {
				t.Errorf("the Git directory the layout names, %s, is not the one that was made", layout.GitDir)
			}
		})
	}
}

func TestPrepareDoesNotMakeTheSyncedDirectory(t *testing.T) {
	root, layout := claim(t, "grafana")

	if err := layout.Prepare(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(root, "grafana")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the synced directory was made, or could not be looked at: %v. Making it is the clone's, with the "+
			"rules for a directory that is not empty (#32)", err)
	}
}

func TestPrepareLeavesWhatItFindsInOrderAsItIs(t *testing.T) {
	root, layout := claim(t, "grafana")
	if err := layout.Prepare(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, stateName, "git", "HEAD")
	if err := os.WriteFile(marker, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(root, "grafana", "sub"), 0o755)

	if err := layout.Prepare(); err != nil {
		t.Fatalf("a second call: %v", err)
	}

	if data, err := os.ReadFile(marker); err != nil || string(data) != "ref: refs/heads/main\n" {
		t.Errorf("a file in the Git directory was lost: %q, %v", data, err)
	}
}

func TestPrepareAcceptsADirectoryThatIsClosedToGroupAndOthers(t *testing.T) {
	// 0700 is what the agent makes. A stricter mode is the owner's own choice: the agent
	// can still enter a directory it may only read and search, and group and others cannot.
	for _, mode := range []fs.FileMode{0o700, 0o500} {
		t.Run(mode.String(), func(t *testing.T) {
			root, layout := claim(t, "grafana")
			mustMkdir(t, filepath.Join(root, stateName, "git"), 0o700)
			if err := os.Chmod(filepath.Join(root, stateName), mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, stateName), 0o700) })

			if err := layout.Prepare(); err != nil {
				t.Errorf("mode %v: %v", mode, err)
			}
		})
	}
}

func TestPrepareRefusesDirectoriesThatAreOpenToGroupOrOthers(t *testing.T) {
	for _, mode := range []fs.FileMode{0o750, 0o770, 0o705, 0o707, 0o755, 0o777, 0o710, 0o701, 0o740} {
		t.Run("state directory "+mode.String(), func(t *testing.T) {
			root, layout := claim(t, "grafana")
			mustMkdir(t, filepath.Join(root, stateName), mode)

			err := layout.Prepare()

			wantUnsafe(t, err, stateName, "open to group or others")
		})
	}
	t.Run("the Git directory", func(t *testing.T) {
		root, layout := claim(t, "grafana")
		mustMkdir(t, filepath.Join(root, stateName, "git"), 0o750)
		if err := os.Chmod(filepath.Join(root, stateName), 0o700); err != nil {
			t.Fatal(err)
		}

		err := layout.Prepare()

		wantUnsafe(t, err, filepath.Join(stateName, "git"), "open to group or others")
	})
}

func TestPrepareRefusesALinkWhereADirectoryShouldBe(t *testing.T) {
	outside := t.TempDir()
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		path  string
	}{
		{
			name: "a link to a directory outside the claim",
			setup: func(t *testing.T, root string) {
				mustSymlink(t, outside, filepath.Join(root, stateName))
			},
			path: stateName,
		},
		{
			name: "a link to a directory inside the claim",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, "elsewhere"), 0o700)
				mustSymlink(t, filepath.Join(root, "elsewhere"), filepath.Join(root, stateName))
			},
			path: stateName,
		},
		{
			name: "a link that points nowhere",
			setup: func(t *testing.T, root string) {
				mustSymlink(t, filepath.Join(root, "gone"), filepath.Join(root, stateName))
			},
			path: stateName,
		},
		{
			name: "a relative link",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, "elsewhere"), 0o700)
				mustSymlink(t, "elsewhere", filepath.Join(root, stateName))
			},
			path: stateName,
		},
		{
			name: "a link in the place of the Git directory",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, stateName), 0o700)
				mustSymlink(t, outside, filepath.Join(root, stateName, "git"))
			},
			path: filepath.Join(stateName, "git"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, layout := claim(t, "grafana")
			tt.setup(t, root)

			err := layout.Prepare()

			wantUnsafe(t, err, tt.path, isALink)
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("something was made behind the link: %v", entries)
			}
		})
	}
}

func TestPrepareRefusesAFileWhereADirectoryShouldBe(t *testing.T) {
	root, layout := claim(t, "grafana")
	if err := os.WriteFile(filepath.Join(root, stateName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := layout.Prepare()

	wantUnsafe(t, err, stateName, "is not a directory")
}

func TestPrepareRefusesADirectoryOfAnotherUser(t *testing.T) {
	root, layout := claim(t, "grafana")
	mustMkdir(t, filepath.Join(root, stateName, "git"), 0o700)
	t.Cleanup(func() { effectiveUID = os.Geteuid })
	effectiveUID = func() int { return os.Geteuid() + 1 }

	err := layout.Prepare()

	wantUnsafe(t, err, stateName, "belongs to another user")
	if !strings.Contains(err.Error(), "uid") {
		t.Errorf("the text does not name who owns it and who the agent is: %q", err.Error())
	}
}

func TestPrepareRefusesADirectoryThatIsSwappedForALinkAfterItWasLookedAt(t *testing.T) {
	root, layout := claim(t, "grafana")
	mustMkdir(t, filepath.Join(root, stateName, "git"), 0o700)
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(outside, "git"), 0o700)

	var swapped bool
	t.Cleanup(func() { afterLookedAt = nil })
	afterLookedAt = func(path string) {
		if swapped || !strings.HasSuffix(path, stateName) {
			return
		}
		swapped = true
		// What an application that can write the claim root could do between the look and the
		// open: put a link where the directory it just passed was.
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, outside, path)
	}

	err := layout.Prepare()

	if !swapped {
		t.Fatal("the swap never happened, so this tests nothing")
	}
	if _, ok := errors.AsType[*UnsafeError](err); !ok {
		t.Fatalf("error = %v, want the directory to be refused", err)
	}
}

func TestPrepareRefusesADirectoryThatIsSwappedForAnotherDirectory(t *testing.T) {
	// No link at all: the directory that was looked at is moved away and a real directory,
	// owned by the same user and private too, is put in its place. The open succeeds, so
	// only the comparison of what was looked at with what was opened can refuse it.
	root, layout := claim(t, "grafana")
	mustMkdir(t, filepath.Join(root, ".treebind", "git"), 0o700)

	var swapped bool
	t.Cleanup(func() { afterLookedAt = nil })
	afterLookedAt = func(path string) {
		if swapped || !strings.HasSuffix(path, ".treebind") {
			return
		}
		swapped = true
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		mustMkdir(t, filepath.Join(path, "git"), 0o700)
	}

	err := layout.Prepare()

	if !swapped {
		t.Fatal("the swap never happened, so this tests nothing")
	}
	wantUnsafe(t, err, ".treebind", "changed while it was being looked at")
}

func TestPrepareRefusesADirectoryThatIsSwappedForALinkInsideTheClaim(t *testing.T) {
	// The open follows a link that stays inside the claim, so the swap is not caught by the
	// open refusing it: the directory that was looked at has to be the one that was opened.
	root, layout := claim(t, "grafana")
	mustMkdir(t, filepath.Join(root, stateName, "git"), 0o700)
	mustMkdir(t, filepath.Join(root, "elsewhere", "git"), 0o700)

	var swapped bool
	t.Cleanup(func() { afterLookedAt = nil })
	afterLookedAt = func(path string) {
		if swapped || !strings.HasSuffix(path, stateName) {
			return
		}
		swapped = true
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, filepath.Join(root, "elsewhere"), path)
	}

	err := layout.Prepare()

	if !swapped {
		t.Fatal("the swap never happened, so this tests nothing")
	}
	wantUnsafe(t, err, stateName, "changed while it was being looked at")
}

func TestPrepareSaysAPlainErrorForADirectoryItMayNotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a directory that is closed to it")
	}
	root, layout := claim(t, "grafana")
	mustMkdir(t, filepath.Join(root, stateName), 0o300) // may be searched and written, not read
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, stateName), 0o700) })

	err := layout.Prepare()

	var unsafe *UnsafeError
	if err == nil || errors.As(err, &unsafe) || !errors.Is(err, syscall.EACCES) {
		t.Errorf("error = %v, want a plain permission error: the directory is not unsafe, the agent is shut out of it", err)
	}
	if err != nil && !strings.Contains(err.Error(), filepath.Join(root, stateName)) {
		t.Errorf("error = %v, want the path of the directory it could not open, %s", err, filepath.Join(root, stateName))
	}
}

func TestAnOwnerThatCannotBeToldIsNotTrusted(t *testing.T) {
	if problem := notAPrivateDirectory(fakeDir{}); !strings.Contains(problem, "owner that cannot be told") {
		t.Errorf("problem = %q, want it to say the owner cannot be told", problem)
	}
}

// fakeDir is a directory whose file info carries no owner.
type fakeDir struct{ fakeInfo }

func (fakeDir) Mode() fs.FileMode { return fs.ModeDir | 0o700 }
func (fakeDir) IsDir() bool       { return true }

func TestPrepareRefusesALinkOnTheWayToTheSyncedDirectory(t *testing.T) {
	tests := []struct {
		name    string
		subPath string
		setup   func(t *testing.T, root string)
		path    string
		problem string
	}{
		{
			name: "the synced directory is a link", subPath: dashboardsDir,
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, "real"), 0o755)
				mustSymlink(t, filepath.Join(root, "real"), filepath.Join(root, dashboardsDir))
			},
			path: dashboardsDir, problem: isALink,
		},
		{
			name: "the synced directory is a link to the state directory", subPath: dashboardsDir,
			setup: func(t *testing.T, root string) {
				mustSymlink(t, stateName, filepath.Join(root, dashboardsDir))
			},
			path: dashboardsDir, problem: isALink,
		},
		{
			name: "a directory on the way is a link", subPath: teamDashboards,
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, "real"), 0o755)
				mustSymlink(t, filepath.Join(root, "real"), filepath.Join(root, teamDir))
			},
			path: teamDir, problem: isALink,
		},
		{
			name: "a directory on the way is a link to somewhere outside", subPath: teamDashboards,
			setup: func(t *testing.T, root string) {
				mustSymlink(t, t.TempDir(), filepath.Join(root, teamDir))
			},
			path: teamDir, problem: isALink,
		},
		{
			name: "a link at the end of a path whose start is real", subPath: teamDashboards,
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, teamDir), 0o755)
				mustSymlink(t, "..", filepath.Join(root, teamDir, dashboardsDir))
			},
			path: filepath.Join(teamDir, dashboardsDir), problem: isALink,
		},
		{
			name: "the synced directory is a file", subPath: dashboardsDir,
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, dashboardsDir), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			path: dashboardsDir, problem: "is not a directory",
		},
		{
			name: "a directory on the way is a file", subPath: teamDashboards,
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, teamDir), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			path: teamDir, problem: "is not a directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, layout := claim(t, tt.subPath)
			tt.setup(t, root)

			err := layout.Prepare()

			wantUnsafeOn(t, err, tt.path, tt.problem, true)
		})
	}
}

func TestPrepareAcceptsTheWayToTheSyncedDirectoryWhenItIsRealOrNotThereYet(t *testing.T) {
	t.Run("not there", func(t *testing.T) {
		_, layout := claim(t, teamDashboards)
		if err := layout.Prepare(); err != nil {
			t.Error(err)
		}
	})
	t.Run("half of it there", func(t *testing.T) {
		root, layout := claim(t, teamDashboards)
		mustMkdir(t, filepath.Join(root, teamDir), 0o755)
		if err := layout.Prepare(); err != nil {
			t.Error(err)
		}
	})
	t.Run("all of it there, whoever owns it and whatever its mode", func(t *testing.T) {
		root, layout := claim(t, teamDashboards)
		mustMkdir(t, filepath.Join(root, teamDir, dashboardsDir), 0o777)
		if err := layout.Prepare(); err != nil {
			t.Error(err)
		}
	})
	t.Run("the whole claim is synced", func(t *testing.T) {
		_, layout := claim(t, "")
		if err := layout.Prepare(); err != nil {
			t.Error(err)
		}
	})
}

func TestPrepareSaysWhenTheClaimCannotBeUsedAndDoesNotCallItUnsafe(t *testing.T) {
	t.Run("the mount path does not exist", func(t *testing.T) {
		layout, err := New(filepath.Join(t.TempDir(), "missing"), "grafana")
		if err != nil {
			t.Fatal(err)
		}

		err = layout.Prepare()

		var unsafe *UnsafeError
		if err == nil || errors.As(err, &unsafe) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want a plain error that wraps fs.ErrNotExist", err)
		}
	})
	t.Run("the mount path is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		layout, err := New(file, "")
		if err != nil {
			t.Fatal(err)
		}

		var unsafe *UnsafeError
		if err := layout.Prepare(); err == nil || errors.As(err, &unsafe) {
			t.Errorf("error = %v, want a plain error", err)
		}
	})
	t.Run("the claim cannot be written to", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory that is closed to it")
		}
		root, layout := claim(t, "grafana")
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		err := layout.Prepare()

		var unsafe *UnsafeError
		if err == nil || errors.As(err, &unsafe) || !errors.Is(err, syscall.EACCES) {
			t.Errorf("error = %v, want a plain error that wraps EACCES, so a missing permission is not called unsafe", err)
		}
		if err != nil && !strings.Contains(err.Error(), filepath.Join(root, stateName)) {
			t.Errorf("error = %v, want the path of the directory it could not make, %s", err, filepath.Join(root, stateName))
		}
	})
	t.Run("the state directory cannot be written to", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a directory that is closed to it")
		}
		root, layout := claim(t, "grafana")
		state := filepath.Join(root, stateName)
		mustMkdir(t, state, 0o500)
		t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

		err := layout.Prepare()

		if err == nil || !errors.Is(err, syscall.EACCES) {
			t.Fatalf("error = %v, want a plain error that wraps EACCES", err)
		}
		if want := filepath.Join(state, "git"); !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want the path of the Git directory it could not make, %s", err, want)
		}
	})
	t.Run("the claim cannot be searched", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory that is closed to it")
		}
		root, layout := claim(t, "grafana")
		if err := os.Chmod(root, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		err := layout.Prepare()

		if err == nil || !errors.Is(err, syscall.EACCES) {
			t.Fatalf("error = %v, want a plain error that wraps EACCES", err)
		}
		if want := filepath.Join(root, "grafana"); !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want the path it could not look at, %s", err, want)
		}
	})
}

func TestUnsafeErrorNamesThePathAndTheProblem(t *testing.T) {
	err := &UnsafeError{Path: "/work/.treebind", Problem: isALink}

	if err.Error() != "/work/.treebind is a link" {
		t.Errorf("text = %q", err.Error())
	}
}

func TestOwnerOfSaysWhoOwnsADirectoryAndFailsClosedWhenItCannotTell(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := ownerOf(info)
	if !ok || uid != os.Geteuid() {
		t.Errorf("ownerOf = %d, %v, want the current user, %d", uid, ok, os.Geteuid())
	}

	// A file info that does not carry the owner is not trusted.
	if _, ok := ownerOf(fakeInfo{}); ok {
		t.Error("an owner was made up for a file info that has none")
	}
}

type fakeInfo struct{ fs.FileInfo }

func (fakeInfo) Sys() any { return nil }
