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
	"slices"
	"strconv"
	"strings"
)

// procMountInfo is the kernel's table of the mounts that the agent's process
// sees.
const procMountInfo = "/proc/self/mountinfo"

// mountEntry is the part of a mount table line that the check uses. It leaves
// out the source of the mount, which can name a storage server.
type mountEntry struct {
	point    string
	fsType   string
	readOnly bool
}

// checkMount checks that the path is a directory on a mounted volume, by the
// mount table in the file mountInfo. A path on the container's own filesystem
// means the claim is not mounted there: whatever the agent wrote would be lost
// with the container, and the application would not see it.
func checkMount(path, mountInfo string) Result {
	info, err := os.Stat(path)
	if err != nil {
		return Result{
			Check:   CheckMount,
			Outcome: Failed,
			Reason:  ReasonMountPathInvalid,
			Message: fmt.Sprintf("the mount path cannot be used: %v. Check that the claim is mounted at this path", err),
		}
	}
	if !info.IsDir() {
		return Result{
			Check:   CheckMount,
			Outcome: Failed,
			Reason:  ReasonMountPathInvalid,
			Message: path + " is not a directory",
		}
	}

	// The mount table lists canonical, absolute paths.
	resolved, err := filepath.Abs(path)
	if err != nil {
		resolved = filepath.Clean(path)
	}
	if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = canonical
	}
	table, err := os.ReadFile(mountInfo)
	if err != nil {
		return skipped(CheckMount, fmt.Sprintf(
			"the mount table cannot be read (%v), so it is not known whether %s is a mounted volume", err, path))
	}
	entry, ok := containingMount(string(table), resolved)
	if !ok {
		return skipped(CheckMount, fmt.Sprintf("the mount table has no entry that covers %s", path))
	}
	if entry.point == "/" {
		return Result{
			Check:   CheckMount,
			Outcome: Failed,
			Reason:  ReasonNotMounted,
			Message: fmt.Sprintf("%s is on the container's own filesystem, not on a mounted volume, so what the agent "+
				"wrote there would be lost with the container and the application would not see it. "+
				"Check that the claim is mounted at this path", path),
		}
	}

	mode := "read-write"
	if entry.readOnly {
		mode = "read-only"
	}
	return Result{
		Check:   CheckMount,
		Outcome: Passed,
		Message: fmt.Sprintf("%s is on the volume mounted at %s (%s, %s)", path, entry.point, entry.fsType, mode),
	}
}

// containingMount returns the mount that holds the path: the one with the longest
// mount point that is the path or one of its parents. When several mounts share a
// mount point, the last one in the table is the one in effect.
func containingMount(table, path string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for line := range strings.SplitSeq(table, "\n") {
		entry, ok := parseMountInfoLine(line)
		if !ok || !covers(entry.point, path) {
			continue
		}
		if !found || len(entry.point) >= len(best.point) {
			best, found = entry, true
		}
	}
	return best, found
}

// covers reports whether the path is the mount point or below it. A mount at
// /work does not cover /work2.
func covers(point, path string) bool {
	return point == "/" || path == point || strings.HasPrefix(path, point+"/")
}

// parseMountInfoLine reads one line of /proc/self/mountinfo:
//
//	id parent major:minor root mount-point options [optional fields] - type source super-options
func parseMountInfoLine(line string) (mountEntry, bool) {
	fields := strings.Fields(line)
	separator := slices.Index(fields, "-")
	if separator < 6 || len(fields) < separator+2 {
		return mountEntry{}, false
	}
	// The mount can be read-only by its own options, or because its filesystem is.
	readOnly := hasOption(fields[5], "ro")
	if len(fields) > separator+3 {
		readOnly = readOnly || hasOption(fields[separator+3], "ro")
	}
	return mountEntry{
		point:    unescapeMountPath(fields[4]),
		fsType:   fields[separator+1],
		readOnly: readOnly,
	}, true
}

// hasOption reports whether a comma separated list of mount options holds one.
func hasOption(options, option string) bool {
	return slices.Contains(strings.Split(options, ","), option)
}

// unescapeMountPath undoes the octal escapes the kernel uses for the characters
// that would break the fields apart: a space is \040, a tab \011, a newline \012
// and a backslash \134.
func unescapeMountPath(escaped string) string {
	if !strings.Contains(escaped, `\`) {
		return escaped
	}
	var path strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '\\' && i+3 < len(escaped) {
			if value, err := strconv.ParseUint(escaped[i+1:i+4], 8, 8); err == nil {
				path.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		path.WriteByte(escaped[i])
	}
	return path.String()
}
