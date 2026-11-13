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
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The operator runs as two replicas with leader election (NFR-21), and the other
// settings that make two replicas work: a budget that lets a drain evict one at a
// time, a rolling update that keeps both ready, and a spread over nodes. The
// manifests in config/ and the chart each carry their own copy, so these checks
// hold the two together and check that each copy is wired up.

const (
	nameLabel       = "app.kubernetes.io/name"
	leaderElectFlag = "--leader-elect"
	// minReplicas is the least that gives the operator a standby.
	minReplicas = 2
	// sentinel is a number no default uses, to see that a value reaches the template.
	sentinel = 7

	leasesGroup    = "coordination.k8s.io"
	leasesResource = "leases"

	// The two copies that the tests compare, as they are named in the output.
	fromConfig = "config/manager"
	fromChart  = "the chart"
)

// chartBudget reads the fixed text of the chart's PodDisruptionBudget template, with
// the number it takes from the values set to maxUnavailable.
func chartBudget(t *testing.T, maxUnavailable int) policyv1.PodDisruptionBudget {
	t.Helper()
	numbers := strings.NewReplacer("{{ .Values.manager.podDisruptionBudget.maxUnavailable }}", fmt.Sprint(maxUnavailable))
	return parse[policyv1.PodDisruptionBudget](t, "the PodDisruptionBudget template of the chart",
		flattenTemplate(t, numbers, "charts", "treebind", "templates", "manager", "pod-disruption-budget.yaml"))
}

func TestOperatorRunsTwoReplicasWithLeaderElection(t *testing.T) {
	config := configDeployment(t)
	values := defaultValues(t)

	tests := []struct {
		name     string
		replicas int32
		args     []string
	}{
		{fromConfig, ptrValue(config.Spec.Replicas),
			container(t, "config/manager/manager.yaml", config.Spec.Template.Spec.Containers).Args},
		{fromChart, values.Manager.Replicas, values.Manager.Args},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.replicas < minReplicas {
				t.Errorf("runs %d replica(s) by default, want at least %d so that a standby can take over",
					tt.replicas, minReplicas)
			}
			if !slices.Contains(tt.args, leaderElectFlag) {
				t.Errorf("does not pass %s, so every replica would reconcile at once; it passes %v",
					leaderElectFlag, tt.args)
			}
		})
	}
}

// The chart takes the argument from a list that a user can replace, so with two
// replicas it has to refuse a list without the argument. A test cannot render the
// template, so this checks that the guard is there and names the argument.
func TestChartRefusesReplicasWithoutLeaderElection(t *testing.T) {
	template := readRepoFile(t, "charts", "treebind", "templates", "manager", "manager.yaml")
	for _, part := range []string{"fail ", fmt.Sprintf("has %q .Values.manager.args", leaderElectFlag),
		"gt (int .Values.manager.replicas) 1"} {
		if !strings.Contains(template, part) {
			t.Errorf("the Deployment template of the chart has no guard that refuses several replicas "+
				"without %s: it does not contain %q", leaderElectFlag, part)
		}
	}
}

func ptrValue[T any](pointer *T) T {
	var zero T
	if pointer == nil {
		return zero
	}
	return *pointer
}

// availability holds the fields of the Deployment that the chart takes from its
// values, so that they are read from config/manager only.
type availability struct {
	Spec struct {
		Strategy appsv1.DeploymentStrategy `json:"strategy"`
		Template struct {
			Spec struct {
				TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func configAvailability(t *testing.T) availability {
	t.Helper()
	return parse[availability](t, "the Deployment in config/manager/manager.yaml",
		configManifest(t, "Deployment", "manager", "manager.yaml"))
}

// withoutName replaces the value of the name label in the selectors, because the
// chart takes it from an expression and config/manager spells it out. The selector
// test below compares the label with the one the pods carry.
func withoutName(constraints []corev1.TopologySpreadConstraint) []corev1.TopologySpreadConstraint {
	result := make([]corev1.TopologySpreadConstraint, len(constraints))
	for i, constraint := range constraints {
		result[i] = *constraint.DeepCopy()
		if selector := result[i].LabelSelector; selector != nil && selector.MatchLabels != nil {
			selector.MatchLabels[nameLabel] = "NAME"
		}
	}
	return result
}

func TestChartRolloutAndSpreadMatchConfig(t *testing.T) {
	config := configAvailability(t)
	values := defaultValues(t)

	if len(config.Spec.Template.Spec.TopologySpreadConstraints) == 0 || len(values.Manager.TopologySpreadConstraints) == 0 {
		t.Fatal("config/manager and the chart no longer both set a topology spread constraint")
	}
	sameInChart(t, "the rolling update strategy", config.Spec.Strategy, values.Manager.Strategy)
	sameInChart(t, "the topology spread constraints",
		withoutName(config.Spec.Template.Spec.TopologySpreadConstraints), withoutName(values.Manager.TopologySpreadConstraints))
}

// The spread must stay soft: a required rule leaves the second replica Pending on a
// cluster with one node, and the surge pod of an update Pending on one with two.
func TestSpreadConstraintsAreSoft(t *testing.T) {
	tests := []struct {
		name        string
		constraints []corev1.TopologySpreadConstraint
	}{
		{fromConfig, configAvailability(t).Spec.Template.Spec.TopologySpreadConstraints},
		{fromChart, defaultValues(t).Manager.TopologySpreadConstraints},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.constraints) == 0 {
				t.Fatal("sets no topology spread constraint")
			}
			for _, constraint := range tt.constraints {
				if constraint.WhenUnsatisfiable != corev1.ScheduleAnyway {
					t.Errorf("a constraint on %s is %q, which can leave a replica Pending; want %q",
						constraint.TopologyKey, constraint.WhenUnsatisfiable, corev1.ScheduleAnyway)
				}
				if !slices.Contains(constraint.MatchLabelKeys, "pod-template-hash") {
					t.Errorf("a constraint on %s counts the pods of every revision, so an update can leave both "+
						"replicas on one node; it names the match label keys %v", constraint.TopologyKey, constraint.MatchLabelKeys)
				}
			}
		})
	}
}

func TestChartPodDisruptionBudgetMatchesConfig(t *testing.T) {
	values := defaultValues(t)
	config := parse[policyv1.PodDisruptionBudget](t, "config/manager/pod_disruption_budget.yaml",
		configManifest(t, "PodDisruptionBudget", "manager", "pod_disruption_budget.yaml"))

	chart := chartBudget(t, int(values.Manager.PodDisruptionBudget.MaxUnavailable))

	if !values.Manager.PodDisruptionBudget.Enabled {
		t.Error("the chart does not install the PodDisruptionBudget by default")
	}
	if config.Spec.MaxUnavailable == nil || config.Spec.MaxUnavailable.IntValue() < 1 {
		t.Error("config/manager allows no voluntary disruption, so a drain would wait for ever")
	}
	sameInChart(t, "the number of replicas that may be unavailable", config.Spec.MaxUnavailable, chart.Spec.MaxUnavailable)
	sameInChart(t, "the unhealthy pod eviction policy", config.Spec.UnhealthyPodEvictionPolicy, chart.Spec.UnhealthyPodEvictionPolicy)

	// The default above matches by luck if the template ignores its value.
	if moved := chartBudget(t, sentinel); moved.Spec.MaxUnavailable == nil || moved.Spec.MaxUnavailable.IntValue() != sentinel {
		t.Errorf("the chart value manager.podDisruptionBudget.maxUnavailable does not reach the template: "+
			"set to %d it renders %s", sentinel, asJSON(moved.Spec.MaxUnavailable))
	}
	if !strings.Contains(readRepoFile(t, "charts", "treebind", "templates", "manager", "pod-disruption-budget.yaml"),
		".Values.manager.podDisruptionBudget.enabled") {
		t.Error("the PodDisruptionBudget template does not follow manager.podDisruptionBudget.enabled")
	}
}

// kustomizationResources lists the files that a kustomization under config/ includes.
func kustomizationResources(t *testing.T, dir string) []string {
	t.Helper()
	kustomization := parse[struct {
		Resources []string `json:"resources"`
	}](t, "config/"+dir+"/kustomization.yaml", []byte(readRepoFile(t, "config", dir, "kustomization.yaml")))
	return kustomization.Resources
}

// A manifest that no kustomization lists is never deployed, whatever else is right.
func TestConfigDeploysTheAvailabilityObjects(t *testing.T) {
	tests := []struct {
		dir  string
		file string
	}{
		{"manager", "pod_disruption_budget.yaml"},
		{"rbac", "leader_election_role.yaml"},
		{"rbac", "leader_election_role_binding.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			if resources := kustomizationResources(t, tt.dir); !slices.Contains(resources, tt.file) {
				t.Errorf("config/%s/kustomization.yaml does not list %s; it lists %v", tt.dir, tt.file, resources)
			}
		})
	}
}

// managerSelectors holds the label selectors that have to find the operator pods,
// and the labels of those pods.
type managerSelectors struct {
	podLabels  map[string]string
	deployment metav1.LabelSelector
	budget     metav1.LabelSelector
	spread     []metav1.LabelSelector
}

func configSelectors(t *testing.T) managerSelectors {
	t.Helper()
	deployment := configDeployment(t)
	budget := parse[policyv1.PodDisruptionBudget](t, "config/manager/pod_disruption_budget.yaml",
		configManifest(t, "PodDisruptionBudget", "manager", "pod_disruption_budget.yaml"))

	result := managerSelectors{
		podLabels:  deployment.Spec.Template.Metadata.Labels,
		deployment: deployment.Spec.Selector,
	}
	if budget.Spec.Selector != nil {
		result.budget = *budget.Spec.Selector
	}
	for _, constraint := range configAvailability(t).Spec.Template.Spec.TopologySpreadConstraints {
		if constraint.LabelSelector != nil {
			result.spread = append(result.spread, *constraint.LabelSelector)
		}
	}
	return result
}

func chartSelectors(t *testing.T) managerSelectors {
	t.Helper()
	values := defaultValues(t)
	deployment := chartDeployment(t, values)
	budget := chartBudget(t, 1)

	result := managerSelectors{
		podLabels:  deployment.Spec.Template.Metadata.Labels,
		deployment: deployment.Spec.Selector,
	}
	if budget.Spec.Selector != nil {
		result.budget = *budget.Spec.Selector
	}
	// The default constraints go through tpl in the chart, so their label values may
	// hold the same expressions as the template, which the placeholder makes comparable.
	for _, constraint := range values.Manager.TopologySpreadConstraints {
		if constraint.LabelSelector == nil {
			continue
		}
		selector := constraint.LabelSelector.DeepCopy()
		for key, value := range selector.MatchLabels {
			selector.MatchLabels[key] = inlineExpression.ReplaceAllStringFunc(value, placeholder)
		}
		result.spread = append(result.spread, *selector)
	}
	return result
}

// A budget with no labels selects every pod in the namespace, and one with a label
// the pods do not carry selects none, which is a budget that protects nothing and
// says nothing. The same holds for the spread. Each selector has to be the one the
// Deployment uses, and that one has to select the pods the Deployment creates.
func TestSelectorsFindTheManagerPods(t *testing.T) {
	sources := []struct {
		name      string
		selectors func(*testing.T) managerSelectors
	}{
		{fromConfig, configSelectors},
		{fromChart, chartSelectors},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			selectors := source.selectors(t)

			if len(selectors.deployment.MatchLabels) == 0 {
				t.Fatal("the Deployment selects pods by no label")
			}
			if selector, err := metav1.LabelSelectorAsSelector(&selectors.deployment); err != nil {
				t.Errorf("the Deployment selector is invalid: %v", err)
			} else if !selector.Matches(labels.Set(selectors.podLabels)) {
				t.Errorf("the Deployment selector %v does not select its own pods, which carry %v",
					selectors.deployment.MatchLabels, selectors.podLabels)
			}

			if !maps.Equal(selectors.budget.MatchLabels, selectors.deployment.MatchLabels) {
				t.Errorf("the PodDisruptionBudget selects %v, but the Deployment selects %v",
					selectors.budget.MatchLabels, selectors.deployment.MatchLabels)
			}
			if len(selectors.spread) == 0 {
				t.Error("no topology spread constraint has a label selector, so none counts any pod")
			}
			for _, selector := range selectors.spread {
				if !maps.Equal(selector.MatchLabels, selectors.deployment.MatchLabels) {
					t.Errorf("a topology spread constraint counts the pods with %v, but the Deployment selects %v",
						selector.MatchLabels, selectors.deployment.MatchLabels)
				}
			}
		})
	}
}

// leaderElection holds what leader election needs, from one of the two copies.
type leaderElection struct {
	role               rbacv1.Role
	binding            rbacv1.RoleBinding
	serviceAccountName string
	namespace          string
}

func configLeaderElection(t *testing.T) leaderElection {
	t.Helper()
	deployment := configDeployment(t)
	return leaderElection{
		role: parse[rbacv1.Role](t, "config/rbac/leader_election_role.yaml",
			configManifest(t, "Role", "rbac", "leader_election_role.yaml")),
		binding: parse[rbacv1.RoleBinding](t, "config/rbac/leader_election_role_binding.yaml",
			configManifest(t, "RoleBinding", "rbac", "leader_election_role_binding.yaml")),
		serviceAccountName: deployment.Spec.Template.Spec.ServiceAccountName,
		namespace:          deployment.Metadata.Namespace,
	}
}

func chartLeaderElection(t *testing.T) leaderElection {
	t.Helper()
	deployment := chartDeployment(t, defaultValues(t))
	none := strings.NewReplacer()
	return leaderElection{
		role: parse[rbacv1.Role](t, "the leader election Role of the chart",
			flattenTemplate(t, none, "charts", "treebind", "templates", "rbac", "leader-election-role.yaml")),
		binding: parse[rbacv1.RoleBinding](t, "the leader election RoleBinding of the chart",
			flattenTemplate(t, none, "charts", "treebind", "templates", "rbac", "leader-election-rolebinding.yaml")),
		serviceAccountName: deployment.Spec.Template.Spec.ServiceAccountName,
		namespace:          deployment.Metadata.Namespace,
	}
}

// leaseVerbs returns the verbs that a set of rules grants on leases.
func leaseVerbs(rules []rbacv1.PolicyRule) []string {
	var verbs []string
	for _, rule := range rules {
		if coversLeases(rule.APIGroups, rule.Resources) {
			verbs = append(verbs, rule.Verbs...)
		}
	}
	return verbs
}

func coversLeases(groups, resources []string) bool {
	return (slices.Contains(groups, leasesGroup) || slices.Contains(groups, "*")) &&
		(slices.Contains(resources, leasesResource) || slices.Contains(resources, "*"))
}

func TestLeaderElectionRoleIsNamespacedAndBoundToTheOperator(t *testing.T) {
	sources := []struct {
		name     string
		election func(*testing.T) leaderElection
	}{
		{"config/rbac", configLeaderElection},
		{fromChart, chartLeaderElection},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			election := source.election(t)

			// Role and RoleBinding, not ClusterRole and ClusterRoleBinding: the lease
			// sits in the operator's namespace, and nothing cluster-wide is needed.
			if election.role.Kind != "Role" || election.binding.Kind != "RoleBinding" {
				t.Errorf("leader election is granted through a %s and a %s, want a Role and a RoleBinding",
					election.role.Kind, election.binding.Kind)
			}
			for _, verb := range []string{"get", "create", "update"} {
				if !slices.Contains(leaseVerbs(election.role.Rules), verb) {
					t.Errorf("the Role does not allow %q on leases, which leader election needs", verb)
				}
			}

			if election.binding.RoleRef.Kind != "Role" || election.binding.RoleRef.Name != election.role.Name {
				t.Errorf("the RoleBinding refers to %s %q, not to the Role %q",
					election.binding.RoleRef.Kind, election.binding.RoleRef.Name, election.role.Name)
			}
			if len(election.binding.Subjects) != 1 {
				t.Fatalf("the RoleBinding has %d subjects, want the operator's service account only", len(election.binding.Subjects))
			}
			subject := election.binding.Subjects[0]
			if subject.Kind != "ServiceAccount" || subject.Name != election.serviceAccountName ||
				subject.Namespace != election.namespace {
				t.Errorf("the RoleBinding grants %s %s/%s, but the operator runs as the service account %s/%s",
					subject.Kind, subject.Namespace, subject.Name, election.namespace, election.serviceAccountName)
			}
		})
	}
}

func TestChartLeaderElectionRulesMatchConfig(t *testing.T) {
	sameInChart(t, "the rules of the leader election Role",
		configLeaderElection(t).role.Rules, chartLeaderElection(t).role.Rules)
}

// The operator's cluster role must not grow a rule on leases: the lease is
// namespaced, and the Role above is the only grant it needs.
func TestOperatorClusterRolesGrantNoLeases(t *testing.T) {
	for _, path := range []string{
		"config/rbac/role.yaml",
		"charts/treebind/templates/rbac/manager-role.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			for _, rule := range rulesFrom(t, filepath.Join("..", "..", filepath.FromSlash(path))) {
				if coversLeases(rule.APIGroups, rule.Resources) {
					t.Errorf("the cluster role grants %v on leases, a cluster-wide permission that leader election does not need: %+v",
						rule.Verbs, rule)
				}
			}
		})
	}
}

// The check above passes when no rule exists, so this keeps it from passing
// because the detector is broken.
func TestCoversLeases(t *testing.T) {
	tests := []struct {
		name      string
		groups    []string
		resources []string
		want      bool
	}{
		{"leases", []string{leasesGroup}, []string{leasesResource}, true},
		{"every resource of the group", []string{leasesGroup}, []string{"*"}, true},
		{"every resource of every group", []string{"*"}, []string{"*"}, true},
		{"leases of every group", []string{"*"}, []string{leasesResource}, true},
		{"another resource of the group", []string{leasesGroup}, []string{"leasecandidates"}, false},
		{"leases of another group", []string{"batch"}, []string{leasesResource}, false},
		{"jobs", []string{"batch"}, []string{"jobs"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coversLeases(tt.groups, tt.resources); got != tt.want {
				t.Errorf("coversLeases(%v, %v) = %v, want %v", tt.groups, tt.resources, got, tt.want)
			}
		})
	}
}
