# 0004. Treebind status conditions

- Status: accepted
- Date: 2026-11-11
- Issue: #26

## Context

NFR-6 asks that every sync outcome is visible as a condition on the Treebind.
Today the status has `SpecSupported`, `Ready` and `observedGeneration`. The
outcome of a sync is not there yet, and its source is not either: the sync agent
has no Kubernetes credentials, so the operator will write the outcome from the
agent's reports (the status poller in #99, the agent endpoint in #104).

Several later issues add conditions of their own (#31, #37, #38 and #77 to
#80). Without a shared contract, two writers overwrite one condition and
`Ready` means whatever the last change made it mean.

## Decision

Four condition types, each with one owner:

- `SpecSupported`, written by the reconcile loop: `True` when every field set
  in the spec is implemented, `False` when a set field is accepted but ignored
  (ADR 0003). It is never `Unknown`.
- `Ready`, written by the reconcile loop: `True` when the sync pod is ready and
  the volume is not known to be out of sync, `False` otherwise. It is never
  `Unknown`.
- `Synced`, written by the status poller (#99): `True` when the volume matches
  the repository at `status.lastSyncedCommit`, `False` when the last sync failed
  or the volume is behind, with a reason that names why, `Unknown` when no
  agent report has arrived yet.
- `Conflict`, written by the status poller (#99): `True` when parked local work
  waits for someone, `False` when there is none, `Unknown` when no agent report
  has arrived yet.

The reconcile loop never writes `Synced` or `Conflict`, except to initialize
them. The first time it sees a Treebind it adds both as `Unknown` with reason
`AwaitingAgentReport` and the message "No report from the sync agent has been
received yet", but only when the condition is absent. A condition that exists is
never overwritten, whatever its status. Applying the conditions the first time
an object is seen, even as `Unknown`, is what the Kubernetes API conventions
recommend, and it makes `kubectl get` show `Unknown` instead of an empty cell.
The addition goes into the same status write as the rest of the pass.

`Ready` is computed from the StatefulSet and the current `Synced` condition:

- The sync pod is not ready: `False`, reason `SyncPodNotReady`, whatever
  `Synced` says. A ready count from a StatefulSet generation the StatefulSet
  controller has not observed yet counts as zero.
- The pod is ready and `Synced` is absent, `Unknown` or `True`: `True`, reason
  `SyncPodReady`. No report yet is not evidence of a failure.
- The pod is ready and `Synced` is `False`: `False`, reason `NotSynced`, message
  `1 of 1 sync pods ready, but Synced is False: <reason of Synced>`.
- `Conflict` never changes `Ready`. With `followRemote` the sync keeps running
  while a conflict waits for attention, so the Treebind works and someone needs
  to look; that is a different signal from "not ready".

`status.observedGeneration` and every condition the reconcile loop writes carry
the generation the loop observed. A condition it does not own keeps the
generation its writer observed, and the loop does not touch it, so comparing a
condition's `observedGeneration` with `metadata.generation` tells a reader
whether it describes the current spec.

Reasons are CamelCase, short, and stable, because users and tooling match on
them and they are therefore API. Each distinct cause gets its own reason; a
reason is not reused for a different cause. This record defines only
`AwaitingAgentReport` and `NotSynced`. The reasons of `Synced` and `Conflict`
for True and False arrive with the issue that writes them.

Messages never contain a credential or a URL that carries one. Text that comes
from a Git host, such as error output or a branch name, is sanitized before it
goes into a message, because everyone who can read the Treebind reads it.

A later issue that adds a condition states its owner, what True, False and
Unknown mean, and whether it can make `Ready` False. One that can gets its own
reason on `Ready`, the way `Synced` gets `NotSynced`, and a record that extends
the aggregation above.

## Consequences

- `kubectl get treebind` shows `Ready`, `Synced`, `Conflict` and `Age`. Printer
  columns replace the default `Age` column, so it is listed explicitly.
- Until #99 lands nothing writes `Synced` or `Conflict` past the initial
  `Unknown`, so `Ready` still means only that the sync pod is ready.
- Because the reconcile loop writes `Synced` and `Conflict` only when they are
  absent, it cannot fight the status poller over them. A condition that is
  removed is added back as `Unknown` on the next pass.
- `Ready` does not yet compare the `observedGeneration` of `Synced` with the
  Treebind's, so a `Synced` written before a spec change is still trusted. Which
  generation an agent report describes is part of the report format (#104), so
  the status poller (#99) decides how a stale `Synced` counts and adds the
  comparison with it.
- Conditions are state that stays on the object. Events and metrics for the
  same outcomes are separate work (#47) and do not replace them.
