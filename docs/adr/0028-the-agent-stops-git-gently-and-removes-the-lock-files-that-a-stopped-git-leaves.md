# 0028. The agent stops Git gently and removes the lock files that a stopped Git leaves

- Status: accepted
- Date: 2026-11-16
- Issue: #181, #39

## Context

While Git writes it holds a lock file: `index.lock` for the index, `<ref>.lock` for a ref. It removes the
file when it is done. The agent stops a Git command when it is told to stop and when a command times out, and
it did so with SIGKILL to the whole process group (ADR 0013). Nothing removed what a killed command left, so
every command after it failed with `Unable to create '...lock': File exists`, the pull said `PullFailed`, the
wait before the next one grew to ten minutes (ADR 0024), and it did not end until a person removed the file in
a pod that mounted the claim. An out-of-memory kill, a lost node and a pod that is killed when its grace
period ends leave the same.

What a stopped Git leaves was tried on the Git of a developer's machine (2.34.1) and on the one in the image of
the agent (2.56.1), with the same result. A real command is held inside a write, by a hook of
`reference-transaction` that waits while the locks of a transaction are held, or by a filter that waits in the
middle of the files of an update, and is stopped with SIGKILL or with SIGTERM to its process group
(`internal/agent/git/killed_test.go`):

| Held command, at | Lock files it holds | Left after SIGKILL | Left after SIGTERM | The work tree |
| :-- | :-- | :-- | :-- | :-- |
| `merge --ff-only`, before its first write | `ORIG_HEAD.lock` | the same | none | as it was |
| `merge --ff-only`, in the middle of the files | `index.lock` | the same | none | half written: some files new, one deleted, the index and HEAD as they were |
| `merge --ff-only`, at the update of the branch | `HEAD.lock`, `refs/heads/<branch>.lock` | the same | none | all files new, the index new, HEAD as it was |
| `update-ref` of a new ref | `refs/treebind/recovery/<n>.lock` | the same | none | as it was |
| `update-ref -d` of a ref | `refs/treebind/recovery/<n>.lock`, `packed-refs.lock` | the same | none | as it was |
| `fetch` into a remote ref | `refs/remotes/origin/<branch>.lock` | the same | none | as it was |

So a SIGTERM lets Git remove its own locks and a SIGKILL does not. The work tree is what the command had written
by then, whichever signal ended it. With the locks gone, the next `merge --ff-only` goes on after the first and
the third step, and is refused after the second, for the files that the update had written (`Your local changes
... would be overwritten`).

The check of the Git directory (ADR 0019) accepted the lock of a file that the agent makes, and not
`packed-refs.lock`. `update-ref -d`, which tidies the recovery refs (ADR 0026), locks `packed-refs` whether or
not there are packed refs. A kill during it left a directory that the agent refused as `GitDirUnsafe`, which
only a person puts right.

## Decision

**A command that is stopped is told to end first.** The Runner sends SIGTERM to the process group, and SIGKILL to
the group only when the call has not ended after `termGrace`, 3 seconds. The kill of the group stays, so that an
ssh that Git started does not outlive the call (ADR 0013). A child that left the group is still not waited for
beyond the grace: Wait gives up on the output after `termGrace` and `killGrace`, which is 5 seconds, and was 2.

**A stop does not cut off the merge of an update.** The merge that applies an update has `Drain`, 10 seconds by
default, to finish once the agent is told to stop, and is stopped the same gentle way after that. The sync pod has
the 30 seconds that Kubernetes gives, and the lock is given up after the pull has stopped (ADR 0023). An update
does not begin once the agent has been told to stop. The other commands of a pull are stopped at once: they are
stopped well by SIGTERM, which the table shows. This changes the line of ADR 0024 that says a stop cancels the
pull that is running.

**The agent removes the lock files that a stopped Git left.** `git.RemoveStaleLocks` removes the lock of each file
at the top of the Git directory that the agent's commands write, and the lock of `packed-refs`, and every `*.lock`
file under `refs`. It removes only a regular file, follows no link, and leaves a directory or a link with such a
name and every other file, so a lock of another name at the top is left to the check of the Git directory. It
goes on after a file that it cannot remove and says which. The agent calls it before the checkout and before each
pull, with the fence of the writer lock called first and only when there is something to remove, so that a Git
directory with no lock file costs a look and writes nothing (NFR-17). The `Checkout` and `Pull` checks say which
files were removed, and so does the log.

**The check of the Git directory accepts `packed-refs.lock`**, and still refuses `packed-refs`.

### Why removing a lock is safe, and where it is not

A lock file is stale when no command holds it. The agent holds the writer lock of the volume (NFR-1, ADR 0012), so
no other agent writes to the checkout, and it runs one Git command at a time. `RemoveStaleLocks` refuses, with
`ErrBusy`, while a command of this process runs on the Git directory, and a command that starts while it works
waits for it, so a lock file that is there is not one that a command of the agent holds.

What this does not rest on, and where it can be wrong:

- **A holder that froze and was taken over.** If it wakes up in the middle of a write, it can finish one that
  was past its last `Check`. That is the limit that ADR 0012 states, and compare-and-swap branch updates (#106)
  are for it. Removing its lock file does not widen what it can write to the work tree. What it adds is that the
  holder's own rename can find the file of the new holder under its lock name. The window is the length of one
  Git command in a holder that was silent for a whole lock TTL.
- **Git run by hand in the sync pod** while the agent works can lose its own write, when the agent removes the
  lock file that it holds. The Git directory is the agent's (ADR 0016, ADR 0019).
- **A lock under another name.** Only what the table lists, and what the agent's commands are known to write, is
  removed.

## Consequences

- A stop of the pod, which is a SIGTERM and then a wait, leaves no lock file, and does not cut off an update that
  is writing its files, unless the update takes longer than the drain. A node drain and an upgrade of the
  operator are stops of this kind.
- An out-of-memory kill, a lost node, and an update that outlasts the drain and the grace period still kill Git. The
  lock files are removed at the next pull, and the next pull goes on, for every step but one: **an update that was
  killed in the middle of the files leaves the files half written**, and the next pull is `Blocked` for files
  that the agent wrote itself. It is not repaired by this change. It is the next change of #39: a record of the
  update that was begun, and putting its files back when it was not finished. That has to land before #36
  commits local changes, or a half-applied update is committed as an edit of the application.
- The pull now looks at the top of the Git directory and at `refs` before each pull, which is a read of a few
  directories.
- The first checkout still removes its own `index.lock` when it resumes (ADR 0017), as `Open` alone has to.
- A lock-age metric is not made: there are no metrics yet (#47), and the report says what was removed.
- What a killed `fetch` leaves in `objects`, such as the pack that was being received, is not a lock and is not
  removed. Garbage collection is off (ADR 0013).
