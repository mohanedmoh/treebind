# 0043. The agent packs the objects of its Git directory with a repack under the writer lock

- Status: accepted
- Date: 2026-11-27
- Issue: #289
- Supersedes: the part of [ADR 0013](0013-git-runs-only-through-a-runner-with-a-fixed-configuration.md) that leaves the
  Git directory without any maintenance: `repack` is an allowed subcommand now, with `-n`. `gc` and `maintenance` stay
  out, and `gc.auto=0` and `maintenance.auto=false` stay
- Extends: [ADR 0028](0028-the-agent-stops-git-gently-and-removes-the-lock-files-that-a-stopped-git-leaves.md), whose removal
  of the temporary files of the object store takes the files that a killed repack leaves as well

## Context

The Runner sets `gc.auto=0` and `maintenance.auto=false` and does not allow `gc` or `maintenance` (ADR 0013): a gc detaches
from the call and from its process group, and it packs the refs into `packed-refs`, which the check of the Git directory
refuses (ADR 0019) and which ADR 0026 counts on never appearing. So nothing packs or prunes, and every commit, fetch and
resolution adds loose objects, each a file of its own, which takes at least a block of the volume and an inode.

### What was measured

The real pull, scan, commit and push of the agent (`internal/agent/loops/growth_test.go`), on a local bare repository over
Git's ssh transport with a stand-in for ssh, as the tests of the loops do, with two Treebinds on the repository and the time
of the loop compressed: each step runs at once, and a minute of sync is the steps that the defaults allow in a minute.

- **The rate.** `sync.settleTime` is 5 seconds and the scan comes at a change, so an application that saves a file every
  few seconds has it settled and committed every 5 to 6 seconds. The breaker holds that to `commit.maxPerHour`, 60 by default
  (ADR 0035), so the defaults make **one commit a minute**, 1440 a day, and that is what was run. The pull comes every
  `sync.interval`, 30 seconds: two pulls a minute.
- **The workload.** 200 files of about 6 KB of JSON in 10 directories. Each minute the application of A changes three values
  in each of two of its 150 files, and A commits and pushes. Every other minute B, the other side, changes one of its 50 files,
  commits and pushes, before A, so that A's push is refused and its next pull resolves the conflict with `followRemote`, which
  makes a commit and keeps the commits of A under `refs/treebind/local`. Both sides change all the time: a week was 10,080
  commits of A, 5,040 resolutions and 5,040 commits of B.
- **What is measured** is the Git directory of A: the blocks that its files take (`st_blocks`), their size, their number, and
  what repacking, pruning and expiring reflogs takes back on a copy. Git 2.34.1, on ext4 with blocks of 4 KiB.

| After | On the volume | Files (inodes) | Loose objects | Packs | Reflogs |
| :-- | --: | --: | --: | --: | --: |
| the start | 0.28 MiB | 10 | 0 | 0.18 MiB | 0.01 MiB |
| a day | 53.4 MiB | 12,988 | 12,956 (50.6 MiB) | 0.18 MiB | 1.4 MiB |
| three days | 159.5 MiB | | | | |
| a week | 372.4 MiB | 90,567 | 90,535 (353.7 MiB) | 0.18 MiB | 9.9 MiB |

The growth is linear, 53 MiB and 13,000 inodes a day, and 95% of it is loose objects: a loose object of a few hundred bytes
takes a block of 4 KiB. A claim of 1 GiB is full in about 19 days, and its inodes, 65,536 on ext4 for that size, run out in
about five. What a maintenance would take back, on copies of the directory after a week:

| On the copy after a week | On the volume | Files | Packs | Reflogs |
| :-- | --: | --: | --: | --: |
| nothing done | 372.4 MiB | 90,567 | 0.18 MiB | 9.9 MiB |
| `repack -d`: the loose objects packed | 22.3 MiB | 34 | 12.3 MiB | 9.9 MiB |
| `repack -a -d`: one pack | 22.3 MiB | 32 | 12.3 MiB | 9.9 MiB |
| and `prune --expire=now` | 22.3 MiB | 32 | 12.3 MiB | 9.9 MiB |
| reflogs expired, `repack -a -d`, `prune` | 11.0 MiB | 32 | 10.8 MiB | 0 |

After a day the same rows were 53.4, 3.4, 3.4, 3.4 and 1.7 MiB. Packing takes back 94%. Pruning takes back nothing: every
object is reached by a ref or a reflog, because the history only grows and the reflogs are never expired. What is left grows
by about 3.2 MiB a day: the packed history, 1.7 MiB, which is about what the repository on the host grows by, and the reflogs,
1.4 MiB. The week was run, and not extrapolated from the day. The run with the maintenance is in the consequences.

## Decision

The agent packs the objects of its Git directory with `git repack`, in its loop, after a pull that passed.

### When

After each pull that passed, the agent looks at two directories, as `gc --auto` looks: `objects/17`, and the packs.

- More loose objects in `objects/17` than a 256th of 6700 rounded up, which is 27, so 28 or more, about 6,700 in all,
  `gc.auto`: the loose objects are packed.
- 50 packs that are not kept with a `.keep` file, `gc.autoPackLimit`: every pack is put in one.

At the measured rate that is a repack of the loose objects about every 12 hours, which bounds what loose objects take to
about 30 MiB and 6,700 inodes, and a repack of every pack after 50 of them, about 25 days. A pull comes every interval and the
look is two directory listings. A maintenance that failed, found no room, was held (below), left the limit reached, or found
fewer loose objects than the look at `objects/17` guessed, is looked for again an hour later, and not at the next pull.

A pull that failed is not followed by a maintenance, and that is a limit: while the pulls fail, because the host does not answer
or the volume refuses, the commits of the application go on adding loose objects, about 53 MiB and 13,000 inodes a day at the
measured rate, and nothing packs them until a pull passes again. That is what the agent did before this change, for as long
as the pulls fail.

### What runs

- With about 6,700 loose objects: `git repack -d -n -q --threads=1 --window-memory=32m`. It packs the loose objects that a ref,
  a reflog or the index reaches into a new pack, and then removes the loose objects that are in a pack (`prune-packed`).
- With 50 packs: the same with `-a --keep-unreachable`. It puts every object of every pack and every loose object in one pack,
  the objects that nothing reaches among them, and then removes the packs that it has made redundant and the loose objects.
- When the repack of the loose objects leaves as many as the limit, which are the loose objects that nothing reaches, it is
  followed at once by the repack of every pack into one, when the volume has the room for that, behind the fence again. Without
  the room the result says that the limit is still reached, and the agent looks again an hour later, and not at every pull.

Neither removes an object that is not in the pack it has just written, and neither touches a ref, so the commits that
`refs/treebind/recovery/*`, `refs/treebind/local/*`, a ref that a person made by hand and the reflogs keep are kept, and so is
every object that nothing reaches. `--keep-unreachable` is what makes that hold for the one pack: without it `repack -a -d`
drops the packed objects that nothing reaches, which a test shows. `prune`, `reflog expire`, `pack-refs`, `gc`, `maintenance`,
`commit-graph` and `multi-pack-index` are not run, and only `repack` is allowed.

`-n` leaves out `update-server-info`, which writes `info/refs` and `objects/info/packs`, and the check of the Git directory
refuses an `info` that is not empty. The Runner refuses a `repack` without `-n`, so that no caller can make the agent refuse its
own Git directory. The setting `repack.updateServerInfo=false` does not do it everywhere: Git 2.34.1 writes `info/refs` with it.

`--threads=1` and `--window-memory=32m` bound the memory of the search for deltas. Git's defaults are no bound and a search for
each processor, of up to ten blobs each, and a blob up to `core.bigFileThreshold`, 16 MiB in the Runner, is searched: more
than the 128 MiB of the sync container on a machine with many processors. On copies of the measured Git directory the repack of
every pack into one had a largest resident set, with what it started, of 65 MiB at day 7, 72 at day 8 and 81 at day 9, and
`repack -a -d` with Git's defaults 69, 75 and 85, on this machine; the repack of the loose objects 28 to 35 MiB. The memory grows
with the repository. Those figures were measured before the next two settings and were not measured again.

The Runner also sets, for every command, `pack.deltaCacheSize=16m` and `core.deltaBaseCacheLimit=16m`. `--window-memory` bounds
neither, and Git's defaults are 256 MiB for the deltas that `pack-objects` keeps until it writes them and 96 MiB for the bases of
deltas that Git keeps while it reads a pack, against the 128 MiB of the sync container. A repack, a fetch and a push use them. A
smaller cache makes Git compute again what it would have kept, which is slower and not wrong.

### Under the writer lock, in the loop, with a time limit

- It runs in the turn of the pull, so no pull, scan, commit or push runs beside it (ADR 0024, ADR 0029), and the fence of the
  writer lock is called immediately before the first write, as before every write of the agent (ADR 0023). A lock that is lost
  while it runs cancels it with the rest of the loop.
- It is a Git command of the Runner, which starts it in a process group of its own and stops the group, as for every command
  (ADR 0013, ADR 0028): a repack does not detach, and what it starts, `pack-objects`, is in the group. It has a timeout of 10
  minutes, which the Runner tells the liveness probe as it does for any command (ADR 0030). The loop waits for it; the heartbeat
  of the lock does not.
- It runs only when the Git directory has the room for what it writes: the new pack is written beside what it holds, which is
  removed after it. The room is the size of the files of the loose objects (a pack compresses them better than the loose files
  are, and finds deltas), 64 bytes for each of them for the indexes, 64 KiB, and a reserve of 1 MiB. For every pack in one it is
  that and the size of the packs. Without the room for every pack the agent packs the loose objects, and without the room for
  those it writes nothing, and the `Pull` check and the log say how much it needs and how much the volume has. The disk-space
  guard of #78 does not exist yet; this is the repack's own check.

### What a kill leaves

What a repack leaves when it is killed was tried on Git 2.34.1 by killing a real repack with SIGKILL at each of its moves
(strace injects the signal at the system call), and is in the tests:

| Killed | Left among the packs | The objects |
| :-- | :-- | :-- |
| `pack-objects` while it writes, or as it moves its files | `tmp_pack_*`, `tmp_idx_*`, or `.tmp-<pid>-pack-<hash>.pack` and a `tmp_idx_*` | all where they were |
| the repack as it moves the pack to its name | `.tmp-<pid>-pack-<hash>.pack` and `.idx`, a whole pack under another name | all where they were |
| the repack as it moves the index to its name, which it does last | `pack-<hash>.pack` without its index, and the index as `.tmp-<pid>-...idx` | all where they were |
| the repack while it removes the loose objects that it packed | nothing | in the new pack, and some loose as well |

Git reads a pack only through its index, and a repack removes what it made redundant only after its pack has its index, so
none of these loses an object or makes the repository unusable, and the check of the Git directory takes each (it does not look
at the files of `objects/pack`). The next repack removes none of them, and the second can be as large as the repository.
`git.RemoveStaleTempObjects` (ADR 0028) removes, besides the temporary files it removed before, a `.tmp-*` file among the packs
and a `.pack`, `.rev`, `.bitmap` or `.mtimes` of a pack without its `.idx`. A `.keep` and a `.promisor` are left. It is called
behind the fence before each repack and after one that failed while the agent holds the lock, and at a start for the files that
have not changed for 31 minutes, as before. A SIGTERM, which is how the agent stops a command, lets Git remove its own files.

An out-of-memory kill takes the agent with its Git, and a repack that needs more memory than the container has would be killed
at every start. The agent writes `.treebind/maintaining`, with the time, before a repack and removes it after, behind the fence:
an agent that has lost its lock leaves it. When it finds the file at a maintenance, the repack was cut off with the agent, and
no maintenance is made until a day after that time; the `Pull` check says so. The out-of-memory killer can also take only the
repack or the `pack-objects` that it started. The agent kills a command only when its context ended, which the Runner says as a
cancellation or a timeout, so a repack that ended for a SIGKILL, or that says that `pack-objects` died of signal 9, was killed by
something else: the agent leaves the file then, and holds the maintenance a day as well. A maintenance that is held removes,
behind the fence, what the repack that was cut off left among the objects, which can keep the volume full, and which the start
removes only after 31 minutes.

### What the agent says

What a maintenance did is at the end of the message of the `Pull` check, and in the log, at `INFO` when it packed and at `WARN`
when it failed, found no room, was held or left the limit reached: `packed 6712 loose objects of the Git directory: its objects
took 26.2 MiB and take 2.1 MiB`. The count is of the objects that the repack packed. What a maintenance that could not be made
said stays at the end of the message of every `Pull` check, of a pull that failed too, until a maintenance packs or finds nothing
to pack. The note names ten of the files that were removed at most. A maintenance never fails the pull, and has no check or
reason of its own.

## Consequences

- The measured workload was run again with the maintenance, for 9 days and not 14: the run was stopped at day 9. In those 9
  days the agent packed the loose objects 16 times and never put every pack in one (16 packs, below 50). The Git directory took
  between 18 and 51 MiB on the volume and between 750 and 7,300 files: 25 MiB at day 1, 25 MiB and 748 files at day 7, 45 MiB
  and 4,236 files at day 9, of which 14.6 MiB packs, 12.7 MiB reflogs and 16.3 MiB loose objects that wait to be packed. What is
  not taken back, the packs and the reflogs, grew by about 2.9 MiB a day. Extrapolated, not measured: at 14 days about 42 MiB of
  packs and reflogs and at most about 30 MiB of loose objects, so at most about 75 MiB, against about 745 MiB without the
  maintenance. It levels off in the files and the loose objects, and not in the history and the reflogs, below.
- What is left grows with the history and with the reflogs, about 3.2 MiB a day at the measured rate. The history is in the
  repository on the host as well and cannot be taken off the claim without a shallow repository, which the check of the Git
  directory refuses. The reflogs could be expired, `reflog expire`, which would bound them at the days kept; that is not done
  here, because nothing of the agent reads them and an expiry is a removal that this change does not need to make.
- `docs/git-directory.md` says how much room the Git directory needs.
- A Treebind whose `maxPerHour` is raised commits more, and its loose objects reach the limit sooner: the bound of what they take
  stays about 30 MiB.
- An agent of an older release that ran for a while has every object loose. Its first repack packs them, and needs the room for
  the size of their files, about a sixth of what they take on the volume at the measured workload (the files of a week were 64
  MiB, and took 372 MiB). A claim that does not have it says so in the `Pull` check, and gets more room.
- The loop is held while a repack runs. The repacks of the loose objects of the measured workload took 1.1 to 1.6 seconds, and 6.3 at most; the
  repack of every pack is longer the larger the repository, and the 10 minutes are its bound.
- Not tried: Git 2.56, the Git of the agent image, on which the names of the temporary files and the order of the moves are the
  same in its source as far as the tests see; the tests that kill a repack need `strace` and the permission to trace, and skip
  without them, which the agent image does not have. The memory of a repack of a large repository, of hundreds of thousands of
  objects, against the 128 MiB of the sync container, was not measured. NFS and CephFS were not tried.
