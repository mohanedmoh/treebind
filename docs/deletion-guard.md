# What the agent does before it removes your files: the cap and the recovery ref

A branch that is emptied by mistake, or a repository that was reorganised, would empty your directory at the
next pull. So before the agent applies an update (see [pulling](pull.md)) it looks at what the update would
remove from your directory, and it keeps the commit your directory is at under a ref of its own, so that
the files can be got back. The decisions behind it are in
[ADR 0026](adr/0026-the-agent-refuses-an-update-that-removes-many-files-and-keeps-the-commit-it-came-from.md).

## The cap

An update is **not applied** when it would remove from your directory

- more than **100 files**, or
- any file, and leave the branch with no file at all.

The count is of the files that the update removes and that are in your directory now: a file that your
application removed itself is not counted. A file that moved is counted as removed. Nothing is written, the
check `Pull` says `TooManyDeletions` with how many files and the names of a few, and the agent asks again at
the next interval, as it does for `Blocked`:

```
kubectl describe treebind <name>
```

The 100 is the cap of a Treebind that does not say otherwise. The rule about a branch with no file at all is
not a number.

### Letting the update through

There are two ways. If the update is what you mean, raise the number: set `spec.sync.maxDeletions` of the
Treebind to the number of files that the message says, or more. The sync pod is replaced once, and the update
goes on at its first pull.

```yaml
spec:
  sync:
    maxDeletions: 500
```

The number stays until you change it, so an update that removes as many files goes through next time as well.
Take the field away to go back to 100.

The other way is for an update that leaves the branch with no file at all, which no number lets through, and
for anyone who would rather not change the spec: you remove the files, and the next pull goes on, because the
update then has few files left to remove. These commands run in a pod that has `git` and a shell, and that mounts
the **whole claim**, not only the `subPath`, because the Git directory is beside the directory that is synced
(see [the Git directory](git-directory.md)). `MOUNT` is where the claim is mounted in that pod, `SUB` is
`spec.target.subPath` (`.` when there is none) and `BRANCH` is `spec.repository.branch`. Run them as the user of
the agent (`65532`, or the user ID that the platform gives the sync pod), so that Git and the files keep their owner.

The files the update would remove:

<!-- tested: list-the-files-of-the-update -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
git diff-tree -r --no-renames --diff-filter=D --name-only HEAD "refs/remotes/origin/$BRANCH"
```

Removing them, which is what the agent declined to do. Look at the list first:

<!-- tested: remove-the-files-of-the-update -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
git diff-tree -r -z --no-renames --diff-filter=D --name-only HEAD "refs/remotes/origin/$BRANCH" |
  (cd "$GIT_WORK_TREE" && xargs -0 -r rm -f --)
```

You can also remove only some of them, as long as what is left is not more than the cap.

## The recovery ref

Before it applies an update, the agent keeps the commit your directory is at under
`refs/treebind/recovery/<n>`, where `<n>` is a number of eight digits that goes up with every update. A
checkout that has no commit yet has none to keep. The agent keeps the last 20 and removes the older ones after
an update is applied.
An update that is `Blocked` and tried again at every interval does not make a new ref each time. A ref in that
namespace whose name is not a number is yours: the agent leaves it alone and does not count it.

The commit stays in the history of the branch, so the ref does not keep it alive today. What it gives you is
a name for the state your directory was in before an update, which does not move with the branch and which
holds when a later change moves the branch off that history.

### Getting files back

The same pod as above. `REF` is the ref to go back to, for example `refs/treebind/recovery/00000007`.

<!-- tested: restore-the-files-of-a-recovery-ref -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
# The refs, the commit each keeps, and its first line:
git for-each-ref --format='%(refname) %(objectname:short) %(subject)' refs/treebind/recovery/
# The files that the commit has and the branch has not:
git diff-tree -r --no-renames --diff-filter=D --name-only "$REF" HEAD
# Putting those files back:
git diff-tree -r -z --no-renames --diff-filter=D --name-only "$REF" HEAD |
  xargs -0 -r git restore --source="$REF" --worktree --
```

The files come back as they were in that commit and nothing else changes. Do not give a directory to
`git restore --source=<ref> -- <dir>` instead: that also removes the files that the branch added to the
directory since. The files you put back are files of your application like any other: an update that
changes the same path is `Blocked` until you remove them or let them be committed.

## Things to know

- **Do not run `git gc` or `git pack-refs` in the Git directory.** They pack the refs into a `packed-refs`
  file, which the agent refuses in its Git directory (`GitDirUnsafe`).
- **A very large update cannot be let through from here.** When the names of the files that an update removes
  do not fit in 16 MiB, some hundred thousand files, the agent refuses it without a count, because it cannot
  list them. The list is of the commits and not of your directory, so removing files does not shorten it, and
  `spec.sync.maxDeletions` is not looked at. Put the files back in the repository with a commit, so that the
  update has little to remove, and push the removal again in steps that each remove fewer files, with
  `spec.sync.maxDeletions` raised to what a step needs.
- **A crash in the middle of writing a ref leaves a `.lock` file next to it**, as it does for the index. Git
  does not remove it. The pull says `PullFailed`, with the path of the file, until you remove it. A lock next to
  an old ref does not stop the updates: the ref stays, and the check `Pull` says that it could not be removed.
