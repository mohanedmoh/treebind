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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	optForce   = "--force"
	optRecurse = "--recurse-submodules"
	optExecX   = "--exec=x"
	pushURL    = "ssh://example.invalid/r.git"
	pushBranch = "refs/heads/main:refs/heads/main"
)

// pushWith builds the arguments of a push as the agent makes it: the options, then -- and the repository and
// the refspecs.
func pushWith(options []string, refspecs ...string) []string {
	args := append([]string{cmdPush}, options...)
	args = append(args, "--", pushURL)
	return append(args, refspecs...)
}

func TestAPushThatCouldForceDeleteOrWriteMoreThanTheBranchIsRefusedBeforeGitRuns(t *testing.T) {
	root := tempDir(t)
	marker := filepath.Join(root, "ran")
	r := fakeGit(t, root, fmt.Sprintf("echo ran >> '%s'", marker))

	refused := map[string][]string{
		// FR-4: never force. Git takes any start of a long option that is not the start of another.
		optForce:                 pushWith([]string{optForce}, pushBranch),
		"-f":                     pushWith([]string{"-f"}, pushBranch),
		"-qf in a word":          pushWith([]string{"-qf"}, pushBranch),
		"-fq in a word":          pushWith([]string{"-fq"}, pushBranch),
		"--forc":                 pushWith([]string{"--forc"}, pushBranch),
		"--fo":                   pushWith([]string{"--fo"}, pushBranch),
		"--force-with-lease":     pushWith([]string{"--force-with-lease"}, pushBranch),
		"--force-with-lease=x:y": pushWith([]string{"--force-with-lease=main:abc"}, pushBranch),
		"--force-if-includes":    pushWith([]string{"--force-if-includes"}, pushBranch),
		"--force-wit=main:abc":   pushWith([]string{"--force-wit=main:abc"}, pushBranch),
		"--force-if-incl":        pushWith([]string{"--force-if-incl"}, pushBranch),
		"--mirror":               pushWith([]string{"--mirror"}, pushBranch),
		"--delete":               pushWith([]string{"--delete"}, "refs/heads/main"),
		"-d":                     pushWith([]string{"-d"}, "refs/heads/main"),
		// A push that names no refspec is Git's own choice of what to push, by a setting that the Runner does not pin.
		"a repository and no refspec": pushWith(nil),
		"nothing but the subcommand":  {cmdPush},
		"options and no refspec":      pushWith([]string{"--porcelain", "--no-verify"}),
		"--all":                       pushWith([]string{"--all"}),
		"--branches":                  pushWith([]string{"--branches"}),
		"--tags":                      pushWith([]string{"--tags"}),
		"--prune":                     pushWith([]string{"--prune"}, pushBranch),
		"--follow-tags":               pushWith([]string{"--follow-tags"}, pushBranch),
		"--set-upstream":              pushWith([]string{"--set-upstream"}, pushBranch),
		"-u":                          pushWith([]string{"-u"}, pushBranch),
		"--push-option":               pushWith([]string{"--push-option=ci.skip"}, pushBranch),
		"-o":                          pushWith([]string{"-o", "ci.skip"}, pushBranch),
		"-oci.skip":                   pushWith([]string{"-oci.skip"}, pushBranch),
		"--repo":                      pushWith([]string{"--repo=origin"}, pushBranch),
		"recurse-submodules":          pushWith([]string{optRecurse + "=on-demand"}, pushBranch),
		"--signed (as before)":        pushWith([]string{"--signed"}, pushBranch),
		"--receive-pack (before)":     pushWith([]string{"--receive-pack=x"}, pushBranch),
		"--exec (as before)":          pushWith([]string{optExecX}, pushBranch),
		// A refspec that forces, deletes, matches many refs or writes outside the branches.
		"a refspec with a +":                     pushWith(nil, "+"+pushBranch),
		"a refspec that deletes":                 pushWith(nil, ":refs/heads/main"),
		"a refspec with a glob":                  pushWith(nil, "refs/heads/*:refs/heads/*"),
		"a refspec to a tag":                     pushWith(nil, "refs/heads/main:refs/tags/v1"),
		"a refspec to a bare name":               pushWith(nil, "refs/heads/main:main"),
		"a bare name":                            pushWith(nil, "main"),
		"a second refspec that forces":           pushWith(nil, pushBranch, "+refs/heads/other:refs/heads/other"),
		"a refspec to a note":                    pushWith(nil, "refs/heads/main:refs/notes/x"),
		"an empty refspec":                       pushWith(nil, ""),
		"a destination that is only refs/heads/": pushWith(nil, "refs/heads/main:refs/heads/"),
	}
	for name, args := range refused {
		_, err := r.Run(context.Background(), args...)
		var gitErr *Error
		if !errors.Is(err, ErrNotAllowed) || !errors.As(err, &gitErr) || (gitErr.Option == "" && gitErr.Refspec == "") {
			t.Errorf("%s (%q): got %v, want it refused", name, args, err)
		}
	}
	if exists(marker) {
		t.Error("git was started for a push that is refused")
	}
}

func TestTheRefusalOfAPushNamesTheOptionOrTheRefspec(t *testing.T) {
	r := fakeGit(t, tempDir(t), "exit 0")

	_, err := r.Run(context.Background(), pushWith([]string{optForce}, pushBranch)...)
	if want := `git: the option "--force" is not one the agent may pass to push`; err == nil || err.Error() != want {
		t.Errorf("the text is %v, want %q", err, want)
	}
	_, err = r.Run(context.Background(), pushWith(nil, "+"+pushBranch)...)
	if want := `git: the refspec "+refs/heads/main:refs/heads/main" is not one the agent may push: it has to name a branch, and nothing that forces, deletes or matches many`; err == nil || err.Error() != want {
		t.Errorf("the text is %v, want %q", err, want)
	}
}

func TestAPushOfTheBranchWithTheOptionsOfTheAgentRuns(t *testing.T) {
	root := tempDir(t)
	marker := filepath.Join(root, "ran")
	r := fakeGit(t, root, fmt.Sprintf("echo ran >> '%s'", marker))

	for _, args := range [][]string{
		pushWith([]string{flagPorcelain, "--no-verify"}, pushBranch),
		pushWith([]string{flagPorcelain}, "HEAD:refs/heads/main"),
		pushWith([]string{"-q", "-v"}, "refs/heads/feature/x:refs/heads/feature/x"),
		pushWith([]string{flagPorcelain, "--atomic"}, pushBranch),
		// The form that names only the branch pushes it to the branch of the same name.
		pushWith([]string{flagPorcelain}, "refs/heads/main"),
		// Nothing after -- is an option, and the URL is not a refspec.
		{cmdPush, flagPorcelain, "--", "https://example.invalid/a+b*c.git", pushBranch},
		{cmdPush, "--", optForce, pushBranch}, // a repository that is called --force is a repository, which Git refuses by itself
	} {
		if _, err := r.Run(context.Background(), args...); err != nil {
			t.Errorf("%q: got %v, want it run", args, err)
		}
	}
	if !exists(marker) {
		t.Error("git was not started for the pushes that are allowed")
	}
}

func TestAnOptionThatForcesAfterTheEndOfTheOptionsIsNotAnOptionButStillAVerbatimRepository(t *testing.T) {
	// After -- every word is a repository or a refspec. A word that looks like --force there is a repository,
	// and the refspecs after it are still held to the rule.
	r := fakeGit(t, tempDir(t), "exit 0")
	if _, err := r.Run(context.Background(), cmdPush, "--", optForce, "+refs/heads/main:refs/heads/main"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("got %v, want the refspec refused", err)
	}
}

func TestOtherSubcommandsAreNotHeldToTheRulesOfPush(t *testing.T) {
	r := fakeGit(t, tempDir(t), "exit 0")
	for _, args := range [][]string{
		{cmdFetch, optForce, "--", pushURL, "+refs/heads/main:refs/remotes/origin/main"},
		{cmdFetch, "-f", "--", pushURL, "+refs/heads/main:refs/remotes/origin/main"},
		{"update-ref", "-d", "refs/remotes/origin/gone"},
	} {
		if _, err := r.Run(context.Background(), args...); err != nil {
			t.Errorf("%q: got %v, want it run", args, err)
		}
	}
	if strings.Contains(fmt.Sprint(refusedOptions[cmdFetch].long), "force") {
		t.Error("fetch is refused --force: the pull fetches with a + refspec")
	}
}

func TestGitTakesAStartOfAForcingOptionForTheOptionSoTheRunnerRefusesTheStartsToo(t *testing.T) {
	// This is why --force-wit is refused and not only --force-with-lease: the plain Git of the machine, and of the
	// image, forces with it. A start of --force alone is ambiguous with the longer options and Git refuses it, and
	// the Runner refuses it as well. The repository is pushed to a bare one on disk, rewritten, and pushed again.
	r := newRepo(t, outsideWork)
	bare := filepath.Join(r.root, "remote.git")
	if out, err := exec.Command(r.git, "init", "-q", "--bare", "--initial-branch=main", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	r.plain("push", "-q", bare, "HEAD:refs/heads/main")
	remoteTip := strings.TrimSpace(r.plain("rev-parse", refHead))
	r.plain("commit", "-q", "--amend", "--allow-empty", "-m", "rewritten")

	if out, err := r.plainErr(r.env(), "push", bare, "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a push of a rewritten history was not rejected: %s", out)
	}
	if out, err := r.plainErr(r.env(), "push", "--forc", bare, "HEAD:refs/heads/main"); err == nil || !strings.Contains(out, "ambiguous") {
		t.Errorf("--forc was %v: %s, want Git to call it ambiguous", err, out)
	}
	if out, err := r.plainErr(r.env(), "push", "--force-wit=main:"+remoteTip, bare, "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("Git does not take --force-wit for --force-with-lease: %v: %s", err, out)
	}
	if tip := strings.TrimSpace(r.plain("--git-dir="+bare, "rev-parse", "main")); tip != strings.TrimSpace(r.plain("rev-parse", refHead)) {
		t.Errorf("the repository is at %s after the push with --force-wit, want the rewritten commit", tip)
	}
}
