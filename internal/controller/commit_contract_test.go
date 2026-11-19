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
	"math"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/commitmsg"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The agent reads the author and the template of its commits from its pod (docs/adr/0031), and a message
// template may hold a '$', which the kubelet rewrites in the value of an environment variable (#179).

const (
	commitBot       = "treebind-bot"
	commitBotEmail  = "bot@example.org"
	commitTemplate  = "treebind: sync {{.Count}} file(s)"
	messageTogether = "authorName and authorEmail must be set together"

	// The messages of the rules of the CRD that the tests look for, and an address that comes up in the lists.
	messageNameChars  = "authorName must not contain '<', '>' or a control character"
	messageNameEnds   = "authorName must not start or end with"
	messageEmailShape = "authorEmail must be an address"
	messageEmailBare  = "authorEmail must be a bare address"
	messageEmailEnds  = "authorEmail must not start or end with"
	shortAddress      = "a@b"
)

// expandLikeTheKubelet is the expansion that the kubelet makes in the value of an environment variable
// (k8s.io/kubernetes/third_party/forked/golang/expansion, which is not a module that can be imported): $(NAME)
// becomes the value of a variable that came before it, or stays as it is when there is none; $$ becomes $;
// and a $ that starts nothing stays.
func expandLikeTheKubelet(input string, lookup func(string) (string, bool)) string {
	var out strings.Builder
	checkpoint := 0
	for cursor := 0; cursor < len(input); cursor++ {
		if input[cursor] != '$' || cursor+1 >= len(input) {
			continue
		}
		out.WriteString(input[checkpoint:cursor])
		var read string
		var advance int
		switch rest := input[cursor+1:]; rest[0] {
		case '$':
			read, advance = "$", 1
		case '(':
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				read, advance = "$(", 1
				break
			}
			name := rest[1:end]
			if value, ok := lookup(name); ok {
				read = value
			} else {
				read = "$(" + name + ")"
			}
			advance = end + 1
		default:
			read, advance = "$"+string(rest[0]), 1
		}
		out.WriteString(read)
		cursor += advance
		checkpoint = cursor + 1
	}
	return out.String() + input[checkpoint:]
}

// environmentAfterTheKubelet is what the container sees: each value expanded with the variables before it.
func environmentAfterTheKubelet(container corev1.Container) map[string]string {
	seen := agentEnvironmentOf(container)
	env := map[string]string{}
	for _, variable := range container.Env {
		value := seen[variable.Name]
		if variable.ValueFrom == nil {
			value = expandLikeTheKubelet(variable.Value, func(name string) (string, bool) {
				got, ok := env[name]
				return got, ok
			})
		}
		env[variable.Name] = value
	}
	return env
}

func TestTheKubeletExpansionOfThisTestIsTheOneOfTheKubelet(t *testing.T) {
	env := map[string]string{"NAME": "dev-ssh", "EMPTY": ""}
	lookup := func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	for input, want := range map[string]string{
		"plain":            "plain",
		"$(NAME)":          "dev-ssh",
		"a $(NAME) b":      "a dev-ssh b",
		"$$(NAME)":         "$(NAME)",
		"$$":               "$",
		"$$$$":             "$$",
		"$$$(NAME)":        "$dev-ssh",
		"$(NOPE)":          "$(NOPE)",
		"$(EMPTY)x":        "x",
		"$":                "$",
		"a$":               "a$",
		"$x":               "$x",
		"$(":               "$(",
		"$(NAME":           "$(NAME",
		"cost $5 and $$5":  "cost $5 and $5",
		"$(NAME)$(NAME)":   "dev-ssh" + "dev-ssh",
		"é$(NAME)日本":       "édev-ssh日本",
		"$()":              "$()",
		"$$(NAME) $(NAME)": "$(NAME) dev-ssh",
	} {
		if got := expandLikeTheKubelet(input, lookup); got != want {
			t.Errorf("expand(%q) = %q, want %q", input, got, want)
		}
	}
}

// commitSettingsOfTheCorpus are the settings that the operator puts in a pod, with the characters that the
// kubelet would rewrite and the ones that an environment variable holds as they are.
func commitSettingsOfTheCorpus() []treebindv1alpha1.CommitSpec {
	return []treebindv1alpha1.CommitSpec{
		{AuthorName: commitBot, AuthorEmail: commitBotEmail, MessageTemplate: commitTemplate},
		{AuthorName: "Zoë Müller 田中", AuthorEmail: "bot+tag@example.org", MessageTemplate: "line one\n\n  line three\t{{.Count}}\n"},
		{AuthorName: "cost $5", AuthorEmail: "a$b@example.org", MessageTemplate: "$(TREEBIND_NAME) $$ $(NOPE) $ $( $x {{.Count}}$"},
		{AuthorName: "$(TREEBIND_NAMESPACE)", AuthorEmail: "$$@example.org", MessageTemplate: "$$$$ $$(X) $(X"},
		{AuthorName: "$", AuthorEmail: "$@example.org", MessageTemplate: "$"},
		{MessageTemplate: "only a template, with a {{if gt .Count 1}}$(TREEBIND_POD_NAME){{end}}"},
		{AuthorName: "only a name", AuthorEmail: "only@example.org"},
		{MessageTemplate: strings.Repeat("$", 4096)},
		{MessageTemplate: "{{.Count}} \"quoted\" 'single' \\back\\ <angle> &amp;"},
	}
}

func TestTheAgentReadsTheSettingsOfTheCommitAsTheSpecWritesThemAfterTheKubelet(t *testing.T) {
	for _, spec := range commitSettingsOfTheCorpus() {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Commit = &spec
		treebind.Namespace = testNamespace
		container, _ := agentContainer(t, treebind)

		env := environmentAfterTheKubelet(container)
		config, err := agentconfig.FromEnv(func(name string) string { return env[name] })

		if err != nil {
			t.Fatalf("the agent refuses the environment of its own pod for %+v: %v", spec, err)
		}
		if config.CommitAuthorName != spec.AuthorName || config.CommitAuthorEmail != spec.AuthorEmail || config.CommitMessageTemplate != spec.MessageTemplate {
			t.Errorf("the agent reads %q, %q and %q, want %q, %q and %q", config.CommitAuthorName, config.CommitAuthorEmail,
				config.CommitMessageTemplate, spec.AuthorName, spec.AuthorEmail, spec.MessageTemplate)
		}
	}
}

func TestTheVariablesOfTheCommitAreSetOnlyForTheSettingsThatTheSpecHas(t *testing.T) {
	commitVariables := []string{agentconfig.EnvCommitAuthorName, agentconfig.EnvCommitAuthorEmail, agentconfig.EnvCommitMessageTemplate}
	for name, c := range map[string]*treebindv1alpha1.CommitSpec{
		"no commit section":    nil,
		"an empty one":         {},
		"only a template":      {MessageTemplate: commitTemplate},
		"only an author":       {AuthorName: commitBot, AuthorEmail: commitBotEmail},
		"all three":            {AuthorName: commitBot, AuthorEmail: commitBotEmail, MessageTemplate: commitTemplate},
		"a template of spaces": {MessageTemplate: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Commit = c
			container, _ := agentContainer(t, treebind)
			env := agentEnvironmentOf(container)

			for i, variable := range commitVariables {
				var want bool
				if c != nil {
					want = [...]string{c.AuthorName, c.AuthorEmail, c.MessageTemplate}[i] != ""
				}
				if _, set := env[variable]; set != want {
					t.Errorf("%s is set = %v, want %v for %+v: a variable of a setting that the spec does not have changes the pod "+
						"template, and the pod is replaced for nothing", variable, set, want, c)
				}
			}
		})
	}
}

var _ = Describe("spec.commit", func() {
	// valid says whether the API server takes a Treebind with the commit section, and why not. The dry run
	// validates it without keeping it.
	valid := func(ctx context.Context, spec treebindv1alpha1.CommitSpec) error {
		GinkgoHelper()
		treebind := newTreebind("commit")
		treebind.Spec.Commit = &spec
		err := k8sClient.Create(ctx, treebind, client.DryRunAll)
		if err != nil && !apierrors.IsInvalid(err) {
			Fail(fmt.Sprintf("the API server failed to judge %+v: %v", spec, err))
		}
		return err
	}

	It("takes every author that the agent takes", func() {
		ctx := context.Background()
		names := []string{
			commitBot, "Treebind Bot", "Zoë Müller", "田中 太郎", "bot (ci)", "a", "O'Brien", "Anne-Marie", "bot 2", "x.y",
			"$(X)", "cost $5", shortAddress, "é", "bot #1", "100%", strings.Repeat("n", 256), "Bot, the", "x\u00a0y",
		}
		emails := []string{
			commitBotEmail, "bot+ci@example.org", "a.b@c.d", "x_y@sub.example.org", "a$b@example.org", "o'brien@example.org",
			shortAddress, "1@2.3", "a-b@c-d.example", "UPPER@EXAMPLE.ORG", "a%b@example.org", "a/b@example.org",
		}
		var accepted int
		for _, name := range names {
			for _, email := range emails[:3] {
				if commitmsg.ValidateAuthor(name, email) != nil {
					continue
				}
				accepted++
				Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorName: name, AuthorEmail: email})).To(Succeed(), "%q <%s>: the agent takes it", name, email)
			}
		}
		for _, email := range emails {
			if commitmsg.ValidateAuthor(commitBot, email) != nil {
				continue
			}
			accepted++
			Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorName: commitBot, AuthorEmail: email})).To(Succeed(), "<%s>: the agent takes it", email)
		}
		// The corpus has to hold many that the agent takes, or the check above says little.
		Expect(accepted).To(BeNumerically(">", 40))
	})

	It("refuses what the agent could never use, and says which rule", func() {
		ctx := context.Background()
		for name, message := range map[string]string{
			"bot <x>":                messageNameChars,
			"<bot>":                  messageNameChars,
			"bo\nt":                  messageNameChars,
			"bo\tt":                  messageNameChars,
			"bo>t":                   messageNameChars,
			"bo\x7ft":                messageNameChars,
			"bo\x00t":                messageNameChars,
			"bot;":                   messageNameEnds,
			":bot":                   messageNameEnds,
			";bot":                   messageNameEnds,
			",bot":                   messageNameEnds,
			" bot":                   messageNameEnds,
			"bot ":                   messageNameEnds,
			".bot":                   messageNameEnds,
			"bot.":                   messageNameEnds,
			"bot,":                   messageNameEnds,
			`bot\`:                   messageNameEnds,
			`"bot"`:                  messageNameEnds,
			"'bot'":                  messageNameEnds,
			strings.Repeat("n", 257): "256",
		} {
			Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorName: name, AuthorEmail: commitBotEmail})).
				To(MatchError(ContainSubstring(message)), "%q", name)
		}
		for email, message := range map[string]string{
			"not-an-email":                      messageEmailShape,
			"a@":                                messageEmailShape,
			"@b":                                messageEmailShape,
			"a@@b":                              messageEmailShape,
			"a b@c.d":                           messageEmailShape,
			"a@b.c\n":                           messageEmailShape,
			"Name <a@b.c>":                      messageEmailShape,
			"<a@b.c>":                           messageEmailBare,
			"a@b.c(comment)":                    messageEmailBare,
			".a@b.c":                            messageEmailEnds,
			"a(b@c.d":                           messageEmailBare,
			"a@b.c)":                            messageEmailBare,
			"a>b@c.d":                           messageEmailBare,
			"a\x7f@c.d":                         messageEmailBare,
			`a@b.c\`:                            messageEmailEnds,
			`\a@b.c`:                            messageEmailEnds,
			"'a@b.c":                            messageEmailEnds,
			`a@b.c'`:                            messageEmailEnds,
			`"a@b.c`:                            messageEmailEnds,
			"a@b.c,":                            messageEmailEnds,
			"a@b.c;":                            messageEmailEnds,
			":a@b.c":                            messageEmailEnds,
			"a@b.c.":                            messageEmailEnds,
			strings.Repeat("a", 250) + "@b.cde": "254",
		} {
			Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorName: commitBot, AuthorEmail: email})).
				To(MatchError(ContainSubstring(message)), "%q", email)
		}
	})

	It("takes both parts of the author or neither", func() {
		ctx := context.Background()
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{})).To(Succeed())
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{MessageTemplate: commitTemplate})).To(Succeed())
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorName: commitBot})).To(MatchError(ContainSubstring(messageTogether)))
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{AuthorEmail: commitBotEmail})).To(MatchError(ContainSubstring(messageTogether)))
	})

	It("takes every template that the agent takes up to the limit, and refuses a NUL and a longer one", func() {
		ctx := context.Background()
		for _, template := range []string{
			commitmsg.DefaultTemplate, commitTemplate, "{{.Count}}", "one\n\ntwo\n", "# {{.Count}}", "$(TREEBIND_NAME) $$ $",
			"é日本 {{if gt .Count 1}}files{{else}}file{{end}}", strings.Repeat("x", 4096),
		} {
			Expect(valid(ctx, treebindv1alpha1.CommitSpec{MessageTemplate: template})).To(Succeed(), "%q", template)
		}
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{MessageTemplate: pathsNUL})).To(MatchError(ContainSubstring("messageTemplate must not contain a NUL")))
		Expect(valid(ctx, treebindv1alpha1.CommitSpec{MessageTemplate: strings.Repeat("x", 4097)})).To(MatchError(ContainSubstring("4096")))
	})
})

func TestTheAgentIsToldTheLimitOfCommitsOnlyWhenItIsNotTheDefault(t *testing.T) {
	// 60 is the default of the CRD and of the agent, so it is not passed: a pod template that never had the variable is not
	// changed, and its pod is not replaced (docs/adr/0035).
	defaulted := int32(agentconfig.DefaultCommitMaxPerHour)
	for name, c := range map[string]*treebindv1alpha1.CommitSpec{
		"no commit section":    nil,
		"an empty one":         {},
		"the default, filled in": {MaxPerHour: &defaulted},
	} {
		t.Run(name, func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Commit = c

			config, env, err := agentConfigOf(t, treebind)

			if err != nil {
				t.Fatal(err)
			}
			if value, set := env[agentconfig.EnvCommitMaxPerHour]; set || config.CommitMaxPerHour != agentconfig.DefaultCommitMaxPerHour {
				t.Errorf("%s is %q and the agent reads %d, want the variable left out and the default", agentconfig.EnvCommitMaxPerHour, value, config.CommitMaxPerHour)
			}
		})
	}

	limit := int32(12)
	treebind := newTreebind(testTreebindName)
	treebind.Spec.Commit = &treebindv1alpha1.CommitSpec{MaxPerHour: &limit}

	config, env, err := agentConfigOf(t, treebind)

	if err != nil {
		t.Fatalf("the agent refuses the environment of its own pod: %v", err)
	}
	// The name is written out, because it is what ADR 0035 and the page about the loops tell a person.
	if env["TREEBIND_COMMIT_MAX_PER_HOUR"] != "12" || config.CommitMaxPerHour != 12 {
		t.Errorf("TREEBIND_COMMIT_MAX_PER_HOUR = %q and the agent reads %d, want 12 on both sides", env["TREEBIND_COMMIT_MAX_PER_HOUR"], config.CommitMaxPerHour)
	}
}

func TestTheAgentAcceptsEveryLimitOfCommitsTheOperatorCanPutInThePod(t *testing.T) {
	for _, limit := range []int32{agentconfig.MinCommitMaxPerHour, 2, 59, 60, 61, 1000, agentconfig.MaxCommitMaxPerHour} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Commit = &treebindv1alpha1.CommitSpec{MaxPerHour: &limit}

			config, _, err := agentConfigOf(t, treebind)

			if err != nil || config.CommitMaxPerHour != int(limit) {
				t.Errorf("the agent reads %d, %v, want %d and no refusal", config.CommitMaxPerHour, err, limit)
			}
		})
	}
}

var _ = Describe("commit.maxPerHour", func() {
	It("is accepted by the API server exactly when the agent accepts it", func() {
		ctx := context.Background()
		var accepted, refused int
		for i, limit := range []int64{
			math.MinInt64, -1 << 40, -1, 0, 1, 2, 59, 60, 61, 9999, 10000, 10001, 1 << 20, math.MaxInt32, math.MaxInt32 + 1, math.MaxInt64,
		} {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("max-per-hour-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, limit, "spec", "commit", "maxPerHour")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			createErr := k8sClient.Create(ctx, treebind)

			// The operator writes the number as Go does, which is what a value that the API server took reaches the agent as.
			_, agentErr := agentconfig.CheckCommitMaxPerHour(strconv.FormatInt(limit, 10))
			if (createErr == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("maxPerHour %d: the API server accepts it = %v (%v), the agent accepts it = %v (%v): "+
					"a value that the API server takes and the agent refuses leaves a sync pod that exits at start for ever",
					limit, createErr == nil, createErr, agentErr == nil, agentErr))
			}
			if createErr == nil {
				accepted++
				Expect(k8sClient.Delete(ctx, treebind)).To(Succeed())
			} else {
				refused++
			}
		}
		// The list has to hold both, or the check above says nothing.
		Expect(accepted).To(BeNumerically(">", 4))
		Expect(refused).To(BeNumerically(">", 4))
	})

	It("is refused by the API server when it is not a whole number", func() {
		ctx := context.Background()
		for i, value := range []any{"60", 1.5, true, []any{int64(1)}, map[string]any{"n": int64(1)}} {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTreebind(fmt.Sprintf("max-per-hour-type-%d", i)))
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(object, value, "spec", "commit", "maxPerHour")).To(Succeed())
			treebind := &unstructured.Unstructured{Object: object}
			treebind.SetGroupVersionKind(treebindv1alpha1.GroupVersion.WithKind("Treebind"))

			Expect(k8sClient.Create(ctx, treebind)).NotTo(Succeed(), "%#v", value)
		}
	})

	It("is filled in with the default of the agent when the commit section is there, and not when it is not", func() {
		ctx := context.Background()
		withSection := newTreebind("max-per-hour-default")
		withSection.Spec.Commit = &treebindv1alpha1.CommitSpec{MessageTemplate: commitTemplate}
		Expect(k8sClient.Create(ctx, withSection)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, withSection)).To(Succeed()) })
		without := newTreebind("max-per-hour-no-section")
		Expect(k8sClient.Create(ctx, without)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, without)).To(Succeed()) })

		var got treebindv1alpha1.Treebind
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: withSection.Namespace, Name: withSection.Name}, &got)).To(Succeed())
		Expect(got.Spec.Commit.MaxPerHour).To(HaveValue(BeEquivalentTo(agentconfig.DefaultCommitMaxPerHour)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: without.Namespace, Name: without.Name}, &got)).To(Succeed())
		Expect(got.Spec.Commit).To(BeNil())
	})
})
