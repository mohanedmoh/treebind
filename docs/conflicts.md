# Conflicts: when your directory and the repository each changed

A `bidirectional` Treebind commits what the application writes and pushes it. When the repository gained commits
meanwhile, your directory and the branch each have commits that the other lacks, and the agent resolves it with
`spec.sync.conflictPolicy` in the pull that finds it. The decision is
[ADR 0034](adr/0034-the-agent-resolves-a-conflict-by-the-policy-of-the-treebind-one-file-at-a-time-in-the-update-of-a-pull.md).
A `pull` Treebind has no commits of its own and no policy: it says `Diverged` and writes nothing
([pulling](pull.md)).

## What decides: the file, and the side that wins

The agent compares what each side changed since the commit they share, a file at a time.

- A file that only one side changed takes that side's change, a deletion too.
- A file that both sides changed, to different things, takes the whole file of the side that wins: its content and its mode,
  or its absence. Two edits of one file are not joined, because a dashboard or a configuration that is right in each
  edit can be wrong with both.
- A rename is a deletion and an addition. If one side renamed a file and the other edited it, the winner's version of the
  old name and the other side's new name both exist afterwards.

| `conflictPolicy` | Who wins | What the checkout becomes | What is pushed |
| :-- | :-- | :-- | :-- |
| `followRemote`, the default | The repository | The repository's tip, with the files that only your directory changed put on it, in one commit on top of the tip | Those files, as a fast-forward. The history of the repository stays a line |
| `followLocal` | Your directory | A commit with two parents, your directory's last commit and the repository's tip, in which a file that both changed has your version | The merge, as a fast-forward. The history of the repository is in it, and nothing is forced or rewritten |

When your directory and the branch hold the same files, there is nothing to decide: with either policy the checkout moves to the
branch's tip with no commit, and your commits are kept under `refs/treebind/local/<number>`, as with `followRemote`. Nothing is
pushed. A host that gives back what it is sent as another commit with the same files would otherwise make a merge at every pull
([echo loops](echo-loops.md)).

## Getting back a version that lost

With `followRemote` the commits that your directory had are kept before anything moves, under
`refs/treebind/local/<number>` in the Git directory on the claim. They are never pushed. The last 20 are kept. The
`Conflict` check names the ref.

```
git --git-dir=<mount>/.treebind/git show refs/treebind/local/00000001:dashboards/sales.json
```

With `followLocal` your commits are in the history of the branch, and the repository's version of a file that lost is in
the commits that came from it, except when both sides held the same files: the commits are then in the ref, as with
`followRemote`.

## What the report says

The `Pull` check says `resolved a conflict` and points at the `Conflict` check, which comes after `Push` and is `Skipped` until
a conflict has been resolved, and says that none has been since the agent started, which is all it knows. After a resolution
it is `Passed` with the reason `ConflictResolved`, and its message has the policy, how many commits each side had, how many
files had the winner's version, how many were kept from the other side, the commit that was made, or that none was, and, for
`followRemote`, the ref that keeps the commits of your directory. It says that until the next resolution, and it does not
change whether the agent is ready.

A resolution that cannot be made, for a file with a local edit, an untracked file in the way, too many deletions or the limit
of commits of the hour, is a failed `Pull` whose message begins with `the conflict between the checkout and the branch ... was
not resolved`, and it makes the `Conflict` check `Failed` with the reason of the update: `Blocked`, `TooManyDeletions`,
`TooManyCommits`, `Refused` or `PullFailed`. An update that is no conflict fails with the same reasons and leaves the check as
it was. The next pull that passes without a conflict puts the check back to what it said before, the last resolution or
`Skipped`, and a pull that fails for another cause leaves it as it was. The push of what a resolution made is tried at the next scan, without waiting for the interval of a push
that was refused. A resolution that made no commit has nothing new to push, and leaves that wait as it was.

## What the Treebind says

The operator derives the condition `Conflict` from that check, and writes it with `Synced` at its next poll of the agent
([the conditions](agent-status.md#conflict)).

| `Conflict` | Reason | When |
| :-- | :-- | :-- |
| `True` | The reason of the update that could not be made | The last pull found a conflict and could not resolve it, and no pull has passed since. Local work waits for a person: the message and the `Pull` check say which update was refused |
| `False` | `ConflictResolved` | The last conflict that the agent found since it started was resolved, with either policy |
| `False` | `NoConflict` | The agent knows of no conflict that waits, and has resolved none since it started. It is the answer for a `pull` Treebind too, whose `Diverged` is `Synced` `False` and not a conflict |
| `Unknown` | As `Synced` | The operator does not know what the sync does, and `Synced` says why: no report yet, a report that is not of the current spec, none yet in which the agent has seen the two agree since it started, or an agent that cannot be read |

A conflict that cannot be resolved is a failed step, so `Synced` is `False` with the same reason, and `Ready` follows that as
[the status page](agent-status.md#ready) says. `Conflict` is not an input of `Ready`: with `TooManyDeletions`, `TooManyCommits`,
`Refused` and `PullFailed`, which a person puts right, `Ready` turns `False` with `NotSynced`, and with `Blocked`, which the
next scan and pull put right by committing the edit, it stays `True`. A resolution is not a conflict that waits: `ConflictResolved`
is `False`, because the sync went on by the policy. The agent keeps what it knows in memory, so after a restart `Conflict` can
say `NoConflict`, when `Synced` is `False` for another step, until a pull finds the conflict again.

## What it does not do

- **It does not write over a local edit.** A file that has an edit that is not committed yet, and that the resolution would
  change, an untracked file in its way, or a file that a `.gitignore` hides, makes the pull `Blocked`, as for an update, and
  nothing is written. `Conflict` is `True` with that reason until a pull passes. The agent commits the edit at its next scan,
  and the pull goes on.
- **It does not remove many files.** The cap of [the deletion guard](deletion-guard.md) applies to what a resolution removes.
- **It does not force and does not rewrite.** The repository's refs are never written by a pull, and the push that follows
  is the fast-forward of [pushing](local-changes.md).
- **A file that cannot be put on the winner's tree** because it has a file where your directory needs a directory, or the
  other way round, is left out, named in the check, and kept in the ref with `followRemote`. When that leaves nothing to put on
  the repository's tip, `followRemote` makes no commit and the checkout is moved to the tip.
- **A branch that was force-pushed is a conflict like any other.** With `followRemote` the files that only the commits that
  were dropped changed are carried onto the new history, and the commits are in the ref. If the branch was rewritten to
  remove a file, remove the file from your directory too.

## If the agent is killed in the middle

Nothing is lost and nothing stays half done. A resolution is an update, so a kill while the files are written is undone by
the repair that every pull makes first, and an index that was left with files staged is put back, as
[the Git directory](git-directory.md) says, and the pull resolves again. A ref that was made and a commit that nothing
refers to are harmless, and the ref is not made twice.
