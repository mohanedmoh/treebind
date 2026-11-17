# 0030. The agent is ready after its first pull, and stays ready while it runs

- Status: accepted
- Date: 2026-11-17
- Issue: #48

## Context

NFR-2 asks that a crash at any point recovers on restart. The kubelet is what notices that an agent has crashed or
has not started its work, through the two probes that the operator puts on the sync pod (ADR 0011): the readiness
probe asks `/readyz`, the liveness probe asks `/healthz`, every 10 seconds, and three misses in a row count as a
failure. Until now `/readyz` answered 503 for ever, because the agent had no sync, and ADR 0024 left what the
readiness waits for to this issue.

What readiness means is not the agent's alone. The operator turns the `Ready` condition of a Treebind on when the
StatefulSet's pod is ready, and `Ready` is about the data path (ADR 0004, ADR 0015): the Treebind is not ready
before anything has synced, and a failure of the repository is not a reason to say it is not ready when the data on the
volume is as it was. Tools such as `kubectl wait` and the health checks of a GitOps controller act on `Ready`.

This record is the readiness probe. The liveness probe, which `/healthz` answers from the heartbeat of the loop, is a
second change for the same issue, and it extends this record when it lands.

## Decision

### The agent is ready once a pull has passed

`/readyz` answers 200 from the moment a pull has ended with a result that the check `Pull` of the report calls
`Passed` (ADR 0024): `UpToDate`, the checkout is at the tip of the branch; `Advanced`, it was brought to the tip; or
`Ahead`, it has commits that the repository does not have yet and lacks none that the repository has. The three have
in common that the repository has nothing that the directory lacks.

Nothing else makes the agent ready:

- **The checkout is not a sync.** A checkout that was cloned or adopted has just fetched the tip, and one that was
  reopened may be behind. They count alike, so that the rule for a start is one rule. The first pull is up to one interval
  after the checkout (ADR 0024), so the agent is ready up to one interval after the checkout, and the probe sees it at
  the next of its asks, up to 10 seconds later.
- **A pull that did not pass is not a sync.** `Blocked`, `Diverged`, `TooManyDeletions`, `ReservedPath`,
  `GitDirUnsafe`, `BranchGone`, `FetchFailed`, `PullFailed` and `Refused` leave the agent not ready when none has
  passed yet, and each says why in the check `Pull`.
- **A repository with no commit yet, and a branch that is gone, are the same.** The pull asks the repository for the
  tip of the branch and gets none, and says `BranchGone` for both: the agent cannot tell an empty repository from a
  branch that was named wrong, and it has not synced either way. When the agent commits and pushes (#36, #37) the first
  push gives the branch a tip, and the next pull makes it ready. Until then a Treebind on an empty repository is not
  ready, and the check says why.
- **The direction does not matter.** Both directions only pull until #36 and #37 are there. When they are, the first sync
  of a `bidirectional` Treebind has to include what it pushes, and the change that adds the push says so.
- **A scan is not a sync.** What the scans of a `bidirectional` Treebind find, or that one fails, does not change
  the readiness.

The report says that the pull passed before the probe does: the check is stored, and then the agent turns ready. A caller
that sees 200 on `/readyz` and reads the report finds the pull `Passed`. The agent logs one line when it turns ready.

### It stays ready while the process runs

The agent does not go back to not ready. A pull that fails after the first that passed leaves the files on the volume as
they were, and the check `Pull` says what it found, at the next read of the report. The probe is for whether the pod is
the one that the Treebind wants, and a pod that is the right one while the Git host is down is still the right one:

- A readiness that followed the last pull would turn `Ready` of every Treebind that uses a Git host False for as long as
  the host is down, a reason that is not the data path (ADR 0015). It would make `kubectl wait --for=condition=Ready` fail
  for it, and a GitOps controller would see a Treebind that is degraded where the sync is only waiting for the repository.
- A pull that is `Blocked`, `Diverged` or `TooManyDeletions` is waiting for a person (ADR 0024), and says so in its
  check. The sync state has its own place, the `Synced` condition (ADR 0004), for when the report carries it.
- The kubelet takes a pod that is not ready out of the endpoints of a Service. The headless Service of the sync pod has
  no port and publishes not-ready addresses, and nothing else uses it (ADR 0015), so a readiness that flapped would
  change nothing but `Ready`.

What does end it: the process. A lost writer lock ends the agent with exit code 3 (ADR 0023), and a container that is
restarted starts not ready, because nothing in its memory says that its predecessor synced. It is ready again after its
first pull, up to one interval after its checkout. The volume checks stop once they pass (ADR 0011), so they do not
bear on the readiness.

### What does not change

The pod template, the operator, the CRD and the report do not change: the probes, their paths and their timings are
the ones that the operator already sets, and a test holds them. The report has no new check and no new reason (ADR 0004
makes the reasons API). `Options.Ready` of the agent, which said "never" and which only a test set, is removed, and the
agent says it itself.

## Consequences

- A Treebind turns `Ready` after its first pull that passes, and `kubectl wait --for=condition=Ready treebind/<name>`
  waits for it: the time to make the checkout, up to one interval, and up to 10 seconds for the probe. It was never
  `Ready` before.
- A Treebind with a long interval is not ready for up to that interval after each start of its agent, as ADR 0024 foresaw.
  The first pull waits for a point in the interval that comes from the name of the Treebind, so that agents that start
  together do not ask the Git host together, and readiness inherits that. Counting a cloned checkout as synced would
  shorten it for the first start, and would be a second rule; it is left until someone needs it.
- A Treebind on an empty repository, or on a branch that does not exist, stays not ready, with `BranchGone` in the check
  `Pull`. That is the case until #37 pushes the first commit.
- A pull that fails after the first success does not show in `Ready`. It shows in the check `Pull` and in the log, and
  `Synced` is where it will show once the report carries the sync state (ADR 0015).
- A container that is restarted is not ready until it has pulled, so a restart is seen in `Ready` for up to one interval.
  What causes one today is a crash and the loss of the writer lock; the liveness probe adds another.
- Not decided here: what makes the liveness probe fail, which is the second change of #48; whether a `bidirectional`
  Treebind is ready before its first push (#37); and a probe that depends on the volume probe (#101).
