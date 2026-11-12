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
as the user its image names. Both images are built on distroless `nonroot`,
which is UID 65532 and GID 65532. The sync image gets a fixed UID of its own in
[#84](https://github.com/mohanedmoh/treebind/issues/84).

`spec.agent.runAsUser`, `runAsGroup` and `fsGroup` exist in the API but are not
applied yet. A Treebind that sets them reports `SpecSupported=False` with the
reason `NotImplemented`.

## The SSH key

The Secret with the SSH key is mounted at `/etc/treebind/ssh` with mode `0444`.
The kubelet makes Secret files owned by root, and the agent runs as a non-root
user without an `fsGroup`, so a stricter mode such as `0400` would leave the
agent unable to read its key. OpenSSH accepts a key that root owns even when
others can read it. The file is on a memory-backed volume that only the agent
container mounts. The reasons and the alternatives are in
[ADR 0007](adr/0007-ssh-key-is-mounted-readable-by-the-agent.md).

## What the volume needs

The agent writes to the claim as UID 65532 and GID 65532, with umask 0002, so
the files and directories it creates are group-writable (`0664` and `0775`).

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

See the Kubernetes documentation on
[configuring volume permissions](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#configure-volume-permission-and-ownership-change-policy-for-pods)
for how `fsGroup` and `fsGroupChangePolicy` behave on your storage driver.

## What is still to come

- The agent checks at startup that it can write to the volume and reports a clear
  condition when it cannot ([#31](https://github.com/mohanedmoh/treebind/issues/31)).
- Git's `safe.directory` for a checkout owned by another user
  ([#31](https://github.com/mohanedmoh/treebind/issues/31)).
- A writable home and temporary directory for the agent. Its root filesystem is
  read-only and Git and SSH write there, so the agent has to keep them on a
  temporary volume
  ([#107](https://github.com/mohanedmoh/treebind/issues/107),
  [#84](https://github.com/mohanedmoh/treebind/issues/84)).
- `spec.agent` user, group and `fsGroup` overrides (FR-28).
