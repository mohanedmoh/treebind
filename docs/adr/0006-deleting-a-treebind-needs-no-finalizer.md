# 0006. Deleting a Treebind needs no finalizer

- Status: accepted
- Date: 2026-11-11
- Issue: #27

## Context

Deleting a Treebind must never delete the claim or its data (FR-17). The risk is
that a delete removes data or leaves orphaned workloads (R-20), and the planned
mitigation named owner references, keeping the claim and a minimal finalizer.

For each Treebind the operator creates a headless Service and a one-replica
StatefulSet (#25). The claim belongs to the user: the operator mounts it and
never creates it, because the StatefulSet has no `volumeClaimTemplates`.

## Decision

There is no finalizer.

- Everything the operator creates has a controller owner reference to the
  Treebind, so Kubernetes' garbage collector deletes it when the Treebind goes,
  with or without the operator running.
- The claim is never owned, and the operator cannot delete it: its roles have no
  rule on claims or volumes, and a test fails if one ever grants more than read
  access. The read access that #30 and #98 add stays allowed.
- Work that is not pushed yet stays in the checkout on the volume. Nothing
  deletes it.
- A Treebind that is being deleted is left alone by the reconcile loop (#134),
  so foreground deletion does not fight the garbage collector.

A finalizer was considered so that the sync pod could push before the Treebind
disappears. It was rejected: it adds an action at delete time whose failures
hold the deletion back (a Git host that is down, an operator that is not
running, a namespace that is being deleted), and the work it would push is
already safe on the volume.

This is what the agent has to do (not built yet): on SIGTERM it stops cleanly.
It finishes or abandons the operation in progress and leaves no half-written
checkout (R-7, R-30). Deleting the Treebind deletes the pod, and the signal is
the only warning the agent gets.

## Consequences

- Deleting never blocks, whatever the state of the operator, the Git host or the
  cluster.
- Nothing pushes the last local changes when a Treebind is deleted. A user who
  needs them on the remote waits until the sync has settled before deleting.
- A new Treebind for the same claim finds the old checkout. How it adopts a
  directory that already has files is #32.
- Owner references cannot cross namespaces or reach cluster scope. If the
  operator ever creates an object outside the Treebind's namespace, removing it
  needs a finalizer or another cleanup, and a new record.
