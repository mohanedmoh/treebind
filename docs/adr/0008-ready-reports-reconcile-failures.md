# 0008. Ready reports reconcile failures

- Status: accepted
- Date: 2026-11-12
- Issue: #136

## Context

When the operator cannot create or update the sync Service or StatefulSet, for
example because the API server rejects the pod template, the reconcile returned
the error before it wrote any status. The failure showed only in the operator
log. `kubectl describe treebind` showed nothing, and `SpecSupported` was not
published either, although the pass had already worked it out.

ADR 0004 asks that a condition which can make `Ready` False gets its own reason
and a record that extends the aggregation. This is that record.

## Decision

A pass that cannot create or update the sync objects publishes the status it
has, then returns the error.

- `Ready` is `False` with the reason `ReconcileFailed`. The message is
  `The last reconcile failed: <error>`, where the error names the object that
  failed and carries the API server's answer, cut at 1024 bytes.
- It comes before the other reasons: whatever the pod count or `Synced` say,
  the sync objects may not match the spec after a failed pass. The next pass
  that succeeds computes `Ready` as ADR 0004 describes.
- The rest of the status is written in the same update: `SpecSupported`, the
  initial `Unknown` for `Synced` and `Conflict`, `observedGeneration` and the
  generation of the conditions the loop owns.
- The error is still returned, so the request is retried with backoff as
  before. Reporting it and returning it do not replace each other.
- An error that a retry makes go away is returned and not reported: an update
  conflict and an object that already exists, which come from a cache that has
  not caught up with the previous pass, and the cancellation of the context
  when the manager stops. Reporting them would turn `Ready` False for a moment
  on every rollout.
- The status write is still an update of the object that was read, so it fails
  on a conflict and the next pass starts from the newer object. If it fails,
  both errors are returned.
- A failure that repeats with the same text changes no condition, so it writes
  nothing and cannot start a loop through the watch on the Treebind. This
  relies on the API server answering with the same text for the same object on
  every pass, which it does for validation errors. A cause whose text changed
  on every pass would write on every pass.
- `status.observedGeneration` and the generation of the conditions go to the
  current generation on a failed pass too. They say which spec the operator has
  looked at, as in ADR 0004, and `Ready` `False` at that generation is what
  tells a reader that the spec was seen and could not be applied.

The message holds the error text of the API server about objects built from the
spec. That is what anyone who can read the Treebind can read anyway. The sync
objects hold no credential, because the key is a reference to a Secret that the
operator never reads, and the repository URL is not in them. Text from a Git
host stays out of the message, as ADR 0004 requires.

## Consequences

- A Treebind whose sync objects cannot be written shows `Ready` `False` with the
  reason and the error in `kubectl describe treebind`, and `kubectl get
  treebind` shows `False`.
- The reason is API: tooling can match on `ReconcileFailed`. It is for failures
  of the operator's own writes. A sync that fails belongs to `Synced`.
- Every other error is reported whatever its cause, a timeout of the API server
  included, so `Ready` can turn `False` for one pass during an outage of the
  API server and come back on the next one.
- The schema still accepts Secret and ConfigMap names that the API server does
  not check either, so a wrong name shows up in the sync pod and not here.
