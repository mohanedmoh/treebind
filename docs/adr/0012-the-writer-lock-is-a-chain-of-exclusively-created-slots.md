# 0012. The writer lock is a chain of exclusively created slots

- Status: accepted
- Date: 2026-11-13
- Issue: #105

## Context

At most one agent may write to a checkout at a time (NFR-1): when a pod is
replaced or rolled, when two pods overlap for a moment, and when a pod is frozen
by a stopped node or a partition and resumes after another has taken over (R-7).
NFR-17 asks for a writer lock on the volume with a heartbeat and an epoch, whose
staleness is judged on the reader's own clock and which is checked again before
every commit, apply and push.

The agent holds no Kubernetes credentials (ADR 0004), so it cannot use a Lease.
The volume is the one thing that all agents of a checkout share. Advisory locks
(flock, fcntl) are not kept reliably by network file systems (R-43), and they
vanish with their holder without telling the next one anything. What a supported
volume has to offer is atomic rename and exclusive create (#101, #103).

A single record file that the holder rewrites by rename was considered first.
Rename replaces whatever is there. A holder that renews at the wrong moment
overwrites the record of the agent that has just taken over, and a contender
cannot make its takeover depend on the holder not having renewed since it last
looked. Both need a compare-and-swap, which rename is not.

## Decision

### Every change of the lock is one exclusive create

The lock is a directory on the volume, named by the caller. It holds files
called `slot-` and a number in twenty digits. A slot is made with an exclusive
create (`O_CREAT|O_EXCL`), written once and never changed. Whoever made the
highest slot holds the lock.

The holder's heartbeat makes the next slot. A takeover makes the next slot. A
release makes the next slot with a record that says so. Heartbeat and takeover
therefore ask for the same file name, and the file system gives it to one of
them: a contender loses when the holder it judged gone renewed after all, and a
holder that was frozen while another agent took over fails to make its next
slot. Nothing is renamed, because nothing is ever updated.

### Staleness is judged on the reader's clock

A contender lists the directory every interval. It takes over when it has itself
seen the same highest slot for a whole TTL on its own monotonic clock. A higher
slot number is the only sign of life. It never reads a time that another machine
wrote: the record carries the holder's wall time for people, and no decision
reads it. A released record at the top is followed at once, and so is a
directory in which the contender has never seen a slot. Slots that vanish are a
change like any other and start the wait anew.

### The epoch is the number of the slot a tenure began with

It is higher than every earlier epoch, it does not change within a tenure, and
it needs nothing from an earlier record. Epochs rise but are not consecutive.

### Check, Keep and Release

`Acquire` blocks until the lock is held; an agent that waits in it is a standby
replica (#114). `Check` makes the next slot and then lists the directory. It
returns `ErrLost` when the slot exists or a higher one is there, and from then
on always. Any other error means that the volume refused and the agent must not
write now. When no check has succeeded for a TTL of the holder's own clock, the
holder gives the lock up as lost, because a contender may have judged it gone.
`Keep` checks every interval and returns when the lock is lost, so that the
caller can cancel its work. `Release` looks for a higher slot first and then
makes a released slot. It does not look afterwards, because the next agent may
follow a release at once.

The holder removes the slots below its newest, except the first of its tenure.
That slot is the next one of the holder before it, which therefore collides with
it however late it wakes. A holder that slept through more than one tenure can
find its next number free again. That is why the directory is listed after the
create, and only after the slot is synced: it sees the higher slot and has lost.

### The record informs and never decides

A slot holds one JSON object: `version` (1), `epoch`, `holder` (a name the agent
chooses, such as the pod's name and a random part for each start), `time` and
`released`. Exclusion does not depend on it, because a slot counts by existing.

- A slot exists before its record is written, so a reader can meet it empty. A
  record that is empty, torn, garbage or longer than 4096 bytes means "held by
  someone unknown". It is taken over after a TTL like any silent holder, never
  earlier, and it is not an error.
- A record of a newer format is never taken over, because how its holder shows
  that it is alive is unknown. `Acquire` returns `ErrNewerVersion`. A later
  format has to keep the slot names and a JSON object with `version`, and has to
  say how a rollback works.
- Within a version fields are only added, and a reader ignores the ones it does
  not know.

### What the lock relies on

- Of several exclusive creates of one name, by any clients, one succeeds. Local
  file systems and CephFS do this, and NFS from version 3 on, where the server
  decides. NFSv2 does not and is not supported.
- An agent that has just created a file in the directory sees, in a listing it
  makes afterwards, the files that other clients created before. A contender
  that sees a stale listing is only delayed, because its create fails.
- A slot and its directory entry are synced before the agent relies on them.

It does not rely on flock, fcntl locks, rename, hard links, or on two machines
agreeing about the time.

### What the lock does not guarantee

- It is a lease and not a fence around the files. No takeover happens earlier
  than a TTL of the contender's clock after the holder's last successful check.
  A holder that is frozen in the middle of a step, after its check, finishes
  that step when it wakes. Steps have to be short against the TTL, and a push is
  a compare-and-swap for this reason (#106).
- A contender whose clock runs fast takes over early. That is safe, because the
  holder finds out at its next check, but it costs a tenure.
- The directory must not be writable by the application: a program that makes
  or removes slots can take the lock from every agent, or let two hold it. The
  same holds for an operator who removes slots while agents run.
- Storage that loses a create it acknowledged can lower the epoch.

## Consequences

- Nothing calls the package yet. The agent's sync loop will have to:
  - put the lock directory on the volume of the checkout and outside the tree
    the application sees (#45), and give every agent of a checkout the same TTL;
  - acquire after the preflight checks and before its first write, and report
    that it waits while it does;
  - run `Keep` beside the loop and cancel the loop's context, and with it the
    Git command in progress, when it returns an error;
  - call `Check` directly before every commit, apply and push, stop on any
    error, and on `ErrLost` drop what it was doing and acquire anew;
  - call `Release` on SIGTERM after the step in progress has finished or was
    abandoned (ADR 0006).
- The status report gains the epoch and whether the agent holds the lock or
  waits. Naming the holder an agent waits for needs a reader of the top record,
  which is not built.
- A heartbeat creates one file and removes one. The directory holds two or
  three small files. A program that watches the directory sees an event per
  heartbeat, which is one more reason to keep it out of the application's tree.
- An agent that starts after a crash waits a TTL. One that follows a clean
  shutdown does not.
- A rollback from a release with a newer record format is refused until that
  release says how, or until someone empties the lock directory while no agent
  runs.
- Nothing was measured on NFS or CephFS. The volume probe (#101, #103) has to
  test exclusive create, and the conformance runs have to test both things the
  lock relies on with two clients.
- The end-to-end tests of R-7, a deleted pod and a frozen pod, come with the
  agent that uses the lock.
