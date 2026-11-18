# 0031. The agent commits the files that have settled, as literal paths, in one commit by one author

- Status: accepted
- Date: 2026-11-18
- Issue: #36

## Context

FR-3 asks the agent to commit what the application writes after a settle time, and FR-3's author and message are
the Treebind's: a name, an email and a template, so that a commit of the agent is easy to tell from one by a
person. ADR 0029 finds the changes and says when they are done, and leaves them at a hook that nothing implements.
`commitmsg` (#188) renders the message and says which authors Git would change. This record is the commit itself.

Git runs only through the Runner (ADR 0013), which has no standard input. The filter of ADR 0022 has already
decided which files are synced. Every write of the agent is made after the writer lock was checked (ADR 0023),
and a command that was stopped leaves lock files that ADR 0028 removes. The push is #37, conflicts are #38 and
the echo loop is #41, and none of them is here.

## Decision

### What is committed

The files that a scan lists as settled, in one commit for each scan that finds any, for a `bidirectional`
Treebind. The scan has put them through the filter, so the commit stages what the scan listed and nothing
else. A file that settles at another scan is in another commit, so two writes of the application that are
seconds apart may be two commits. Nothing is committed for a `pull` Treebind.

### Staging: literal paths, from a file

Each path is staged as `:(literal)<path>`, and the list is given to `git add --all --pathspec-from-file
--pathspec-file-nul`. A name is not a pattern: `a[1].txt` must not take in `a1.txt`, and a name that starts with
`:(top)` or `-` is a name (tried, on Git 2.34.1 and 2.56.1, with a name with a newline, one with bytes that
are not UTF-8, and names that are patterns). The list is in a file in the state directory, which the scan
leaves out, because the Runner has no standard input and a list of thousands of names does not fit in the
arguments of a command: 9,000 names of 230 characters, about two megabytes, fail as arguments and commit from a
file. Deletions, changes of mode and a file that became a link are staged by the same call.

A path that was added and is gone by the time of the commit is left out, and counted, because `git add` refuses
the whole list for one path that matches nothing ("pathspec did not match any files", tried). A path that the
commit has is matched by the index even when the file is gone: it is a removal, and it is committed as one.
What changes after the scan is the next scan's.

### The index starts from HEAD

`git reset --quiet` comes first, so that what a run that was killed left staged, and what the filter no longer
keeps, does not ride along. It works on a branch that has no commit yet. It writes `HEAD`, so a branch whose
reference is locked fails at this step and not at the commit. It does not refresh the stat data of the index
(tried: the modification time of a file that was touched is the same in the index after it), which `git
status` with `GIT_OPTIONAL_LOCKS=0` (ADR 0013) does not either.

### Nothing to commit, and how many files

After staging, the files that the index differs from `HEAD` by (`git diff-index --cached --name-only`, against
the empty tree when there is no commit) are the files of the commit. None means nothing to commit, which is not
an error: a file that was changed back before the commit comes to nothing, and `git commit` would fail with exit
status 1 for it. The count is the number in the message, and it is the files that are in the commit and not the
ones that were given.

### The message and the author

The message is `commitmsg.Render` with that count, passed with `--cleanup=verbatim`, so that it is stored as
the template rendered it: the default cleanup of Git, even for a message given on the command line, collapses
blank lines that come one after the other and strips the spaces at the end of a line (tried; a line that
starts with `#` is kept either way). The author and the committer are one identity. The Runner gets it as an
option and puts `GIT_AUTHOR_*` and `GIT_COMMITTER_*` in its environment, which beats the `user.name` and
`user.email` of a repository config that an application could write. Hooks and signing are already off (ADR
0013), and the Runner refuses `--trailer` and `--gpg-sign`.

### Before it writes, and when it fails

The writer lock is checked once, immediately before the first write, and nothing is written without it. A lock
file that a stopped Git left is removed first, after the same check (ADR 0028). A commit that fails after it
staged puts the index back as `HEAD`, which is a write too and so checks the lock again, and is not given up
by a stop, because a pull can be blocked by what is staged. A commit that fails is tried again at the next scan,
because the files are still there. A kill in the middle leaves at most a lock file and staged entries, which the
next commit clears and resets, so there is nothing to journal.

### The settings

`TREEBIND_COMMIT_AUTHOR_NAME`, `TREEBIND_COMMIT_AUTHOR_EMAIL` and `TREEBIND_COMMIT_MESSAGE_TEMPLATE`. The
operator passes each only when the spec sets it, so no sync pod is replaced for it, and the agent has the
defaults `treebind-bot`, `treebind-bot@treebind.invalid` (an address that cannot exist, RFC 2606) and
`commitmsg.DefaultTemplate`. A name and an email are set together or not at all.

The agent reads them as they are written and judges them when it makes its committer, with `ValidateAuthor` and a
trial render. A wrong one is a failed check in the report and the pulls and the scans go on, where a setting that
the configuration refused would end the agent at start for ever.

The kubelet expands `$(NAME)` and `$$` in a plain environment value (#179), and a message template may well
hold a `$`. The operator writes each `$` as `$$`, which the kubelet turns back into one, in front of a `(` or not.
A test applies the expansion of the kubelet to the pod the operator makes and holds that the agent reads the
values of the spec as written.

The CRD refuses what the agent could never use and CEL can say: a `<`, a `>` or a control character in a name, a
name or an email that starts or ends with a space or one of `. , : ; " ' \`, an email that is not of the form
`name@host`, a NUL or a length over 4096 in a template. It takes every author of a corpus of names and emails that
the agent takes, which a test holds against the API server; the agent stays the judge of the rest, such as an
address that `net/mail` does not read as it is written.

### The report

A check `Commit` after `Changes`, for a `bidirectional` Treebind. `Passed` says how many files and the short id,
or that the files that had settled came to nothing; `Failed` carries `CommitFailed` (new, and reasons are API:
ADR 0004) or `GitDirUnsafe` as for the checkout and the pull; `Skipped` with `NotChecked` says that no file has
settled yet or that the checkout is not made. The check keeps the last commit until the next one. A commit that
failed is taken out of the report when the scans find no change at all, because the files that could not be
committed were put back or are gone; a wrong author or template stays, because it is about the settings and not
about the files, and the files that settle are not committed until the spec is put right. The `Changes` check
stops saying that the agent does not commit.

## Consequences

- The commit of the application's edits exists, and nothing pushes it. Until #37, the commits stay on the volume,
  and a pull that finds the repository changed says `Diverged` (ADR 0020), because the checkout has commits that
  the repository lacks and the repository has commits that the checkout lacks. #37 and #38 follow.
- A commit of many files is one commit, with no limit on its size. Two writes seconds apart may be two commits.
- A file that is removed between the look at the paths and `git add` fails that commit, with the words of Git, which
  name the path, and the next scan commits the rest. The look makes the window short and does not close it.
- The id of the commit is read after it was made, and a failure of that read is told as a failed commit although
  `HEAD` moved. It is not expected: it takes a timeout of a command that has no work to do. The files are no longer
  changes, so the next commit is not affected.
- A sync pod of a Treebind that already sets `spec.commit` is replaced once, when the operator that passes the
  settings is installed, as for `settleTime` (ADR 0029). A Treebind that does not set it is not.
- The CRD refuses an empty `authorName`, `authorEmail` or `messageTemplate` that is written out, which it took
  before; a Go client or Helm that leaves the field out is not affected, and an object that is stored is not judged
  again until it changes.
- Every commit step runs a reset, an add, a comparison and a commit, and the cost of the reset grows with the
  number of files of the checkout. It was not measured here.
- `CommitFailed` is a new reason. A wrong author or template is found when the committer is made, once, and not
  at every commit.
- The files that a scan found settled are committed as the filter saw them. A file written between the add and
  the commit is in the commit as it was when it was added, and what comes after is the next scan's.
- Not decided here: the push (#37), conflicts (#38), the commit trailer and the breaker of the echo loop (#41; the
  Runner refuses `commit --trailer`, so a trailer would be text of the message), a scan for secrets before a
  push (#65), the check of the staged blob (#94), signed commits (#64), and a limit on the size of one commit.
