# 0040. Ready names a sync pod that cannot be placed and a node that is not ready, and Synced is Unknown without a report

- Status: accepted
- Date: 2026-11-24
- Issue: #266, #267
- Extended by: [0046](0046-the-sync-pod-runs-beside-the-application-on-a-claim-that-one-node-mounts.md), which says in the
  message of `SyncPodUnschedulable` that a pod with the affinity to the application may only run beside it
- Extends: [0004](0004-treebind-status-conditions.md), [0015](0015-the-operator-reads-the-agents-status.md), [0037](0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md), [0039](0039-ready-names-an-http-repository-address-and-a-sync-container-killed-for-memory-and-the-operator-may-get-a-pod.md)

## Context

A release test of v0.0.1-alpha.3 on a kind cluster of three nodes found two sync pods that could not run, and Treebinds that
did not say so.

**A drain (#266).** After `kubectl drain` of the node of a claim of local-path, the new sync pod stayed Pending for as long as the
node was cordoned, because only that node can mount the volume. A Pending pod has no address, so its name does not resolve, and
`AgentUnreachable` said `Unknown` with `AgentNotRunning`, which never turns `True` (ADR 0015). ADR 0037 leaves `Synced` and
`Conflict` as they are while `AgentUnreachable` is `Unknown`, so `Synced` said `True` with `InSync` for the 4.5 minutes of the
test, while a pushed commit and a file that the application wrote were not synced. `Ready` said `SyncPodNotReady`, "0 of 1 sync
pods ready", and nothing of why.

**A lost node (#267).** When the node of the sync pod stopped, `Ready` said `SyncPodNotReady` once the node was NotReady, and after
3 minutes `AgentUnreachable` was `True` with `ConnectionFailed`, whose message sends a reader to a network policy or a service
mesh. The pod stayed Running, then Terminating, on the lost node. Nothing named the node.

The reconcile already reads the sync pod by name while no sync pod is ready (ADR 0039). What the pod carries names both causes.

## Decision

### `SyncPodUnschedulable`

When the pod is `Pending` and its condition `PodScheduled` is `False` with the reason `Unschedulable`, `Ready` is `False` with the
reason `SyncPodUnschedulable`. The message is the one of `SyncPodNotReady`, the name of the pod and the scheduler's words, which
name what no node has: `0 of 1 sync pods ready: the sync pod app-sync-0 is Pending, because the scheduler cannot place it: 0/3
nodes are available: ..., 1 node(s) had volume node affinity conflict, 1 node(s) were unschedulable. ...`. The words are cut at
1024 bytes, as the error of `ReconcileFailed` is (ADR 0008), and without any the message says to run `kubectl describe pod`. They
come from the cluster and not from a Git host. They can change from one pass to the next while a cluster autoscaler adds or
removes nodes, and `Ready` is then written again each time. A pod that waits for a scheduling gate, or that is placed and is
being started, stays `SyncPodNotReady`.

### `SyncPodNodeNotReady`

When the pod is `Running` on a node, has no readiness gate, its condition `Ready` is `False` and its condition `ContainersReady` is
`True`, `Ready` is `False` with the reason `SyncPodNodeNotReady`: `0 of 1 sync pods ready: the node kind-worker2 of the sync pod app-sync-0 is not
ready, and the pod is stuck there. If the node is shut down, give it the taint node.kubernetes.io/out-of-service with the effect
NoExecute, so that Kubernetes deletes the pod and makes a new one`. The node is the pod's `spec.nodeName`.

Why the pod tells it: the kubelet writes `Ready` and `ContainersReady` from the same statuses of the containers, and writes `Ready`
`False` with `ContainersReady` `True` only for a readiness gate, with the reason `ReadinessGatesNotReady`. A pod with a readiness
gate, which a service mesh or a webhook can add, is left out, because whoever owns the gate can hold `Ready` `False` for a cause
of its own; without one, the pair is not the kubelet's. The node lifecycle controller sets `Ready` to `False` on every pod of a node whose own `Ready` condition is not `True`, and changes
nothing else of the pod, so the kubelet's last word, that the containers are ready, stays. It stays too once the taint manager has
deleted the pod, which then shows Terminating. When the node is back, its kubelet writes the pod again, and `Ready` says what the
pod says then. The runbook of #90 is not written, so the message links nothing.

`AgentUnreachable` keeps the reason `ConnectionFailed` when the calls to such a pod fail for 3 minutes, because its reasons say
what the poll saw. Its message drops the network policy, which is not the cause, and says that the node of the sync pod is not
ready and that `Ready` names it. The poller takes that from the `Ready` of the Treebind that it polls, and reads nothing more. For
a pod whose node is ready the message is as before.

Both reasons join the ones for which the reconcile looks at the pod again after about 30 seconds (ADR 0039), so that `Ready`
follows the pod when the node is uncordoned or comes back, which changes nothing in the StatefulSet until the pod is ready.

### `Synced` is `Unknown` after 3 minutes without a report

`Synced` and `Conflict` are `Unknown` with the reason `AgentUnreachable` and its message once the polls have read no report for 3
minutes in a row, whatever the reason: a name that does not resolve, no token, keys that could not be put in place, as much as an
agent that does not answer. This replaces ADR 0037's rule that they do not change while `AgentUnreachable` is `Unknown`. The time
is the one that `AgentUnreachable` waits, counted in memory as its own is, from the first poll that read no report, and a report
ends it. So `Synced` changes once, when the 3 minutes have passed, and the next report puts it back. `AgentUnreachable` does not
change: a name that does not resolve still ends its own count and never makes it `True`.

The count starts only once a report has been read, that is once `Synced` no longer says `AwaitingAgentReport`. Before that the
status holds nothing that can go stale, and a sync pod that waits minutes for its claim to be bound or its image to be pulled is a
slow first start and not an outage: `Synced` and `Conflict` keep `AwaitingAgentReport`, and no event is recorded.

The reason is not new: `AgentUnreachable` on `Synced` already says that the operator has not read the report for 3 minutes and
does not know what the sync does, which is the cause for `Synced` here too. Why no report could be read is in `AgentUnreachable`
and in `Ready`. The change is the `Warning` event of ADR 0038.

Not taken:

- **Longer messages of `SyncPodNotReady`.** A pod that cannot be placed and a node that is not ready are causes that a person acts
  on, by uncordoning the node or freeing what the scheduler lacks, and by bringing the node back or tainting it, so each gets its
  own reason (ADR 0004, ADR 0039).
- **Reading the node, or the events of the pod.** It needs `get` on nodes, across the cluster, or `list` on events. The pod names
  both causes, and the role of the operator stays as ADR 0039 left it.
- **A reason of `AgentUnreachable` for a node that is not ready.** Its reasons are what the poll saw, and the cause has its reason
  on `Ready`.

## Not here

- **A pod that was not ready when its node was lost**, before its first pull or after a kill for memory, does not carry the mark:
  `ContainersReady` was `False` already. It stays `SyncPodNotReady` or `SyncPodOOMKilled`. Neither does a sync pod with a
  readiness gate.
- **The runbook for a lost or drained node** is #90.

## Consequences

- After a drain that a claim of one node keeps from moving, `Ready` says `SyncPodUnschedulable` with the scheduler's words, and
  `Synced` is `Unknown` 3 to 4 minutes after the first poll that read no report. After a node is lost, `Ready` says
  `SyncPodNodeNotReady` once the node lifecycle controller has marked the pod, and `ConnectionFailed` points to it.
- `SyncPodUnschedulable` and `SyncPodNodeNotReady` are API once released (ADR 0004). A tool that matched `SyncPodNotReady` for
  every sync pod that is not ready no longer sees it for these two.
- A Treebind whose operator has had no token, or could not put the keys in place, for 3 minutes shows `Synced` `Unknown` where it
  kept the last value. A Treebind that has never been read keeps `AwaitingAgentReport`, as before, unless `AgentUnreachable`
  turns `True`.
- Not tried on a cluster: the drain and the lost node of the issues with this change, and the out-of-service taint.
