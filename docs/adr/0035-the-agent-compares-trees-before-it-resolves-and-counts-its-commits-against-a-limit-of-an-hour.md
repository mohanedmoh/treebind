# 0035. The agent compares trees before it resolves and counts its commits against a limit of an hour

- Status: accepted
- Date: 2026-11-19
- Issue: #41
- Extends: [0029](0029-the-agent-looks-for-local-changes-with-a-scan-and-events-only-make-it-scan-sooner.md), [0031](0031-the-agent-commits-the-files-that-have-settled-as-literal-paths-in-one-commit-by-one-author.md), [0033](0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md), [0034](0034-the-agent-resolves-a-conflict-by-the-policy-of-the-treebind-one-file-at-a-time-in-the-update-of-a-pull.md)

## Context

R-18 is a pull that changes files and a watcher that commits them back, or a commit that the agent pushed that comes back, to
the same Treebind or to another, as something new. Since the agent commits (ADR 0031), pushes (ADR 0033) and resolves conflicts
(ADR 0034) there are commits that go out and come back, and ADRs 0029, 0031, 0033 and 0034 left the echo to this record.

The issue names five mitigations: ignore the agent's own writes, compare tree hashes before committing, a commit trailer, a
commits-per-hour breaker, and an optional skip-CI marker. Before any of them was designed, the loops were made to happen on real
repositories, with the real pull, scan, commit and push of the agent, a stand-in for ssh, and two Treebinds on one repository, on
Git 2.34.1 and on 2.56.1, the Git of the agent image. Only what happened is designed for.

**Not a loop**, on both Gits, and now held by tests (`internal/agent/loops`):

- A pull's files are not changes of the application: attributes `text`, `eol=crlf`, `eol=lf`, `text=auto`, `ident` and `filter`,
  an executable file, a link, names with spaces, a backslash, a newline and non-ASCII letters, and every file touched to another
  time. A `chmod -x` by the application is a change, a real one.
- The application's write right after a pull, with the same size and in the same second, is still a change: 30 of 30.
- The steps of the agent do not wake its own watcher: a pull that writes files makes one signal, which is one scan that finds
  nothing, and a scan, a commit and a push make none, with the synced directory in a sub path of the claim and with it the claim.
- A commit that was pushed is `UpToDate` at the next pull.
- Two Treebinds whose applications edit at random, with commits, pushes and pulls in any order and the four pairs of the two
  policies, end at one tree on both and on the repository and then do nothing: 60 schedules of 40 steps for each pair on each Git,
  and 3 for each in the suite.

**A loop**:

1. *An application that answers what a pull brought.* Two Treebinds whose applications write `pong.txt` when `ping.txt` changes
   and the other way round make a commit, a push and a pull at every turn for as long as both run. The agent cannot tell this from
   a person's edit.
2. *A host that gives back what it is sent.* A `post-receive` hook that replaces each pushed commit with one that has the same
   tree, on top of the old tip, as a squash queue or a rebase on submit does (modelled, not seen on a host), makes the checkout
   and the repository each have a commit that the other lacks. With `followLocal` the resolution was a merge commit, which was
   pushed and rewritten again, at every pull interval and with no application: 8 passes made 7 resolutions. `followRemote` has
   nothing to carry, and moved to the tip. With a host that also adds a file of its own to each commit the trees are never the
   same, and nothing but a limit ends it.

## Decision

### A resolution of the same tree makes no commit

When the checkout and the repository have the same tree there is nothing for a policy to decide. With either policy the
checkout is moved to the repository's tip, and the commits that it had are kept under `refs/treebind/local/<number>`, as
`followRemote` keeps them. Nothing is pushed, and loop 2 ends at the first resolution. This changes ADR 0034 for one case:
`followLocal` made a merge commit there. The history of the repository is still the history of the branch, and what the volume
had is in the local ref and in the files, which are the same.

### The breaker

The agent makes at most `spec.commit.maxPerHour` commits in any hour, the commits of the commit step and the commits of
resolutions counted together: 60 when it is not set, from 1 to 10000. The times of the commits are in a file of the state
directory, `commit-times`, one in seconds since the epoch on each line. It is not read from the history of the branch: a host
that rewrites what it is sent, and a resolution that moves the branch off the commits of the checkout, take commits out of the
history, and the refs that keep them are 20. A file also survives the restart of the pod, so a restart is not a new budget.

- Asking is a read. The commit step asks after it knows that there is something to commit and before it checks the lock or writes
  anything, so that a commit that is held writes no index, no file and no ref. A resolution asks when it will make a commit, which
  is `followLocal` with different trees and `followRemote` with files to carry, before its first write, the ref for the local
  commits included, which a resolution that is held at every interval would make at each. A resolution that makes no commit is not
  asked, and is not held, with one exception: a `followRemote` resolution whose every file that the volume changed cannot be put
  on the tip (the tip has a directory where a file is, or a file where a directory is) makes no commit either, but that is only
  known once the tree is built, which writes the index, so it is asked before and is held while the limit is reached. A pull that
  advances, is up to date or is ahead is not a resolution.
- Telling that a commit was made is the one write. It is made after the commit is in the branch and the checkout is at it, behind
  the writer-lock fence, to a file that is written aside and renamed, with a context that a stop does not cancel. A commit that
  could not be counted stands, and the check says so.
- The clock is the loop's when it says what time it is, which in the agent is the system's, and a time more than an hour ahead of
  it is dropped, so that a clock that was set back does not hold commits for more than a window.
- At the limit the `Commit` check is `Failed` with the new reason `TooManyCommits`, or the `Pull` check for a resolution, with a
  message that says the limit and that the changes stay on the volume. The message has no time, so that a hold that is the same is
  not a new line of the log. Scans, pulls and pushes go on, the agent stays ready, and the wait before the next pull does not grow:
  the repository answered, and the held resolution asks it nothing that the next pull does not. Nothing is reset by a pull, a push
  or a restart.

### Not built

- *Ignoring the agent's own writes.* There is nothing to ignore. A guard can only go by what the pull left (size and times), and a
  write right after it, on a clock that is coarse, is what stat cannot tell from the pull's own; Git tells it by content.
- *A commit trailer.* No loop here needs it. The Runner refuses `commit --trailer`, so it would be a line of the message that
  writes the namespace and the name of the Treebind into the history of the repository.
- *A field for a skip-CI marker.* A template can say it: `messageTemplate: "treebind: sync {{.Count}} file(s) [skip ci]"`, which a
  test holds. A CI that pushes back after the agent's push is a loop of the host's own.

## Consequences

- Loop 1 and the loops like it go on at `maxPerHour` commits in each hour and not for ever, and the `Commit` check says
  `TooManyCommits` while they do. A person who sees it looks at what writes the files. The agent does not stop for good, because a
  limit that stops for good is a limit that a burst of real writes can set off.
- The limit is exceeded by at most one commit for each kill of the agent between a commit and the write of the file. The file is
  written aside, synced and renamed, and the directory is not synced, so a crash of the node that loses the rename leaves the
  old file, and the commits since are not counted. A restart of the pod does not.
- A file of times that is larger than the agent writes, or cannot be read, makes the commit step and the resolutions fail with the
  cause and not count what fits in the start of it. An agent that cannot make its breaker fails them the same way and does not run
  without a bound.
- A Treebind whose application really writes every minute is at the limit for ever at 60, and raises it.
- A held resolution leaves the checkout and the repository apart: the pull says `TooManyCommits` at every interval, and the push
  says `NonFastForward` without asking the host, until the hour allows it.
- A sync pod of a Treebind that sets `maxPerHour` to another value than 60 is replaced once, when the operator that passes it is
  installed. The API server fills the default in for a Treebind that has the commit section, and the operator does not pass it.
- A Git before 2.41, such as the one of a developer's machine, applies the attributes of the work tree, and a blob with both kinds
  of line ending under `eol=crlf` is listed as a change straight after a pull. The Runner reads the attributes from an empty tree
  (ADR 0013) where Git has `GIT_ATTR_SOURCE`, and the agent image has it, so this is not reachable in the agent. The test of it
  is skipped on an older Git. The variable is in the source of Git from the tag v2.41.0 and not at v2.40.1 (`attr.c` and `git.c`),
  and not in 2.40 as ADR 0013 and `CheckVersion` say; a 2.40 Git was not at hand to try, and the agent image is far above it.
- Not tried here: a volume that folds case, keeps no mode bits or has a coarse modification time (SMB, vfat, some NFS exports),
  where `git init` probes for `core.filemode` and `core.ignorecase`; a host that rewrites commits, which is modelled by a hook; a
  real kill of the agent in the cluster; and the effect on a repository that has a very large history, since the breaker does not
  read it.
- `TooManyCommits` is a new reason, and so is the field: both are API (ADR 0004).
