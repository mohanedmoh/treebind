# 0027. The Treebind can raise the cap on the files an update removes

- Status: accepted
- Date: 2026-11-16
- Issue: #40

## Context

ADR 0026 makes the agent refuse an update that would remove more than 100 files from the directory, or any file
and leave the branch with none. A person who means a big deletion can let it through by removing the files
themselves, because the agent counts only the files that are in the directory. That works, and it is what the
page about the guard gives commands for, but it asks someone to log in to a pod that mounts the claim and delete
files from an application's directory, for a decision that is already in the repository. A repository where a
reorganisation removes a few hundred files every so often has the same wait at each of them.

A field of the Treebind is how every other setting of the sync is chosen, and the person who writes the spec is
the person who owns the directory.

## Decision

`spec.sync.maxDeletions` is the most files that one update may remove from the directory.

- It is a whole number from 1 to 2147483647. The CRD says both: the API server does not check the range of a
  field that is only marked `int32`, which a test found, so the maximum is a marker of its own.
- It has **no default in the CRD**. The agent has 100 when the field is not set, which is the cap of ADR 0026.
  A default that the API server filled in would put the variable in the pod of every Treebind that exists, and
  replace every sync pod when the operator is upgraded.
- The operator passes it as `TREEBIND_SYNC_MAX_DELETIONS`, a number, which the kubelet does not rewrite (#179),
  and **only when the field is set**. The pod template of a Treebind that does not set it is the same, byte for
  byte, and a test holds the hashes of such templates.
- The agent reads it with the other settings and exits with code 2 on a value it cannot use, with every problem
  at once: digits only, from 1 to 2147483647, which is the rule of the CRD. A test holds the two together, as it
  does for the interval: for a list of numbers on both sides of each limit, the API server accepts a number
  exactly when the agent does. A value that the API server takes and the agent refuses would leave a sync pod
  that exits at start for ever.
- It changes how many files an update may remove, and nothing else. **An update that leaves the branch with no
  file at all is refused whatever it says**, because a branch that was emptied by mistake is the case that no
  number can tell from a wanted one. To let that through, a person removes the files, as ADR 0026 says.

To let a big update through, a person raises the number, and the update goes on at the next pull after the sync
pod has been replaced. To go back to the protection, they set it again or take the field away.

## Consequences

- A change of the field replaces the sync pod once, because the agent reads it at start (#87). The new pod
  reopens the checkout (ADR 0017) and pulls again. A Treebind that never sets it is not touched by the upgrade of
  the operator.
- The number stays in the spec after the update has gone through, and a later update that removes as many files
  goes through too. The cap is what the person says it is until they say otherwise.
- The field is in `v1alpha1`, and a field can be added to it without breaking a client that does not know it.
  Taking it away later would break one that sets it.
