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

package commitmsg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// authorGitWrote makes a commit with the author in the environment, as the agent will, and returns the name and the
// email that Git wrote.
func authorGitWrote(t *testing.T, git, name, email string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(),
		// No configuration of the machine reaches the commit: not the global file, and not the system file either, with
		// the variable that every release of Git reads.
		"HOME="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
		"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email,
	)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	// An empty template, so that no hook of the machine is copied in, and no signing and no hook for the commit.
	run("init", "-q", "--template=", filepath.Join(dir, "r"))
	cmd := exec.Command(git, "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", "message")
	cmd.Dir, cmd.Env = filepath.Join(dir, "r"), env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command(git, "log", "-1", "--format=%an%x00%ae")
	cmd.Dir, cmd.Env = filepath.Join(dir, "r"), env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	wrote := strings.TrimSuffix(string(out), "\n")
	gotName, gotEmail, _ := strings.Cut(wrote, "\x00")
	return gotName, gotEmail
}

func gitBinary(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed, and this test runs the real git binary")
	}
	return git
}

// An author that ValidateAuthor accepts is the author of the commit, as it was given.
func TestGitKeepsAnAuthorThatValidateAuthorAccepts(t *testing.T) {
	git := gitBinary(t)
	for _, tt := range []struct{ name, email string }{
		{DefaultAuthorName, goodEmail},
		{"Treebind Bot", "bot+treebind@example.org"},
		{"Zoë Ünï ✓", "zoe@example.org"},
		{"bo.t", goodEmail},
		{"Treebind Bot, the sync agent", goodEmail},
		{"it's the bot", goodEmail},
		{"bøt", "bøt@example.org"},
		{bareName, "o'brien@example.org"},
		{bareName, "bot@[127.0.0.1]"},
	} {
		if err := ValidateAuthor(tt.name, tt.email); err != nil {
			t.Fatalf("ValidateAuthor(%q, %q) = %v: the test is about an author that it accepts", tt.name, tt.email, err)
		}
		gotName, gotEmail := authorGitWrote(t, git, tt.name, tt.email)
		if gotName != tt.name || gotEmail != tt.email {
			t.Errorf("Git wrote %q <%q> for %q <%q>, want the author as it was given", gotName, gotEmail, tt.name, tt.email)
		}
	}
}

// Git writes another author than the one it was given when the name or the email has a newline, '<' or '>' in it, or
// a space, a control character or one of , : ; " ' \ at its start or its end, which is why ValidateAuthor refuses them.
// A period at the ends is trimmed by Git 2.34.1 and kept by 2.56.1, and is refused for that reason: the test below
// holds it with every other character.
func TestGitChangesAnAuthorThatValidateAuthorRefuses(t *testing.T) {
	git := gitBinary(t)
	for _, tt := range []struct{ what, name, email string }{
		{"a name with <", "tree<bind", goodEmail},
		{"a name with >", "tree>bind", goodEmail},
		{"a name with a newline", "tree\nbind", goodEmail},
		{"an email with a display name", bareName, displayNameEmail},
		{"an email in angle brackets", bareName, bracketedEmail},
		{"an email with a newline", bareName, "bot@example.org\nx"},
		{"a name with a space before it", " bot", goodEmail},
		{"a name in single quotes", "'bot'", goodEmail},
		{"a name with a comma before it", ",bot", goodEmail},
		{"an email with a quote before it", bareName, "'bot@example.org"},
		{"an email with a quote after it", bareName, "bot@example.org'"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			if err := ValidateAuthor(tt.name, tt.email); err == nil {
				t.Fatalf("ValidateAuthor(%q, %q) = nil: the test is about an author that it refuses", tt.name, tt.email)
			}
			gotName, gotEmail := authorGitWrote(t, git, tt.name, tt.email)
			if gotName == tt.name && gotEmail == tt.email {
				t.Errorf("Git kept %q <%q> as it was given, so there is nothing for ValidateAuthor to protect", gotName, gotEmail)
			}
		})
	}
}

// The whole contract, byte by byte: for each ASCII character at the start, in the middle and at the end of a name, and at
// the start and the end of an email, a value that this Git changes is a value that ValidateAuthor refuses. So the author
// that ValidateAuthor accepts is the author that is written, on the Git that runs the test. It is run on the Git of the
// machine and on the Git of the agent image, which differ in the period.
func TestEveryByteThatGitChangesIsRefusedByValidateAuthor(t *testing.T) {
	git := gitBinary(t)
	type form struct {
		what        string
		name, email func(c string) string
	}
	same := func(s string) func(string) string { return func(string) string { return s } }
	forms := []form{
		{"at the start of a name", func(c string) string { return c + "bot" }, same("a@b.org")},
		{"at the end of a name", func(c string) string { return "bot" + c }, same("a@b.org")},
		{"in the middle of a name", func(c string) string { return "bo" + c + "t" }, same("a@b.org")},
		{"at the start of an email", same("bot"), func(c string) string { return c + "a@b.org" }},
		{"at the end of an email", same("bot"), func(c string) string { return "a@b.org" + c }},
	}
	var accepted int
	for c := byte(1); c < 127; c++ {
		ch := string([]byte{c})
		for _, f := range forms {
			name, email := f.name(ch), f.email(ch)
			gotName, gotEmail := authorGitWrote(t, git, name, email)
			changed := gotName != name || gotEmail != email
			err := ValidateAuthor(name, email)
			switch {
			case changed && err == nil:
				t.Errorf("%q %s: Git wrote %q <%q> for %q <%q>, and ValidateAuthor accepts it", ch, f.what, gotName, gotEmail, name, email)
			case err == nil:
				accepted++
			}
		}
	}
	if accepted < 100 {
		t.Errorf("ValidateAuthor accepted %d of the values, want most of the characters in the middle of a name: the test says nothing otherwise", accepted)
	}
}

// No argument of a command can hold a NUL byte, which is why Render refuses a message that has one.
func TestACommandCannotBeGivenANulByte(t *testing.T) {
	git := gitBinary(t)
	if err := exec.Command(git, "--version", "sync\x00 3 files").Run(); err == nil {
		t.Error("a command was started with a NUL byte in an argument")
	} else if strings.Contains(err.Error(), "exit status") {
		t.Errorf("error = %v, want the command not to start at all", err)
	}
}
