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
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// cLocale is the locale that Git words its messages in, so that a test reads them as they are written.
const cLocale = "C"

// A page of docs/ that gives commands for a person to type marks each of them with a line
// `<!-- tested: name -->` before its fenced block, so that a test runs the block as it is written, on a real
// repository, and the test is what says whether the page is true (docs/deletion-guard.md is the first such page).

// Page returns the text of a page of docs/, for a test of a package under internal/agent, whose working directory is
// three levels below the root of the repository.
func Page(t *testing.T, page string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", page))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Block returns the shell block that follows the marker of the name in a page of docs/.
func Block(t *testing.T, page, name string) string {
	t.Helper()
	marker := "<!-- tested: " + name + " -->\n```sh\n"
	_, rest, found := strings.Cut(Page(t, page), marker)
	if !found {
		t.Fatalf("docs/%s has no block marked %q", page, name)
	}
	block, _, found := strings.Cut(rest, "\n```")
	if !found {
		t.Fatalf("the block %q of docs/%s is not closed", name, page)
	}
	return block + "\n"
}

// Output returns the block that follows the marker `<!-- output: name -->` in a page of docs/, which is what a command of the
// page writes, so that a test can hold it to what the code makes.
func Output(t *testing.T, page, name string) string {
	t.Helper()
	marker := "<!-- output: " + name + " -->\n```\n"
	_, rest, found := strings.Cut(Page(t, page), marker)
	if !found {
		t.Fatalf("docs/%s has no output marked %q", page, name)
	}
	block, _, found := strings.Cut(rest, "\n```")
	if !found {
		t.Fatalf("the output %q of docs/%s is not closed", name, page)
	}
	return block + "\n"
}

// RunBlock runs the block of the page in a shell, as a person does in a pod that mounts the whole claim: MOUNT is where
// the claim is, SUB the directory that is synced ("." for the whole claim) and BRANCH the branch. The variables in extra,
// each NAME=value, are set as well. It returns what the block wrote to standard output and error, and fails the test when
// it fails. A program that the block uses and that is not here skips the test, as the tests of the Runner skip without Git.
func (f *Fixture) RunBlock(t *testing.T, l layout.Layout, page, name, branch string, programs []string, extra ...string) string {
	t.Helper()
	for _, program := range append([]string{"sh"}, programs...) {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("%s is not here, and the commands of the page need it", program)
		}
	}
	sub := l.SubPath
	if sub == "" {
		sub = "."
	}
	cmd := exec.Command("sh", "-e", "-c", Block(t, page, name))
	cmd.Dir = TempDir(t)
	// The commands of a page that are run here read and write refs and files, and make no commit, so they need no identity.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + cmd.Dir, "LC_ALL=" + cLocale,
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
		"MOUNT=" + l.Root, "SUB=" + sub, "BRANCH=" + branch,
	}, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the block %q failed: %v\n%s", name, err, out)
	}
	return string(out)
}
