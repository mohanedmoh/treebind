# 0020. The agent pulls by fast-forward and never writes over a local edit

- Status: accepted
- Date: 2026-11-14
- Issue: #33

## Context

Once the checkout is made (ADR 0017) the agent has to bring in what the repository gains, on an
interval, while the application keeps writing to the same directory (FR-2). Three things can go
wrong. A pull can overwrite a file that the application changed and that has not been committed
yet. It can apply a history that is not the one the checkout was built on, such as a force-pushed
branch (R-2). And many Treebinds that poll one Git host at once can hit its rate limits (R-19).

The agent has no Kubernetes credential and no way to talk to the other agents (ADR 0011), so what it
can do about the third is limited to what one agent can do alone.

## Decision

### One pull

`internal/agent/pull` runs one pull through the Runner (ADR 0013), with the same look at the Git
directory before every command as the checkout (ADR 0019).

1. **Ask first.** `ls-remote` for the branch returns the commit it is at. When that is the commit
   that `refs/remotes/origin/<branch>` already holds, nothing is fetched. This is the cheap check
   R-19 names, and it costs one round trip for a pull that has nothing to do.
2. **Fetch** the branch into `refs/remotes/origin/<branch>`, as the checkout does: the address on the
   command line and never in the config, no tags, no `FETCH_HEAD`, and a refspec with `+`, so that a
   branch that was rewritten is fetched and then recognised for what it is.
3. **Compare** the checkout with the tip, with Git's ancestry test. The checkout at the tip, or at a
   commit that the tip has as an ancestor, is `UpToDate` or `Ahead`, which is a checkout that has
   commits the repository does not have yet. A checkout that is an ancestor of the tip, or that has no
   commit yet, is fast-forwarded. Neither is `Diverged`.
4. **Apply** with `merge --ff-only`.

### Never over a local edit

The fast-forward is Git's, and so is the refusal. `merge --ff-only` writes nothing and exits when a
file that the update changes has a local edit, or when an untracked file is in the way, and it keeps
every other local edit. That is FR-2, and it is Git's own check and not a list of mine: it knows
about the index, about files that the update deletes, and about a file that the update adds where
the application made one of its own. The pull then says `Blocked`, names a few of the files, and
writes nothing. The next pull tries again, and goes on once the files are committed or given up.

A pull that finds the two sides have gone different ways does not merge. It says `Diverged` with how
many commits there are on each side and writes nothing, and what wins is the conflict policy of the
Treebind (#38). A force-pushed branch is `Diverged` as well, and is never written to the checkout.

### What a pull may write, and when

- **A commit that writes into the state directory.** When the whole claim is synced, the state
  directory is inside the work tree, so the tip is looked at for a file named `.treebind` in any case,
  as the first checkout does (ADR 0017), and refused with `ReservedPath`.
- **The fence.** Immediately before the apply, the pull calls a function that the caller gives it, which
  is where the writer lock is checked (ADR 0012, NFR-17). A pull with nothing to apply does not call it.
- **The guard.** Before the fence, the pull calls a second function with the commit the checkout is at
  and the one it would move to, which may refuse. It is where the cap on deletions and the recovery ref
  of #40 go. Until they are there, an update that deletes files is applied.

### The reasons

A pull that works is `UpToDate`, `Ahead` or `Advanced`. One that does not is an error with a reason:
`Blocked`, `Diverged`, `BranchGone` when the repository no longer has the branch, `FetchFailed`,
`ReservedPath`, `GitDirUnsafe`, `Refused` when the fence or the guard stopped it, with their error
underneath, and `PullFailed` for the rest. They are API in the sense of ADR 0004.

### The loop, in the changes that follow

The agent pulls every `spec.sync.interval`, with jitter of about 20% so that it does not poll at one
beat, and after a failure waits longer with a doubling that is spread at random over its whole length,
up to ten minutes, and goes back to the interval after a success. Each agent also starts its cycle at a
phase that comes from a hash of the namespace and the name of its Treebind, so that agents that start
together do not poll together, and no agent runs more than one Git operation that asks the repository at
a time. That is what one agent can do alone. A cap across Treebinds needs something that counts them,
which only the operator could, and is not part of this.

The loop takes the writer lock before the first checkout, since that writes too, keeps it with the
heartbeat, and gives the pull a fence that checks it. A lost lock stops the agent with a non-zero exit,
and the restart of the pod waits for the lock again.

## Consequences

- A file that the application edits and does not commit blocks the updates of that file, and only of
  that file, until it is committed, reverted or removed. In a Treebind that only pulls, where the agent
  never commits, that is until someone does something about the file, and the pull says which.
- An update that deletes files is applied. The guard that caps deletions and keeps a recovery ref (#40)
  has to be in before the loop is on, so that a fast-forward is always reversible.
- A branch that the repository rewound to an older commit looks like a checkout with commits that
  were not pushed yet, and is `Ahead`: nothing is lost and nothing is written. The agent never
  force-pushes, so the push of #37 sends the commits again as an ordinary update of the rewound branch.
- A pull needs the repository to answer, at least for the question of what the tip is. The checkout
  and the application do not depend on it.
- The check between the ancestry test and the apply is not atomic with the writes of the application:
  a file edited in between is protected by Git's own check, which happens as the apply starts, and not by
  the pull.
- A cap across Treebinds is not here. What this does is spread the load and make most pulls one question.
