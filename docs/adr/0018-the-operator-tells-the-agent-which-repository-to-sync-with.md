# 0018. The operator tells the agent which repository to sync with

- Status: accepted
- Date: 2026-11-14
- Issue: #32

## Context

The agent clones and reopens the checkout (ADR 0017), and for that it has to know which
repository to ask, which branch to follow and where the credentials are. Until now the pod
told it only where the claim is, what it is called and how the operator reaches its status
endpoint (ADR 0011, ADR 0016). The credentials are already in the pod: the operator mounts
the Secret with the SSH key and the ConfigMap with the host keys, and never reads them
(ADR 0007).

## Decision

The operator puts four more settings in the sync pod as environment variables, as it does
for the others (ADR 0011):

| Variable | Meaning | Set when |
| :-- | :-- | :-- |
| `TREEBIND_REPO_URL` | `spec.repository.url`, as it is | always |
| `TREEBIND_REPO_BRANCH` | `spec.repository.branch` | the spec has a branch; without one the agent follows the default branch of the repository |
| `TREEBIND_SSH_KEY_FILE` | the file of the SSH key in the pod: the mount path of the Secret and the key | `auth.sshKeySecretRef` is set |
| `TREEBIND_KNOWN_HOSTS_FILE` | the file of the host keys in the pod: the mount path of the ConfigMap and the key | `auth.knownHostsConfigMapRef` is set |

The two files are paths, never the content, and nothing reaches the agent through the
environment that a credential could be in (NFR-3). The path is the one the operator mounts
the key at, so the agent is told of a file that the pod has.

The agent reads them with the others and exits with code 2 on a value it cannot use, with
every problem at once. The address is required and must have an explicit `ssh://` or
`https://` scheme, in any case. An `ssh://` address needs both files, because host keys are
always verified. An `https://` address needs neither, and the agent takes them when the
CRD lets a Treebind have them. The branch is checked with `config.CheckBranch`, which has
the rules of the CRD for `repository.branch`.

### The one place where the agent is stricter than the CRD

The CRD admits `http://` addresses, and the Runner refuses Git over plain http, as NFR-15
says. A Treebind with such an address cannot sync, so the agent says so at start and exits
with code 2, naming the requirement, and does not start a sync that fails at the first
fetch with Git's wording. It is the only value that the CRD takes and the agent refuses,
and a test holds that: for every address in a list that covers the rules on both sides,
the agent takes what the API server takes, and a second test says that `http://` is the
exception, so the day the CRD stops admitting it that test says so. Tightening the CRD is
the better fix, because the user then sees the error when the Treebind is applied, and is a
change to the API that is not made here.

### The branch and the address change the pod

The agent reads them at start, so a change of either replaces the sync pod, which then
reopens the checkout and finds the branch changed or the repository different (ADR 0017).
The branch was not read by the pod before, so a change of it did not restart anything.

## Consequences

- Every sync pod template changes, because the address is always set, so each sync pod is
  replaced once when the operator is upgraded (#87). The replacement is the agent
  restarting on the same claim, which it is built to do: nothing is lost, and the checkout
  is reopened.
- A Treebind with an `http://` address gets a sync pod that exits at start, with its
  reason in the agent's log, until the CRD stops admitting it or the address is
  changed.
- `CheckBranch` mirrors the CRD, which checks `.lock` only at the end of the whole name,
  where Git refuses it at the end of any component. A branch such as `a.lock/b` passes both
  and fails at the first fetch. Tightening the CRD would close that too.
- Nothing in the agent uses the settings yet. The agent runs the checkout in the next change.
