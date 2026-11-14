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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	valid := Options{GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath}
	tests := []struct {
		name   string
		change func(*Options)
		want   string
	}{
		{"valid", func(*Options) {}, ""},
		{"no git dir", func(o *Options) { o.GitDir = "" }, "GitDir is required"},
		{"no work tree", func(o *Options) { o.WorkTree = "" }, "WorkTree is required"},
		{"relative git dir", func(o *Options) { o.GitDir = "state/git" }, wantAbsolute},
		{"relative work tree", func(o *Options) { o.WorkTree = "work" }, wantAbsolute},
		{"unclean work tree", func(o *Options) { o.WorkTree = "/work/../etc" }, wantAbsolute},
		{"trailing slash", func(o *Options) { o.WorkTree = "/work/" }, wantAbsolute},
		{"a pattern as the work tree", func(o *Options) { o.WorkTree = "/work/*" }, "must not contain '*'"},
		{"a star as the work tree", func(o *Options) { o.WorkTree = "*" }, wantAbsolute},
		{"a newline in the git dir", func(o *Options) { o.GitDir = "/state/a\nb" }, "must not contain"},
		{"a relative ssh program", func(o *Options) { o.SSH.Program = sshName }, "SSH.Program"},
		{"a space in the key path", func(o *Options) { o.SSH.IdentityFile = "/keys/my key" }, "SSH.IdentityFile"},
		{"a quote in the known hosts path", func(o *Options) { o.SSH.KnownHostsFile = "/k/'x" }, "SSH.KnownHostsFile"},
		{"a shell character in the CA path", func(o *Options) { o.CABundle = "/ca;rm" }, "CABundle"},
		{"the git dir inside the work tree", func(o *Options) { o.GitDir = "/work/.git" }, "inside WorkTree"},
		{"the git dir at the work tree", func(o *Options) { o.GitDir = fakeWorkDir }, "inside WorkTree"},
		{"the git dir inside the work tree, allowed", func(o *Options) {
			o.GitDir = "/work/.git"
			o.AllowGitDirInWorkTree = true
		}, ""},
		{"a git dir that only starts like the work tree", func(o *Options) { o.GitDir = "/work.git" }, ""},
		{"a negative timeout", func(o *Options) { o.Timeout = -time.Second }, "must not be negative"},
		{"a negative output limit", func(o *Options) { o.MaxOutput = -1 }, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := valid
			tt.change(&o)
			_, err := New(o)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("got %v, want no error", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}

	t.Run("a git dir that is a link into the work tree", func(t *testing.T) {
		root := tempDir(t)
		work := filepath.Join(root, "work")
		if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "gitlink")
		if err := os.Symlink(filepath.Join(work, ".git"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{GitDir: link, WorkTree: work, Git: gitPath}); err == nil || !strings.Contains(err.Error(), "inside WorkTree") {
			t.Errorf("got %v, want it refused", err)
		}
		// And the other way round: a work tree reached through a link.
		workLink := filepath.Join(root, "worklink")
		if err := os.Symlink(work, workLink); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{GitDir: filepath.Join(work, ".git"), WorkTree: workLink, Git: gitPath}); err == nil {
			t.Error("a work tree that is a link to the one with the git dir in it was accepted")
		}
		if _, err := New(Options{GitDir: filepath.Join(root, "elsewhere"), WorkTree: workLink, Git: gitPath}); err != nil {
			t.Errorf("a git dir that is outside, through the same link, was refused: %v", err)
		}
	})

	t.Run("an ssh at a path that cannot go on a command line", func(t *testing.T) {
		dir := filepath.Join(tempDir(t), "my tools")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		_, err := New(Options{GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath})
		if err == nil || !strings.Contains(err.Error(), "set SSH.Program") {
			t.Errorf("got %v, want the setup mistake reported", err)
		}
	})

	t.Run("the defaults", func(t *testing.T) {
		r, err := New(valid)
		if err != nil {
			t.Fatal(err)
		}
		if r.timeout != DefaultTimeout || r.maxOutput != DefaultMaxOutput {
			t.Errorf("the limits are %s and %d, want the defaults", r.timeout, r.maxOutput)
		}
	})
}

// wantSettings is the -c configuration of every command, written out again here
// so that a change to one of the values, or one that is dropped, fails.
var wantSettings = map[string]string{
	"core.hooksPath":            wantNull,
	"core.fsmonitor":            "",
	"core.askPass":              "",
	"credential.helper":         "",
	"core.symlinks":             wantTrue,
	"core.autocrlf":             wantFalse,
	"core.attributesFile":       wantNull,
	"core.excludesFile":         wantNull,
	"protocol.allow":            "never",
	"protocol.ssh.allow":        valueAlways,
	"protocol.https.allow":      valueAlways,
	"http.sslVerify":            wantTrue,
	"http.proxy":                "",
	"http.extraHeader":          "",
	"submodule.recurse":         wantFalse,
	"fetch.recurseSubmodules":   wantFalse,
	"diff.ignoreSubmodules":     "all",
	"commit.gpgSign":            wantFalse,
	"tag.gpgSign":               wantFalse,
	"push.gpgSign":              wantFalse,
	"merge.verifySignatures":    wantFalse,
	"gpg.format":                "openpgp",
	"gpg.program":               wantNull,
	"gpg.openpgp.program":       wantNull,
	"gpg.ssh.program":           wantNull,
	"gpg.x509.program":          wantNull,
	"gpg.ssh.defaultKeyCommand": "",
	"gc.auto":                   "0",
	"maintenance.auto":          wantFalse,
	"safe.directory":            fakeWorkDir,
}

func TestEveryCommandGetsTheFixedConfiguration(t *testing.T) {
	r, err := New(Options{GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath})
	if err != nil {
		t.Fatal(err)
	}
	cmd := r.command(context.Background(), []string{cmdStatus, flagPorcelain})

	got := map[string]string{}
	var i int
	for ; i+1 < len(cmd.Args)-1 && cmd.Args[i+1] == "-c"; i += 2 {
		key, value, _ := strings.Cut(cmd.Args[i+2], "=")
		if _, dup := got[key]; dup {
			t.Errorf("%s is set twice", key)
		}
		got[key] = value
	}
	if !maps.Equal(got, wantSettings) {
		t.Errorf("the settings are\n%v\nwant\n%v", got, wantSettings)
	}
	if rest := cmd.Args[i+1:]; !slices.Equal(rest, []string{cmdStatus, flagPorcelain}) {
		t.Errorf("what follows the settings is %q, want the caller's arguments only", rest)
	}
	for _, s := range fixedSettings {
		if s.why == "" {
			t.Errorf("%s has no reason", s.key)
		}
	}
	if cmd.Dir != fakeWorkDir {
		t.Errorf("the command runs in %s, want the work tree", cmd.Dir)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("the command does not get a process group of its own, so a timeout cannot stop what it starts")
	}
}

func TestTheEnvironmentIsBuiltFromNothing(t *testing.T) {
	// A hostile environment, of the kind a pod can have.
	for _, name := range []string{
		envGitDir, envWorkTree, envSSHCommand, "GIT_SSH", envConfigGlobal, "GIT_CONFIG_COUNT",
		"GIT_CONFIG_PARAMETERS", "GIT_EXEC_PATH", "GIT_TRACE", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_EDITOR",
		"GIT_PAGER", "EDITOR", "VISUAL", "PAGER", "XDG_CONFIG_HOME", "HTTPS_PROXY", "https_proxy",
		"SSL_CERT_FILE", "LD_PRELOAD", "LANG", "TMPDIR", "GIT_TEMPLATE_DIR",
	} {
		t.Setenv(name, "inherited")
	}
	t.Setenv("HOME", "/home/inherited")
	t.Setenv("PATH", "/inherited/bin")

	r, err := New(Options{
		GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath,
		SSH: SSH{Program: "/usr/bin/ssh", IdentityFile: "/keys/id", KnownHostsFile: "/hosts/known"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := r.command(context.Background(), []string{"version"}).Env

	want := map[string]string{
		"PATH":                "/usr/local/bin:/usr/bin:/bin",
		"HOME":                "/nonexistent",
		"LC_ALL":              "C",
		envGitDir:             stateGitDir,
		envWorkTree:           fakeWorkDir,
		envConfigGlobal:       wantNull,
		"GIT_CONFIG_SYSTEM":   wantNull,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_EDITOR":          ":",
		"GIT_PAGER":           "cat",
		envSSHCommand: "/usr/bin/ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes " +
			"-o UpdateHostKeys=no -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile=/hosts/known " +
			"-o IdentitiesOnly=yes -i /keys/id",
		"GIT_ALLOW_PROTOCOL": "ssh:https",
		"GIT_ATTR_SOURCE":    emptyTree,
	}
	// With no bundle given, the system's is named, so that one that a repository
	// config names is not used.
	if bundle := systemCABundle(); bundle != "" {
		want["GIT_SSL_CAINFO"] = bundle
	}
	got := map[string]string{}
	for _, e := range env {
		key, value, _ := strings.Cut(e, "=")
		if _, dup := got[key]; dup {
			t.Errorf("%s is set twice", key)
		}
		got[key] = value
	}
	if !maps.Equal(got, want) {
		t.Errorf("the environment is\n%v\nwant\n%v", got, want)
	}

	t.Run("a CA bundle is passed in its own variable", func(t *testing.T) {
		r, err := New(Options{GitDir: stateGitDir, WorkTree: fakeWorkDir, Git: gitPath, CABundle: "/ca/bundle.pem"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(r.command(context.Background(), nil).Env, "GIT_SSL_CAINFO=/ca/bundle.pem") {
			t.Error("GIT_SSL_CAINFO is not set")
		}
	})
}

func TestSSHCommand(t *testing.T) {
	t.Run("no known hosts file means no host is trusted", func(t *testing.T) {
		got := sshCommand(SSH{Program: "/usr/bin/ssh"})
		if !strings.Contains(got, "UserKnownHostsFile=/dev/null") || !strings.Contains(got, "StrictHostKeyChecking=yes") {
			t.Errorf("a connection could succeed with no known hosts: %s", got)
		}
		if strings.Contains(got, " -i ") {
			t.Errorf("a key is offered although none was given: %s", got)
		}
		if !strings.Contains(got, "-o IdentityFile=/dev/null") {
			t.Errorf("the default identity files are left in use when no key is given: %s", got)
		}
	})

	t.Run("what ssh makes of it", func(t *testing.T) {
		if _, err := exec.LookPath(sshName); err != nil {
			t.Skip("ssh is not installed")
		}
		// ssh refuses to run for a user that has no passwd entry (ADR 0010).
		if out, err := exec.Command(sshName, "-G", "example.invalid").CombinedOutput(); err != nil {
			t.Skipf("ssh does not run as this user: %v: %s", err, out)
		}
		dir := tempDir(t)
		key, known := filepath.Join(dir, "id"), filepath.Join(dir, "known_hosts")
		// ssh leaves out a key that it cannot read.
		if err := os.WriteFile(key, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		command := sshCommand(SSH{Program: sshName, IdentityFile: key, KnownHostsFile: known})

		// -G prints the configuration that ssh would use, and connects to nothing. Git
		// runs the command through a shell, and so does this; the command is made of
		// constants of this test.
		out, err := exec.Command("sh", "-c", command+" -G example.invalid").Output()
		if err != nil {
			t.Fatal(err)
		}
		effective := map[string][]string{}
		for line := range strings.SplitSeq(string(out), "\n") {
			key, value, _ := strings.Cut(line, " ")
			effective[key] = append(effective[key], value)
		}
		for key, want := range map[string]string{
			"stricthostkeychecking": wantTrue,
			"batchmode":             "yes",
			"userknownhostsfile":    known,
			"globalknownhostsfile":  wantNull,
			"identitiesonly":        "yes",
			"identityfile":          key,
			"updatehostkeys":        wantFalse,
		} {
			if !slices.Contains(effective[key], want) {
				t.Errorf("%s is %q, want %q", key, effective[key], want)
			}
		}

		// With no key given, ssh has no identity file of the user's to try.
		out, err = exec.Command("sh", "-c", sshCommand(SSH{Program: "ssh", KnownHostsFile: known})+" -G example.invalid").Output()
		if err != nil {
			t.Fatal(err)
		}
		var identities []string
		for line := range strings.SplitSeq(string(out), "\n") {
			if value, ok := strings.CutPrefix(line, "identityfile "); ok {
				identities = append(identities, value)
			}
		}
		if !slices.Equal(identities, []string{"/dev/null"}) {
			t.Errorf("with no key given, ssh would try %q", identities)
		}
		for _, key := range []string{"proxycommand", "proxyjump", "localcommand", "include"} {
			if v := effective[key]; len(v) > 0 && v[0] != "none" {
				t.Errorf("%s is %q: something other than the command line configured ssh", key, v)
			}
		}
	})
}

func TestOnlyTheSubcommandsOfTheAgentCanBeRun(t *testing.T) {
	wantAllowed := []string{
		cmdAdd, cmdCatFile, cmdCheckout, "checkout-index", cmdCommit, "commit-tree", cmdDiffIndex, "diff-tree",
		cmdFetch, "for-each-ref", "hash-object", "init", "ls-files", cmdLsRemote, "ls-tree", cmdMerge,
		"merge-base", cmdPush, "read-tree", "reset", "rev-list", "rev-parse", "show-ref", cmdStatus,
		"symbolic-ref", "update-index", "update-ref", "version", "write-tree",
	}
	if got := slices.Sorted(maps.Keys(allowed)); !slices.Equal(got, wantAllowed) {
		t.Errorf("the allowed subcommands are\n%v\nwant\n%v", got, wantAllowed)
	}

	root := tempDir(t)
	marker := filepath.Join(root, "ran")
	fake := fakeGit(t, root, fmt.Sprintf("echo ran >> '%s'", marker))
	for _, args := range [][]string{
		nil, {""}, {"submodule", "update"}, {"clone", "x"}, {"diff"}, {"log"}, {"show"}, {"config", "alias.x", "!y"},
		{"pull"}, {"gc"}, {"Status"}, {"treebindtrap"}, {"-c", "core.hooksPath=/x", cmdStatus},
		{"--exec-path=/x", cmdStatus}, {"-C", "/", cmdStatus}, {"status\n"}, {gitPath},
	} {
		_, err := fake.Run(context.Background(), args...)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q: got %v, want it refused", args, err)
		}
	}
	if exists(marker) {
		t.Error("git was started for a subcommand that is not allowed")
	}
	if _, err := fake.Run(context.Background(), cmdStatus); err != nil {
		t.Fatal(err)
	}
	if !exists(marker) {
		t.Error("git was not started for an allowed subcommand")
	}
}

func TestAnAliasCannotTakeTheNameOfABuiltIn(t *testing.T) {
	r := newRepo(t, insideWork)
	script, marker := r.trap("alias")
	for name := range allowed {
		r.config("alias."+name, "!"+script)
	}
	// Git runs a built-in command before it looks for an alias.
	if _, err := r.run(r.runner(), cmdStatus, flagPorcelain); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(r.runner(), "version"); err != nil {
		t.Fatal(err)
	}
	wantNotRan(t, marker, "an alias")
}

func TestRunReturnsWhatGitWrites(t *testing.T) {
	r := newRepo(t, outsideWork)
	result, err := r.run(r.runner(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(result.Stdout), "git version ") {
		t.Errorf("got %q", result.Stdout)
	}
	result, err = r.run(r.runner(), "rev-parse", refHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(result.Stdout))) != 40 {
		t.Errorf("got %q, want a commit id", result.Stdout)
	}
	// Relative paths are the work tree's.
	if _, err := r.run(r.runner(), "ls-files", "--error-unmatch", fileA); err != nil {
		t.Errorf("a path is not relative to the work tree: %v", err)
	}
}

// fakeGit is a Runner whose git is a shell script, for what a real Git cannot be
// made to do on cue: hang, flood, fail in a given way.
func fakeGit(t *testing.T, root, body string, change ...func(*Options)) *Runner {
	t.Helper()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "fakegit")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{GitDir: filepath.Join(root, "gitdir"), WorkTree: work, Git: script, Timeout: time.Minute}
	for _, c := range change {
		c(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A command that starts a child that never ends and then waits for it.
func hangWithAChild(pidFile string) string {
	return fmt.Sprintf("(while :; do :; done) &\necho $! > '%s'\nwait", pidFile)
}

// readPID waits for a script to have written the pid of its child.
func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	eventually(t, "the command to start its child", func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	return pid
}

// eventually polls a condition until it holds, and fails the test when it does not
// within a generous deadline.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// gone reports whether a process no longer runs. A process that has ended and has
// not been reaped yet, because its parent was killed and the one that adopted it is
// busy, is gone.
func gone(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state is the field after the name, which is in parentheses.
	_, after, _ := strings.Cut(string(data), ") ")
	return strings.HasPrefix(after, "Z")
}

func TestATimeoutStopsTheCommandAndWhatItStarted(t *testing.T) {
	root := tempDir(t)
	pidFile := filepath.Join(root, "pid")
	r := fakeGit(t, root, hangWithAChild(pidFile))

	start := time.Now()
	_, err := r.RunTimeout(context.Background(), time.Second, cmdStatus)
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want a timeout, which is not a cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("the call took %s for a timeout of one second", elapsed)
	}
	if want := "git status did not finish within 1s and was stopped"; err.Error() != want {
		t.Errorf("the text is %q, want %q", err, want)
	}
	pid := readPID(t, pidFile)
	eventually(t, "the child of the command to be killed", func() bool { return gone(pid) })
}

func TestCancellingStopsTheCommandAndWhatItStarted(t *testing.T) {
	root := tempDir(t)
	pidFile := filepath.Join(root, "pid")
	r := fakeGit(t, root, hangWithAChild(pidFile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, cmdStatus)
		done <- err
	}()
	pid := readPID(t, pidFile)
	cancel()

	err := <-done
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want a cancellation", err)
	}
	eventually(t, "the child of the command to be killed", func() bool { return gone(pid) })
}

func TestAChildThatLeftTheGroupDoesNotHoldTheCall(t *testing.T) {
	// A child in a group of its own is out of reach of the kill, and it holds the
	// pipes that the command's output goes through. Waiting for those to close would
	// hold the call for as long as the child lives.
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is not installed, and a child cannot leave the process group without it")
	}
	root := tempDir(t)
	pidFile := filepath.Join(root, "pid")
	r := fakeGit(t, root, fmt.Sprintf("%s sh -c 'while :; do :; done' &\necho $! > '%s'\nwait", setsid, pidFile))
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := r.RunTimeout(context.Background(), 200*time.Millisecond, cmdStatus)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("got %v, want a timeout", err)
		}
	case <-time.After(killGrace + 8*time.Second):
		t.Fatal("the call is held by a child that is not in the process group")
	}
	if gone(readPID(t, pidFile)) {
		t.Error("the child was killed with the command, so the test proves nothing")
	}
}

func TestTheTimeoutOfTheRunnerAndTheOneOfACall(t *testing.T) {
	t.Run("the Runner's applies when a call has none", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "while :; do :; done", func(o *Options) { o.Timeout = 100 * time.Millisecond })
		_, err := r.Run(context.Background(), cmdStatus)
		if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrCanceled) || !strings.Contains(err.Error(), "within 100ms") {
			t.Errorf("got %v, want a timeout of 100ms", err)
		}
	})

	t.Run("a call's replaces it, also when it is longer", func(t *testing.T) {
		root := tempDir(t)
		release := filepath.Join(root, "release")
		r := fakeGit(t, root, fmt.Sprintf("while [ ! -e '%s' ]; do :; done", release),
			func(o *Options) { o.Timeout = 50 * time.Millisecond })

		done := make(chan error, 1)
		go func() {
			_, err := r.RunTimeout(context.Background(), time.Minute, cmdStatus)
			done <- err
		}()
		// Four times the Runner's timeout, and the command has not been stopped.
		select {
		case err := <-done:
			t.Fatalf("the command ended: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Errorf("got %v, want the command to finish", err)
		}
	})

	t.Run("the caller's context can end it sooner", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "while :; do :; done")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := r.RunTimeout(ctx, time.Minute, cmdStatus)
		if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want the caller's deadline", err)
		}
	})
}

func TestTheOutputIsBounded(t *testing.T) {
	t.Run("a command that never stops writing is stopped", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "while :; do echo xxxxxxxxxx; done", func(o *Options) {
			o.MaxOutput = 1024
			o.Timeout = 30 * time.Second
		})
		start := time.Now()
		result, err := r.Run(context.Background(), cmdStatus)
		if !errors.Is(err, ErrOutputLimit) || errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want the output limit", err)
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Errorf("it took %s to stop a command that was over the limit, so it ran into its timeout", elapsed)
		}
		if len(result.Stdout) > 1024 {
			t.Errorf("%d bytes were kept, the limit is 1024", len(result.Stdout))
		}
		if want := "git status wrote more than 1024 bytes to its output and was stopped"; err.Error() != want {
			t.Errorf("the text is %q, want %q", err, want)
		}
	})

	t.Run("one that ends with too much is an error, not a short answer", func(t *testing.T) {
		body := "i=0; while [ $i -lt 200 ]; do echo xxxxxxxxxx; i=$((i+1)); done"
		r := fakeGit(t, tempDir(t), body, func(o *Options) { o.MaxOutput = 1024 })
		if _, err := r.Run(context.Background(), cmdStatus); !errors.Is(err, ErrOutputLimit) {
			t.Errorf("got %v, want the output limit", err)
		}
	})

	t.Run("exactly the limit is fine", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "printf '0123456789'", func(o *Options) { o.MaxOutput = 10 })
		result, err := r.Run(context.Background(), cmdStatus)
		if err != nil || string(result.Stdout) != "0123456789" {
			t.Errorf("got %q, %v", result.Stdout, err)
		}
	})
}

func TestAFailureIsReportedInFixedWords(t *testing.T) {
	t.Run("with the exit status, the output, and the end of stderr", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "echo out\necho 'fatal: nothing here' >&2\nexit 3")
		result, err := r.Run(context.Background(), cmdFetch, "--", "https://example.invalid/r.git")

		var gitErr *Error
		var exit *exec.ExitError
		if !errors.As(err, &gitErr) || !errors.As(err, &exit) || gitErr.ExitCode != 3 || exit.ExitCode() != 3 {
			t.Fatalf("got %v, want a failed command with exit status 3", err)
		}
		if want := "git fetch failed with exit status 3: fatal: nothing here"; err.Error() != want {
			t.Errorf("the text is %q, want %q", err, want)
		}
		if string(result.Stdout) != "out\n" || result.Stderr != "fatal: nothing here\n" {
			t.Errorf("got %q and %q", result.Stdout, result.Stderr)
		}
		if strings.Contains(err.Error(), "example.invalid") {
			t.Error("the arguments are in the text of the error")
		}
	})

	t.Run("a command that cannot be started", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "", func(o *Options) { o.Git = "/nonexistent/git" })
		_, err := r.Run(context.Background(), cmdStatus)
		var gitErr *Error
		if !errors.As(err, &gitErr) || !strings.HasPrefix(err.Error(), "git status could not run: ") ||
			!errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrTimeout) {
			t.Errorf("got %v", err)
		}
	})

	t.Run("a subcommand that is refused", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "")
		_, err := r.Run(context.Background(), "log")
		if want := `git: the subcommand "log" is not one the agent may run`; err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
		_, err = r.Run(context.Background(), strings.Repeat("x", 100))
		if err == nil || len(err.Error()) > 100 {
			t.Errorf("a long subcommand is put into the text whole: %v", err)
		}
	})
}

func TestStderrHasNoCredentialAndIsBounded(t *testing.T) {
	body := `i=0
while [ $i -lt 2000 ]; do echo "noise noise noise noise noise noise noise noise" >&2; i=$((i+1)); done
echo "fatal: unable to access 'https://user:s3cret@example.com/r.git/?token=abc123': it broke" >&2
exit 128`
	r := fakeGit(t, tempDir(t), body)

	result, err := r.Run(context.Background(), cmdFetch)
	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("got %v", err)
	}
	for _, text := range []string{err.Error(), gitErr.Stderr, result.Stderr} {
		for _, secret := range []string{"s3cret", "abc123", "user:"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q is in %q", secret, text)
			}
		}
	}
	if !strings.Contains(err.Error(), "https://<redacted>@example.com/r.git/?<redacted>': it broke") {
		t.Errorf("the reason is not in the error: %s", err)
	}
	if len(gitErr.Stderr) > 2048 || len(result.Stderr) > stderrLimit {
		t.Errorf("stderr is %d bytes in the error and %d in the result", len(gitErr.Stderr), len(result.Stderr))
	}
	if strings.Contains(err.Error(), "\n") {
		t.Error("the error has more than one line")
	}
}

func TestRedact(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"nothing to remove", "fatal: repository not found", "fatal: repository not found"},
		{"a password", "https://user:pw@example.com/r.git", "https://<redacted>@example.com/r.git"},
		{"a token as the user", "https://ghp_abc@github.com/o/r", "https://<redacted>@github.com/o/r"},
		{"an at sign in the password", "https://user:p@ss@example.com/r", "https://<redacted>@example.com/r"},
		{"the user of an ssh URL", "ssh://git@example.com/r.git", "ssh://<redacted>@example.com/r.git"},
		{"a query", "https://example.com/r.git?private_token=abc&x=1 failed", "https://example.com/r.git?<redacted> failed"},
		{"in quotes", "unable to access 'https://u:p@h/r/': x", "unable to access 'https://<redacted>@h/r/': x"},
		{"two URLs", "https://a:b@h1/ and https://c:d@h2/", "https://<redacted>@h1/ and https://<redacted>@h2/"},
		{"an at sign in the path is taken for the end of the userinfo", "https://example.com/a@b", "https://<redacted>@b"},
		{"a slash in the password", "fatal: unable to access 'https://user:pa/ss@host/repo'", "fatal: unable to access 'https://<redacted>@host/repo'"},
		{"a question mark in the password", "https://user:p?w@host/r", "https://<redacted>@host/r"},
		{"an scp-style address", "git@example.com:o/r.git", "git@example.com:o/r.git"},
		{"a port", "https://u:p@example.com:8443/r", "https://<redacted>@example.com:8443/r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTailBuffer(t *testing.T) {
	t.Run("keeps the end", func(t *testing.T) {
		b := &tailBuffer{max: 10}
		for _, s := range []string{"first line\n", "second\n", "last\n"} {
			if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
				t.Fatalf("the write was %d, %v", n, err)
			}
		}
		// The ten bytes that are left start inside a line, which is cut off whole.
		if got := b.String(); got != "last\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("does not cut a URL in two", func(t *testing.T) {
		b := &tailBuffer{max: 40}
		_, _ = b.Write([]byte("fatal: see https://user:secret@example.com/r\nnext line\n"))
		if got := Redact(b.String()); strings.Contains(got, "secret") || strings.Contains(got, "user") {
			t.Errorf("part of a credential is left: %q", got)
		}
	})

	t.Run("a short stream is kept whole", func(t *testing.T) {
		b := &tailBuffer{max: 100}
		_, _ = b.Write([]byte("a\nb"))
		if got := b.String(); got != "a\nb" {
			t.Errorf("got %q", got)
		}
	})
}

func TestClassify(t *testing.T) {
	ended := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	exit := &exec.ExitError{}
	tests := []struct {
		name    string
		ctx     context.Context
		over    bool
		err     error
		want    error // nil means that the command succeeded
		notWant error
	}{
		{"success", context.Background(), false, nil, nil, nil},
		{"success that wrote too much", context.Background(), true, nil, ErrOutputLimit, nil},
		{"a failure", context.Background(), false, exit, exit, ErrTimeout},
		{"a timeout", ended(ErrTimeout), false, errors.New("killed"), ErrTimeout, ErrCanceled},
		{"a cancellation", ended(context.Canceled), false, errors.New("killed"), ErrCanceled, ErrTimeout},
		{"the limit, which also ended the context", ended(ErrOutputLimit), true, errors.New("killed"), ErrOutputLimit, ErrCanceled},
		{"a command that finished as the timeout fired", ended(ErrTimeout), false, nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.ctx, cmdStatus, time.Second, &boundedBuffer{over: tt.over}, "", tt.err)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %v, want no error", got)
				}
				return
			}
			if !errors.Is(got, tt.want) || (tt.notWant != nil && errors.Is(got, tt.notWant)) {
				t.Errorf("got %v, want %v and not %v", got, tt.want, tt.notWant)
			}
		})
	}
}

func TestOptionsThatSignOrRunProgramsAreRefused(t *testing.T) {
	root := tempDir(t)
	marker := filepath.Join(root, "ran")
	r := fakeGit(t, root, fmt.Sprintf("echo ran >> '%s'", marker))

	refused := [][]string{
		{cmdCommit, "-S", "-m", "x"},
		{cmdCommit, "-SABCD", "-m", "x"},
		{cmdCommit, "-qS", "-m", "x"},
		{cmdCommit, "-aSm", "x"},
		{cmdCommit, "-m", "-S"},
		{cmdCommit, "--gpg-sign", "-m", "x"},
		{cmdCommit, "--gpg-sign=ABCD", "-m", "x"},
		{cmdCommit, "--gpg", "-m", "x"},
		{cmdCommit, "--trailer", "Key: value", "-m", "x"},
		{cmdCommit, "--trailer=Key: value", "-m", "x"},
		{cmdCommit, "--trail", "Key: value", "-m", "x"},
		{cmdFetch, "--upload-pack=x", "u"},
		{cmdFetch, "--exec=x", "u"},
		{"ls-remote", "--upload-pack", "x", "u"},
		{cmdPush, "--receive-pack=x", "u", "r"},
		{cmdPush, "--exec=x", "u", "r"},
		{cmdDiffIndex, "--output=/tmp/x", "-p", refHead},
		{"commit-tree", "-S", "HEAD^{tree}"},
		{cmdMerge, "-S", branchSide},
		{cmdMerge, "--gpg-sign", branchSide},
		{cmdMerge, "--verify-signatures", branchSide},
		{cmdMerge, "--verify-sig", branchSide},
		{cmdPush, "--signed", "x", "y"},
		{cmdPush, "--signed=if-asked", "x", "y"},
		{cmdDiffIndex, "--ext-diff", "-p", refHead},
		{"diff-tree", "--textconv", "-p", refHead},
		{cmdCatFile, "--textconv", "HEAD:a.txt"},
		{cmdCatFile, "--filters", "HEAD:a.txt"},
	}
	for _, args := range refused {
		_, err := r.Run(context.Background(), args...)
		var gitErr *Error
		if !errors.Is(err, ErrNotAllowed) || !errors.As(err, &gitErr) || gitErr.Option == "" {
			t.Errorf("%q: got %v, want the option refused", args, err)
		}
	}
	_, err := r.Run(context.Background(), cmdCommit, "-S")
	if want := `git: the option "-S" is not one the agent may pass to commit`; err == nil || err.Error() != want {
		t.Errorf("the text is %v, want %q", err, want)
	}
	if exists(marker) {
		t.Error("git was started for a command with an option that is refused")
	}

	allowed := [][]string{
		{cmdCommit, "--no-gpg-sign", "-m", "x"},
		{cmdCommit, "-q", "--message=-Sync", flagAllowEmpty},
		{cmdCommit, "-mSync", flagAllowEmpty},
		{cmdCommit, "-qmSync", flagAllowEmpty},
		{cmdCommit, "-FSomeFile", flagAllowEmpty},
		{"ls-remote", "--exit-code", "u"},
		{cmdFetch, "--unshallow", "u"},
		{cmdCommit, "-m", "x", "--", "-S"},
		{cmdMerge, "--no-verify-signatures", branchSide},
		{"rev-parse", "--verify", refHead},
		{cmdStatus, "-s"},
		{cmdPush, "--set-upstream", "x", "y"},
		{cmdDiffIndex, "--no-ext-diff", "-p", refHead},
		{cmdCatFile, "-p", refHead},
	}
	for _, args := range allowed {
		if _, err := r.Run(context.Background(), args...); err != nil {
			t.Errorf("%q: got %v, want it run", args, err)
		}
	}
}

func TestACommandThatLeavesAChildBehindStillSucceeds(t *testing.T) {
	// A child that outlives Git and holds its output, which a detached gc does,
	// delays the call, but Git itself succeeded.
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is not installed, and a child cannot leave the process group without it")
	}
	root := tempDir(t)
	pidFile := filepath.Join(root, "pid")
	r := fakeGit(t, root, fmt.Sprintf("%s sh -c 'while :; do :; done' &\necho $! > '%s'\necho done", setsid, pidFile))
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	result, err := r.Run(context.Background(), cmdStatus)
	if err != nil || string(result.Stdout) != "done\n" {
		t.Errorf("got %q and %v, want the output and no error", result.Stdout, err)
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		name, output, want string
	}{
		{"the oldest that works", "git version 2.40.0", ""},
		{"a newer one", "git version 2.56.1", ""},
		{"a newer major", "git version 3.0.0", ""},
		{"a vendor suffix", "git version 2.44.0.windows.1", ""},
		{"one before", "git version 2.39.5", msgOlder},
		{"a much older one", "git version 2.34.1", msgOlder},
		{"an older major", "git version 1.9.0", msgOlder},
		{"something else", "not git", "cannot read a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeGit(t, tempDir(t), "echo '"+tt.output+"'")
			err := r.CheckVersion(context.Background())
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("got %v, want no error", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}

	t.Run("a Git that cannot run", func(t *testing.T) {
		r := fakeGit(t, tempDir(t), "exit 1")
		if err := r.CheckVersion(context.Background()); err == nil {
			t.Error("got no error")
		}
	})
}
