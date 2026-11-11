# 0005. SSH repositories require known hosts

- Status: accepted
- Date: 2026-11-11
- Issue: #28

## Context

An `ssh://` repository is only as safe as the check of the Git host's key. If
the agent does not compare it with keys the user trusts, anyone on the network
path can answer as the Git host and receive the connection that carries the
deploy key's authentication and the checkout (R-14).

Until now `auth.sshKeySecretRef` and `auth.knownHostsConfigMapRef` were both
optional, so an SSH repository could be created with no host key set up, and the
safe behaviour depended on the agent doing something the API never asked for.
The sync pod already mounts both when they are set (#25).

## Decision

Admission rejects an `ssh://` repository unless both references are set. The rule
reads the scheme with the same URL parser as the `url` rule, so `SSH://` counts,
and the error names both references. `http` and `https` repositories are not
affected: they have no host key and take no credentials in the URL.

There is no field that turns host key checking off, no trust on first use and no
built-in list of hosts. The user states which host keys to trust, in the
ConfigMap, for example from `ssh-keyscan <host>`.

This is the contract for the agent, which is not built yet (#81, #80): it
verifies the Git host only against the mounted known hosts file, with strict host
key checking, and never falls back to trust on first use or to a known hosts
file of the image or the system. When the host has no entry or the key does not
match, it stops with a clear error and a condition.

## Consequences

- A user who sets up SSH has to produce the known hosts first. `ssh-keyscan`
  trusts whatever the network shows at that moment, so the entries should be
  compared with the fingerprints the host publishes. The sample shows the
  commands.
- A missing reference field fails at `kubectl apply`, naming what is missing,
  instead of failing later inside the sync pod. Whether the referenced Secret
  and ConfigMap exist is not checked at admission, because the operator never
  reads them; the preflight checks report it (#31).
- This tightens `v1alpha1`: an `ssh://` object without both references, which
  earlier builds accepted, is rejected when it is created and when its
  `spec.repository` is edited. CRD validation ratcheting, which the supported
  Kubernetes versions enable, keeps such an object usable otherwise: a label, a
  status update or an edit of another field succeeds with a warning. Anyone
  upgrading should add the references to existing `ssh://` objects first.
- When a host rotates its key the user updates the ConfigMap. Detecting the
  mismatch and the runbook are #80.
