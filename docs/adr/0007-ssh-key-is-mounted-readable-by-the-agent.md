# 0007. The SSH key is mounted readable by the agent

- Status: accepted
- Date: 2026-11-11
- Issue: #135

## Context

The sync pod mounts the Secret that holds the SSH key as a whole-directory volume
(#25) and runs as a non-root user without an `fsGroup`, as the `restricted` Pod
Security level requires (#29). The kubelet creates Secret files owned by root,
and the mode was 0400, so the agent could not read its key. Tried on kind with
the pod's settings and a non-root user, OpenSSH 10.3:

| Secret mode | `fsGroup` | File in the pod | Key loads |
| :-- | :-- | :-- | :-- |
| 0400 | none | `root:root`, 0400 | no, permission denied |
| 0400 | set | `root:<fsGroup>`, 0440 | yes |
| 0444 | none | `root:root`, 0444 | yes |

OpenSSH refuses a private key that the group or others can read only when the
user running it owns the file. A file owned by root is accepted whatever its mode.

## Decision

The key is mounted with mode 0444. The agent reads it in place.

The other ways to make the key readable were rejected:

- **`fsGroup`** works, but it changes group ownership and permissions of the
  whole claim, with a recursive change on every mount (R-36). Platforms that
  assign the `fsGroup` themselves reject a fixed value (R-46). Users can still set
  one through `spec.agent.fsGroup` (FR-28).
- **Copying the key into a private `emptyDir`** does not help on its own: the
  step that copies runs as the same non-root user and cannot read the original
  either. The copy would also have to be made again after every rotation, and the
  Secret has to stay a whole-directory mount so that the kubelet refreshes it.
- **A user that owns the file** is not possible. The kubelet makes root the
  owner, and `restricted` forbids running as root.

The key is on a tmpfs that only the agent container mounts, and only the user
that has to read it can reach it, so the wider mode exposes nothing new.

## Consequences

- The agent can use `auth.sshKeySecretRef` as it is, on any storage and with any
  user ID, including the arbitrary user IDs of hardened platforms (R-46).
- A running agent that owns the key file would be refused by OpenSSH, because the
  mode is wider than 0600. That only happens when it runs as UID 0, which
  `restricted` does not allow. If a later change allows it, the agent has to copy
  the key to a private file first.
- An e2e case runs the operator's pod template as a non-root user and checks that
  a key from the Secret loads, so a change back to 0400 fails the suite.
