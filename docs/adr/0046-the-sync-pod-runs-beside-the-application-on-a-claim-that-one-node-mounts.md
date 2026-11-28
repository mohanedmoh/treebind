# 0046. The sync pod runs beside the application on a claim that one node mounts

- Status: accepted
- Date: 2026-11-28
- Issue: #97
- Extends: [0003](0003-reserved-placement-and-agent-fields.md), [0032](0032-the-operator-may-do-what-it-does-and-can-be-limited-to-namespaces.md), [0037](0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md), [0040](0040-ready-names-a-sync-pod-that-cannot-be-placed-and-a-node-that-is-not-ready-and-synced-is-unknown-without-a-report.md), [0041](0041-what-the-breaking-release-before-beta-changes-in-the-api.md)

## Context

A `ReadWriteOnce` claim is mounted on one node at a time, and the sync pod was placed wherever the scheduler liked. On
a cluster of several nodes the sync pod or the application could then not mount the claim (R-4). FR-25 asks for required
pod affinity to the application's pods on such a claim, never `nodeName`, and ADR 0041 decided the field,
`spec.placement.appSelector`. Open were the access modes other than the two, a claim that is not there or changes, a
selector that matches the sync pod, and what the status says.

What bears on it:

- The operator did not read claims, and had no right to.
- The access modes of a claim cannot be changed. A claim has other modes only when it is deleted and made again.
- Kubernetes places the first pod of a group that has required affinity to itself anywhere: when no pod matches a term
  and the incoming pod does, the term is satisfied.
- `ReadWriteOncePod` lets one pod use a claim, so standalone placement cannot share it (ADR 0003). The operator would
  still make a sync pod, which can take the claim before the application does.
- The status poller tells a report of the current spec by a hash of the pod template, which it computes from the spec
  and the settings of the operator.

## Decision

`spec.placement.appSelector` is an optional label selector. The CRD refuses one with no entry in `matchLabels` and no
entry in `matchExpressions`, which would select every pod of the namespace. What else a selector may hold is left to the
API server, which refuses a bad one when the StatefulSet is written: `Ready` is `False` with `ReconcileFailed`.

With an `appSelector` the operator reads `spec.accessModes` of the claim and gives the pod template a
`requiredDuringSchedulingIgnoredDuringExecution` pod affinity with the topology key `kubernetes.io/hostname`, in the
Treebind's namespace:

| The claim | Affinity |
| :-- | :-- |
| lists `ReadWriteMany` | none |
| lists `ReadWriteOnce` and not `ReadWriteMany` | required |
| is `ReadWriteOncePod` | required |
| lists only `ReadOnlyMany` | none |
| is not there | required |

- **The claim's spec, not the volume's modes.** It is what the person wrote, it is there before the claim is bound,
  and it cannot change. `status.accessModes` is the volume's, empty until binding, and would replace the sync pod when a
  claim is bound to a volume that allows more.
- **`ReadWriteOncePod` gets the affinity** although it cannot help the two to share the claim. The sync pod is then never
  placed before a selected pod is, so it does not take the claim and leave the application `Pending`. Making no sync pod
  for it, with a reason of its own, was the other choice: it says the problem more plainly, and adds a reason to the API
  for a case that the scheduler already reports.
- **A claim that is not there counts as one that one node mounts.** A pod without the affinity could be placed by the
  scheduler as soon as the claim is made, before the operator saw it, and on a volume bound at first use that decides the
  node of the volume. The sync pod could not run without its claim anyway.
- **A changed claim** is read at the next reconcile. The operator does not watch claims: a claim that is missing or was
  made again leaves the sync pod not ready, and such a Treebind is looked at about every 30 seconds already (ADR 0039).
- **The selector never matches a sync pod.** The operator adds `app.kubernetes.io/managed-by NotIn (treebind-operator)`,
  a label that every sync pod has and that `spec.agent.labels` cannot change. Refusing such a selector instead would
  refuse one that selects the application well, because a team label is on both.
- **Several pods on several nodes** each satisfy the affinity. The selector has to name pods that mount the claim.
- **Without an `appSelector`** the operator reads no claim, and the pod template is the one it was.

**The affinity is not in the hash that the agent reports.** The StatefulSet's annotation covers the whole template, so a
change of the affinity replaces the pod. `TREEBIND_TEMPLATE_HASH` covers the template without it: the affinity changes
nothing that the agent does, and including it would have the status poller read the claim at every poll to expect the
same hash, and take a good report for an outdated one when the two had read different claims.

**`SyncPodUnschedulable` says it.** When the template has the affinity, the message says that the pod may only run on
the node of a pod that `spec.placement.appSelector` selects, before the scheduler's words, which only say that pod
affinity rules did not match. No condition or reason is added.

**The operator may `get` claims**, read from the API server and not from a cache, as for pods (ADR 0039): no `list` and
no `watch`. A read that fails, other than for a claim that is not there, is `ReconcileFailed`, and the StatefulSet is
left as it is.

## Consequences

- A Treebind without `appSelector` is unchanged, and an upgrade replaces no sync pod. Placement next to the application is
  opt-in, since the operator cannot tell which pods are the application's.
- The sync pod waits while no selected pod has a node: an application that is scaled to zero is not synced, if the sync
  pod is scheduled during that time.
- **The sync pod does not follow the application.** The scheduler ignores the affinity once the pod runs. A sync pod
  that stays keeps the volume attached to its node, and an application pod on another node cannot mount it until the sync
  pod is deleted. #113 does that; it deletes the pod, and the template and the hashes need no change for it.
- Adding, changing or removing `appSelector` replaces the sync pod when the template changes with it.
- The operator can read any claim where its role applies, by name. It could read none before.
- Whether the scheduler places the pods as meant, and how a detach and attach goes when an application moves, needs a
  cluster of several nodes with a volume that one node mounts. The tests of the operator have no scheduler.
