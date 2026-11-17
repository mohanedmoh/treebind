# 0028. The agent stops Git gently, removes the lock files that a stopped Git leaves, and undoes an update that was cut off

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

### An update that was cut off is undone before the next pull

A lock file is the smaller part of what a kill in the middle of an update leaves. The files that the merge had
written stay (the table above), and once the lock is gone the next pull is `Blocked` for files that the agent wrote
itself. R-8 and NFR-2 ask for a crash at any point to be recovered on restart.

**A journal.** After the fence has checked the writer lock and the commit the checkout is at has been kept, and
before the merge, the pull writes the file `updating` in the state directory: a line of JSON with the format
version, the commit the checkout is at, which is empty when it had none, and the commit it goes to. It is written
to a file of its own, synced and renamed, so that it is there whole or not at all, and an update whose journal
cannot be written is not applied. It is removed after the merge, and after a merge that Git refused with
`Blocked`, which wrote nothing. A merge that failed for any other reason, a full volume for one, or was stopped,
leaves it, because the files it wrote are half an update.

**Looking at it.** `Puller.Pull` looks for the journal with one stat, after it has checked that the checkout is on
its branch, and so after the agent removed the lock files a stopped Git left (this ADR above). The fence is
called before the first write, and not when there is nothing to put right. A journal that cannot be read, is torn,
is of another version or names what is not a commit, is no journal: it is removed and the Pull check says so, and
the pull goes on. What the checkout is at decides the rest:

- At the commit the update went to, the update had finished, and only the journal was left. It is removed.
- At the commit it came from, which is no commit for a first update, the update is undone, as below.
- At neither, the journal is removed and nothing else is done.

**Undoing it** puts the files that the update changed back as they were, as far as they are the agent's own. The
files are listed with `diff-tree` between the two commits, or with `ls-tree` of the second for a checkout that had
no commit. For each, what is in the work tree decides, by what the update did to it:

| The update | In the work tree | Done |
| :-- | :-- | :-- |
| added the file | not there | nothing |
| added the file | the whole of the file the update adds, or the start of it, or empty | removed |
| changed the file | not there | put back from the index |
| changed the file | the whole of the new file, or the start of it, or empty | put back from the index |
| changed the file | as the commit before had it | nothing |
| deleted the file | anything | nothing: it is gone, or as it was, or an edit that the next pull is `Blocked` by |
| added or changed it | anything else, a link where the commits have a file, a directory, an edit | left, and named |

A file is the whole of a file when the id that Git would give its content is the id in the commit, which is
computed while the file is read, so that a file of any size is compared and no command is run. A file is the start
of a file when it is shorter and the blob begins with it, which takes the blob, read with `cat-file`. A blob that is
longer than what the Runner lets a command write cannot be read, so a file that is the start of such a blob is
not told from an edit: it is left, and the note says that it could not be compared. A whole copy of it is
still recognised, by its id. A link is whole or not there: it is compared by what it points to, with the blob of the
link, and a link where the commits have a file is anything else. An empty file is the start of every file that has
content. Files are put back with `checkout-index -f -u` in batches of 500, and removed through
the directory of the work tree, which follows no link out of it. The files that were left are what the next pull
is `Blocked` by, with the names that the message of `Blocked` already gives, and the note names them too.

Undoing never applies the update: the files are put back as the commit before had them, HEAD does not move, and
the next pull applies the update with the cap on deletions, the fence, the recovery ref and the rest of what a pull
checks. That it begins again where it was is the point.

### The index

Before the journal is looked at, and before every pull, the index is looked at with `diff-index --cached --quiet
HEAD`, one command that reads it and writes nothing, and for a checkout with no commit with `ls-files`. An index
that Git cannot read is made again, and one that holds what the commit does not, files that are staged, is reset:
`read-tree HEAD`, or `read-tree --empty` when there is no commit. The work tree is not touched, so an edit of the
application is still an edit, as an unstaged change. Nothing here stages for long today, and #36 will: a kill in
the middle of a commit leaves files staged, which the scan of #34 would otherwise list as they are. A command that
was stopped says nothing about the index, and the index is not made again on its word. The reset comes before
the files are put back, which are put back from the index, and the index of a merge that was killed at the update
of the branch holds the new commit. It costs a read of the index and of the tree of HEAD for each pull, which is
about a tenth of a second for 50,000 files on a developer's machine.

That the checkout is on the branch that the Treebind follows was already checked at the start of every pull, and a
checkout that is not is refused before anything here is looked at.

### What the pull says

The `Pull` check and the log say what was put right, in the message of the check and in the sentence that the log
line has: `the update from <commit> to <commit> was cut off, and was undone, and the next pull applies it again: 2
file(s) put back, 1 removed`, and `the index could not be read, and was made again from the commit the checkout is
at`. A pull that failed says it too, with its own reason. No reason is new: a pull that cannot put the checkout right
is `PullFailed`, and a fence that stopped it is `Refused`.

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
  lock files are removed at the next pull, the update that was cut off is undone, and the next pull applies it.
  A cut-off update no longer leaves a checkout that is refused for good, and the files that are not the agent's
  own are left for the person whose they are, with their names.
- The tests kill a real merge that a filter holds in the middle of the files, which leaves some files whole and one not there, and a merge that a hook holds
  at the update of the branch, which leaves every file and the index. Git writes a file with one write, so the
  state of a file that is the start of the new one cannot be had on cue, and the tests put it in place by hand.
- A directory where a file was, or a file where a directory was, is not looked at specially: a path under it is
  looked at as it is, and what the update would write over is left for `Blocked`.
- A list of files that is longer than what the Runner lets a command write cannot be read, and then no file is
  put back, the journal is removed, and the note says so.
- The pull now looks at the top of the Git directory and at `refs` before each pull, which is a read of a few
  directories.
- The first checkout still removes its own `index.lock` when it resumes (ADR 0017), as `Open` alone has to.
- A lock-age metric is not made: there are no metrics yet (#47), and the report says what was removed.
- What a killed `fetch` leaves in `objects`, such as the pack that was being received, is not a lock and is not
  removed. Garbage collection is off (ADR 0013).
