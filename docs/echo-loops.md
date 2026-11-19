# Echo loops: when what the agent writes comes back as something new

A `bidirectional` Treebind commits what the application writes and pushes it, and pulls what others pushed. A loop is when
that never settles: every commit that goes out brings something back that makes another commit. The decision is
[ADR 0035](adr/0035-the-agent-compares-trees-before-it-resolves-and-counts-its-commits-against-a-limit-of-an-hour.md). The
agent was tried against the loops below on real repositories, with Git 2.34.1 and with the Git of the agent image, 2.56.1.

## What is not a loop

- **The files that a pull writes are not your application's changes.** Whatever the repository's attributes say about line
  endings, and whatever touches the times of the files, the scan lists only what the application changed. The agent has no
  guard that "ignores its own writes", because there is nothing to ignore, and a guard by size and time would also hide a write
  of the application right after a pull.
- **A commit that the agent pushed is not new when it is pulled.** The next pull says `UpToDate`.
- **Two Treebinds that edit and resolve conflicts settle.** With any pair of the two conflict policies, and with edits, commits,
  pushes and pulls in any order, they end at one tree and then do nothing.

## What is a loop, and what the agent does

**Your application answers what a pull brought.** An application that rewrites a file when another file changes, and another
Treebind whose application does the same the other way round, make a commit, a push and a pull at every turn. The agent cannot
tell this from a person's edit, so it counts: at most `spec.commit.maxPerHour` commits in any hour, the commits of the files that
the application changed and the commits of resolutions together. The default is 60.

```yaml
spec:
  commit:
    maxPerHour: 60   # from 1 to 10000
```

At the limit the agent makes no commit until one of the commits of the hour is an hour old. The changes stay on the volume, and
are committed then. The agent goes on pulling, scanning and pushing what it has, and stays ready. It stops nothing for good, so a
loop that goes on is committed at that rate, and the check says so.

| Check | Outcome | Reason | What it means and what to do |
| :-- | :-- | :-- | :-- |
| `Commit` | `Failed` | `TooManyCommits` | The files that settled are not committed because the limit of the hour is reached. The message has the count and the limit. Look at what writes the files, and at what answers it. Raise `spec.commit.maxPerHour` only if the writes are expected |
| `Pull` | `Failed` | `TooManyCommits` | A conflict would need a commit and the limit is reached, so it was not resolved and nothing was written: not even the ref that keeps the commits of your directory. The pull tries again at every interval, with no longer wait. A conflict that needs no commit is not held, except a `followRemote` one whose every changed file cannot be put on the tip, which is only known after the agent has started to write |

The count is in the file `commit-times` of the `.treebind` directory on the claim, one time in seconds on each line, so it
survives a restart of the pod. Delete the file to start the count again. A file that is larger than the agent writes makes the commits fail with that cause, and so does an agent that cannot make its breaker. A kill of the agent between a commit and the write of
the file leaves that commit uncounted, so the limit can be one over for each kill.

**A host gives back what it is sent as another commit.** A host that squashes or rebases what is pushed makes your directory and
the branch each have a commit that the other lacks, with the same files. The agent moves to the branch's tip and makes no commit,
with either policy, so nothing is pushed and nothing comes back. The commits of your directory are kept under
`refs/treebind/local/<number>`, as [conflicts](conflicts.md) says. If the host also changes the files at every push, the limit
above is what ends it.

## A commit that a CI should not run for

Put the marker that your CI reads in the commit message. There is no setting for it: the message template is yours.

```yaml
spec:
  commit:
    messageTemplate: "treebind: sync {{.Count}} file(s) [skip ci]"
```

`[skip ci]` is read by GitHub Actions and GitLab, among others. A CI that pushes a commit back after each push of the agent is a
loop of its own, and the marker, or a rule of the CI that skips the author `treebind-bot`, is what ends it.

## What was not tried

Volumes that fold case, keep no mode bits or have a coarse modification time, such as SMB, vfat and some NFS exports: only a
local file system was at hand. A host that rewrites commits was modelled with a hook and not seen on a real host.
