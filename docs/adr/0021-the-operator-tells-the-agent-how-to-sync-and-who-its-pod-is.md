# 0021. The operator tells the agent how to sync and who its pod is

- Status: accepted
- Date: 2026-11-14
- Issue: #33

## Context

The agent is going to pull on an interval and, later, to commit and push (ADR 0020). For that it has to
know how often to pull and in which direction, and it takes the writer lock of the volume (ADR 0012),
which names its holder after its pod. None of this is in the pod yet. The CRD has the interval, with a
default of 30 seconds and a least of one second, and the direction, which is `pull` or `bidirectional`.

## Decision

The operator puts three more settings in the sync pod as environment variables, as it does for the others
(ADR 0011, ADR 0018):

| Variable | Meaning | Set when |
| :-- | :-- | :-- |
| `TREEBIND_SYNC_INTERVAL` | `spec.sync.interval`, as Go writes a duration, such as `1m30s` | the spec has an interval. Without one the agent uses 30 seconds, the default of the CRD |
| `TREEBIND_SYNC_DIRECTION` | `spec.sync.direction`: `pull` or `bidirectional` | always |
| `TREEBIND_POD_NAME` | the name of the sync pod, from the downward API field `metadata.name` | always |

The agent reads them with the others and exits with code 2 on a value it cannot use, with every problem
at once. The interval has to be a duration of at least one second, which is the rule of the CRD. The direction
has to be one of the two. The pod name has to be there.

The rules are the same on both sides, and a test holds them together: for a list of intervals that covers both
sides of every rule, the API server accepts an interval exactly when the agent does, and so for the directions,
and the interval that the API server fills in is the one the agent has as its default. A value that the API
server takes and the agent refuses would leave a sync pod that exits at start for ever.

The lock names its holder with the name of the pod and a token that the agent makes at each start. The name of a
pod of a StatefulSet is the same after every restart, and the lock needs a holder that differs between two
starts, so that a holder that was frozen and wakes up is not taken for the one that replaced it.

## Consequences

- The direction is always set, so every sync pod template changes, and each sync pod is replaced once when the
  operator is upgraded (#87). The replacement is the agent restarting on the same claim, which it is built to do.
- A change of the interval or the direction of a Treebind replaces its sync pod, because the agent reads them at
  start. The new pod reopens the checkout (ADR 0017). `conflictPolicy` is not read by the pod yet, and a change of
  it does not replace it. `settleTime` is read, as the interval is (#34): it is passed as `TREEBIND_SYNC_SETTLE_TIME`
  when the spec has one, which the API server always fills in, so each sync pod is replaced once when the operator
  that passes it is installed, and again when the Treebind changes it.
- Nothing in the agent uses the settings yet. The lock and the loop that do come in the next changes.
