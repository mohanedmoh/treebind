# What the agent does after the checkout: pulling

Once the checkout is made (see [the checkout](checkout.md)), the agent pulls: every `spec.sync.interval`, 30
seconds when it is not set, it asks the repository for the tip of the branch, and if there is a newer
commit it brings it into your directory. The decisions behind it are in
[ADR 0020](adr/0020-the-agent-pulls-by-fast-forward-and-never-writes-over-a-local-edit.md) and
[ADR 0024](adr/0024-the-agent-pulls-on-a-schedule-that-spreads-the-load-and-waits-longer-while-the-repository-fails.md),
and what it does before it removes files is in [ADR 0026](adr/0026-the-agent-refuses-an-update-that-removes-many-files-and-keeps-the-commit-it-came-from.md).

The result is the check `Pull` in the agent's report, and a line in its log when what it says changes:

```
kubectl logs <name>-sync-0
kubectl describe treebind <name>
```

## What it does

- **It never writes over your application's edits.** An update that would change a file that has a local edit,
  or that would put a file where the application made one of its own, is not applied at all. A file that a
  `.gitignore` hides is the application's too. Every other local edit stays as it is.
- **It applies a fast-forward, and resolves a conflict by the policy.** When the repository and your directory each have
  commits that the other lacks, a `bidirectional` Treebind resolves it with `spec.sync.conflictPolicy` (see
  [conflicts](conflicts.md)): `followRemote`, the default, makes the repository win, and `followLocal` makes your directory
  win. A `pull` Treebind has no commits of its own, and says `Diverged` and writes nothing.
- **It applies the deletions of the branch, up to a cap.** A file that the branch removed is removed from your
  directory. An update that would remove more than 100 files from it, or every file of the branch, is not
  applied, and the check says `TooManyDeletions`. See [the deletion guard](deletion-guard.md).
- **It keeps where your directory was before an update.** The commit it was at is kept under
  `refs/treebind/recovery/<n>`, so that removed files can be got back. The same page says how.
- **It undoes an update that was cut off.** A Git that is killed in the middle of the files of an update leaves some of
  them written. The pull that follows puts back the ones that are the agent's own, says so in the check, and applies the
  update again. A file that is yours is left, and the pull is `Blocked` by it. Until then nothing is committed, and the
  first pull after a start that finds such an update comes at once. It makes the index again when it cannot be read.
  See [the Git directory](git-directory.md).
- **It does not cut off an update that is writing files.** When the agent is told to stop, the update that is
  being applied has up to 10 seconds to finish. Git that is stopped is told to end first, so that it removes its
  own lock files, and the agent removes the ones that a Git that was killed left before each pull and says so in
  the check. See [the Git directory](git-directory.md).
- **It asks cheaply.** Most pulls are one question to the repository, "what is the tip?", and fetch nothing.
- **It spreads its questions.** Each wait is the interval give or take a fifth, and the first pull of a Treebind
  comes after a point in the interval that comes from its name, so that many Treebinds on one Git host do not
  ask it together. After a failure of the repository the wait grows, up to ten minutes, and goes back to the
  interval at the first pull that works.
- **Both directions pull.** `bidirectional` also looks for what the application changed, commits it and pushes the
  commits, which is [its own page](local-changes.md); a `pull` Treebind does none of that.

## What the check says

| Result | What it means | What to do |
| :-- | :-- | :-- |
| `Passed`, at the tip | Your directory has what the branch has | Nothing |
| `Passed`, advanced | The agent brought in new commits. The message names the recovery ref that keeps the commit your directory was at | Nothing |
| `Passed`, ahead | Your directory has commits that the repository does not have yet | Nothing: a `bidirectional` Treebind pushes them ([local changes](local-changes.md)) |
| `Passed`, resolved | Your directory and the repository had each gained commits, and the policy decided. The message says which, and the check `Conflict` says how | Nothing, or read [conflicts](conflicts.md) to get back a file that lost |
| `Skipped` `NotChecked` | The volume, the lock or the checkout has not passed, or the first pull is not due yet | Read the check that failed |

## When it does not work

The agent stays up, says why in the `Pull` check, and tries again. It carries on once the cause is put right,
with no restart. [Recovering from a conflict or a rejected push](recovering-from-a-conflict-or-a-rejected-push.md) starts
from the conditions of the Treebind and says what to do.

| Reason | What it means | What to do | The next pull |
| :-- | :-- | :-- | :-- |
| `Blocked` | A file that the update changes has a local edit, or an untracked file is in its way. The message names some of them. Nothing was written | Commit, revert or remove the files. The next pull goes on | At the interval |
| `Diverged` | Your directory and the branch each have commits that the other lacks, and the Treebind only pulls, so it has no policy to say which side wins. The message says how many. Nothing was written. A branch that was force-pushed looks like this | Nothing is lost. Decide which side is right: [recovering from a conflict or a rejected push](recovering-from-a-conflict-or-a-rejected-push.md#a-branch-that-was-rewritten-under-a-treebind-that-only-pulls) says how to make the volume follow the branch | At the interval |
| `BranchGone` | The repository has no such branch any more | Put the branch back, or set `spec.repository.branch` to one it has | At the interval |
| `PathNotFound` | The branch has something at `spec.repository.path` that is not a directory. A directory that the repository removed is an update that removes its files, which [the deletion guard](deletion-guard.md) judges. Nothing was written | Put the directory back, or check `spec.repository.path` | Longer, up to ten minutes |
| `PathMismatch` | The checkout was made for another directory, or for the whole repository: see [the directory of the repository](repository-path.md). Nothing was written | Remove or set back `spec.repository.path`, or start again | Longer, up to ten minutes |
| `ReservedPath` | The update brings a file named `.treebind` into a directory that holds the agent's state | Use a `subPath`, or rename the file | At the interval |
| `GitDirUnsafe` | The Git directory holds something the agent did not put there | See [the Git directory](git-directory.md) | At the interval |
| `TooManyDeletions` | The update would remove more than 100 files from your directory, or every file of the branch. The message says how many and names some. Nothing was written | Check the branch. If the update is meant, raise `spec.sync.maxDeletions` or remove the files yourself, as [the deletion guard](deletion-guard.md) says, and the next pull goes on | At the interval |
| `TooManyCommits` | A conflict would need a commit, and the agent made as many commits in the last hour as `spec.commit.maxPerHour` allows. The conflict was not resolved and nothing was written | See [echo loops](echo-loops.md). The next pull goes on once one of the commits of the hour is an hour old | At the interval |
| `FetchFailed` | The repository cannot be reached, the key is refused, or the host key is not in the known hosts. The message has what Git said | Check the address, the deploy key and the known hosts. The agent recovers by itself | Longer, up to ten minutes |
| `PullFailed` | The agent could not read or write the volume, as for a fetch that has no room on it, or the checkout is not what a pull expects, or it could not set up what pulls, or it could not remove a lock file that a killed Git left. After a write that the volume refused, the agent removes the temporary object files that Git left, and the message names them ([the Git directory](git-directory.md#room-on-the-claim)) | The message says which. Make room on the claim | Longer, up to ten minutes |
| `Refused` | The update was not applied, or lock files were not removed, because the agent could not check that it still holds the writer lock | The agent stops with the exit code 3 if the lock is lost, and its pod's restart waits for the lock again | Longer, up to ten minutes |

## Things to know

- **The first pull is up to one interval after the checkout.** It is not a delay of the checkout: the checkout
  has just fetched the branch. After a start that finds an update that was cut off, it comes at once.
- **Commits on the branch that remove files remove them from your directory**, up to 100 files in one update.
  The commit your directory was at is kept, and [the deletion guard](deletion-guard.md) says how to get files
  back from it.
- **A conflict is resolved by an update, so it is refused for what an update is refused for.** A file that has a local edit and
  that the resolution would change, an untracked file in its way, too many deletions and a lost lock are `Blocked`,
  `TooManyDeletions` and `Refused` for a resolution as they are for an update, and nothing is written.
- **A pull that is `Blocked`, `Diverged` or `TooManyDeletions` is tried again at every interval**, and is logged
  once. It costs one question to the repository while the tip does not change.
- **Changing `spec.sync.interval` replaces the sync pod.** The new pod uses the new interval.
- **The agent is ready once a pull has passed, and stays ready.** The sync pod, and so the `Ready` condition of the
  Treebind, turns ready after the first pull that is `Passed`, whether the checkout was at the tip, advanced or
  ahead. A pull that fails later does not make the pod not ready: the files are as they were, and the check says what the
  pull found. The condition `Synced` says it in the Treebind, and turns `Ready` `False` only for a reason that a person puts
  right, as [the conditions](agent-status.md#ready) say. A repository with no commit yet, and a branch that is gone, say
  `BranchGone`, so the Treebind is not ready until the branch has a commit. See [the probes](agent-status.md#the-probes).
