# 0038. The operator records an event and a metric where it writes a condition of the sync

- Status: accepted
- Date: 2026-11-22
- Issue: #47
- Extends: [0004](0004-treebind-status-conditions.md), [0032](0032-the-operator-may-do-what-it-does-and-can-be-limited-to-namespaces.md), [0037](0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md)

## Context

NFR-6 asks that every sync outcome is visible as a condition, an event and a metric, and R-29 that someone notices when a
conflict or a refused push stops the sync: "alert when no sync has succeeded for several intervals". ADR 0004 left events and
metrics to this issue, and ADR 0037 named the place for them: the function that makes `Synced` and `Conflict` is the one place
where an outcome changes, so an event hung there is one for each change and not one for each poll.

The conditions were all there was. The recovery guide could say since when `Synced` had said what it said, and that an old
`status.lastSyncTime` is no sign of trouble, and nothing more: there was no source for an alert.

Two places could make them. The agent sees every pull, scan, commit and push, but it holds no Kubernetes credential, so it
cannot record an event, and a metrics endpoint on every sync pod is a scrape target for each Treebind, with its own port,
authentication and network policy, in clusters where the operator's call to the pod is already what a policy or a mesh can
block (ADR 0015). The operator reads the agent's report every 30 seconds, already writes the conditions from it, already serves
a secured metrics endpoint, and its manager has an event recorder.

## Decision

### Made where the conditions are made

The status poller makes the events and the metrics of what it writes, from the conditions before the poll and after it, once
the write that carries the change has succeeded, or when the poll changed nothing. A write that loses a conflict with the
reconcile of the spec makes nothing, and the poll that then writes the change makes it, so a change makes one event however
the writes go. The reconcile of the spec records the conditions that it owns, `Ready` and `SpecSupported`, in the same gauge.
Each records only its own conditions, from its own copy of the Treebind, because the two copies are not of the same moment and
one must not put back what the other changed.

Nothing comes from the agent. The events and the metrics say what the conditions say, as fresh as the poll, and nothing of the
agent's own text: the conditions' messages are fixed sentences (ADR 0037), and no label holds a commit, a file, a message or a
time.

### The events

One for each change of the status or the reason of `Synced` or `Conflict`, on the Treebind, through the `events.k8s.io` API with
the recorder of the manager, as the reporting controller `treebind-operator`:

- **The reason is the reason of the condition.** Those are API already (ADR 0037), so nothing is added to learn, and a filter on
  events uses the words of the conditions.
- **`Warning`** for `Synced` `False`, for `Synced` `Unknown` with `AgentUnreachable`, and for `Conflict` `True`; **`Normal`**
  for the others.
- **The note** is the condition, its status and its message: `Synced is False: The last push failed: ...`.
- **The action is the condition.** The recorder of `events.k8s.io` folds two events of one object, type, reason and action into
  one series and keeps the first note, and `Synced` `False` and `Conflict` `True` of a conflict that was not resolved have the
  same reason and come from one write.
- **`Conflict` makes none while it is `Unknown`**, when it says what `Synced` says, nor when it comes to `NoConflict` from
  `Unknown`, which every start of the agent makes it do.
- **No event for a newer commit alone.** A busy Treebind agrees at a new commit at almost every poll; an event for each would
  double the writes that ADR 0037 counts, one for each new commit, for a fact that the status and a metric already hold.

The operator may create and patch `events` of `events.k8s.io` wherever the Treebinds are: in its ClusterRole, or in each Role of
a namespaced install, as ADR 0032 has it for every call. The `events` of the core group that its Role of leader election has
stay as they are, for the recorder that leader election uses.

### The metrics

On the metrics endpoint that the manager already serves, with its authentication, labelled with the `namespace` and the `name`
of the Treebind:

| Name | Type | What it says |
| :-- | :-- | :-- |
| `treebind_status_condition` | gauge | A condition with its status and its reason, value 1, one series for each condition |
| `treebind_sync_outcome_changes_total` | counter | The changes of `Synced` and `Conflict`, by what they changed to, one for each event |
| `treebind_sync_interval_seconds` | gauge | `spec.sync.interval` |
| `treebind_last_synced_commit_timestamp_seconds` | gauge | `status.lastSyncTime` |
| `treebind_in_sync_last_seen_timestamp_seconds` | gauge | When the operator last read a report that said `InSync` |

The names and the labels are API, as the reasons are: alerts and dashboards match on them.

**The state is one series of value 1 for each condition**, with the status and the reason as labels, as kube-state-metrics has
the reason a container waits. A change sets the new series before it deletes the old one, so a scrape always finds the
condition. The alternative was a series for each status with 0 and 1 and no reason, which says less, or one for each reason,
which is about 40 series for each Treebind where one does.

**R-29 is written on the time last seen in sync and the interval**: `time() - treebind_in_sync_last_seen_timestamp_seconds >
clamp_min(5 * treebind_sync_interval_seconds, 600)`. The time moves at each poll that reads `InSync` and at no other, so it stops
for a sync that fails, that is pending, whose agent the operator cannot read or whose pod does not run. The alternatives:

- `status.lastSyncTime` moves only with the commit (ADR 0037), so a Treebind with nothing to sync would fire.
- The time of the last change of `Synced`, which the status keeps, moves at every restart of the agent, so a pod that restarts
  every few minutes and never syncs would not fire.
- The gauge of `Synced` with a `for:` of a fixed time cannot say "intervals", which are a second for one Treebind and an hour for
  another, and loses its count when a failover leaves a gap in the series.

The time is held in memory, so a replica that begins to lead has none. For a Treebind whose `Synced` is `True` it takes the time
of its first poll, and for one whose `Synced` is not it takes the time of the last change of status of `Synced`, which is no
earlier than the last time that it was in sync. So an alert can come late after a failover, and never says that a Treebind is
out of sync when it is not. A Treebind that is recreated with the name starts again from its own status.

**The counter counts the changes that the operator wrote, not the tries of the agent.** A push refused at every interval for
an hour is one change; a change that lasts less than a scrape is counted where the gauge can miss it. The tries are only in the
agent, which has no endpoint.

**Cardinality**: 8 gauge series for each Treebind, and a counter series for each status and reason that it has changed to, a
handful in practice and about 40 at most. A Treebind that is gone or is being deleted has none: both reconcilers forget it.

**The label `namespace`** is also the label that Prometheus gives a target. The ServiceMonitors of the chart and of
`config/prometheus` honor the labels of the target, as the ServiceMonitor of kube-state-metrics does, so it is the namespace of
the Treebind; a scrape of one's own needs the same, or reads `exported_namespace`. The alternative was another name, such as
`treebind_namespace`, which every query would have to spell and no other operator's metrics use.

### Several replicas

Only the leader reconciles (ADR 0014), so only the leader records events and has series of a Treebind; the other replica
serves its endpoint with none. A new leader makes every gauge again at its first poll of each Treebind, starts its counters at
zero, and takes the times as above. The events are made against the status, which is stored, so a new leader does not repeat
the event of a change that the one before it wrote. The recorder sends events in the background, so an event of the last
moments of a leader that is killed can be lost; a leader that hands over flushes them.

### Not here

- **Metrics of the agent itself**, of each pull, scan, commit and push, of the age of the writer lock (R-8) and of the scans
  (R-11). They need an endpoint on the sync pod and its scraping, and are for an issue of their own.
- **Events for `Ready`, `SpecSupported` and `AgentUnreachable`.** `Ready` follows `Synced`, and an operator that cannot read the
  agent for 3 minutes says so in an event of `Synced`. The gauge has their state.
- **A rule in the chart.** The thresholds are a choice of each site; the documentation has the expressions and a
  `PrometheusRule` to adapt.

## Consequences

- `kubectl describe treebind` and `kubectl events --for treebind/<name>` show what changed and when: the first sync, a refused
  push, a conflict, the agent unreachable. The events last an hour on most clusters.
- An alert on a sync that has not succeeded for several intervals is one expression, and fires for a failing, a pending, an
  unreachable and a stopped sync alike, and not for a Treebind with nothing to sync.
- The operator writes an event for each change of the outcome, at most one for each poll of a Treebind and in practice a few a
  day, and serves a fixed number of series for each Treebind.
- The roles of the operator grow by `create` and `patch` on `events.k8s.io` `events`, in every namespace that it serves.
- Users of the chart's ServiceMonitor see the label `namespace` of the operator's own series unchanged and the namespace of the
  Treebind on the new ones; a scrape configuration of one's own needs `honor_labels: true`.
- [Metrics and events](../metrics-and-events.md) documents them, and the recovery guide points at the alert and the events.
- Tried on a kind cluster with two replicas: a Treebind that went from `SyncPending` to `InSync`, a push that a hook of the
  remote refused (a `Warning` with `PushRejected`, and `InSync` again once the hook was gone), the series on the leader and none
  on the other replica, the endpoint refusing a caller without a token, a failover after which the new leader had the gauges
  and repeated no event, and a deleted Treebind leaving no series. Not tried there: a conflict that could not be resolved, and
  an operator with many Treebinds.
