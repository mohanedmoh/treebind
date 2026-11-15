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

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const (
	problemJSON    = "must be a JSON array of strings"
	problemPattern = `has a pattern, "a//b", that must not have an empty or '.' segment`
	problemTooMany = "must have at most 64 patterns"
	problemTooLong = "must have at most 16384 characters in all"
)

// patternsOf returns a list of patterns as the operator writes it into the pod.
func patternsOf(t *testing.T, patterns ...string) string {
	t.Helper()
	b, err := json.Marshal(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// repeated returns n patterns of the same text, each made different by its first
// character, so that the length of the list is n times that of the text.
func repeated(n int, text string) []string {
	list := make([]string, n)
	for i := range list {
		list[i] = string(rune('a'+i%26)) + string([]rune(text)[1:])
	}
	return list
}

func TestFromEnvTakesThePatternsOfSpecPaths(t *testing.T) {
	env := complete()
	include := []string{"dashboards/", "*.json", "a b", "é/**"}
	exclude := []string{"*.bak", `\!keep`}
	env[EnvPathsInclude] = patternsOf(t, include...)
	// A value from a manifest can carry a newline after it.
	env[EnvPathsExclude] = patternsOf(t, exclude...) + "\n"

	config, err := fromMap(env)

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.PathsInclude, include) || !slices.Equal(config.PathsExclude, exclude) {
		t.Errorf("the agent reads %q and %q, want %q and %q", config.PathsInclude, config.PathsExclude, include, exclude)
	}
}

func TestFromEnvHasNoPatternsWithoutThem(t *testing.T) {
	for _, raw := range []string{"", "[]", "null", " [ ] "} {
		env := complete()
		env[EnvPathsInclude], env[EnvPathsExclude] = raw, raw

		config, err := fromMap(env)

		if err != nil || len(config.PathsInclude) != 0 || len(config.PathsExclude) != 0 {
			t.Errorf("%q: the agent reads %q and %q, %v; want no patterns", raw, config.PathsInclude, config.PathsExclude, err)
		}
	}
}

func TestFromEnvRefusesPatternsThatCannotBeOnes(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"a pattern that is not JSON", "dashboards/", problemJSON},
		{"one string", `"*.tmp"`, problemJSON},
		{"an object", `{"include": ["*.tmp"]}`, problemJSON},
		{"numbers", `[1, 2]`, problemJSON},
		{"something after the list", `["*.tmp"] ["*.swp"]`, problemJSON},
		{"a byte that is not UTF-8", "[\"\xff\"]", "must be UTF-8"},
		{"a pattern that paths.Check refuses", patternsOf(t, "*.tmp", "a//b"), problemPattern},
		{"an empty pattern", patternsOf(t, ""), `has a pattern, "", that must not be empty`},
		{"too many patterns", patternsOf(t, repeated(MaxPathPatterns+1, "x")...), problemTooMany},
		{"patterns too long in all", patternsOf(t, repeated(33, strings.Repeat("x", 512))...), problemTooLong},
	}
	for _, name := range []string{EnvPathsInclude, EnvPathsExclude} {
		for _, tt := range tests {
			t.Run(name+" with "+tt.name, func(t *testing.T) {
				env := complete()
				env[name] = tt.raw
				env[EnvNamespace] = ""

				config, err := fromMap(env)

				if err == nil || !strings.Contains(err.Error(), name+" "+tt.want) {
					t.Errorf("got %v, want a problem that says %s %s", err, name, tt.want)
				}
				if err == nil || !strings.Contains(err.Error(), EnvNamespace+" is not set") {
					t.Errorf("got %v, want the other problems too", err)
				}
				if len(config.PathsInclude)+len(config.PathsExclude) != 0 {
					t.Errorf("the agent took %q and %q from a list it refuses", config.PathsInclude, config.PathsExclude)
				}
			})
		}
	}
}

func TestCheckPathPatternsTakesTheLongestListsTheCRDTakes(t *testing.T) {
	for _, list := range [][]string{
		nil,
		repeated(MaxPathPatterns, "x"),
		repeated(MaxPathPatternsLength/512, strings.Repeat("x", 512)),
		// The length is in characters, as the CRD counts it, and not in bytes.
		repeated(MaxPathPatternsLength/512, strings.Repeat("é", 512)),
	} {
		if err := CheckPathPatterns(list); err != nil {
			t.Errorf("CheckPathPatterns of %d patterns: %v", len(list), err)
		}
	}
	if err := CheckPathPatterns(append(repeated(MaxPathPatternsLength/512, strings.Repeat("x", 512)), "y")); err == nil ||
		!strings.Contains(err.Error(), problemTooLong+", and has 16385") {
		t.Errorf("CheckPathPatterns of a list one character too long = %v", err)
	}
}

// The longest list, all in a character that JSON writes in six bytes, the most it writes
// any in, fits in one environment variable, which Linux limits to 128 KiB, name and all.
func TestTheLongestListFitsInAnEnvironmentVariable(t *testing.T) {
	const limit, spare = 128 << 10, 16 << 10
	list := slices.Repeat([]string{strings.Repeat("\x01", 512)}, MaxPathPatternsLength/512)
	if err := CheckPathPatterns(list); err != nil {
		t.Fatal(err)
	}
	if size := len(EnvPathsExclude) + len("=") + len(patternsOf(t, list...)) + 1; size > limit-spare {
		t.Errorf("the variable is %d bytes, which leaves less than %d of the %d that it may have", size, spare, limit)
	}
}
