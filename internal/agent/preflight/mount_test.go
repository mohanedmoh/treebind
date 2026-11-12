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

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fsExt4 = "ext4"

// The root mount of a container, as /proc/self/mountinfo lists it.
const rootMountLine = "36 1 0:30 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/a,upperdir=/b"

var mountPathEscaper = strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)

// mountLine builds one line of the mount table for a mount point.
func mountLine(point, options, fsType string) string {
	return fmt.Sprintf("41 36 8:1 / %s %s shared:2 - %s /dev/sda1 rw", mountPathEscaper.Replace(point), options, fsType)
}

// mountTable writes a mount table made of the given lines to a file and returns
// its path.
func mountTable(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noMountTable is the path of a mount table that does not exist.
func noMountTable(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-such-table")
}

// tempDir returns a temporary directory by its canonical path, which is how the
// mount table names it. On macOS the temporary directory sits behind a symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestContainingMount(t *testing.T) {
	table := strings.Join([]string{
		rootMountLine,
		mountLine(workMountPoint, "rw,relatime", fsExt4),
		mountLine("/work/cache", "ro,relatime", "tmpfs"),
		mountLine("/data with space", "rw", "xfs"),
		mountLine("/data", "rw", "nfs4"),
		mountLine("/data", "ro", fsExt4),
		"",
		"not a mount table line",
	}, "\n")

	tests := []struct {
		name         string
		path         string
		wantPoint    string
		wantFSType   string
		wantReadOnly bool
	}{
		{"the mount point itself", workMountPoint, workMountPoint, fsExt4, false},
		{"a directory below a mount", "/work/dashboards/team", workMountPoint, fsExt4, false},
		{"the longest mount point wins", "/work/cache/x", "/work/cache", "tmpfs", true},
		{"a mount does not cover a sibling with the same prefix", "/work2", "/", "overlay", false},
		{"a path on no volume is on the root mount", "/etc", "/", "overlay", false},
		{"escapes in the mount point are undone", "/data with space/x", "/data with space", "xfs", false},
		{"the last of several mounts at one point is the one in effect", "/data/x", "/data", fsExt4, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, ok := containingMount(table, test.path)
			if !ok {
				t.Fatalf("no mount found for %s", test.path)
			}
			if entry.point != test.wantPoint || entry.fsType != test.wantFSType || entry.readOnly != test.wantReadOnly {
				t.Errorf("containingMount(%q) = %+v, want point %q, type %q, read-only %v",
					test.path, entry, test.wantPoint, test.wantFSType, test.wantReadOnly)
			}
		})
	}

	t.Run("a table without a root mount can leave a path uncovered", func(t *testing.T) {
		if _, ok := containingMount(mountLine(workMountPoint, "rw", fsExt4), "/etc"); ok {
			t.Error("found a mount for a path that nothing covers")
		}
	})
}

func TestParseMountInfoLine(t *testing.T) {
	for _, line := range []string{
		"",
		"36 35 98:0 /mnt1 /mnt2",
		"36 35 98:0 /mnt1 /mnt2 rw",
		"36 35 98:0 /mnt1 /mnt2 rw - ",
	} {
		if entry, ok := parseMountInfoLine(line); ok {
			t.Errorf("parseMountInfoLine(%q) = %+v, want a line that is rejected", line, entry)
		}
	}

	// Optional fields sit between the options and the separator, and there can be
	// several or none.
	for _, line := range []string{
		"36 35 98:0 /mnt1 /mnt2 rw - ext3 /dev/root rw",
		"36 35 98:0 /mnt1 /mnt2 rw shared:1 - ext3 /dev/root rw",
		"36 35 98:0 /mnt1 /mnt2 rw shared:1 master:2 propagate_from:3 - ext3 /dev/root rw",
	} {
		entry, ok := parseMountInfoLine(line)
		if !ok || entry.point != "/mnt2" || entry.fsType != "ext3" || entry.readOnly {
			t.Errorf("parseMountInfoLine(%q) = %+v, %v", line, entry, ok)
		}
	}
}

func TestParseMountInfoLineReadOnly(t *testing.T) {
	tests := map[string]bool{
		"36 35 98:0 / /mnt rw - ext4 /dev/sda1 rw":                          false,
		"36 35 98:0 / /mnt ro,relatime - ext4 /dev/sda1 rw":                 true,
		"36 35 98:0 / /mnt rw,relatime - ext4 /dev/sda1 ro":                 true,
		"36 35 98:0 / /mnt rw,relatime shared:1 - nfs4 server:/x ro,vers=4": true,
		"36 35 98:0 / /mnt rw - ext4 /dev/sda1":                             false,
		"36 35 98:0 / /mnt rw - ext4 /dev/sda1 rw,errors=remount-ro":        false,
	}
	for line, want := range tests {
		entry, ok := parseMountInfoLine(line)
		if !ok || entry.readOnly != want {
			t.Errorf("parseMountInfoLine(%q) read-only = %v, %v, want %v", line, entry.readOnly, ok, want)
		}
	}
}

func TestUnescapeMountPath(t *testing.T) {
	tests := map[string]string{
		`/plain`:             "/plain",
		`/a\040b`:            "/a b",
		`/tab\011here`:       "/tab\there",
		`/new\012line`:       "/new\nline",
		`/back\134slash`:     `/back\slash`,
		`/trailing\`:         `/trailing\`,
		`/not\0octal`:        `/not\0octal`,
		`/short\04`:          `/short\04`,
		`/two\040\040spaces`: "/two  spaces",
	}
	for escaped, want := range tests {
		if got := unescapeMountPath(escaped); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", escaped, got, want)
		}
	}
}

func TestCheckMount(t *testing.T) {
	t.Run("a path that does not exist is invalid", func(t *testing.T) {
		result := checkMount(filepath.Join(tempDir(t), "missing"), mountTable(t, rootMountLine))

		wantResult(t, result, CheckMount, Failed, ReasonMountPathInvalid)
		if !strings.Contains(result.Message, "no such file or directory") {
			t.Errorf("the message does not say what is wrong: %s", result.Message)
		}
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		file := filepath.Join(tempDir(t), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		result := checkMount(file, mountTable(t, rootMountLine))

		wantResult(t, result, CheckMount, Failed, ReasonMountPathInvalid)
		if !strings.Contains(result.Message, "is not a directory") {
			t.Errorf("the message does not say what is wrong: %s", result.Message)
		}
	})

	t.Run("a directory on the container's own filesystem is not mounted", func(t *testing.T) {
		result := checkMount(tempDir(t), mountTable(t, rootMountLine))

		wantResult(t, result, CheckMount, Failed, ReasonNotMounted)
		if !strings.Contains(result.Message, "claim is mounted at this path") {
			t.Errorf("the message does not say what to check: %s", result.Message)
		}
	})

	t.Run("a mount point passes and the message names the mount", func(t *testing.T) {
		dir := tempDir(t)

		result := checkMount(dir, mountTable(t, rootMountLine, mountLine(dir, "rw,relatime", fsExt4)))

		wantResult(t, result, CheckMount, Passed, "")
		for _, want := range []string{dir, fsExt4, "read-write"} {
			if !strings.Contains(result.Message, want) {
				t.Errorf("the message %q does not contain %q", result.Message, want)
			}
		}
	})

	t.Run("a relative path is made absolute before it is looked up", func(t *testing.T) {
		dir := tempDir(t)
		below := filepath.Join(dir, "checkout")
		if err := os.Mkdir(below, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		result := checkMount("checkout", mountTable(t, rootMountLine, mountLine(dir, "rw", "xfs")))

		wantResult(t, result, CheckMount, Passed, "")
	})

	t.Run("a symlink to a mounted directory is followed to the mount", func(t *testing.T) {
		mounted, links := tempDir(t), tempDir(t)
		link := filepath.Join(links, "work")
		if err := os.Symlink(mounted, link); err != nil {
			t.Fatal(err)
		}

		result := checkMount(link, mountTable(t, rootMountLine, mountLine(mounted, "rw", fsExt4)))

		wantResult(t, result, CheckMount, Passed, "")
	})

	t.Run("a mount that is read-only, by its own options or its filesystem's, says so", func(t *testing.T) {
		dir := tempDir(t)
		for name, line := range map[string]string{
			"mount":      mountLine(dir, "ro,relatime", fsExt4),
			"filesystem": fmt.Sprintf("41 36 8:1 / %s rw,relatime - ext4 /dev/sda1 ro", dir),
		} {
			result := checkMount(dir, mountTable(t, rootMountLine, line))

			wantResult(t, result, CheckMount, Passed, "")
			if !strings.Contains(result.Message, "read-only") {
				t.Errorf("a read-only %s is not reported: %s", name, result.Message)
			}
		}
	})

	t.Run("the message leaves out the source of the mount", func(t *testing.T) {
		dir := tempDir(t)
		table := mountTable(t, rootMountLine,
			fmt.Sprintf("41 36 0:40 / %s rw - nfs4 storage.internal:/exports/team rw", dir))

		result := checkMount(dir, table)

		wantResult(t, result, CheckMount, Passed, "")
		if strings.Contains(result.Message, "storage.internal") {
			t.Errorf("the message names a storage server: %s", result.Message)
		}
	})

	t.Run("an unreadable mount table skips the check", func(t *testing.T) {
		wantResult(t, checkMount(tempDir(t), noMountTable(t)), CheckMount, Skipped, ReasonNotChecked)
	})

	t.Run("a mount table that does not cover the path skips the check", func(t *testing.T) {
		table := mountTable(t, mountLine("/somewhere/else", "rw", fsExt4))

		wantResult(t, checkMount(tempDir(t), table), CheckMount, Skipped, ReasonNotChecked)
	})
}
