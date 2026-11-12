# 0009. Volume preflight checks and their reasons

- Status: accepted
- Date: 2026-11-12
- Issue: #31

## Context

FR-20 asks that the agent checks, before its first sync, that the claim is
mounted and writable as the agent's own user, and that it reports each failure as
its own condition. Without such a check, a volume the agent cannot write to shows
up later as a failed commit or a failed pull, far from its cause (R-12).

The agent holds no Kubernetes credentials (ADR 0004). A condition can only come
from what it reports, through the status endpoint (#104) that the poller (#99)
reads. Neither exists yet, and the agent is still a stub. The Git side of FR-20
is not ready either: Git has to run with the agent's final configuration
(`safe.directory` per command, #81) against the final checkout location (#45,
#32), and the sync image has no Git until #84.

## Decision

The checks are a package that the agent will call once at start, before it
touches the volume. It is not wired into the agent yet. Two checks run, in this
order:

- `Mount`: the path exists, is a directory and is on a mounted volume, not on
  the container's own filesystem. A path on the container's filesystem means the
  claim is not mounted there, so what the agent wrote would be lost with the
  container and the application would not see it.
- `Writable`: the agent's own user creates, writes and removes a hidden file
  named `.treebind-preflight-*` in the directory.

Each check returns a result with its name, an outcome, a reason and a message.
The outcome is `Passed`, `Failed` or `Skipped`. A check never refuses a setup
that works: one that cannot tell, because the mount table cannot be read or the
mount check failed, is `Skipped`, which is not a failure. A volume that does not
answer within 30 seconds fails with `TimedOut`, so a network file system that is
down looks like a failed check and not a hung agent.

The reasons are API in the sense of ADR 0004, because the poller will use them
as the reasons of conditions and users and tooling will match on them. Each
distinct cause has its own and none is reused:

| Reason | Check | Means |
| :-- | :-- | :-- |
| `MountPathInvalid` | Mount | the path does not exist, cannot be read or is not a directory |
| `NotMounted` | Mount | the path is on the container's own filesystem |
| `NotWritable` | Writable | the agent's user lacks the permission to create a file |
| `ReadOnlyVolume` | Writable | the volume refuses writes |
| `WriteFailed` | Writable | anything else, such as a full volume or an operation not permitted |
| `TimedOut` | either | the volume did not answer in time |
| `NotChecked` | either | the check was skipped, and the message says why |

`NotWritable` is only a permission error that the owner and mode of the
directory explain. An operation that is not permitted, which an immutable or
append-only directory returns, is not cured by changing them, so it is a
`WriteFailed`.

Messages hold no credential and name no storage server: the mount check reads
the mount point, the type and the mode of a mount, and leaves out its source. A
`NotWritable` message names the user and groups the agent runs as and the owner
and mode of the directory, which is what someone needs to fix it.

The reasons of the volume checks are meant as the reasons of the `VolumeReady`
condition that #100 and #103 already name: `Failed` as `False`, `Skipped` as
`Unknown`. The condition itself, its owner, and whether it can make `Ready`
`False` are decided with the poller (#99), as ADR 0004 requires of a new
condition. The sync pod is not ready before its first sync (#48), so `Ready` is
already `False` while a check fails; the condition would say why.

## Consequences

- Nothing calls the package yet. Wiring it into the agent comes with the status
  endpoint (#104), where a failed check does not have to end the agent: it can
  stay up, report the failure and run the checks again.
- The Git ownership check and the reachability probe (#77) are not here. Git
  ownership follows #81, #45, #32 and #84.
- The volume probe (#101) extends the same package with more checks of the same
  shape.
- The mount check reads `/proc/self/mountinfo`, so it can only tell on Linux,
  where the agent runs. Elsewhere it is skipped.
- The hidden file is created and removed once at start, which can wake a program
  that watches the directory. A crash between the two leaves one file behind. The
  prefix `.treebind-preflight-` is reserved, so the adoption rules (#32) and the
  scan (#34, #43) can ignore a file that carries it.
