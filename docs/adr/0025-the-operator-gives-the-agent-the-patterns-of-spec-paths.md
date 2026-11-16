# 0025. The operator gives the agent the patterns of spec.paths

- Status: accepted
- Date: 2026-11-16
- Issue: #43

## Context

The agent decides what it commits with the include and exclude patterns of `spec.paths` (ADR 0022), and
reads them at start from `TREEBIND_PATHS_INCLUDE` and `TREEBIND_PATHS_EXCLUDE`, each a JSON array of strings.
It refuses a list that the CRD would refuse. The patterns are not in the pod yet. The operator hashes the pod
template, and a template that changes replaces the sync pod (#87).

## Decision

The operator puts the two lists in the sync pod as environment variables, as it does for the other settings
(ADR 0018, ADR 0021), after `TREEBIND_POD_NAME`:

| Variable | Meaning | Set when |
| :-- | :-- | :-- |
| `TREEBIND_PATHS_INCLUDE` | `spec.paths.include`, as a JSON array of strings | the list has a pattern |
| `TREEBIND_PATHS_EXCLUDE` | `spec.paths.exclude`, as a JSON array of strings | the list has a pattern |

A list that is empty is left out, as an interval that is not set is, so the pod template of a Treebind without
lists is the one the operator wrote before, byte for byte. A test holds the hash of such a template to the one
from before.

Go's `encoding/json` writes the lists. It escapes `<`, `>`, `&` and the control characters, and the agent reads
them back as they were. It would write a byte that is not UTF-8 as U+FFFD, but a string from the API server is
UTF-8. The kubelet expands `$(NAME)` and `$$` in the value of a variable, and a pattern may hold a `$`, so the
operator writes each `$` as the JSON escape `\u0024`, which the kubelet leaves as it is and the agent reads as
`$`. The CRD keeps the patterns of a list to 16384 characters in all, so the JSON, six bytes a character at most
with these escapes too, fits in one environment variable, which Linux limits to 128 KiB, with nearly 32 KiB to
spare.

A test builds the pod for lists that hold some 870 patterns that the agent takes, for the longest list the CRD
allows, and for lists of the characters that JSON escapes and of `$`, and holds what the agent reads from the pod
to what the spec has.

## Consequences

- Upgrading the operator replaces no sync pod of a Treebind without lists, and replaces the sync pod of a
  Treebind with lists once. The replacement is the agent restarting on the same claim, which it is built to do.
- A change of a list, and taking a list away, replaces the sync pod, because the agent reads the lists at start.
- Nothing in the agent uses the lists yet. The scan of #34 and the commit of #36 will, through the filter of
  ADR 0022.
