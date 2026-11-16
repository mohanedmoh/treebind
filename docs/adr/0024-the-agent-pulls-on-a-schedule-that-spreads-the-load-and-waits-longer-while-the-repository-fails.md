# 0024. The agent pulls on a schedule that spreads the load and waits longer while the repository fails

- Status: accepted
- Date: 2026-11-15
- Issue: #33

## Context

ADR 0020 decided what one pull does and left the loop to a later change. The loop is what makes many
Treebinds ask one Git host at once, which is R-19: a host that is shared by a few hundred Treebinds, each
polling every 30 seconds, sees them as a crowd unless something spreads them, and a host that starts to
refuse them sees them all come back at the same moment.

The agent has no Kubernetes credential and no way to talk to the other agents (ADR 0011), so it cannot count
how many agents ask the host. What it can do alone is spread its own questions in time, ask the cheap question
first (ADR 0020), wait longer while the answers are failures, and never have two questions in flight.

## Decision

`internal/agent/syncloop` is the schedule and the loop, with no knowledge of Git: a pull is a function that
says whether it failed. `internal/agent/app` runs it after the checkout, with the settings the agent already
has (ADR 0021) and the pull of ADR 0020.

### The schedule

- **The interval** is `spec.sync.interval`, 30 seconds when it is not set and at least one second. Each wait is
  drawn at random between 80% and 120% of it, so that agents that started together drift apart.
- **The phase.** The first pull waits a point in the interval that comes from a hash of the namespace and the
  name of the Treebind (FNV-1a), the same at every start of its agent and different for different Treebinds.
  Pods that start together, after an upgrade of the operator or the restart of a node, do not pull together.
  The checkout has just fetched the branch, so nothing is missed by waiting, and the first pull is at most one
  interval after the checkout.
- **After a failure** the wait is drawn at random over the whole length between the interval and a ceiling that
  doubles with each failure in a row, up to ten minutes: with the interval of 30 seconds, 30 to 60, 30 to 120,
  30 to 240, 30 to 480 and then 30 to 600. The first pull that does not fail goes back to the interval. The
  draw is over the whole length, and not a fixed doubling, so that agents that failed together do not come
  back together. It starts at the interval and not at zero, so that a host that fails is never asked sooner than
  the interval, which is what a host that does not fail is asked, give or take the jitter. An interval that is longer than ten minutes has no room to grow, and is the wait, moved by
  the jitter as every other is, and a failure does not make it shorter. The ten minutes is not configurable.
- **One at a time.** The loop waits for a pull to end before it waits for the next. An agent never has two
  operations that ask the repository running at once, and the push of #37 is made in the same loop and not
  beside it. This is the only limit on one agent, and a limit across agents is not here (ADR 0020).

### Which pulls make the wait longer

A pull that failed because the repository did not answer, or because the volume did not let the agent write,
is followed by a longer wait. A pull that found the repository answering and something that a person puts
right is not: the next pull should see it put right at the next interval, and a slower poll would only delay
that by minutes to save one question.

| Result of the pull | What it means | The next wait |
| :-- | :-- | :-- |
| `UpToDate`, `Ahead`, `Advanced` | It worked | The interval |
| `Blocked` | A file that the update changes has a local edit | The interval |
| `Diverged` | The checkout and the repository went different ways | The interval |
| `BranchGone` | The repository answered, and has no such branch | The interval |
| `ReservedPath` | The branch holds a file where the agent keeps its state | The interval |
| `GitDirUnsafe` | The Git directory holds something the agent did not put there. No command was run | The interval |
| `FetchFailed` | The repository did not answer, or refused the key | Longer |
| `PullFailed` | Git failed on the volume, or the checkout is not what a pull expects | Longer |
| `Refused` | The fence or the guard stopped the update. Today that is a lock that could not be checked, which is a volume that does not answer | Longer |
| A pull that could not be set up | The agent could not make what pulls | Longer |

### The writer lock and the stop

- The fence of a pull is the `Check` of the lock that the agent holds, called by the pull immediately before it
  writes (ADR 0020). A lock that `Check` finds lost is said to the agent at once, in the way the keeper says it,
  and the agent stops with exit code 3 (ADR 0023), also in the middle of a pull cycle, without waiting for the
  next beat of the keeper. A `Check` that fails for another reason is a `Refused` pull, which is followed by a
  longer wait, and the keeper decides whether the lock is lost after its TTL.
- The lock is given up on a stop after the checkout and the pull have stopped, and not when the stop comes: a
  Git command that is being killed in the middle of an update may still write for a moment, and a lock that
  was already given up would let the next agent write beside it (NFR-1, ADR 0023). A stop that does not end in
  the grace period leaves the lock to run out its TTL.
- The guard of a pull is empty until the recovery ref and the cap on deletions of #40 are there. Until then a
  fast-forward that deletes files is applied. #40 has to land before the loop reaches a release, and has to say
  whether a refusal of its guard, which a person has to put right and which `Refused` also names, keeps the
  interval and does not make the wait longer: today every `Refused` makes it longer, because only the fence
  can refuse.
- A stop cancels the pull that is running, which kills the Git command that runs, and the wait. A pull that
  was cut short by a stop says nothing about the repository, so it is not reported.
- Both directions only pull. `bidirectional` also commits and pushes, which are #36 and #37, and until then it
  pulls like `pull`.

### The report

A check `Pull` after `Checkout`. It is skipped, with `NotChecked` and the reason, while the volume, the lock or
the checkout has not passed, until the first pull is due, and while the first pull runs. A pull that worked is
`Passed`, with where the checkout is. A pull that did not is `Failed`, with its reason and a message that
names what matters: the files in the way, the number of commits on each side, what Git said. A line is
logged when what the check says changes, and not at every poll, so the messages hold no time, and a pull that
keeps finding the same thing is logged once. This is not readiness: the Treebind turns ready after the first
sync (#48).

## Consequences

- The pod template, the CRD and the RBAC do not change: the settings were passed by ADR 0021 and nothing reads
  them differently, so no sync pod is replaced by this.
- A Treebind with the interval of one second asks its repository about once a second. The CRD admits it, and it
  is the choice of whoever sets it.
- A `Blocked` or a `Diverged` pull is tried again at every interval for as long as it lasts, and costs one
  question to the repository while the tip does not change, and one line in the log.
- The spread is of the agent's own making and is statistical: Treebinds whose names hash close together, and
  intervals that are short, are still near one another. What this does for a host that many agents share is to
  make most pulls one cheap question, at moments that are not the same. A cap that holds across Treebinds needs
  the operator to give each agent its share, and is a separate change.
- The first pull comes up to one interval after the checkout, which is the same wait as the readiness of #48 will
  see after a restart.
