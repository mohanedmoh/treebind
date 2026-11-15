# 0023. The agent holds the writer lock from before the checkout

- Status: accepted
- Date: 2026-11-14
- Issue: #33

## Context

At most one agent writes to a checkout at a time (NFR-1), and a pod that was replaced, a node that was
lost or a pod that froze must not leave two that do. The writer lock of ADR 0012 does that, and #105 said
its wiring into the agent comes with the first loop. The first thing the agent writes is the first checkout
(ADR 0017), and the pull (ADR 0020) writes after it, so the lock has to be held before either.

## Decision

The agent takes the lock once the volume checks pass and before it makes the checkout, waits for it while
another agent holds it, and keeps it with the heartbeat of the lock until it stops.

- **Where.** The directory of the lock is `.treebind/lock`, beside the Git directory, and `Layout.Prepare`
  makes it, mode 0700 and the agent's, with the checks of the other two directories. The lock would make
  its own directory open to the group, so that agents that reach the volume as different users of one
  group can all make slots. The application shares the group of the claim, and a slot that it made would
  take the lock or break it, so the directory is made private first and the lock finds it there.
- **Who.** The holder is the name of the pod, which the operator passes (ADR 0021), and a token that the
  agent makes at each start. The name of a pod of a StatefulSet is the same after every restart.
- **How long.** A holder that stops giving a sign for 30 seconds is taken over, and it gives a sign every 10.
  A pod that is replaced gives the lock up when it stops, and the agent that takes its place gets it at once.
  A pod that was lost with its node is waited for 30 seconds.
- **What the report says.** A new check, `Lock`, between the volume checks and the `Checkout`. It is skipped
  while the agent waits, and says so, and passed once the lock is held. A volume that refuses is `LockFailed`
  with what it said, and the agent tries again with the growing wait of its other checks.
- **A lost lock stops the agent.** The heartbeat finds that another holder has the lock, the work in
  progress is told to stop, and the agent exits with code 3. The restart of the pod waits for the lock
  again. A lock that was lost is never taken back by the agent that lost it.

The pull is given the check of the lock as its fence, to call immediately before it writes (ADR 0020). That
comes with the loop.

## Consequences

- An agent that cannot take the lock does not check out, pull or write, and says why in the report. That is
  the replica that waits in sidecar mode (#114), and it is also what a second agent on a claim does today.
- A write that was already past its last check when the holder froze is not stopped by the lock. That is
  what compare-and-swap branch updates are for (#106), and ADR 0012 says so.
- The first checkout is not fenced step by step: the lock is held, and its heartbeat stops it when it is
  lost, which can take up to the length of one heartbeat.
- The exit code 3 is for a pod that did nothing wrong. A pod that is replaced often because two agents keep
  taking the lock from one another shows as restarts, with the loss in its log.
- The lock directory is the agent's, so an agent that runs as another user than the one that made it, after
  a change of `runAsUser`, is refused by the check of the directory, and says so, like the Git directory.
