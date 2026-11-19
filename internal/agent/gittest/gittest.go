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

// Package gittest is what the tests of the parts of the agent that run Git share: a repository
// served over Git's real ssh transport by a stand-in that runs what it is asked locally, so that
// a test fetches and pushes with the real protections of the Runner and no server, and a claim
// to make a checkout on. It is for tests only and nothing but tests imports it.
package gittest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/checkout"
	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// Fixture is a repository on a stand-in for an ssh server, and an empty claim.
type Fixture struct {
	// T is the test that owns the fixture.
	T *testing.T
	// URL is the address of the repository.
	URL string
	// Bare is the repository on disk, and Claim the claim to make the checkout on.
	Bare, Claim string

	git, root, home, scratch, ssh, sshLog string
}

// Binary returns the git that the tests run, or skips the test.
func Binary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed, and these tests run the real git binary")
	}
	return path
}

// TempDir makes a directory that the Runner takes: its name has letters, digits and . _ / + @ -.
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tbgt-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// New makes an empty repository with the branch main, the stand-in for ssh that serves it, and an
// empty claim.
func New(t *testing.T) *Fixture {
	t.Helper()
	f := &Fixture{T: t, git: Binary(t), root: TempDir(t)}
	f.home = filepath.Join(f.root, "home")
	f.scratch = filepath.Join(f.root, "scratch")
	f.Claim = filepath.Join(f.root, "claim")
	f.Bare = filepath.Join(f.root, "remote.git")
	f.URL = "ssh://localhost" + f.Bare
	for _, dir := range []string{f.home, f.scratch, f.Claim, filepath.Join(f.root, "fake")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.Plain(f.root, "init", "-q", "--bare", "--initial-branch=main", f.Bare)
	f.Plain(f.scratch, "init", "-q", "--initial-branch=main")

	execPath := strings.TrimSpace(f.Plain(f.root, "--exec-path"))
	// It is named ssh, because Git runs a program of another name with -G to find out what kind
	// of ssh it is.
	f.ssh = filepath.Join(f.root, "fake", "ssh")
	f.sshLog = filepath.Join(f.root, "ssh.log")
	script := fmt.Sprintf("#!/bin/sh\ncmd=\nfor a in \"$@\"; do cmd=$a; done\necho \"$cmd\" >> '%s'\nPATH='%s':\"$PATH\"\nexec sh -c \"$cmd\"\n",
		f.sshLog, execPath)
	if err := os.WriteFile(f.ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// Sessions returns how many times the repository was reached over ssh since the fixture was made:
// each command of Git that asks the repository, a listing of its branches or a fetch, is one.
func (f *Fixture) Sessions() int {
	f.T.Helper()
	data, err := os.ReadFile(f.sshLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		f.T.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// Plain runs Git with none of the protection, as someone who prepares a repository does, and
// fails the test if it fails.
func (f *Fixture) Plain(dir string, args ...string) string {
	f.T.Helper()
	out, err := f.PlainErr(dir, nil, args...)
	if err != nil {
		f.T.Fatal(err)
	}
	return out
}

// PlainErr is Plain with the environment added to, and the error returned.
func (f *Fixture) PlainErr(dir string, extra []string, args ...string) (string, error) {
	cmd := exec.Command(f.git, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + f.home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}, extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// Publish commits the files, given by path, on top of the scratch repository, pushes the commit
// to the branch of the repository and returns it. A file whose content is empty is removed. A
// history that was rewritten is published with Rewrite.
func (f *Fixture) Publish(branch string, files map[string]string) string {
	f.T.Helper()
	for name, content := range files {
		path := filepath.Join(f.scratch, name)
		if content == "" {
			if err := os.Remove(path); err != nil {
				f.T.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.T.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.T.Fatal(err)
		}
	}
	f.Plain(f.scratch, "add", "-A")
	f.Plain(f.scratch, "commit", "-q", "--allow-empty", "-m", "change")
	f.Plain(f.scratch, "push", "-q", "--force", f.Bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.Plain(f.scratch, "rev-parse", "HEAD"))
}

// PublishExecutable is Publish for files that are executable: they are written with the mode 0755, and the commit has
// them as such.
func (f *Fixture) PublishExecutable(branch string, files map[string]string) string {
	f.T.Helper()
	for name, content := range files {
		path := filepath.Join(f.scratch, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.T.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			f.T.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			f.T.Fatal(err)
		}
	}
	f.Plain(f.scratch, "add", "-A")
	f.Plain(f.scratch, "commit", "-q", "--allow-empty", "-m", "executable")
	f.Plain(f.scratch, "push", "-q", "--force", f.Bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.Plain(f.scratch, "rev-parse", "HEAD"))
}

// PublishRaw commits the files with exactly the bytes given, whatever the attributes of the scratch repository say: they are
// made into blobs with no conversion and put in the index by name, so that a file with both kinds of line ending is in the
// repository with both, as one that was committed before an attribute was set. It is for a test that publishes nothing else
// after it, because the scratch repository's own files are not kept in step with the index.
func (f *Fixture) PublishRaw(branch string, files map[string]string) string {
	f.T.Helper()
	aside := filepath.Join(f.root, "raw")
	for name, content := range files {
		if err := os.WriteFile(aside, []byte(content), 0o644); err != nil {
			f.T.Fatal(err)
		}
		blob := strings.TrimSpace(f.Plain(f.scratch, "hash-object", "-w", "--no-filters", aside))
		f.Plain(f.scratch, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	f.Plain(f.scratch, "commit", "-q", "--allow-empty", "-m", "raw")
	f.Plain(f.scratch, "push", "-q", "--force", f.Bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.Plain(f.scratch, "rev-parse", "HEAD"))
}

// PublishLink commits a link in the scratch repository, with the target, and pushes it as Publish does.
func (f *Fixture) PublishLink(branch, name, target string) string {
	f.T.Helper()
	path := filepath.Join(f.scratch, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.T.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		f.T.Fatal(err)
	}
	f.Plain(f.scratch, "add", "-A")
	f.Plain(f.scratch, "commit", "-q", "--allow-empty", "-m", "link")
	f.Plain(f.scratch, "push", "-q", "--force", f.Bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.Plain(f.scratch, "rev-parse", "HEAD"))
}

// PublishIgnored is Publish for files that a .gitignore of the scratch repository hides: they are
// added by name, which Git does for an ignored file only when it is forced.
func (f *Fixture) PublishIgnored(branch string, files map[string]string) string {
	f.T.Helper()
	for name, content := range files {
		path := filepath.Join(f.scratch, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.T.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.T.Fatal(err)
		}
		f.Plain(f.scratch, "add", "-f", "--", name)
	}
	f.Plain(f.scratch, "commit", "-q", "--allow-empty", "-m", "change")
	f.Plain(f.scratch, "push", "-q", "--force", f.Bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.Plain(f.scratch, "rev-parse", "HEAD"))
}

// Rewrite resets the history of the scratch repository to the commit before its last, as a
// force-push of an amended branch would, and the next Publish is a history that does not contain
// what was there.
func (f *Fixture) Rewrite() {
	f.T.Helper()
	f.Plain(f.scratch, "reset", "-q", "--hard", "HEAD~1")
}

// Layout returns the layout of the claim for a sub path, with the state directory made as the
// agent does at start.
func (f *Fixture) Layout(sub string) layout.Layout {
	f.T.Helper()
	l, err := layout.New(f.Claim, sub)
	if err != nil {
		f.T.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		f.T.Fatal(err)
	}
	return l
}

// GitOptions are the settings of the Runner with the stand-in for ssh.
func (f *Fixture) GitOptions() git.Options {
	return git.Options{
		Git: f.git, Timeout: time.Minute,
		SSH: git.SSH{
			Program:        f.ssh,
			IdentityFile:   filepath.Join(f.root, "id_ed25519"),
			KnownHostsFile: filepath.Join(f.root, "known_hosts"),
		},
	}
}

// CheckoutOptions are the options for the checkout of a layout and a branch.
func (f *Fixture) CheckoutOptions(l layout.Layout, branch string) checkout.Options {
	return checkout.Options{Layout: l, URL: f.URL, Branch: branch, FetchTimeout: time.Minute, Git: f.GitOptions()}
}

// InCheckout runs plain Git on the checkout of a layout.
func (f *Fixture) InCheckout(l layout.Layout, args ...string) string {
	f.T.Helper()
	out, err := f.PlainErr(l.WorkTree, []string{"GIT_DIR=" + l.GitDir, "GIT_WORK_TREE=" + l.WorkTree}, args...)
	if err != nil {
		f.T.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// Write puts a file in the work tree.
func Write(t *testing.T, l layout.Layout, name, content string) {
	t.Helper()
	path := filepath.Join(l.WorkTree, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Read returns a file of the work tree, or fails the test.
func Read(t *testing.T, l layout.Layout, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.WorkTree, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Exists reports whether a path is there, without following a link.
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
