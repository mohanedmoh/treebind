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
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Strings that the tests repeat. A constant says what a string is for, and the
// linter asks for one.
const (
	cmdAdd         = "add"
	cmdCheckout    = "checkout"
	cmdCommit      = "commit"
	cmdStatus      = "status"
	cmdPush        = "push"
	cmdFetch       = "fetch"
	cmdMerge       = "merge"
	cmdCatFile     = "cat-file"
	cmdDiffIndex   = "diff-index"
	flagNoEdit     = "--no-edit"
	flagNoFF       = "--no-ff"
	refHead        = "HEAD"
	branchSide     = "side"
	urlExample     = "ssh://example.invalid/r"
	urlGitProto    = "git://127.0.0.1:1/r.git"
	msgNoPrompt    = "terminal prompts disabled"
	msgNotAllowed  = "not allowed"
	msgOlder       = "older than 2.40"
	gitPath        = "/usr/bin/git"
	fakeWorkDir    = "/work"
	cmdLsRemote    = "ls-remote"
	flagAllowEmpty = "--allow-empty"
	flagPorcelain  = "--porcelain"
	fileA          = "a.txt"
	stateGitDir    = "/state/git"
	wantAbsolute   = "absolute, clean"
	valueAlways    = "always"
	sshName        = "ssh"
	markerSSH      = "ssh"
	wantNull       = "/dev/null"
	wantTrue       = "true"
	wantFalse      = "false"

	envGlobalNull   = "GIT_CONFIG_GLOBAL=/dev/null"
	envSystemNull   = "GIT_CONFIG_SYSTEM=/dev/null"
	envConfigGlobal = "GIT_CONFIG_GLOBAL"
	envGitDir       = "GIT_DIR"
	envWorkTree     = "GIT_WORK_TREE"
	envSSHCommand   = "GIT_SSH_COMMAND"
)

// The scripts and hooks that the tests plant use only what a shell has built in
// (echo, test, loops), because the image of the agent has no touch, cat or sleep
// and the tests are also run there.

// gitBinary returns the git the tests use, or skips the test when there is none.
func gitBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed, and these tests run the real git binary")
	}
	return path
}

// needGit skips the test when the git binary is older than major.minor. The agent
// image carries Git 2.50.1 or newer (hack/check-agent-image.sh), so a protection
// that needs a newer Git than a developer's machine has is proved in CI and in the
// image.
func needGit(t *testing.T, major, minor int, why string) {
	t.Helper()
	out, err := exec.Command(gitBinary(t), "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("cannot read the version from %q", out)
	}
	have, _ := strconv.Atoi(m[1])
	haveMinor, _ := strconv.Atoi(m[2])
	if have < major || (have == major && haveMinor < minor) {
		t.Skipf("%s is older than %d.%d, which %s; CI and the agent image have a newer one",
			strings.TrimSpace(string(out)), major, minor, why)
	}
}

// tempDir makes a directory that is cleaned up, with a name made of the
// characters that the Runner accepts in a path that it gives to ssh. t.TempDir
// puts the name of the test in the path.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tbgit-")
	if err != nil {
		t.Fatal(err)
	}
	// A temporary directory is behind a symlink on some systems.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// layout says where the Git directory of a test repository is.
type layout string

const (
	// outsideWork is a Git directory next to the work tree, which is what the
	// agent aims for (R-17).
	outsideWork layout = "gitdir outside the work tree"
	// insideWork is .git in the work tree, where an application can write it.
	insideWork layout = "gitdir inside the work tree"
)

var layouts = []layout{outsideWork, insideWork}

// repo is a repository the tests prepare with plain, unprotected Git, as someone
// who wants to plant something would, and then run the Runner on.
type repo struct {
	t      *testing.T
	root   string
	work   string
	gitDir string
	home   string
	git    string
}

// newRepo makes a repository with one commit, identity set in its own config.
func newRepo(t *testing.T, l layout) *repo {
	t.Helper()
	root := tempDir(t)
	r := &repo{t: t, root: root, work: filepath.Join(root, "work"), home: filepath.Join(root, "home"), git: gitBinary(t)}
	r.gitDir = filepath.Join(root, "gitdir")
	if l == insideWork {
		r.gitDir = filepath.Join(r.work, ".git")
	}
	for _, dir := range []string{r.work, r.home, filepath.Join(root, "scripts")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.plain("init", "-q")
	// The name of the default branch depends on the Git release and its settings.
	r.plain("symbolic-ref", refHead, "refs/heads/main")
	r.plain("config", "user.name", "Test")
	r.plain("config", "user.email", "test@example.com")
	r.write(fileA, "a\n")
	r.plain(cmdAdd, fileA)
	r.plain(cmdCommit, "-q", "-m", "first")
	return r
}

// env is the environment of plain Git: the repository, and nothing from the
// machine that runs the tests.
func (r *repo) env(extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		envGlobalNull,
		envSystemNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"GIT_DIR=" + r.gitDir,
		"GIT_WORK_TREE=" + r.work,
	}, extra...)
}

// plainCmd is a git command without any of the protection, in the work tree.
func (r *repo) plainCmd(env []string, args ...string) *exec.Cmd {
	cmd := exec.Command(r.git, args...)
	cmd.Dir = r.work
	cmd.Env = env
	return cmd
}

// plain runs plain Git and fails the test if it fails.
func (r *repo) plain(args ...string) string {
	r.t.Helper()
	out, err := r.plainErr(r.env(), args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// plainErr runs plain Git with the given environment and returns what it wrote.
func (r *repo) plainErr(env []string, args ...string) (string, error) {
	out, err := r.plainCmd(env, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// runner returns a Runner for the repository. The options can be changed first.
func (r *repo) runner(change ...func(*Options)) *Runner {
	r.t.Helper()
	o := Options{
		GitDir: r.gitDir, WorkTree: r.work, Git: r.git, Timeout: time.Minute,
		// Tests of the settings run with the Git directory where an application can
		// write it, to show that the settings hold there too.
		AllowGitDirInWorkTree: strings.HasPrefix(r.gitDir, r.work+"/"),
	}
	for _, c := range change {
		c(&o)
	}
	runner, err := New(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return runner
}

// run runs a command with the Runner and returns its output.
func (r *repo) run(runner *Runner, args ...string) (Result, error) {
	r.t.Helper()
	return runner.Run(context.Background(), args...)
}

// write puts a file in the work tree.
func (r *repo) write(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.work, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// marker is the path of a file that a planted script creates when it runs, and
// that nothing else creates.
func (r *repo) marker(name string) string {
	return filepath.Join(r.root, "marker-"+name)
}

// scriptAt writes an executable shell script.
func (r *repo) scriptAt(path, body string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// script writes an executable shell script and returns its path.
func (r *repo) script(name, body string) string {
	r.t.Helper()
	path := filepath.Join(r.root, "scripts", name)
	r.scriptAt(path, body)
	return path
}

// trap is a script that notes in the marker that it ran.
func (r *repo) trap(name string) (script, marker string) {
	marker = r.marker(name)
	return r.script(name, fmt.Sprintf("echo ran >> '%s'", marker)), marker
}

// hook plants a hook in the Git directory that notes in the marker that it ran.
func (r *repo) hook(name string) (marker string) {
	r.t.Helper()
	marker = r.marker("hook")
	r.scriptAt(filepath.Join(r.gitDir, "hooks", name), fmt.Sprintf("echo %s >> '%s'", name, marker))
	return marker
}

// config sets a key in the repository's own config, as an application that can
// write .git/config would.
func (r *repo) config(key, value string) {
	r.t.Helper()
	r.plain("config", "--add", key, value)
}

// fakeSSH writes a program that stands in for ssh: it records its arguments and
// runs the command that it was given locally, the way sshd would. With it the tests
// fetch from a real repository over Git's real ssh transport, without a server.
func (r *repo) fakeSSH() (program, log string) {
	r.t.Helper()
	log = filepath.Join(r.root, "ssh.log")
	execPath, err := exec.Command(r.git, "--exec-path").Output()
	if err != nil {
		r.t.Fatal(err)
	}
	// It is named ssh, because Git runs a program of another name with -G to find
	// out what kind of ssh it is.
	program = filepath.Join(r.root, "fake", "ssh")
	r.scriptAt(program, fmt.Sprintf(`echo "$*" >> '%s'
cmd=
for a in "$@"; do cmd=$a; done
PATH='%s':"$PATH"
exec sh -c "$cmd"`, log, strings.TrimSpace(string(execPath))))
	return program, log
}

// remote makes a bare repository with the work tree's history and returns the
// ssh:// URL that the fake ssh serves it on.
func (r *repo) remote() (url, path string) {
	r.t.Helper()
	path = filepath.Join(r.root, "remote.git")
	if out, err := exec.Command(r.git, "init", "-q", "--bare", path).CombinedOutput(); err != nil {
		r.t.Fatalf("%v: %s", err, out)
	}
	r.plain(cmdPush, "-q", path, "HEAD:refs/heads/main")
	return "ssh://localhost" + path, path
}

// exists reports whether a path exists, without following a symlink.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// wantRan fails the test unless the marker exists, which proves that a trap was
// armed and that the test could see it spring.
func wantRan(t *testing.T, marker, what string) {
	t.Helper()
	if !exists(marker) {
		t.Errorf("%s did not run in the unprotected control, so the test would pass with no protection", what)
	}
}

// wantNotRan fails the test if the marker exists.
func wantNotRan(t *testing.T, marker, what string) {
	t.Helper()
	if exists(marker) {
		t.Errorf("%s ran", what)
	}
}

// wantGitFailure checks that the error is a Git command that ran and failed with
// text that has the wanted substring, and not one that did not start, timed out
// or was refused by the Runner. A protection test that ends in an error has to
// end in the right one.
func wantGitFailure(t *testing.T, err error, substring string) {
	t.Helper()
	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("got %v, want a git failure containing %q", err, substring)
	}
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("got %v, which is not a command that ran and failed", err)
	}
	if !strings.Contains(gitErr.Stderr, substring) {
		t.Fatalf("the error says %q, want it to contain %q", gitErr.Stderr, substring)
	}
}

// webServer is an https server that records what it is asked, so that a test can
// tell what Git sent and what it did not send. Its certificate is not trusted by
// the system: Git reaches it only with the CA bundle that the test names.
type webServer struct {
	*httptest.Server
	// CABundle is a file that holds the server's certificate.
	CABundle string

	mu       sync.Mutex
	requests []*http.Request
}

// newWebServer starts a server that answers with handler.
func newWebServer(t *testing.T, dir string, handler http.HandlerFunc) *webServer {
	t.Helper()
	w := &webServer{}
	w.Server = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		w.mu.Lock()
		w.requests = append(w.requests, req)
		w.mu.Unlock()
		handler(rw, req)
	}))
	t.Cleanup(w.Close)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: w.Certificate().Raw})
	w.CABundle = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(w.CABundle, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return w
}

// seen returns what the server was asked so far.
func (w *webServer) seen() []*http.Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.requests)
}

// unauthorized answers every request as a server does that wants a login.
func unauthorized(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("WWW-Authenticate", `Basic realm="treebind"`)
	rw.WriteHeader(http.StatusUnauthorized)
}

// notFound answers every request as a server does that has no such repository.
func notFound(rw http.ResponseWriter, _ *http.Request) {
	rw.WriteHeader(http.StatusNotFound)
}

// readFile returns the text of a file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
