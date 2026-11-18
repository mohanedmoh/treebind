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

package push

import (
	"strings"
	"testing"
)

const (
	// hostWords is a line that a host says, which the tests look for in what is reported.
	hostWords = "Changes must be made through a pull request."
	refMain   = "refs/heads/main"
	toLine    = "To ssh://git@example.invalid/acme/dashboards.git\n"
)

// porcelain is what `git push --porcelain` writes to standard output for one ref.
func porcelain(flag, ref, summary string) string {
	return toLine + flag + "\t" + ref + ":" + ref + "\t" + summary + "\nDone\n"
}

// The first three outputs are the ones that real Git, 2.34.1, wrote in the tests of this package. The ones of the
// hosts below are SAMPLES, written from the wording that each host documents or is known to use, and not captured
// from a host: they say what the classifier does with that wording, and not that a host words it so today.
func TestTheClassifierReadsOnlyWhatGitWritesAndKeepsWhatTheHostSaid(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		stderr     string
		reason     string
		wantInText []string
	}{
		{
			name:   "git: fetch first, the remote has commits that are not here",
			stdout: porcelain("!", refMain, "[rejected] (fetch first)"),
			stderr: "error: failed to push some refs to 'x'\nhint: Updates were rejected because the remote contains work that you do\n",
			reason: ReasonNonFastForward,
		},
		{
			name:   "git: non-fast-forward, the tip of the remote is known here",
			stdout: porcelain("!", refMain, "[rejected] (non-fast-forward)"),
			reason: ReasonNonFastForward,
		},
		{
			name:       "git: a hook of the repository declines, with its own words",
			stdout:     porcelain("!", refMain, "[remote rejected] (pre-receive hook declined)"),
			stderr:     "remote: GH006: Protected branch update failed for refs/heads/main.        \nremote: Changes must be made through a pull request.        \nerror: failed to push some refs to 'x'\n",
			reason:     ReasonPushRejected,
			wantInText: []string{"pre-receive hook declined", "GH006: Protected branch update failed for refs/heads/main.", hostWords},
		},
		{
			name:   "SAMPLE GitHub: a protected branch",
			stdout: porcelain("!", refMain, "[remote rejected] (protected branch hook declined)"),
			stderr: "remote: error: GH006: Protected branch update failed for refs/heads/main.\nremote: error: Changes must be made through a pull request.\n",
			reason: ReasonPushRejected, wantInText: []string{"protected branch hook declined", hostWords},
		},
		{
			name:   "SAMPLE GitHub: push protection found a secret",
			stdout: porcelain("!", refMain, "[remote rejected] (push declined due to repository rule violations)"),
			stderr: "remote: error: GH013: Repository rule violations found for refs/heads/main.\nremote: - GITHUB PUSH PROTECTION\nremote:   Resolve the following violations before pushing again\n",
			reason: ReasonPushRejected, wantInText: []string{"repository rule violations", "GITHUB PUSH PROTECTION"},
		},
		{
			name:   "SAMPLE GitHub: a file that is too large",
			stdout: porcelain("!", refMain, "[remote rejected] (pre-receive hook declined)"),
			stderr: "remote: error: File big.bin is 120.00 MB; this exceeds GitHub's file size limit of 100.00 MB\n",
			reason: ReasonPushRejected, wantInText: []string{"exceeds GitHub's file size limit"},
		},
		{
			name:   "SAMPLE GitLab: a protected branch",
			stdout: porcelain("!", refMain, "[remote rejected] (pre-receive hook declined)"),
			stderr: "remote: GitLab: You are not allowed to push code to protected branches on this project.\n",
			reason: ReasonPushRejected, wantInText: []string{"You are not allowed to push code to protected branches"},
		},
		{
			name:   "SAMPLE Gitea: a branch that does not take a push",
			stdout: porcelain("!", refMain, "[remote rejected] (pre-receive hook declined)"),
			stderr: "remote: Gitea: branch main is protected from force push\n",
			reason: ReasonPushRejected, wantInText: []string{"is protected from force push"},
		},
		{
			name:   "SAMPLE Bitbucket: a branch restriction",
			stdout: porcelain("!", refMain, "[remote rejected] (hook declined)"),
			stderr: "remote: Bitbucket: Push rejected because of a branch restriction on refs/heads/main.\n",
			reason: ReasonPushRejected, wantInText: []string{"hook declined", "branch restriction"},
		},
		{
			name:   "SAMPLE: a rejection of Git that is not of the branch moving",
			stdout: porcelain("!", refMain, "[rejected] (already exists)"),
			reason: ReasonPushRejected, wantInText: []string{"already exists"},
		},
		{
			name:   "SAMPLE: a failure of the remote that Git words as a failure",
			stdout: porcelain("!", refMain, "[remote failure] (unpacker error)"),
			reason: ReasonPushRejected, wantInText: []string{"unpacker error"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status := parsePorcelain(c.stdout, refMain)
			if !status.found || status.flag != '!' {
				t.Fatalf("parsePorcelain = %+v, want the rejected ref found", status)
			}
			reason, message := rejection(status, c.stderr, "main")
			if reason != c.reason {
				t.Errorf("reason = %s, want %s", reason, c.reason)
			}
			for _, want := range c.wantInText {
				if !strings.Contains(message, want) {
					t.Errorf("message = %q, want it to hold %q", message, want)
				}
			}
			if strings.Contains(message, "\n") || strings.Contains(message, "  ") {
				t.Errorf("message = %q, want one line with its spaces collapsed", message)
			}
		})
	}
}

func TestPorcelainOfASuccessAndOfNothingToPush(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout string
		flag   byte
	}{
		"a new branch":      {porcelain("*", refMain, "[new branch]"), '*'},
		"a fast-forward":    {porcelain(" ", refMain, "f62870c..62118ec"), ' '},
		"up to date":        {porcelain("=", refMain, "[up to date]"), '='},
		"a forced update":   {porcelain("+", refMain, "f62870c...62118ec (forced update)"), '+'},
		"a deletion":        {porcelain("-", refMain, "[deleted]"), '-'},
		"with extra output": {"remote: hello\n" + porcelain("*", refMain, "[new branch]"), '*'},
	} {
		t.Run(name, func(t *testing.T) {
			status := parsePorcelain(tc.stdout, refMain)
			if !status.found || status.flag != tc.flag {
				t.Errorf("parsePorcelain = %+v, want the flag %q", status, tc.flag)
			}
		})
	}
}

func TestPorcelainFindsNothingWhenGitSaidNothingOfTheBranch(t *testing.T) {
	for name, stdout := range map[string]string{
		"nothing at all":           "",
		"only the repository":      toLine,
		"another ref":              porcelain("!", "refs/heads/other", "[rejected] (fetch first)"),
		"a line with no tabs":      toLine + "! refs/heads/main:refs/heads/main [rejected]\nDone\n",
		"a line with two fields":   toLine + "!\trefs/heads/main:refs/heads/main\n",
		"a flag of two characters": toLine + "!!\trefs/heads/main:refs/heads/main\t[rejected] (fetch first)\n",
		"no flag":                  toLine + "\trefs/heads/main:refs/heads/main\t[rejected] (fetch first)\n",
	} {
		t.Run(name, func(t *testing.T) {
			if status := parsePorcelain(stdout, refMain); status.found {
				t.Errorf("parsePorcelain = %+v, want nothing found", status)
			}
		})
	}
}

func TestWhatTheHostSaidIsTheRemoteLinesOnlyBoundedAndInOneLine(t *testing.T) {
	stderr := "Counting objects: 3, done.\nremote: first line   \nremote:\nremote:    indented\nerror: failed to push some refs\nhint: not the host\n" +
		strings.Repeat("remote: filler filler filler filler filler\n", 200)
	got := hostText(stderr)

	if !strings.HasPrefix(got, "first line indented filler") {
		t.Errorf("hostText = %q, want the remote lines, in order, without the prefix and the blank ones", got[:min(len(got), 80)])
	}
	if strings.Contains(got, "hint") || strings.Contains(got, "Counting") || strings.Contains(got, "failed to push") {
		t.Errorf("hostText = %q, want what Git itself says left out", got)
	}
	if len(got) > maxHostText {
		t.Errorf("hostText is %d bytes, want at most %d", len(got), maxHostText)
	}
	if hostText("error: nothing from the host\n") != "" {
		t.Error("a standard error with no remote line has host text")
	}
}

func TestWhatTheHostSaidKeepsItsUTF8Whole(t *testing.T) {
	got := hostText("remote: " + strings.Repeat("é日", maxHostText) + "\n")
	if len(got) > maxHostText {
		t.Fatalf("hostText is %d bytes, want at most %d", len(got), maxHostText)
	}
	if strings.ToValidUTF8(got, "") != got {
		t.Error("the text was cut in the middle of a character")
	}
}

func TestWhatTheHostSaidHasTheCredentialsOfAnyURLRemovedWhateverTheRunnerDidBefore(t *testing.T) {
	// The Runner removes them from standard error already. This is a second protection, for text that reaches
	// the message by another way.
	got := hostText("remote: see ssh://git:hunter2@example.invalid/acme/dashboards.git and https://example.invalid/x?token=abc123\n")

	if strings.Contains(got, "hunter2") || strings.Contains(got, "abc123") {
		t.Errorf("hostText = %q, want the credentials of the URLs removed", got)
	}
	if !strings.Contains(got, "example.invalid/acme/dashboards.git") {
		t.Errorf("hostText = %q, want the rest of what the host said kept", got)
	}
}
