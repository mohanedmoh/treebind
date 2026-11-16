# 0029. The agent looks for local changes with a scan, and events only make it scan sooner

- Status: accepted
- Date: 2026-11-16
- Issue: #34

## Context

FR-3 asks the agent to detect what the application writes by file events plus a periodic scan, and to commit
it after a settle time. R-11 is the risk behind the scan: on a network file system another client's writes make
no event at all, so a program that trusts events never sees them. R-10 is the one behind the settle time: a file
that is still being written must not be committed half done. NFR-17 asks that every write of the agent is made
after the writer lock was checked.

Git runs only through the Runner (ADR 0013). ADR 0022 decided that the agent, and not a pathspec, says which
files are synced, from one `git status`. ADR 0024 keeps what asks the repository to one operation at a time
and says that a push, when there is one, is made in the loop of the pulls and not beside it. #36 commits and #37
pushes, and neither is here: this decision is about finding the changes, and about when they are done.

## Decision

### The scan decides

A scan is one `git status --porcelain=v2 -z --untracked-files=all --no-renames` through the Runner. Its list goes
through the filter of ADR 0022 and what remains is the list of changes: a file added, modified, deleted or
changed in type, from the modes that status gives for the commit and for the directory. `--untracked-files=all`
lists the files of a new directory and not the directory, and `--no-renames` lists a file that moved as one
removed and one added, which is what a commit of the files by name stages, and which a move that a killed
commit left staged would otherwise hide as an entry of another kind. A path that is in neither the commit nor
the directory, which is what a file that was staged and removed leaves, is no change. An unmerged path, which a
merge that was not finished leaves, is a failure of the scan and not a guess, and so is a list that is longer
than the Runner's limit on output, and a Git directory that holds more than the agent put there, with the
reason of the checkout for that.

What the filter leaves out is counted, by the rule that does, for the report and not listed. When the state
directory is in the work tree, which it is when the whole claim is synced, the scan excludes it from the walk
with a pathspec. Without that Git lists every file of the Git directory as an untracked file at every scan: 30,025
lines in a test, and more with every object the agent writes.

A scan only reads. A plain `git status` refreshes the index and writes it back under `index.lock` (tried, Git
2.34.1: the index file changes after a status of files that were only touched), which is a write outside the check of
the writer lock, and a kill in the middle of it leaves a lock behind. The Runner sets `GIT_OPTIONAL_LOCKS=0`
for every command (ADR 0013), which makes status a read. A test on a real repository holds that a scan leaves
`index` as it was, and that a plain status does not, so that the test can fail. A stale `index.lock` does not stop
a scan, which takes no lock.

The cost is a status that cannot refresh its stat cache. On 100,000 files (Git 2.34.1, one machine) a scan took
0.15 to 0.4 s when the index was current and 0.8 to 1.0 s when every file's stat data was stale, and ten changes
took 0.3 s.

### What the scan saw

Each change carries what the scan saw of the file after Git listed it: the mode (a link is not followed), the size,
the modification time, the change time and the inode, from `Lstat`. They are what Git's own index compares. Two
scans that see the same thing saw a file that was not written between them. The change time catches a file that
was rewritten with its modification time set back, as `rsync -t` does, which a program can do and cannot do to
the change time. Where a volume does not keep the change time as it should, the modification time and the inode
are what is left. These values are compared only with one another, between scans. A time that a file server
reports is never compared with the agent's clock.

### Settle

A file settles when what the scan saw of it has not changed for `spec.sync.settleTime`, measured on the agent's
own clock between the scans that saw it, and not from times that the volume reports. Each file settles on its own,
so a file that keeps changing, a log, waits and does not hold back the files that have stopped. The price is that
what an application writes over a few seconds can be split over more than one commit, which is taken over a
commit that never comes. A deletion settles like any other change, so the delete half of an atomic save, which is
followed by the file again, is not committed. A change whose kind changes with the same sight of the file, which a
pull that moves the commit under it can do, starts its quiet period again. A file that is no longer a change is
forgotten.

The tracker also says how long until the first waiting file settles, and the loop scans again then, and not only
at the interval.

### Events

`internal/agent/watch` puts one inotify watch on each directory of the synced directory, because inotify does not look
into the directories of the one it watches, using `fsnotify`, which controller-runtime already brought into the
module and which the agent now links. It gives one kind of signal, "look at the directory". An event never says what
changed and is not trusted to say that nothing did: it only makes the scan sooner, so a missed event delays a commit by
up to one scan interval and never loses a change. The files that a pull writes are in the commit, so a scan does not see
them as changes, and a pull cannot echo into a commit.

A directory is not watched when nothing below it is ever synced by the name of the directory alone: a segment that is
`.git`, the state directory, a name that is a secret (`Filter.SkipsDir`). The patterns of the spec are not asked: a
pattern can take some of the files of a directory and not others, and what a directory holds is for the scan.

A directory that appears is watched before the signal for it is given, so that a file that is written after the signal
is not missed, and the files that were made in it before it was watched are the scan's. The watcher says when it cannot
do all of it: the limit of the kernel on watches (`fs.inotify.max_user_watches`, which the node sets and the pod cannot)
when it is reached, a directory that cannot be watched, and events that were lost, after which it looks at every directory
again. In each case the answer is a scan, and the report says so. The kernel takes the watch of a directory that is renamed,
and the watch that is made for the new name at the same moment can go with it, without a word, which a test showed. So the
loop calls `Refresh` at every scan, which watches the directories that have no watch, and what a rename costs is what a
missed event costs: up to one scan interval.

### The loop

The scans are a second kind of turn of the loop of the pulls (`syncloop`), so they never run beside a pull. The first scan
is at once, and each next one is a scan interval after the end of the one before, or sooner: when the scan says that a file
settles before that, and when an event calls for one, after a gap of one second since the last scan ended, which keeps a
program that writes a great deal from being scanned all the time, and a large tree from being scanned again as soon as one scan
ends: the scan that a settling file calls for is not sooner than the gap either. The events that come while a scan runs are one
scan after it. The loop walks the directories for the watches that they lack once in a scan interval, at the scan of the
interval, and not at the scans that events call for. A pull and a scan that are due together are taken pull first. Scans
are local, so they have no jitter and no backoff. The scan interval is 30 seconds, a default of the agent with no field: the
volume probe (#101, #102) is to choose it, and until then it is what R-11 asks to be documented.

The scans run only for a Treebind whose direction is `bidirectional`. A Treebind that only pulls never looks at what the
application writes.

A loop with scans needs a clock that says what time it is (`syncloop.WallClock`), to tell how much is left of a wait that an
event cut short.

### The setting

The agent reads `TREEBIND_SYNC_SETTLE_TIME`, a Go duration of at least a second, with the default of the CRD, 5s, for one that
is not set. The operator passes `spec.sync.settleTime` when the spec has one, which the API server always fills in, so each sync
pod is replaced once when this operator is installed, as it was for the interval and the direction (ADR 0021), and again when a
Treebind changes it. A test holds the API server and the agent to the same settle times.

### The report

A check `Changes`, after `Pull`, for a Treebind that syncs in both directions. It is skipped, with `NotChecked` and the reason,
until the checkout is made and while the first scan runs. A scan that worked is `Passed`, and says how many files changed and how
many of them have settled, what the filter left out by reason, and what the events cannot do. A scan that did not is `Failed`, with
`ScanFailed`, or with the reason of the checkout for a Git directory that holds more than the agent put there, and what went wrong.
The message holds no time and no count of the directories that are watched, so that a scan that finds the same again is not a new
line of the log, which has a line when what the check says changes.

## Consequences

- Nothing commits the settled changes yet (#36), and the message says so. `Options.Settled` of the agent is where the commit goes; it
  is called with the changes that are settled at each scan that finds them still there, and is none until then.
- Each sync pod is replaced once when the operator that passes the setting is installed.
- On a tree with more directories than the kernel's limit on watches, the agent scans on its interval for the rest, and says which.
  The limit is the node's, and is not set from the pod.
- On NFS and CephFS another client's writes make no event, so the scan is the only thing that finds them there. This follows from how
  inotify works and has not been tried here: the storage conformance runs (#108, #109) are to confirm it.
- The filter is made with case sensitive matching. A volume that folds case (#101) is to say so, and the filter has the option for it.
- A file that a killed commit left staged, and that was then put back by hand to what the commit has, is listed as a change, because
  status says only that the index differs from the commit. Resetting the index after a crash is the recovery of #39.
- A change of a file that the scan sees while it is written is seen again, with other sizes and times, at the next scan, and settles
  later: a file that is written every second never settles, and is committed when it stops.
- Not decided here, and left to the issues that need them: the commit (#36), the push (#37), the conflict policies (#38), the breaker
  against a pull that is committed back (#41), a scan for secrets (#65), live database files (#94), and metrics (#47).
