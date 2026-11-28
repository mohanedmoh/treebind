# Where the agent keeps its Git directory

The sync agent works in a Git checkout on your claim. The Git directory of that checkout
holds the repository's config and hooks, and Git runs what they say with the agent's
credentials. So it must not be somewhere the application can write. The reasons are in
[ADR 0016](adr/0016-the-git-directory-lives-on-the-claim-beside-the-synced-directory.md).

The agent mounts the whole claim and keeps its own state in the directory `.treebind` at
the root of the claim, the Git directory in `.treebind/git`. It syncs the directory named
by `spec.target.subPath`, or the whole claim when there is none.

## Use a `subPath`, and mount only that

```yaml
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: dashboards
spec:
  repository:
    url: ssh://git@example.com/team/dashboards.git
    branch: main
  target:
    claimName: grafana-data
    subPath: dashboards
  sync:
    direction: bidirectional
```

With a `subPath`, `.treebind` is beside the synced directory. Mount the claim in your
application with the same `subPath`, and the application sees the synced files and
nothing else:

```yaml
containers:
  - name: grafana
    volumeMounts:
      - name: data
        mountPath: /var/lib/grafana/dashboards
        subPath: dashboards
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: grafana-data
```

Nothing in the cluster can make an application mount only the `subPath`: the mount is in
your own pod. If it mounts the whole claim, it can see `.treebind`.

## If a `subPath` mount is not an option where you run

Treebind's own sync pod never uses a `subPath` mount: it mounts the whole claim, and
`target.subPath` is only a directory that the agent works in. The `subPath` mount is the one
in *your* application, and it is a bind mount that the kubelet makes. Some platforms advise
against such mounts, for example where the kubelet runs in a container and can be restarted,
or where the volume under a running pod can be remounted. If that holds where you run, there
are two other ways, and neither needs a change in Treebind:

- **A volume whose root already is the directory.** Give the application a claim to a volume
  that starts at the synced directory, for example a static PersistentVolume for NFS whose
  `path` is that directory. The application then mounts that claim whole and cannot see
  `.treebind`. The agent keeps using the main claim with a `subPath`. It takes static
  provisioning, which Treebind does not do for you.
- **No `subPath` at all.** The protection below applies. It is weaker, and the agent works.

## Without a `subPath`

The whole claim is synced, so `.treebind` is inside what the application mounts. That works,
with less protection: the directory is closed to other users (mode 0700, owned by the agent),
and it is never committed. An application that runs as the same user as the agent, or as
root, can still read and write it. The agent looks at what is in `.treebind/git` before every
Git command, and refuses to start Git when it finds a config key, a hook or another file that it
did not put there (see below), so what such an application plants there stops the sync instead
of being run. A change made between that look and the start of Git is not ruled out: the look
narrows the gap for such an application and does not close it. A `subPath` cannot be `.treebind`
or start with it.

## What the agent checks

At start the agent makes `.treebind` and `.treebind/git` with mode 0700 and checks them. It
refuses a directory that is a link, is not a directory, belongs to another user than the one the
agent runs as, or is open to group or others. It refuses a link on the way to the synced
directory too, because a link there could point the work tree into `.treebind`. It follows no
link in `.treebind` and `.treebind/git`, and refuses one of them that was swapped for a link, or
for another directory, while it was being checked. The way to the synced directory is looked at
once, at start.

The agent also looks at what is in `.treebind/git`: at start, after the directories, and again
before every Git command it runs, so that a file that turns up later is found too. It takes only
what it puts there itself, because some of what a Git directory can hold makes Git run a program
or read from somewhere else, and the agent cannot switch all of it off from the command line:

- **The config** may hold only the keys that `git init` writes, in the sections `core` and
  `extensions`, with the synced directory as the work tree. Any other key or section is refused,
  among them `url.<base>.insteadOf`, a key for one URL such as `http.<url>.sslVerify`,
  `http.sslCAPath`, `core.sshCommand`, a filter and an include.
- **`hooks` and `info`** have to be empty, so a hook, `info/attributes` or `info/exclude` is refused.
- **`objects/info/alternates` and `objects/info/http-alternates`** have to be absent, so that no
  object is read from another place.
- **At the top of `.treebind/git`** only the files and directories that Git writes for the commands
  the agent runs are taken, and the lock file that such a command leaves when it is killed. Anything
  else, such as `commondir`, `packed-refs` or a file of your own, is refused, and so is a link.
- **Replace refs** are not looked at but switched off for every command, so one in the refs
  changes nothing that Git sees.

A directory that fails is reported by the check `GitDir` with the reason `GitDirUnsafe`, and Git
is not run against it. The message names the directory, or the file in `.treebind/git` and what
is wrong with it, with the line for the config. The agent keeps running and checks again, with a
longer wait each time up to a minute, so it carries on once the directory is put right. A Git
directory that the agent cannot look at, because it is not readable by the agent, is refused the same
way: what it cannot look at it cannot tell is its own. A claim
that cannot be written to fails the check with `ReadOnlyVolume`, `NotWritable` or `WriteFailed`,
as the check `Writable` does. The result is in the agent's log, once for each change, and in its
status report, which [the status page](agent-status.md) shows how to read by hand. The operator
shows a failed check in the condition `Synced`, `False` with the reason of the check
([the conditions](agent-status.md#synced)). A `.treebind` with mode `2770` and `open to group or
others` in the message is most often the walk of an `fsGroup`:
[what the volume needs](pod-security.md#what-the-volume-needs) says how to stop it and how to repair it.

The checkout looks as well, before it starts and before each command. When it finds such a file
it stops with the same reason, `GitDirUnsafe`, and the name of the file, and runs no more Git. It
leaves `.treebind/git` as it is, so that you can see what is there, and when the file was there
before it began, it changes nothing in the synced directory either.

To put it right, remove `.treebind` or `.treebind/git` when the message names one of them, or
make it a directory of the user the agent runs as with mode 0700. When the message names a link
or a file on the way to the synced directory, replace it with a real directory. The agent does
not change a directory that is not its own.

When the message names something inside `.treebind/git`, find out how it got there: something
other than the agent wrote to the directory. Then remove the file it names, or the line of the
config. The agent checks again, carries on, and reopens the checkout as it was. You can instead
empty `.treebind/git`. The agent then makes it again and makes the checkout as on a first start,
which takes the files in the synced directory when they match the branch and refuses them with
`DirectoryNotEmpty` when they do not. The commits that were not pushed are lost with it.

## When Git was stopped in the middle of a write

While Git writes it holds a lock file in `.treebind/git`, such as `index.lock` or `refs/heads/main.lock`, and
removes it when it is done. A Git that is killed cannot, and every Git command after it fails with `Unable to
create '...lock': File exists` until the file is gone.

The agent stops Git gently. It tells a command to end first, and Git then removes its own lock files. It kills it
only when it has not ended after 3 seconds. A stop of the pod does not cut off an update that is writing its
files either: the update has up to 10 seconds to finish. What still kills Git is an out-of-memory kill, a lost
node, and a pod that is killed when its termination grace period ends.

Before it makes the checkout and before every pull, the agent looks for lock files that a stopped Git left. When
it finds some, it checks that it still holds the writer lock, removes them, and says which in the check
`Checkout` or `Pull` and in its log:

```
removed the lock file that a Git command that was stopped left: index.lock
```

It removes only a regular file named like the lock of a file that its Git commands write at the top of
`.treebind/git` (`index.lock`, `HEAD.lock`, `ORIG_HEAD.lock`, `config.lock`, `FETCH_HEAD.lock`, `packed-refs.lock`)
and the files that end in `.lock` under `refs`. It follows no link and leaves every other file. This is safe
because one agent at a time writes to the checkout and it runs one Git command at a time, so a lock file that is
there when none of its commands runs was left by one that ended. Do not run Git by hand in the sync pod while the
agent works: the agent can remove the lock file that your command holds, and your command then fails. The reasons
are in [ADR 0028](adr/0028-the-agent-stops-git-gently-and-removes-the-lock-files-that-a-stopped-git-leaves.md).

## When an update was cut off in the middle of its files

A Git that is killed while it applies an update leaves some of its files written. Before it applies an update, the
agent writes a small file, `.treebind/updating`, with the commit the checkout is at and the commit the update goes
to, and removes it when the update has been applied. A pull that finds it left puts the checkout back as it was before
the update began, and then applies the update again, with every check a pull has. The `Pull` check says what it did:

```
the update from 0123456789ab to fedcba987654 was cut off, and was undone, and the next pull applies it again: 2 file(s) put back, 1 removed
```

It puts back a file that the update changed when the file is not there, or is the whole of the new file, or the start of
it, which is what a write that was cut off leaves. It removes a file that the update added in the same cases. A file
that is as it was is left. **A file that is anything else is yours and is left**, and the next pull is `Blocked` by it and
names it, as for any local edit: revert it or remove it, and the next pull goes on. A file that the update deletes is left
as it is. [ADR 0028](adr/0028-the-agent-stops-git-gently-and-removes-the-lock-files-that-a-stopped-git-leaves.md) has the
rule for each state, the links, and the files too large to compare.

Until a pull has put the update right, a `bidirectional` Treebind commits nothing. The files that the update wrote look
like changes to the scan, and some may be half written, so they are not taken for yours: the check `Commit` is `Skipped`
with `NotChecked` and says that it waits for the update to be put right, and the files that are still changes after the
pull, which are yours, are committed then. The first pull after a start that finds `.treebind/updating` comes at once, and
not at the point of the interval that spreads the pulls, and it puts the update right before it asks the repository
anything, so the wait is short even when the repository does not answer. A pull that cannot put it right is a failed `Pull`
check that says why, and the commits wait for one that can.

An update that fails in the middle of its files while the agent runs, as on a full volume or a timeout, leaves the journal
too. The agent puts that right at once, when the pull that failed has ended, and does not leave it to the next pull, so the
files that the application writes are committed at the next scan. The `Pull` check, which is failed for the update, says what
was undone. Those commits are on the commit that the checkout is at, which the update has not moved, so the repository refuses
a push of them as not a fast-forward until the next pull. It comes after the wait of a pull that failed, applies the update and
joins it with them as it does for any local commit, and the `Conflict` check says `ConflictResolved` when it had to resolve one.

If the update cannot be put right, the `Pull` check says why and the commits wait until it can be. A journal that the state
directory refuses to remove is one such cause, and the message names it: look at `.treebind/updating`. A pull that applied its
update and then could not remove the journal passes, with a warning, and the repair that the next pull begins with fails for
the same cause.

If the index cannot be read, the agent makes it again from the commit the checkout is at before a pull, and says so. It
does the same reset when the index holds files that are staged, which nothing here leaves for long. Your files are not
touched: an edit stays an edit, as a change that is not staged.

## Room on the claim

The Git directory holds the history of the branch, so the claim needs room for it besides
the files. Every commit, fetch and resolved conflict adds objects to it, first each as a file of its own, which takes at least
a block of the volume and an inode. The agent packs them: after a pull, when there are about 6,700 loose objects or 50 packs,
the limits that Git has for its own garbage collection, it runs `git repack`, which puts them in a pack and removes the files.
It removes no object that is not in the pack it has just written, and no ref, so the commits that the agent keeps for you under
`refs/treebind/recovery` and `refs/treebind/local`, and under a ref you made by hand, stay. It does not run `git gc`, and you
should not either (see [the deletion guard](deletion-guard.md#things-to-know)).

How much room the Git directory needs, measured with the defaults (`sync.interval` 30 seconds, `sync.settleTime` 5 seconds,
`commit.maxPerHour` 60) for an application that saves files of about 6 KB every few seconds, which makes a commit a minute,
while another Treebind changes the same repository every other minute, on ext4:

| | After a day | After a week | After two weeks |
| :-- | --: | --: | --: |
| The Git directory, packed by the agent | 25 MiB, 5,500 files | 25 MiB, 750 files; between 18 and 51 MiB in the days around it | at most about 75 MiB, extrapolated from 9 days (45 MiB at day 9) |
| Without the packing (releases before it), for comparison | 53 MiB, 13,000 files | 372 MiB, 90,600 files | about 745 MiB, extrapolated |

So give the claim, besides the files and the history of the repository as you have it, about 30 MiB for the objects that wait to
be packed, and about 20 MiB a week, about 3 MiB a day, at that rate of changes: what the history grows by, which the repository on
the host grows by too, and the logs of the refs. A Treebind that commits more, with a higher `commit.maxPerHour`, needs that much
more for each week. The numbers and how they were measured are in
[ADR 0043](adr/0043-the-agent-packs-the-objects-of-its-git-directory-with-a-repack-under-the-writer-lock.md).

The repack writes the new pack beside what it packs and then removes that, so it needs free room on the claim for the size of the
files of the loose objects, and for every pack in one the size of the packs too. When the claim does not have it, the agent
packs nothing, and the `Pull` check says how much it needs and how much the claim has:

```
the Git directory was not packed: it has 90535 loose objects and 1 pack, and the repack needs 70.6 MiB free on the volume and the volume has 12.0 MiB; give the claim more room
```

It looks again an hour later, and the note stays in the `Pull` check until a repack is made. Nothing is packed while the pulls
fail, as when the host does not answer: the commits go on, and their objects, about 53 MiB and 13,000 files a day at the rate
above, wait for the first pull that passes. An agent of a release before the packing has all its objects loose, and its first repack needs about
a fifth of what they take: give the claim that room before you upgrade. A repack that is killed, as when the sync container is
killed for its memory, leaves the objects as they were and is put right by the next, which waits a day after the kill, and the
`Pull` check says so. While a repack runs, the agent does not pull, scan, commit or push; that is seconds for the loose objects.

A fetch that has no room on the claim fails, and the `Pull` check says `PullFailed` with what Git said, such as
`No space left on device`. Git leaves the temporary files of the objects that it was writing in `.treebind/git/objects`,
which keep the claim full, and so does a Git that is killed in the middle of a fetch. After a pull that the volume
refused, the agent removes them and names them in the check:

```
removed the temporary object file that a Git command that failed left: objects/ab/tmp_obj_Xy12Ab
```

It checks the writer lock first, as before it removes a lock file, and leaves them when the lock is lost; a volume
that refuses the heartbeat of the lock, because it is full, does not stop it. When the agent starts, before the volume
checks, it removes the ones that have not changed for 31 minutes, longer than a Git command of an agent may run, and
says so in its log: it does not hold the writer lock yet then, and another agent may be writing the newer ones. It
removes only a regular file whose name is one that `git prune` takes for a temporary file, `tmp_obj_*` in the
directories of loose objects and `tmp_*` at the top of `objects` and in `objects/pack`, and what a repack or a fetch that was
killed leaves in `objects/pack`: a `.tmp-*` file, and the `.pack`, `.rev`, `.bitmap` or `.mtimes` of a pack that has no `.idx`,
which Git does not read. It follows no link, and leaves a `.keep` and a `.promisor`. It does the same behind the writer lock
before each repack, and after one that failed. The
objects that a fetch wrote in full stay: free room on the claim, and the next pull fetches the rest.
