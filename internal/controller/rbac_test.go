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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Deleting a Treebind never deletes the claim or its data (FR-17). The first
// line of defence is that the operator never creates or owns a claim; this is
// the second: its roles give it no more than read access on claims and volumes,
// so a code mistake or a later permission cannot delete one.
const (
	claims     = "persistentvolumeclaims"
	volumes    = "persistentvolumes"
	verbGet    = "get"
	verbDelete = "delete"
)

var (
	volumeResources = []string{claims, volumes}
	readOnlyVerbs   = []string{verbGet, "list", "watch"}
)

type policyRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// helmDirective matches a line that holds nothing but a Helm directive.
var helmDirective = regexp.MustCompile(`^\s*\{\{.*\}\}\s*$`)

// rulesFrom parses the rules of a role file. The chart's file is a Helm
// template with directives in its header, so parsing starts at "rules:". Lines
// that hold only a directive are dropped, which makes a rule inside a
// conditional count as always present: the strict reading, for a safety check.
func rulesFrom(t *testing.T, path string) []policyRule {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)
	start := strings.Index(text, "\nrules:\n")
	if start < 0 {
		t.Fatalf("%s has no rules section", path)
	}

	var kept []string
	for line := range strings.SplitSeq(text[start:], "\n") {
		if !helmDirective.MatchString(line) {
			kept = append(kept, line)
		}
	}

	var role struct {
		Rules []policyRule `json:"rules"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &role); err != nil {
		t.Fatalf("parse the rules of %s: %v", path, err)
	}
	if len(role.Rules) == 0 {
		t.Fatalf("%s has no rules", path)
	}
	return role.Rules
}

// coversVolumes reports whether a rule can apply to claims or volumes, by name
// or through a wildcard. Only the core API group holds them.
func coversVolumes(rule policyRule) bool {
	if !slices.Contains(rule.APIGroups, "") && !slices.Contains(rule.APIGroups, "*") {
		return false
	}
	return slices.ContainsFunc(rule.Resources, func(resource string) bool {
		base, _, _ := strings.Cut(resource, "/")
		return base == "*" || slices.Contains(volumeResources, base)
	})
}

// volumeWriteRules returns the rules that grant more than read access on claims
// or volumes. A wildcard verb counts as a write.
func volumeWriteRules(rules []policyRule) []policyRule {
	var violations []policyRule
	for _, rule := range rules {
		if !coversVolumes(rule) {
			continue
		}
		if slices.ContainsFunc(rule.Verbs, func(verb string) bool { return !slices.Contains(readOnlyVerbs, verb) }) {
			violations = append(violations, rule)
		}
	}
	return violations
}

func TestOperatorRolesGiveNoWriteAccessToVolumes(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "rbac", "role.yaml"),
		filepath.Join("..", "..", "charts", "treebind", "templates", "rbac", "manager-role.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			if violations := volumeWriteRules(rulesFrom(t, path)); len(violations) > 0 {
				t.Errorf("the operator may write to claims or volumes, so deleting a Treebind could lose data: %+v",
					violations)
			}
		})
	}
}

// The check above passes when no rule exists, so this keeps it from passing
// because the detector is broken.
func TestVolumeWriteRules(t *testing.T) {
	tests := []struct {
		name string
		rule policyRule
		want bool
	}{
		{"no rule on volumes", policyRule{[]string{"apps"}, []string{"statefulsets"}, []string{verbDelete}}, false},
		{"read access on claims", policyRule{[]string{""}, []string{claims}, []string{verbGet, "list", "watch"}}, false},
		{"read access on volumes", policyRule{[]string{""}, []string{volumes}, []string{verbGet}}, false},
		{"delete on claims", policyRule{[]string{""}, []string{claims}, []string{verbGet, verbDelete}}, true},
		{"patch on claim status", policyRule{[]string{""}, []string{claims + "/status"}, []string{"patch"}}, true},
		{"create on volumes", policyRule{[]string{""}, []string{volumes}, []string{"create"}}, true},
		{"wildcard verb on claims", policyRule{[]string{""}, []string{claims}, []string{"*"}}, true},
		{"patch on the status of every core resource", policyRule{[]string{""}, []string{"*/status"}, []string{"patch"}}, true},
		{"delete on every core resource", policyRule{[]string{""}, []string{"*"}, []string{verbDelete}}, true},
		{"delete on every resource of every group", policyRule{[]string{"*"}, []string{"*"}, []string{verbDelete}}, true},
		{"delete on every resource of another group", policyRule{[]string{"apps"}, []string{"*"}, []string{verbDelete}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(volumeWriteRules([]policyRule{tt.rule})) > 0; got != tt.want {
				t.Errorf("volumeWriteRules(%+v) flags a write = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}
