# What the agent does with your application's edits: finding them, committing them and pushing them

For a Treebind whose `spec.sync.direction` is `bidirectional`, the agent looks at your directory for what the
application changed since the commit the checkout is at, waits until the changes are done, commits them, and pushes
the commits to the branch that it follows. It never forces. A Treebind that only pulls (`direction: pull`) never
looks, never commits and never pushes. The decisions behind it are in
[ADR 0029](adr/0029-the-agent-looks-for-local-changes-with-a-scan-and-events-only-make-it-scan-sooner.md) for
finding the changes and in
[ADR 0031](adr/0031-the-agent-commits-the-files-that-have-settled-as-literal-paths-in-one-commit-by-one-author.md)
for the commit and
[ADR 0033](adr/0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md)
for the push.

The result is three checks in the agent's report, `Changes`, `Commit` and `Push`, and a line in its log when what one says
changes:

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

## It commits, as one author, with the message you choose

When a file has settled, the agent commits it. Every commit is by one author, who is also the committer, so that a
commit of the agent is easy to tell from one by a person. A scan that finds several settled files makes one commit
of them, and a file that settles at another scan is in another commit.

```yaml
spec:
  commit:
    authorName: treebind-bot
    authorEmail: bot@example.org
    messageTemplate: "treebind: sync {{.Count}} file(s)"
```

- **The author** is `treebind-bot <treebind-bot@treebind.invalid>` when the spec says none. A name and an email
  are set together, or not at all. Git drops a newline, `<` and `>` from a name, and a space, a control character
  and any of `. , : ; " ' \` from its ends, so a name that has one of them is refused when you write the
  Treebind: the commits would say another name than yours.
- **The message** is the template, which is a Go `text/template` with `{{.Count}}` for the number of files in the
  commit, and `treebind: sync {{.Count}} file(s)` when the spec says none. Only the built-in functions are there,
  so a template cannot run a program or read a file. The blank lines inside the message are kept, and the first
  line is the subject. A template can be at most 4096 characters, and what it renders at most 64 KiB.
- **A `$` in a name or a template** is what you wrote. The operator takes care of the way the kubelet rewrites
  `$(NAME)` and `$$` in an environment variable.
- **A name or a template that is wrong** and that the API server could not tell, such as a template that names a
  field that is not there, is a failed check `Commit` with the reason `CommitFailed` and what is wrong, and the
  agent goes on pulling and scanning. Put it right in the spec and the pod is replaced.

## What the checks say

| Outcome | Reason | What it means and what to do |
| :-- | :-- | :-- |
| `Passed` | | The scan worked. The message says `no local changes`, or how many files changed and how many of them have settled, for example `2 files changed on the volume: 1 settled, 1 waiting for their quiet period`. It names what was left out by rule, for example `left out: Secret 1`. It says that the agent does not commit them yet, until it does |
| `Failed` | `ScanFailed` | Git could not list the changes, and the message says why: a merge that was not finished (finish or abort it in the Git directory), so many changes that Git's list does not fit in the agent's limit, or a file that cannot be looked at. The next scan is at the interval |
| `Failed` | `GitDirUnsafe` | The Git directory holds something that the agent did not put there. See [the Git directory](git-directory.md) |
| `Skipped` | `NotChecked` | The checkout is not made yet, a volume check failed, or the first scan is running |

### The check `Commit`

It is after `Changes`, and it says what came of the last commit, until the next one.

| Outcome | Reason | What it means and what to do |
| :-- | :-- | :-- |
| `Passed` | | The files that had settled are committed. The message says how many and the short id of the commit, for example `committed 2 files as 1a2b3c4d5e6f`. It also says when files that had settled were gone before the commit, and which lock files of a stopped Git were removed first |
| `Passed` | | `the files that had settled came to nothing to commit`: they were changed back, or are gone. Nothing was committed, and nothing is wrong |
| `Failed` | `CommitFailed` | The commit could not be made, and the message says why: what Git said, that the writer lock could not be checked, that the volume refused a write, or that the author or the template of the spec cannot be used. The files are still there, and the agent tries again at the next scan. A failure of the first kinds goes away from the report when the scans find no change at all; a wrong author or template stays until you put it right in the spec, and the files that settle are not committed until then |
| `Failed` | `GitDirUnsafe` | The Git directory holds something that the agent did not put there. See [the Git directory](git-directory.md) |
| `Skipped` | `NotChecked` | No file has settled yet, or the checkout is not made, or the scan cannot be set up |

## It pushes, as a fast-forward, and never forces

The agent pushes right after a commit, in the same turn, and never beside a pull. It pushes only the branch that it
follows, to the branch of the same name, and only as a fast-forward: when the repository has commits that the checkout
lacks, or when the host refuses the push, the agent reports it and does not work around it. It does not force, delete,
push tags or write the Git config, and the Runner refuses a push that tries ([ADR 0033](adr/0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md)).
It tries when commits may wait: at the start, for commits that an earlier run left, after a commit, and again after a try
that did not work.

The first push that works makes the Treebind ready, like the first pull that passes does, whichever comes first, so a
Treebind on an empty repository is ready once its first commit is pushed.

### The check `Push`

It is after `Commit`, and it says what came of the last push, until the next one.

| Outcome | Reason | What it means and what to do |
| :-- | :-- | :-- |
| `Passed` | | The commits are in the repository. The message says how many and the two ids, for example `pushed 2 commits to the branch main: 1a2b3c4d5e6f..9f8e7d6c5b4a`, or that the branch is new |
| `Passed` | | `nothing to push`: the repository has every commit of the checkout. Nothing is wrong |
| `Failed` | `NonFastForward` | The repository has commits on the branch that the checkout lacks, so the push is not a fast-forward and is not forced. A pull brings them and says `Diverged` when the checkout has commits of its own. Put it right as [pulling](pull.md) says; the agent tries again after the interval |
| `Failed` | `PushRejected` | The repository refused the push: a protected branch, a hook, a rule, a file that is too large, a secret that the host found. The message has what Git said and then the words of the host. Put right what it says, for example allow the deploy key to push to the branch; the agent tries again after the interval and does not force or push to another branch |
| `Failed` | `PushFailed` | The push could not be made: the repository did not answer, the key was refused, the volume refused, or the writer lock could not be checked. The message says why. The agent tries again after a wait that grows to ten minutes |
| `Failed` | `GitDirUnsafe` | The Git directory holds something that the agent did not put there. See [the Git directory](git-directory.md) |
| `Skipped` | `NotChecked` | The checkout is not made yet, or the first scan is running |

Until the conflict policies exist (the next change), a `NonFastForward` is for a person to put right.

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

It does not resolve conflicts, or push to another branch, in the way that a conflict policy will (the next change), and it
does not scan for secrets in the files it commits, apart from the names above. A scan interval that follows what the volume can do, and a way to set
it, come with the volume probe.
