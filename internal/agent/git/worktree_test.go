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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs plain Git in a directory with no repository set in the environment,
// as someone who prepares a repository would.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBinary(t), append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, envGlobalNull, envSystemNull,
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	for _, l := range layouts {
		t.Run(string(l), func(t *testing.T) {
			r := newRepo(t, l)
			outside := filepath.Join(r.root, "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(outside, "secret")
			if err := os.WriteFile(secret, []byte("TOP SECRET\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for link, target := range map[string]string{"dirlink": outside, "filelink": secret} {
				if err := os.Symlink(target, filepath.Join(r.work, link)); err != nil {
					t.Fatal(err)
				}
			}
			runner := r.runner()

			if _, err := r.run(runner, cmdAdd, "-A"); err != nil {
				t.Fatal(err)
			}

			staged, err := r.run(runner, "ls-files", "-s")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"120000", "dirlink", "filelink"} {
				if !strings.Contains(string(staged.Stdout), want) {
					t.Errorf("the links were not staged as links (%s): %s", want, staged.Stdout)
				}
			}
			if strings.Contains(string(staged.Stdout), "dirlink/") {
				t.Errorf("a file behind a link was staged: %s", staged.Stdout)
			}
			blob, err := r.run(runner, cmdCatFile, "blob", ":filelink")
			if err != nil {
				t.Fatal(err)
			}
			if string(blob.Stdout) != secret {
				t.Errorf("the link is staged as %q, want the path it points to, %q", blob.Stdout, secret)
			}

			_, err = r.run(runner, cmdAdd, "--", "dirlink/secret")
			wantGitFailure(t, err, "beyond a symbolic link")
		})
	}
}

func TestGitDoesNotWriteThroughASymlink(t *testing.T) {
	for _, l := range layouts {
		t.Run(string(l), func(t *testing.T) {
			r := newRepo(t, l)
			r.write("dir/file", "tracked\n")
			r.plain(cmdAdd, "dir/file")
			r.plain(cmdCommit, "-q", "-m", "dir")
			// An application replaces the directory with a link to somewhere else.
			outside := filepath.Join(r.root, "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(r.work, "dir")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(r.work, "dir")); err != nil {
				t.Fatal(err)
			}

			_, _ = r.run(r.runner(), "reset", "--hard", "-q")

			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("Git wrote through the link into the directory it points to: %d entries", len(entries))
			}
		})
	}
}

func TestACheckedOutSymlinkIsALinkWhateverTheRepositoryConfigSays(t *testing.T) {
	r := newRepo(t, insideWork)
	if err := os.Symlink(fileA, filepath.Join(r.work, "ln")); err != nil {
		t.Fatal(err)
	}
	r.plain(cmdAdd, "ln")
	r.plain(cmdCommit, "-q", "-m", "a link")
	r.config("core.symlinks", "false")
	remove := func() {
		if err := os.Remove(filepath.Join(r.work, "ln")); err != nil {
			t.Fatal(err)
		}
	}
	isLink := func() bool {
		info, err := os.Lstat(filepath.Join(r.work, "ln"))
		return err == nil && info.Mode()&os.ModeSymlink != 0
	}

	remove()
	r.plain(cmdCheckout, "-q", "--", "ln")
	if isLink() {
		t.Fatal("plain Git created a link in spite of core.symlinks=false, so the test proves nothing")
	}

	remove()
	if _, err := r.run(r.runner(), cmdCheckout, "-q", "--", "ln"); err != nil {
		t.Fatal(err)
	}
	if !isLink() {
		t.Error("the link was checked out as a file, because the repository config said so")
	}
}

func TestASymlinkedGitignoreIsNotRead(t *testing.T) {
	// Git has not followed a link for a .gitignore in the work tree since 2.32.
	r := newRepo(t, insideWork)
	patterns := filepath.Join(r.root, "patterns")
	if err := os.WriteFile(patterns, []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(patterns, filepath.Join(r.work, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	r.write("x.log", "x\n")

	result, err := r.run(r.runner(), cmdStatus, flagPorcelain, "-uall")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Stdout), "x.log") {
		t.Errorf("the patterns behind the link were applied: %q", result.Stdout)
	}
}

func TestAnExcludesFileOfTheConfigIsNotRead(t *testing.T) {
	r := newRepo(t, insideWork)
	patterns := filepath.Join(r.root, "patterns")
	if err := os.WriteFile(patterns, []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.config("core.excludesFile", patterns)
	r.write("x.log", "x\n")

	if out := r.plain(cmdStatus, flagPorcelain, "-uall"); strings.Contains(out, "x.log") {
		t.Fatal("plain Git did not apply the excludes file, so the test proves nothing")
	}
	result, err := r.run(r.runner(), cmdStatus, flagPorcelain, "-uall")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Stdout), "x.log") {
		t.Errorf("the excludes file of the config was applied: %q", result.Stdout)
	}
}

// submodules is a superproject, repo, at a commit where it has a populated
// submodule, and a newer commit, in a remote, where the submodule has moved.
type submodules struct {
	repo *repo
	// url is where the newer commit is, to fetch over the fake ssh, and path is the
	// same place for plain Git.
	url, path string
	// superRef is where the newer commit is once it has been fetched.
	superRef string
	// moved is the submodule's newer commit, and subWork its work tree.
	moved   string
	subWork string
	// fakeSSH and sshLog are the stand-in for ssh and what it was asked. The
	// submodule's remote is on ssh too, so that fetching it is possible: only a
	// setting can stop it.
	fakeSSH, sshLog string
}

func newSubmodules(t *testing.T) *submodules {
	t.Helper()
	// Git's own submodule command is a shell script, and the agent image has no
	// basename for it. Nothing in the agent runs it; only the test that prepares a
	// submodule does.
	if _, err := exec.LookPath("basename"); err != nil {
		t.Skip("git submodule, which prepares the test, needs basename, and this system has none")
	}
	r := newRepo(t, outsideWork)
	root := tempDir(t)
	source := filepath.Join(root, "source")
	author := filepath.Join(root, "author")
	remote := filepath.Join(root, "super.git")
	for _, dir := range []string{source, author} {
		gitIn(t, root, "init", "-q", "-b", "main", dir)
	}
	gitIn(t, root, "init", "-q", "--bare", remote)

	if err := os.WriteFile(filepath.Join(source, "s.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, cmdAdd, "s.txt")
	gitIn(t, source, cmdCommit, "-q", "-m", "s1")
	gitIn(t, author, "submodule", "-q", cmdAdd, source, "sub")
	gitIn(t, author, cmdCommit, "-q", "-m", "c1")
	gitIn(t, author, cmdPush, "-q", remote, "HEAD:refs/heads/main")

	// The repository under test starts at c1, with the submodule checked out.
	r.plain(cmdFetch, "-q", remote, "+refs/heads/main:refs/remotes/origin/main")
	r.plain("reset", "-q", "--hard", "refs/remotes/origin/main")
	r.plain("-c", "protocol.file.allow=always", "submodule", "-q", "update", "--init")
	gitIn(t, filepath.Join(r.work, "sub"), "remote", "set-url", "origin", "ssh://localhost"+source)

	// The author moves the submodule, and the new commit is in the remote.
	if err := os.WriteFile(filepath.Join(source, "s.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, cmdCommit, "-q", "-am", "s2")
	gitIn(t, filepath.Join(author, "sub"), cmdFetch, "-q", "origin", "main")
	gitIn(t, filepath.Join(author, "sub"), cmdCheckout, "-q", "FETCH_HEAD")
	gitIn(t, author, cmdAdd, "sub")
	gitIn(t, author, cmdCommit, "-q", "-m", "c2")
	gitIn(t, author, cmdPush, "-q", remote, "HEAD:refs/heads/main")

	// Whatever the repository's own config asks for.
	r.config("submodule.recurse", "true")
	r.config("fetch.recurseSubmodules", "true")
	r.config("submodule.sub.fetchRecurseSubmodules", "true")
	r.config("submodule.sub.update", cmdCheckout)

	fake, log := r.fakeSSH()
	return &submodules{
		repo: r, url: "ssh://localhost" + remote, path: remote, superRef: "refs/remotes/origin/main",
		moved: gitIn(t, source, "rev-parse", refHead), subWork: filepath.Join(r.work, "sub"),
		fakeSSH: fake, sshLog: log,
	}
}

// fetchPlain fetches the new commit with plain Git, which follows the submodule
// into its remote.
func (s *submodules) fetchPlain(t *testing.T) {
	t.Helper()
	env := s.repo.env("GIT_SSH_COMMAND=" + s.fakeSSH)
	if out, err := s.repo.plainErr(env, cmdFetch, "-q", s.path, "+refs/heads/main:"+s.superRef); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

// hasCommit reports whether the submodule's repository has the commit.
func (s *submodules) hasCommit(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command(s.repo.git, "-C", s.subWork, cmdCatFile, "-e", s.moved+"^{commit}")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), envGlobalNull, envSystemNull}
	return cmd.Run() == nil
}

// head is the commit that the submodule's work tree is at.
func (s *submodules) head(t *testing.T) string {
	t.Helper()
	return gitIn(t, s.subWork, "rev-parse", refHead)
}

func TestSubmodulesAreNotFetched(t *testing.T) {
	control := newSubmodules(t)
	control.fetchPlain(t)
	if !control.hasCommit(t) {
		t.Fatal("plain Git did not fetch the submodule, so the test proves nothing")
	}

	s := newSubmodules(t)
	runner := s.repo.runner(func(o *Options) { o.SSH.Program = s.fakeSSH })
	if _, err := s.repo.run(runner, cmdFetch, "-q", "--", s.url, "+refs/heads/main:"+s.superRef); err != nil {
		t.Fatal(err)
	}

	if s.hasCommit(t) {
		t.Error("the submodule was fetched")
	}
	if calls := strings.Count(readFile(t, s.sshLog), "\n"); calls != 1 {
		t.Errorf("ssh was called %d times, want once, for the superproject: %s", calls, readFile(t, s.sshLog))
	}
}

func TestACheckoutDoesNotMoveASubmodule(t *testing.T) {
	prepare := func(t *testing.T) *submodules {
		s := newSubmodules(t)
		s.fetchPlain(t)
		return s
	}

	control := prepare(t)
	before := control.head(t)
	control.repo.plain("reset", "-q", "--hard", control.superRef)
	if control.head(t) == before {
		t.Fatal("plain Git did not update the submodule, so the test proves nothing")
	}

	s := prepare(t)
	before = s.head(t)
	if _, err := s.repo.run(s.repo.runner(), "reset", "-q", "--hard", s.superRef); err != nil {
		t.Fatal(err)
	}
	if got := s.head(t); got != before {
		t.Errorf("the submodule moved from %s to %s", before, got)
	}
}

func TestStatusDoesNotLookInsideASubmodule(t *testing.T) {
	s := newSubmodules(t)
	if err := os.WriteFile(filepath.Join(s.subWork, "s.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := s.repo.plain(cmdStatus, flagPorcelain); !strings.Contains(out, "sub") {
		t.Fatalf("plain Git did not report the submodule, so the test proves nothing: %q", out)
	}

	result, err := s.repo.run(s.repo.runner(), cmdStatus, flagPorcelain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Stdout), "sub") {
		t.Errorf("status looked inside the submodule: %q", result.Stdout)
	}
}

func TestCommandsThatWouldFetchSubmodulesCannotBeRun(t *testing.T) {
	r := newRepo(t, outsideWork)
	runner := r.runner()
	for _, args := range [][]string{
		{"submodule", "update", "--init", "--recursive"},
		{"clone", "--recurse-submodules", "https://example.invalid/r.git"},
		{"pull", "--recurse-submodules"},
	} {
		_, err := runner.Run(context.Background(), args...)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("git %s: got %v, want it refused", args[0], err)
		}
	}
}

func TestAnApplicationPlantedGitDirectoryIsNotUsed(t *testing.T) {
	r := newRepo(t, outsideWork)
	// An application that can write the work tree plants a repository there.
	gitIn(t, r.work, "init", "-q")
	hook := filepath.Join(r.work, ".git", "hooks", "pre-commit")
	marker := r.marker("planted")
	r.scriptAt(hook, fmt.Sprintf("echo ran >> '%s'", marker))
	r.scriptAt(filepath.Join(r.root, "fsmonitor"), fmt.Sprintf("echo ran >> '%s'", marker))
	gitIn(t, r.work, "config", "core.fsmonitor", filepath.Join(r.root, "fsmonitor"))

	// Git finds that repository when it looks for one, as plain Git does...
	if _, err := r.plainErr(withoutRepositoryVariables(r.env()), "-c", "user.name=a", "-c", "user.email=a@b", cmdCommit, "-q", flagAllowEmpty, "-m", "x"); err != nil {
		t.Fatal(err)
	}
	wantRan(t, marker, "the planted hook")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	// ... but the Runner names its own Git directory, so it never does.
	if _, err := r.run(r.runner(), cmdCommit, "-q", flagAllowEmpty, "-m", "x"); err != nil {
		t.Fatal(err)
	}
	wantNotRan(t, marker, "the planted hook")
	if out := gitIn(t, r.work, "rev-list", "--all", "--count"); out != "1" {
		t.Errorf("the planted repository has %s commits, want the one that the control made", out)
	}
	if out := gitIn(t, r.work, "--git-dir="+r.gitDir, "rev-list", "--count", refHead); out != "2" {
		t.Errorf("the Runner's repository has %s commits, want its own two", out)
	}
}

func TestSafeDirectoryNamesTheWorkTreeOnly(t *testing.T) {
	runner, err := New(Options{GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath})
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for i, arg := range runner.settings {
		if v, ok := strings.CutPrefix(arg, "safe.directory="); ok && runner.settings[i-1] == "-c" {
			values = append(values, v)
		}
	}
	if len(values) != 1 || values[0] != fakeWorkDir {
		t.Errorf("safe.directory is set to %q, want only the work tree", values)
	}
}

func TestSafeDirectoryAcceptsTheWorkTreeAndNoOtherDirectory(t *testing.T) {
	r := newRepo(t, insideWork)
	other := newRepo(t, insideWork)
	runner := r.runner()

	// A directory that someone else owns cannot be had in a test that does not run
	// as root, so Git is told to treat every directory as one.
	discover := func(dir string) error {
		cmd := runner.command(context.Background(), []string{cmdStatus, flagPorcelain})
		cmd.Dir = dir
		cmd.Env = append(withoutRepositoryVariables(cmd.Env), "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}

	other.plain(cmdStatus)
	probe := other.plainCmd(append(withoutRepositoryVariables(other.env()), "GIT_TEST_ASSUME_DIFFERENT_OWNER=1"), cmdStatus)
	if out, err := probe.CombinedOutput(); err == nil || !strings.Contains(string(out), "dubious ownership") {
		t.Skipf("this Git cannot be made to see a directory as owned by someone else: %v: %s", err, out)
	}

	if err := discover(r.work); err != nil {
		t.Errorf("the work tree was refused: %v", err)
	}
	if err := discover(other.work); err == nil || !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("another directory was accepted, or failed for another reason: %v", err)
	}
	// With the repository named, as the Runner does, the check is not made.
	cmd := runner.command(context.Background(), []string{cmdStatus, flagPorcelain})
	cmd.Env = append(cmd.Env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the Runner's own command was refused: %v: %s", err, out)
	}
}

// withoutRepositoryVariables drops GIT_DIR and GIT_WORK_TREE, so that Git looks for
// the repository from the current directory.
func withoutRepositoryVariables(env []string) []string {
	var kept []string
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_DIR=") && !strings.HasPrefix(e, "GIT_WORK_TREE=") {
			kept = append(kept, e)
		}
	}
	return kept
}

func TestNothingIsInheritedFromTheEnvironment(t *testing.T) {
	r := newRepo(t, outsideWork)
	fake, _ := r.fakeSSH()
	url, remote := r.remote()
	trap := func(name string) string {
		script, _ := r.trap(name)
		return script
	}

	// Each source of configuration or of behaviour gets a trap of its own, so that
	// a trap that springs says where it came from.
	evilHooks := filepath.Join(r.root, "evilhooks")
	r.scriptAt(filepath.Join(evilHooks, "pre-commit"), fmt.Sprintf("echo ran >> '%s'", r.marker("hooks-count")))
	identity := "[user]\n\tname = Evil\n\temail = evil@example.com\n"
	hostileConfig := filepath.Join(r.root, "hostile.cfg")
	r.write("../hostile.cfg", identity)
	home := filepath.Join(r.root, "evilhome")
	r.write("../evilhome/.gitconfig", identity)
	r.write("../evilhome/.config/git/config", identity)
	trace := filepath.Join(r.root, "trace")
	for k, v := range map[string]string{
		"HOME":                  home,
		"XDG_CONFIG_HOME":       filepath.Join(home, ".config"),
		envConfigGlobal:         hostileConfig,
		"GIT_CONFIG_SYSTEM":     hostileConfig,
		"GIT_CONFIG_COUNT":      "1",
		"GIT_CONFIG_KEY_0":      "core.hooksPath",
		"GIT_CONFIG_VALUE_0":    evilHooks,
		"GIT_CONFIG_PARAMETERS": "'core.fsmonitor'='" + trap("fsmonitor-parameters") + "'",
		envGitDir:               filepath.Join(r.root, "not-the-repository"),
		envWorkTree:             filepath.Join(r.root, "not-the-work-tree"),
		"GIT_INDEX_FILE":        filepath.Join(r.root, "not-the-index"),
		"GIT_OBJECT_DIRECTORY":  filepath.Join(r.root, "not-the-objects"),
		envSSHCommand:           trap(markerSSH),
		"GIT_TRACE":             trace,
		"GIT_ALLOW_PROTOCOL":    "file:ext:git:http:ssh:https",
		"GIT_AUTHOR_NAME":       "Evil",
		"GIT_AUTHOR_EMAIL":      "evil@example.com",
	} {
		t.Setenv(k, v)
	}
	markers := []string{markerSSH, "hooks-count", "fsmonitor-parameters"}

	// Control: Git that inherits all of it springs every trap, makes the commit as
	// the author that the environment names, and writes the trace.
	control := append(os.Environ(), "GIT_DIR="+r.gitDir, "GIT_WORK_TREE="+r.work,
		"GIT_INDEX_FILE="+filepath.Join(r.gitDir, "index"), "GIT_OBJECT_DIRECTORY="+filepath.Join(r.gitDir, "objects"))
	_, _ = r.plainErr(control, cmdLsRemote, "ssh://localhost"+remote)
	if _, err := r.plainErr(control, cmdCommit, "-q", flagAllowEmpty, "-m", "control"); err != nil {
		t.Fatal(err)
	}
	for _, name := range markers {
		wantRan(t, r.marker(name), name)
		_ = os.Remove(r.marker(name))
	}
	if author := gitIn(t, r.work, "--git-dir="+r.gitDir, "log", "-1", "--format=%an <%ae>"); author != "Evil <evil@example.com>" {
		t.Fatalf("plain Git did not read the author of the environment, so the test proves nothing: %s", author)
	}
	if !exists(trace) {
		t.Fatal("plain Git did not write the trace of GIT_TRACE, so the test proves nothing")
	}
	if err := os.Remove(trace); err != nil {
		t.Fatal(err)
	}

	runner := r.runner(func(o *Options) { o.SSH.Program = fake })
	for _, args := range [][]string{
		{cmdLsRemote, "--", url},
		{cmdCommit, "-q", flagAllowEmpty, "-m", "runner"},
		{cmdStatus, flagPorcelain},
	} {
		if _, err := r.run(runner, args...); err != nil {
			t.Fatalf("git %s: %v", args[0], err)
		}
	}
	_, err := r.run(runner, cmdLsRemote, "--", "file://"+remote)
	wantGitFailure(t, err, msgNotAllowed)

	for _, name := range markers {
		wantNotRan(t, r.marker(name), name)
	}
	if exists(trace) {
		t.Error("GIT_TRACE was inherited")
	}
	// The commit went to the repository that the Runner names, as the repository's
	// own user.
	if author := gitIn(t, r.work, "--git-dir="+r.gitDir, "log", "-1", "--format=%an <%ae> %s"); author != "Test <test@example.com> runner" {
		t.Errorf("the last commit is %q: the environment was read, or the repository was not the Runner's", author)
	}

	// Every setting that applies comes from the repository's own config or from the
	// command line of the Runner, and from nothing else.
	result, err := runner.run(context.Background(), 0, []string{"config", "--list", "--show-origin"})
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(result.Stdout)), "\n") {
		if !strings.HasPrefix(line, "command line:") && !strings.HasPrefix(line, "file:"+filepath.Join(r.gitDir, "config")) {
			t.Errorf("a setting comes from somewhere else: %s", line)
		}
	}
}

func TestTheWorkTreeOfTheRepositoryConfigIsNotUsed(t *testing.T) {
	r := newRepo(t, outsideWork)
	elsewhere := filepath.Join(r.root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	r.plain("config", "core.worktree", elsewhere)

	// Told only where the Git directory is, Git takes the work tree from the config.
	env := append(withoutRepositoryVariables(r.env()), "GIT_DIR="+r.gitDir)
	if out, err := r.plainErr(env, "rev-parse", "--show-toplevel"); err != nil || strings.TrimSpace(out) != elsewhere {
		t.Fatalf("plain Git did not move the work tree, so the test proves nothing: %q, %v", out, err)
	}

	result, err := r.run(r.runner(), "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != r.work {
		t.Errorf("the work tree is %s, want the one that was given, %s", got, r.work)
	}
}

func TestTheGlobalConfigFileIsNotReadWhateverHomeIs(t *testing.T) {
	r := newRepo(t, outsideWork)
	// With no identity of its own, the repository gets one only from a file
	// that Git reads outside of it.
	r.plain("config", "--unset", "user.name")
	r.plain("config", "--unset", "user.email")
	home := filepath.Join(r.root, "evilhome")
	r.write("../evilhome/.gitconfig", "[user]\n\tname = Evil\n\temail = evil@example.com\n")
	commit := func(dropGlobal bool) error {
		cmd := r.runner().command(context.Background(), []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"})
		env := make([]string, 0, len(cmd.Env))
		for _, e := range cmd.Env {
			switch {
			case strings.HasPrefix(e, "HOME="):
				e = "HOME=" + home
			case dropGlobal && strings.HasPrefix(e, "GIT_CONFIG_GLOBAL="):
				continue
			}
			env = append(env, e)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}

	if err := commit(true); err != nil {
		t.Fatalf("Git did not read the file in the home directory, so the test proves nothing: %v", err)
	}
	if err := commit(false); err == nil {
		t.Error("the commit was made with the identity of a file in the home directory")
	}
}
