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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/app"
)

// docs/metrics-and-events.md names the metrics, their labels, the events and the expressions to alert on, and the recovery
// guide repeats some of them. There is no Prometheus here to run the expressions, so what is held is what a wrong one would
// get wrong with no error at all: a name of a metric, a label or a value of a label that the operator does not make finds
// nothing, and an alert on nothing never fires.

const metricsPage = "metrics-and-events.md"

// pageOf returns a page of docs/.
func pageOf(t *testing.T, page string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", page))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fencedIn returns what follows a marker on a page, in the fenced block that comes next, which has the language.
func fencedIn(t *testing.T, page, kind, name, language string) string {
	t.Helper()
	marker := "<!-- " + kind + ": " + name + " -->\n```" + language + "\n"
	_, rest, found := strings.Cut(pageOf(t, page), marker)
	if !found {
		t.Fatalf("docs/%s has no %s marked %q in %q", page, kind, name, language)
	}
	block, _, found := strings.Cut(rest, "\n```")
	if !found {
		t.Fatalf("the %s %q of docs/%s is not closed", kind, name, page)
	}
	return block + "\n"
}

// exportedFamily is a family as the registry gives it to a scrape.
type exportedFamily struct {
	kind   string
	labels []string
}

// exportedFamilies are the families that the operator exports, with a series of each, as a scrape reads them.
func exportedFamilies(t *testing.T) map[string]exportedFamily {
	t.Helper()
	registry := prometheus.NewRegistry()
	m, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	treebind := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	agreed := metav1.NewTime(agreedAt)
	treebind.Status.LastSyncTime = &agreed
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced)
	m.recordOutcomeChange(treebind, syncEvent{condition: treebindv1alpha1.ConditionSynced, status: yes, reason: reasonInSync})
	m.recordSync(treebind, nil, agreedAt)

	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	families := map[string]exportedFamily{}
	for _, family := range gathered {
		var labels []string
		for _, label := range family.GetMetric()[0].GetLabel() {
			labels = append(labels, label.GetName())
		}
		slices.Sort(labels)
		families[family.GetName()] = exportedFamily{kind: strings.ToLower(family.GetType().String()), labels: labels}
	}
	return families
}

func TestThePageNamesEveryFamilyThatTheOperatorExportsWithItsTypeAndLabels(t *testing.T) {
	_, table, found := strings.Cut(pageOf(t, metricsPage), "<!-- families -->\n")
	if !found {
		t.Fatal("the page has no table of the families")
	}
	quoted := regexp.MustCompile("`([a-z_]+)`")
	documented := map[string]exportedFamily{}
	for line := range strings.Lines(table) {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		name := quoted.FindStringSubmatch(cells[1])
		if name == nil {
			continue
		}
		var labels []string
		for _, label := range quoted.FindAllStringSubmatch(cells[3], -1) {
			labels = append(labels, label[1])
		}
		slices.Sort(labels)
		documented[name[1]] = exportedFamily{kind: strings.TrimSpace(cells[2]), labels: labels}
	}

	exported := exportedFamilies(t)
	if got, want := slices.Sorted(maps.Keys(documented)), slices.Sorted(maps.Keys(exported)); !slices.Equal(got, want) {
		t.Fatalf("the page names %v, the operator exports %v", got, want)
	}
	for name, family := range exported {
		if page := documented[name]; page.kind != family.kind || !slices.Equal(page.labels, family.labels) {
			t.Errorf("%s: the page says %s with %v, the operator exports a %s with %v", name, page.kind, page.labels,
				family.kind, family.labels)
		}
	}
}

// knownValues are the values that the expressions may give the labels that have a fixed set of them.
func knownValues() map[string][]string {
	reasons := slices.Collect(maps.Keys(failedStepReasons))
	reasons = append(reasons,
		treebindv1alpha1.ReasonSyncFailed, treebindv1alpha1.ReasonInSync, treebindv1alpha1.ReasonSyncPending,
		treebindv1alpha1.ReasonAgentOutdated, treebindv1alpha1.ReasonAgentUnreachable, treebindv1alpha1.ReasonAwaitingAgentReport,
		treebindv1alpha1.ReasonNoConflict, treebindv1alpha1.ReasonConflictResolved, treebindv1alpha1.ReasonNotSynced,
		treebindv1alpha1.ReasonSyncPodReady, treebindv1alpha1.ReasonSyncPodNotReady, treebindv1alpha1.ReasonReconcileFailed,
		treebindv1alpha1.ReasonSupported, treebindv1alpha1.ReasonNotImplemented, treebindv1alpha1.ReasonReportReceived,
		treebindv1alpha1.ReasonPollFailing, treebindv1alpha1.ReasonAgentNotRunning, treebindv1alpha1.ReasonTokenUnavailable,
		treebindv1alpha1.ReasonKeysUnavailable, treebindv1alpha1.ReasonConnectionFailed, treebindv1alpha1.ReasonTokenRefused,
		treebindv1alpha1.ReasonUnexpectedResponse, treebindv1alpha1.ReasonReportUnreadable)
	return map[string][]string{
		labelCondition: {
			treebindv1alpha1.ConditionReady, treebindv1alpha1.ConditionSpecSupported, treebindv1alpha1.ConditionSynced,
			treebindv1alpha1.ConditionConflict, treebindv1alpha1.ConditionAgentUnreachable,
		},
		labelStatus: {string(yes), string(no), string(unknown)},
		labelReason: reasons,
	}
}

var (
	metricName = regexp.MustCompile(`\btreebind_[a-z_]+`)
	// selector is a metric with its matchers, and matcher one matcher: the label, the operator and the value.
	selector = regexp.MustCompile(`(treebind_[a-z_]+)\{([^}]*)\}`)
	matcher  = regexp.MustCompile(`^\s*([a-z_]+)\s*(=~|!~|!=|=)\s*"([^"]*)"\s*$`)
	grouping = regexp.MustCompile(`\b(?:by|without|on|ignoring)\s*\(([^)]*)\)`)
)

// reporter is what checkExpression needs of a test, so that a test of the checker can see what it reports.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// probe is a reporter that keeps whether it was told of a mistake.
type probe struct{ failed bool }

func (p *probe) Helper()               {}
func (p *probe) Errorf(string, ...any) { p.failed = true }

// checkExpression holds an expression of PromQL to the families that the operator exports: every name is one of them,
// every matcher names a label that the family has and a value that the label can take, and every grouping names labels
// that the families of the expression have.
func checkExpression(t reporter, where, expression string, exported map[string]exportedFamily) {
	t.Helper()
	names := metricName.FindAllString(expression, -1)
	if len(names) == 0 {
		t.Errorf("%s: %q names no metric of the operator", where, expression)
	}
	labelsOfExpression := map[string]bool{}
	for _, name := range names {
		family, ok := exported[name]
		if !ok {
			t.Errorf("%s: %s is no metric that the operator exports", where, name)
			continue
		}
		for _, label := range family.labels {
			labelsOfExpression[label] = true
		}
	}
	values := knownValues()
	for _, match := range selector.FindAllStringSubmatch(expression, -1) {
		family := exported[match[1]]
		for text := range strings.SplitSeq(match[2], ",") {
			parts := matcher.FindStringSubmatch(text)
			if parts == nil {
				t.Errorf("%s: %q is not a matcher that this test reads", where, text)
				continue
			}
			label, operator, value := parts[1], parts[2], parts[3]
			if !slices.Contains(family.labels, label) {
				t.Errorf("%s: %s has no label %s", where, match[1], label)
			}
			if known, fixed := values[label]; fixed && (operator == "=" || operator == "!=") && !slices.Contains(known, value) {
				t.Errorf("%s: %s is no value that the label %s takes", where, value, label)
			}
		}
	}
	for _, match := range grouping.FindAllStringSubmatch(expression, -1) {
		for label := range strings.SplitSeq(match[1], ",") {
			if label = strings.TrimSpace(label); !labelsOfExpression[label] {
				t.Errorf("%s: the grouping names %s, which no metric of the expression has", where, label)
			}
		}
	}
}

// promqlExpressions are the expressions of a block of PromQL: its lines that are not comments, an expression that goes on to
// the next line joined to it.
func promqlExpressions(block string) []string {
	var expressions []string
	for line := range strings.Lines(block) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.TrimSpace(line) == "", strings.HasPrefix(strings.TrimSpace(line), "#"):
		case strings.HasPrefix(line, " ") && len(expressions) > 0:
			expressions[len(expressions)-1] += " " + strings.TrimSpace(line)
		default:
			expressions = append(expressions, strings.TrimSpace(line))
		}
	}
	return expressions
}

func TestTheExpressionsOfThePagesUseMetricsLabelsAndValuesThatTheOperatorMakes(t *testing.T) {
	exported := exportedFamilies(t)
	for _, block := range []struct{ page, name string }{
		{metricsPage, "promql-not-in-sync"},
		{metricsPage, "promql-others"},
	} {
		expressions := promqlExpressions(fencedIn(t, block.page, "tested", block.name, "promql"))
		if len(expressions) == 0 {
			t.Errorf("%s %s: no expression", block.page, block.name)
		}
		for _, expression := range expressions {
			checkExpression(t, block.page+" "+block.name, expression, exported)
		}
	}
}

// The checks above pass when the checker is blind, so this holds it to the mistakes that it is there for.
func TestCheckExpressionFindsWhatFindsNothing(t *testing.T) {
	exported := exportedFamilies(t)
	for _, tt := range []struct{ name, expression string }{
		{"a name that is not exported", `treebind_synced == 1`},
		{"a label that the family lacks", `treebind_sync_interval_seconds{reason="InSync"}`},
		{"a condition that does not exist", `treebind_status_condition{condition="Pushed"} == 1`},
		{"a status spelled as Prometheus would not", `treebind_status_condition{status="true"} == 1`},
		{"a reason that does not exist", `treebind_status_condition{reason="PushRefused"} == 1`},
		{"a grouping by a label that is not there", `sum by (namespace, pod_name) (treebind_sync_interval_seconds)`},
		{"no metric at all", `up == 0`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			found := &probe{}
			checkExpression(found, tt.name, tt.expression, exported)
			if !found.failed {
				t.Errorf("%q passed", tt.expression)
			}
		})
	}
	checkExpression(t, "a good one", `max by (namespace, name) (treebind_status_condition{condition="Synced", status!="True"}) == 1`,
		exported)
}

// prometheusRule is what the tests read of a PrometheusRule.
type prometheusRule struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Alert string `json:"alert"`
				Expr  string `json:"expr"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

func TestThePrometheusRuleOfThePageAlertsWithTheExpressionsOfThePage(t *testing.T) {
	var rule prometheusRule
	if err := yaml.Unmarshal([]byte(fencedIn(t, metricsPage, "tested", "prometheus-rule", "yaml")), &rule); err != nil {
		t.Fatalf("the PrometheusRule does not parse: %v", err)
	}
	if rule.APIVersion != "monitoring.coreos.com/v1" || rule.Kind != "PrometheusRule" || len(rule.Spec.Groups) != 1 {
		t.Fatalf("%s %s with %d groups is not the PrometheusRule of the Prometheus Operator", rule.APIVersion, rule.Kind,
			len(rule.Spec.Groups))
	}
	exported := exportedFamilies(t)
	written := map[string]bool{}
	for _, block := range []string{"promql-not-in-sync", "promql-others"} {
		for _, expression := range promqlExpressions(fencedIn(t, metricsPage, "tested", block, "promql")) {
			written[strings.Join(strings.Fields(expression), " ")] = true
		}
	}
	for _, alert := range rule.Spec.Groups[0].Rules {
		if alert.Alert == "" || alert.Expr == "" {
			t.Errorf("a rule of the group has no alert or no expression: %+v", alert)
		}
		checkExpression(t, "the rule "+alert.Alert, alert.Expr, exported)
		if !written[strings.Join(strings.Fields(alert.Expr), " ")] {
			t.Errorf("the rule %s alerts on %q, which the page does not explain", alert.Alert, alert.Expr)
		}
	}
	if len(rule.Spec.Groups[0].Rules) < 3 {
		t.Errorf("the rule has %d alerts", len(rule.Spec.Groups[0].Rules))
	}
}

// The events that the page shows for a Treebind that synced and then had a push refused are the ones that the operator makes
// for those polls, with their types, reasons and messages, on the Treebind.
func TestTheEventsThatThePageShowsAreTheOnesThatTheOperatorMakes(t *testing.T) {
	sample := fencedIn(t, metricsPage, "output", "events-of-a-refused-push", "")
	lines := strings.Split(strings.TrimSuffix(sample, "\n"), "\n")
	columns := regexp.MustCompile(`\s{2,}`)
	if header := columns.Split(lines[0], -1); !slices.Equal(header, []string{"LAST SEEN", "TYPE", "REASON", "OBJECT", "MESSAGE"}) {
		t.Fatalf("the header is %q, which is not what kubectl events prints", header)
	}

	pending := pollFrom(awaiting(), bidirectional, syncReport(bidirectional, ""), no)
	inSync := pollFrom(pending, bidirectional, syncReport(bidirectional, commitOne), no)
	refused := pollFrom(inSync, bidirectional,
		syncReport(bidirectional, commitOne, failedCheck(app.CheckPush, reasonPushRejected)), no)
	steps := [][2][]metav1.Condition{{awaiting(), pending}, {pending, inSync}, {inSync, refused}}
	made := make([]syncEvent, 0, len(steps))
	for _, step := range steps {
		made = append(made, syncEventsOf(step[0], step[1])...)
	}

	if len(lines)-1 != len(made) {
		t.Fatalf("the page shows %d events, the operator makes %d: %q", len(lines)-1, len(made), summary(made))
	}
	for i, line := range lines[1:] {
		cells := columns.Split(line, 5)
		if len(cells) != 5 {
			t.Fatalf("line %d has %d columns: %q", i+1, len(cells), line)
		}
		event := made[i]
		if cells[1] != event.eventType || cells[2] != event.reason || cells[4] != event.note {
			t.Errorf("line %d shows\n%s %s %s\nthe operator makes\n%s %s %s", i+1, cells[1], cells[2], cells[4],
				event.eventType, event.reason, event.note)
		}
		if !strings.HasPrefix(cells[3], treebindv1alpha1.GroupVersion.WithKind("Treebind").Kind+"/") {
			t.Errorf("line %d is of %s, not of a Treebind", i+1, cells[3])
		}
	}
}

// The commands that list the events name the Treebind as kubectl knows the resource, by the singular name of the CRD.
func TestTheCommandsThatListTheEventsNameTheTreebindAsKubectlKnowsIt(t *testing.T) {
	var crd struct {
		Spec struct {
			Names struct {
				Singular string `json:"singular"`
			} `json:"names"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, dirConfig, "crd", "bases", "treebind.example.org_treebinds.yaml")),
		&crd); err != nil {
		t.Fatal(err)
	}
	forTreebind := regexp.MustCompile(`kubectl events (?:-n <namespace> )?--for ([a-z]+)/<name>`)
	for _, page := range []string{metricsPage} {
		block := fencedIn(t, page, "tested", "read-the-events", "sh")
		match := forTreebind.FindStringSubmatch(block)
		if match == nil {
			t.Errorf("%s: no kubectl events --for in %q", page, block)
			continue
		}
		if match[1] != crd.Spec.Names.Singular {
			t.Errorf("%s: kubectl events --for %s, and the CRD is %s", page, match[1], crd.Spec.Names.Singular)
		}
	}
}

// The commands that read the metrics by hand name the role that grants the endpoint, the lease of leader election and the
// port of the endpoint as the repository has them.
func TestTheCommandsThatReadTheMetricsNameTheRoleTheLeaseAndThePort(t *testing.T) {
	block := fencedIn(t, metricsPage, "tested", "read-the-metrics", "sh")
	main := readRepoFile(t, "cmd", "operator", "main.go")
	lease := regexp.MustCompile(`LeaderElectionID:\s*"([^"]+)"`).FindStringSubmatch(main)
	if lease == nil || !strings.Contains(block, "get lease "+lease[1]+" ") {
		t.Errorf("the block does not read the lease that the operator takes, %v", lease)
	}
	if !strings.Contains(readRepoFile(t, dirConfig, "default", "kustomization.yaml"), "namePrefix: treebind-") ||
		!strings.Contains(readRepoFile(t, dirConfig, dirRBAC, "metrics_reader_role.yaml"), "name: metrics-reader") ||
		!strings.Contains(block, "--clusterrole=treebind-metrics-reader ") {
		t.Error("the block does not bind the ClusterRole treebind-metrics-reader that config/rbac makes")
	}
	if !strings.Contains(readRepoFile(t, "charts", dirTreebind, "values.yaml"), "port: 8443") ||
		!strings.Contains(block, "8443:8443") || !strings.Contains(block, "https://127.0.0.1:8443/metrics") {
		t.Error("the block does not read the port of the metrics endpoint, 8443")
	}
	if !strings.Contains(block, "grep '^treebind_'") {
		t.Error("the block does not keep the series of the Treebinds")
	}
}
