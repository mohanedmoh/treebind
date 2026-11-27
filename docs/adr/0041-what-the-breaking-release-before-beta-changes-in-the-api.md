# 0041. What the breaking release before beta changes in the API

- Status: accepted
- Date: 2026-11-27
- Issue: #305
- Supersedes: the part of [ADR 0003](0003-reserved-placement-and-agent-fields.md) that accepts `placement.mode: sidecar`

## Context

The API group `treebind.example.org` is a placeholder (ADR 0001, #15). Changing it changes the `apiVersion` of every
Treebind, so every user creates their objects again, once. That release is the one chance to make a change that an
existing object could not live with.

Most of what is planned does not need that chance. An optional field, a new value of an enum or a looser rule breaks no
object and can be added in any later release. What cannot be added later without breaking someone is a rule that
refuses what is accepted today, a field or a value that is taken away, and a new immutability.

## Decision

### The group and the version

- The group is a domain that the owner registers. The domain is not chosen yet (#15), and a record of its own will name it.
  The annotation `treebind.example.org/template-hash` moves with it.
- The first release under the new group serves `v1alpha1`. The first beta adds `v1beta1` with the same schema, served
  beside `v1alpha1` with the conversion strategy `None`, and stores `v1beta1`. Nobody creates an object again for that,
  because `None` only changes `apiVersion`.

### Changes that go into the breaking release

| Change | Why now |
| :-- | :-- |
| `spec.placement.mode` loses the value `sidecar`; `standalone` stays | Sidecar placement (#63, #114) comes after beta. A beta should not accept a value that does nothing, and the value can be added back when it works. This reverses ADR 0003 for this value |
| `spec.target.mountPath` is removed | It only sets where the claim is mounted inside the sync container, which no user of the volume sees |
| `spec.sync.interval` is at least 5 s (1 s today, ADR 0002) | Every pull asks the Git host. One Treebind at 1 s asks 3600 times an hour. The end-to-end tests and the chart test already use 5 s |
| `commit` is refused with `direction: pull` (#299) | It is ignored today |
| A repository URL needs a host, and its scheme in lower case followed by `//` (#298) | `ssh:/h/x.git`, `ssh:///x.git` and `HTTPS://...` are accepted and can never work |

`spec.sync.settleTime` stays at 1 s or more: it only decides when the agent looks at its own volume.

### What stays as it is

- **`spec.repository.url`, `branch` and `path` can still be changed.** ADR 0002 keeps them mutable "because the agent can
  re-sync", which is not how it works now: the agent refuses another branch (`RepositoryMismatch`), another directory
  (`PathMismatch`) and a repository with no history in common. A new address of the same repository is followed. The
  decision stands for another reason: the agent protects the volume, moving a repository to another host works, and the
  way out that the messages give, to set the field back, needs a field that can be set.
- Only `spec.target.claimName` and `subPath` are immutable (ADR 0002).
- **The fields of `spec.agent` stay** (ADR 0003). They are implemented before beta (#96, #83), and `SpecSupported`
  goes on saying what is not.
- Condition types and reasons stay (ADR 0004). The list is read once as a whole before the first beta.
- The cap of 56 characters on the name, and the rules for names, branches, paths and patterns, stay.

### Added later, with the shape decided now

- `spec.placement.appSelector`, a label selector for the pods of the application, for the pod affinity of a
  ReadWriteOnce claim, never `nodeName` (#97).
- `spec.repository.auth.tokenSecretRef`, a key of a Secret that holds an HTTPS token (#58). It is not set together with
  `sshKeySecretRef`, and only with an `https://` address.

### Plain `http://`

This is #268 and is not decided here. It has to be decided before the breaking release. Allowing it later, with an
opt-in, adds a field. Deciding that it is never allowed means refusing `http://` in the schema, which belongs in the
breaking release. Until then the API accepts the address and the operator refuses it in `Ready` (ADR 0039).

## Consequences

- Users move once: delete the Treebinds, install the new release, create them again. The claims keep their data and the
  checkout is reopened (#295 writes and tests the steps). An agent of the old group and one of the new cannot both write
  a volume, because of the writer lock.
- A manifest that sets `sidecar`, `mountPath`, an interval under 5 s, or `commit` on a pull Treebind is refused after
  the move. The migration steps list each.
- Serving two versions later means that removing `v1alpha1` needs the stored objects rewritten first.
- The versioning and deprecation policy (#69) says what may change from here on.
- The changes of the table are not released before the group changes, so that users are broken once and not twice.
