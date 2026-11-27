# 0042. The sync pod can write a passwd entry for its user in an init container

- Status: accepted
- Date: 2026-11-27
- Issue: #107

## Context

Hardened platforms give a pod a user ID of their own choosing and a read-only root
filesystem: OpenShift's `restricted-v2` takes one from the range of the namespace,
with group 0 (R-46). The agent has to work under any user ID, and the owner of the
files it writes has to come from the security context of the pod, not from the image
(NFR-20).

The agent image runs as UID 65532, which has an entry in `/etc/passwd`
([ADR 0010](0010-the-agent-image-is-built-on-chainguards-git-image.md)). Run with
`docker run --read-only --cap-drop ALL --security-opt no-new-privileges --user
1000770000:0`, with OpenSSH 10.6 and Git 2.56.1:

- `ssh -V`, `ssh -G` and every connection fail with `No user exists for uid
  1000770000`. OpenSSH looks its own user up by ID and exits when there is no entry,
  whatever its options and `HOME` say.
- The agent stops at its first step, the question to the repository for its
  branches: `git ls-remote failed with exit status 128: No user exists for uid
  1000770000 fatal: Could not read from remote repository.`
- Git itself works. The agent's Runner gives it `HOME=/nonexistent`, no global or
  system config, `GIT_DIR`, a work tree on the claim and the identity of every commit,
  so Git needs no passwd entry and writes nothing outside the claim.

With an entry for the user mounted over `/etc/passwd`, the same container cloned,
committed and pushed over ssh, with the root filesystem read-only and no writable
`/tmp` or home directory. Nothing else stood in the way: the SSH key is mounted with
mode 0444 and readable by any user ID
([ADR 0007](0007-ssh-key-is-mounted-readable-by-the-agent.md)), and ssh reads no
home directory with `-F /dev/null`, the known hosts file named and an identity file
named. On kind, in a namespace that enforces `restricted`, a sync pod with the init
container below (before the working directory and the annotation were added to it) ran
as `runAsUser: 1000770000` and `runAsGroup: 0`, cloned, committed and pushed. The same
template ran as the image's user, 65532, as well.

The operator cannot know the user ID: the platform sets it when it admits the pod,
and the downward API does not give it. So the entry has to be written inside the pod,
as the user the pod runs as, before ssh runs. The root filesystem is read-only, and
`/etc/passwd` is a file of the image, so the entry has to be a file of a volume
mounted over it, and a volume's file can only be mounted once it is there.

## Decision

The operator has a setting, `AGENT_ARBITRARY_USER_ID` (the flag
`--agent-arbitrary-user-id`, the chart value `agent.arbitraryUserID`), which is off by
default. Off, the pod template is the one of the operator before. On, the sync pod of
every Treebind has an init container, `passwd`, of the agent image. It runs the agent
with the arguments `write-passwd /run/treebind/passwd`, which copies the passwd file
of the image into an `emptyDir` volume and adds
`treebind:x:<uid>:<gid>:treebind:/nonexistent:/sbin/nologin` when no entry has the
user ID, then exits. The sync container mounts that file at `/etc/passwd`, read-only,
with `subPath`. Both containers have the same security context, so the platform gives
them the same user, and the init container asks for the resources of the sync
container, so the pod asks for no more than before. Both containers have the working
directory `/`, because the image's own, `/home/git`, is mode 0700 for UID 65532, and a
runtime may change to it after it changes the user. The pod has the annotation
`cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes: passwd`, because the
cluster autoscaler does not take down a node with a pod that has an `emptyDir`
otherwise; the file is written again at every start.

Always on was rejected. The operator cannot tell when the user ID is one the image
lacks, so it would have to give every sync pod the init container, and that costs
every user something whether their platform assigns user IDs or not:

- An upgrade of the operator would replace every sync pod once.
- `kubectl drain` refuses a pod with an `emptyDir`, of any medium, unless it is given
  `--delete-emptydir-data` (`k8s.io/kubectl` `pkg/drain/filters.go`), so a drain that
  worked before would stop at every sync pod.

A user whose platform assigns user IDs sees the failure at once, `No user exists for
uid` in `Synced`, and turns the setting on, which costs them the same replacement and
the same flag for a drain, once and knowingly.

The agent gets no writable home or temporary directory, because nothing it runs
writes to either. The claim stays the only place it writes.

The other ways were rejected:

- **nss_wrapper**, with `LD_PRELOAD` on ssh and a passwd file that the agent writes
  itself, needs a library that the base image does not have. ko cannot add a package,
  so it means an image of our own, built with apko and signed and checked like the
  Chainguard one.
- **An SSH client in the agent**, with `golang.org/x/crypto/ssh` as the program
  that Git runs for ssh, needs no passwd entry and no ssh binary. It is a new
  dependency and a new client to keep right: strict known hosts with hashed and
  `[host]:port` entries, the host key algorithms that those entries allow, the bounds
  of ADR 0013 on a silent remote, and every key format that users mount.
- **Writing the entry into `/etc/passwd` at start**, as `git-sync --add-user` and
  the OpenShift `uid_entrypoint` scripts do, needs a writable root filesystem.
- **A passwd file from the operator**, in a ConfigMap, works only for a user ID set
  in the spec (#96), not for one that the platform assigns.
- **A mount namespace of the agent's own**, with the file bound over
  `/etc/passwd`, needs a user namespace, which `RuntimeDefault` and no capabilities
  forbid.

## Consequences

- Upgrading the operator replaces no sync pod: with the setting off the template is
  the one before, which `TestATemplateWithoutListsIsTheOneOfTheOperatorBefore` holds.
  Turning the setting on or off replaces every sync pod once.
- With the setting on, the agent runs under any user ID, with group 0 or any other,
  and so would `spec.agent.runAsUser` once it is applied (#96). `kubectl drain` needs
  `--delete-emptydir-data` for the sync pods.
- The setting needs an agent image of this release or newer. An older agent ignores the
  arguments, starts as the agent and never exits, so the init container never finishes
  and the pod never starts. That can happen with a pinned or mirrored agent image, or
  with `helm upgrade --reuse-values`, which keeps the agent image of the release before.
- A sync pod starts one more container, of the image it already pulled, before the
  agent. A failure of it shows as an init container that does not finish, and Ready
  says `SyncPodNotReady` without naming it.
- The state directory and the Git directory are private to the user that made them
  (mode 0700). A pod that runs as another user ID on a claim made before, after a
  move to a platform that assigns one or a change of `runAsUser`, fails the check
  `GitDir` with `GitDirUnsafe`, which names both users. The way out is to give the
  directory to the new user with `chown -R`, as root. Removing it clears the check too,
  but loses the commits that were not pushed, and the new checkout refuses a synced
  directory that does not match the branch with `DirectoryNotEmpty`
  ([the Git directory](../git-directory.md#what-the-agent-checks)). Git's check of the
  owner of a repository is not reached: the Runner sets `GIT_DIR` and a
  `safe.directory` of the work tree (ADR 0013). Within one namespace OpenShift gives
  every pod the same user ID, so a restart does not change it.
- `hack/check-agent-image.sh` runs the image as a random user ID with group 0, as
  the template with the setting on does: without the file ssh has to fail with `No user exists`, the
  init step writes it, and the agent clones over ssh from a throwaway server with a
  read-only root filesystem and no `/tmp`. CI runs it on every pull request.
- The image keeps a working directory, `/home/git`, that only UID 65532 can enter.
  Git run there without `GIT_DIR` fails with `fatal: error reading '/home/git/.git'`.
  The agent runs every Git command in the work tree with `GIT_DIR`, and Docker and
  kind (runc) started the container there under another user ID, but a runtime that
  changes the directory after the user would not, so the setting sets `/`.
- On a platform that assigns user IDs with group 0, `fsGroup: 0` and `fsGroup: 65532`
  are refused by OpenShift's `restricted-v2`; docs/pod-security.md says which group the
  application's pod and the sync pod share there.
