#!/usr/bin/env bash
# Renders the consolidated installer of a release (what `make build-installer` makes): config/default with the
# operator image set and, when an agent image is given, the agent image that the operator makes the sync pods from.
#
# Usage: hack/build-installer.sh OPERATOR_IMAGE AGENT_IMAGE OUTPUT
#
# The base sets AGENT_IMAGE to an image that no release publishes, so an installer that left it would start an
# operator whose sync pods cannot pull their image. With an agent image the script replaces it, and then reads the
# output back: the operator and the agent image have to be the ones given, and no image of the installer may have the
# tag latest. It stops at the first thing that is not so. Without an agent image (for a local build) the base is kept.
#
# Environment: KUSTOMIZE is the kustomize to run, `kustomize` when unset. config/manager/kustomization.yaml is edited,
# as `kustomize edit set image` always did, so run this from a checkout that can be discarded.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 OPERATOR_IMAGE AGENT_IMAGE OUTPUT (AGENT_IMAGE may be empty)" >&2
  exit 2
fi
operator="$1"
agent="$2"
output="$3"
kustomize="${KUSTOMIZE:-kustomize}"
root="$(cd "$(dirname "$0")/.." && pwd)"

fail() {
  echo "build-installer: $*" >&2
  exit 1
}

(
  cd "$root/config/manager"
  "$kustomize" edit set image "controller=$operator"
  if [ -n "$agent" ]; then
    # A strategic merge patch finds the container and the variable by their names.
    "$kustomize" edit add patch --kind Deployment --name controller-manager --patch "$(
      cat <<PATCH
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        - name: AGENT_IMAGE
          value: $agent
PATCH
    )"
  fi
)
mkdir -p "$(dirname "$output")"
"$kustomize" build "$root/config/default" >"$output"

[ -n "$agent" ] || exit 0

# What the installer says, read back.
unquote() { sed -e "s/^[\"']//" -e "s/[\"']\$//"; }
manager_image="$(awk '/^ *- name: AGENT_IMAGE$/ { found = 1 } found && /^ *image: / { print $2; exit }' "$output" | unquote)"
agent_image="$(awk 'found { sub(/^ *value: */, ""); print; exit } /^ *- name: AGENT_IMAGE$/ { found = 1 }' "$output" | unquote)"
[ "$(grep -c '^ *- name: AGENT_IMAGE$' "$output")" = 1 ] || fail "$output does not set AGENT_IMAGE exactly once"
[ "$agent_image" = "$agent" ] || fail "AGENT_IMAGE of $output is '$agent_image', want '$agent'"
[ "$manager_image" = "$operator" ] || fail "the image of the operator in $output is '$manager_image', want '$operator'"
if latest="$(grep -nE '^ *(image: |value: ).*:latest($|[[:space:]"'"'"'])' "$output")"; then
  fail "$output has an image with the tag latest, which a release does not publish: $latest"
fi
echo "build-installer: $output has the operator image $operator and the agent image $agent"
