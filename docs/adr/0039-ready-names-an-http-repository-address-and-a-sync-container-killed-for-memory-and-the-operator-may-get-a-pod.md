# 0039. Ready names an http repository address and a sync container killed for memory, and the operator may get a pod

- Status: accepted
- Date: 2026-11-24
- Issue: #256, #258
- Extended by: [0040](0040-ready-names-a-sync-pod-that-cannot-be-placed-and-a-node-that-is-not-ready-and-synced-is-unknown-without-a-report.md),
  which gives `Ready` two more reasons of its own, `SyncPodUnschedulable` and `SyncPodNodeNotReady`, and makes `Synced`
  `Unknown` after 3 minutes without a report, whatever the reason
- Extends: [0004](0004-treebind-status-conditions.md), [0032](0032-the-operator-may-do-what-it-does-and-can-be-limited-to-namespaces.md)

## Context

ADR 0004 says that every distinct cause of a not-ready Treebind gets its own reason on `Ready`, and that the record that adds
one extends the aggregation. Two causes had none. Both were reported on v0.0.1-alpha.3, on a kind cluster.

**An http address (#258).** The CRD admits `http://` in `spec.repository.url`. The agent refuses it at start with exit code 2,
because only ssh and https are allowed (NFR-15, ADR 0018). The operator made the sync pod anyway, and it went into
CrashLoopBackOff. `Ready` said `SyncPodNotReady`. After about 3 minutes `AgentUnreachable` turned `True` with
`ConnectionFailed`, which sends a reader to a network policy or a service mesh. Nothing named the address.

**A sync container killed for memory (#256).** The sync container has a memory limit of 128 MiB. Git went over it on a file of
150 MB and on one of 200 MB. The container was killed, restarted, tried the same update and was killed again. `Ready` said
`SyncPodNotReady`, `Synced` `SyncPending` and `AgentUnreachable` `PollFailing`. Nothing named the memory limit, and
`spec.agent.resources` is accepted and ignored (#96), so the person could not raise it either. The operator had no way to know:
it read the StatefulSet, whose status has a count of ready pods and no cause, and its role had no permission on pods (ADR 0032).

The fix of #256 also makes Git stream a file above 16 MiB (`core.bigFileThreshold`, in the fixed configuration of ADR 0013),
so a new file of 200 MB needs no more than the limit. A large file that an update changes can still need more, and the
container is then killed as above.

## Decision

### `UnsupportedRepositoryScheme`

The reconcile creates no Service and no StatefulSet for a Treebind whose `spec.repository.url` has the scheme `http`, in any
case. From its first pass `Ready` is `False` with the reason `UnsupportedRepositoryScheme`. The message says that `http://`
repositories are not supported, to use an `ssh://` or `https://` address in `spec.repository.url`, and that no sync pod is
created or changed until then. It holds no address.

- The check is the scheme as `net/url` parses it. `HTTP://` matches. `https://`, `ssh://`, `http+ssh://`, an scp-style address
  and an address that does not parse do not.
- It comes before every other reason of `Ready`, because the sync objects are not looked at.
- The pass returns no error, so there is no retry with backoff: only a change of the spec puts it right.
- A sync pod that the Treebind already had is left as it is, not updated and not deleted. When the address changes to one that
  the agent takes, the next pass makes or updates the sync objects.
- The CRD still admits `http://` and the agent still refuses it at start.

Not taken:

- **Make the sync pod and say why.** A sync pod for such an address can only exit at start, for ever.
- **Reuse `ReconcileFailed` (ADR 0008).** It is for a failure of the operator's own writes, which the pass returns and retries.
  An http address is no such failure, and a retry does not help. ADR 0004 does not reuse a reason for another cause.
- **Tighten the CRD.** The person would see the error when the Treebind is applied, which ADR 0018 calls the better fix. It is
  the open decision of #268: whether plain http gets supported, and under which rules, with the question how the API change is
  made compatible within v1alpha1. A CRD that refuses http now would have to admit it again if #268 allows it. The check in the
  operator changes no field of the API, and it is meant to be removed.

### `SyncPodOOMKilled`

While no sync pod is ready, the reconcile reads the pod `<name>-sync-0` by name and says in `Ready` when its container was
killed for memory. `Ready` stays `False`. The reason is `SyncPodOOMKilled`, and the message is the one of `SyncPodNotReady` and
the cause: `0 of 1 sync pods ready: the sync container was killed for using more than its memory limit of 128Mi (OOMKilled)`.
The limit is the one in the spec of the pod, and without one the cause reads `the sync container was killed for memory`.

The reason is given when the container is not ready and one of these holds:

- the kubelet ended it with `OOMKilled` and it has not started again;
- it waits to start again, and its last end was `OOMKilled`;
- it runs again, and its last end was `OOMKilled` at most 10 minutes ago.

Everything else stays `SyncPodNotReady`: an end for another reason, a container that is ready again, a kill of another
container, a pod that is not there yet, and a pod that cannot be read (the failed read is logged). The kubelet keeps the last
end of a container for the life of the pod, so a kill of hours ago would otherwise name the cause of every later wait. Ten
minutes is longer than the longest wait before a restart, 5 minutes, plus a run, so a container that is killed at every try
is named also while it is up.

A pod that has never been ready gives its StatefulSet nothing to change while its container is killed and started again, so no
event starts a pass that reads the pod. While no sync pod is ready the reconcile therefore asks to run again after about 30
seconds, and `Ready` says it within about half a minute of the kill. No pod is read while a sync pod is ready.

Not taken:

- **Keep `SyncPodNotReady` with a longer message.** People and tools match on the reason (ADR 0004), and `SyncPodNotReady` is
  also what a pod that has not pulled yet, a pod being replaced, an unbound claim and an image that cannot be pulled say. A
  killed container has a cause that a person can act on, so it gets its own reason.
- **A pod informer.** It needs `list` and `watch` on pods, a watch of every pod where the operator serves, for one pod that
  matters only while it is not ready. The read by name needs `get` only, and its cost is the pass that asks again.

### The operator may get a pod

The manager role gains `get` on `pods` of the core group: no `list` and no `watch`. It is in `config/rbac/role.yaml`, from the
marker of the controller, and in the manager role of the chart. `docs/rbac.md` has its row, and the test that holds each list
of the roles exactly (ADR 0032) holds it. The reconcile reads with the API reader of the manager, past its cache, so no
informer for pods is started, and the operator refuses to start without that reader.

The grant cannot be limited to the sync pod by name, because the names of sync pods are made from the names of the Treebinds.
It reaches every pod where the role applies: every namespace with the ClusterRole, and with `rbac.namespaced=true` the
namespaces of `rbac.namespaces`, which are the ones the operator watches (ADR 0032). The code reads only `<name>-sync-0`.

## Not here

- **A way to size the sync pod.** The limit stays 128 MiB and `spec.agent.resources` stays ignored. That is #96, and it stays
  open. `SyncPodOOMKilled` names the limit and does not let anyone change it.
- **Whether plain http gets supported, and a change of the CRD.** That is #268.
- **`Synced` and `AgentUnreachable`.** Both fixes change `Ready` only.

## Consequences

- An http address shows `UnsupportedRepositoryScheme` at the first reconcile, with the fix in the message, where it showed
  `ConnectionFailed` after 3 minutes. A killed sync container shows `SyncPodOOMKilled` with its limit within about half a
  minute.
- The two reason names are API once released, as ADR 0004 says of reasons. A tool that matched `SyncPodNotReady` for every sync
  pod that is not ready no longer sees it for a container that was killed for memory.
- The operator reads one more resource: one uncached read of one pod for each pass while no sync pod is ready, and none for a
  ready one. A Treebind whose sync pod is not ready for any reason is asked for again about every 30 seconds, where a pass
  without an event came about every 5 minutes (ADR 0015).
- The role of the operator changes, so a chart upgrade or a new apply of `config/rbac` is part of the upgrade. An operator whose
  role lacks `get` on pods logs the failed read and leaves `SyncPodNotReady`. The sync is not touched.
- ADR 0018 says that a Treebind with an `http://` address gets a sync pod that exits at start. For the operator this record
  governs: it makes none. An agent that is started with such an address still exits at start.
- If #268 allows http, or the CRD stops admitting it, delete `repository_scheme.go` and its test in `internal/controller`, the
  use of `usesPlainHTTP` in `Reconcile` and in the contract test of `repository.url`, and what `docs/checkout.md` and
  `docs/agent-status.md` say of the reason. The name of the reason is API, so the record of that decision says what becomes
  of it.
