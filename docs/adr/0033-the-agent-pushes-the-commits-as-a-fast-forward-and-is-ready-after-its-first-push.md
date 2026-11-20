# 0033. The agent pushes the commits as a fast-forward, reports a refusal, and is ready after its first push

- Status: accepted
- Date: 2026-11-18
- Issue: #37
- Extends: [0030](0030-the-agent-is-ready-after-its-first-pull-and-stays-ready-while-it-runs.md)
- Extended by: [0037](0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md), which
  shows a rejected or failed push as `Synced` `False` with the reason of the `Push` check

## Context

FR-4 asks the agent to push its commits by policy and never to force. R-13 is the risk behind the reasons: protected
branches, secret push protection and file-size limits of a host refuse pushes, and the sync stalls without a word
unless the refusal reaches the report. ADR 0031 commits what the application writes and leaves the commits on the
volume, and says that a pull that finds the repository changed meanwhile says `Diverged`.

Git runs only through the Runner (ADR 0013). A push comes in the same loop as the pulls and not beside them (ADR 0024).
Every write is made after the writer lock was checked (ADR 0023, #105). A command tells the liveness probe how long it
may take through the Runner options (ADR 0030). The conflict policies and a side branch for local work are #38, the echo
loop is #41, a scan for secrets is #65 and pull-request mode is #62: none of them is here.

## Decision

### Only a fast-forward, only the followed branch, and never a force

The agent pushes the branch that the checkout follows to the branch of the same name, with
`git push --porcelain --no-verify -- <url> refs/heads/<branch>:refs/heads/<branch>`. A push that the repository
refuses is reported and not worked around.

FR-4 holds in the Runner and not in the caller. Before Git starts, the Runner refuses on `push` `--force` and `-f`,
`--force-with-lease`, `--force-if-includes`, `--mirror`, `--delete` and `-d`, `--all`, `--branches`, `--tags`,
`--prune`, `--follow-tags`, `--set-upstream` and `-u` (which write the Git config), `--push-option` and `-o` (which
reach the hooks of the host), `--repo` and `--recurse-submodules`, with the starts of them that Git takes, and any
refspec that starts with `+`, has no source, has a `*`, or has a destination that is not under `refs/heads/`, and a push that
names no refspec, because it leaves what is pushed to Git. Tried on
Git 2.34.1: `git push -- <url> :refs/heads/main` reaches the host and asks it to delete the branch, and
`--force-wit=main:<id>` forces. A start of `--force` alone is ambiguous with the longer options, and Git refuses it
itself. Other subcommands are not held to this, so the fetch of the pull still forces its tracking ref.

### When it is tried

In the scan turn, after the commit, in the same turn: a push that is made beside a pull would be two operations that ask
the repository at once (ADR 0024). It is tried when commits may wait: at the first scan, for commits that an earlier run
left; after a commit; and again after a try that did not work, no sooner than the wait. It is not tried at every scan, so
a Treebind that has nothing to push asks Git nothing. A push does not depend on the commit step being there.

What waits is counted from the commits that the checkout has and the last fetched tip (`refs/remotes/origin/<branch>`)
does not. Nothing is sent when there are none, or when the checkout has no commit. A push that cannot be a fast-forward,
because the checkout and the last fetched tip have gone different ways, is not sent either: the host would refuse it, and
a host that is asked in vain at every try is a load for nothing (R-19). That is `NonFastForward` without asking.

The writer lock is checked immediately before the push, and nothing is sent without it. A push is made through the Runner
options that the agent gives, so it tells the liveness probe how long it may take, with the timeout of a fetch (30 minutes),
as any command does. After a push that worked the agent moves the tracking ref, which Git does not do for a push to a URL
(tried: no ref under `refs/remotes` after the push), so that the count of what waits stays true. The move is a write, so it checks
the lock again and does nothing without it. A failure of it, or a lock that cannot be checked, is a warning in the check,
and the push still worked.

### What Git said, and what is read of it

`--porcelain` writes one line for the branch, which Git words itself: a flag, the refspec, and a summary with a reason in
parentheses. Tried on Git 2.34.1, and the tests run on the Git of the agent image, 2.56.1:

- a first push to an empty repository: `*  [new branch]`, exit 0; a fast-forward: a space and the two ids, exit 0; and
  nothing to push: `=  [up to date]`, exit 0;
- the repository moved: `!  [rejected] (fetch first)`, and `(non-fast-forward)` once the tip of the repository is known
  locally, exit 1;
- a hook that declines: `!  [remote rejected] (pre-receive hook declined)`, exit 1, with the words of the host on standard
  error behind `remote:`;
- a repository that is not there: no ref line, exit 128.

Only that is read. A rejection of Git that says the repository moved is **`NonFastForward`**. Any other `!` line is
**`PushRejected`**: a protected branch, a hook, a rule, a file that is too large, a secret that the host found. Its message
is what Git said in brackets and then the words of the host as they came, from the `remote:` lines in order, with the
credentials of any URL removed, in one line and at most 2 KiB. Nothing that is a host's own wording decides anything,
because every host words it differently (GitHub `protected branch hook declined`, GitLab and Gitea `pre-receive hook
declined`, Bitbucket `hook declined`); the tests carry samples of each, written from the documented wording and labelled
as that, and not captured from a host. No ref line at all is **`PushFailed`**: the host did not answer, the key was
refused, the volume refused, or the writer lock could not be checked. A Git directory that holds more than the agent put
there has the reason of the checkout and the pull (`GitDirUnsafe`). Reasons are API (ADR 0004), and the three are new.

### How long it waits after a failure

The push waits as a pull does (ADR 0024). A push that got no answer (`PushFailed`) is tried again after a wait that is
drawn between the interval and a ceiling that doubles with each failure in a row, up to ten minutes. A push that the
repository answered and refused (`NonFastForward`, `PushRejected`) is tried again after the interval, give or take a
fifth, and is reported until a person puts it right: the next try finds it at once when they have, and costs one push.
A commit that is made in the middle of a wait does not end it.

### The report

A check `Push` after `Commit`, for a `bidirectional` Treebind. `Passed` says how many commits and the two ids, or that there
is nothing to push; `Failed` carries one of the reasons above; `Skipped` with `NotChecked` says why it was not tried. The
check keeps the last result until the next try. A pusher that cannot be made, for a repository with no address, is a failed
check that stays, and the commits and the pulls go on.

### Readiness: the first push is a sync too (extends ADR 0030)

ADR 0030 said that the first sync of a `bidirectional` Treebind would have to include what it pushes. The maintainer
decided that the first push that works makes it ready, and this is how it is read: the agent is ready from the first pull
that ends `Passed`, as before, or from the first push that works, whichever comes first, and once ready it stays ready for as
long as the process runs. So a `bidirectional` Treebind on an empty repository is ready after its first commit is pushed,
and does not wait for a second pull. A push that is rejected, that fails, or that finds nothing to push does not make it
ready, because it says nothing of the repository and the directory having come together. A `pull` Treebind is unchanged.
The report holds the push before the probe says ready, as it holds the pull.

### What the operator does

Nothing is new there. The status poller of #99 reads the report and sets one condition, from whether it could be read; it
interprets no check and no reason. A test of the client holds that a report with the check `Push` and its reasons, and with
reasons that it does not know, is read like any other, and that the longest report the agent can make is far under the limit.
The conditions for a rejection are left to #38 and #74.

## Consequences

- The commits of the application reach the repository, and a Treebind on an empty repository gets its first commit there
  and turns `Ready`. A pull that finds the repository changed meanwhile still says `Diverged` when the checkout has
  commits of its own, and the push is then `NonFastForward` and is not sent. The conflict policies (#38) say what to do.
- A host that refuses the push for a reason that a person fixes (a protected branch, a rule) is asked once for each
  interval, and shows the host's own words in the check. Nothing is retried with force, and nothing is pushed to another
  branch: a side branch is #38.
- The guard of the Runner refuses an option that a later change might want, such as `--atomic` for several refs: it is
  allowed, and the refspec rule is what limits it to a branch. Anything else has to be added on purpose.
- The first push makes a Treebind ready that no pull has passed for, so `Ready` can be `True` while the check `Pull` says
  `BranchGone` or `FetchFailed`. The report says which, and the pull goes on.
- Not decided here: what a rejection does to the local commits (#38), the echo loop of a push that the next pull sees as
  its own (#41), a scan for secrets before a push (#65), pull-request mode (#62), and a probe that depends on the volume
  probe (#101).
