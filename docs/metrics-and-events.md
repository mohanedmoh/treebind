# Metrics and events: seeing the outcome of a sync

Every outcome of a sync is a condition, an event and a metric (NFR-6). The operator makes all three in the one place where
the outcome changes: when it writes `Synced` and `Conflict` from the report of the sync agent
([the status page](agent-status.md#what-synced-and-conflict-say)). An event says that something changed and when, the
metrics let Prometheus keep the history and alert on it, and the conditions say what holds now. The decisions are in
[ADR 0038](adr/0038-the-operator-records-an-event-and-a-metric-where-it-writes-a-condition-of-the-sync.md).

Nothing of this comes from the sync pod: the agent has no metrics endpoint, and the operator makes the events and the metrics
from the report that it reads every 30 seconds. So they are as fresh as the conditions, and they show the changes that a poll
sees, not every pull and push of the agent.

## Events

The operator records an event on the Treebind, in its namespace, for each change of the status or the reason of `Synced` or
`Conflict` that it writes. A poll that finds what the last one found records none, so a push that the host refuses at every
interval for an hour is one event, at the start.

<!-- tested: read-the-events -->
```sh
kubectl events -n <namespace> --for treebind/<name>
kubectl describe treebind -n <namespace> <name>
```

For a Treebind that synced and then had a push refused by the host, the first command printed this on a kind cluster, where the
commit was another:

<!-- output: events-of-a-refused-push -->
```
LAST SEEN   TYPE      REASON         OBJECT         MESSAGE
61s         Normal    SyncPending    Treebind/dod   Synced is Unknown: The sync agent has not seen the volume and the repository agree since it started, and no step has failed: the lock, the checkout, the first pull or the first push is still to come
29s         Normal    InSync         Treebind/dod   Synced is True: No step of the sync failed. The volume and the repository last agreed at commit 0123456789ab
2s          Warning   PushRejected   Treebind/dod   Synced is False: The last push failed: the repository refused the push, as for a protected branch, a hook or a rule. The volume and the repository last agreed at commit 0123456789ab. The log of the sync pod says more
```

| What changed | Type | Reason |
| :-- | :-- | :-- |
| `Synced` to `True` | `Normal` | `InSync` |
| `Synced` to `False`, or to another reason while `False` | `Warning` | The reason of the step that failed: `PushRejected`, `FetchFailed`, `TooManyDeletions` and the others of [the table](recovering-from-a-conflict-or-a-rejected-push.md#which-section-deals-with-a-reason), or `SyncFailed` |
| `Synced` to `Unknown` because the operator has not read the report for 3 minutes | `Warning` | `AgentUnreachable` |
| `Synced` to `Unknown` for another reason | `Normal` | `SyncPending`, as after every start of the sync pod, or `AgentOutdated`, while the sync pod is replaced |
| `Conflict` to `True`: a conflict that was not resolved | `Warning` | The reason of the update that could not be made: `Blocked`, `TooManyDeletions`, `TooManyCommits`, `Refused` or `PullFailed` |
| `Conflict` to `False` with `ConflictResolved`: the policy resolved a conflict | `Normal` | `ConflictResolved` |
| `Conflict` from `True` or `ConflictResolved` to `NoConflict` | `Normal` | `NoConflict` |

- **The reason is the reason of the condition**, so a filter or an alert on events uses the words of the conditions. The
  message begins with the condition and its status, `Synced is False:` or `Conflict is True:`, and goes on with the message of
  the condition. A conflict that was not resolved is two events with the same reason, one for each condition.
- **The message holds nothing that the agent wrote**: no name of a file and no words of the Git host, as the conditions do not.
  Those are in the checks of the report and in the log of the sync pod
  ([reading the agent's log](recovering-from-a-conflict-or-a-rejected-push.md#reading-the-agents-log-and-report)).
- **No event while `Conflict` is `Unknown`**, which it is exactly when `Synced` is, with the same reason, and none when it comes
  to `NoConflict` from `Unknown`, which every start of the agent makes it do.
- **No event for a newer commit alone.** A busy Treebind agrees at a new commit at almost every poll, which
  `status.lastSyncedCommit` and the metric `treebind_last_synced_commit_timestamp_seconds` say.
- **The events are of the `events.k8s.io` API**, from the reporting controller `treebind-operator`, and their action is the
  condition that changed: `Synced` or `Conflict`. `kubectl get events` shows them too. The API server keeps an event for an
  hour by default (its flag `--event-ttl`), so the history is for Prometheus.

## Metrics

The operator serves them on its metrics endpoint, with the metrics of controller-runtime: port 8443, HTTPS, and only for a
caller that may `get` the path `/metrics`, which the ClusterRole `treebind-metrics-reader` grants. The chart's ServiceMonitor
(`prometheus.enabled`) scrapes it. To read them by hand, for an operator installed from `config/` in `treebind-system` (with the
chart, the namespace is the one of the release, and the ClusterRole is `<release>-treebind-metrics-reader` unless the release is
called `treebind`):

<!-- tested: read-the-metrics -->
```sh
kubectl -n treebind-system create serviceaccount metrics-probe
kubectl create clusterrolebinding metrics-probe --clusterrole=treebind-metrics-reader --serviceaccount=treebind-system:metrics-probe
TOKEN=$(kubectl -n treebind-system create token metrics-probe)
# The pod that holds the lease is the one that has the metrics of the Treebinds:
LEADER=$(kubectl -n treebind-system get lease c4984a8a.example.org -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1)
kubectl -n treebind-system port-forward "pod/$LEADER" 8443:8443 &
FORWARD=$!
# The port-forward listens a moment after it starts, so ask until it answers, for 10 seconds at most:
for i in $(seq 10); do curl -sk -o /dev/null https://127.0.0.1:8443/metrics && break; sleep 1; done
curl -sk -o /dev/null https://127.0.0.1:8443/metrics || echo "the port-forward did not answer: check the pod name and that port 8443 is free"
curl -sk -H "Authorization: Bearer $TOKEN" https://127.0.0.1:8443/metrics | grep '^treebind_'
kill $FORWARD
```

<!-- families -->
| Name | Type | Labels | What it says |
| :-- | :-- | :-- | :-- |
| `treebind_status_condition` | gauge | `namespace`, `name`, `condition`, `status`, `reason` | A condition of the Treebind with its status and its reason, of value 1. Each condition has one series, of what it says now: `Ready`, `SpecSupported`, `Synced`, `Conflict` and `AgentUnreachable` |
| `treebind_sync_outcome_changes_total` | counter | `namespace`, `name`, `condition`, `status`, `reason` | The changes of `Synced` and `Conflict` that the operator wrote, by what they changed to. One for each event above |
| `treebind_sync_interval_seconds` | gauge | `namespace`, `name` | `spec.sync.interval`, in seconds |
| `treebind_last_synced_commit_timestamp_seconds` | gauge | `namespace`, `name` | `status.lastSyncTime`: when the volume and the repository came to agree at `status.lastSyncedCommit`. It moves only when the commit changes, and is absent until there is one |
| `treebind_in_sync_last_seen_timestamp_seconds` | gauge | `namespace`, `name` | When the operator last read a report that said `InSync`. It stops while the sync fails, is pending, or cannot be read, and while the sync pod does not run |

`namespace` and `name` are those of the Treebind. The times are in seconds since the epoch. A few things to know:

- **`treebind_in_sync_last_seen_timestamp_seconds` is the one to alert on**, and not `treebind_last_synced_commit_timestamp_seconds`.
  A Treebind with nothing to sync keeps the time of its last commit for days and is well, as
  [noticing that nothing has synced](recovering-from-a-conflict-or-a-rejected-push.md#noticing-that-nothing-has-synced-for-a-while)
  says of `status.lastSyncTime`. The time in sync is held in the memory of the operator. A leader that has just started, after
  a restart or a failover, takes for a Treebind whose `Synced` is not `True` the time of its last change of status, which is no
  earlier than the last time it was in sync, so an alert on it can come late after a failover, and never wrongly.
- **The counter counts changes, not tries.** A push that is refused at every interval is one change to `PushRejected`. A
  change that lasts less than a scrape is counted, where the gauge of the condition can miss it. The counts begin at zero when a
  replica begins to lead.
- **Only the leader has them.** The operator runs as two replicas, and only the one that holds the lease reconciles
  ([operator replicas](operator-replicas.md)). The other serves the endpoint with no series of a Treebind. After a failover the
  new leader makes every series again at its first poll of each Treebind, within a poll interval.
- **A Treebind that is deleted takes its series with it**, the counters too, as soon as the operator sees the deletion.
- **How many series.** A Treebind has 8 gauge series, and a counter series for each status and reason that `Synced` and
  `Conflict` have changed to since the replica began to lead: a handful, and at most about 40. No label holds a commit, a file,
  a message or a time.

### Prometheus and the label `namespace`

The Prometheus Operator gives every target of a ServiceMonitor the label `namespace`, which here is the namespace of the
operator, as most scrape configurations for Kubernetes do, and Prometheus keeps the label of the target unless the scrape honors
the labels of the series. The ServiceMonitors of the chart and of `config/prometheus` set `honorLabels: true`, so `namespace` is
the namespace of the Treebind. With a scrape configuration of your own, set `honor_labels: true` for the job, or read the
namespace of the Treebind from `exported_namespace` in the expressions below.

## Alerts

The project ships no rule, because how long a sync may wait before a person is called is for each site to say. These are
the expressions to start from.

**No sync has succeeded for several intervals.** The time that the operator last saw the Treebind in sync, against five of
its intervals, and never less than 10 minutes, because the operator reads a report every 30 seconds, a fifth more or less,
and the scrape and the evaluation add their own intervals:

<!-- tested: promql-not-in-sync -->
```promql
time() - treebind_in_sync_last_seen_timestamp_seconds
  > clamp_min(5 * treebind_sync_interval_seconds, 600)
```

It fires for a sync that fails, for one that is pending, as with a second agent on the claim, for an agent that the operator
cannot read, and for a sync pod that does not run. It does not fire for a Treebind with nothing to sync.

<!-- tested: promql-others -->
```promql
# A step failed for a reason that waits for a person, which turns Ready False (agent-status.md#ready):
treebind_status_condition{condition="Ready", reason="NotSynced"} == 1
# A conflict that the policy could not resolve:
treebind_status_condition{condition="Conflict", status="True"} == 1
# A push that the host refused, in the last hour, even if it went through since:
increase(treebind_sync_outcome_changes_total{condition="Synced", reason="PushRejected"}[1h]) > 0
# The operator has not been able to read the report of the agent for 3 minutes:
treebind_status_condition{condition="AgentUnreachable", status="True"} == 1
# The conflicts that the policy resolved, in the last day:
sum by (namespace, name) (increase(treebind_sync_outcome_changes_total{condition="Conflict", reason="ConflictResolved"}[1d]))
```

A `PrometheusRule` for the Prometheus Operator, to adapt:

<!-- tested: prometheus-rule -->
```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: treebind
  namespace: treebind-system
spec:
  groups:
  - name: treebind
    rules:
    - alert: TreebindNotInSync
      expr: time() - treebind_in_sync_last_seen_timestamp_seconds > clamp_min(5 * treebind_sync_interval_seconds, 600)
      labels:
        severity: warning
      annotations:
        summary: Treebind {{ $labels.namespace }}/{{ $labels.name }} has not synced for five intervals
        description: kubectl describe treebind -n {{ $labels.namespace }} {{ $labels.name }} says why.
    - alert: TreebindNeedsAPerson
      expr: treebind_status_condition{condition="Ready", reason="NotSynced"} == 1
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: A step of the sync of Treebind {{ $labels.namespace }}/{{ $labels.name }} failed for a reason that a person puts right
    - alert: TreebindConflictNotResolved
      expr: treebind_status_condition{condition="Conflict", status="True"} == 1
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: A conflict of Treebind {{ $labels.namespace }}/{{ $labels.name }} waits for a person ({{ $labels.reason }})
```

The page for what to do is [recovering from a conflict or a rejected push](recovering-from-a-conflict-or-a-rejected-push.md).

## What is not there

- **Metrics of the sync agent itself**: how long a pull, a scan or a push takes, how many files a commit has, how old the writer
  lock is. The agent has no metrics endpoint, and the operator sees its outcomes at the poll, not each step.
- **A count of every try.** The operator sees the conditions change, so a conflict resolved twice between two polls, or a push
  refused at every interval, is one change.
- **Events for `Ready`, `SpecSupported` and `AgentUnreachable`.** `Ready` follows `Synced`, and the operator that loses the
  agent for 3 minutes says so in an event of `Synced`. Their state is in `treebind_status_condition`.
- **A rule in the chart.** The expressions above are for your own rules.
