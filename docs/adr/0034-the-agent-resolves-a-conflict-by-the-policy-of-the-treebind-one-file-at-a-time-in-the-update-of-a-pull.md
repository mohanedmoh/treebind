# 0034. The agent resolves a conflict by the policy of the Treebind, one file at a time, in the update of a pull

- Status: accepted
- Date: 2026-11-18
- Issue: #38
- Extends: [0020](0020-the-agent-pulls-by-fast-forward-and-never-writes-over-a-local-edit.md), [0033](0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md)
- Extended by: [0037](0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md), which
  writes the `Conflict` condition, and makes a resolution that could not be made fail the `Conflict` check

## Context

FR-5 asks for a conflict policy and R-3 is the risk behind it: a `bidirectional` Treebind commits what the application
writes (ADR 0031) and pushes it as a fast-forward (ADR 0033), so when the repository gained commits meanwhile the checkout
and the repository each have commits that the other lacks. The pull says `Diverged` and writes nothing (ADR 0020), the push
says `NonFastForward` and sends nothing, and the sync is stuck until a person steps in. The API already has
`spec.sync.conflictPolicy`, `followRemote` by default and `followLocal`, and ADR 0004 gave the condition `Conflict` its
place and left its reasons to the issue that writes them.

The rules that stand are the ones of the agent's other writes: Git runs only through the Runner (ADR 0013), every write is
made after the writer lock was checked (ADR 0012, ADR 0023), nothing is written over a file that has a local edit or that
the application keeps out of the repository (ADR 0020), an update does not remove many files at once (ADR 0026), a stop does
not cut off a write and what a kill leaves is put right by the next pull (ADR 0028), and a push is never forced (ADR 0033).

## Decision

### The unit is the file

The base is the commit that the checkout and the repository's tip share, which is the empty tree when their histories share
none. A file is a path, with its mode and its content, and what a side changed is what `git diff-tree --no-renames` between
the base and its tip lists, so a rename is a delete and an add, and a mode change is a change of the file.

- A file that only one side changed takes that side's change, a delete included.
- A file that both sides changed, and not to the same thing, takes the whole version of the side that wins: its content and
  its mode, or its absence.

Hunks are not mixed. Git's own merge joins the changes of one file when they do not overlap, and the files of a Treebind
are dashboards and configuration, JSON and YAML, where two edits that are right apart can make a file that is wrong together,
and nothing here can check a result against a schema. A winner's file is the file as someone wrote it.

### followRemote: the repository wins

The tree is the repository's tip with the files that only the checkout changed put on it, and the commit has the tip as its
only parent. The history of the repository stays a line, and what a push sends afterwards, as a fast-forward by ADR 0033, is
only those files. When the checkout changed nothing that the repository did not, or nothing of what it changed can be put on
the tip, there is no commit and the checkout is moved to the tip: a commit would have the tree of the tip, push nothing and
say that it kept files. The files that could not be put on the tip are the ones for which the tip has a directory where the
file is, or a file where the directory is, and they are named in the result and are in the local ref.

The commits that the checkout had are kept before anything moves, under `refs/treebind/local/<number>`: a loose, numbered
ref of the same kind as the recovery refs (ADR 0026), local, and never pushed, because the push guard of the Runner refuses
a destination that is not under `refs/heads/`. Its cap is its own, 20 (`LocalRefs`), so that the routine updates of the
pulls do not push the commits of a conflict out, and the newest ref is not made again while it keeps the same commit, so a
resolution that is blocked and tried again at every interval makes one. When the application commits in between, each try
makes a ref, so the old ones are removed after a move that was refused as after one that was made, and not after the lock was
refused, which would be a write. A ref that is not a number is somebody's and is left alone. The files that both sides changed have the repository's version, and the application's is in that ref, which is
how it is got back: `git show refs/treebind/local/00000001:<file>`.

### followLocal: the volume wins

The tree is the checkout's with the files that only the repository changed put on it, and the commit has two parents, the
checkout's and the repository's tip. The history of the repository stays in the history of the branch, and nothing of it is
rewritten, forced or lost. The commit descends from the checkout, so it is applied as a fast-forward, and from the
repository's tip, so the push that follows is one too. There is no local ref: the commits of the checkout are in the history
of what it moves to.

### It is an update, with the checks of one

The commit is built with plumbing, and then the checkout is moved to it by `apply`, the code that applies the commits of a
pull: the state directory is looked at, the cap on deletions is applied to the files that the move removes, the lock is
checked immediately before the journal is written, the journal says that files may be half written, and Git refuses to write
over a file that has a local edit or an untracked file in the way. A resolution is therefore refused for what an update is
refused for, with the reasons `Blocked`, `TooManyDeletions`, `ReservedPath` and `Refused`, and writes nothing then. It makes
no recovery ref: the local ref, or the history of the merge, keeps what the recovery ref would.

followLocal moves with `git merge --ff-only --no-overwrite-ignore`, as a pull does. followRemote moves to a commit that does
not descend from the checkout, so it moves with `git checkout --no-overwrite-ignore -B <branch> <commit>`. Tried on Git
2.34.1 and in the image of the agent, Git 2.56.1: `git reset --keep` and `git read-tree -m -u` write over an ignored file of the
application that is in the way, and `checkout --no-overwrite-ignore` refuses it, as it refuses a file with a local edit and an
untracked file, and keeps a local edit to a file that the move does not change.

### The index is the one place a tree is made, and is put back

Git makes a tree from files that are not in the work tree only in an index, and the Runner has no index of its own to give a
command, so the real one is used: `read-tree` of the winner's tip, `update-index --force-remove` and
`update-index --add --cacheinfo` for the files of the other side, in batches of 200, `write-tree`, and then `read-tree HEAD`,
before the commit is made. Deletions are made first, so that a file that the other side replaced with a directory is out of
the way of the files in it. The index is not refreshed afterwards: after a `read-tree` the index has no state of the files,
and `git reset --keep` says `not uptodate` for every file that it changes, edited or not (tried), but a merge and a checkout
refresh the index before they decide that a file is edited, and the move is one of them, which the tests of a local edit hold.
A batch that
fails is tried a file at a time to find a file that cannot go on the winner's tree because the tree has a file where it
needs a directory, or a directory where it needs a file, and that file is left out and named in the result, and only in
the local ref with followRemote. Any other failure fails the resolution.

### A kill at each step

Nothing is written before the lock is checked, and before the first write only reads are made.

| Where the agent is killed | What it leaves | What puts it right |
| :-- | :-- | :-- |
| Before the local ref | Nothing | The next pull resolves |
| After the local ref | The ref | The next pull resolves and makes no second ref, because the newest keeps the same commit |
| After the index was read, while files are put in it, after the tree was written | An index with files staged | The repair that every pull makes before it begins puts the index back to the commit the checkout is at (ADR 0028), says so, and the pull resolves |
| After the index was put back, after the commit was made | Objects that nothing refers to | The next pull resolves |
| After the journal was written, before the move | A journal and no change | The repair undoes the update, which puts back no file, and the pull resolves |
| In the middle of the files | Some files written, the journal | The repair puts back the files that are the agent's own, as for any update, and the pull resolves |
| After the move, before the journal is removed | The new commit and the journal | The repair finds the checkout at the commit of the journal, removes it, and the pull finds the checkout ahead and the push goes on |

A stop is not a kill: the objects and the index of a resolution that has begun are finished, with the time of ADR 0028, and
the move is not begun once the agent was told to stop. Each of these is a test on a real repository, with a stand-in for git
that fails every command from the step on, so that nothing the agent does to clean up can hide what a kill leaves, and a real
SIGKILL in the middle of the files.

### Only a Treebind with a policy resolves

A `pull` Treebind has no commits of its own and no policy, and a divergence stays `Diverged`, as it was: the repository was
force-pushed, and nothing here decides for a person who did not ask for it. The `Policy` of the Puller is empty for it.

### The setting, the check and the reasons

The agent reads `TREEBIND_SYNC_CONFLICT_POLICY`. The operator sets it only for `followLocal`, so that the default of the CRD
is the default of the agent and no sync pod is replaced for it; a test holds the two to the same value. The pull has the
outcome `Resolved`. The Pull check says `Passed` and that it resolved a conflict, which makes the agent ready as a pull that
advanced does, and a `bidirectional` Treebind has a check `Conflict` after `Push`: `Skipped` with `NotChecked` until a conflict has been resolved, saying that none has been since the agent
started, which is all the check knows: a conflict that every resolution fails is only in the failed `Pull` check, and the
check does not claim that there is none. After a resolution it is `Passed` with the new reason `ConflictResolved`,
whose message says the policy, how many commits each side had, how many files had the winner's version, how many were kept from
the other side, the commit that was made or that none was, the ref that keeps the commits of the checkout for followRemote,
and the files that were left out and in whose commits they are. A resolution that made a commit calls for a push at the next
scan, and one that made none leaves the wait of a push that failed as it was. A resolution that could not be made is a
`Failed` `Pull` check with the reason of the update, and a message that says that the conflict was not resolved; the
`Conflict` check is not changed by it, because nothing was resolved. A pull that has a policy and no valid author for the
commit refuses before it writes anything. The outcome `Resolved` and the reason `ConflictResolved` are API
(ADR 0004), and nothing else is new. The `Conflict` condition and its event are not written here: the status poller reads the
report and interprets no check, and #74 can match the check and the reason.

## Consequences

- A diverged Treebind does not stall: the next pull resolves it, and the push that follows is a fast-forward.
- With followRemote the application's version of a file that both sides changed is not in the branch. It is in the local ref
  while it is among the last 20, with the commit that held it, and the check names the ref. A person who wants it back takes
  it from there.
- A file is the unit, so two edits of one file that Git could join are not joined: one side's version is taken whole.
- A rename that one side made and the other side edited is a delete and an add. With followRemote the repository's edited file
  comes back under its old name and the application's new name is kept, so both names exist until a person removes one.
- A repository whose branch was force-pushed over commits that the checkout has is a conflict like any other: with
  followRemote the files that only those commits changed are carried onto the new history, which puts back a file that the
  rewrite removed, and the commits are in the local ref. A person who rewrote the branch to remove something has to remove it
  from the volume too. This is the part least certain, and is named in the pull request.
- Each resolution of followRemote that carries files makes a commit that the application did not write, by the author of the
  commits of the agent.
- Not here: the echo of a pushed resolution that another Treebind pulls again (#41), a scan of what is pushed for secrets
  (#65), pull-request mode (#62), and the `Conflict` condition and event (#74).
