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
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The RBAC templates of the chart are run here with the functions of Helm and sprig that they use, which are
// written below in a few lines each. That is not Helm: a function that differs from sprig's would show
// nothing here, and helm lint and helm template are what hold the chart to Helm itself. What it does hold is
// the logic of the templates, which nothing else does without a cluster: which objects they make for which
// values, in which namespaces, and with which rules, for the three ways that the operator can be installed.

// chartRelease is the release that the templates are run for.
const (
	releaseName      = dirTreebind
	releaseNamespace = dirTreebind + "-system"
)

// isEmpty is what sprig calls empty: a missing value, an empty string, a zero, false and an empty list or map.
func isEmpty(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	default:
		return false
	}
}

func toInt(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// The names of two of the functions of the templates, which several tests of the chart also name.
const (
	funcInclude = "include"
	funcDefault = "default"
)

// chartFuncs are the functions of the templates. include runs a template of the chart by name.
func chartFuncs(set **template.Template) template.FuncMap {
	return template.FuncMap{
		funcInclude: func(name string, data any) (string, error) {
			var out bytes.Buffer
			err := (*set).ExecuteTemplate(&out, name, data)
			return out.String(), err
		},
		"dict": func(pairs ...any) map[string]any {
			made := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				made[fmt.Sprint(pairs[i])] = pairs[i+1]
			}
			return made
		},
		funcDefault: func(fallback any, given ...any) any {
			if len(given) == 0 || isEmpty(given[0]) {
				return fallback
			}
			return given[0]
		},
		"required": func(message string, value any) (any, error) {
			if isEmpty(value) {
				return nil, errors.New(message)
			}
			return value, nil
		},
		"fail": func(message string) (string, error) { return "", errors.New(message) },
		"trunc": func(n int, s string) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
		"contains":   func(part, s string) bool { return strings.Contains(s, part) },
		"replace":    func(old, replacement, s string) string { return strings.ReplaceAll(s, old, replacement) },
		"sub":        func(a, b any) int { return toInt(a) - toInt(b) },
		"int":        toInt,
		"list":       func(items ...any) []any { return items },
		"splitList":  func(sep, s string) []string { return strings.Split(s, sep) },
		"regexMatch": func(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) },
		"toString": func(value any) string {
			if text, ok := value.(string); ok {
				return text
			}
			return fmt.Sprint(value)
		},
		"hasKey": func(set map[string]any, key string) bool {
			_, found := set[key]
			return found
		},
		"set": func(set map[string]any, key string, value any) map[string]any {
			set[key] = value
			return set
		},
		"join": func(sep string, items any) string {
			var words []string
			switch list := items.(type) {
			case []string:
				words = list
			case []any:
				for _, item := range list {
					words = append(words, fmt.Sprint(item))
				}
			}
			return strings.Join(words, sep)
		},
		"ternary": func(whenTrue, whenFalse any, condition bool) any {
			if condition {
				return whenTrue
			}
			return whenFalse
		},
	}
}

// mergeValues puts the overrides over the values, key by key where both are maps.
func mergeValues(base, overrides map[string]any) map[string]any {
	merged := maps.Clone(base)
	for key, value := range overrides {
		if inner, ok := value.(map[string]any); ok {
			if existing, ok := merged[key].(map[string]any); ok {
				merged[key] = mergeValues(existing, inner)
				continue
			}
		}
		merged[key] = value
	}
	return merged
}

// chartData is what a template sees as ".": the default values of the chart with the overrides on them, and
// the release.
func chartData(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()
	var values map[string]any
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "charts", dirTreebind, "values.yaml")), &values); err != nil {
		t.Fatalf("read the values of the chart: %v", err)
	}
	return map[string]any{
		"Values":  mergeValues(values, overrides),
		"Release": map[string]any{"Name": releaseName, "Namespace": releaseNamespace, "Service": "Helm"},
		"Chart":   map[string]any{"Name": dirTreebind, "Version": "0.0.0-dev"},
	}
}

// chartTemplates parses the helpers of the chart and the templates named, which are files of
// charts/treebind/templates.
func chartTemplates(t *testing.T, files ...string) *template.Template {
	t.Helper()
	var set *template.Template
	set = template.New("chart").Option("missingkey=zero").Funcs(chartFuncs(&set))
	if _, err := set.Parse(readRepoFile(t, "charts", dirTreebind, "templates", "_helpers.tpl")); err != nil {
		t.Fatalf("parse the helpers of the chart: %v", err)
	}
	for _, file := range files {
		if _, err := set.New(file).Parse(readRepoFile(t, "charts", dirTreebind, "templates", file)); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
	}
	return set
}

// renderChart runs one template of the chart for the values and returns what it writes.
func renderChart(t *testing.T, file string, overrides map[string]any) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := chartTemplates(t, file).ExecuteTemplate(&out, file, chartData(t, overrides))
	return out.String(), err
}

// renderedObject holds the fields of an RBAC object that the tests read.
type renderedObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Rules      []policyRule      `json:"rules"`
	RoleRef    rbacv1.RoleRef    `json:"roleRef"`
	Subjects   []rbacv1.Subject  `json:"subjects"`
}

// objectsOf reads the documents that a template wrote. A document that holds nothing is left out, as Helm does.
func objectsOf(t *testing.T, rendered string) []renderedObject {
	t.Helper()
	var objects []renderedObject
	for document := range strings.SplitSeq(rendered, "\n---\n") {
		document = strings.TrimPrefix(strings.TrimSpace(document), "---")
		if strings.TrimSpace(document) == "" {
			continue
		}
		var object renderedObject
		if err := yaml.UnmarshalStrict([]byte(document), &object); err != nil {
			t.Fatalf("a document that the template wrote does not parse: %v\n%s", err, document)
		}
		objects = append(objects, object)
	}
	return objects
}

// rendered runs a template and reads its objects, and fails the test when the template fails.
func rendered(t *testing.T, file string, overrides map[string]any) []renderedObject {
	t.Helper()
	out, err := renderChart(t, file, overrides)
	if err != nil {
		t.Fatalf("%s did not render: %v", file, err)
	}
	return objectsOf(t, out)
}

// namespacedValues are the values of a chart that installs Roles and not ClusterRoles, in the namespaces.
func namespacedValues(namespaces ...string) map[string]any {
	rbac := map[string]any{"namespaced": true}
	if len(namespaces) > 0 {
		list := make([]any, len(namespaces))
		for i, name := range namespaces {
			list[i] = name
		}
		rbac["namespaces"] = list
	}
	return map[string]any{dirRBAC: rbac}
}

const (
	managerRoleFile    = "rbac/manager-role.yaml"
	managerBindingFile = "rbac/manager-rolebinding.yaml"
	watchNamespacesDef = "treebind.watchNamespaces"
	// watchNamespacesArg is the argument of the operator that the Deployment adds, which is empty when the
	// roles are cluster-wide.
	watchNamespacesArg = "treebind.watchNamespacesArg"
	kindClusterRole    = "ClusterRole"
	operatorAccount    = "treebind-controller-manager"
	managerRoleName    = "treebind-manager-role"
	// The keys and the words that several tests of the chart use.
	keyNamespaced = "namespaced"
	keyNamespaces = "namespaces"
	labelCluster  = "cluster-wide"
	caseWildcard  = "a wildcard"
	nsApps2       = "apps"
	nsTools2      = "tools"
)

func TestTheHelpersOfTheChartHoldWhatTheTestsOfTheRendererAssume(t *testing.T) {
	// If the renderer of these tests names a resource differently from what the chart should, every other test
	// here compares the wrong thing, so the one name that they all depend on is held first.
	out, err := renderChart(t, managerBindingFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: "+managerRoleName) || !strings.Contains(out, "name: "+operatorAccount) {
		t.Errorf("the binding does not name %s and %s:\n%s", managerRoleName, operatorAccount, out)
	}
}

func TestTheManagerRoleIsAClusterRoleAndAClusterRoleBindingByDefault(t *testing.T) {
	roles := rendered(t, managerRoleFile, nil)
	bindings := rendered(t, managerBindingFile, nil)

	if len(roles) != 1 || roles[0].Kind != kindClusterRole || roles[0].Metadata.Namespace != "" || roles[0].Metadata.Name != managerRoleName {
		t.Fatalf("roles = %+v, want one ClusterRole named %s", roles, managerRoleName)
	}
	if len(bindings) != 1 || bindings[0].Kind != "ClusterRoleBinding" || bindings[0].RoleRef.Kind != kindClusterRole ||
		bindings[0].RoleRef.Name != managerRoleName {
		t.Fatalf("bindings = %+v, want one ClusterRoleBinding of %s", bindings, managerRoleName)
	}
	if diff := grantDifference(grantsOf(roles[0].Rules), managerGrants); diff != "" {
		t.Error(diff)
	}
}

func TestTheNamespacedManagerRoleIsOneRolePerNamespaceAndNoClusterRole(t *testing.T) {
	tests := []struct {
		name       string
		namespaces []string
		want       []string
	}{
		{"with no list it is the release namespace", nil, []string{releaseNamespace}},
		{"with one namespace", []string{nsApps2}, []string{nsApps2}},
		{"with two", []string{nsApps2, nsTools2}, []string{nsApps2, nsTools2}},
		{"with the release namespace among others", []string{releaseNamespace, nsApps2}, []string{releaseNamespace, nsApps2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := namespacedValues(tt.namespaces...)
			roles := rendered(t, managerRoleFile, values)
			bindings := rendered(t, managerBindingFile, values)

			var roleNamespaces, bindingNamespaces []string
			for _, role := range roles {
				if role.Kind != kindRole || role.Metadata.Name != managerRoleName {
					t.Errorf("an object of kind %s named %s, want a Role named %s", role.Kind, role.Metadata.Name, managerRoleName)
				}
				roleNamespaces = append(roleNamespaces, role.Metadata.Namespace)
				if diff := grantDifference(grantsOf(role.Rules), managerGrants); diff != "" {
					t.Errorf("the Role in %s: %s", role.Metadata.Namespace, diff)
				}
			}
			for _, binding := range bindings {
				if binding.Kind != kindRoleBinding || binding.RoleRef.Kind != kindRole || binding.RoleRef.Name != managerRoleName {
					t.Errorf("a binding of kind %s that names %s %s, want a RoleBinding of the Role %s",
						binding.Kind, binding.RoleRef.Kind, binding.RoleRef.Name, managerRoleName)
				}
				bindingNamespaces = append(bindingNamespaces, binding.Metadata.Namespace)
				// The account is the operator's, in the namespace of the release, whatever namespace the Role is in.
				if len(binding.Subjects) != 1 || binding.Subjects[0].Name != operatorAccount ||
					binding.Subjects[0].Namespace != releaseNamespace || binding.Subjects[0].Kind != kindServiceAccount {
					t.Errorf("subjects = %+v, want the service account %s of %s", binding.Subjects, operatorAccount, releaseNamespace)
				}
			}
			if !slices.Equal(roleNamespaces, tt.want) || !slices.Equal(bindingNamespaces, tt.want) {
				t.Errorf("Roles in %v and RoleBindings in %v, want both in %v", roleNamespaces, bindingNamespaces, tt.want)
			}
		})
	}
}

func TestTheNamespacesOfTheRolesAreTheNamespacesOfTheFlagOfTheOperator(t *testing.T) {
	// One definition gives both, so the operator cannot watch a namespace that it has no Role in.
	for name, tt := range map[string]struct {
		values map[string]any
		want   string
	}{
		labelCluster:    {nil, ""},
		"release only":  {namespacedValues(), releaseNamespace},
		"one namespace": {namespacedValues(nsApps2), nsApps2},
		"two":           {namespacedValues(nsApps2, nsTools2), nsApps2 + "," + nsTools2},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := chartTemplates(t).ExecuteTemplate(&out, watchNamespacesDef, chartData(t, tt.values)); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Errorf("%s = %q, want %q", watchNamespacesDef, out.String(), tt.want)
			}
		})
	}
}

// chartConsumers are the templates that read the namespaces of the operator, each as a function of the values
// that either renders it or says why it cannot.
func chartConsumers(t *testing.T) map[string]func(values map[string]any) error {
	t.Helper()
	file := func(name string) func(map[string]any) error {
		return func(values map[string]any) error {
			_, err := renderChart(t, name, values)
			return err
		}
	}
	definition := func(name string) func(map[string]any) error {
		return func(values map[string]any) error {
			var out bytes.Buffer
			return chartTemplates(t).ExecuteTemplate(&out, name, chartData(t, values))
		}
	}
	return map[string]func(map[string]any) error{
		"the Role":                       file(managerRoleFile),
		"the RoleBinding":                file(managerBindingFile),
		"the list of namespaces":         definition(watchNamespacesDef),
		"the argument of the Deployment": definition(watchNamespacesArg),
	}
}

func TestEveryConsumerOfTheNamespacesRefusesThemWithoutTheSwitchAndNoneTakesThemForClusterWideRoles(t *testing.T) {
	// A list that is ignored would leave a ClusterRole where the person who wrote the list expects Roles, so
	// no template that reads the list may go on without the switch. The check is in the one helper that they
	// all read the list through, and this holds that each of them reaches it.
	values := map[string]any{dirRBAC: map[string]any{keyNamespaces: []any{nsApps2}}}
	for name, consumer := range chartConsumers(t) {
		t.Run(name, func(t *testing.T) {
			if err := consumer(values); err == nil || !strings.Contains(err.Error(), "rbac.namespaced") {
				t.Errorf("rendering with rbac.namespaces and no rbac.namespaced = %v, want an error that names rbac.namespaced", err)
			}
		})
	}
}

func TestTheDeploymentReadsTheNamespacesThroughTheHelperSoThatItRefusesThemToo(t *testing.T) {
	// manager.yaml is not run by these tests, so this holds that it takes the argument from the helper whose
	// refusals the test above holds, and from nowhere else.
	manager := readRepoFile(t, "charts", dirTreebind, "templates", "manager", "manager.yaml")
	if !strings.Contains(manager, `include "`+watchNamespacesArg+`" .`) {
		t.Errorf("manager.yaml does not take its --watch-namespaces argument from %s", watchNamespacesArg)
	}
	if strings.Contains(manager, "rbac.namespaces") {
		t.Error("manager.yaml reads rbac.namespaces itself, and not through the helper that checks it")
	}
}

// chartInstall is a way of installing the chart that the tests run, by name and by the values it sets.
type chartInstall struct {
	name   string
	values map[string]any
}

// chartInstalls are the ways of installing that the tests of the chart cover, the same for the renderer of
// these tests and for real Helm (chart_helm_test.go).
func chartInstalls() []chartInstall {
	return []chartInstall{
		{labelCluster, nil},
		{"namespaced, in the release namespace", namespacedValues()},
		{"namespaced, in one namespace", namespacedValues(nsApps2)},
		{"namespaced, in two", namespacedValues(nsApps2, nsTools2)},
		{"cluster-wide, with the volume role", volumeReadValues(false)},
		{"namespaced, with the volume role", volumeReadValues(true)},
		{"namespaced, with a long and a short name", namespacedValues(strings.Repeat("a", 63), "b", "team-1", "2c")},
	}
}

// chartRefusal is values that the chart refuses to install, and a part of what it says.
type chartRefusal struct {
	name   string
	values map[string]any
	says   string
}

func namespaceList(entries ...any) map[string]any {
	return map[string]any{dirRBAC: map[string]any{keyNamespaced: true, keyNamespaces: entries}}
}

// chartRefusals are the lists of namespaces that the chart refuses, each for what the operator would take
// for something else, or refuse at its start, or read as another list than the one that the Roles are made
// for. The message names the entry that is wrong.
func chartRefusals() []chartRefusal {
	return []chartRefusal{
		{"a list without the switch", map[string]any{dirRBAC: map[string]any{keyNamespaces: []any{nsApps2}}}, "rbac.namespaced"},
		{"an empty entry", namespaceList(nsApps2, ""), "empty entry"},
		{"a blank before a name", namespaceList(" a"), `" a"`},
		{"a blank after a name", namespaceList("a "), `"a "`},
		{"a capital and an underscore", namespaceList("A_b"), `"A_b"`},
		{"a dot", namespaceList("a.b"), `"a.b"`},
		{"a dash at the start", namespaceList("-a"), `"-a"`},
		{"a dash at the end", namespaceList("a-"), `"a-"`},
		{caseWildcard, namespaceList("*"), `"*"`},
		{"a name of 64 characters", namespaceList(strings.Repeat("a", 64)), strings.Repeat("a", 64)},
		{"a name that comes twice", namespaceList(nsApps2, nsTools2, nsApps2), `"apps" twice`},
	}
}

func TestTheChartRefusesTheListsThatTheOperatorTakesForOthersAndNamesTheEntry(t *testing.T) {
	for _, refusal := range chartRefusals() {
		for name, consumer := range chartConsumers(t) {
			t.Run(refusal.name+", "+name, func(t *testing.T) {
				err := consumer(refusal.values)
				if err == nil || !strings.Contains(err.Error(), refusal.says) {
					t.Errorf("rendering with %s = %v, want an error that holds %q", refusal.name, err, refusal.says)
				}
			})
		}
	}
}

func TestTheChartWritesAListThatTheOperatorReadsAsTheSameList(t *testing.T) {
	// The Roles are made from the list and the operator reads the argument, so what the one writes the other
	// has to read back as the same names in the same order, for every list that the chart takes.
	for _, install := range chartInstalls() {
		t.Run(install.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := chartTemplates(t).ExecuteTemplate(&out, watchNamespacesDef, chartData(t, install.values)); err != nil {
				t.Fatal(err)
			}
			got, err := ParseWatchNamespaces(out.String())
			if err != nil {
				t.Fatalf("the operator refuses what the chart writes, %q: %v", out.String(), err)
			}
			wanted := rendered(t, managerRoleFile, install.values)
			var inRoles []string
			for _, role := range wanted {
				if role.Kind == kindRole {
					inRoles = append(inRoles, role.Metadata.Namespace)
				}
			}
			if !slices.Equal(got, inRoles) {
				t.Errorf("the operator reads %v and the Roles are in %v", got, inRoles)
			}
		})
	}
}

func TestEveryListThatTheChartTakesIsOneThatTheOperatorTakes(t *testing.T) {
	// The chart is the stricter of the two: it takes no blanks and no duplicates, which the operator
	// forgives. What it must never do is take a list that the operator would refuse, so that a list passes
	// the install and stops the operator at its start.
	for _, install := range chartInstalls() {
		var out bytes.Buffer
		if err := chartTemplates(t).ExecuteTemplate(&out, watchNamespacesDef, chartData(t, install.values)); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseWatchNamespaces(out.String()); err != nil {
			t.Errorf("%s: the chart takes a list that the operator refuses: %v", install.name, err)
		}
	}
}

func TestTheArgumentOfTheDeploymentIsTheFlagWithTheNamespacesAndNoneWhenClusterWide(t *testing.T) {
	for name, tt := range map[string]struct {
		values map[string]any
		want   string
	}{
		labelCluster:    {nil, ""},
		"release only":  {namespacedValues(), "--watch-namespaces=" + releaseNamespace},
		"two namespace": {namespacedValues(nsApps2, nsTools2), "--watch-namespaces=" + nsApps2 + "," + nsTools2},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := chartTemplates(t).ExecuteTemplate(&out, watchNamespacesArg, chartData(t, tt.values)); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Errorf("%s = %q, want %q", watchNamespacesArg, out.String(), tt.want)
			}
		})
	}
}

func TestTheChartPassesItsFlagAfterTheArgumentsOfTheUserSoThatTheChartWins(t *testing.T) {
	// A flag that comes twice takes the last value. The generated flag has to be the last, or a
	// --watch-namespaces in manager.args would make the operator watch other namespaces than the Roles are for.
	manager := readRepoFile(t, "charts", dirTreebind, "templates", "manager", "manager.yaml")
	userArgs := strings.Index(manager, "range .Values.manager.args")
	generated := strings.Index(manager, `include "`+watchNamespacesArg+`" .`)
	if userArgs < 0 || generated < 0 {
		t.Fatalf("manager.yaml does not hold both the arguments of the user (%d) and the generated one (%d)", userArgs, generated)
	}
	if generated < userArgs {
		t.Error("manager.yaml passes the generated --watch-namespaces before the arguments of the user, which then override it")
	}

	// And the claim that the last one wins is the one of the flag package that the operator parses with.
	var seen string
	set := flag.NewFlagSet("operator", flag.ContinueOnError)
	set.StringVar(&seen, "watch-namespaces", "", "")
	if err := set.Parse([]string{"--watch-namespaces=elsewhere", "--watch-namespaces=apps,tools"}); err != nil {
		t.Fatal(err)
	}
	if seen != "apps,tools" {
		t.Errorf("the operator reads %q from a flag that comes twice, want the last, apps,tools", seen)
	}
}

const (
	volumeRoleFile    = "rbac/volume-read-role.yaml"
	volumeBindingFile = "rbac/volume-read-rolebinding.yaml"
	volumeRoleName    = "treebind-volume-read-role"
)

// volumeReadValues turn the optional role on, over the values of one of the two ways of installing.
func volumeReadValues(namespaced bool) map[string]any {
	values := map[string]any{dirRBAC: map[string]any{"volumeRead": map[string]any{"enabled": true}}}
	if namespaced {
		values = mergeValues(values, namespacedValues(nsApps2))
	}
	return values
}

func TestTheOptionalVolumeRoleIsAbsentUnlessTheValueTurnsItOn(t *testing.T) {
	for _, file := range []string{volumeRoleFile, volumeBindingFile} {
		for name, values := range map[string]map[string]any{labelCluster: nil, keyNamespaced: namespacedValues()} {
			if got := rendered(t, file, values); len(got) != 0 {
				t.Errorf("%s with the defaults, %s, made %+v, want nothing", file, name, got)
			}
		}
	}
}

func TestTheOptionalVolumeRoleIsAClusterRoleInBothWaysOfInstallingAndReadsOnly(t *testing.T) {
	for _, namespaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaced %v", namespaced), func(t *testing.T) {
			roles := rendered(t, volumeRoleFile, volumeReadValues(namespaced))
			bindings := rendered(t, volumeBindingFile, volumeReadValues(namespaced))

			// Volumes and storage classes have no namespace, so a Role could not grant them.
			if len(roles) != 1 || roles[0].Kind != kindClusterRole || roles[0].Metadata.Name != volumeRoleName || roles[0].Metadata.Namespace != "" {
				t.Fatalf("roles = %+v, want one ClusterRole named %s", roles, volumeRoleName)
			}
			if diff := grantDifference(grantsOf(roles[0].Rules), volumeReadGrants); diff != "" {
				t.Error(diff)
			}
			if len(bindings) != 1 || bindings[0].Kind != "ClusterRoleBinding" || bindings[0].RoleRef.Name != volumeRoleName ||
				bindings[0].RoleRef.Kind != kindClusterRole || len(bindings[0].Subjects) != 1 ||
				bindings[0].Subjects[0].Name != operatorAccount || bindings[0].Subjects[0].Namespace != releaseNamespace {
				t.Errorf("bindings = %+v, want one ClusterRoleBinding of %s to %s of %s", bindings, volumeRoleName, operatorAccount, releaseNamespace)
			}
		})
	}
}

func TestTheChartPassesTheFlagThatTheOperatorHasForTheNamespacesOfItsRoles(t *testing.T) {
	const flagName = "watch-namespaces"
	if !strings.Contains(readRepoFile(t, "cmd", "operator", "main.go"), `flag.StringVar(&watchNamespaces, "`+flagName+`"`) {
		t.Errorf("the operator does not register the flag --%s that the chart passes", flagName)
	}
	manager := readRepoFile(t, "charts", dirTreebind, "templates", "manager", "manager.yaml")
	wanted := "{{- with (include \"" + watchNamespacesArg + "\" .) }}\n        - {{ . }}\n        {{- end }}"
	if !strings.Contains(manager, wanted) {
		t.Errorf("manager.yaml does not pass the argument of %s as a list item; it holds no:\n%s", watchNamespacesArg, wanted)
	}
	if !strings.Contains(readRepoFile(t, "charts", dirTreebind, "templates", "_helpers.tpl"), "--"+flagName+"=") {
		t.Errorf("the helper %s does not write --%s=", watchNamespacesArg, flagName)
	}
}

// Every template of the chart has to parse: a block that is not closed, or an action that is not
// finished, fails at install and nowhere sooner. The functions that these tests do not write are
// given a body that does nothing, because only the syntax is held here, and a template that these
// tests cannot run, such as the Deployment, is held to that much at least.
func TestEveryTemplateOfTheChartParses(t *testing.T) {
	root := "charts/treebind/templates"
	var files []string
	for _, dir := range []string{"manager", dirRBAC, "metrics", "network-policy", "prometheus", "crd"} {
		entries, err := os.ReadDir(filepath.Join("..", "..", root, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			files = append(files, dir+"/"+entry.Name())
		}
	}
	files = append(files, "NOTES.txt")
	if len(files) < 20 {
		t.Fatalf("found %d templates, which is fewer than the chart has: %v", len(files), files)
	}

	unknown := regexp.MustCompile(`function "([^"]+)" not defined`)
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			text := readRepoFile(t, "charts", dirTreebind, "templates", file)
			var set *template.Template
			funcs := chartFuncs(&set)
			for range 60 {
				set = template.New(file).Funcs(funcs)
				_, err := set.Parse(readRepoFile(t, "charts", dirTreebind, "templates", "_helpers.tpl"))
				if err == nil {
					_, err = set.New("body").Parse(text)
				}
				if err == nil {
					return
				}
				missing := unknown.FindStringSubmatch(err.Error())
				if missing == nil {
					t.Fatalf("%s does not parse: %v", file, err)
				}
				funcs[missing[1]] = func(...any) any { return nil }
			}
			t.Fatalf("%s needs more functions than the test stubs", file)
		})
	}
}
