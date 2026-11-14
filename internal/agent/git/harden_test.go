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
	"path/filepath"
	"strings"
	"testing"
)

// armed is a trap that a test planted in a repository: the command that makes plain
// Git spring it, and where to look for the proof that it did.
type armed struct {
	// args is the command, subcommand first.
	args []string
	// controlArgs is the command that springs the trap in plain Git, when that is
	// not the one that the Runner is given: the commands of the Runner are the ones
	// that do not run a diff program, and plain Git needs one that does.
	controlArgs []string
	// after checks what else has to hold once the Runner has run the command.
	after func(t *testing.T, r *repo)
	// marker is a file that the planted program creates. Empty when the trap
	// has no program, and then the control has to succeed instead.
	marker string
	// env is added to the environment of the unprotected control.
	env []string
	// option changes the Runner's options, for a trap that needs ssh or a CA.
	option func(*Options)
	// wantFailure is what the protected command has to fail with: the text
	// of its standard error. Empty means that it has to succeed.
	wantFailure string
}

// traps are things a repository, or a directory an application can write, can make
// Git run or read. Each one is planted with plain Git. The test then runs the same
// command two times: unprotected, where the trap has to spring, which proves the test
// can see it, and through the Runner, where it must not.
var traps = []struct {
	name string
	// need skips the trap on a Git that is too old to have the protection.
	need func(t *testing.T)
	arm  func(t *testing.T, r *repo) armed
}{
	{name: "a post-checkout hook", arm: func(_ *testing.T, r *repo) armed {
		return armed{args: []string{cmdCheckout, "-q", "--detach"}, marker: r.hook("post-checkout")}
	}},
	{name: "a pre-commit and a post-commit hook", arm: func(_ *testing.T, r *repo) armed {
		r.hook("pre-commit")
		return armed{args: []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"}, marker: r.hook("post-commit")}
	}},
	{name: "a post-merge hook", arm: func(_ *testing.T, r *repo) armed {
		r.plain(cmdCheckout, "-q", "-b", branchSide)
		r.write("b.txt", "b\n")
		r.plain(cmdAdd, "b.txt")
		r.plain(cmdCommit, "-q", "-m", branchSide)
		r.plain(cmdCheckout, "-q", "main")
		return armed{args: []string{cmdMerge, "-q", flagNoFF, flagNoEdit, branchSide}, marker: r.hook("post-merge")}
	}},
	{name: "core.hooksPath in the repository config", arm: func(_ *testing.T, r *repo) armed {
		dir := filepath.Join(r.root, "evilhooks")
		_, marker := r.trap("hooks")
		r.scriptAt(filepath.Join(dir, "pre-commit"), fmt.Sprintf("echo ran >> '%s'", marker))
		r.config("core.hooksPath", dir)
		return armed{args: []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"}, marker: marker}
	}},
	{name: "core.hooksPath in a file that the repository config includes", arm: func(_ *testing.T, r *repo) armed {
		dir := filepath.Join(r.root, "evilhooks")
		_, marker := r.trap("hooks")
		r.scriptAt(filepath.Join(dir, "pre-commit"), fmt.Sprintf("echo ran >> '%s'", marker))
		included := filepath.Join(r.root, "included.cfg")
		r.write("../included.cfg", fmt.Sprintf("[core]\n\thooksPath = %s\n", dir))
		r.config("include.path", included)
		return armed{args: []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"}, marker: marker}
	}},
	{name: "core.fsmonitor in the repository config", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("fsmonitor")
		r.config("core.fsmonitor", script)
		return armed{args: []string{cmdStatus, flagPorcelain}, marker: marker}
	}},
	{name: "core.sshCommand in the repository config", arm: func(_ *testing.T, r *repo) armed {
		fake, _ := r.fakeSSH()
		url, _ := r.remote()
		script, marker := r.trap("sshcommand")
		r.config("core.sshCommand", script)
		return armed{
			args: []string{cmdLsRemote, url}, marker: marker,
			option: func(o *Options) { o.SSH.Program = fake },
		}
	}},
	{name: "a credential helper, scoped and not", arm: func(t *testing.T, r *repo) armed {
		web := newWebServer(t, r.root, unauthorized)
		script, marker := r.trap("helper")
		r.config("credential.helper", "!"+script)
		r.config("credential."+web.URL+".helper", "!"+script)
		return armed{
			args: []string{cmdLsRemote, web.URL + "/r.git"}, marker: marker,
			env:         []string{"GIT_SSL_CAINFO=" + web.CABundle},
			option:      func(o *Options) { o.CABundle = web.CABundle },
			wantFailure: msgNoPrompt,
		}
	}},
	{name: "core.askPass in the repository config", arm: func(t *testing.T, r *repo) armed {
		web := newWebServer(t, r.root, unauthorized)
		script, marker := r.trap("askpass")
		r.config("core.askPass", script)
		return armed{
			args: []string{cmdLsRemote, web.URL + "/r.git"}, marker: marker,
			env:         []string{"GIT_SSL_CAINFO=" + web.CABundle},
			option:      func(o *Options) { o.CABundle = web.CABundle },
			wantFailure: msgNoPrompt,
		}
	}},
	{name: "core.editor in the repository config", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("editor")
		r.config("core.editor", script)
		return armed{args: []string{cmdCommit, "-q", flagAllowEmpty}, marker: marker, wantFailure: "empty commit message"}
	}},
	{name: "a filter that .gitattributes in the work tree selects, on add", need: needAttrSource, arm: func(_ *testing.T, r *repo) armed {
		marker := plantFilter(r, "clean")
		r.write(".gitattributes", "* filter=x\n")
		r.write(fileA, "changed\n")
		return armed{args: []string{cmdAdd, fileA}, marker: marker}
	}},
	{name: "a filter that .gitattributes in the work tree selects, on checkout", need: needAttrSource, arm: func(_ *testing.T, r *repo) armed {
		// Committed before the filter exists, so that arming it does not run it.
		r.write(".gitattributes", "* filter=x\n")
		r.plain(cmdAdd, ".gitattributes")
		r.plain(cmdCommit, "-q", "-m", "attributes")
		marker := plantFilter(r, "smudge")
		r.plain("rm", "-q", "--cached", fileA)
		return armed{args: []string{cmdCheckout, "-q", refHead, "--", fileA}, marker: marker}
	}},
	{name: "a filter that core.attributesFile selects", arm: func(_ *testing.T, r *repo) armed {
		marker := plantFilter(r, "clean")
		attributes := filepath.Join(r.root, "attributes")
		r.write("../attributes", "* filter=x\n")
		r.config("core.attributesFile", attributes)
		r.write(fileA, "changed\n")
		return armed{args: []string{cmdAdd, fileA}, marker: marker}
	}},
	{name: "an alias that no built-in command shadows", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("alias")
		r.config("alias.treebindtrap", "!"+script)
		return armed{args: []string{"treebindtrap"}, marker: marker, wantFailure: "is not one the agent may run"}
	}},
	{name: "a url rewrite to an ext:: transport", arm: func(_ *testing.T, r *repo) armed {
		marker := r.marker("ext")
		// In an ext:: address "% " is a space that belongs to the argument.
		r.config("url.ext::sh -c echo% ran% >>% "+marker+" %S.insteadOf", urlExample)
		return armed{
			args: []string{cmdLsRemote, urlExample}, marker: marker,
			env: []string{"GIT_ALLOW_PROTOCOL=ext"}, wantFailure: msgNotAllowed,
		}
	}},
	{name: "a url rewrite to a file:// transport", arm: func(_ *testing.T, r *repo) armed {
		_, path := r.remote()
		r.config("url.file://"+path+".insteadOf", urlExample)
		return armed{args: []string{cmdLsRemote, urlExample}, wantFailure: msgNotAllowed}
	}},
	{name: "a push url rewrite to an ext:: transport", arm: func(_ *testing.T, r *repo) armed {
		marker := r.marker("pushext")
		r.config("url.ext::sh -c echo% ran% >>% "+marker+" %S.pushInsteadOf", urlExample)
		return armed{
			args: []string{cmdPush, urlExample, "HEAD:refs/heads/x"}, marker: marker,
			env: []string{"GIT_ALLOW_PROTOCOL=ext"}, wantFailure: msgNotAllowed,
		}
	}},
	{name: "a credential helper scoped to the URL and no other", arm: func(t *testing.T, r *repo) armed {
		web := newWebServer(t, r.root, unauthorized)
		script, marker := r.trap("scopedhelper")
		r.config("credential."+web.URL+".helper", "!"+script)
		return armed{
			args: []string{cmdLsRemote, web.URL + "/r.git"}, marker: marker,
			env:         []string{"GIT_SSL_CAINFO=" + web.CABundle},
			option:      func(o *Options) { o.CABundle = web.CABundle },
			wantFailure: msgNoPrompt,
		}
	}},
	{name: "core.gitProxy in the repository config", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("gitproxy")
		r.config("core.gitProxy", script)
		return armed{
			args: []string{cmdLsRemote, urlGitProto}, marker: marker, wantFailure: msgNotAllowed,
		}
	}},
	{name: "core.hooksPath in a file that includeIf names", arm: func(_ *testing.T, r *repo) armed {
		dir := filepath.Join(r.root, "evilhooks")
		_, marker := r.trap("hooks")
		r.scriptAt(filepath.Join(dir, "pre-commit"), fmt.Sprintf("echo ran >> '%s'", marker))
		r.write("../included.cfg", fmt.Sprintf("[core]\n\thooksPath = %s\n", dir))
		r.config("includeIf.gitdir:"+r.gitDir+".path", filepath.Join(r.root, "included.cfg"))
		return armed{args: []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"}, marker: marker}
	}},
	{name: "commit.gpgSign with a signing program", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("gpg")
		r.config("commit.gpgSign", "true")
		r.config("gpg.program", script)
		return armed{
			args: []string{cmdCommit, "-q", flagAllowEmpty, "-m", "x"}, marker: marker,
			after: func(t *testing.T, r *repo) {
				t.Helper()
				if head := r.plain(cmdCatFile, "-p", refHead); strings.Contains(head, "gpgsig") || !strings.Contains(head, "\nx\n") {
					t.Errorf("the commit is signed, or is not the one that was asked for:\n%s", head)
				}
			},
		}
	}},
	{name: "push.gpgSign with a signing program", arm: func(t *testing.T, r *repo) armed {
		fake, _ := r.fakeSSH()
		url, path := r.remote()
		// A server that takes signed pushes advertises a nonce.
		gitIn(t, path, "config", "receive.certNonceSeed", "seed")
		script, marker := r.trap("pushsigner")
		r.config("push.gpgSign", "true")
		r.config("gpg.program", script)
		return armed{
			args: []string{cmdPush, "-q", url, "HEAD:refs/heads/pushed"}, marker: marker,
			env:    []string{"GIT_SSH_COMMAND=" + fake},
			option: func(o *Options) { o.SSH.Program = fake },
		}
	}},
	{name: "merge.verifySignatures with a verifying program", arm: func(_ *testing.T, r *repo) armed {
		r.plain(cmdCheckout, "-q", "-b", branchSide)
		r.write("b.txt", "b\n")
		r.plain(cmdAdd, "b.txt")
		r.plain(cmdCommit, "-q", "-m", branchSide)
		r.plain(cmdCheckout, "-q", "main")
		signCommit(r, branchSide)
		script, marker := r.trap("verify")
		r.config("merge.verifySignatures", "true")
		r.config("gpg.program", script)
		return armed{args: []string{cmdMerge, "-q", flagNoFF, flagNoEdit, branchSide}, marker: marker}
	}},
	{name: "a merge driver that core.attributesFile selects", arm: func(_ *testing.T, r *repo) armed {
		attributes := filepath.Join(r.root, "attributes")
		r.write("../attributes", "* merge=x\n")
		return armMerge(r, "core.attributesFile", attributes)
	}},
	{name: "a merge driver that .gitattributes in the work tree selects", need: needAttrSource, arm: func(_ *testing.T, r *repo) armed {
		r.write(".gitattributes", "* merge=x\n")
		r.plain(cmdAdd, ".gitattributes")
		r.plain(cmdCommit, "-q", "-m", "attributes")
		return armMerge(r, "", "")
	}},
	{name: "diff.external and a text converter that core.attributesFile selects", arm: func(_ *testing.T, r *repo) armed {
		script, marker := r.trap("diff")
		attributes := filepath.Join(r.root, "attributes")
		r.write("../attributes", "* diff=x\n")
		r.config("core.attributesFile", attributes)
		r.config("diff.external", script)
		r.config("diff.x.textconv", script)
		r.config("diff.x.command", script)
		r.write(fileA, "changed\n")
		// "git diff" runs them, and the commands of the agent that show a diff do not.
		return armed{
			args: []string{cmdDiffIndex, "-p", refHead}, controlArgs: []string{"diff", refHead}, marker: marker,
		}
	}},
}

// armMerge prepares two branches that change one file in places that do not
// overlap, so that Git has to merge it, and plants a merge driver. The attributes
// that select the driver come from a config key, when there is one, or from the
// work tree.
func armMerge(r *repo, key, value string) armed {
	r.write(fileA, "1\n2\n3\n")
	r.plain(cmdAdd, fileA)
	r.plain(cmdCommit, "-q", "-m", "three lines")
	r.plain(cmdCheckout, "-q", "-b", branchSide)
	r.write(fileA, "1\n2\nthree\n")
	r.plain(cmdCommit, "-q", "-a", "-m", branchSide)
	r.plain(cmdCheckout, "-q", "main")
	r.write(fileA, "one\n2\n3\n")
	r.plain(cmdCommit, "-q", "-a", "-m", "main")
	script, marker := r.trap("mergedriver")
	r.config("merge.x.driver", script+" %O %A %B")
	if key != "" {
		r.config(key, value)
	}
	return armed{args: []string{cmdMerge, "-q", flagNoFF, flagNoEdit, branchSide}, marker: marker}
}

// needAttrSource skips a test on a Git that cannot read attributes from a tree.
func needAttrSource(t *testing.T) {
	t.Helper()
	needGit(t, 2, 40, "added GIT_ATTR_SOURCE")
}

// plantFilter defines a filter for the given driver, "clean" or "smudge", that
// copies its input and notes in a marker that it ran.
func plantFilter(r *repo, driver string) (marker string) {
	marker = r.marker("filter-" + driver)
	script := r.script("filter-"+driver, fmt.Sprintf(
		"echo ran >> '%s'\nwhile IFS= read -r line; do printf '%%s\\n' \"$line\"; done", marker))
	r.config("filter.x."+driver, script)
	return marker
}

func TestPlantedTrapsDoNotSpring(t *testing.T) {
	for _, tc := range traps {
		for _, l := range layouts {
			t.Run(tc.name+", "+string(l), func(t *testing.T) {
				if tc.need != nil {
					tc.need(t)
				}

				control := newRepo(t, l)
				trap := tc.arm(t, control)
				controlArgs := trap.args
				if trap.controlArgs != nil {
					controlArgs = trap.controlArgs
				}
				if trap.marker != "" {
					// The marker proves that the trap is armed: plain Git springs it.
					// Whether the command succeeds does not matter.
					_, _ = control.plainErr(control.env(trap.env...), controlArgs...)
					wantRan(t, trap.marker, tc.name)
				} else if out, err := control.plainErr(control.env(trap.env...), controlArgs...); err != nil {
					t.Fatalf("the unprotected control should have succeeded: %v: %s", err, out)
				}

				protected := newRepo(t, l)
				trap = tc.arm(t, protected)
				var change []func(*Options)
				if trap.option != nil {
					change = append(change, trap.option)
				}
				_, err := protected.run(protected.runner(change...), trap.args...)

				switch {
				case trap.wantFailure == "" && err != nil:
					t.Fatalf("the protected command failed: %v", err)
				case strings.HasPrefix(trap.wantFailure, "is not one"):
					if !errors.Is(err, ErrNotAllowed) {
						t.Fatalf("got %v, want the subcommand to be refused", err)
					}
				case trap.wantFailure != "":
					wantGitFailure(t, err, trap.wantFailure)
				}
				if trap.marker != "" {
					wantNotRan(t, trap.marker, tc.name)
				}
				if trap.after != nil {
					trap.after(t, protected)
				}
			})
		}
	}
}

func TestAStagedBlobIsTheBytesOfTheFile(t *testing.T) {
	const crlf = "a\r\nb\r\n"
	t.Run("whatever core.autocrlf says", func(t *testing.T) {
		r := newRepo(t, insideWork)
		r.config("core.autocrlf", "true")
		r.write("plain.txt", crlf)
		r.write("runner.txt", crlf)

		r.plain(cmdAdd, "plain.txt")
		if blob := r.plain(cmdCatFile, "blob", ":plain.txt"); blob == crlf {
			t.Fatal("plain Git did not convert the line endings, so the test proves nothing")
		}
		if _, err := r.run(r.runner(), cmdAdd, "runner.txt"); err != nil {
			t.Fatal(err)
		}
		blob, err := r.run(r.runner(), cmdCatFile, "blob", ":runner.txt")
		if err != nil {
			t.Fatal(err)
		}
		if string(blob.Stdout) != crlf {
			t.Errorf("the staged blob is %q, want the bytes of the file", blob.Stdout)
		}
	})

	t.Run("whatever .gitattributes says", func(t *testing.T) {
		needAttrSource(t)
		r := newRepo(t, insideWork)
		r.write(".gitattributes", "* text eol=lf\n")
		r.write("plain.txt", crlf)
		r.write("runner.txt", crlf)

		r.plain(cmdAdd, "plain.txt")
		if blob := r.plain(cmdCatFile, "blob", ":plain.txt"); blob == crlf {
			t.Fatal("plain Git did not convert the line endings, so the test proves nothing")
		}
		if _, err := r.run(r.runner(), cmdAdd, "runner.txt"); err != nil {
			t.Fatal(err)
		}
		blob, err := r.run(r.runner(), cmdCatFile, "blob", ":runner.txt")
		if err != nil {
			t.Fatal(err)
		}
		if string(blob.Stdout) != crlf {
			t.Errorf("the staged blob is %q, want the bytes of the file", blob.Stdout)
		}
	})
}

// signCommit puts a signature that means nothing into the commit that the branch
// points at. Git starts the verifying program only for a commit that has one.
func signCommit(r *repo, branch string) {
	r.t.Helper()
	commit := r.plain(cmdCatFile, "commit", branch)
	head, message, _ := strings.Cut(commit, "\n\n")
	signed := head + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n abcd\n -----END PGP SIGNATURE-----\n\n" + message
	file := filepath.Join(r.root, "signed-commit")
	if err := os.WriteFile(file, []byte(signed), 0o644); err != nil {
		r.t.Fatal(err)
	}
	id := strings.TrimSpace(r.plain("hash-object", "-t", "commit", "-w", file))
	r.plain("update-ref", "refs/heads/"+branch, id)
}

func TestASigningProgramIsNotRunEvenWhenSigningIsAskedFor(t *testing.T) {
	// The Runner refuses -S, so this goes around it: the settings alone have to keep
	// each program that a config names from running when a command signs.
	for _, key := range []string{"gpg.program", "gpg.openpgp.program"} {
		for _, l := range layouts {
			t.Run(key+", "+string(l), func(t *testing.T) {
				arm := func(t *testing.T) (*repo, string) {
					r := newRepo(t, l)
					script, marker := r.trap("signer")
					r.config(key, script)
					return r, marker
				}
				signing := []string{cmdCommit, "-q", "-S", flagAllowEmpty, "-m", "x"}

				control, marker := arm(t)
				_, _ = control.plainErr(control.env(), signing...)
				wantRan(t, marker, key)

				r, marker := arm(t)
				_, err := r.runner().run(context.Background(), 0, signing)
				if err == nil {
					t.Error("a commit was made although the signing program is /dev/null")
				}
				wantNotRan(t, marker, key)
			})
		}
	}
}

func TestAutomaticGcDoesNotRun(t *testing.T) {
	// Two packs and a limit of one, so that Git wants to consolidate them after
	// a commit, and does it before the call returns.
	arm := func(t *testing.T) *repo {
		r := newRepo(t, outsideWork)
		r.plain("repack", "-q", "-d")
		r.write("b.txt", "b\n")
		r.plain(cmdAdd, "b.txt")
		r.plain(cmdCommit, "-q", "-m", "second")
		r.plain("repack", "-q", "-d")
		r.config("gc.auto", "1")
		r.config("gc.autoPackLimit", "1")
		r.config("gc.autoDetach", "false")
		return r
	}
	packs := func(r *repo) int {
		found, err := filepath.Glob(filepath.Join(r.gitDir, "objects", "pack", "*.pack"))
		if err != nil {
			t.Fatal(err)
		}
		return len(found)
	}

	control := arm(t)
	if packs(control) != 2 {
		t.Fatalf("the test starts with %d packs, want 2", packs(control))
	}
	control.plain(cmdCommit, "-q", flagAllowEmpty, "-m", "x")
	if packs(control) != 1 {
		t.Fatalf("plain Git did not collect the garbage, so the test proves nothing: %d packs", packs(control))
	}

	r := arm(t)
	if _, err := r.run(r.runner(), cmdCommit, "-q", flagAllowEmpty, "-m", "x"); err != nil {
		t.Fatal(err)
	}
	if got := packs(r); got != 2 {
		t.Errorf("a commit started a gc: %d packs", got)
	}
}
