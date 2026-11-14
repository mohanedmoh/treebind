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
package checkout

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
)

// Names that the tests use again and again.
const (
	branchMain  = "main"
	branchTrunk = "trunk"
	subPath     = "dashboards"
	fileA       = "a.json"
	fileB       = "sub/b.json"
	contentA    = "a\n"
	contentB    = "b\n"
	stateName   = ".treebind"
	mine        = "mine\n"
	onOther     = "on other\n"
)

// fixture is a repository on a stand-in for an ssh server, and a claim to check it out
// on.
type fixture struct {
	t     *testing.T
	git   string
	root  string
	home  string
	scrat string
	// bare is the repository that the Treebind follows, and url the address of it.
	bare, url string
	ssh       string
	claim     string
}

// gitBinary returns the git that the tests run, or skips them.
func gitBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return path
}

// tempDir makes a directory whose name the Runner takes: letters, digits and . _ / + @ -.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tbco-")
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

// newFixture makes an empty repository, served over Git's real ssh transport by a program
// that runs what it is asked to locally, and an empty claim.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, git: gitBinary(t), root: tempDir(t)}
	f.home = filepath.Join(f.root, "home")
	f.scrat = filepath.Join(f.root, "scratch")
	f.claim = filepath.Join(f.root, "claim")
	f.bare = filepath.Join(f.root, "remote.git")
	f.url = "ssh://localhost" + f.bare
	for _, dir := range []string{f.home, f.scrat, f.claim, filepath.Join(f.root, "fake")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.plain(f.root, "init", "-q", "--bare", "--initial-branch="+branchMain, f.bare)
	f.plain(f.scrat, "init", "-q", "--initial-branch="+branchMain)

	execPath := strings.TrimSpace(f.plain(f.root, "--exec-path"))
	// It is named ssh, because Git runs a program of another name with -G to find out what
	// kind of ssh it is.
	f.ssh = filepath.Join(f.root, "fake", "ssh")
	script := fmt.Sprintf("#!/bin/sh\ncmd=\nfor a in \"$@\"; do cmd=$a; done\nPATH='%s':\"$PATH\"\nexec sh -c \"$cmd\"\n", execPath)
	if err := os.WriteFile(f.ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// plain runs Git with none of the protection, as someone who prepares a repository does.
func (f *fixture) plain(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.plainErr(dir, nil, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) plainErr(dir string, extra []string, args ...string) (string, error) {
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

// publish commits the files, given by path, on top of the scratch repository and pushes
// the commit to the branch of the repository. It returns the commit. A file whose content
// is empty is removed.
func (f *fixture) publish(branch string, files map[string]string) string {
	f.t.Helper()
	for name, content := range files {
		path := filepath.Join(f.scrat, name)
		if content == "" {
			if err := os.Remove(path); err != nil {
				f.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.plain(f.scrat, "add", "-A")
	f.plain(f.scrat, "commit", "-q", "--allow-empty", "-m", "change")
	f.plain(f.scrat, "push", "-q", "--force", f.bare, "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(f.plain(f.scrat, "rev-parse", "HEAD"))
}

// layout returns the layout of the claim for a sub path, with the state directory made as
// the agent does at start.
func (f *fixture) layout(sub string) layout.Layout {
	f.t.Helper()
	l, err := layout.New(f.claim, sub)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		f.t.Fatal(err)
	}
	return l
}

// options are the options for a layout and a branch, with the stand-in for ssh.
func (f *fixture) options(l layout.Layout, branch string) Options {
	return Options{
		Layout: l, URL: f.url, Branch: branch, FetchTimeout: time.Minute,
		Git: git.Options{
			Git: f.git, Timeout: time.Minute,
			SSH: git.SSH{
				Program:        f.ssh,
				IdentityFile:   filepath.Join(f.root, "id_ed25519"),
				KnownHostsFile: filepath.Join(f.root, "known_hosts"),
			},
		},
	}
}

// inCheckout runs plain Git on the checkout of a layout.
func (f *fixture) inCheckout(l layout.Layout, args ...string) string {
	f.t.Helper()
	out, err := f.plainErr(l.WorkTree, []string{"GIT_DIR=" + l.GitDir, "GIT_WORK_TREE=" + l.WorkTree}, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// write puts a file in the work tree.
func write(t *testing.T, l layout.Layout, name, content string) {
	t.Helper()
	path := filepath.Join(l.WorkTree, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readA returns the file a.json of the work tree, or fails the test.
func readA(t *testing.T, l layout.Layout) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.WorkTree, fileA))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// exists reports whether a path is there, without following a link.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// wantReason fails the test unless err is an *Error with the reason.
func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	_ = errorOf(t, err, reason)
}

// errorOf fails the test unless err is an *Error with the reason, and returns it.
func errorOf(t *testing.T, err error, reason string) *Error {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) {
		t.Fatalf("error = %v (%T), want an *Error with the reason %s", err, err, reason)
	}
	if got.Reason != reason {
		t.Fatalf("reason = %s (%s), want %s", got.Reason, got.Message, reason)
	}
	return got
}

// wantFiles fails the test unless the files of the work tree are exactly these, not
// counting the state directory when it is inside the work tree.
func wantFiles(t *testing.T, l layout.Layout, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(l.WorkTree, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(l.WorkTree, path)
		if entry.IsDir() && rel == stateName && l.GitDirInWorkTree {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		got[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for name, content := range want {
		if got[name] != content {
			t.Fatalf("files = %v, want %v", got, want)
		}
	}
}
