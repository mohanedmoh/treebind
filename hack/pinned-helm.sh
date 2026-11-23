# Sourced by the scripts that install the chart on a kind cluster (hack/test-chart.sh, hack/test-quickstart.sh), so that
# they use the same Helm. install_helm sets HELM to the Helm that release.yml pins, by version and SHA-256, which is the Helm
# that publishes the chart, and downloads it into HELM_DIR the first time. A HELM that is set already is kept. The download
# is for linux/amd64 only: set HELM on another platform. ci.yml checks that the pin of its chart job is the same.
#
# Environment:
#   HELM      a Helm to use instead of the one this downloads
#   HELM_DIR  where the pinned Helm is kept, bin/ of the checkout when unset

# pin prints the value that release.yml gives a key.
pin() {
  sed -n "s/^ *$1: *//p" "$(dirname "${BASH_SOURCE[0]}")/../.github/workflows/release.yml" | head -n 1
}

install_helm() {
  if [ -n "${HELM:-}" ]; then
    return
  fi
  local version sha tmp dir
  version="$(pin HELM_VERSION)"
  sha="$(pin HELM_SHA256)"
  if [ -z "$version" ] || [ -z "$sha" ]; then
    echo "FAIL: release.yml has no HELM_VERSION and HELM_SHA256" >&2
    return 1
  fi
  if [ "$(uname -sm)" != "Linux x86_64" ]; then
    echo "FAIL: the pinned Helm is for linux/amd64: set HELM to a Helm $version" >&2
    return 1
  fi
  dir="${HELM_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin}"
  HELM="$dir/helm-$version/helm"
  if [ ! -x "$HELM" ]; then
    tmp="$(mktemp -d)"
    curl -fsSLo "$tmp/helm.tar.gz" "https://get.helm.sh/helm-${version}-linux-amd64.tar.gz"
    echo "$sha  $tmp/helm.tar.gz" | sha256sum -c -
    tar -xzf "$tmp/helm.tar.gz" -C "$tmp" linux-amd64/helm
    mkdir -p "$(dirname "$HELM")"
    mv "$tmp/linux-amd64/helm" "$HELM"
    rm -rf "$tmp"
  fi
}
