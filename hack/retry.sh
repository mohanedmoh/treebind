#!/usr/bin/env bash
# Retries a command with exponential backoff. Source it, then call retry:
#
#   source hack/retry.sh
#   retry 4 some-command arg...
#
# Right after GHCR creates a new package it can briefly refuse the next write
# with "denied: requested access to the resource is denied", and registries
# return the occasional 5xx. Retrying an idempotent push covers both.
#
# The first wait is RETRY_DELAY seconds (default 15) and doubles on each retry.
# Messages go to stderr, so a command substitution captures only the command's
# own output.

retry() {
  local attempts="$1" delay="${RETRY_DELAY:-15}" n=1
  shift
  until "$@"; do
    if [ "$n" -ge "$attempts" ]; then
      echo "::error::Giving up after ${n} attempts: $*" >&2
      return 1
    fi
    echo "::warning::Attempt ${n} of ${attempts} failed: $*. Retrying in ${delay}s." >&2
    sleep "$delay"
    n=$((n + 1))
    delay=$((delay * 2))
  done
}
