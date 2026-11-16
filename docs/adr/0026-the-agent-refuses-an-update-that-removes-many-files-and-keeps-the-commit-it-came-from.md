# 0026. The agent refuses an update that removes many files and keeps the commit it came from

- Status: accepted
- Date: 2026-11-16
- Issue: #40

## Context

FR-18 asks the agent to refuse destructive surprises, and R-2 is the risk: a pull deletes the data of the
application because the remote was emptied, rewritten, or is the wrong one. Three of the four mitigations are
in. The first checkout adopts only an empty directory or one that matches the branch (ADR 0017), a pull never
applies a rewritten branch and never writes over a local edit (ADR 0020), and the agent never force-pushes.
Two are not: a cap on what one update removes, and a ref that keeps where the checkout was before it.

ADR 0020 left a place for them, a function that the caller gives the pull, called before the fence, and said
that until they are there an update that deletes files is applied. ADR 0024 said that #40 has to land before
the loop reaches a release, and has to say whether a refusal of that guard makes the wait longer.

A fast-forward cannot rewrite history, so a branch that was emptied by a new commit is an ordinary update:
nothing in Git says it is wrong. What can be said is how many files it removes from the directory.

## Decision

### Where it sits

In `internal/agent/pull`, and not in a function of the caller, because what the guard does needs the Runner and
the caller has none. `Options.Guard` is removed: nothing sets it. An update that is to be applied goes through
these steps, in this order:

1. The check of the state directory, as before.
2. **The cap.** It reads, and writes nothing.
3. **The fence**, the check of the writer lock (ADR 0012, ADR 0020).
4. **The recovery ref.** It is the first write.
5. The merge.

The ref is a write, and nothing writes without the lock (NFR-1, NFR-17), so it is made after the fence and not
before it. An update that is refused by the cap or the fence writes nothing, not even a ref.

### What counts, and the cap

The files that the update would remove from the directory are those that
`diff-tree -r --no-renames --diff-filter=D` lists between the commit the checkout is at and the one it moves
to, and that are in the directory now. A file that moved is a file removed and another added. A file that the
application removed itself is not counted, because the update does not remove it. A file that cannot be looked at
counts as there, because an update that is refused for it is tried again at the next interval, and one that is
applied is not undone. A checkout with no commit has nothing for an update to remove.

An update is refused when it removes **more than 100 files**, or when it removes **any file and leaves the branch
with no file at all**, which is an emptied remote. Both are fixed numbers of the agent and not settings (see
below). There is no share of the files as a second number: the second rule is the case that a share is for, and
a repository that was reorganised and lost most of its files to others is not a mistake to be refused.

A list of names that does not fit the Runner's limit on the output of a command, 16 MiB, is far more than the
cap, and is refused without a count.

### What a refused update does

Nothing is written. The pull ends with the reason `TooManyDeletions`, and a message that gives how many files,
the cap, and the names of three, and says what to do. The reason is API in the sense of ADR 0004.

It is a state that a person has to put right, as `Blocked` is, and not a failure of the repository or the volume,
so it **keeps the interval**: the next pull is at the next interval, which costs one question to the repository
while the tip is the same, and it is logged once, because the message holds no time and the log line is written
when what the check says changes. A refusal of the fence stays `Refused`, which now means only that the lock
could not be checked, and keeps its longer wait.

This adds a row to the table of ADR 0024, whose text stays as it is, and changes the meaning of one:

| Result of the pull | What it means | The next wait |
| :-- | :-- | :-- |
| `TooManyDeletions` | The update would remove more than 100 files from the directory, or every file of the branch. Nothing was written | The interval |
| `Refused` | The fence stopped the update: the agent could not check that it still holds the writer lock | Longer |

### How a person lets it through

There is no field in `spec` for it in this change. A field is an API decision, with a CRD marker, CEL, a setting
for the pod and the replacement of every sync pod that sets it, and a Treebind that never meets the cap would
carry it for nothing. Without it, the agent never removes more than the cap, and the person who means it does:
because the count is of the files that are in the directory, files that were removed by hand are not counted,
and the next pull goes on. Git applies a fast-forward whose deleted files are already gone, which the tests
hold. [The page about it](../deletion-guard.md) gives the commands that list the files of the update and remove
them, and the tests run those commands as they are written.

A person can set the number the same way, by removing some of the files and leaving fewer than the cap.
`spec.sync.maxDeletions`, passed to the agent in an environment variable that the operator sets only when the
field is set, so that no pod of a Treebind without it is replaced, is the change to make if removing files by
hand is not enough.

### The recovery ref

Before an update is applied the pull makes `refs/treebind/recovery/<n>`, with `<n>` a number of eight digits,
which points at the commit the checkout was at. A checkout with no commit has none to keep.

- The number is the one after the highest of the refs whose name is a number of at most 32 bits. A counter and
  not the time, so that a clock that moves does not misorder them, and not more than 32 bits, so that a ref of a
  person with a huge number cannot make it wrap. It is good for four billion updates.
- It is made with an empty old value, so that Git refuses to overwrite a ref that is there.
- **It is not made again while the newest ref already points at that commit.** An update that is `Blocked` is
  tried at every interval, and would otherwise push every older ref out in a few minutes. The result gives the
  name of the ref that was kept.
- The agent keeps the last 20. It removes the older with `update-ref -d`, naming the commit it expects, after the
  update is applied, and not before: an update that is `Blocked` tidies nothing. A ref in the namespace with a
  name that is not such a number is somebody's, and is not listed, counted or removed.
- A failure to look at the refs or to write one is a failure of the pull, `PullFailed`: an update that cannot be
  undone is not applied. The next pull tries again, and with the same commit it does not write the ref a second
  time. A failure to remove an old ref is not: the ref that keeps the commit is there, the update is applied,
  and the result carries a warning that the `Pull` check shows. The next update tries again.

A fast-forward keeps the commit it came from as an ancestor of the new tip, so the ref does not keep objects
alive today, and the agent never runs `gc`. What it gives is a name for the state before an update that does not
move with the branch, which a person can restore from without reading history, and which holds when a later
change moves the branch off that history, as the conflict policy of #38 may. The restore is on
[the page](../deletion-guard.md) with its commands, and goes through `git restore --worktree` on the files that
the ref has and the branch has not, so that the index and every other file stay as they are. A restore of a
directory from the ref would also remove the files that the branch added since, and the test of the page shows it.

### The Git directory

Only loose refs are made, in `refs/`, which the check of the Git directory allows (ADR 0019). A `packed-refs`
file, which it refuses, never appears: the Runner sets `gc.auto` and `maintenance.auto` off, and nothing here runs
`gc` or `pack-refs`. The tests make refs, remove them, and look at the Git directory with the same check
afterwards. The commands that are run are `for-each-ref` and `update-ref`, which the Runner already allows, and
`diff-tree` and `ls-tree`.

### What the tests hold

These claims about Git are run against a real repository, and the tests also run in the image of the agent:
a fast-forward applies when the files it deletes were already removed by hand; creating and removing refs
leaves no `packed-refs` and nothing else in the Git directory that the agent does not make; `update-ref` with an
empty old value refuses a ref that exists; `diff-tree` lists a moved file as removed; and the commands of the
page for restoring and for letting an update through do what the page says, on a layout with a `subPath` and one
without. The commands of the page need a shell, which the image of the agent has not, so they run on the host
and in an image that has one.

## Consequences

- The sync pod template does not change, so no sync pod is replaced by this (#87).
- An update that removes more than 100 files, or all of them, is not applied until a person removes the files,
  and says so in the `Pull` check at every interval. For a repository where that is routine, a field is the
  next change.
- The cap stops a surprise and not a sequence: updates that each remove fewer than 100 files remove as many as
  they like. A person with write access to the repository can also remove the data of the application, as
  they could before. What the cap is for is a mistake.
- A list of names over 16 MiB is refused without a count, and removing files cannot let it through, because
  the list is of the commits and not of the directory. That is some hundred thousand files in one update.
- A `Blocked` update leaves a ref behind that keeps the commit the checkout is still at. That is harmless, and
  the ref is reused when the update is applied.
- A process that is killed while it writes a ref leaves the `.lock` file that Git makes, and Git does not
  remove it, so the pulls fail with `PullFailed` until a person does. The index has the same fault, which a
  pull does not handle either, and a change that handles both is a separate one. A lock left next to an old
  ref does not stop the updates: the old ref stays, and the `Pull` check says so.
- Files that a person removes by hand to let an update through are removed from an application that may use
  them. That is the reason the agent does not do it.
- The refs of a Git directory that was made before this change are as they are: there are none, and the first
  update makes the first.
