# 0014. Operator replicas and their placement

- Status: accepted
- Date: 2026-11-13
- Issue: #118

## Context

NFR-21 asks that the operator runs as two replicas with leader election. Two
replicas only help if they do not share a failure. Both on one node die with it, a
drain can evict both at once, and an update can stop the one that still works. The
cluster may also have a single node, as kind and k3s do, and a rule that demands
different nodes leaves a replica Pending there.

## Decision

The operator runs two replicas, and each one runs with `--leader-elect`. The
settings around them are these, in `config/manager` and in the chart:

- **Lease timing is set in code**: 15 seconds of lease, a 10 second renew deadline and
  a 2 second retry period. They are the library's defaults today, but the
  documentation quotes them, so a library upgrade must not change them unseen. The
  leader releases the lease when the manager stops, so a rolling update or a drain
  hands over within a retry period instead of a lease duration. Nothing runs after
  the manager returns, which is what makes that safe.
- **The spread over nodes is a soft topology spread constraint**: `maxSkew: 1` on
  `kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, and
  `matchLabelKeys: [pod-template-hash]` so that only the pods of the same revision
  are counted. It prefers different nodes and never refuses a node.
- **The PodDisruptionBudget has `maxUnavailable: 1`**, and
  `unhealthyPodEvictionPolicy: AlwaysAllow`.
- **The rolling update has `maxSurge: 1` and `maxUnavailable: 0`**, so two replicas
  are ready throughout an update.

The readiness probe stays `/readyz`, which does not depend on holding the lease, so
a standby is ready.

Other choices were considered.

- **A required pod anti-affinity** leaves the second replica Pending on a cluster
  with one node. On a cluster with two it also leaves the extra pod of a rolling
  update Pending, and the update stops. A user who always has the nodes can still set
  it through `manager.affinity`, with a strategy that does not surge.
- **A preferred pod anti-affinity** is the other soft choice. It counts every pod that
  matches its selector, so during an update it counts the pods of the old revision
  too, where the constraint above counts only the new ones. In a test on a kind
  cluster with two schedulable nodes this made no difference: ten rollouts of the
  Deployment ended with the two replicas on different nodes with the constraint (8 of
  8), with a preferred anti-affinity (10 of 10), and with a preferred anti-affinity
  and the scheduler's own default spreading switched off (9 of 9). The test does not
  separate the three. The constraint is chosen because it states the intent in the manifests, counts the
  right pods by design and does not depend on a scheduler default that a cluster can
  change.
- **`minAvailable: 1`** gives a budget of zero when one replica runs, and a drain
  then waits for ever. `maxUnavailable: 1` allows one disruption with one replica and
  with two.

## Consequences

- A soft rule is a preference and not a guarantee. When the nodes have no room, both
  replicas can share one, and a cluster that needs a guarantee sets a required rule.
- A ready pod does not say which replica leads. The lease does, and the metrics
  Service reaches both pods.
- A failover with a clean shutdown takes seconds, and one after a crash takes about
  the lease duration. The documentation gives measured figures.
- `matchLabelKeys` is a beta field that is on by default from Kubernetes 1.27, and
  the chart already requires 1.35.
- The chart's copy of each setting is a hand-written template, so tests compare it
  with `config/manager` and check that each selector selects the operator's pods.
