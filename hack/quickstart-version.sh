#!/usr/bin/env bash
# Prints the version of the chart that the quickstart installs: the --version of the line of docs/quickstart.md that
# installs oci://ghcr.io/mohanedmoh/charts/treebind, which is the version of a release without its leading v. The release
# workflow holds it to the tag that it publishes, and hack/test-quickstart.sh installs the chart of the checkout in its place.
# It fails when the page installs that chart on no line or on more than one, or names a version that no tag could be.
#
# Usage: hack/quickstart-version.sh
set -euo pipefail

page="$(cd "$(dirname "$0")/.." && pwd)/docs/quickstart.md"
versions="$(sed -n 's|.*helm install treebind oci://ghcr\.io/mohanedmoh/charts/treebind --version \([^ ]*\).*|\1|p' "$page")"
count="$(printf '%s' "$versions" | grep -c '' || true)"
if [ "$count" != 1 ]; then
  echo "docs/quickstart.md has to install oci://ghcr.io/mohanedmoh/charts/treebind with --version on one line, and does on $count" >&2
  exit 1
fi
# The versions that release.yml accepts for a tag, without the v.
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.[0-9]+)?$'
if ! [[ "$versions" =~ $semver ]]; then
  echo "docs/quickstart.md installs the version '$versions', which is not the version of a release: MAJOR.MINOR.PATCH, optionally with -alpha.N, -beta.N or -rc.N" >&2
  exit 1
fi
echo "$versions"
