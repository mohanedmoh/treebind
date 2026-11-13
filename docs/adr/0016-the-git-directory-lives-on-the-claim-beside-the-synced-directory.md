# 0016. The Git directory lives on the claim, beside the synced directory

- Status: accepted
- Date: 2026-11-13
- Issue: #45

## Context

The agent runs Git in a checkout whose work tree is on the claim, and the application
mounts the same claim. A Git directory that the application can write is a way to run
code with the agent's credentials: a hook, `core.sshCommand`, a filter, a rewrite of
the URL, `info/attributes`. It also lets the application read the history (R-17). The
Runner that every Git call goes through (#81) switches off nearly everything that Git
can be told to run, but a few keys cannot be listed or reset, and it relies on the
application not being able to write the Git directory. Where the Git directory lives has
to be settled before the sync code clones (#32).

Three places were weighed: on the claim beside the synced directory, in the pod's own
`emptyDir`, and on a claim of its own through `volumeClaimTemplates`.

## Decision

### The layout

The sync pod mounts the whole claim at `target.mountPath`, as it does today. The agent
keeps its own state in the directory `.treebind` at the root of the claim, the Git
directory in `.treebind/git`, and syncs `<mountPath>/<target.subPath>`, or the whole
claim when there is no `subPath`.

With a `subPath`, `.treebind` is beside the synced directory and not in it. An
application that mounts only that `subPath` never sees the Git directory, its config or
its hooks, and the history is not exposed to it. `target.subPath` cannot be `.treebind`
or start with it, which the CRD refuses next to its rule against `.git`. The agent
refuses the same values, and a test holds the two sets of rules together: a value that
the API server accepts and the agent refuses would leave a sync pod that exits at start
for ever.

### Without a `subPath`

The whole claim is the work tree, so `.treebind` is inside it and the application can see
it. This is the weaker case, and the documentation says to use a `subPath`. Three things
stand in for the structure:

- `.treebind` and everything the agent makes in it are mode 0700 and owned by the
  agent's user, so on a filesystem that honours Unix permissions an application that
  runs as another non-root user can neither read nor write it.
- `.treebind` is never staged: the layout names it as the one path the sync must leave
  out.
- Git is not run on a config that has keys outside a short list. That check needs the
  Runner, so it follows #81.

### The application's side

Nothing in the cluster can make an application mount only the `subPath`, because that
mount is in the user's own pod. It is a requirement that the documentation states with
an example, and the agent never trusts what it finds in `.treebind`. At start it checks
that `.treebind` and `git` are real directories and not links, owned by the agent's user
and closed to group and others, and that it reached them from the claim root without
following a link. If not, a preflight check `GitDir` fails with its own reason and Git is
not run against them. That check is the next change.

The `subPath` mount in the application is a bind mount that the kubelet makes in the
application's pod, and some platforms advise against such mounts, where the kubelet runs in a
container and can be restarted or the volume under a running pod can be remounted. Treebind's
own pod never uses one: it mounts the whole claim. Where the application cannot use one, two
other ways keep `.treebind` out of its reach, and neither needs a change in Treebind: a
volume whose root is the synced directory, such as a static PersistentVolume with that path,
mounted whole by the application, or no `subPath` and the protection above.

### What the operator and the agent share

The operator passes `target.subPath` to the agent as `TREEBIND_SUB_PATH`, and leaves the
variable out when there is none, so the pod template of a Treebind without a `subPath`
does not change. The agent reads it without trimming, because the CRD accepts a path
with spaces and the operator passes it on as it is. The layout, a package that only works
out paths, is the one place that says where everything is.

### The two places that were not taken

- **The pod's own `emptyDir`** is safe by construction and needs nothing from the user,
  but it is lost whenever the pod is replaced: an operator upgrade, a node drain. The
  agent would have to fetch everything again and still tell the application's edits from
  the remote's changes, which needs the last synced commit from somewhere durable. The
  recovery ref before every pull (#40) would not survive, and that ref is what saves the
  application's data after a remote that was force-pushed.
- **A claim of its own** is safe and durable and needs nothing from the user, but it
  needs dynamic provisioning, so a cluster with static volumes only would leave the pod
  pending, and a StatefulSet cannot change its `volumeClaimTemplates` after it is made,
  which makes upgrading the operator awkward.

## Consequences

- A Treebind should have a `subPath`, and the application should mount only that, or a
  volume whose root is that directory. A Treebind without one works, with the weaker
  protection above.
- `target.subPath` is a documented part of the layout, and the API refuses `.treebind`.
  The API is `v1alpha1`.
- Only a Treebind with a `subPath` changes its pod template, so only its sync pod is
  replaced once when the operator is upgraded (#87).
- The agent keeps the claim for its own state as well as the application's files. The
  Git directory grows with the history, so the claim needs room for it (#50).
- Still to do: making the directories and checking them with the `GitDir` preflight check
  (the next change), refusing a Git config with unexpected keys (after #81), and using the
  layout to run Git, with `AllowGitDirInWorkTree` set when the Git directory is in the work
  tree and `.treebind` left out of what is staged (#32).
