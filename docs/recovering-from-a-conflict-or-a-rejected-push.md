# Recovering when the sync stops: a conflict that was not resolved and a push that the host refused

A Treebind can stop short of the repository for reasons that the agent will not decide for you: the Git host refuses a push, or your
directory and the branch each changed in a way that the policy cannot apply. This page is for the
person who runs the Treebind and finds `Synced` or `Ready` not `True`. It starts from what `kubectl` shows, says what each reason
means and what the agent does by itself, and then what you do, with commands that were tried against real Git repositories. The
decisions behind the reasons are in
[ADR 0033](adr/0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md),
[ADR 0034](adr/0034-the-agent-resolves-a-conflict-by-the-policy-of-the-treebind-one-file-at-a-time-in-the-update-of-a-pull.md) and
[ADR 0037](adr/0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md).

Whatever the reason, the agent does not force a push and pushes nothing but the branch it follows, never rewrites the history of the
repository, never writes over a file that has an edit that is not committed yet, and before it applies an update keeps the commit
your directory was at under a ref of its own, so that files that the update removes can be got back
([the deletion guard](deletion-guard.md)). When it cannot go on it says why in its report, tries again, and goes on by itself once
the cause is put right.

## Start with what `kubectl` shows

```
kubectl get treebind <name>
```

shows `Ready`, `Synced` and `Conflict`. To read the conditions with their reasons and messages, and the commit at which the
volume and the repository last agreed:

<!-- tested: read-the-conditions -->
```sh
kubectl get treebind <name> -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
kubectl get treebind <name> -o jsonpath='{.status.lastSyncedCommit}{"\t"}{.status.lastSyncTime}{"\n"}'
```

For a Treebind whose last push was refused, the first command prints this, one line to a condition (the order of the lines can
differ):

<!-- output: a-push-that-the-host-refused -->
```
SpecSupported	True	Supported	Every field set in the spec is implemented
Ready	False	NotSynced	1 of 1 sync pods ready, but Synced is False: PushRejected
Synced	False	PushRejected	The last push failed: the repository refused the push, as for a protected branch, a hook or a rule. The volume and the repository last agreed at commit 0123456789ab. The log of the sync pod says more
Conflict	False	NoConflict	The sync agent knows of no conflict between the volume and the repository that waits to be resolved, and has resolved none since it started
AgentUnreachable	False	ReportReceived	The operator read the sync agent's report
```

The second command prints the full id of that commit, a tab and the time. `kubectl describe treebind <name>` shows the same
conditions, and below them the events of the Treebind, one for each change of `Synced` and `Conflict`, with its time and the
reason of the condition, and of the type `Warning` for a failure. To list them alone:

<!-- tested: read-the-events -->
```sh
kubectl events --for treebind/<name>
```

[Metrics and events](metrics-and-events.md#events) says which change makes which event. What each condition says
([the status page](agent-status.md#what-synced-and-conflict-say) has the whole of it):

- **`Synced` `True` with `InSync`.** No step of the sync has failed, and the agent has seen the volume and the repository agree. If
  something you expected has not reached the repository, the sync did not fail: see
  [my change has not reached the repository](#my-change-has-not-reached-the-repository).
- **`Synced` `False`.** A step failed, and the reason is the one of that step. The message says which step, what the reason means,
  and at which commit the volume and the repository last agreed. It does not say which file or what the Git host said: the
  messages of a condition are fixed sentences, because everyone who can read the Treebind reads them, and the names of files and
  the words of a host are in the agent's check, which [the log](#reading-the-agents-log-and-report) has.
- **`Synced` `Unknown`.** `SyncPending` is a start that has not seen the two agree yet, which is no failure
  ([below](#a-sync-that-is-stuck-and-says-nothing-alarming)); `AgentOutdated`, `AgentUnreachable` and `AwaitingAgentReport` say that
  the operator does not know what the sync does, and [the status page](agent-status.md) says why.
- **`Conflict` `True`.** The last pull found that your directory and the repository each have commits that the other lacks, and
  could not resolve it: [a conflict that was not resolved](#a-conflict-that-was-not-resolved). `Conflict` `False` with
  `ConflictResolved` is a conflict that the policy did resolve.
- **`Ready` `False` with `NotSynced`.** The sync pod is ready, but `Synced` is `False` with a reason that a person puts right. For
  `FetchFailed`, `PushFailed` and, in a `bidirectional` Treebind, `NonFastForward` and `Blocked`, `Ready` stays `True`, because the
  next try puts them right by itself. A Treebind whose Git host is down is `Ready` and `Synced` is `False`: read `Synced`, and not
  `Ready` alone.

What you do depends on the direction, the policy and whether the Treebind syncs a directory of the repository:

<!-- tested: read-the-settings -->
```sh
kubectl get treebind <name> -o jsonpath='{.spec.sync.direction}{"\t"}{.spec.sync.conflictPolicy}{"\t"}{.spec.sync.interval}{"\t"}{.spec.repository.path}{"\n"}'
```

### Which section deals with a reason

`Ready` is what it is when `Synced` says the reason, as [the status page](agent-status.md#ready) says. A reason that does not have
its section here is for the page that the last column names.

| Reason of `Synced` `False` (and of `Conflict` `True`) | The step | `Ready` | Where |
| :-- | :-- | :-- | :-- |
| `PushRejected` | `Push` | `False` | [A push that the host refused](#a-push-that-the-host-refused) |
| `NonFastForward` | `Push` | stays `True` for a `bidirectional` Treebind | [A push that the host refused](#a-push-that-the-host-refused): the next pull puts it right |
| `PushFailed` | `Push` | stays `True` | [A push that the host refused](#a-push-that-the-host-refused): the host or the key |
| `FetchFailed` | `Checkout`, `Pull` | stays `True` | [A push that the host refused](#a-push-that-the-host-refused): the host or the key |
| `Blocked` | `Pull` | stays `True`; `False` for a Treebind that only pulls | [A conflict that was not resolved](#a-conflict-that-was-not-resolved) |
| `TooManyDeletions` | `Pull` | `False` | [A conflict that was not resolved](#a-conflict-that-was-not-resolved) |
| `TooManyCommits` | `Pull`, `Commit` | `False` | [A conflict that was not resolved](#a-conflict-that-was-not-resolved) |
| `Refused` | `Pull` | `False` | [A conflict that was not resolved](#a-conflict-that-was-not-resolved), and [the lock](#another-agent-holds-the-lock) |
| `PullFailed` | `Pull` | `False` | [A conflict that was not resolved](#a-conflict-that-was-not-resolved) |
| `Diverged` | `Pull`, a Treebind that only pulls | `False` | [A branch that was rewritten](#a-branch-that-was-rewritten-under-a-treebind-that-only-pulls) |
| `LockFailed` | `Lock` | `False` | [Another agent holds the lock](#another-agent-holds-the-lock) |
| `CommitFailed`, `ScanFailed` | `Commit`, `Changes` | `False` | [My change has not reached the repository](#my-change-has-not-reached-the-repository) |
| `GitDirUnsafe` | `GitDir`, `Checkout`, `Pull`, `Changes`, `Commit`, `Push` | `False` | [The Git directory](git-directory.md) |
| `MountPathInvalid`, `NotMounted`, `NotWritable`, `ReadOnlyVolume`, `WriteFailed`, `TimedOut` | `Mount`, `Writable`, `GitDir` | `False` | [Pod security and file ownership](pod-security.md), and [the checks of the volume](#a-sync-that-is-stuck-and-says-nothing-alarming) |
| `DirectoryNotEmpty`, `RepositoryMismatch`, `BranchNotFound`, `ReservedPath`, `CheckoutFailed` | `Checkout` | `False` | [The checkout](checkout.md) |
| `PathNotFound`, `PathMismatch` | `Checkout`, `Pull` | `False` | [The directory of the repository](repository-path.md) |
| `BranchGone` | `Pull` | `False` | [Pulling](pull.md#when-it-does-not-work) |
| `SyncFailed` | any | `False` | A reason that this release of the operator does not define, as an agent of another release gives: the log of the sync pod says which, and the operator and the agent should be of one release |

## Reading the agent's log and report

The agent says in its log, once for each change, what each check came to. The host's words and the names of files are there and
in the report, and not in the conditions:

```
kubectl logs <name>-sync-0
kubectl logs <name>-sync-0 | grep 'check=Push'
kubectl logs <name>-sync-0 --previous
```

A line has the check, the outcome, the reason and the message, and a check that did not pass is at the level `WARN`. `--previous` is the container
before a restart, which says why it stopped. A check that fails in the same way at every try is one line, so a line that is old
can still be what the agent says now: `Synced` says whether it is. The whole report, with the order of the checks, is read as
[the status page](agent-status.md#see-what-the-operator-sees) says, and [what it holds](agent-status.md#what-the-report-holds) is
listed there.

## Before you run a command on the volume

The commands below that set `GIT_DIR` run in a pod that has `git` and a shell and that mounts the **whole claim**, as those of
[the deletion guard](deletion-guard.md#letting-the-update-through) do, as the user of the agent (`65532`, or the user ID that the platform gives the sync pod), so that Git and the files keep
their owner. `MOUNT` is where the claim is mounted in that pod, `SUB` is `spec.target.subPath` (`.` when there is none) and `BRANCH` is
`spec.repository.branch`. Two things to know first:

- **The agent does not know about your commands.** It runs one Git command at a time and holds the writer lock of the volume, which a
  pod of yours does not take. If a command of yours says `Unable to create '...lock': File exists`, the agent had the lock of that file
  at that moment, or removed yours as it removes the lock of a Git that was stopped
  ([the Git directory](git-directory.md#when-git-was-stopped-in-the-middle-of-a-write)): run it again. Commands that only read change
  nothing.
- **They were tried on a Treebind that syncs the whole repository.** With `spec.repository.path` the commits of the checkout are
  snapshots of the directory ([the directory of the repository](repository-path.md)), and these commands were not tried there.

## A push that the host refused

`Synced` is `False` with `PushRejected`, `NonFastForward` or `PushFailed`, and the `Push` check, which comes after `Commit`, says the
same with its message. The three are not the same:

| Reason | What it means |
| :-- | :-- |
| `PushRejected` | The host answered and refused: a protected branch, a hook, a rule, a file that is too large, a secret that it found. The message has what Git said and then the words of the host |
| `NonFastForward` | The repository has commits on the branch that the checkout lacks, so the push would not be a fast-forward, and it is not forced |
| `PushFailed` | The push could not be made: the host did not answer, the key was refused, the volume refused, or the writer lock could not be checked |

`FetchFailed` is the same kind of reason for the pull: the repository cannot be reached, the key is refused, or the host key is not in
the known hosts ([pulling](pull.md#when-it-does-not-work)). It comes from the `Pull` check, or from the `Checkout` check on a first
start.

### What the agent does by itself

- **It asks again.** After `PushRejected` and `NonFastForward` it tries the push again after the interval, give or take a fifth. After
  `PushFailed` it waits longer each time, up to ten minutes. A pull that fails for `FetchFailed` waits the same way
  ([pulling](pull.md#when-it-does-not-work)). It goes on at the first try that works, with no restart.
- **It keeps committing.** What the application writes is committed as before, and the commits wait on the branch of the checkout, in
  the Git directory, and are pushed together when the host takes them. Nothing is put on a side branch for a push that is refused, and
  nothing is pushed to another branch or forced: a command that would is refused by the Runner before Git starts.
- **It puts `NonFastForward` right.** In a `bidirectional` Treebind the next pull brings the commits that the push lacked and, when the
  checkout has commits of its own, resolves the conflict with `spec.sync.conflictPolicy` ([conflicts](conflicts.md)). The push is tried
  again at the next scan after a resolution that made a commit, without waiting for the interval. You do nothing unless the `Pull`
  check fails: then it is [a conflict that was not resolved](#a-conflict-that-was-not-resolved).

### What you do

Read what the host said, in the `Push` check ([the log](#reading-the-agents-log-and-report)). The line below is from a test repository
whose hook declines with words like those of a host, which each word in their own way. The agent keeps what follows `remote:` in Git's
output, and that is what comes after `It said:`:

<!-- output: the-words-of-a-host -->
```
time=2026-11-20T09:15:03.512Z level=WARN msg=push check=Push outcome=Failed reason=PushRejected message="the repository refused the push of the branch main (pre-receive hook declined). It said: GH006: Protected branch update failed for refs/heads/main. Changes must be made through a pull request."
```

| What the check says | What you do |
| :-- | :-- |
| `PushRejected` and a rule: a protected branch, a required pull request, a restriction on who may push | Change the rule, or allow the deploy key that the Treebind uses to push to the branch. The agent tries again after the interval and needs no restart. It will not open a pull request or push to another branch |
| `PushRejected` and a file, a secret or a size that the host will not take | The commit that holds it is one of those that wait. A host that reads every commit of a push looks at it again, so a later commit that removes the file does not help: the commit has to come off the branch. Take the file out of the volume or out of what is committed, and then take the commits off the branch, below |
| `PushFailed`, or a host that does not answer | Check the address, that the deploy key can write to the repository, and the known hosts. The agent recovers by itself once the host answers. A key that can only read is a thing to check whichever reason you see |

To see what waits, which is what the host is sent:

<!-- tested: see-what-waits-to-be-pushed -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
# The commits that wait, with the files each changed:
git log --stat --format='%h %an %s' "refs/remotes/origin/$BRANCH..HEAD"
# The five largest objects that they would send, with their size in bytes and where they are:
git rev-list --objects "refs/remotes/origin/$BRANCH..HEAD" |
  git cat-file --batch-check='%(objectsize) %(objecttype) %(rest)' | sort -n | tail -5
```

To compare the volume's branch with the repository's, as the agent last saw it (the last fetch, or the last push that worked), which
says how many commits each side has that the other lacks:

<!-- tested: compare-with-the-repository -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
# The two counts: the commits that only your directory has, then the commits that only the repository has:
git rev-list --left-right --count "HEAD...refs/remotes/origin/$BRANCH"
# The commits themselves, marked < for your directory and > for the repository:
git log --left-right --format='%m %h %an %s' "HEAD...refs/remotes/origin/$BRANCH"
```

A host that refuses a push does not move `refs/remotes/origin/<branch>`, so a repository that moved meanwhile shows only after the next
pull has fetched it.

To take the commits that wait off the branch and keep every file as it is on the volume, first take what the host refused out of the
volume, or out of what is committed with `spec.paths.exclude` (the sync pod is replaced once), so that the next scan does not commit
it again. Then:

<!-- tested: drop-the-commits-that-wait -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
# Keep the commits first, under a name of your own. A ref in this namespace whose name is not a number is yours, and the agent leaves it alone:
git update-ref "refs/treebind/recovery/by-hand-$(date +%Y%m%d-%H%M%S)" HEAD
# Then move the branch back to what the repository has. The files of your directory are not touched:
git reset --mixed "refs/remotes/origin/$BRANCH"
```

The files of those commits are changes of your application now, like any other: the agent finds them at its next scan, commits those
that have settled, and pushes. That is why the file that the host refused has to be out of reach of the scan first. The commits that you
kept are in the ref, and `git log` on the ref shows them.

## A conflict that was not resolved

A `bidirectional` Treebind resolves a conflict between your directory and the repository by itself, with the policy
`followRemote`, the default, or `followLocal` ([conflicts](conflicts.md)). When it cannot, `Conflict` is `True` and `Synced` is `False`,
with the reason of the update that could not be made, and the `Pull` check says
`the conflict between the checkout and the branch <branch> of the repository was not resolved`, and why. A pull that passes without
finding a conflict puts `Conflict` back.

### What the agent does by itself

- **It does not move the checkout and writes none of your files.** They are as the application left them, and the application can go
  on writing. It may have made the ref that keeps your commits, which does no harm ([conflicts](conflicts.md#getting-back-a-version-that-lost)).
- **It asks again at every interval**, for `Blocked`, `TooManyDeletions` and `TooManyCommits`, and after a longer wait, up to ten
  minutes, for `PullFailed` and `Refused`. It goes on at the first pull that can resolve it, with no restart.
- **It keeps committing** what the application writes, and keeps the commits. Nothing is pushed while the pull cannot resolve: the push
  is `NonFastForward` and is not sent, which is the same conflict again.

### What you do

Read the `Pull` check ([the log](#reading-the-agents-log-and-report): `grep 'check=Pull'`), which names some of the files, and:

| Reason | What it means | What you do |
| :-- | :-- | :-- |
| `Blocked` | A file that the resolution would change has an edit that is not committed, or an untracked file is in its way | In a `bidirectional` Treebind nothing, usually: the agent commits the edit at its next scan and the pull goes on. It stays `Blocked` when the file is one that the agent does not commit: a file that a `.gitignore` hides, or one that `Changes` says is `left out:` (a secret by its name, a file that `spec.paths` leaves out). Revert it, remove it or move it |
| `TooManyDeletions` | The resolution would remove more files than `spec.sync.maxDeletions` allows, 100 by default, or every file of the branch | Check what the repository removed. If it is meant, raise `spec.sync.maxDeletions` to the number in the message, or remove the files yourself ([the deletion guard](deletion-guard.md#letting-the-update-through)) |
| `TooManyCommits` | The resolution needs a commit, and the agent made as many in the last hour as `spec.commit.maxPerHour` allows, 60 by default. The `Commit` check says it too when the files that settle cannot be committed | Wait: it goes on when one commit of the hour is an hour old. If it keeps coming back, something writes in a loop ([echo loops](echo-loops.md)) |
| `Refused` | The agent could not check that it still holds the writer lock | [Another agent holds the lock](#another-agent-holds-the-lock) |
| `PullFailed` | Git failed on the volume, or the checkout is not what a pull expects | The message says which. A lock file that a killed Git left is removed by the agent, which says so |

### What `ConflictResolved` means when it is `False`

`Conflict` `False` with `ConflictResolved` is not a problem. It says that the last conflict that the agent found since it started was
resolved by the policy, the sync went on, and `Ready` was not touched. The `Conflict` check says which policy, how many commits each
side had, how many files had the winner's version, and where the commits that lost are kept. If a file of yours lost and you want it
back, that is the next section. The agent keeps this in memory: after a restart `Conflict` says `NoConflict` until a pull finds a
conflict again.

### Where each policy leaves your version

| Policy | Who wins | Where the version that lost is |
| :-- | :-- | :-- |
| `followRemote` | The repository | In the commits of your directory, under `refs/treebind/local/<number>` in the Git directory. They are never pushed, and the last 20 are kept |
| `followLocal` | Your directory | The repository's version is in the history of the branch: the merge commit that the agent made has your directory's last commit as its first parent and the repository's tip as its second |
| Either, when your directory and the branch hold the same files | Nobody: there is nothing to decide | Your commits are under `refs/treebind/local/<number>`, as for `followRemote` |

## Getting your version back

The agent never joins two edits of one file, because a configuration that is right in each edit can be wrong with both: the whole file
of the winner is taken ([conflicts](conflicts.md#what-decides-the-file-and-the-side-that-wins)). What lost is kept, and these commands
find it. They only read until the one that puts a file back. `REF` is the ref to look at, for example `refs/treebind/local/00000001`,
and `FILE` is a path in the repository, without the `subPath`.

The refs, the commit each keeps, and its first line:

<!-- tested: list-the-refs-of-your-version -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
git for-each-ref --format='%(refname) %(objectname:short) %(subject)' refs/treebind/local/
```

What your directory changed that the branch did not have, and one of the files as it was:

<!-- tested: see-what-your-directory-changed -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
# The commits that your directory had and the branch of the repository did not:
git log --format='%h %an %s' "refs/remotes/origin/$BRANCH..$REF"
# The files they changed, each marked M (changed), A (added) or D (deleted):
git diff --name-status "$(git merge-base "$REF" "refs/remotes/origin/$BRANCH")" "$REF"
# One of the files as your directory had it:
git show "$REF:$FILE"
```

To put a file back as your directory had it, over the repository's version that is there now:

<!-- tested: put-back-a-file-of-your-version -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
git restore --source="$REF" --worktree -- "$FILE"
```

The file is an edit of your application's like any other: the agent commits it when it has settled and pushes it, so your version
becomes the repository's. If your directory had deleted the file, it is not in the ref, and you delete it again.

To keep both edits of a file after `followRemote`, join them, which Git can when they changed different places. The file that is in
your directory now is the repository's version, the common ancestor is the commit that the two sides shared, and the result is
written beside the file and not over it. Where both edits changed the same place, Git cannot choose, and it leaves both versions in
the result between marks that begin with `<<<<<<<` and `>>>>>>>`. The block shows the result and says how many such places it has:

<!-- tested: join-the-two-versions-of-a-file -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
WORK=$(mktemp -d)
git show "$(git merge-base "$REF" HEAD):$FILE" > "$WORK/shared"
git show "$REF:$FILE" > "$WORK/yours"
# The exit status of merge-file is the number of places that both edits changed, up to 127, and 255 when it could not do its work.
# It is kept in STATUS, so that a conflict does not stop the block, and shown below.
STATUS=0
git merge-file -p "$GIT_WORK_TREE/$FILE" "$WORK/shared" "$WORK/yours" > "$WORK/joined" || STATUS=$?
if [ "$STATUS" -eq 255 ]; then
  rm -f "$WORK/joined"
  echo "merge-file failed with exit status 255: nothing was joined, and your directory is as it was"
else
  cat "$WORK/joined"
  echo "merge-file exit status $STATUS, places marked: $(grep -c '^<<<<<<<' "$WORK/joined"), joined file $WORK/joined, your directory is as it was"
fi
```

Read the result. Where it has marks, put it right in `$WORK/joined`: keep one of the two versions, or write both in, and take out the
marks. Run the next command only then, in the same terminal: the agent commits and pushes what is in your directory once it has
settled, marks included. It puts the joined file where the application reads it, and does not copy a file that still has a line that
begins with `<<<<<<<` or `>>>>>>>`:

<!-- tested: put-the-joined-file-in-place -->
```sh
if [ -z "$WORK" ] || [ -z "$FILE" ] || [ -z "$MOUNT" ] || [ -z "$SUB" ] || [ ! -f "$WORK/joined" ]; then
  echo "WORK, FILE, MOUNT or SUB is not set, or there is no joined file: run the block above first, in this terminal. Nothing was copied"
elif grep -q -e '^<<<<<<<' -e '^>>>>>>>' "$WORK/joined"; then
  echo "$WORK/joined still has conflict marks: take them out and run this block again. Nothing was copied"
else
  cp "$WORK/joined" "$MOUNT/$SUB/$FILE"
fi
```

With `followLocal` it is the repository's version that lost. It is in the last merge that the agent made:

<!-- tested: find-the-version-of-the-repository-that-lost -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
MERGE=$(git log --merges -1 --format=%H HEAD)
# The commits that came from the repository, and one file as the repository had it:
git log --format='%h %an %s' "$MERGE^1..$MERGE^2"
git show "$MERGE^2:$FILE"
```

A file that an update removed from your directory, with no conflict, is got back from the recovery ref that the update made, which
[the deletion guard](deletion-guard.md#getting-files-back) shows how to read.

## A branch that was rewritten under a Treebind that only pulls

A `pull` Treebind has no commits of its own, so it has no policy: when the branch was force-pushed, and the checkout is at a commit
that is not on it any more, the `Pull` check says `Diverged` and nothing is written ([pulling](pull.md#when-it-does-not-work)).
`Synced` is `False` with `Diverged`, `Ready` too, and `Conflict` says `NoConflict`, because there is no conflict to resolve. The
agent asks again at every interval and finds the same. Nothing is lost: the files are as they were.

The agent does not move a checkout onto a history that does not contain it. If the files of the branch as it is now are what you want
on the volume, you do, in the same pod as above. This writes the files of the branch over the files of your directory, those with an
edit that is not committed too, and removes the files that the old commit had and the branch has not, so it is for a directory that
only the repository writes. It keeps the old commit first:

<!-- tested: make-the-volume-the-branch -->
```sh
export GIT_DIR="$MOUNT/.treebind/git" GIT_WORK_TREE="$MOUNT/$SUB"
git update-ref "refs/treebind/recovery/by-hand-$(date +%Y%m%d-%H%M%S)" HEAD
git reset --hard "refs/remotes/origin/$BRANCH"
```

The next pull finds the checkout at the tip of the branch.

## A sync that is stuck and says nothing alarming

The report calls none of these an error. Read the checks in the order of the report, which is the order of the steps: a check that
cannot be tried is `Skipped` with the reason `NotChecked` and says what it waits for, so the first one that is not `Passed` is the one
to look at.

| Check | What it says | What it means |
| :-- | :-- | :-- |
| `Lock` | `Skipped`: `waiting: the lock is being asked for, and another agent may hold it` | [Another agent holds the lock](#another-agent-holds-the-lock). `Checkout` says `not tried, because the lock is not held yet` |
| `Checkout` | `Failed` with a reason, or `Skipped` | [The checkout](checkout.md) |
| `Pull` | `Skipped`: `waiting: the first pull is due in ...` | The first pull comes within one interval of the checkout, and not at once ([pulling](pull.md#things-to-know)) |
| `Pull` | `Failed` | [A conflict that was not resolved](#a-conflict-that-was-not-resolved), and the table of [pulling](pull.md#when-it-does-not-work) |
| `Changes`, `Commit` | `Passed`, or `Failed` | [My change has not reached the repository](#my-change-has-not-reached-the-repository) |
| `Push` | `Passed` with `nothing to push`, or `Failed` | [A push that the host refused](#a-push-that-the-host-refused) |
| `Mount`, `Writable`, `GitDir` | `Failed` | The volume or the Git directory: [pod security](pod-security.md), [the Git directory](git-directory.md) |

### `SyncPending` after a restart

The agent keeps what it saw in memory, so after every start of the sync pod `Synced` and `Conflict` say `Unknown` with `SyncPending`
until a pull or a push has found the checkout and the branch in agreement, which takes up to one interval after the checkout. It is no
failure and it needs nothing. If it lasts longer than that, a check before the first pull has not passed: the table above.

### Another agent holds the lock

Only one agent writes to the checkout of a volume ([ADR 0023](adr/0023-the-agent-holds-the-writer-lock-from-before-the-checkout.md)).
The agent keeps its Git directory and its lock at the root of the claim, whatever `spec.target.subPath` is, so a second Treebind on
the same claim waits for the first for ever: it writes nothing, `Lock` says `waiting`, its pod is not ready (`Ready` `False` with
`SyncPodNotReady`), `Synced` says `SyncPending`, and nothing in the report is an error. To find them:

<!-- tested: find-treebinds-that-share-a-claim -->
```sh
kubectl get treebind -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.target.claimName}{"\t"}{.spec.target.subPath}{"\n"}{end}'
```

Two lines with the same namespace and the same claim are the two. A pod that was replaced gives the lock up when it stops, so its
replacement takes it at once, and one that was lost with its node is waited for 30 seconds. A lock that another agent took from this one
stops this one with the exit code 3, and the line `the writer lock was lost to another agent` is in the log of the container before
the restart (`--previous`); its restart waits for the lock again. A pod that restarts again and again with that line has a second agent
on its volume. `Refused`, `CommitFailed` and `PushFailed` say so too, when the lock could not be checked. An agent whose heartbeat the
volume refused for 30 seconds, as a full one does, stops with the exit code 3 as well, because another agent may take the lock then,
and its line says instead that `the heartbeat of the writer lock could not be written`, with the error of the volume: make room on
the claim.

### An update that was cut off

When the agent is killed in the middle of the files of an update, as by an out-of-memory kill or a lost node, some files are written
and the rest are not. The agent writes the update it begins in a journal, so that the next pull puts back the files that are its own and
applies the update again, and the `Pull` check says so (`the update from ... to ... was cut off, and was undone, and the next pull
applies it again`). A file that is yours is left, and the pull is `Blocked` by it and names it: revert it or remove it, and the next
pull goes on ([the Git directory](git-directory.md#when-an-update-was-cut-off-in-the-middle-of-its-files)).

Until a pull has put the update right, a `bidirectional` Treebind commits nothing: the files that the update wrote look like changes
to the scan, and some may be half written, so they are not taken for yours. The `Commit` check is `Skipped` with `NotChecked` and says
that it waits for the update to be put right, which is what the `Commit` line of the report shows after a kill in the middle of an
update. The first pull after the restart comes at once, and the files that are still changes after it, which are yours, are committed
then. A pull that cannot put the update right is a failed `Pull` check that says why, and the commits wait for one that can. The
[Git directory](git-directory.md#when-an-update-was-cut-off-in-the-middle-of-its-files) has the rest, with the two limits of that wait.

### My change has not reached the repository

`Synced` follows the steps and not the files, so a change that has not been pushed is not a failure. A change goes through three
checks, and the first that does not say what you expect is where it waits:

1. **`Changes`** says how many files changed, how many of them have settled (they are quiet for `spec.sync.settleTime`, 5 seconds by
   default), and what was left out: for example `left out: Secret 1`. A file that is a secret by its name, one that `spec.paths`
   leaves out and one in a repository of its own is never committed. A file that a `.gitignore` hides is not listed at all. `no
   local changes` says that your files are as the last commit has them. A `pull` Treebind never looks ([local changes](local-changes.md)).
2. **`Commit`** says `committed N files as <id>`, or `TooManyCommits` when the limit of the hour is reached ([echo loops](echo-loops.md)),
   or `CommitFailed` with why, which [local changes](local-changes.md#the-check-commit) explains.
3. **`Push`** says `pushed N commits`, or `nothing to push`, or one of the reasons of [a push that the host refused](#a-push-that-the-host-refused).

To make the agent try again now and not at the next interval, delete its pod: the StatefulSet makes it again, and a start looks for
commits to push at its first scan, which is as soon as the checkout is made. What the agent kept in memory is lost with it, which
`SyncPending` and `NoConflict` above say. The files and the commits are on the volume.

```
kubectl delete pod <name>-sync-0
```

## Noticing that nothing has synced for a while

With Prometheus, alert on the time that the operator last saw the Treebind in sync, against its interval. The operator exports it
for every Treebind, and this fires when that time is older than five intervals, and never sooner than 10 minutes:

<!-- tested: promql-not-in-sync -->
```promql
time() - treebind_in_sync_last_seen_timestamp_seconds
  > clamp_min(5 * treebind_sync_interval_seconds, 600)
```

It fires for a step that keeps failing, for a sync that stays pending, as with a second agent on the claim, for an agent that the
operator cannot read and for a sync pod that does not run, and not for a Treebind that has nothing to sync.
[Metrics and events](metrics-and-events.md#alerts) has the metrics, the other alerts (a conflict that waits, a push that the host
refused) and a `PrometheusRule` to adapt, and says how a new leader of the operator takes the time after a failover.

Without Prometheus, the conditions say it. `Synced` is not `True` for as long as a step fails, and the time of its last change of
status is when it began:

<!-- tested: since-when-synced-says-what-it-says -->
```sh
kubectl get treebind <name> -o jsonpath='{.status.conditions[?(@.type=="Synced")].status}{"\t"}{.status.conditions[?(@.type=="Synced")].reason}{"\t"}{.status.conditions[?(@.type=="Synced")].lastTransitionTime}{"\t"}{.spec.sync.interval}{"\n"}'
```

<!-- output: since-when-synced-says-what-it-says -->
```
False	PushRejected	2026-11-20T09:15:00Z	30s
```

Compare the time with the interval: a `Synced` that has been `False` for many intervals is a sync that has not gone on. The time moves
when the status changes, `True`, `False` or `Unknown`, and not when the reason changes, so a restart, which makes `Synced` `Unknown`
until the first pull, moves it. To say it in minutes, with GNU `date`:

<!-- tested: how-long-synced-has-said-it -->
```sh
since=$(kubectl get treebind <name> -o jsonpath='{.status.conditions[?(@.type=="Synced")].lastTransitionTime}')
echo "Synced has said what it says for $(( ($(date -u +%s) - $(date -u -d "$since" +%s)) / 60 )) minutes"
```

For every Treebind of the cluster, one line each, so that you can look at the ones that are not `True`:

<!-- tested: synced-of-every-treebind -->
```sh
kubectl get treebind -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Synced")].status}{"\t"}{.status.conditions[?(@.type=="Synced")].reason}{"\t"}{.status.conditions[?(@.type=="Synced")].lastTransitionTime}{"\n"}{end}'
```

**An old `status.lastSyncTime` is not the sign.** It moves only when the commit at which the two agree changes, and a pull that finds
nothing new changes neither, so a Treebind with nothing to sync keeps its time and is well, and so does the metric
`treebind_last_synced_commit_timestamp_seconds`, which is the same time. Use it to say when the last change came through, and
`Synced`, or the time last seen in sync, to say whether the sync is going on. A stuck agent is restarted by its liveness probe, which
[the status page](agent-status.md#the-probes) describes, and shows as restarts of the pod.

The events of a Treebind say when each change came, for an hour on most clusters: `kubectl events --for treebind/<name>`, as
[above](#start-with-what-kubectl-shows). A `Warning` with `PushRejected`, or a `Conflict is True` with the reason of the update, is
where it began.

## What is not there yet

- **The host's message in a condition or an event.** The conditions carry fixed sentences, by decision
  ([ADR 0037](adr/0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md)),
  and the events carry the conditions' sentences, so the words of the host are in the `Push` check, which the log and the report
  have, and a `PushRejected` is the reason of `Synced` and of its event, not a condition of its own.
- **A count of every refused push.** The metrics count the changes of `Synced`, so a push that the host refuses at every interval
  is one `PushRejected` until a push goes through ([metrics and events](metrics-and-events.md#metrics)).
- **A side branch for the commits of a push that was refused.** Those commits wait on the branch of the checkout. The commits that a
  policy leaves out are kept, under `refs/treebind/local/<number>`, which is [above](#where-each-policy-leaves-your-version).
- **A way to pause the agent, or to make it try now.** There is no setting for either. Deleting its pod makes it try at once, as
  [above](#my-change-has-not-reached-the-repository) says.
- **Commands for a Treebind with `spec.repository.path`.** They were tried on a Treebind that syncs the whole repository.
