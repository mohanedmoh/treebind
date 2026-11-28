# 0044. The operator applies spec.agent to the sync pod, except fsGroup

- Status: accepted
- Date: 2026-11-28
- Issue: #96
- Supersedes: the part of [ADR 0003](0003-reserved-placement-and-agent-fields.md) that reports every field of
  `spec.agent` as not implemented

## Context

`spec.agent` has been in the API since the first release, and the operator reported each field that was set in
`SpecSupported` and ignored it (ADR 0003). FR-28 asks for the overrides: resources, node selector, tolerations,
priority class, pull secrets, labels, annotations, and user, group and fsGroup for the ownership of the files. NFR-20
asks that the ownership comes from the security context and not from the image.

The following was decided before this record: a Treebind without `spec.agent` keeps its pod template byte for byte; a
`runAsUser` other than the image's 65532 gives that sync pod the passwd init container of ADR 0042, whatever the
operator's setting; the validation of the CRD does not change and no field is added; the sync pod keeps
`runAsNonRoot`, the read-only root filesystem, no capabilities, no privilege escalation and the `RuntimeDefault`
seccomp profile; and what the operator sets itself is not overridden.

What was still open, and what was found:

- **fsGroup.** With an `fsGroup`, the kubelet walks the volume when it mounts it into the pod (`SetVolumeOwnership` in
  `pkg/volume/volume_linux.go` of Kubernetes): every file, links excepted, gets the group, group read and write, and a
  directory also group execute and setgid. With the default policy `Always` it does that at every mount, and with
  `OnRootMismatch` only while the root of the volume does not have the group, those bits and setgid. The agent keeps
  its state directory, `.treebind`, and the Git and lock directories in it, at mode 0700, and refuses one that is open to
  the group (`GitDirUnsafe`, ADRs 0016 and 0019): it cannot tell the kubelet from another user of the group, and the
  application usually is one. A test that does to a claim what the kubelet does
  (`internal/agent/layout/fsgroup_test.go`) shows:
  - a walk before the first start, on a claim without `.treebind`, does not stop the agent: what it makes after the walk
    is 0700, also under a root that is setgid (the directories inherit setgid, which the check does not look at);
  - a walk of a claim on which the agent has started before turns `.treebind` into mode 2770, and the agent refuses it
    at its next start. With `Always` that is every new sync pod after the first;
  - with `OnRootMismatch` the walk of the first mount is the last one while nobody changes the root, and the agent
    starts again on the same claim. A claim with a checkout already on it, which is when someone adds an `fsGroup` to
    fix a permission problem, is still walked once with `.treebind` in it, and refused;
  - the files of the application get group read and write and keep their execute bits. Git records only the execute bit
    of the owner, so it sees no change of mode; `git status` stays clean after the same change of a checkout. The change
    time of every file moves, so Git and the agent's scan read each file once more after a walk, and find it the same.

  On a kind cluster with a local PersistentVolume, which applies `fsGroup`, and the released agent v0.0.1-alpha.5, the
  same was seen: after a pod with `runAsUser: 1000` and `fsGroup: 65532`, with the default policy, mounted the claim,
  `.treebind` went from 0700 to 2770, and the agent failed the check `GitDir` with `GitDirUnsafe` and `open to group or
  others`. `chmod -R g-rwxs <mount>/.treebind`, run as UID 65532, repaired it: `GitDir` passed and the agent went on.
  After the repair, a restart of the application with `fsGroupChangePolicy: OnRootMismatch` left `.treebind` at 0700,
  since the root of the volume already matched; with the default policy it was 2770 again. The bug is #313.

  There is no repair the agent could make on its own that is safe: putting the mode back to 0700 after the walk leaves a
  window in which a user of the group could have put something into the Git directory, or a hard link to a file of it.
  The content check of ADR 0019 would catch a hook or a config key, but not changed refs or objects, a hard link or a
  file that was held open.
- **Resources.** The defaults are requests of 10m CPU and 32Mi memory and a limit of 128Mi memory. A partial setting
  must never give an invalid pod, such as a request of 256Mi above the default limit.
- **What the API server refuses.** The CRD takes label values and quantities that a pod does not. The StatefulSet is
  validated with its pod template, so most of that is refused when the operator writes the StatefulSet. A priority class
  that does not exist, a quota or the pod security admission refuse the pod only, when the StatefulSet controller makes
  it.
- **A change of `runAsUser`** on a claim the agent has used gives the claim a new owner of its state directory (ADR
  0042).
- **A user without a group.** On the same kind cluster a pod with only `runAsUser: 1000` ran as `uid=1000 gid=0(root)`:
  the runtime gives group 0 when the image's user is not the one that runs. The files of the claim are shared through
  group 65532 (the pod security page), so such a pod could not change what the application wrote, and what it wrote
  would be in group 0.
- **Annotations** reach the pod, and the deprecated AppArmor and seccomp fields are annotations
  (`container.apparmor.security.beta.kubernetes.io/*`, `seccomp.security.alpha.kubernetes.io/*`,
  `container.seccomp.security.alpha.kubernetes.io/*`) that a cluster which still reads them may take over the profile
  of the pod, `unconfined` included.
- **The passwd init container** has the memory limit of the sync container, so a limit of `spec.agent` that is too low
  kills it before the sync container starts, and `SyncPodOOMKilled` looked only at the sync container.
- **Who may create a Treebind** now chooses, for one pod in the namespace, its priority class, tolerations, node
  selector, labels and pull secrets.

## Decision

The operator applies every field of `spec.agent` to the sync pod but `fsGroup` and `resources.claims`, which
`SpecSupported` goes on reporting as accepted and not implemented. `fsGroup` waits for #313, how the agent keeps its
state directory private on a claim the kubelet has walked (above); `resources.claims` has no counterpart in the
pod, which would need `spec.resourceClaims` too.

- **Resources.** Each request and each limit that is set takes the place of the default of its resource. A default
  limit below a request that is set is raised to the request, and a default request above a limit that is set is lowered
  to the limit, as the API server does with a request that is not set. A request above a limit that were both set is
  left for the API server to refuse. Other resources, such as `ephemeral-storage`, are passed on. The passwd init
  container has the resources of the sync container, as before.
- **Scheduling.** `nodeSelector`, `tolerations`, `priorityClassName` and `imagePullSecrets` are the pod's, as set.
- **User and group.** `runAsUser` and `runAsGroup` go into the pod security context, which the init container shares,
  beside `runAsNonRoot` and the seccomp profile. The security context of the container does not change. A `runAsUser`
  other than 65532 gives the pod the passwd init container, so `kubectl drain` of its node needs
  `--delete-emptydir-data`, and it needs an agent image of this release or newer. A `runAsUser` without a `runAsGroup`
  gets group 65532, the image's, and not group 0. `runAsUser: 0` gets no pod: the operator creates or changes no sync object, leaves a sync pod
  that the Treebind had as it was, and `Ready` is `False` with the reason `UnsupportedAgentUser`, whose message names the
  field. That follows `UnsupportedRepositoryScheme`: the kubelet would refuse the pod, and a change of the spec is what
  puts it right.
- **Labels and annotations** are added to the pod template after the operator's own. A key the operator sets, the
  labels that select the pod and the annotation of the cluster autoscaler for the passwd volume, keeps the operator's
  value. The template hash is an annotation of the StatefulSet, not of the pod, and cannot be reached. The annotations
  of the deprecated AppArmor and seccomp fields are left out, so that the pod keeps its profile.
- **`SyncPodOOMKilled`** names the `passwd` init container, with its memory limit, when it was killed for memory and has
  not written the file since, as it names the sync container.
- **What the API server refuses** of the StatefulSet makes `Ready` `False` with `ReconcileFailed` and the API server's
  words, as for any other write that fails. A pod that is refused when it is made leaves `Ready` at `SyncPodNotReady`,
  and the events of the StatefulSet (`FailedCreate`) say why.
- **A change of `runAsUser`** on a claim with a checkout is refused by the agent with `GitDirUnsafe`, naming both users;
  [the pod security page](../pod-security.md#when-the-user-id-changes) says how to give the directory to the new user.
- **[The permissions page](../rbac.md)** says what a person who may create a Treebind can choose for the sync pod.

## Consequences

- A Treebind without `spec.agent`, or with only `fsGroup` or `resources.claims`, keeps its pod template and its hashes.
  Any other field changes the template, and its sync pod is replaced when it is set or changed.
- `fsGroup` stays open in #96 and #313. The choices: apply it with `fsGroupChangePolicy: OnRootMismatch` and
  document the one walk of a claim that has a checkout already, with the change of mode that puts `.treebind` right; or
  move the agent's private state where the walk does not reach it; or have the agent take back a directory the walk
  opened, with what that leaves open (above).
- The same walk happens when the application's pod has an `fsGroup`, which [the pod security
  page](../pod-security.md#what-the-volume-needs) suggests: with `Always`, every start of the application's pod after the
  agent's first opens `.treebind` to the group, and the next sync pod refuses it (#313). The page now says to set
  `fsGroupChangePolicy: OnRootMismatch` on such a pod, and gives the repair. Kind's `hostPath` and `local-path` volumes
  do not apply `fsGroup`, so the end-to-end tests do not show it.
- An upgrade applies the `spec.agent` that the operators before accepted and ignored: the pages on the status and on pod
  security say what that does.
- Whoever may create a Treebind may give one pod a priority class that preempts others, tolerations of tainted nodes and
  labels that a NetworkPolicy or a Service selects on. A cluster that limits that for pods has to limit it for
  Treebinds too: a ResourceQuota with a `scopeSelector` on `PriorityClass` holds for the sync pod, since the pod is made
  in the namespace.
