# Pod Security and file ownership

Treebind runs under the `restricted` Pod Security level. This page says what its
two pods set, and what a volume needs when a user other than the agent has
written to it.

## What the pods set

The operator's Deployment and the sync pod that the operator creates for each
Treebind use the same settings, and both are admitted in a namespace that
enforces `pod-security.kubernetes.io/enforce=restricted`:

| Setting | Value |
| :-- | :-- |
| `runAsNonRoot` | `true` |
| `seccompProfile` | `RuntimeDefault` |
| `allowPrivilegeEscalation` | `false` |
| `capabilities` | drop `ALL` |
| `readOnlyRootFilesystem` | `true` |

Neither pod sets `runAsUser`, `runAsGroup` or `fsGroup`, so each container runs
as the user its image names, unless the platform gives the pod another. The
operator image is built on distroless `nonroot`, which is UID 65532 and GID 65532.
The agent image is built on Chainguard's `git` image, which runs as the user `git`,
also UID 65532 and GID 65532. It carries Git and OpenSSH. What is in the image and
how it is kept current is in [releasing](releasing.md#the-agent-image).

## Any user ID

On a platform that gives the pod a user ID of its own, such as the user ID from the
range of the namespace with group 0 that OpenShift's `restricted-v2` assigns, ssh
does not run in the sync pod: it refuses a user that has no passwd entry
(`No user exists for uid ...`), and the image has one only for 65532. `Synced` then
says that the repository cannot be asked for its branches. Tell the operator to give
the sync pods a passwd file for their user:

- with the chart, set `agent.arbitraryUserID=true`;
- with `install.yaml`, set the variable `AGENT_ARBITRARY_USER_ID` of the operator's
  Deployment to `"true"`, in the file or afterwards:
  `kubectl -n treebind-system set env deployment/treebind-controller-manager AGENT_ARBITRARY_USER_ID=true`.

Each sync pod then has an init container, `passwd`, of the agent image. It runs as
the same user, before the agent, and writes a copy of the image's `/etc/passwd`, with
an entry for its user ID when the copy has none, into an `emptyDir`. The sync container
sees that file at `/etc/passwd`, read-only. Both containers start in `/`. The reasons
are in [ADR 0042](adr/0042-the-sync-pod-can-write-a-passwd-entry-for-its-user-in-an-init-container.md).

What to know before you turn it on:

- Turning it on, or off again, changes the pod template of every Treebind, so every
  sync pod is replaced once ([upgrading the operator](agent-status.md#upgrading-the-operator)).
- With it on, the sync pods have an `emptyDir`, so `kubectl drain` refuses them unless
  you pass `--delete-emptydir-data`. Nothing of value is lost: the file is written
  again when the pod starts. The pods carry the annotation
  `cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes: passwd`, so the
  cluster autoscaler may still take their node down.
- It needs an agent image of the release that brought it or newer. An older agent
  ignores the arguments of the init container and starts as the agent, so the init
  container never finishes and the sync pod never starts. That matters when you pin
  `agent.image` or `AGENT_IMAGE` to a digest or a mirror, and when you upgrade with
  `helm upgrade --reuse-values`, which keeps the agent image of the release before.

The agent writes only to the claim. Git and ssh read no home directory and write
nothing to `/tmp`, so the sync container has no volume for either, and its root
filesystem stays read-only.

### When the user ID changes

The files the agent makes belong to the user ID it runs as. Its state directory,
`.treebind` at the root of the claim, is private to that user (mode 0700). If the
pod later runs as another user ID, after a move to a platform that assigns one or a
change of the user, the agent refuses the directory with `GitDirUnsafe`, and the
message names both users. Give the directory to the new user, from a pod that mounts
the whole claim and runs as root, because only root may give a file to another user
(on OpenShift that takes a security context constraint such as `anyuid`):

```sh
chown -R <new uid>:<new gid> "$MOUNT/.treebind"
```

The agent then carries on with the checkout as it was. Removing `.treebind` instead
also clears the check, but it loses the commits that were not pushed, and the agent
makes the checkout as on a first start: it takes the files of the synced directory
when they match the branch and refuses them with `DirectoryNotEmpty` when they do not
([the Git directory](git-directory.md#what-the-agent-checks)). The files that it
synced are group-writable, as below, so they stay writable for a user in the same
group.

`spec.agent.runAsUser` changes the user ID of the sync pod, and the same applies:
see [the overrides of `spec.agent`](#the-overrides-of-specagent).

## The overrides of spec.agent

A Treebind can change some of its sync pod with `spec.agent`. What the operator sets
itself stays: `runAsNonRoot`, the seccomp profile, the security context of the
container, the labels that select the pod and the annotations the operator puts on
it. [ADR 0044](adr/0044-the-operator-applies-spec-agent-to-the-sync-pod-except-fsgroup.md)
has the reasons. Each field that is set changes the pod template, so the sync pod is
replaced once when you set or change one.

| Field | What it does |
| :-- | :-- |
| `resources` | The requests and limits of the sync container. Each one takes the place of the default of its resource (requests of `10m` CPU and `32Mi` memory, a limit of `128Mi` memory). A default limit below a request that you set is raised to it, and a default request above a limit that you set is lowered to it. A request above a limit, both set, is refused by the API server. `claims` is not applied |
| `nodeSelector`, `tolerations`, `priorityClassName`, `imagePullSecrets` | Those of the pod, as set |
| `labels`, `annotations` | Added to the pod. A key that the operator sets keeps the operator's value |
| `runAsUser`, `runAsGroup` | The user and the group of the pod, the init container's too. `0` is refused (below) |
| `fsGroup` | Not applied yet, and reported in `SpecSupported` (below) |

- **A user other than 65532**, the image's, gives that sync pod the `passwd` init
  container of [any user ID](#any-user-id), whatever the operator's setting, because
  ssh refuses a user without a passwd entry. Its `emptyDir` means that `kubectl drain`
  of the node needs `--delete-emptydir-data`.
- **`runAsUser: 0`** gets no sync pod: the kubelet would refuse to start a pod that
  is non-root as root. `Ready` is `False` with `UnsupportedAgentUser`, the operator
  creates or changes no sync object, and a sync pod that the Treebind had runs on as
  it was until you set another user.
- **A change of the user** on a claim that the agent has used is refused by the
  agent with `GitDirUnsafe`, because its state directory belongs to the user before:
  see [when the user ID changes](#when-the-user-id-changes).
- **`fsGroup`** is not applied. For a volume that supports it, the kubelet then gives
  every file of the claim the group, with group read and write, also the agent's own
  `.treebind`, and the agent refuses its state directory once it is open to the group.
  [ADR 0044](adr/0044-the-operator-applies-spec-agent-to-the-sync-pod-except-fsgroup.md)
  has what was found. Give the claim its group on the storage side, as below.
- **What the API server refuses**, such as a label value that a label cannot have,
  makes `Ready` `False` with `ReconcileFailed` and the API server's words. A pod that
  is refused only when it is made, for a priority class that does not exist or a
  quota, shows as `FailedCreate` in the events of the StatefulSet
  ([what you see](agent-status.md)).

## The SSH key

The Secret with the SSH key is mounted at `/etc/treebind/ssh` with mode `0444`.
The kubelet makes Secret files owned by root, and the agent runs as a non-root
user without an `fsGroup`, so a stricter mode such as `0400` would leave the
agent unable to read its key. OpenSSH accepts a key that root owns even when
others can read it. The file is on a memory-backed volume that only the agent
container mounts. The reasons and the alternatives are in
[ADR 0007](adr/0007-ssh-key-is-mounted-readable-by-the-agent.md).

## What the volume needs

The agent writes to the claim as UID 65532 and GID 65532, or as the user and
group that the platform gives the pod, with umask 0002, so the files and
directories it creates are group-writable (`0664` and `0775`). On a platform that
assigns user IDs, read the user ID of the pod where this section says UID 65532, and
see below for the group.

For a claim that your application wrote to first:

- The agent must be able to write to the mount point. A new volume is often owned
  by root and not group-writable, so the agent gets "permission denied" on its
  first write. Make the root of the volume owned by 65532, or writable by a
  group the agent is in.
- Files your application wrote must be writable by UID 65532 or GID 65532, or the
  agent cannot update them when the remote changes.

For your application's pod:

- Run it with `fsGroup: 65532`. Kubernetes then makes the volume group-owned by
  that GID, so the application can change what the agent wrote. On drivers that
  honour it, the volume's directories are also setgid, so new files keep the
  group. Not every storage driver supports this, NFS for one: on those the
  setting has no effect, so set the owner, the group and the mode on the storage
  side instead.
- Give the application a umask of 0002 as well, or have it create files with
  group write. With the default 0022 the agent cannot change what the
  application creates, even with the same group.

On a platform that assigns the user ID with group 0, such as OpenShift's
`restricted-v2`, do not set `fsGroup: 65532`, and do not set `fsGroup: 0` either:
`restricted-v2` refuses both. It gives a pod that sets no `fsGroup` the first group of
the range of the namespace, the annotation `openshift.io/sa.scc.supplemental-groups`,
so leave `fsGroup` out of your application's pod, and the sync pod sets none. Both pods
of the namespace then share that group on the volume. If you set one, take a value of
that range.

See the Kubernetes documentation on
[configuring volume permissions](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#configure-volume-permission-and-ownership-change-policy-for-pods)
for how `fsGroup` and `fsGroupChangePolicy` behave on your storage driver.

## What the agent checks of the volume

When the sync pod starts, the agent checks that it can write to the volume as the
user above: it creates, writes to and removes a hidden file at the mount point of
the claim (the check `Writable`). A claim that is mounted read-only fails it with
`ReadOnlyVolume`, a directory that the agent's user cannot write to with
`NotWritable`, and any other failure with `WriteFailed`. The message of
`NotWritable` names the user, and the owner and mode of the directory. The agent
stays up and checks again, with a longer wait each time up to a minute, so it
carries on once you put the volume right. The operator shows a failed check in the
condition `Synced` ([the conditions](agent-status.md#synced), [the checks of the
report](agent-status.md#what-the-report-holds)).

Git's refusal of a directory that another user owns does not stop the agent: every
Git command of the agent sets `safe.directory` to the work tree of the checkout,
and to nothing else
([ADR 0013](adr/0013-git-runs-only-through-a-runner-with-a-fixed-configuration.md)).

## What is still to come

- `spec.agent.fsGroup` (FR-28, [#96](https://github.com/mohanedmoh/treebind/issues/96)),
  once the agent keeps its state directory private on a claim that the kubelet has
  given to a group.
