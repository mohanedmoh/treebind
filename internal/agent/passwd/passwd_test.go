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

package passwd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// imageFile is the start of the passwd file of the agent image, with the user it runs as.
const imageFile = "root:x:0:0:root:/root:/bin/ash\n" +
	"nobody:x:65534:65534:nobody:/:/sbin/nologin\n" +
	"git:x:65532:65532:Account created by apko:/home/git:/bin/sh\n"

func TestWithUserAddsAnEntryForAUserIDThatTheFileDoesNotHave(t *testing.T) {
	got := string(WithUser([]byte(imageFile), 1000770000, 0))
	want := imageFile + "treebind:x:1000770000:0:treebind:/nonexistent:/sbin/nologin\n"
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestWithUserLeavesAFileThatHasTheUserIDAsItIs(t *testing.T) {
	for _, uid := range []int{65532, 0, 65534} {
		if got := string(WithUser([]byte(imageFile), uid, 1234)); got != imageFile {
			t.Errorf("uid %d: file =\n%s\nwant it as it was", uid, got)
		}
	}
}

// Only the third field is the user ID: a group ID, a name or a comment that reads like the number is not an entry
// of the user, and neither is a line that only begins with the number.
func TestWithUserLooksOnlyAtTheUserIDOfAnEntry(t *testing.T) {
	file := "a:x:1:4242:4242:/4242:/sbin/nologin\n" +
		"4242:x:2:2::/:/sbin/nologin\n" +
		"b:x:42420:2::/:/sbin/nologin\n"
	got := string(WithUser([]byte(file), 4242, 4242))
	want := file + "treebind:x:4242:4242:treebind:/nonexistent:/sbin/nologin\n"
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestWithUserEndsALastLineWithoutANewlineBeforeTheEntry(t *testing.T) {
	got := string(WithUser([]byte(strings.TrimSuffix(imageFile, "\n")), 5000, 5000))
	want := imageFile + "treebind:x:5000:5000:treebind:/nonexistent:/sbin/nologin\n"
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestWithUserMakesAFileOfTheEntryAloneFromNothing(t *testing.T) {
	got := string(WithUser(nil, 5000, 0))
	if want := "treebind:x:5000:0:treebind:/nonexistent:/sbin/nologin\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestWriteWritesTheFileForTheUserOfTheProcess(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "image-passwd")
	if err := os.WriteFile(source, []byte("root:x:0:0:root:/root:/bin/ash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "out", "passwd")
	if err := os.Mkdir(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	// What was there before, from an earlier start of the pod, is replaced.
	if err := os.WriteFile(target, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Write(source, target); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(WithUser([]byte("root:x:0:0:root:/root:/bin/ash\n"), os.Getuid(), os.Getgid())); string(got) != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(string(got), ":"+strconv.Itoa(os.Getuid())+":") {
		t.Errorf("file =\n%s\nwant an entry of uid %d", got, os.Getuid())
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	// Every program of the container reads it, and nothing writes it.
	if mode := info.Mode().Perm(); mode != 0o444 {
		t.Errorf("mode = %v, want 0444", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %v, want the file alone", entries)
	}
}

func TestWriteTakesAMissingSourceForAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "passwd")
	if err := Write(filepath.Join(dir, "missing"), target); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(WithUser(nil, os.Getuid(), os.Getgid())); string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestWriteFailsForASourceThatCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	// A directory is there, and is not a file.
	if err := Write(dir, filepath.Join(dir, "passwd")); err == nil {
		t.Error("Write of a directory as the source: no error, want one")
	}
}

func TestWriteFailsWhereItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	err := Write(filepath.Join(dir, "missing"), filepath.Join(dir, "no-such-dir", "passwd"))
	if err == nil || !strings.Contains(err.Error(), "no-such-dir") {
		t.Errorf("error = %v, want one that names the directory", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "no-such-dir")); !os.IsNotExist(statErr) {
		t.Errorf("stat = %v, want the directory not made", statErr)
	}
}
