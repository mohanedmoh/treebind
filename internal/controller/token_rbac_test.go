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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The operator asks the API server for tokens of its own service account, to call
// the sync agents with (docs/adr/0015). A token is a credential, so the permission
// to ask for one is as small as it can be: a Role in the operator's own namespace
// that allows create on the token of that one service account. Cluster-wide it
// would let the operator mint a token for any service account, which is to say
// become any of them.

const (
	serviceAccountsResource = "serviceaccounts"
	tokenSubresource        = "token"
	tokenResource           = serviceAccountsResource + "/" + tokenSubresource

	// foreignGroup is an API group that none of the operator's rules is for.
	foreignGroup = "example.org"

	kindRole           = "Role"
	kindRoleBinding    = "RoleBinding"
	kindServiceAccount = "ServiceAccount"
	verbMint           = "create"
)

// tokenGrant holds what the permission to mint tokens consists of, from one of the
// two copies.
type tokenGrant struct {
	role               rbacv1.Role
	binding            rbacv1.RoleBinding
	serviceAccountName string
	namespace          string
}

func configTokenGrant(t *testing.T) tokenGrant {
	t.Helper()
	deployment := configDeployment(t)
	return tokenGrant{
		role: parse[rbacv1.Role](t, "config/rbac/token_role.yaml",
			configManifest(t, "Role", "rbac", "token_role.yaml")),
		binding: parse[rbacv1.RoleBinding](t, "config/rbac/token_role_binding.yaml",
			configManifest(t, "RoleBinding", "rbac", "token_role_binding.yaml")),
		serviceAccountName: deployment.Spec.Template.Spec.ServiceAccountName,
		namespace:          deployment.Metadata.Namespace,
	}
}

func chartTokenGrant(t *testing.T) tokenGrant {
	t.Helper()
	deployment := chartDeployment(t, defaultValues(t))
	none := strings.NewReplacer()
	return tokenGrant{
		role: parse[rbacv1.Role](t, "the token Role of the chart",
			flattenTemplate(t, none, "charts", "treebind", "templates", "rbac", "token-role.yaml")),
		binding: parse[rbacv1.RoleBinding](t, "the token RoleBinding of the chart",
			flattenTemplate(t, none, "charts", "treebind", "templates", "rbac", "token-rolebinding.yaml")),
		serviceAccountName: deployment.Spec.Template.Spec.ServiceAccountName,
		namespace:          deployment.Metadata.Namespace,
	}
}

func TestTokenRoleIsNamespacedAndLimitedToTheOperatorsOwnServiceAccount(t *testing.T) {
	sources := []struct {
		name  string
		grant func(*testing.T) tokenGrant
	}{
		{"config/rbac", configTokenGrant},
		{fromChart, chartTokenGrant},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			grant := source.grant(t)

			if grant.role.Kind != kindRole || grant.binding.Kind != kindRoleBinding {
				t.Errorf("the permission is granted through a %s and a %s, want a Role and a RoleBinding: "+
					"a cluster-wide grant would let the operator mint a token for any service account",
					grant.role.Kind, grant.binding.Kind)
			}

			if len(grant.role.Rules) != 1 {
				t.Fatalf("the Role has %d rules, want the one that allows the operator's own token: %+v",
					len(grant.role.Rules), grant.role.Rules)
			}
			rule := grant.role.Rules[0]
			if !slices.Equal(rule.APIGroups, []string{""}) ||
				!slices.Equal(rule.Resources, []string{tokenResource}) ||
				!slices.Equal(rule.Verbs, []string{verbMint}) {
				t.Errorf("the rule is %v %v on %v, want create on serviceaccounts/token of the core group",
					rule.Verbs, rule.Resources, rule.APIGroups)
			}
			if !slices.Equal(rule.ResourceNames, []string{grant.serviceAccountName}) {
				t.Errorf("the rule is limited to %v, want only the service account that the operator runs as, %q: "+
					"without a name it covers every service account of the namespace",
					rule.ResourceNames, grant.serviceAccountName)
			}

			if grant.binding.RoleRef.Kind != kindRole || grant.binding.RoleRef.Name != grant.role.Name {
				t.Errorf("the RoleBinding refers to %s %q, not to the Role %q",
					grant.binding.RoleRef.Kind, grant.binding.RoleRef.Name, grant.role.Name)
			}
			if len(grant.binding.Subjects) != 1 {
				t.Fatalf("the RoleBinding has %d subjects, want the operator's service account only",
					len(grant.binding.Subjects))
			}
			subject := grant.binding.Subjects[0]
			if subject.Kind != kindServiceAccount || subject.Name != grant.serviceAccountName ||
				subject.Namespace != grant.namespace {
				t.Errorf("the RoleBinding grants %s %s/%s, but the operator runs as the service account %s/%s",
					subject.Kind, subject.Namespace, subject.Name, grant.namespace, grant.serviceAccountName)
			}
		})
	}
}

func TestChartTokenRoleMatchesConfig(t *testing.T) {
	// The name of the service account is a different text in each copy, and each is
	// checked against its own Deployment above.
	withoutNames := func(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
		result := make([]rbacv1.PolicyRule, len(rules))
		for i, rule := range rules {
			result[i] = *rule.DeepCopy()
			result[i].ResourceNames = []string{"OPERATOR"}
		}
		return result
	}
	sameInChart(t, "the rules of the token Role",
		withoutNames(configTokenGrant(t).role.Rules), withoutNames(chartTokenGrant(t).role.Rules))
}

// kustomize puts the name prefix on the name of the service account and of the
// Role, but not on the resourceNames of a rule, so without a replacement the Role
// would allow the token of a service account that does not exist, and the operator
// could never mint one. Nothing fails until the operator runs in a cluster, so this
// holds the replacement in place.
func TestKustomizeNamesTheOperatorsServiceAccountInTheTokenRule(t *testing.T) {
	var kustomization struct {
		Replacements []struct {
			Source struct {
				Kind      string `json:"kind"`
				Name      string `json:"name"`
				FieldPath string `json:"fieldPath"`
			} `json:"source"`
			Targets []struct {
				Select struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"select"`
				FieldPaths []string `json:"fieldPaths"`
			} `json:"targets"`
		} `json:"replacements"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "config", "default", "kustomization.yaml")), &kustomization); err != nil {
		t.Fatal(err)
	}
	grant := configTokenGrant(t)

	for _, replacement := range kustomization.Replacements {
		if replacement.Source.Kind != kindServiceAccount || replacement.Source.Name != grant.serviceAccountName ||
			replacement.Source.FieldPath != "metadata.name" {
			continue
		}
		for _, target := range replacement.Targets {
			if target.Select.Kind == kindRole && target.Select.Name == grant.role.Name &&
				slices.Contains(target.FieldPaths, "rules.0.resourceNames.0") {
				return
			}
		}
	}
	t.Errorf("config/default/kustomization.yaml does not copy the name of the service account %q into "+
		"rules.0.resourceNames.0 of the Role %q: the prefix is not added to resourceNames, so the Role would "+
		"allow the token of a service account that does not exist", grant.serviceAccountName, grant.role.Name)
}

// coversServiceAccountTokens reports whether a rule lets its subject create a token
// of a service account, by name or through a wildcard.
func coversServiceAccountTokens(rule policyRule) bool {
	if !slices.Contains(rule.APIGroups, "") && !slices.Contains(rule.APIGroups, "*") {
		return false
	}
	if !slices.Contains(rule.Verbs, verbMint) && !slices.Contains(rule.Verbs, "*") {
		return false
	}
	return slices.ContainsFunc(rule.Resources, func(resource string) bool {
		base, sub, hasSub := strings.Cut(resource, "/")
		if !hasSub {
			return base == "*"
		}
		return (base == "*" || base == serviceAccountsResource) && (sub == "*" || sub == tokenSubresource)
	})
}

// The operator's cluster role must not grow a rule that covers tokens: the one it
// needs is the Role above, limited to its own service account.
func TestOperatorClusterRolesCannotMintTokens(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "rbac", "role.yaml"),
		filepath.Join("..", "..", "charts", "treebind", "templates", "rbac", "manager-role.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			for _, rule := range rulesFrom(t, path) {
				if coversServiceAccountTokens(rule) {
					t.Errorf("the cluster role lets the operator create tokens of any service account: %+v", rule)
				}
			}
		})
	}
}

// The check above passes when no rule exists, so this keeps it from passing
// because the detector is broken.
func TestCoversServiceAccountTokens(t *testing.T) {
	tests := []struct {
		name string
		rule policyRule
		want bool
	}{
		{"create on the token", policyRule{[]string{""}, []string{tokenResource}, []string{verbMint}}, true},
		{"every verb on the token", policyRule{[]string{""}, []string{tokenResource}, []string{"*"}}, true},
		{"create on every subresource of service accounts", policyRule{[]string{""}, []string{"serviceaccounts/*"}, []string{verbMint}}, true},
		{"create on the token of every resource", policyRule{[]string{""}, []string{"*/token"}, []string{verbMint}}, true},
		{"create on everything", policyRule{[]string{""}, []string{"*"}, []string{verbMint}}, true},
		{"create on everything of every group", policyRule{[]string{"*"}, []string{"*"}, []string{"*"}}, true},
		{"get on the token", policyRule{[]string{""}, []string{tokenResource}, []string{"get"}}, false},
		{"create on service accounts but not their token", policyRule{[]string{""}, []string{"serviceaccounts"}, []string{verbMint}}, false},
		{"create on another subresource", policyRule{[]string{""}, []string{"serviceaccounts/status"}, []string{verbMint}}, false},
		{"create on the token of pods", policyRule{[]string{""}, []string{"pods/token"}, []string{verbMint}}, false},
		{"create on the token in another group", policyRule{[]string{foreignGroup}, []string{tokenResource}, []string{verbMint}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coversServiceAccountTokens(tt.rule); got != tt.want {
				t.Errorf("coversServiceAccountTokens(%+v) = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}
