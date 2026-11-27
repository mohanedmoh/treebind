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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/passwd"
)

// withPasswdSource makes the passwd file of the image, for the test, the one given.
func withPasswdSource(t *testing.T, content string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	before := passwdSource
	passwdSource = source
	t.Cleanup(func() { passwdSource = before })
}

func TestTheAgentWritesThePasswdFileOfItsUserWhenAskedAndExits(t *testing.T) {
	const image = "root:x:0:0:root:/root:/bin/ash\n"
	withPasswdSource(t, image)
	target := filepath.Join(t.TempDir(), "passwd")
	var stderr bytes.Buffer

	code := runCommand([]string{passwd.Command, target}, &stderr)

	if code != exitOK {
		t.Fatalf("exit code %d, want %d; stderr:\n%s", code, exitOK, stderr.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(passwd.WithUser([]byte(image), os.Getuid(), os.Getgid())); string(got) != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestTheAgentRefusesArgumentsThatAreNotTheCommandAndAnAbsolutePath(t *testing.T) {
	for _, args := range [][]string{
		{"sync"},
		{passwd.Command},
		{passwd.Command, "passwd"},
		{passwd.Command, "/run/passwd", "/run/other"},
		{"/run/passwd", passwd.Command},
	} {
		var stderr bytes.Buffer
		if code := runCommand(args, &stderr); code != exitConfig {
			t.Errorf("%q: exit code %d, want %d", args, code, exitConfig)
		}
		if !strings.Contains(stderr.String(), passwd.Command) {
			t.Errorf("%q: the message does not name %s:\n%s", args, passwd.Command, stderr.String())
		}
	}
}

func TestTheAgentFailsWhenItCannotWriteThePasswdFile(t *testing.T) {
	withPasswdSource(t, "")
	target := filepath.Join(t.TempDir(), "missing", "passwd")
	var stderr bytes.Buffer

	if code := runCommand([]string{passwd.Command, target}, &stderr); code != exitFailed {
		t.Errorf("exit code %d, want %d", code, exitFailed)
	}
	if !strings.Contains(stderr.String(), "passwd file") {
		t.Errorf("the message does not say what failed:\n%s", stderr.String())
	}
}
