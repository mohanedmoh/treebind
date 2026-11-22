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
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	agentconfig "github.com/mohanedmoh/treebind/internal/agent/config"
)

// The metrics of the Treebinds that the operator serves (docs/metrics-and-events.md, docs/adr/0038). They are made where
// the conditions are made, by the reconcilers, so only the leader has them, and they say what the status says: nothing
// of the agent's own text, and no commit, file, message or time in a label. Their names and labels are API, as the
// reasons of the conditions are, because alerts and dashboards match on them.
const (
	metricCondition      = "treebind_status_condition"
	metricOutcomeChanges = "treebind_sync_outcome_changes_total"
	metricInterval       = "treebind_sync_interval_seconds"
	metricLastSynced     = "treebind_last_synced_commit_timestamp_seconds"
	metricInSyncSeen     = "treebind_in_sync_last_seen_timestamp_seconds"

	labelNamespace = "namespace"
	labelName      = "name"
	labelCondition = "condition"
	labelStatus    = "status"
	labelReason    = "reason"
)

// Metrics holds the metric families of the Treebinds, and what it needs to remember to keep one series for each
// condition of each Treebind. A nil *Metrics records nothing, so that a reconciler that a test builds without it works.
type Metrics struct {
	condition      *prometheus.GaugeVec
	outcomeChanges *prometheus.CounterVec
	interval       *prometheus.GaugeVec
	lastSynced     *prometheus.GaugeVec
	inSyncSeen     *prometheus.GaugeVec

	mu sync.Mutex
	// conditions are the status and the reason of the series that each condition of each Treebind has, so that it
	// is deleted when they change and the condition keeps one series.
	conditions map[conditionKey]statusReason
	// inSync is when each Treebind was last seen in sync, for the Treebind of that UID.
	inSync map[types.NamespacedName]seenInSync
}

type conditionKey struct {
	treebind      types.NamespacedName
	conditionType string
}

type statusReason struct {
	status, reason string
}

type seenInSync struct {
	uid types.UID
	at  time.Time
}

// NewMetrics makes the metric families and registers them, as the operator does with the registry that its metrics
// endpoint serves.
func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	treebind := []string{labelNamespace, labelName}
	outcome := []string{labelNamespace, labelName, labelCondition, labelStatus, labelReason}
	m := &Metrics{
		condition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricCondition,
			Help: "A condition of the status of a Treebind, with its status and reason. The value is 1, and a condition " +
				"has one series, of what it says now.",
		}, outcome),
		outcomeChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricOutcomeChanges,
			Help: "Changes of the Synced and Conflict conditions of a Treebind that the operator wrote, by the status " +
				"and the reason that they changed to. Each is also an event on the Treebind.",
		}, outcome),
		interval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricInterval,
			Help: "How often the sync agent of a Treebind pulls, spec.sync.interval, in seconds.",
		}, treebind),
		lastSynced: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricLastSynced,
			Help: "When the volume and the repository of a Treebind came to agree at status.lastSyncedCommit, " +
				"status.lastSyncTime, in seconds since the epoch. It moves only when the commit changes.",
		}, treebind),
		inSyncSeen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricInSyncSeen,
			Help: "When the operator last read a report of the sync agent of a Treebind that said InSync, in seconds " +
				"since the epoch. Until it has read one since it began to lead, when Synced last changed its status.",
		}, treebind),
		conditions: map[conditionKey]statusReason{},
		inSync:     map[types.NamespacedName]seenInSync{},
	}
	collectors := []prometheus.Collector{m.condition, m.outcomeChanges, m.interval, m.lastSynced, m.inSyncSeen}
	errs := make([]error, 0, len(collectors))
	for _, collector := range collectors {
		errs = append(errs, registerer.Register(collector))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// recordConditions makes the series of each of the condition types say what the status of the Treebind says. A
// condition that changed its status or its reason gets the series of the new ones before the old series is deleted,
// so that a scrape in between sees the condition, and a condition that is not in the status has no series.
func (m *Metrics) recordConditions(treebind *treebindv1alpha1.Treebind, conditionTypes ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name := types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name}
	for _, conditionType := range conditionTypes {
		key := conditionKey{treebind: name, conditionType: conditionType}
		old, had := m.conditions[key]
		condition := meta.FindStatusCondition(treebind.Status.Conditions, conditionType)
		if condition == nil {
			if had {
				m.condition.DeleteLabelValues(name.Namespace, name.Name, conditionType, old.status, old.reason)
				delete(m.conditions, key)
			}
			continue
		}
		now := statusReason{status: string(condition.Status), reason: condition.Reason}
		if had && old == now {
			continue
		}
		m.condition.WithLabelValues(name.Namespace, name.Name, conditionType, now.status, now.reason).Set(1)
		if had {
			m.condition.DeleteLabelValues(name.Namespace, name.Name, conditionType, old.status, old.reason)
		}
		m.conditions[key] = now
	}
}

// recordOutcomeChange counts a change of Synced or Conflict, which is also an event.
func (m *Metrics) recordOutcomeChange(treebind *treebindv1alpha1.Treebind, change syncEvent) {
	if m == nil {
		return
	}
	m.outcomeChanges.WithLabelValues(treebind.Namespace, treebind.Name, change.condition, string(change.status),
		change.reason).Inc()
}

// recordSync sets the interval of the Treebind, the time of status.lastSyncTime, and when it was last seen in sync.
// polled is the Synced condition that the poll made from the agent's report, nil when it made none; now is the time of
// the poll.
//
// A poll that read a report that says InSync makes now the time it was seen in sync. Any other poll leaves the time,
// except the first one after the operator began to lead, which has no time yet: it takes now when the status says
// Synced True, which the poller before it wrote from a report that said InSync, and otherwise the time Synced last
// changed its status, which is no earlier than the last time it was in sync. So the time never says that a Treebind was
// in sync later than it was, and a leader that starts with a Treebind that has not synced for a day says so.
func (m *Metrics) recordSync(treebind *treebindv1alpha1.Treebind, polled *metav1.Condition, now time.Time) {
	if m == nil {
		return
	}
	name := types.NamespacedName{Namespace: treebind.Namespace, Name: treebind.Name}
	interval := agentconfig.DefaultSyncInterval
	if treebind.Spec.Sync.Interval != nil {
		interval = treebind.Spec.Sync.Interval.Duration
	}
	m.interval.WithLabelValues(name.Namespace, name.Name).Set(interval.Seconds())
	if synced := treebind.Status.LastSyncTime; synced != nil {
		m.lastSynced.WithLabelValues(name.Namespace, name.Name).Set(seconds(synced.Time))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	seen, known := m.inSync[name]
	known = known && seen.uid == treebind.UID
	switch {
	case polled != nil && polled.Status == metav1.ConditionTrue:
		seen = seenInSync{uid: treebind.UID, at: now}
	case known:
		return
	default:
		synced := meta.FindStatusCondition(treebind.Status.Conditions, treebindv1alpha1.ConditionSynced)
		switch {
		case synced == nil:
			return
		case synced.Status == metav1.ConditionTrue:
			seen = seenInSync{uid: treebind.UID, at: now}
		default:
			seen = seenInSync{uid: treebind.UID, at: synced.LastTransitionTime.Time}
		}
	}
	m.inSync[name] = seen
	m.inSyncSeen.WithLabelValues(name.Namespace, name.Name).Set(seconds(seen.at))
}

// forget deletes every series of the Treebind and what is remembered of it, for a Treebind that is gone or is being
// deleted.
func (m *Metrics) forget(name types.NamespacedName) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	labels := prometheus.Labels{labelNamespace: name.Namespace, labelName: name.Name}
	m.condition.DeletePartialMatch(labels)
	m.outcomeChanges.DeletePartialMatch(labels)
	m.interval.DeletePartialMatch(labels)
	m.lastSynced.DeletePartialMatch(labels)
	m.inSyncSeen.DeletePartialMatch(labels)
	for key := range m.conditions {
		if key.treebind == name {
			delete(m.conditions, key)
		}
	}
	delete(m.inSync, name)
}

// seconds is the time in seconds since the epoch, as Prometheus keeps a timestamp.
func seconds(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}
