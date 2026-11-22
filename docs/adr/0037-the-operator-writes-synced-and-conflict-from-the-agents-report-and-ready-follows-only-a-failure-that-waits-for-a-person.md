# 0037. The operator writes Synced and Conflict from the agent's report, and Ready follows only a failure that waits for a person

- Status: accepted
- Date: 2026-11-20
- Issue: #214
- Extended by: [0038](0038-the-operator-records-an-event-and-a-metric-where-it-writes-a-condition-of-the-sync.md), which records an event and a
  metric where `Synced` and `Conflict` are written, as the section "Not here" below planned
- Extends: [0004](0004-treebind-status-conditions.md), [0011](0011-the-agent-status-endpoint.md), [0015](0015-the-operator-reads-the-agents-status.md), [0030](0030-the-agent-is-ready-after-its-first-pull-and-stays-ready-while-it-runs.md), [0033](0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md), [0034](0034-the-agent-resolves-a-conflict-by-the-policy-of-the-treebind-one-file-at-a-time-in-the-update-of-a-pull.md)

## Context

NFR-6 asks that every sync outcome is visible as a condition. ADR 0004 gave `Synced` and `Conflict` to the status poller and
left to the issue that writes them what it did not decide: the reasons of `True` and `False`, and how a `Synced` that was
written before a spec change counts. ADR 0015 built the poller and decided the second in words, for a report that had no state
of the sync to carry. #99 shipped `AgentUnreachable` and nothing else: of the report the poller used the fact that it got one.

The definition of done of the MVP, tried by hand on a kind cluster with the merged code, ended in `kubectl describe` showing
`Synced` and `Conflict` `Unknown` with `AwaitingAgentReport` after a file written in the pod had become a commit on the remote
and a commit pushed from Git had become a file in the pod, and no `lastSyncedCommit` and no `lastSyncTime`. Two things followed
from reading the code. `Ready` could not be `False` for `NotSynced`, because `Synced` was never `False`: a push that the host
rejected, a `Diverged` pull and a refused update left the Treebind `Ready`. And the report had no field for the commit that the
volume and the repository last agreed on: the short id was in the text of a message, which a reader must not parse.

The texts of the earlier records did not agree with each other, and each disagreement is a decision below:

- ADR 0004 makes `Ready` `False` whenever `Synced` is `False`. ADR 0030 says that a readiness that followed the last pull
  "would turn `Ready` of every Treebind that uses a Git host False for as long as the host is down, a reason that is not the
  data path", and leaves the sync state to `Synced`. Once `Synced` is written, ADR 0004's rule does that through `Synced`.
- ADR 0011 and ADR 0015 plan to put the generation of the spec in the report, which means passing it to the agent. That
  replaces the sync pod at every change of the spec, and ADRs 0024, 0026, 0031, 0034 and 0036 each take care that no sync pod is
  replaced by a change that the agent does not need.
- ADR 0004 says `Conflict` is `True` when "parked local work waits for someone". It was written before ADR 0034 made both
  policies resolve by themselves. Local work now waits for a person only when a resolution cannot be made, and that was a failed
  `Pull` with the reason of the update (`Blocked`, `TooManyDeletions`, `TooManyCommits`), the same as for an update that is no
  conflict, so that only the message told them apart. ADR 0034 gave the condition and its event to #74.
- ADR 0004 says `Synced` is `True` "when the volume matches the repository at `status.lastSyncedCommit`", and the doc of
  `status.lastSyncTime` said "when the last sync finished". A pull that finds nothing new finishes at every interval.
- ADR 0015 says that nothing from the agent's answer goes into a message. What makes a failed check useful, the file and the
  words of the host, is its message.
- ADR 0009 expected the reasons of the volume checks to be those of a `VolumeReady` condition, decided with the poller. There is
  no such condition.

A trap that shaped the code: `metav1.Time` is kept in whole seconds. A time with a fraction is not equal to itself once it has
been through the API server, so a `lastSyncTime` taken from the report as it is would be written at every poll.

## Decision

### The reasons are API

The reasons of `Synced` and `Conflict` have constants in `api/v1alpha1`, because users and tooling match on them (ADR 0004). A
test holds the reasons of the agent and these to one spelling: a reason that the agent gives and the API lacks, a constant that
no step of the agent gives and a spelling that differs each fail it, so that a reason added to the agent is not missed.

| Condition | Status | Reason |
| :-- | :-- | :-- |
| `Synced` | `True` | `InSync` |
| `Synced` | `False` | The reason of the check that failed, as the agent spells it: `MountPathInvalid`, `NotMounted`, `NotWritable`, `ReadOnlyVolume`, `WriteFailed`, `GitDirUnsafe`, `TimedOut`, `LockFailed`, `DirectoryNotEmpty`, `RepositoryMismatch`, `BranchNotFound`, `ReservedPath`, `FetchFailed`, `CheckoutFailed`, `PathNotFound`, `PathMismatch`, `Blocked`, `Diverged`, `BranchGone`, `TooManyDeletions`, `Refused`, `TooManyCommits`, `PullFailed`, `ScanFailed`, `CommitFailed`, `NonFastForward`, `PushRejected`, `PushFailed`; or `SyncFailed` |
| `Synced` | `Unknown` | `AwaitingAgentReport`, `AgentUnreachable`, `SyncPending`, `AgentOutdated` |
| `Conflict` | `True` | The reason of the update that could not be made, from the same list |
| `Conflict` | `False` | `ConflictResolved`, `NoConflict` |
| `Conflict` | `Unknown` | As `Synced` |

`SyncFailed` is the reason for a step that failed with a reason that this release does not define, as an agent of another
release can give. The operator writes no reason it did not define, and it does not copy the name of a check that it does not
know either. `NotChecked`, the reason of a step that was skipped, is the one reason of the agent that is no failure and has no
constant. The volume checks of ADR 0009 are steps like the others: a failed `Mount`, `Writable` or `GitDir` check is `Synced`
`False` with its reason, and no separate condition is made.

The `Unknown` reasons are used as follows:

- **`AgentUnreachable`**, while the condition `AgentUnreachable` is `True`. The last values of both conditions are no longer
  known to hold, as ADR 0015 says. While `AgentUnreachable` is `Unknown` (`PollFailing`, `AgentNotRunning`, `TokenUnavailable`,
  `KeysUnavailable`), and when a poll gets no report, the poll says nothing of the sync and neither condition changes. The next
  report puts both back.
- **`AgentOutdated`**, when the report is of another pod template than the spec gives now, or of none, as from an agent of the
  release before. It happens while the sync pod is replaced after a change of the spec, and at the upgrade of the operator. The
  agreement that such a report holds is not taken either, because it can be of the repository or the branch of the spec before.
- **`SyncPending`**, when the report is of the current template, no step has failed and the agent has not yet seen the volume and
  the repository agree since it started: the lock, the checkout, the first pull or the first push is still to come. It is what
  every restart of the sync pod shows until then, and it is not a failure.
- **`AwaitingAgentReport`**, from ADR 0004, until the first poll.

### What `Synced` says

`Synced` is `False` when a step failed, with the reason of the first failed check in the order of the report, which is the order
of the steps, except that a reason that waits for a person comes before one that waits for the repository (the next section).
`Ready` acts on the reason of `Synced`, and a Git host that does not answer must not hide a file that someone has to move. It is
`True` with `InSync` when no step failed and the report has an agreement, and `SyncPending` when it has none.

**It is per commit, and the files that the application has written do not count against it.** ADR 0004's "the volume matches the
repository" read plainly is a statement about files, and this record contradicts that reading. What the agent reports is whether
its steps passed and at which commit it saw the checkout and the branch agree, and it commits what the application writes only
after the files have settled. A `Synced` that said `False` for a file that is written and not committed yet would turn `False` at
every write of the application, for a settle time and an interval, and a Treebind that works would flap. So `True` says that no
step has failed and that the agent has seen the two agree, at `status.lastSyncedCommit`. Files that have not settled, files that
have settled and are not committed, and commits that are not pushed yet are not failures, and the `Changes`, `Commit` and `Push`
checks say how many there are. A condition for local changes that wait is a decision of its own and is not made here.

**The messages are fixed (ADR 0015's rule stays).** A message is made of fixed sentences, as they apply: the one of the step, the
one of the reason, the first 12 digits of `status.lastSyncedCommit`, and where to read more, which is the log of the sync pod.
Nothing the agent wrote is in it, not the message of a check, which names files and quotes the Git host, and not the name of a
check that this release does not know. The alternative was the check's message in the condition, cut to a length and without
control characters, which says more in `kubectl describe` and puts the names of files and the words of a Git host in the
Treebind for everyone who can read it. The commit a message names is the one of the status, which only a newer agreement
changes, so a message does not change while its cause stays, a restart of the agent changes none, and a poll that finds what
the last one found writes nothing.

### `Ready` follows a failure that waits for a person

When the sync pod is ready, `Ready` is `False` with `NotSynced` only when `Synced` says `False`, of the Treebind's current
generation, with a reason that waits for a person. It stays `True` for the reasons that wait for the repository, which `Synced`
still says: `FetchFailed` and `PushFailed` always, because the Git host did not answer, which is the case of ADR 0030; and
`NonFastForward` and `Blocked` for a Treebind that syncs in both directions, because the next pull brings the commits that the
push lacked and the next scan commits the edit that stood in the way of the update, which the next pull then resolves. For a
Treebind that only pulls there is no scan, so a local edit in the way waits for a person. Every other reason, `SyncFailed` among
them, waits for a person: a file, a branch, the volume, a limit of the spec or a rule of the repository has to change before the
next try can pass. The classification is one function that the conditions and `Ready` both use.

A `Synced` of an older generation counts as `Unknown` for `Ready`, so a spec change does not make `Ready` `False` for the report
of the spec before, and `Unknown` never makes it `False`. `Conflict` stays no input of `Ready`, as ADR 0004 has it: a conflict
that cannot be resolved is a failed step, so `Synced` says it with the same reason, and `Ready` follows that rule. With
`TooManyDeletions`, `TooManyCommits`, `Refused` or `PullFailed` it turns `False`, and with `Blocked` it stays `True`.

The alternative was ADR 0004 as written, every `False` makes `Ready` `False`. A Git host that is down would make every Treebind
that uses it not ready, and a push that meets a commit made in Git meanwhile would make a busy Treebind not ready for up to an
interval. `Ready` is what `kubectl wait` and the health checks of a GitOps controller act on (ADR 0015), so it keeps its meaning,
the data path, and `Synced` is where the sync state is read. What it costs: a Treebind whose host has been down for a day is
`Ready` and `Synced` `False` with `FetchFailed`, and a person who reads `Ready` alone does not see it.

### The report is of a pod template, and not of a generation

The agent is given the hash of its pod template as `TREEBIND_TEMPLATE_HASH` and reports it as `templateHash`. The operator
computes it over the template without the variable, so the variable is a function of the rest of it and the template changes
exactly when it did before. The annotation of the StatefulSet stays the hash of the whole template. The agent keeps the value and
refuses one that is not 64 lowercase hexadecimal digits at its start, as for its other settings, because the operator compares it
by equality, and an agent that was started without it reports none. The poller computes the hash with the one function that gives
the agent the variable, from the same settings (the image of the agent and the issuer and the subject of the operator's token),
and refuses to start without the issuer and the subject as the reconcile does. If the two could drift apart, no report would be of
the current template and `Synced` would say `AgentOutdated` for as long as the operator runs, so a test holds the hash that the
poller expects to the value of the variable in the StatefulSet that the reconcile builds and to the one that the API server
stores.

A report of the current template gives both conditions the Treebind's generation, and so do the `Unknown` ones that the operator
itself sets. A change of the spec starts a poll at once, because the poller's watch has `GenerationChangedPredicate`, so a
`Synced` of the spec before lasts until that poll only, and `Ready` compares the generation for that moment. This replaces ADR
0015's rule that the poller sets the generation the report says the agent ran with, and ADR 0011's plan to put the generation in
the report.

The alternative was the generation in the pod, as ADR 0011 planned, which is ADR 0015's rule word for word. It replaces the sync
pod at every change of the spec, an ignored `spec.agent` field or a default written out included, and a test holds that an
explicit `mountPath: /work` or `followRemote` is the same template as the default. What the hash costs is **one replacement of
every sync pod when the operator is upgraded**: the StatefulSet that the operator wrote before holds the hash of a template
without the variable, which is the value the variable holds, so each is rewritten once and each pod is replaced, which is how it
gets the variable. While it is replaced `Ready` says `SyncPodNotReady` until the new pod has pulled (ADR 0030), and `Synced` and
`Conflict` are `Unknown`: `AgentOutdated` while the old pod answers, and `SyncPending` once the new one does. There is no file of
upgrade notes in the repository, so [the status page](../agent-status.md#upgrading-the-operator) says it, and the announcement of
the release should, as `docs/releasing.md` asks for upgrade notes there. The test that pins the hashes of the operator before
(#87) pins the value of the variable now, and holds that the annotation differs from it.

### What `Conflict` says

`Conflict` is `True` when the last pull found a conflict between the checkout and the repository and could not resolve it, with
the reason of the update that could not be made. It is `False` with `ConflictResolved` when the last conflict since the agent
started was resolved, and with `NoConflict` when there was none, and it is always `NoConflict` for a Treebind that only pulls: it
has no policy and no commits to resolve with, and its `Diverged` is `Synced` `False`. It is `Unknown` whenever `Synced` is, with
the same reason and message, for both directions.

A resolution is `False`, also one by `followRemote` that kept the application's version of a file only in
`refs/treebind/local/<number>`. The alternative was `True` after a resolution that set the application's version aside, which
is nearer to "parked local work", but nothing acknowledges it, so it would stay `True` until a restart of the agent, and the sync
went on by the policy in the meantime. The check says how it resolved, and `ConflictResolved` points there.

This changes ADR 0034 for one case: **a resolution that could not be made now makes the `Conflict` check `Failed`**, with the
reason of the update, where ADR 0034 left the check as it was and said the failure was only in the `Pull` check. The next pull that
passes without a conflict puts the check back to what it said before, the last resolution or `Skipped`, and a pull that fails for
another cause leaves it as it was. It still never changes whether the agent is ready (ADR 0030). What tells a conflict from an
update that fails with the same reason is the field `Conflict` of the pull's error, which the one place that wraps a failed
resolution sets, and not the message: a fast-forward that a file stops, and a resolution that the same file stops, are the same
reason and differ only there. The agent never parses a message and the operator never sees one. A Treebind that only pulls keeps
having no `Conflict` check.

`NoConflict` is what the agent knows, and the agent keeps it in memory: the check is `Skipped` after every start of the agent
until a conflict has been resolved. So `Conflict` can say `NoConflict` right after a restart, before a pull has run, while `Synced`
says `False` for another step, even though the pull before the restart could not resolve a conflict. When `Synced` is `Unknown`,
as it is when the agent has nothing to say yet, `Conflict` is too. The next pull that finds the conflict again makes it `True`.

### What `lastSynced` and `lastSyncTime` say

The report gains `lastSynced`, with `commit`, `time` and `by` (`Pull` or `Push`), and only fields are added (ADR 0011), so the
report stays at version 1. It is the commit at which the agent saw the checkout and the branch of the repository agree, and when
and by which step it **first** saw them agree there. A pull counts when it ended `UpToDate` or `Advanced`, or `Resolved` by moving
the checkout to the tip with no commit made, which is what a resolution does when the application changed nothing that the
repository did not, or when both sides hold the same files. A push counts when it pushed. A resolution that made a commit, and a
pull that found the checkout ahead, leave the checkout at a commit that the repository does not have yet, so the push that takes
it counts instead. A push with nothing to push asked the repository nothing, so the next pull, which does, records it. A failure,
or a step that left the two apart, changes nothing, and the field is absent until the first such step since the agent started.
The commit is the full id of the repository's commit, also for `spec.repository.path`, where it is the commit whose directory the
checkout holds and not the checkout's own snapshot of it (ADR 0036). It is published in the same snapshot as the checks, so a
report never pairs a check with an agreement from another moment, and nothing of it is kept on the volume.

The operator writes `status.lastSyncedCommit` and `status.lastSyncTime` when the report's commit is not the one in the status, and
never clears them. `lastSyncTime` is when the volume and the repository came to agree at `lastSyncedCommit`, so it moves with the
commit only, and a step that finds them agreeing at the same commit again changes nothing. A restart of the agent, a failed poll
and a pull that finds nothing new write neither field. The alternative was the last pull or push that found them agreeing, which
writes the status of every Treebind at every interval, when `AgentUnreachable` and the transition of `Synced` already say that the
news is fresh. How fresh a report is, is what `checkedAt` says. **Times are in whole seconds**: the time of the agreement is
truncated to the second, in UTC, before it is written, as the API server keeps it, so that what the poller holds is what is
stored and a poll that finds the same thing writes nothing.

### The client refuses what it cannot compare or write

The client of the endpoint refuses as `ReportUnreadable`, like a report of another version, one whose `templateHash` is not 64
lowercase hexadecimal digits, whose `lastSynced.commit` is not the full name of a Git object (40 or 64 lowercase hexadecimal
digits), whose `lastSynced.by` is neither `Pull` nor `Push`, or whose `lastSynced` has a commit and no time, which would clear
`status.lastSyncTime`. The hash is compared by equality, and the commit is written where everyone who can read the Treebind reads
it. The message is fixed and holds none of the values (ADR 0015). Such a report counts as a failed poll, so `AgentUnreachable` says
`ReportUnreadable` after 3 minutes of them. A report without these fields, from an agent of the release before, is read as before,
and says `AgentOutdated`.

The poller writes `AgentUnreachable`, `Synced`, `Conflict` and the two fields in one status update with the resourceVersion it
read, only when one of them changed. A conflict with the reconcile of the spec is tried again a second later and loses none of
them. The write wakes the reconcile of the spec, which watches the Treebind with no predicate, so `Ready` is computed again, and
the reconcile writes only when something that it owns changed. The poller's own watch keeps its predicate, so neither write wakes
it.

### Not here

- **Events and metrics** are #47, as ADR 0004 says. The function that makes the conditions is the one place where an outcome
  changes, so an event hung there is one for each change and not one for each poll. It needs permission to create events wherever
  the Treebinds are, which touches the roles of the operator and `docs/rbac.md`.
- **The adapters' own reporting** is #74. The `Conflict` condition is no longer held for it.
- **A wait before `Synced` turns `False`.** `AgentUnreachable` waits 3 minutes because a failed poll says nothing about the
  agent. A failed step is a fact about the sync, and writes only on change keep a failure that lasts to one write. The flapping
  that a wait would hide is the one of `Ready`, which the section above settles.
- **A printer column for the commit.** `kubectl describe` shows the field, and the columns are a change of their own.
- **A condition for local changes that wait**, and failures injected on a cluster (#55).

## Consequences

- `kubectl describe treebind` shows, after a change in either direction, `Synced` `True` with `InSync`, `Conflict` `False` with
  `NoConflict`, the full id of the commit and the time, a poll after the agent has made the change: 30 seconds, a fifth more or
  less, and a minute after a poll that failed. The end-to-end spec of the definition of done reads them with `kubectl`, and CI
  runs it.
- A rejected push, a `Diverged` pull and a refused update are `Synced` `False` with their reasons, and `Ready` is `False` with
  `NotSynced` for the ones that wait for a person. Those that a Git host's failure causes leave `Ready` `True`.
- Every sync pod is replaced once when the operator is upgraded to the release that has this record. After that no pod is
  replaced for a spec change that the agent does not see.
- The conditions are as old as the last poll, and `Synced` is `Unknown` after a restart of the agent until the poll that follows
  its first pull or push, so a Treebind whose sync pod restarts often is `Unknown` for a few polls each time. `lastSyncedCommit`
  and `lastSyncTime` keep the last agreement through it.
- A `Synced` `True` does not say that the volume has no file that the repository lacks. A person who needs to know whether
  something waits reads the `Changes`, `Commit` and `Push` checks, and `status.lastSyncedCommit` against the tip.
- The operator and the agent have to be of one release for the report to be read in full: an agent of the release before is
  `AgentOutdated`, an agent of a later one that gives a reason that the operator does not define is `SyncFailed`, and a report of
  the same version with a field of another spelling is `ReportUnreadable`. The first two are meant, and the last is what
  ADR 0011's "fields are only added" is there to keep from happening.
- ADRs 0004, 0011, 0015, 0030, 0033 and 0034 say that this record extends them. Where they differ, this record governs: ADR 0004's
  words for `Synced` `True`, `Conflict` `True` and the rule of `Ready`; ADR 0011's generation in the report; ADR 0015's
  generation of a report and its remark that no state of the sync is written yet; ADR 0030's pointer to the `Synced` of the
  report; ADR 0033's and 0034's pointers to #74 for the conditions of a rejection and of a conflict.
- Not tried on a cluster: a push that the host rejects, a conflict that cannot be resolved and an update that is refused, with
  what `Ready` shows for each; the upgrade of a running operator, in which every sync pod is replaced once; and how many status
  writes a busy Treebind makes, which is expected to be one for each new commit and one for each change of an outcome.
- `SyncPending`, `AgentOutdated`, `InSync`, `SyncFailed`, `NoConflict` and the 28 reasons of a failed step are API, and so is the
  rule of which of them make `Ready` `False`.
