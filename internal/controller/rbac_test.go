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
	verbList   = "list"
	verbWatch  = "watch"
	verbDelete = "delete"

	// appsGroup is the API group of the StatefulSets the operator manages.
	appsGroup = "apps"
)

var (
	volumeResources = []string{claims, volumes}
	readOnlyVerbs   = []string{verbGet, verbList, verbWatch}
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
		{"no rule on volumes", policyRule{[]string{appsGroup}, []string{"statefulsets"}, []string{verbDelete}}, false},
		{"read access on claims", policyRule{[]string{""}, []string{claims}, []string{verbGet, verbList, verbWatch}}, false},
		{"read access on volumes", policyRule{[]string{""}, []string{volumes}, []string{verbGet}}, false},
		{"delete on claims", policyRule{[]string{""}, []string{claims}, []string{verbGet, verbDelete}}, true},
		{"patch on claim status", policyRule{[]string{""}, []string{claims + "/status"}, []string{"patch"}}, true},
		{"create on volumes", policyRule{[]string{""}, []string{volumes}, []string{"create"}}, true},
		{"wildcard verb on claims", policyRule{[]string{""}, []string{claims}, []string{"*"}}, true},
		{"patch on the status of every core resource", policyRule{[]string{""}, []string{"*/status"}, []string{"patch"}}, true},
		{"delete on every core resource", policyRule{[]string{""}, []string{"*"}, []string{verbDelete}}, true},
		{"delete on every resource of every group", policyRule{[]string{"*"}, []string{"*"}, []string{verbDelete}}, true},
		{"delete on every resource of another group", policyRule{[]string{appsGroup}, []string{"*"}, []string{verbDelete}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(volumeWriteRules([]policyRule{tt.rule})) > 0; got != tt.want {
				t.Errorf("volumeWriteRules(%+v) flags a write = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}

// The operator keeps the public keys the agents check its token against in a
// ConfigMap per Treebind (docs/adr/0015). Creating ConfigMaps and patching its own
// is all it may do with them: it never reads one, so a ConfigMap of someone else
// stays unread, and it never deletes one, so the garbage collector does that. It
// never reads a Secret either (NFR-21), so no rule may reach Secrets at all.
const (
	configMaps = "configmaps"
	secrets    = "secrets"
	verbCreate = "create"
	verbPatch  = "patch"
)

// coversCoreResource reports whether a rule can apply to the named core resource,
// by name or through a wildcard, including its subresources.
func coversCoreResource(rule policyRule, resource string) bool {
	if !slices.Contains(rule.APIGroups, "") && !slices.Contains(rule.APIGroups, "*") {
		return false
	}
	return slices.ContainsFunc(rule.Resources, func(candidate string) bool {
		base, _, _ := strings.Cut(candidate, "/")
		return base == "*" || base == resource
	})
}

// configMapAndSecretViolations returns the rules that give more than create and
// patch on ConfigMaps, or any access to Secrets. A wildcard verb counts as more.
func configMapAndSecretViolations(rules []policyRule) []policyRule {
	var violations []policyRule
	for _, rule := range rules {
		if coversCoreResource(rule, secrets) {
			violations = append(violations, rule)
			continue
		}
		if coversCoreResource(rule, configMaps) &&
			slices.ContainsFunc(rule.Verbs, func(verb string) bool { return verb != verbCreate && verb != verbPatch }) {
			violations = append(violations, rule)
		}
	}
	return violations
}

func TestOperatorRolesGiveConfigMapsOnlyCreateAndPatchAndSecretsNothing(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "rbac", "role.yaml"),
		filepath.Join("..", "..", "charts", "treebind", "templates", "rbac", "manager-role.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			rules := rulesFrom(t, path)
			if violations := configMapAndSecretViolations(rules); len(violations) > 0 {
				t.Errorf("the operator may read or change more than it needs to: %+v", violations)
			}
			// The check above passes when the rule is missing, and then the operator could
			// not publish its keys.
			if !slices.ContainsFunc(rules, func(rule policyRule) bool {
				return slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, configMaps) &&
					slices.Contains(rule.Verbs, verbCreate) && slices.Contains(rule.Verbs, verbPatch)
			}) {
				t.Error("the operator may not create and patch ConfigMaps, so it cannot publish the keys")
			}
		})
	}
}

// The check above passes when no rule exists, so this keeps it from passing
// because the detector is broken.
func TestConfigMapAndSecretViolations(t *testing.T) {
	tests := []struct {
		name string
		rule policyRule
		want bool
	}{
		{"create and patch on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{verbCreate, verbPatch}}, false},
		{"no rule on either", policyRule{[]string{appsGroup}, []string{"statefulsets"}, []string{verbDelete}}, false},
		{"get on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{verbCreate, verbPatch, verbGet}}, true},
		{"list on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{verbList}}, true},
		{"watch on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{verbWatch}}, true},
		{"update on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{"update"}}, true},
		{"delete on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{verbDelete}}, true},
		{"a wildcard verb on ConfigMaps", policyRule{[]string{""}, []string{configMaps}, []string{"*"}}, true},
		{"get on every core resource", policyRule{[]string{""}, []string{"*"}, []string{verbGet}}, true},
		{"get on every resource of every group", policyRule{[]string{"*"}, []string{"*"}, []string{verbGet}}, true},
		{"get on a ConfigMap subresource", policyRule{[]string{""}, []string{configMaps + "/status"}, []string{verbGet}}, true},
		{"get on Secrets", policyRule{[]string{""}, []string{secrets}, []string{verbGet}}, true},
		{"create on Secrets", policyRule{[]string{""}, []string{secrets}, []string{verbCreate}}, true},
		{"get on ConfigMaps of another group", policyRule{[]string{"example.org"}, []string{configMaps}, []string{verbGet}}, false},
		{"get on Secrets of another group", policyRule{[]string{"example.org"}, []string{secrets}, []string{verbGet}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(configMapAndSecretViolations([]policyRule{tt.rule})) > 0; got != tt.want {
				t.Errorf("configMapAndSecretViolations(%+v) flags a violation = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}
