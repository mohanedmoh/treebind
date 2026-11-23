#!/usr/bin/env bash
# Pushes charts/artifacthub-repo.yml to the OCI repository of the chart, at the tag artifacthub.io, which is where
# Artifact Hub looks for the metadata of a repository that is hosted in a registry. The file carries the ID that Artifact
# Hub gave the repository, and with it the listing of the chart is a verified publisher.
#
# Usage: hack/push-artifacthub-repo.sh REPOSITORY    (for example ghcr.io/mohanedmoh/charts/treebind)
#
# The push is of the same file every time, so it can be repeated. oras has to be logged in to the registry with a
# credential that may write the package. Environment: ORAS is the oras to run, `oras` when unset, and ORAS_FLAGS are flags
# that are added to the push, such as --plain-http for a registry on localhost.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 REPOSITORY (for example ghcr.io/mohanedmoh/charts/treebind)" >&2
  exit 2
fi
repository="$1"
oras="${ORAS:-oras}"
root="$(cd "$(dirname "$0")/.." && pwd)"
file=artifacthub-repo.yml

if ! grep -Eq '^repositoryID: [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' "$root/charts/$file"; then
  echo "push-artifacthub-repo: charts/$file has no repositoryID that is a UUID" >&2
  exit 1
fi

read -r -a flags <<<"${ORAS_FLAGS:-}"
# The file is named by its path from the directory, which is what its layer is called.
cd "$root/charts"
"$oras" push "${flags[@]}" "$repository:artifacthub.io" \
  --config /dev/null:application/vnd.cncf.artifacthub.config.v1+yaml \
  "$file:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml"
