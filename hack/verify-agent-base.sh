#!/usr/bin/env bash
# Verifies that Chainguard signed the base image of the agent, as .ko.yaml pins it.
#
# Usage: hack/verify-agent-base.sh
#
# The pin is a digest, so it names one exact image. The signature adds where that
# image came from: Chainguard's release workflow, which signs keylessly with the
# identity of the workflow run. Needs cosign on the PATH and network access to
# cgr.dev and the Sigstore services.
set -euo pipefail

# The identity of the signing workflow run, as the certificate names it. If
# Chainguard renames or moves the workflow, the check fails until this is updated
# on purpose, after finding out why.
issuer="https://token.actions.githubusercontent.com"
identity="https://github.com/chainguard-images/images/.github/workflows/release.yaml@refs/heads/main"
config="${KO_CONFIG:-$(dirname "$0")/../.ko.yaml}"

base="$(awk '$1 == "github.com/mohanedmoh/treebind/cmd/agent:" { print $2 }' "$config")"
if ! [[ "$base" =~ ^cgr\.dev/chainguard/git@sha256:[0-9a-f]{64}$ ]]; then
  echo "$config does not pin the agent's base image to a cgr.dev/chainguard/git digest: '$base'" >&2
  exit 1
fi

cosign verify "$base" \
  --certificate-oidc-issuer "$issuer" \
  --certificate-identity "$identity" > /dev/null
echo "Chainguard signed $base"
