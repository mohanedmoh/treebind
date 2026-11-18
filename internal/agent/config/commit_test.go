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

package config

import "testing"

func TestFromEnvTakesTheSettingsOfTheCommit(t *testing.T) {
	env := complete()
	env[EnvCommitAuthorName] = "treebind-bot"
	env[EnvCommitAuthorEmail] = "bot@example.org"
	env[EnvCommitMessageTemplate] = "treebind: sync {{.Count}} file(s)\n\nby the agent"

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.CommitAuthorName != "treebind-bot" || config.CommitAuthorEmail != "bot@example.org" {
		t.Errorf("the author is %q <%s>, want treebind-bot <bot@example.org>", config.CommitAuthorName, config.CommitAuthorEmail)
	}
	if want := "treebind: sync {{.Count}} file(s)\n\nby the agent"; config.CommitMessageTemplate != want {
		t.Errorf("the template is %q, want %q, with its blank line", config.CommitMessageTemplate, want)
	}
}

func TestFromEnvLeavesTheSettingsOfTheCommitEmptyWhenTheyAreNotSet(t *testing.T) {
	config, err := fromMap(complete())

	if err != nil {
		t.Fatal(err)
	}
	if config.CommitAuthorName != "" || config.CommitAuthorEmail != "" || config.CommitMessageTemplate != "" {
		t.Errorf("the settings of the commit are %q, %q and %q, want them empty, which the commit package fills in",
			config.CommitAuthorName, config.CommitAuthorEmail, config.CommitMessageTemplate)
	}
}

func TestTheSettingsOfTheCommitAreReadAsTheyAreWrittenWithTheirSpaces(t *testing.T) {
	// Spaces at the ends of a name are what Git would drop, and the agent says so when it makes its committer.
	// Trimming them here would hide it, and the commits would say another name than the spec.
	env := complete()
	env[EnvCommitAuthorName] = " bot "
	env[EnvCommitAuthorEmail] = " bot@example.org "
	env[EnvCommitMessageTemplate] = "  {{.Count}}  \n"

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if config.CommitAuthorName != " bot " || config.CommitAuthorEmail != " bot@example.org " || config.CommitMessageTemplate != "  {{.Count}}  \n" {
		t.Errorf("the settings are %q, %q and %q, want them as written", config.CommitAuthorName, config.CommitAuthorEmail, config.CommitMessageTemplate)
	}
}

func TestFromEnvDoesNotJudgeTheSettingsOfTheCommit(t *testing.T) {
	// An author or a template that is wrong is a failed check of the commit, with the reason in the report, and not
	// an agent that exits at start for ever: the commit package judges them when it makes its committer.
	env := complete()
	env[EnvCommitAuthorName] = "bot <x>"
	env[EnvCommitAuthorEmail] = "not-an-email"
	env[EnvCommitMessageTemplate] = "{{"

	if _, err := fromMap(env); err != nil {
		t.Errorf("FromEnv refuses the settings of the commit: %v", err)
	}
}

func TestTheVariablesOfTheCommitHaveTheNamesThatTheDocumentationGives(t *testing.T) {
	// The operator and the agent use the constants, so a name that is changed in both would pass every other test
	// and leave a pod of an older operator, and the documentation, with the name that was written.
	for got, want := range map[string]string{
		EnvCommitAuthorName:      "TREEBIND_COMMIT_AUTHOR_NAME",
		EnvCommitAuthorEmail:     "TREEBIND_COMMIT_AUTHOR_EMAIL",
		EnvCommitMessageTemplate: "TREEBIND_COMMIT_MESSAGE_TEMPLATE",
	} {
		if got != want {
			t.Errorf("a variable is called %q, want %q", got, want)
		}
	}
}
