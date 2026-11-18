# 0030. The agent is ready after its first pull, and stays ready while it runs

- Status: accepted
- Date: 2026-11-17
- Issue: #48
- Extended by: [0033](0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md), which
  makes the first push that works a sync too for a `bidirectional` Treebind

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

This record is both probes: the readiness probe, which `/readyz` answers from the first pull, and the liveness probe,
which `/healthz` answers from the heartbeat of the loop. They landed as two changes of the same issue.

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
  branch that was named wrong, and it has not synced either way. A `bidirectional` Treebind that commits and pushes gives
  the branch its tip with the first push, and that push makes it ready (ADR 0033). A `pull` Treebind on an empty
  repository is not ready until someone puts a commit on the branch, and the check says why.
- **The direction matters only for the push.** A `pull` Treebind is ready after its first pull that passes. A
  `bidirectional` Treebind is ready after its first pull that passes or its first push that works, whichever comes first
  (ADR 0033, which extends this record).
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

### The loop beats, and `/healthz` answers from the beats

`/healthz` answered 200 whatever the agent was doing, so a loop that had stopped for good, in a call that never returned,
kept its pod. It now answers 503 when the loop has been silent for longer than it said it would be, and the kubelet restarts
the container. The package `internal/agent/heartbeat` holds the beats, and takes its time from a function that a test
moves by hand.

A single deadline for the whole loop cannot work. `spec.sync.interval` has no maximum, so a Treebind may pull once a day.
A pull that failed waits up to ten minutes (ADR 0024), and a fetch of a large repository is allowed half an hour. The
agent waits for the writer lock for as long as another agent holds it (ADR 0023), which is not a fault that a restart puts
right. So each beat carries its own promise: how long it is until the next. The agent is alive until that time and a
grace have passed.

| The loop is | The beat promises | Where it comes from |
| :-- | :-- | :-- |
| Waiting for the next pull or scan | The length of the wait | The loop (`syncloop.Options.Beat`), again when an event ends a wait sooner |
| Waiting to retry the volume checks, the lock or the checkout | The length of the wait | The step, before it says that it failed |
| Waiting for the lock of another agent | The interval of the lock (a third of the TTL, 10 seconds) and then a look at the volume, which is the next row | `lock.Config.Beat`, before each wait |
| In a Git command | The timeout of the command | The Runner (`git.Options.Running`), before it looks at the Git directory |
| In the code between waits and commands | 5 minutes (`heartbeat.Work`) | The start of each step, and the end of each command |

The grace is 30 seconds (`heartbeat.Grace`). It is longer than a timer that fires late, and than a Git command that is being
stopped after its timeout: SIGTERM, SIGKILL three seconds later, and the ten seconds that an update that is being applied is
allowed to finish (ADR 0028). The probe asks every 10 seconds and fails after three misses, so the pod is restarted 20 to
30 seconds after the grace, which is about a minute after a promise that was broken. A beat that comes late revives the agent: the kubelet
counts misses, and a loop that was slow and went on is not restarted. The agent starts with a promise of 5 minutes.

Four choices that are not obvious:

- **Only the loop beats.** The keeper of the writer lock does not. A keeper that cannot write to the volume loses the lock in
  30 seconds and ends the agent (ADR 0023), which is a stronger answer than a probe. The loop does one thing at a time (ADR
  0024), so the last beat is the promise that holds, and a change that runs two commands at once has to give the monitor a
  promise for each.
- **A failed step is not a stuck loop.** A pull that fails, a volume check that fails and a checkout that cannot reach the
  repository are all steps that return, and the loop beats for the wait after them. What the probe asks is whether the loop
  goes on, and the report says what failed.
- **An agent that has nothing more to do is alive for good.** With no lock, checkout or pulls to run, or with settings that
  no wait puts right, the loop has ended and the process serves the report. A restart would bring it back to the same place.
- **The probe does not follow the readiness, and the readiness does not follow the probe.** A pod that is ready and stuck
  stays ready until the restart, and a pod that is alive and not ready is waiting for its first pull.

A late loop is logged once a minute at most, with how late it is, so that the person who finds the pod restarted finds why.

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
- A Treebind on an empty repository, or on a branch that does not exist, is not ready, with `BranchGone` in the check
  `Pull`. A `bidirectional` one turns ready at its first push that works (ADR 0033); a `pull` one stays not ready until
  a commit is on the branch.
- A pull that fails after the first success does not show in `Ready`. It shows in the check `Pull` and in the log, and
  `Synced` is where it will show once the report carries the sync state (ADR 0015).
- A container that is restarted is not ready until it has pulled, so a restart is seen in `Ready` for up to one interval.
  What causes one is a crash, the loss of the writer lock, and a loop that is stuck.
- A loop that is stuck in a call that does not return is restarted about a minute after its promise ran out, and not
  before: the promise of a command is its timeout, so a fetch that hangs is stopped by the Runner at its own timeout (30
  minutes for a fetch) before the liveness probe would find it, and the code between commands has 5 minutes. A restart does
  not cure a volume that does not answer. It gives a pod whose restarts and events say that the agent was stuck, and a new
  process whose volume checks say what the volume does.
- The 5 minutes of `heartbeat.Work` and the 30 seconds of `heartbeat.Grace` are constants and not settings. A step of code
  that takes longer than 5 minutes on a volume that is slow would be restarted, and it would be restarted again at the
  same place. No step does that now (an update is applied by Git, whose command has the timeout of the fetch). A step that
  could has to beat while it works, and that is what a later change must do.
- Not decided here: a probe that depends on the volume probe (#101). Whether a `bidirectional` Treebind is ready from its
  first push was decided with the push (ADR 0033).
