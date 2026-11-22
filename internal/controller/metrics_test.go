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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
)

// newTestMetrics returns metrics registered on a registry of their own, which the test reads.
func newTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	m, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	return m, registry
}

// fataler is what gathered needs of a test, which a Ginkgo spec has too.
type fataler interface {
	Helper()
	Fatal(args ...any)
}

// gathered returns the series of a family as the registry gives them to a scrape: each by its labels, written as
// name=value and in the order of their names, with its value. A family with no series is not gathered, and is empty.
func gathered(t fataler, registry prometheus.Gatherer, family string) map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := map[string]float64{}
	for _, gathered := range families {
		if gathered.GetName() != family {
			continue
		}
		for _, metric := range gathered.GetMetric() {
			var labels []string
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			value := metric.GetGauge().GetValue()
			if metric.GetCounter() != nil {
				value = metric.GetCounter().GetValue()
			}
			series[strings.Join(labels, ",")] = value
		}
	}
	return series
}

// treebindLabels is how gathered writes the labels of a series of the Treebind that has no others.
func treebindLabels(treebind *treebindv1alpha1.Treebind) string {
	return "name=" + treebind.Name + ",namespace=" + treebind.Namespace
}

// conditionLabels is how gathered writes the labels of a series of a condition or of a change of the outcome, in the
// order of their names: condition, name, namespace, reason, status.
func conditionLabels(treebind *treebindv1alpha1.Treebind, conditionType, value, reason string) string {
	return "condition=" + conditionType + "," + treebindLabels(treebind) + ",reason=" + reason + ",status=" + value
}

// metricsTreebind is a Treebind with a UID, as the API server gives it, and with the conditions.
func metricsTreebind(name, uid string, conditions ...metav1.Condition) *treebindv1alpha1.Treebind {
	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	treebind.Name, treebind.UID = name, types.UID(uid)
	for _, condition := range conditions {
		meta.SetStatusCondition(&treebind.Status.Conditions, condition)
	}
	return treebind
}

func condition(conditionType string, value metav1.ConditionStatus, reason string, changed time.Time) metav1.Condition {
	return metav1.Condition{
		Type: conditionType, Status: value, Reason: reason, Message: "a message", LastTransitionTime: metav1.NewTime(changed),
	}
}

func TestTheMetricsFollowTheConventionsOfPrometheus(t *testing.T) {
	m, registry := newTestMetrics(t)
	treebind := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	agreed := metav1.NewTime(agreedAt)
	treebind.Status.LastSyncTime = &agreed
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced)
	m.recordOutcomeChange(treebind, syncEvent{
		condition: treebindv1alpha1.ConditionSynced, status: yes, reason: reasonInSync,
	})
	m.recordSync(treebind, nil, agreedAt)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 5 {
		t.Fatalf("%d families are gathered, want the 5 that the operator makes", len(families))
	}

	problems, err := promlint.NewWithMetricFamilies(families).Lint()
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Errorf("%s: %s", problem.Metric, problem.Text)
	}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "treebind_") {
			t.Errorf("%s does not begin with the name of the project", family.GetName())
		}
	}
}

func TestNewMetricsRefusesARegistryThatHasThemAlready(t *testing.T) {
	registry := prometheus.NewRegistry()
	if _, err := NewMetrics(registry); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMetrics(registry); err == nil {
		t.Error("the metrics were registered twice")
	}
}

func TestAConditionHasOneSeriesThatSaysWhatItSaysNow(t *testing.T) {
	m, registry := newTestMetrics(t)
	treebind := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, unknown, reasonPending, agreedAt))
	expect := func(want ...string) {
		t.Helper()
		got := slices.Sorted(maps.Keys(gathered(t, registry, metricCondition)))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("series %q, want %q", got, want)
		}
		for labels, value := range gathered(t, registry, metricCondition) {
			if value != 1 {
				t.Errorf("%s = %v, want 1", labels, value)
			}
		}
	}

	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict)
	expect(conditionLabels(treebind, treebindv1alpha1.ConditionSynced, "Unknown", reasonPending))

	meta.SetStatusCondition(&treebind.Status.Conditions,
		condition(treebindv1alpha1.ConditionSynced, no, reasonPushRejected, agreedAt))
	meta.SetStatusCondition(&treebind.Status.Conditions,
		condition(treebindv1alpha1.ConditionConflict, no, reasonNoConflict, agreedAt))
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict)
	expect(conditionLabels(treebind, treebindv1alpha1.ConditionSynced, "False", reasonPushRejected),
		conditionLabels(treebind, treebindv1alpha1.ConditionConflict, "False", reasonNoConflict))

	// The same status with another reason is another series too.
	meta.SetStatusCondition(&treebind.Status.Conditions,
		condition(treebindv1alpha1.ConditionSynced, no, reasonFetchFailed, agreedAt))
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict)
	expect(conditionLabels(treebind, treebindv1alpha1.ConditionSynced, "False", reasonFetchFailed),
		conditionLabels(treebind, treebindv1alpha1.ConditionConflict, "False", reasonNoConflict))

	meta.RemoveStatusCondition(&treebind.Status.Conditions, treebindv1alpha1.ConditionConflict)
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionConflict)
	expect(conditionLabels(treebind, treebindv1alpha1.ConditionSynced, "False", reasonFetchFailed))
}

// The reconcile of the spec and the poller each record the conditions that they own, from copies of the Treebind
// that are not of the same moment: one must not put back what the other changed.
func TestEachReconcilerRecordsOnlyTheConditionsItIsGiven(t *testing.T) {
	m, registry := newTestMetrics(t)
	polled := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, no, reasonPushRejected, agreedAt),
		condition(treebindv1alpha1.ConditionReady, yes, treebindv1alpha1.ReasonSyncPodReady, agreedAt))
	reconciled := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt),
		condition(treebindv1alpha1.ConditionReady, no, treebindv1alpha1.ReasonNotSynced, agreedAt))

	m.recordConditions(polled, treebindv1alpha1.ConditionSynced)
	m.recordConditions(reconciled, treebindv1alpha1.ConditionReady)

	got := slices.Sorted(maps.Keys(gathered(t, registry, metricCondition)))
	want := []string{
		conditionLabels(polled, treebindv1alpha1.ConditionReady, "False", treebindv1alpha1.ReasonNotSynced),
		conditionLabels(polled, treebindv1alpha1.ConditionSynced, "False", reasonPushRejected),
	}
	if !slices.Equal(got, want) {
		t.Errorf("series %q, want %q", got, want)
	}
}

func TestTheTimeLastSeenInSync(t *testing.T) {
	poll := agreedAt.Add(time.Hour)
	inSync := condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt)
	failed := condition(treebindv1alpha1.ConditionSynced, no, reasonPushRejected, agreedAt.Add(-time.Hour))
	tests := []struct {
		name   string
		status metav1.Condition
		polled *metav1.Condition
		want   time.Time
		absent bool
	}{
		{"a poll that read InSync", inSync, &inSync, poll, false},
		{"a poll that read a failure, with no time before it: when Synced changed", failed, &failed,
			agreedAt.Add(-time.Hour), false},
		{"a poll that read nothing, with no time before it, of a Treebind that the status says is in sync",
			inSync, nil, poll, false},
		{"a poll that read nothing, of a Treebind that is not", failed, nil, agreedAt.Add(-time.Hour), false},
		{"a Treebind that has no report yet: since the reconcile gave it Synced",
			condition(treebindv1alpha1.ConditionSynced, unknown, treebindv1alpha1.ReasonAwaitingAgentReport, agreedAt),
			nil, agreedAt, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, registry := newTestMetrics(t)
			treebind := metricsTreebind(testTreebindName, "one", tt.status)

			m.recordSync(treebind, tt.polled, poll)

			want := map[string]float64{treebindLabels(treebind): seconds(tt.want)}
			if got := gathered(t, registry, metricInSyncSeen); !maps.Equal(got, want) {
				t.Errorf("series %v, want %v", got, want)
			}
		})
	}

	t.Run("no Synced condition: no series", func(t *testing.T) {
		m, registry := newTestMetrics(t)
		m.recordSync(metricsTreebind(testTreebindName, "one"), nil, poll)
		if got := gathered(t, registry, metricInSyncSeen); len(got) != 0 {
			t.Errorf("series %v, want none", got)
		}
	})

	t.Run("it moves with each poll that reads InSync, and only with those", func(t *testing.T) {
		m, registry := newTestMetrics(t)
		treebind := metricsTreebind(testTreebindName, "one", inSync)
		expect := func(at time.Time) {
			t.Helper()
			want := map[string]float64{treebindLabels(treebind): seconds(at)}
			if got := gathered(t, registry, metricInSyncSeen); !maps.Equal(got, want) {
				t.Errorf("series %v, want %v", got, want)
			}
		}

		m.recordSync(treebind, &inSync, poll)
		expect(poll)
		// The status says False since a minute later, which is when Synced changed; the time stays.
		meta.SetStatusCondition(&treebind.Status.Conditions, condition(treebindv1alpha1.ConditionSynced, no,
			reasonPushRejected, poll.Add(time.Minute)))
		failedNow := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionSynced)
		m.recordSync(treebind, failedNow, poll.Add(time.Minute))
		m.recordSync(treebind, nil, poll.Add(2*time.Minute))
		expect(poll)
		m.recordSync(treebind, &inSync, poll.Add(3*time.Minute))
		expect(poll.Add(3 * time.Minute))
	})

	t.Run("a Treebind made again with the name takes its time from its own status", func(t *testing.T) {
		m, registry := newTestMetrics(t)
		first := metricsTreebind(testTreebindName, "one", inSync)
		m.recordSync(first, &inSync, poll)
		again := metricsTreebind(testTreebindName, "two",
			condition(treebindv1alpha1.ConditionSynced, unknown, treebindv1alpha1.ReasonAwaitingAgentReport,
				poll.Add(time.Hour)))

		m.recordSync(again, nil, poll.Add(time.Hour))

		want := map[string]float64{treebindLabels(again): seconds(poll.Add(time.Hour))}
		if got := gathered(t, registry, metricInSyncSeen); !maps.Equal(got, want) {
			t.Errorf("series %v, want %v", got, want)
		}
	})
}

func TestTheIntervalAndTheTimeOfTheLastSyncedCommit(t *testing.T) {
	m, registry := newTestMetrics(t)
	treebind := metricsTreebind(testTreebindName, "one")
	other := metricsTreebind("other", "two")
	other.Spec.Sync.Interval = &metav1.Duration{Duration: 5 * time.Minute}

	m.recordSync(treebind, nil, agreedAt)
	m.recordSync(other, nil, agreedAt)

	if got, want := gathered(t, registry, metricInterval), map[string]float64{
		treebindLabels(treebind): 30, treebindLabels(other): 300,
	}; !maps.Equal(got, want) {
		t.Errorf("intervals %v, want %v: the default of the agent when the spec has none", got, want)
	}
	if got := gathered(t, registry, metricLastSynced); len(got) != 0 {
		t.Errorf("a time of the last synced commit %v before the status has one", got)
	}

	agreed := metav1.NewTime(agreedAt)
	treebind.Status.LastSyncTime = &agreed
	m.recordSync(treebind, nil, agreedAt.Add(time.Hour))

	if got, want := gathered(t, registry, metricLastSynced), map[string]float64{
		treebindLabels(treebind): float64(agreedAt.Unix()),
	}; !maps.Equal(got, want) {
		t.Errorf("times %v, want %v", got, want)
	}
}

func TestTheChangesOfTheOutcomeAreCountedByWhatTheyChangedTo(t *testing.T) {
	m, registry := newTestMetrics(t)
	treebind := metricsTreebind(testTreebindName, "one")
	rejected := syncEvent{condition: treebindv1alpha1.ConditionSynced, status: no, reason: reasonPushRejected}
	resolved := syncEvent{condition: treebindv1alpha1.ConditionConflict, status: no, reason: reasonResolved}

	m.recordOutcomeChange(treebind, rejected)
	m.recordOutcomeChange(treebind, resolved)
	m.recordOutcomeChange(treebind, rejected)

	want := map[string]float64{
		conditionLabels(treebind, treebindv1alpha1.ConditionSynced, "False", reasonPushRejected): 2,
		conditionLabels(treebind, treebindv1alpha1.ConditionConflict, "False", reasonResolved):   1,
	}
	if got := gathered(t, registry, metricOutcomeChanges); !maps.Equal(got, want) {
		t.Errorf("counts %v, want %v", got, want)
	}
}

func TestATreebindThatIsGoneHasNoSeries(t *testing.T) {
	m, registry := newTestMetrics(t)
	agreed := metav1.NewTime(agreedAt)
	record := func(treebind *treebindv1alpha1.Treebind) {
		treebind.Status.LastSyncTime = &agreed
		synced := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionSynced)
		m.recordConditions(treebind, treebindv1alpha1.ConditionSynced, treebindv1alpha1.ConditionReady)
		m.recordOutcomeChange(treebind, syncEvent{condition: synced.Type, status: synced.Status, reason: synced.Reason})
		m.recordSync(treebind, synced, agreedAt)
	}
	gone := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt),
		condition(treebindv1alpha1.ConditionReady, yes, treebindv1alpha1.ReasonSyncPodReady, agreedAt))
	// The same name in another namespace, and another name in the same one, stay.
	elsewhere := metricsTreebind(testTreebindName, "two",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	elsewhere.Namespace = "other"
	beside := metricsTreebind(testTreebindName+"-2", "three",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	for _, treebind := range []*treebindv1alpha1.Treebind{gone, elsewhere, beside} {
		record(treebind)
	}

	m.forget(types.NamespacedName{Namespace: gone.Namespace, Name: gone.Name})

	for _, family := range []string{metricCondition, metricOutcomeChanges, metricInterval, metricLastSynced, metricInSyncSeen} {
		series := gathered(t, registry, family)
		for labels := range series {
			if strings.Contains(labels, treebindLabels(gone)+",") || strings.HasSuffix(labels, treebindLabels(gone)) {
				t.Errorf("%s still has the series %s", family, labels)
			}
		}
		for _, kept := range []*treebindv1alpha1.Treebind{elsewhere, beside} {
			if !slices.ContainsFunc(slices.Collect(maps.Keys(series)), func(labels string) bool {
				return strings.Contains(labels, treebindLabels(kept))
			}) {
				t.Errorf("%s lost the series of %s/%s", family, kept.Namespace, kept.Name)
			}
		}
	}

	// What was remembered of it is gone too: the same condition is a series again, and the time is taken anew.
	again := metricsTreebind(testTreebindName, "four",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	m.recordConditions(again, treebindv1alpha1.ConditionSynced)
	m.recordSync(again, nil, agreedAt.Add(time.Hour))
	if _, ok := gathered(t, registry, metricCondition)[conditionLabels(again, treebindv1alpha1.ConditionSynced, "True",
		reasonInSync)]; !ok {
		t.Error("the condition of a Treebind made again with the name has no series")
	}
	if got := gathered(t, registry, metricInSyncSeen)[treebindLabels(again)]; got != seconds(agreedAt.Add(time.Hour)) {
		t.Errorf("the time of a Treebind made again with the name is %v", got)
	}
}

func TestNilMetricsRecordNothingAndDoNotFail(t *testing.T) {
	var m *Metrics
	treebind := metricsTreebind(testTreebindName, "one",
		condition(treebindv1alpha1.ConditionSynced, yes, reasonInSync, agreedAt))
	m.recordConditions(treebind, treebindv1alpha1.ConditionSynced)
	m.recordOutcomeChange(treebind, syncEvent{})
	m.recordSync(treebind, nil, agreedAt)
	m.forget(types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name})
}
