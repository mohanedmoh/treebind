# 0003. Reserved placement and agent fields

- Status: accepted
- Date: 2026-11-11
- Issue: #95

## Context

The sync pod can run in two shapes. Standalone runs it as its own pod: anywhere
on a ReadWriteMany claim, and on a ReadWriteOnce claim with required pod
affinity to the application's pods, never by pinning `nodeName` (FR-25).
Sidecar runs the sync container inside the application's pods, which is the
only way to share a ReadWriteOncePod claim and works with several application
replicas (FR-27). Users also need to tune the pod: resources, node selector,
tolerations, priority class, pull secrets, labels, annotations, user, group and
fsGroup (FR-28).

The behaviour lands in later issues (#96 to #100), but the fields they need
shape the CRD that users write manifests against. Adding them one issue at a
time would change the schema, the generated CRD and the Helm chart several
times for a surface that is already known.

## Decision

Reserve the whole surface in v1alpha1 now, ahead of the behaviour:

- `spec.placement.mode` is `standalone` or `sidecar`. `spec.placement` carries
  a default of `{mode: standalone}` on the field itself as well as on `mode`,
  because a nested default only applies when its parent object exists; without
  the parent default a Treebind that omits `placement` would read back without
  a mode.
- `spec.agent` is an optional block with no default. It holds the overrides
  as flat fields: `resources`, `nodeSelector`, `tolerations`,
  `priorityClassName`, `imagePullSecrets`, `labels`, `annotations`, `runAsUser`,
  `runAsGroup` and `fsGroup`. The three IDs are optional pointers with a
  minimum of 0, so an unset ID and an explicit `0` stay distinguishable.
  Validation follows ADR 0002: at most 32 node selector entries, tolerations,
  labels and annotations, at most 16 pull secrets, and a maxLength on the
  priority class name. `imagePullSecrets` is a list map keyed by `name`.

The shape is flat, not a nested pod template or `securityContext`. The operator
keeps control of everything else in the pod (image, command, volumes, mounts
and the rest of the security context), and the flat names are the ones users
already know from a pod spec.

Accept values that are not implemented yet instead of rejecting them. The
operator ignores them and says so in a `SpecSupported` condition: `True` with
reason `Supported`, or `False` with reason `NotImplemented` and a message that
lists the ignored fields by their dotted path, for example
`spec.agent.nodeSelector, spec.placement.mode`. `placement.mode: standalone` is
the default and never counts as ignored; `sidecar` and every set `agent` field
do. The reconciler writes the status only when the condition or the observed
generation changed, so its own status update does not start a loop.

## Consequences

- A manifest written for the final API applies today, and the CRD shape does
  not change when #96 to #100 land. Only the reconciler gains behaviour and the
  condition goes quiet.
- Rejecting the fields at admission was considered and dropped: it would make
  the CRD change every time a field starts working, and a user who sets a field
  before it works gets a failed apply instead of a working Treebind.
- Accepting silently was rejected too, because a user who sets `nodeSelector`
  would believe the pod is constrained. The condition makes the gap visible in
  `kubectl describe`. It is also machine-readable, which an event is not: events
  expire and are not part of the object's state. A validating webhook would
  need to be built, served and certified, and ADR 0002 avoids that.
- The `agent` fields reuse the Kubernetes types for resources, tolerations and
  pull secrets, so the CRD carries their full schema, and a Kubernetes upgrade
  that changes those types changes the generated CRD.
- Every field added to `agent` later must also be added to the list of
  unimplemented fields until it works, or the condition will report it as
  supported. A test fails when a field is missing from the list.
- The `labels`, `annotations` and `nodeSelector` maps are bounded in size only.
  Rules for their keys and values, and which label prefixes the operator keeps
  for itself, arrive with the behaviour in #96 to #100, because that is where
  the operator decides what it owns.
