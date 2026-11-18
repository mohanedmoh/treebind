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

package git

import (
	"strings"
	"testing"
)

const (
	botName  = "treebind-bot"
	botEmail = "bot@example.org"

	// The format that prints the author and the committer of a commit, each as name and email.
	whoFormat = "--format=%an|%ae|%cn|%ce"
)

// commitWith makes a commit with the Runner of the repository, with a change that this call makes, and
// returns who Git recorded as its author and its committer.
func commitWith(t *testing.T, r *repo, runner *Runner) string {
	t.Helper()
	r.write("new.txt", t.Name()+"\n")
	if _, err := r.run(runner, cmdAdd, "new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(runner, cmdCommit, "--quiet", "--message=change"); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(r.plain("log", "-1", whoFormat))
}

func TestACommitIsMadeByTheIdentityOfTheRunnerAsAuthorAndCommitter(t *testing.T) {
	for _, l := range layouts {
		t.Run(string(l), func(t *testing.T) {
			r := newRepo(t, l)
			runner := r.runner(func(o *Options) { o.Identity = Identity{Name: botName, Email: botEmail} })

			if got, want := commitWith(t, r, runner), botName+"|"+botEmail+"|"+botName+"|"+botEmail; got != want {
				t.Errorf("the commit was made by %q, want %q", got, want)
			}
		})
	}
}

func TestTheIdentityBeatsWhatTheRepositoryAndTheProcessSay(t *testing.T) {
	// The repository config of newRepo says that the user is Test, which an application could change, and
	// the environment of the process that starts the Runner could have names of its own.
	t.Setenv("GIT_AUTHOR_NAME", "Process Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "process-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Process Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "process-committer@example.com")
	r := newRepo(t, outsideWork)
	r.plain("config", "user.name", "Config Name")
	r.plain("config", "user.email", "config@example.com")
	runner := r.runner(func(o *Options) { o.Identity = Identity{Name: botName, Email: botEmail} })

	if got, want := commitWith(t, r, runner), botName+"|"+botEmail+"|"+botName+"|"+botEmail; got != want {
		t.Errorf("the commit was made by %q, want %q", got, want)
	}
}

func TestWithNoIdentityNothingOfTheProcessReachesGit(t *testing.T) {
	// The environment of a Runner is built from nothing, so a name in the environment of the agent's own
	// process is not a name of a commit. The repository config is where Git finds the one of this test.
	t.Setenv("GIT_AUTHOR_NAME", "Process Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "process-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Process Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "process-committer@example.com")
	r := newRepo(t, outsideWork)
	runner := r.runner()

	if got, want := commitWith(t, r, runner), "Test|test@example.com|Test|test@example.com"; got != want {
		t.Errorf("the commit was made by %q, want %q, which is the repository's", got, want)
	}
	for _, entry := range runner.environment() {
		if strings.HasPrefix(entry, "GIT_AUTHOR_") || strings.HasPrefix(entry, "GIT_COMMITTER_") {
			t.Errorf("the environment of a Runner with no identity has %q", entry)
		}
	}
}

func TestAnIdentityIsPassedAsItIsWrittenWithTheCharactersOfAnyLanguage(t *testing.T) {
	r := newRepo(t, outsideWork)
	runner := r.runner(func(o *Options) {
		o.Identity = Identity{Name: "Zoë Müller 田中", Email: "zoe.müller+bot@example.org"}
	})

	if got, want := commitWith(t, r, runner), "Zoë Müller 田中|zoe.müller+bot@example.org|Zoë Müller 田中|zoe.müller+bot@example.org"; got != want {
		t.Errorf("the commit was made by %q, want %q", got, want)
	}
}

func TestAnIdentityNeedsBothANameAndAnEmail(t *testing.T) {
	r := newRepo(t, outsideWork)
	for name, id := range map[string]Identity{
		"a name only":   {Name: botName},
		"an email only": {Email: botEmail},
	} {
		t.Run(name, func(t *testing.T) {
			o := Options{GitDir: r.gitDir, WorkTree: r.work, Git: r.git, AllowGitDirInWorkTree: true, Identity: id}
			if _, err := New(o); err == nil || !strings.Contains(err.Error(), "Identity") {
				t.Errorf("New with %s = %v, want an error that names the Identity", name, err)
			}
		})
	}
}

func TestAnIdentityThatAnEnvironmentVariableCannotCarryIsRefused(t *testing.T) {
	r := newRepo(t, outsideWork)
	for name, id := range map[string]Identity{
		"a NUL in the name":  {Name: "bot\x00", Email: botEmail},
		"a NUL in the email": {Name: botName, Email: "bot@example.org\x00"},
	} {
		t.Run(name, func(t *testing.T) {
			o := Options{GitDir: r.gitDir, WorkTree: r.work, Git: r.git, AllowGitDirInWorkTree: true, Identity: id}
			if _, err := New(o); err == nil || !strings.Contains(err.Error(), "Identity") {
				t.Errorf("New with %s = %v, want an error that names the Identity", name, err)
			}
		})
	}
}
