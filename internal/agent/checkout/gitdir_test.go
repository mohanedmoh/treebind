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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mohanedmoh/treebind/internal/agent/git"
	"github.com/mohanedmoh/treebind/internal/agent/layout"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

// Names that the tests of the Git directory use again and again.
const (
	gitConfig = "config"
	gitInfo   = "info"
	gitHooks  = "hooks"
	gitHead   = "HEAD"
	// planted is a file that the agent does not make, put in the Git directory while the
	// checkout runs.
	planted = "planted"
	// notRun is what the message of a refusal says about Git.
	notRun = "Git was not run against it"
)

// The two names are one reason: the preflight check and the checkout refuse a Git directory
// for the same cause, and a user or a tool matches on it (docs/adr/0009).
func TestTheReasonOfAnUnsafeGitDirectoryIsTheOneOfThePreflightCheck(t *testing.T) {
	if ReasonGitDirUnsafe != preflight.ReasonGitDirUnsafe {
		t.Errorf("the checkout says %q and the preflight check %q for a Git directory that is not safe", ReasonGitDirUnsafe,
			preflight.ReasonGitDirUnsafe)
	}
}

// A stage is how far the checkout on the claim got before the agent starts.
type stage string

const (
	stageNone     stage = "no checkout yet"
	stageFinished stage = "a finished checkout"
	stageHalfWay  stage = "a checkout that stopped half way"
)

var stages = []stage{stageNone, stageFinished, stageHalfWay}

// reach brings the claim to the stage, with the protections on.
func reach(t *testing.T, f *fixture, l layout.Layout, s stage) {
	t.Helper()
	switch s {
	case stageNone:
		if err := l.MakeWorkTree(); err != nil {
			t.Fatal(err)
		}
	case stageFinished:
		if got, err := open(t, f, l, branchMain); err != nil || got.Outcome != Cloned {
			t.Fatalf("result = %+v, %v, want Cloned", got, err)
		}
	case stageHalfWay:
		beforeCheckout = func() error { return errors.New("the pod was killed") }
		t.Cleanup(func() { beforeCheckout = nil })
		if _, err := open(t, f, l, branchMain); err == nil {
			t.Fatal("the simulated crash did not stop the checkout")
		}
		beforeCheckout = nil
		if !exists(filepath.Join(l.StateDir, markerName)) {
			t.Fatal("the checkout that stopped half way left no marker")
		}
	}
}

// openWithoutTheCheck is Open with a Runner that does not look at the Git directory: the
// control that shows what a trap in it would do.
func openWithoutTheCheck(t *testing.T, f *fixture, l layout.Layout, branch string) (Result, error) {
	t.Helper()
	o := f.options(l, branch)
	options := o.runnerOptions()
	options.VerifyGitDir = false
	runner, err := git.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return (&checkout{Options: o, git: runner}).open(context.Background())
}

// watchGit makes the options start Git through a script that notes each start in a log,
// and returns the log. With plantAfter above zero the script puts a file that the agent
// does not make in the Git directory once that many commands have run, as an application
// that writes there while the checkout runs would. It uses only what the shell has built in,
// so that it runs in the image of the agent too.
func (f *fixture) watchGit(o *Options, plantAfter int) (log string) {
	f.t.Helper()
	log = filepath.Join(f.root, "git-starts.log")
	script := fmt.Sprintf(`#!/bin/sh
echo started >> '%s'
'%s' "$@"
status=$?
n=0
while read -r line; do n=$((n + 1)); done < '%s'
if [ "$n" -eq %d ]; then : > '%s'; fi
exit $status
`, log, f.git, log, plantAfter, filepath.Join(o.Layout.GitDir, planted))
	program := filepath.Join(f.root, "fake", "git")
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	o.Git.Git = program
	return log
}

// starts returns how many times Git was started, by the log of watchGit.
func starts(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// trapProgram writes a program that notes in a marker that it ran and passes on what it
// reads, as a filter does, and returns the program and the marker.
func (f *fixture) trapProgram(name string) (program, marker string) {
	f.t.Helper()
	marker = filepath.Join(f.root, "marker-"+name)
	program = filepath.Join(f.root, "fake", name)
	script := fmt.Sprintf("#!/bin/sh\necho ran >> '%s'\nwhile IFS= read -r line; do printf '%%s\\n' \"$line\"; done\n", marker)
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return program, marker
}

// inGitDir writes a file in the Git directory, as an application that can write there would.
func inGitDir(t *testing.T, l layout.Layout, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(l.GitDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// addToConfig adds text to the config of the Git directory, and makes one if there is none.
func addToConfig(t *testing.T, l layout.Layout, text string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(l.GitDir, gitConfig), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// plantFilter makes every file go through a filter that notes in a marker that it ran: the
// attributes in info select it, and the config defines it. The Runner cannot neutralise
// info/attributes (docs/adr/0013). It returns the marker.
func plantFilter(t *testing.T, f *fixture, l layout.Layout) (marker string) {
	t.Helper()
	program, marker := f.trapProgram("filter")
	addToConfig(t, l, fmt.Sprintf("[filter \"x\"]\n\tclean = %s\n\tsmudge = %s\n", program, program))
	inGitDir(t, l, filepath.Join(gitInfo, "attributes"), "* filter=x\n", 0o600)
	return marker
}

// A trap is something in the Git directory that makes Git run a program, or read
// configuration or objects from another place.
type trap struct {
	name string
	// path is the file in the Git directory that the refusal names.
	path string
	// plant puts the trap in the Git directory. It returns a function that reports whether it
	// sprang, or nil when what it does is not seen from outside Git.
	plant func(t *testing.T, f *fixture, l layout.Layout) (sprang func() bool)
	// springsAt are the stages at which a command of the checkout springs it when the Git
	// directory is not looked at. It is empty for a trap that the Runner neutralises on its
	// own, which the look stops all the same, and for one that nothing here shows.
	springsAt []stage
}

var traps = []trap{
	{"a filter that info/attributes selects", gitInfo, func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		marker := plantFilter(t, f, l)
		return func() bool { return exists(marker) }
	}, []stage{stageNone, stageHalfWay}},
	{"a hook", gitHooks, func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		marker := filepath.Join(f.root, "marker-hook")
		inGitDir(t, l, filepath.Join(gitHooks, "post-checkout"), "#!/bin/sh\necho ran >> '"+marker+"'\n", 0o700)
		return func() bool { return exists(marker) }
	}, nil},
	{"a rewrite of the URL to another repository", gitConfig, func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		other := newFixture(t)
		theirs := other.publish(branchMain, map[string]string{"theirs.txt": "theirs\n"})
		addToConfig(t, l, fmt.Sprintf("[url \"%s\"]\n\tinsteadOf = %s\n", other.url, f.url))
		return func() bool {
			fetched, err := f.plainErr(l.WorkTree, []string{"GIT_DIR=" + l.GitDir}, "rev-parse", "--verify", "--quiet",
				remotePrefix+branchMain)
			return err == nil && strings.TrimSpace(fetched) == theirs
		}
	}, []stage{stageNone, stageFinished}},
	{"an ssh command", gitConfig, func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		program, marker := f.trapProgram("ssh-command")
		addToConfig(t, l, "[core]\n\tsshCommand = "+program+"\n")
		return func() bool { return exists(marker) }
	}, nil},
	{"a common directory", "commondir", func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		inGitDir(t, l, "commondir", filepath.Join(f.root, "elsewhere")+"\n", 0o600)
		return nil
	}, nil},
	{"alternates", filepath.Join("objects", gitInfo, "alternates"), func(t *testing.T, f *fixture, l layout.Layout) func() bool {
		inGitDir(t, l, filepath.Join("objects", gitInfo, "alternates"), filepath.Join(f.root, "elsewhere", "objects")+"\n", 0o600)
		return nil
	}, nil},
}

// snapshot returns what is under a directory, by path: the content of each file, the target
// of each link and a mark for each directory. It does not follow links.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			got[rel] = "a link to " + target
			return err
		case entry.IsDir():
			got[rel] = "a directory"
			return nil
		default:
			data, err := os.ReadFile(path)
			got[rel] = string(data)
			return err
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// wantUnchanged fails the test unless the snapshots are the same, and names what differs.
func wantUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, was := range before {
		if is, ok := after[path]; !ok || is != was {
			t.Errorf("%s was changed or removed", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was made", path)
		}
	}
}

// wantUnsafe fails the test unless err is the refusal of a Git directory that names the file,
// and says that Git was not run and how to put it right.
func wantUnsafe(t *testing.T, err error, l layout.Layout, rel string) {
	t.Helper()
	got := errorOf(t, err, ReasonGitDirUnsafe)
	if !errors.Is(err, git.ErrGitDirUntrusted) {
		t.Errorf("error = %v, want the refusal of the Git directory underneath", err)
	}
	for _, want := range []string{filepath.Join(l.GitDir, rel) + " ", "was not run", "Remove the file", ".treebind/git", "git-directory.md"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message = %q, want it to say %q", got.Message, want)
		}
	}
}

func TestOpenRunsNoGitAgainstAGitDirectoryWithATrapInIt(t *testing.T) {
	for _, tt := range traps {
		for _, sub := range []string{subPath, ""} {
			for _, s := range stages {
				t.Run(fmt.Sprintf("%s, %s, sub path %q", tt.name, s, sub), func(t *testing.T) {
					f := newFixture(t)
					f.publish(branchMain, tracked)
					l := f.layout(sub)
					reach(t, f, l, s)
					sprang := tt.plant(t, f, l)
					before := snapshot(t, f.claim)
					options := f.options(l, branchMain)
					log := f.watchGit(&options, 0)

					_, err := Open(context.Background(), options)

					wantUnsafe(t, err, l, tt.path)
					if n := starts(t, log); n != 0 {
						t.Errorf("Git was started %d time(s), want not at all", n)
					}
					// Nothing in the work tree, the state directory or the Git directory is
					// touched, and what the refusal names is there to be seen.
					wantUnchanged(t, before, snapshot(t, f.claim))
					if sprang != nil && sprang() {
						t.Error("the trap sprang")
					}
					if !slices.Contains(tt.springsAt, s) {
						return
					}
					// The control: without the look at the Git directory, the same checkout
					// springs the trap. Without it, the test would pass with no protection.
					_, _ = openWithoutTheCheck(t, f, l, branchMain)
					if !sprang() {
						t.Error("the trap did not spring without the check, so the test does not show that the check stops it")
					}
				})
			}
		}
	}
}

// The Runner looks again before every command, so a trap that is planted after Open looked is
// refused too, and no Git is run after it.
func TestOpenRefusesATrapThatIsPlantedWhileTheCheckoutRuns(t *testing.T) {
	for _, sub := range []string{subPath, ""} {
		t.Run("sub path "+sub, func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, tracked)
			l := f.layout(sub)
			options := f.options(l, branchMain)
			log := f.watchGit(&options, 0)
			var marker string
			startedBefore := -1
			beforeCheckout = func() error {
				marker = plantFilter(t, f, l)
				startedBefore = starts(t, log)
				return nil
			}
			t.Cleanup(func() { beforeCheckout = nil })

			_, err := Open(context.Background(), options)

			wantUnsafe(t, err, l, gitInfo)
			if n := starts(t, log); startedBefore < 0 || n != startedBefore {
				t.Errorf("Git was started %d time(s) after the trap was planted, want not at all", n-startedBefore)
			}
			if exists(marker) {
				t.Error("the filter ran")
			}
			wantFiles(t, l, map[string]string{})
			for _, name := range []string{gitHead, filepath.Join(gitInfo, "attributes")} {
				if !exists(filepath.Join(l.GitDir, name)) {
					t.Errorf("%s is gone from the Git directory, which a refusal leaves as it is", name)
				}
			}
		})
		t.Run("the control, sub path "+sub, func(t *testing.T) {
			f := newFixture(t)
			f.publish(branchMain, tracked)
			l := f.layout(sub)
			if err := l.MakeWorkTree(); err != nil {
				t.Fatal(err)
			}
			var marker string
			beforeCheckout = func() error {
				marker = plantFilter(t, f, l)
				return nil
			}
			t.Cleanup(func() { beforeCheckout = nil })

			_, _ = openWithoutTheCheck(t, f, l, branchMain)

			if !exists(marker) {
				t.Error("the filter did not run without the check, so the test does not show that the check stops it")
			}
		})
	}
}

// A refusal of the Runner is GitDirUnsafe at whichever step it comes, and leaves the Git
// directory as it is. The file is planted after each command of a checkout in turn.
func TestOpenSaysGitDirUnsafeAtEveryStepTheRunnerRefuses(t *testing.T) {
	ways := []struct {
		name string
		sub  string
		s    stage
		// files are put in the work tree before the checkout.
		files map[string]string
	}{
		{"a clone with a sub path", subPath, stageNone, nil},
		{"a clone of the whole claim", "", stageNone, nil},
		{"an adoption of the whole claim", "", stageNone, tracked},
		{"a reopen", subPath, stageFinished, nil},
		{"a resume", subPath, stageHalfWay, nil},
	}
	for _, way := range ways {
		setup := func(t *testing.T, plantAfter int) (layout.Layout, Options, string) {
			f := newFixture(t)
			f.publish(branchMain, tracked)
			l := f.layout(way.sub)
			reach(t, f, l, way.s)
			for name, content := range way.files {
				write(t, l, name, content)
			}
			options := f.options(l, branchMain)
			log := f.watchGit(&options, plantAfter)
			return l, options, log
		}
		t.Run(way.name, func(t *testing.T) {
			_, options, log := setup(t, 0)
			if _, err := Open(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			commands := starts(t, log)
			if commands < 3 {
				t.Fatalf("the checkout ran %d command(s), which is too few for the test to show anything", commands)
			}
			for after := 1; after < commands; after++ {
				t.Run(fmt.Sprintf("planted after command %d of %d", after, commands), func(t *testing.T) {
					l, options, log := setup(t, after)

					_, err := Open(context.Background(), options)

					wantUnsafe(t, err, l, planted)
					if n := starts(t, log); n != after {
						t.Errorf("Git was started %d time(s), want %d: none after the file was planted", n, after)
					}
					if !exists(filepath.Join(l.GitDir, planted)) {
						t.Error("the Git directory was cleared after the refusal")
					}
				})
			}
		})
	}
}

// Git writes nothing in the Git directory that the look refuses, whichever way the checkout is
// made. A file of a new release of Git that is not on the list shows up here.
func TestTheGitDirectoryOfEveryCheckoutIsOneTheCheckAccepts(t *testing.T) {
	ways := []struct {
		name string
		s    stage
		// files are put in the work tree before the checkout.
		files map[string]string
		want  Outcome
	}{
		{"a clone", stageNone, nil, Cloned},
		{"an adoption", stageNone, tracked, Adopted},
		{"a reopen", stageFinished, nil, Reopened},
		{"a resume", stageHalfWay, nil, Cloned},
	}
	for _, way := range ways {
		for _, sub := range []string{subPath, ""} {
			t.Run(fmt.Sprintf("%s, sub path %q", way.name, sub), func(t *testing.T) {
				f := newFixture(t)
				f.publish(branchMain, tracked)
				l := f.layout(sub)
				reach(t, f, l, way.s)
				for name, content := range way.files {
					write(t, l, name, content)
				}

				got, err := open(t, f, l, branchMain)

				if err != nil || got.Outcome != way.want {
					t.Fatalf("result = %+v, %v, want %s", got, err, way.want)
				}
				if err := git.VerifyGitDir(l.GitDir, l.WorkTree); err != nil {
					t.Errorf("the Git directory that the checkout left is refused: %v", err)
				}
			})
		}
	}
}

func TestOpenSaysWhenItCannotLookAtTheGitDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory that is closed to it")
	}
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.Chmod(l.GitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.GitDir, 0o700) })

	_, err := open(t, f, l, branchMain)

	// The agent is shut out of its own directory. It cannot tell what is in it, so Git is not
	// run against it, and that is said before the repository is asked anything.
	got := errorOf(t, err, ReasonGitDirUnsafe)
	if !strings.Contains(got.Message, "cannot look at what is in "+l.GitDir) || !strings.Contains(got.Message, "Git is not run") ||
		!errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want the Git directory named and the failure to read it underneath", err)
	}
}

func TestOpenRefusesAPlantedFileBeforeItMakesTheSyncedDirectory(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	if err := os.WriteFile(filepath.Join(l.GitDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exists(l.WorkTree) {
		t.Fatal("the synced directory is there before the test, so it shows nothing")
	}

	_, err := open(t, f, l, branchMain)

	got := errorOf(t, err, ReasonGitDirUnsafe)
	if !strings.Contains(got.Message, notRun) {
		t.Errorf("message = %q, want it to say %q: it is the look at the directory that refused, and no command was started", got.Message, notRun)
	}
	if exists(l.WorkTree) {
		t.Error("the synced directory was made by a start that was refused: a refusal changes nothing")
	}
}

func TestOpenSaysWhichCommandWasNotRunWhenTheRunnerRefuses(t *testing.T) {
	f := newFixture(t)
	f.publish(branchMain, tracked)
	l := f.layout(subPath)
	beforeCheckout = func() error {
		return os.WriteFile(filepath.Join(l.GitDir, "notes.txt"), []byte("x"), 0o600)
	}
	t.Cleanup(func() { beforeCheckout = nil })

	_, err := open(t, f, l, branchMain)

	got := errorOf(t, err, ReasonGitDirUnsafe)
	if !strings.Contains(got.Message, "git checkout was not run") || !strings.Contains(got.Message, "notes.txt") {
		t.Errorf("message = %q, want the command that was not run and the file", got.Message)
	}
}
