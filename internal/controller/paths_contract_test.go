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
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The agent reads the patterns of spec.paths from its pod (docs/adr/0022), and the API server and the
// agent have to agree on which lists exist: a list that the API server takes and the agent refuses
// leaves a sync pod that exits at start for ever, and one that the agent takes and the API server
// refuses is one that nobody can write.

// The patterns and the message that the tests of spec.paths repeat.
const (
	pathsTmp          = "*.tmp"
	pathsJSON         = "*.json"
	pathsUp           = "a/.."
	pathsSpace        = "a b"
	pathsNewline      = "a\nb"
	pathsAbsolute     = "/etc/**"
	pathsEmptySegment = "a//b"
	pathsUpInside     = "a/../b"
	pathsNUL          = "a\x00b"
	messageSyntax     = "patterns must close each '[' within its segment"
)

// pathPatternCandidates are patterns on both sides of every rule: those of the tests of
// internal/agent/paths, every pattern of one or two pieces of a set that has each special character
// in it, and longer ones put together at random from the same pieces, with a fixed seed.
func pathPatternCandidates() []string {
	fixed := []string{
		pathsTmp, "*.TMP", "logs/", "logs", "a/*.json", "*/x.json", "**/x.json", "a/**", "a/**/x.json", "a/**/",
		"tree/**/f", "*/er/*", "**/", "**", "**/**", "*", "a/", "x.json", "ab*/c", "*b*/c", "[!a].tmp", "[^a].tmp",
		"[a-c].txt", "[c-a].txt", "[A-Z].txt", "[Z-a].txt", "[]a]", "[!]a]", "[a-]", "[-a]", "a[-]b", "[a-c-e].txt",
		`[a\-c].txt`, `[\[]x`, `[a-\]]`, "[é].txt", `\#n#`, "#n#", `\!bang`, "sp ", `sp\ `, "a[1].txt", `a\[1].txt`,
		`a\*b`, "a*b", "a?b", `a\?b`, `q\\w`, ".env", ".env.*", "id_*", "?.txt", "??.txt", "é.txt", "tab?here",
		"tab\there", pathsNewline, "a[**]b", `[\a-c].txt`, `x\*y`, `\**`, `*\**`, "..x", "x..", "a..b", ".x", "x.",
		"dashboards/**/*.json", ".git/**", ".treebind", pathsSpace, " a", "a ",
		"", pathsNUL, pathsAbsolute, "/", "!keep.json", "..", "../a", pathsUpInside, pathsUp, "../", ".", "./a.tmp", "a/./b",
		"a/.", pathsEmptySegment, "a//", "//", `b\`, `a\/b`, `a\/`, `[a\`, `[a\/]`, "[x", "a[", "a[b/c]", "[]", "[!]",
		"[^]", "[!",
		"[a[b]", "[[]", "[[:alpha:]]", "[a-[:alpha:]]", "ab**/c", "a**", "**a", "***", "a/***/b", `\***`, `\`, "[",
		"]", "[!^]", "[^!]", "[!!]", "[^^]", `[\]]`, `[]-a]`, "[a-z]/**/[!b]", "a/**/b/**/c",
		strings.Repeat("a", 511), strings.Repeat("a", 512), strings.Repeat("a", 513),
		strings.Repeat("é", 512), strings.Repeat("é", 513), strings.Repeat("日", 512),
		strings.Repeat("a/", 255) + "aa", strings.Repeat("[a]", 170) + "a", strings.Repeat("[a]", 171),
		strings.Repeat("*a", 256), strings.Repeat(`\*`, 256), strings.Repeat("**/", 170) + "a",
	}
	pieces := []string{
		"a", "é", ".", "..", "/", "*", "**", "?", "[", "]", "!", "^", "-", `\`, ":", " ", "\t", "\x00", "#",
		"[a]", "[!a]", "[]a]", "[a-z]", `\[`, `\*`, "[:alpha:]", "{a,b}", "a/b",
	}
	const generated = 400
	candidates := make([]string, 0, len(fixed)+len(pieces)*(len(pieces)+1)+generated)
	candidates = append(candidates, fixed...)
	for _, first := range pieces {
		candidates = append(candidates, first)
		for _, second := range pieces {
			candidates = append(candidates, first+second)
		}
	}
	random := rand.New(rand.NewPCG(43, 43))
	for range generated {
		var pattern strings.Builder
		for range 2 + random.IntN(7) {
			pattern.WriteString(pieces[random.IntN(len(pieces))])
		}
		candidates = append(candidates, pattern.String())
	}
	return candidates
}

// pathListsOfTheCorpus returns the lists that the test gives to the operator: the candidates that the
// agent takes, packed into lists as long as the CRD lets them be; the longest list that is made of
// the characters that JSON writes in the most bytes, and the same made of '$'; and a list of the
// characters that JSON escapes or that the kubelet would expand, with a quote, a backslash and a
// space at the end.
func pathListsOfTheCorpus(t *testing.T) [][]string {
	t.Helper()
	var lists [][]string
	var list []string
	length := 0
	for _, pattern := range pathPatternCandidates() {
		if agentconfig.CheckPathPatterns([]string{pattern}) != nil {
			continue
		}
		full := len(list) == agentconfig.MaxPathPatterns
		if len(list) > 0 && (full || length+utf8.RuneCountInString(pattern) > agentconfig.MaxPathPatternsLength) {
			lists, list, length = append(lists, list), nil, 0
		}
		list = append(list, pattern)
		length += utf8.RuneCountInString(pattern)
	}
	// JSON writes \x01, and the operator writes '$', in six bytes, the most a character takes.
	longest := slices.Repeat([]string{strings.Repeat("\x01", 512)}, agentconfig.MaxPathPatternsLength/512)
	dollars := slices.Repeat([]string{strings.Repeat("$", 512)}, agentconfig.MaxPathPatternsLength/512)
	escaped := []string{
		"a<b>&c", `"quoted"`, `\"`, `q\\w`, "sp ", "é/**", "日本/*.md", "\u2028", "\t*",
		// The kubelet would expand these.
		"cost$$.txt", "$(TREEBIND_NAME)*", "$$(TREEBIND_POD_NAME)", "a$b", "$",
	}
	lists = append(lists, list, longest, dollars, escaped)
	for _, checked := range lists {
		if err := agentconfig.CheckPathPatterns(checked); err != nil {
			t.Fatalf("%q is not a list the CRD takes: %v", checked, err)
		}
	}
	return lists
}

// The agent reads the lists of the spec as they are, from the variables the operator puts in the pod.
func TestTheAgentReadsTheListsOfTheSpec(t *testing.T) {
	lists := pathListsOfTheCorpus(t)
	for i, list := range lists {
		// Each list goes in as an include list once and as an exclude list once, beside another list.
		other := lists[(i+1)%len(lists)]
		for _, paths := range []treebindv1alpha1.PathsSpec{{Include: list, Exclude: other}, {Include: other, Exclude: list}} {
			treebind := newTreebind(testTreebindName)
			treebind.Spec.Paths = &paths

			config, env, err := agentConfigOf(t, treebind)

			if err != nil {
				t.Fatalf("the agent refuses the environment of its own pod with %q and %q: %v", paths.Include, paths.Exclude, err)
			}
			// The kubelet expands $(NAME) and $$ in the value of a variable before the agent reads it. Linux
			// takes a variable of at most 128 KiB, name and all, and the lists keep the agent's 16 KiB to spare.
			const limit, spare = 128 << 10, 16 << 10
			for _, name := range []string{agentconfig.EnvPathsInclude, agentconfig.EnvPathsExclude} {
				if strings.Contains(env[name], "$") {
					t.Errorf("%s = %s, which has a '$' that the kubelet would expand", name, env[name])
				}
				if size := len(name) + len("=") + len(env[name]) + 1; size > limit-spare {
					t.Errorf("%s is %d bytes, which leaves less than %d of the %d that it may have", name, size, spare, limit)
				}
			}
			if !slices.Equal(config.PathsInclude, paths.Include) || !slices.Equal(config.PathsExclude, paths.Exclude) {
				t.Errorf("the agent reads %q and %q, want %q and %q",
					config.PathsInclude, config.PathsExclude, paths.Include, paths.Exclude)
			}
		}
	}
	if len(lists) < 10 {
		t.Errorf("the corpus makes only %d lists", len(lists))
	}
}

// withoutLists are the ways to write a spec.paths that has no lists.
func withoutLists() []*treebindv1alpha1.PathsSpec {
	return []*treebindv1alpha1.PathsSpec{nil, {}, {Include: []string{}, Exclude: []string{}}}
}

// Without lists there are no variables, and the agent reads no patterns.
func TestNoPathsVariablesWithoutLists(t *testing.T) {
	for _, paths := range withoutLists() {
		treebind := newTreebind(testTreebindName)
		treebind.Spec.Paths = paths

		config, env, err := agentConfigOf(t, treebind)

		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{agentconfig.EnvPathsInclude, agentconfig.EnvPathsExclude} {
			if value, set := env[name]; set {
				t.Errorf("%s is %q for spec.paths %+v", name, value, paths)
			}
		}
		if len(config.PathsInclude)+len(config.PathsExclude) != 0 {
			t.Errorf("the agent reads %q and %q for spec.paths %+v", config.PathsInclude, config.PathsExclude, paths)
		}
	}
}

var _ = Describe("spec.paths", func() {
	// valid says whether the API server takes a Treebind with the lists, and why not. The dry run
	// validates it without keeping it.
	valid := func(ctx context.Context, include, exclude []string) error {
		GinkgoHelper()
		treebind := newTreebind("paths")
		treebind.Spec.Paths = &treebindv1alpha1.PathsSpec{Include: include, Exclude: exclude}
		err := k8sClient.Create(ctx, treebind, client.DryRunAll)
		if err != nil && !apierrors.IsInvalid(err) {
			Fail(fmt.Sprintf("the API server failed to judge %q and %q: %v", include, exclude, err))
		}
		return err
	}
	// agree fails when the API server and the agent do not agree on a list, in either place.
	agree := func(ctx context.Context, list []string) bool {
		GinkgoHelper()
		agentErr := agentconfig.CheckPathPatterns(list)
		for _, place := range []string{"include", "exclude"} {
			var err error
			if place == "include" {
				err = valid(ctx, list, nil)
			} else {
				err = valid(ctx, nil, list)
			}
			if (err == nil) != (agentErr == nil) {
				Fail(fmt.Sprintf("%q in spec.paths.%s: the API server takes it = %v (%v), the agent takes it = %v (%v)",
					list, place, err == nil, err, agentErr == nil, agentErr))
			}
		}
		return agentErr == nil
	}

	It("takes a pattern exactly when the agent takes it, in either list", func() {
		ctx := context.Background()
		var taken, refused int
		for _, pattern := range pathPatternCandidates() {
			// A string that is not UTF-8 would reach the API server as another one.
			Expect(utf8.ValidString(pattern)).To(BeTrue(), "%q", pattern)
			if agree(ctx, []string{pattern}) {
				taken++
			} else {
				refused++
			}
		}
		// The candidates have to hold many of both, or the check above says little.
		Expect(taken).To(BeNumerically(">", 300))
		Expect(refused).To(BeNumerically(">", 300))
	})

	It("takes a list exactly when the agent takes it, up to the limits of the agent", func() {
		ctx := context.Background()
		names := func(n int, text string) []string {
			list := make([]string, n)
			for i := range list {
				list[i] = fmt.Sprintf("%03d", i) + text[3:]
			}
			return list
		}
		long := strings.Repeat("x", 512)
		longest := agentconfig.MaxPathPatternsLength / len(long)
		lists := map[string][]string{
			"no list":                         nil,
			"the most patterns":               names(agentconfig.MaxPathPatterns, "xxx"),
			"one pattern too many":            names(agentconfig.MaxPathPatterns+1, "xxx"),
			"the longest list":                names(longest, long),
			"one character too long":          append(names(longest, long), "y"),
			"the longest list in two bytes":   names(longest, "xxx"+strings.Repeat("é", 509)),
			"a good and a bad pattern":        {pathsTmp, pathsEmptySegment},
			"the same pattern twice":          {pathsTmp, pathsTmp},
			"the longest list in many pieces": names(agentconfig.MaxPathPatterns, strings.Repeat("x", 256)),
		}
		Expect(lists["the longest list"]).To(HaveLen(longest))
		taken := map[string]bool{}
		for name, list := range lists {
			taken[name] = agree(ctx, list)
		}
		Expect(taken).To(Equal(map[string]bool{
			"no list": true, "the most patterns": true, "one pattern too many": false, "the longest list": true,
			"one character too long": false, "the longest list in two bytes": true, "a good and a bad pattern": false,
			"the same pattern twice": true, "the longest list in many pieces": true,
		}))
	})

	It("says which rule a pattern breaks", func() {
		ctx := context.Background()
		for pattern, message := range map[string]string{
			pathsAbsolute:     "patterns must be relative and must not contain '..'",
			pathsUpInside:     "patterns must be relative and must not contain '..'",
			"!keep.json":      "patterns must not start with '!'",
			pathsNUL:          "patterns must not contain a NUL",
			pathsEmptySegment: "patterns must not have an empty or '.' segment",
			"./a":             "patterns must not have an empty or '.' segment",
			"[x":              messageSyntax,
			"[[:alpha:]]":     messageSyntax,
			`b\`:              messageSyntax,
			"ab**/c":          "may have '**' only as a whole segment",
		} {
			Expect(valid(ctx, nil, []string{pattern})).To(MatchError(ContainSubstring(message)), "%q", pattern)
		}
		Expect(valid(ctx, []string{strings.Repeat("a", 513)}, nil)).To(MatchError(ContainSubstring("512")))
	})
})
