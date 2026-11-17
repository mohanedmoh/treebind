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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Names that the tests of the Git directory use again and again.
const (
	configFile     = "config"
	alternatesName = "alternates"
	dirInfo        = "info"
	dirHooks       = "hooks"
	indexLock      = "index.lock"
	// What the errors say, in the words that tell one problem from another.
	problemHeader  = "section header that the agent does not write"
	problemSetting = "not a setting"
	problemLink    = "is a link"
	problemEntry   = "not a file that the agent makes"
)

// agentRepo makes a repository the way the agent does: init with an empty template, the Git
// directory beside the work tree, and a commit. What it holds is what VerifyGitDir accepts.
func agentRepo(t *testing.T) *repo {
	t.Helper()
	root := tempDir(t)
	r := &repo{t: t, root: root, work: filepath.Join(root, "work"), home: filepath.Join(root, "home"), git: gitBinary(t)}
	r.gitDir = filepath.Join(root, "gitdir")
	for _, dir := range []string{r.work, r.home, filepath.Join(root, "scripts")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.plain("init", "-q", "--template=")
	r.plain("symbolic-ref", refHead, "refs/heads/main")
	r.write(fileA, "a\n")
	r.plain(cmdAdd, fileA)
	r.plain("-c", "user.name=T", "-c", "user.email=t@example.com", cmdCommit, "-q", "-m", "first")
	return r
}

// appendConfig adds text to the config of the repository, as an application that could write
// it would.
func appendConfig(t *testing.T, r *repo, text string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(r.gitDir, configFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func writeInGitDir(t *testing.T, r *repo, rel, content string) {
	t.Helper()
	path := filepath.Join(r.gitDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func replaceConfig(t *testing.T, r *repo, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.gitDir, configFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyGitDirAcceptsWhatTheAgentMakes(t *testing.T) {
	r := agentRepo(t)

	if err := VerifyGitDir(r.gitDir, r.work); err != nil {
		t.Fatalf("the Git directory that the agent makes is refused: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(r.gitDir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the config of a repository the agent made:\n%s", config)
}

func TestVerifyGitDirAcceptsWhatGitWritesWhileItWorks(t *testing.T) {
	r := agentRepo(t)
	// Files that the subcommands the Runner allows leave at the top of the Git directory.
	r.plain("fetch", "-q", r.gitDir)
	r.plain("update-ref", "refs/remotes/origin/main", "HEAD")
	r.plain("read-tree", "HEAD")
	r.write("b.txt", "b\n")
	r.plain(cmdAdd, "b.txt")
	r.plain("-c", "user.name=T", "-c", "user.email=t@example.com", cmdCommit, "-q", "-m", "second")
	r.plain("checkout", "-q", "-b", "other", "HEAD~1")
	for _, name := range []string{indexLock, lockHead, lockConfig} {
		writeInGitDir(t, r, name, "")
	}

	if err := VerifyGitDir(r.gitDir, r.work); err != nil {
		t.Fatalf("a Git directory that Git has worked in is refused: %v", err)
	}
	for _, name := range []string{"COMMIT_EDITMSG", "index", "logs"} {
		if !exists(filepath.Join(r.gitDir, name)) {
			t.Errorf("%s is not there, so the test does not show that it is accepted", name)
		}
	}
}

func TestVerifyGitDirAcceptsWhatTheConfigOfARepositoryMayHold(t *testing.T) {
	r := agentRepo(t)
	tests := []struct{ name, config string }{
		{"comments, blank lines and any case", "# a comment\n\n[Core]\n\t; another\n\tFileMode = true\n\tBARE = false\n"},
		{"what init finds out about the volume", "[core]\n\tsymlinks = false\n\tignorecase = true\n\tprecomposeunicode = true\n"},
		{"a version 1 repository with SHA-256", "[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha256\n"},
		{"the work tree in quotes", "[core]\n\tworktree = \"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(r.work) + "\"\n"},
		{"the carriage returns of a file that went through Windows", "[core]\r\n\tfilemode = true\r\n"},
		{"nothing at all", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replaceConfig(t, r, tt.config)

			if err := VerifyGitDir(r.gitDir, r.work); err != nil {
				t.Errorf("a config the agent could have written is refused: %v", err)
			}
		})
	}
}

func TestVerifyGitDirAcceptsAGitDirectoryThatIsNotThereOrHasNoConfigYet(t *testing.T) {
	t.Run("not there", func(t *testing.T) {
		if err := VerifyGitDir(filepath.Join(tempDir(t), "missing"), "/work"); err != nil {
			t.Error(err)
		}
	})
	t.Run("empty, before init", func(t *testing.T) {
		if err := VerifyGitDir(tempDir(t), "/work"); err != nil {
			t.Error(err)
		}
	})
}

func TestVerifyGitDirRefusesWhatTheAgentDoesNotWrite(t *testing.T) {
	tests := []struct {
		name    string
		plant   func(t *testing.T, r *repo)
		path    string // the part of the path that the error names
		problem string
	}{
		// The four things of ADR 0013 that the Runner cannot neutralise from the command line.
		{"a rewrite of a URL to another host", func(t *testing.T, r *repo) {
			appendConfig(t, r, "[url \"ssh://evil.example/\"]\n\tinsteadOf = ssh://git@github.com/\n")
		}, configFile, problemHeader},
		{"a key scoped to a URL", func(t *testing.T, r *repo) {
			appendConfig(t, r, "[http \"https://github.com/\"]\n\tsslVerify = false\n")
		}, configFile, problemHeader},
		{"a CA directory", func(t *testing.T, r *repo) {
			appendConfig(t, r, "[http]\n\tsslCAPath = /tmp/ca\n")
		}, configFile, "starts the section [http]"},
		{"info/attributes", func(t *testing.T, r *repo) { writeInGitDir(t, r, "info/attributes", "* filter=x\n") }, dirInfo, "holds attributes"},
		// Other ways to run a program, or to read configuration from somewhere else.
		{"an ssh command", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tsshCommand = /tmp/evil\n") }, configFile, "core.sshcommand"},
		{"an fsmonitor", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tfsmonitor = /tmp/evil\n") }, configFile, "core.fsmonitor"},
		{"a hooks path", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\thooksPath = /tmp/hooks\n") }, configFile, "core.hookspath"},
		{"a filter", func(t *testing.T, r *repo) { appendConfig(t, r, "[filter \"x\"]\n\tclean = /tmp/evil\n") }, configFile, problemHeader},
		{"an alias", func(t *testing.T, r *repo) { appendConfig(t, r, "[alias]\n\tstatus = !/tmp/evil\n") }, configFile, "starts the section [alias]"},
		{"an include", func(t *testing.T, r *repo) { appendConfig(t, r, "[include]\n\tpath = /tmp/more\n") }, configFile, "starts the section [include]"},
		{"a conditional include", func(t *testing.T, r *repo) {
			appendConfig(t, r, "[includeIf \"gitdir:/\"]\n\tpath = /tmp/more\n")
		}, configFile, problemHeader},
		{"a remote", func(t *testing.T, r *repo) { appendConfig(t, r, "[remote \"origin\"]\n\turl = ssh://evil/x\n") }, configFile, "section header"},
		{"a credential helper", func(t *testing.T, r *repo) { appendConfig(t, r, "[credential]\n\thelper = !/tmp/evil\n") }, configFile, "starts the section [credential]"},
		{"a proxy", func(t *testing.T, r *repo) { appendConfig(t, r, "[http]\n\tproxy = http://evil:3128\n") }, configFile, "starts the section [http]"},
		{"a core key that is not on the list", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\teditor = /tmp/evil\n") }, configFile, "core.editor"},
		{"a worktree config", func(t *testing.T, r *repo) {
			appendConfig(t, r, "[extensions]\n\tworktreeConfig = true\n")
		}, configFile, "extensions.worktreeconfig"},
		{"a work tree that is another directory", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tworktree = /tmp/elsewhere\n") }, configFile, "core.worktree"},
		{"a bare repository", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tbare = true\n") }, configFile, "core.bare"},
		{"an object format that is not one", func(t *testing.T, r *repo) { appendConfig(t, r, "[extensions]\n\tobjectformat = md5\n") }, configFile, "extensions.objectformat"},
		{"a boolean that is not a boolean", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tfilemode = !/tmp/evil\n") }, configFile, "core.filemode"},
		{"a continued line", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tfilemode = true \\\n\t\tmore\n") }, configFile, problemSetting},
		{"a setting with no value", func(t *testing.T, r *repo) { appendConfig(t, r, "[core]\n\tfsmonitor\n") }, configFile, problemSetting},
		{"a setting before any section", func(t *testing.T, r *repo) { replaceConfig(t, r, "filemode = true\n") }, configFile, problemSetting},
		{"a header with something after it", func(t *testing.T, r *repo) { appendConfig(t, r, "[core] fsmonitor = /tmp/evil\n") }, configFile, "section header"},
		{"a config that is too large", func(t *testing.T, r *repo) {
			appendConfig(t, r, strings.Repeat("# padding\n", maxConfigSize/10+1))
		}, configFile, "larger than"},
		{"a config that is a link", func(t *testing.T, r *repo) {
			target := filepath.Join(r.root, "elsewhere-config")
			if err := os.Rename(filepath.Join(r.gitDir, configFile), target); err != nil {
				t.Fatal(err)
			}
			mustLink(t, target, filepath.Join(r.gitDir, configFile))
		}, configFile, problemLink},
		// What is in the top of the Git directory.
		{"a hook", func(t *testing.T, r *repo) { writeInGitDir(t, r, "hooks/pre-commit", "#!/bin/sh\n") }, dirHooks, "holds pre-commit"},
		{"a file of ignore patterns", func(t *testing.T, r *repo) { writeInGitDir(t, r, "info/exclude", "*\n") }, dirInfo, "holds exclude"},
		{"grafts", func(t *testing.T, r *repo) { writeInGitDir(t, r, "info/grafts", "") }, dirInfo, "holds grafts"},
		{"alternates", func(t *testing.T, r *repo) { writeInGitDir(t, r, "objects/info/"+alternatesName, "/tmp/objects\n") }, alternatesName, "another directory"},
		{"http alternates", func(t *testing.T, r *repo) { writeInGitDir(t, r, "objects/info/http-alternates", "http://evil/\n") }, "http-alternates", "another directory"},
		{"a common directory", func(t *testing.T, r *repo) { writeInGitDir(t, r, "commondir", "/tmp/elsewhere\n") }, "commondir", problemEntry},
		{"a gitdir file", func(t *testing.T, r *repo) { writeInGitDir(t, r, "gitdir", "/tmp/elsewhere\n") }, "gitdir", problemEntry},
		{"a shallow file", func(t *testing.T, r *repo) { writeInGitDir(t, r, "shallow", "") }, "shallow", problemEntry},
		{"packed refs", func(t *testing.T, r *repo) { writeInGitDir(t, r, "packed-refs", "") }, "packed-refs", problemEntry},
		{"a config for the work tree", func(t *testing.T, r *repo) { writeInGitDir(t, r, "config.worktree", "") }, "config.worktree", problemEntry},
		{"linked work trees", func(t *testing.T, r *repo) { writeInGitDir(t, r, "worktrees/x/gitdir", "") }, "worktrees", problemEntry},
		{"submodules", func(t *testing.T, r *repo) { writeInGitDir(t, r, "modules/x/config", "") }, "modules", problemEntry},
		{"a template", func(t *testing.T, r *repo) { writeInGitDir(t, r, "description", "x\n") }, "description", problemEntry},
		{"any other file", func(t *testing.T, r *repo) { writeInGitDir(t, r, "notes.txt", "x\n") }, "notes.txt", problemEntry},
		{"HEAD as a link", func(t *testing.T, r *repo) {
			if err := os.Remove(filepath.Join(r.gitDir, refHead)); err != nil {
				t.Fatal(err)
			}
			mustLink(t, "refs/heads/main", filepath.Join(r.gitDir, refHead))
		}, refHead, problemLink},
		{"objects as a link", func(t *testing.T, r *repo) {
			target := filepath.Join(r.root, "elsewhere-objects")
			if err := os.Rename(filepath.Join(r.gitDir, "objects"), target); err != nil {
				t.Fatal(err)
			}
			mustLink(t, target, filepath.Join(r.gitDir, "objects"))
		}, "objects", problemLink},
		{"refs that are a file", func(t *testing.T, r *repo) {
			if err := os.RemoveAll(filepath.Join(r.gitDir, "refs")); err != nil {
				t.Fatal(err)
			}
			writeInGitDir(t, r, "refs", "x")
		}, "refs", "is not a directory"},
		{"config that is a directory", func(t *testing.T, r *repo) {
			if err := os.Remove(filepath.Join(r.gitDir, configFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(r.gitDir, configFile), 0o755); err != nil {
				t.Fatal(err)
			}
		}, configFile, "is not a file"},
		{"a lock that is a directory", func(t *testing.T, r *repo) {
			if err := os.Mkdir(filepath.Join(r.gitDir, indexLock), 0o755); err != nil {
				t.Fatal(err)
			}
		}, indexLock, "is not a file"},
		{"hooks that are a file", func(t *testing.T, r *repo) { writeInGitDir(t, r, "hooks", "x") }, dirHooks, "is not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := agentRepo(t)
			tt.plant(t, r)

			err := VerifyGitDir(r.gitDir, r.work)

			var untrusted *GitDirError
			if !errors.As(err, &untrusted) || !errors.Is(err, ErrGitDirUntrusted) {
				t.Fatalf("error = %v (%T), want a *GitDirError that is ErrGitDirUntrusted", err, err)
			}
			if !strings.Contains(untrusted.Path, tt.path) || !strings.Contains(untrusted.Error(), tt.problem) {
				t.Errorf("error = %q, want it to name %q and say %q", untrusted, tt.path, tt.problem)
			}
		})
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyGitDirNamesTheLineOfTheConfig(t *testing.T) {
	r := agentRepo(t)
	appendConfig(t, r, "[core]\n\tfsmonitor = /tmp/evil\n")

	err := VerifyGitDir(r.gitDir, r.work)

	var untrusted *GitDirError
	if !errors.As(err, &untrusted) || !strings.Contains(untrusted.Problem, fmt.Sprintf("line %d ", strings.Count(readConfig(t, r), "\n"))) {
		t.Errorf("error = %v, want the number of the line that sets core.fsmonitor", err)
	}
}

func readConfig(t *testing.T, r *repo) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.gitDir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestVerifyGitDirSaysWhenItCannotLook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory that is closed to it")
	}
	r := agentRepo(t)
	if err := os.Chmod(r.gitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.gitDir, 0o700) })

	err := VerifyGitDir(r.gitDir, r.work)

	var untrusted *GitDirError
	if err == nil || errors.As(err, &untrusted) {
		t.Errorf("error = %v, want a plain error: the agent is shut out of the directory, which is not tampering", err)
	}
}

// The Runner looks before it starts Git, and a Git directory that holds what the Runner cannot
// neutralise stops being a way to run a program.
func TestTheRunnerWithVerifyRefusesAGitDirectoryWithAFilterAndStartsNothing(t *testing.T) {
	// The control: with no check, info/attributes selects a filter, and the filter runs on add.
	// That is the gap of ADR 0013 that the check closes.
	arm := func(t *testing.T, r *repo) string {
		marker := plantFilter(r, "clean")
		writeInGitDir(t, r, "info/attributes", "* filter=x\n")
		r.write("new.txt", "new\n")
		return marker
	}
	t.Run("without the check, the filter runs", func(t *testing.T) {
		r := agentRepo(t)
		marker := arm(t, r)

		if _, err := r.run(r.runner(), cmdAdd, "new.txt"); err != nil {
			t.Fatal(err)
		}

		wantRan(t, marker, "the filter that info/attributes selects")
	})
	t.Run("with the check, Git is not started", func(t *testing.T) {
		r := agentRepo(t)
		marker := arm(t, r)

		_, err := r.run(r.runner(func(o *Options) { o.VerifyGitDir = true }), cmdAdd, "new.txt")

		if !errors.Is(err, ErrGitDirUntrusted) {
			t.Fatalf("error = %v, want ErrGitDirUntrusted", err)
		}
		wantNotRan(t, marker, "the filter that info/attributes selects")
	})
}

func TestTheRunnerWithVerifyRefusesEveryCommandAndSaysWhich(t *testing.T) {
	r := agentRepo(t)
	appendConfig(t, r, "[core]\n\tsshCommand = /tmp/evil\n")
	runner := r.runner(func(o *Options) { o.VerifyGitDir = true })

	for _, sub := range []string{cmdStatus, cmdVersion, cmdRevParse} {
		_, err := runner.Run(t.Context(), sub)

		var gitErr *Error
		if !errors.As(err, &gitErr) || gitErr.Subcommand != sub || !errors.Is(err, ErrGitDirUntrusted) {
			t.Fatalf("%s: error = %v, want an *Error of the subcommand that is ErrGitDirUntrusted", sub, err)
		}
		if want := "git " + sub + " was not run: "; !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "core.sshcommand") {
			t.Errorf("%s: text = %q, want it to start with %q and name the key", sub, err.Error(), want)
		}
	}
}

func TestTheRunnerLooksAgainBeforeEveryCommand(t *testing.T) {
	r := agentRepo(t)
	runner := r.runner(func(o *Options) { o.VerifyGitDir = true })
	if _, err := runner.Run(t.Context(), "status"); err != nil {
		t.Fatalf("a Git directory that is what the agent makes was refused: %v", err)
	}
	appendConfig(t, r, "[core]\n\tfsmonitor = /tmp/evil\n")

	if _, err := runner.Run(t.Context(), "status"); !errors.Is(err, ErrGitDirUntrusted) {
		t.Errorf("error = %v, want a change after the first command to be seen by the second", err)
	}
}

func TestTheRunnerDoesNotLookUnlessItIsAsked(t *testing.T) {
	r := agentRepo(t)
	writeInGitDir(t, r, "notes.txt", "x\n")

	if _, err := r.run(r.runner(), "status"); err != nil {
		t.Errorf("a Runner that was not asked to look refused: %v", err)
	}
}

func TestReplaceRefsAreNotUsed(t *testing.T) {
	r := agentRepo(t)
	blob := strings.TrimSpace(r.plain("rev-parse", "HEAD:"+fileA))
	r.write("replacement.txt", "replaced\n")
	other := strings.TrimSpace(r.plain("hash-object", "-w", "replacement.txt"))
	r.plain("replace", blob, other)

	if got := r.plain("cat-file", "-p", blob); got != "replaced\n" {
		t.Fatalf("the control: plain Git reads %q, want the replacement, or the test shows nothing", got)
	}
	result, err := r.run(r.runner(), "cat-file", "-p", blob)

	if err != nil || string(result.Stdout) != "a\n" {
		t.Errorf("the Runner reads %q, %v, want the object itself: a replace ref changes what Git sees", result.Stdout, err)
	}
}

func TestVerifyGitDirAcceptsTheLockOfAFileTheAgentWritesAndNoOtherName(t *testing.T) {
	// packed-refs.lock is the lock of a file that the agent does not make, and is accepted: a command that
	// removes a ref locks it whether or not there are packed refs, and a command that is killed leaves it
	// (docs/adr/0028). The file itself is refused, as the tests above say.
	tests := []struct {
		name   string
		accept bool
	}{
		{indexLock, true}, {"HEAD.lock", true}, {"config.lock", true}, {"ORIG_HEAD.lock", true}, {"COMMIT_EDITMSG.lock", true},
		{"packed-refs.lock", true},
		{"evil.lock", false}, {"commondir.lock", false}, {"shallow.lock", false},
		{"hooks.lock", false}, {"info.lock", false}, {"objects.lock", false}, {".lock", false}, {"lock", false},
		{"index.lockx", false}, {"index.lock.lock", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := agentRepo(t)
			writeInGitDir(t, r, tt.name, "")

			err := VerifyGitDir(r.gitDir, r.work)

			if tt.accept && err != nil {
				t.Errorf("the lock of a file the agent writes is refused: %v", err)
			}
			if !tt.accept && !errors.Is(err, ErrGitDirUntrusted) {
				t.Errorf("error = %v, want %s refused: it is not the lock of a file the agent writes", err, tt.name)
			}
		})
	}
}

func TestVerifyGitDirRefusesAFormatVersionThatIsNotOneOfGits(t *testing.T) {
	for _, version := range []string{"2", "abc", "", "00", "-1"} {
		t.Run("version "+version, func(t *testing.T) {
			r := agentRepo(t)
			replaceConfig(t, r, "[core]\n\trepositoryformatversion = "+version+"\n")

			if err := VerifyGitDir(r.gitDir, r.work); !errors.Is(err, ErrGitDirUntrusted) {
				t.Errorf("error = %v, want a format version of %q refused", err, version)
			}
		})
	}
}

func TestVerifyGitDirTakesAWorkTreeWithAQuoteAndABackslashInIt(t *testing.T) {
	// Git writes such a path in quotes, with an escape for each.
	r := agentRepo(t)
	workTree := `/data/a\b"c`
	replaceConfig(t, r, "[core]\n\tworktree = \"/data/a\\\\b\\\"c\"\n")

	if err := VerifyGitDir(r.gitDir, workTree); err != nil {
		t.Errorf("the work tree as Git writes it is refused: %v", err)
	}
	if err := VerifyGitDir(r.gitDir, "/data/ab\"c"); !errors.Is(err, ErrGitDirUntrusted) {
		t.Errorf("error = %v, want another work tree refused: the backslash is part of the path", err)
	}
}

func TestVerifyGitDirTakesTheRealPathOfAWorkTreeThatHasALinkInIt(t *testing.T) {
	// Git writes core.worktree as the real path. A mount path that crosses a link is the work
	// tree the agent is given, and the repository it made is not refused for it.
	r := agentRepo(t)
	link := filepath.Join(r.root, "link")
	mustLink(t, r.work, link)
	replaceConfig(t, r, "[core]\n\tworktree = "+r.work+"\n")

	if err := VerifyGitDir(r.gitDir, link); err != nil {
		t.Errorf("the work tree through a link is refused: %v", err)
	}
	if err := VerifyGitDir(r.gitDir, filepath.Join(r.root, "other")); !errors.Is(err, ErrGitDirUntrusted) {
		t.Errorf("error = %v, want another work tree refused", err)
	}
}

func TestVerifyGitDirIsNotFooledByAnEntryThatAnotherCommandRemoves(t *testing.T) {
	// The Runner is safe for concurrent use, and a lock is made and removed while a command
	// runs: it can be in the listing and gone by the time it is looked at.
	r := agentRepo(t)
	root, err := os.OpenRoot(r.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, name := range []string{indexLock, dirHooks, "objects"} {
		if err := os.RemoveAll(filepath.Join(r.gitDir, name)); err != nil {
			t.Fatal(err)
		}
		if err := checkEntry(root, r.gitDir, name); err != nil {
			t.Errorf("%s: an entry that is gone is refused: %v", name, err)
		}
	}
}

func TestVerifyGitDirAcceptsWhatAMergeLeavesBehind(t *testing.T) {
	r := agentRepo(t)
	for _, name := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "AUTO_MERGE", "SQUASH_MSG", "ORIG_HEAD"} {
		writeInGitDir(t, r, name, "x\n")
	}

	if err := VerifyGitDir(r.gitDir, r.work); err != nil {
		t.Errorf("what a merge leaves in the Git directory is refused: %v", err)
	}
}
