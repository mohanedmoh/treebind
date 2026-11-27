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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The sub path, the branch and the address of the repository are passed to the agent as plain environment
// variables, the kubelet expands $(NAME) and $$ in the value of such a variable (#179), and the CRD takes a '$' in each of them
// (#208), so the agent has to read each as the spec writes it.

// loneDollar is a '$' that starts nothing, in the middle of a word.
const loneDollar = "cost$5"

// dollarSetting is one of the settings of the spec that the operator passes as an environment variable, with the values that
// have the characters that the kubelet would rewrite and that the API server takes.
type dollarSetting struct {
	name   string
	env    string
	set    func(*treebindv1alpha1.Treebind, string)
	read   func(agentconfig.Config) string
	values []string
}

// settingsOfTheSpecWithADollar are the four settings, each with values that sit on both sides of the kubelet's rules: a
// variable that came before it ($(TREEBIND_NAME), and the ones of the settings that are set first), $$, a '$' that starts
// nothing, a '$' at the end and a '$' that is all there is. Each is a value that the CRD and the agent take.
func settingsOfTheSpecWithADollar() []dollarSetting {
	return []dollarSetting{
		{
			name: "spec.target.subPath", env: agentconfig.EnvSubPath,
			set:  func(tb *treebindv1alpha1.Treebind, v string) { tb.Spec.Target.SubPath = v },
			read: func(c agentconfig.Config) string { return c.SubPath },
			values: []string{
				"data/$(TREEBIND_NAME)", "data/$(TREEBIND_NAMESPACE)/$(TREEBIND_NAME)", "$(TREEBIND_MOUNT_PATH)", "data/$$",
				"$$(TREEBIND_NAME)", loneDollar, "data$", "$", "$$", "a/$/b", "$x", "$(", "x/$(NOPE)", "release-1.2/$(TREEBIND_NAME)$",
			},
		},
		{
			name: "spec.repository.branch", env: agentconfig.EnvRepoBranch,
			set:  func(tb *treebindv1alpha1.Treebind, v string) { tb.Spec.Repository.Branch = v },
			read: func(c agentconfig.Config) string { return c.RepoBranch },
			values: []string{
				"feature/$(TREEBIND_NAME)", "$(TREEBIND_NAMESPACE)", "$(TREEBIND_REPO_URL)", "feature/$$", "feature/$$(TREEBIND_NAME)",
				loneDollar, "feature$", "$", "$$", "pre$post", "$x", "a$(", "feature/$(NOPE)", "release/$(TREEBIND_NAME)$",
			},
		},
		{
			name: "spec.repository.url", env: agentconfig.EnvRepoURL,
			set:  func(tb *treebindv1alpha1.Treebind, v string) { tb.Spec.Repository.URL = v },
			read: func(c agentconfig.Config) string { return c.RepoURL },
			values: []string{
				"https://example.com/$(TREEBIND_NAME).git", "https://example.com/$(TREEBIND_NAMESPACE)/$(TREEBIND_NAME).git",
				"https://example.com/$(TREEBIND_SUB_PATH).git", "https://example.com/$$.git", "https://example.com/$$(TREEBIND_NAME).git",
				"https://example.com/" + loneDollar + ".git", "https://example.com/repo$", "https://example.com/$", "https://example.com/$$",
				"https://example.com/a.git?x=$(TREEBIND_NAME)", "https://example.com/$x.git", "https://example.com/$(", "https://$(TREEBIND_NAME).example.com/a.git",
			},
		},
	}
}

// treebindForTheSettings is a Treebind that sets all three settings to values that have no '$', so that a variable that comes
// before the one under test is set and can be named in its value.
func treebindForTheSettings() *treebindv1alpha1.Treebind {
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Target.SubPath = subPathNested
	return treebind
}

func TestTheAgentReadsTheSubPathTheBranchAndTheUrlAsTheSpecWritesThemAfterTheKubelet(t *testing.T) {
	for _, setting := range settingsOfTheSpecWithADollar() {
		for _, value := range setting.values {
			treebind := treebindForTheSettings()
			setting.set(treebind, value)
			container, _ := agentContainer(t, treebind)

			env := environmentAfterTheKubelet(container)
			config, err := agentconfig.FromEnv(func(name string) string { return env[name] })

			if err != nil {
				t.Errorf("%s %q: the agent refuses the environment of its own pod: %v", setting.name, value, err)
				continue
			}
			if got := setting.read(config); got != value {
				t.Errorf("%s %q: the agent reads %q, and the kubelet gave it %q in %s", setting.name, value, got, env[setting.env], setting.env)
			}
		}
	}
}

// A lone '$' is left as it is by the kubelet, so the test above cannot tell a '$' that was written as '$$' from one that was
// not. The operator writes every '$' the same way (ADR 0031), and a rule that told them apart would have to know the rules of
// the kubelet, which is what a record of the form of the value pins.
func TestEveryDollarOfTheSubPathTheBranchAndTheUrlIsWrittenAsTwoInThePod(t *testing.T) {
	for _, setting := range settingsOfTheSpecWithADollar() {
		for _, value := range setting.values {
			treebind := treebindForTheSettings()
			setting.set(treebind, value)
			container, _ := agentContainer(t, treebind)

			got := agentEnvironmentOf(container)[setting.env]

			if want := strings.ReplaceAll(value, "$", "$$"); got != want {
				t.Errorf("%s %q: %s is %q in the pod, want %q: each '$' has to be written as '$$'", setting.name, value, setting.env, got, want)
			}
		}
	}
}

// A value that has no '$' is in the pod as the spec writes it, byte for byte, so that the pod template of a Treebind that has
// none is the one that the operator wrote before it escaped them and none of its sync pods is replaced (#208). The hashes that
// hold it for whole templates are in TestATemplateWithoutListsIsTheOneOfTheOperatorBefore.
func TestTheSubPathTheBranchAndTheUrlWithoutADollarAreInThePodAsTheSpecWritesThem(t *testing.T) {
	var withoutDollar []string
	for _, candidate := range append(branchCandidates(), repositoryURLCandidates()...) {
		if candidate != "" && !strings.Contains(candidate, "$") {
			withoutDollar = append(withoutDollar, candidate)
		}
	}
	// A sub path is relative, so its values are of its own kind. The address and the branch are put in as they are, whatever
	// they hold, and the operator does not judge them.
	values := map[string][]string{
		"spec.target.subPath":    {subPathOne, subPathNested, subPathLookalike, subPathRelease, " a b ", "é/日本語", "a%b", "a'b", `a"b`},
		"spec.repository.branch": withoutDollar,
		"spec.repository.url":    withoutDollar,
	}
	var checked int
	for _, setting := range settingsOfTheSpecWithADollar() {
		for _, value := range values[setting.name] {
			treebind := treebindForTheSettings()
			setting.set(treebind, value)
			container, _ := agentContainer(t, treebind)

			checked++
			if got := agentEnvironmentOf(container)[setting.env]; got != value {
				t.Errorf("%s %q: %s is %q in the pod, want it as the spec writes it", setting.name, value, setting.env, got)
			}
		}
	}
	// The lists have to hold many, or the check above says little.
	if checked < 100 {
		t.Errorf("only %d values were checked", checked)
	}
}

var _ = Describe("a '$' in the sub path, the branch and the address of the repository", func() {
	It("is taken by the API server, which is why the operator has to write it for the kubelet", func() {
		ctx := context.Background()
		var accepted int
		for _, setting := range settingsOfTheSpecWithADollar() {
			for _, value := range setting.values {
				treebind := treebindForTheSettings()
				treebind.Name = "dollar"
				setting.set(treebind, value)

				err := k8sClient.Create(ctx, treebind, client.DryRunAll)

				if err != nil && !apierrors.IsInvalid(err) {
					Fail(fmt.Sprintf("the API server failed to judge %s %q: %v", setting.name, value, err))
				}
				Expect(err).NotTo(HaveOccurred(), "%s %q: the list holds values that the API server takes", setting.name, value)
				accepted++
			}
		}
		Expect(accepted).To(BeNumerically(">", 40))
	})
})
