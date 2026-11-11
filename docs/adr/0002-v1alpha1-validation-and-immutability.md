# 0002. v1alpha1 validation and immutability

- Status: accepted
- Date: 2026-11-10
- Issue: #24

## Context

The v1alpha1 schema accepted almost any string. The operator turns those
strings into pod names, mount paths and git invocations, so a bad value only
surfaced once the sync pod ran — or worse, as path traversal out of the synced
directory (risk R-23) or as traffic to an unexpected host (risk R-37). CRDs
can enforce most of this at admission with OpenAPI limits and CEL rules, which
also give defaults and immutability without an admission webhook.

## Decision

Validate in the CRD schema, at admission:

- `repository.url` must carry an explicit `ssh://`, `https://` or `http://`
  scheme; the scp-style `git@host:path` shorthand, whitespace and control
  characters are rejected. `http://` and `https://` URLs may not carry any
  userinfo and `ssh://` URLs may not carry a password, because credentials come
  from `auth`, not from a spec that `kubectl get` prints. The scheme list is
  shape checking only: which hosts the agent may reach (R-37) stays enforced by
  the host allowlist and the egress NetworkPolicy at the controller and agent
  level, not by the schema.
- `repository.branch` must be a name `git check-ref-format` would accept in the
  cases the schema can see: no whitespace, control character or `~ ^ : ? * [`
  or backslash, no `..`, `@{`, `//`, lone `@`, leading `-` or `/`, component
  starting with `.`, or trailing `.`, `/` or `.lock`.
- `repository.path` and `target.subPath` must be relative, with no `.` or `..`
  segment, no `.git` segment, no empty segment, no trailing slash and no
  backslash, so the agent cannot be pointed at git metadata or out of the
  synced directory (R-23). The `paths` patterns must be relative with no `..`
  segment; a backslash stays legal there because it escapes a glob character.
  `target.mountPath` must be absolute with no `..` segment.
- `sync.interval` and `sync.settleTime` must be at least 1s. Every string gets
  a maxLength and every list a maxItems, because an unbounded field under a CEL
  rule can blow the CEL cost budget and make the apiserver reject the CRD.

Default `sync.interval` to 30s, `sync.settleTime` to 5s and
`sync.conflictPolicy` to `followRemote`.

Make `target.claimName` and `target.subPath` immutable through CEL transition
rules. Together they name the directory whose git history the agent owns, so
retargeting a live Treebind would mix the histories of two directories.
Everything else, branch included, stays mutable because the agent can re-sync.

Cap `metadata.name` at 56 characters: the sync StatefulSet's pod is named
`<name>-sync-0`, and a pod name must fit the 63-character DNS label limit.

## Consequences

- A bad object fails at `kubectl apply` with a named field instead of a broken
  sync pod, and no webhook needs to be built, served or certified.
- The rules only bound what the schema can see. The agent must still treat the
  URL, paths and branch as untrusted input when it builds git commands, and the
  host allowlist plus NetworkPolicy remain the enforcement for where traffic
  goes.
- Repointing a Treebind at another claim or directory means deleting and
  recreating it. That is intended.
- The 56-character cap and the `-sync-0` naming scheme now depend on each
  other; changing the suffix needs a new ADR and a CRD update.
