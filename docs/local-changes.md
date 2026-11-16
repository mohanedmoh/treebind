# What the agent does with your application's edits: looking for them

For a Treebind whose `spec.sync.direction` is `bidirectional`, the agent looks at your directory for what the
application changed since the commit the checkout is at. It does not commit or push what it finds yet: that is
the next change. What it does now is find the changes, wait until they are done, and say in its report what it
found. A Treebind that only pulls (`direction: pull`) never looks. The decisions behind it are in
[ADR 0029](adr/0029-the-agent-looks-for-local-changes-with-a-scan-and-events-only-make-it-scan-sooner.md).

The result is the check `Changes` in the agent's report, and a line in its log when what it says changes:

```
kubectl logs <name>-sync-0
kubectl describe treebind <name>
```

## What it does

- **It scans.** A scan is one read of your directory by Git, which lists the files that differ from the commit.
  The first scan is as soon as the checkout is made, and the next is 30 seconds after the end of the one before.
  A scan changes nothing: it does not touch the index or take a lock, so a scan cannot leave a stale lock behind.
- **It listens, to scan sooner.** The agent watches your directories for events of the file system and scans a
  second after one. An event never says what changed, and a missed event costs up to 30 seconds and never a
  change. On network file systems such as NFS and CephFS, writes by another client make no event at all, and the
  scan every 30 seconds is the only thing that finds them.
- **It waits until a file is done.** A file settles when it has been the same for `spec.sync.settleTime`, 5 seconds
  when it is not set. A file that is still being written, or that is removed and written again as an editor
  saves, is not done. Each file settles on its own, so a log that is written all the time does not hold back the
  other files.
- **It leaves some files out, whatever the spec says.** The files of `.git`, a repository inside yours, the
  agent's own `.treebind` directory, editor swap and lock files, and files that are secrets by their names:
  `.env` (not `.env.example`, `.env.sample` or `.env.template`), `.netrc`, `.pgpass`, `.git-credentials`, private
  ssh keys, and names that end with `.key`, `.p12`, `.pfx`, `.jks` or `.keystore`. Then `spec.paths.include`
  and `spec.paths.exclude`, and the `.gitignore` files of your directory, which hide the files that are not in
  the repository. See [ADR 0022](adr/0022-the-agent-decides-what-it-syncs-with-gitignore-patterns-of-its-own.md).

## What the check says

| Outcome | Reason | What it means and what to do |
| :-- | :-- | :-- |
| `Passed` | | The scan worked. The message says `no local changes`, or how many files changed and how many of them have settled, for example `2 files changed on the volume: 1 settled, 1 waiting for their quiet period`. It names what was left out by rule, for example `left out: Secret 1`. It says that the agent does not commit them yet, until it does |
| `Failed` | `ScanFailed` | Git could not list the changes, and the message says why: a merge that was not finished (finish or abort it in the Git directory), so many changes that Git's list does not fit in the agent's limit, or a file that cannot be looked at. The next scan is at the interval |
| `Failed` | `GitDirUnsafe` | The Git directory holds something that the agent did not put there. See [the Git directory](git-directory.md) |
| `Skipped` | `NotChecked` | The checkout is not made yet, a volume check failed, or the first scan is running |

The message of a scan that worked also says when the events cannot do all of their work:

- `no events of the file system`: the agent could not set up its watcher, for example because the node has used
  all of its inotify instances (`fs.inotify.max_user_instances`), and the scan every 30 seconds finds the
  changes.
- `N directories cannot be watched`: usually the node's limit on watches (`fs.inotify.max_user_watches`) is reached,
  which a large tree can do. The scan finds the changes in those directories, up to 30 seconds late. The limit is a
  setting of the node and not of the pod: raise it where the node is configured.
- `events were lost, and a scan followed`: the kernel's queue of events overflowed. Nothing is lost: a scan
  followed.

## What it does not do yet

It does not commit, push or resolve conflicts (the next changes), and it does not scan for secrets in the files it
would commit, apart from the names above. A scan interval that follows what the volume can do, and a way to set
it, come with the volume probe.
