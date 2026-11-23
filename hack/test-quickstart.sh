#!/usr/bin/env bash
# Runs the quickstart (docs/quickstart.md) and the worked examples (docs/examples/) on a kind cluster as a reader does, by
# copying their commands, so that the pages cannot say what does not work.
#
# Each command of those pages is in a fenced sh block that follows a line <!-- tested: name -->. This script takes the
# blocks out of the pages and runs each, as it is written, in a bash of its own, all in one working directory and in the
# order of the pages, and checks in between what the page says that the reader sees. A block that shows what a command
# prints follows a line <!-- output: name -->, and is held to what the command printed, with commit ids, times and ages left
# out and runs of spaces taken as one; a line "..." in it stands for lines that the page leaves out. A block that a reader
# runs until it shows a change is run here until it does. Every sh block of the pages has to be marked tested or
# <!-- not tested: why -->, and every block and output that is marked has to be run and held here, so that a command that is
# added to a page is tested or says why it is not.
#
# The pages install the published chart, whose images are the ones of a release, and a pull request is not a release. So
# by default this builds the operator and the agent image from the checkout with ko into the nodes of the cluster, as
# hack/test-chart.sh does, and puts first on the PATH of the blocks a helm that runs the pinned Helm with the same arguments,
# except in `helm install` and `helm upgrade`: there the published chart has to be oci://ghcr.io/mohanedmoh/charts/treebind
# with the version of the page (hack/quickstart-version.sh), and is replaced by charts/treebind, with --set for the two
# images. Nothing else of any command changes. With RELEASED=1 there is no build and no such helm: the blocks run exactly as
# the pages say, against the published chart and images, which is how a release is checked (docs/releasing.md).
#
# Usage: hack/test-quickstart.sh      (make test-quickstart makes the cluster, runs this and deletes it)
#
# The cluster has to be fresh: no Treebind CRD and none of the namespaces that the pages make. The blocks that make and
# delete a kind cluster are not run, because the cluster is made before. Environment:
#   KIND_CLUSTER  the kind cluster, treebind-test-e2e when unset
#   KIND, KO, KUBECTL  the programs, `kind`, bin/ko and `kubectl` when unset
#   HELM, HELM_DIR     as for hack/pinned-helm.sh
#   RELEASED      1 to run the pages against the published release, as above
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

CLUSTER="${KIND_CLUSTER:-treebind-test-e2e}"
KIND="${KIND:-kind}"
KO="${KO:-$root/bin/ko}"
KUBECTL="${KUBECTL:-kubectl}"
RELEASED="${RELEASED:-}"
case "$RELEASED" in
  "" | 0 | 1) ;;
  *) echo "FAIL: RELEASED is '$RELEASED': set it to 1 to run the pages against the published release, or leave it unset" >&2; exit 1 ;;
esac
# shellcheck source=hack/pinned-helm.sh
source "$root/hack/pinned-helm.sh"

QUICKSTART=docs/quickstart.md
DASHBOARDS=docs/examples/dashboards-edited-in-the-application.md
SITE=docs/examples/a-static-site-served-by-nginx.md
MONOREPO=docs/examples/one-directory-of-a-monorepo.md
PAGES=("$QUICKSTART" "$DASHBOARDS" "$SITE" "$MONOREPO")

PUBLISHED_CHART=oci://ghcr.io/mohanedmoh/charts/treebind
NAMESPACE=treebind-system
CRD=treebinds.treebind.example.org
# The namespaces that the pages make.
PAGE_NAMESPACES=(demo git-server)

# How long a reader waits, at most, for a change that they run a command again for: the settle time, a scan or an interval,
# and what a cluster that is not quick adds.
CHANGE_BOUND=90

started=$SECONDS
step() { printf '\n=== [%ss] %s\n' "$((SECONDS - started))" "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# block PAGE NAME prints the sh block of the page that follows the line <!-- tested: NAME -->, and nothing when there is
# none or it is not closed.
block() {
  awk -v marker="<!-- tested: $2 -->" '
    state == 2 && /^```/ { closed = 1; exit }
    state == 2 { text = text $0 "\n"; next }
    state == 1 { if ($0 == "```sh") { state = 2; next } exit }
    $0 == marker { state = 1 }
    END { if (closed) printf "%s", text }
  ' "$1"
}

# output PAGE NAME prints the block of the page that follows the line <!-- output: NAME -->.
output() {
  awk -v marker="<!-- output: $2 -->" '
    state == 2 && /^```/ { closed = 1; exit }
    state == 2 { text = text $0 "\n"; next }
    state == 1 { if ($0 == "```") { state = 2; next } exit }
    $0 == marker { state = 1 }
    END { if (closed) printf "%s", text }
  ' "$1"
}

# normalize leaves out of a text what differs from one run to the next: commit ids (seven to forty hexadecimal digits with
# a digit among them), times and ages become placeholders, a run of spaces becomes one space, and a line has none at its
# ends.
normalize() {
  perl -pe '
    s/\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z/<time>/g;
    s/\b(?=[0-9a-f]*[0-9])[0-9a-f]{7,40}\b/<commit>/g;
    s/\b(?:\d+[smhd])+\b/<age>/g;
    s/[ \t]+/ /g; s/^ //; s/ $//;
  '
}

# holds PAGE NAME FILE checks that what a command printed, in FILE, is what the output NAME of the page shows: the lines
# between two lines "..." of the output have to be lines of FILE one after the other, and in the order of the output.
holds() {
  local shown
  shown="$(output "$1" "$2")"
  [ -n "$shown" ] || fail "$1 has no output marked $2"
  printf '%s\n' "$1:$2" >>"$scratch/held"
  printf '%s' "$shown" | normalize >"$scratch/shown"
  normalize <"$3" >"$scratch/printed"
  awk '
    function place(   i, j, same) {
      for (i = next_line; i + size - 1 <= count; i++) {
        same = 1
        for (j = 1; j <= size; j++) if (printed[i + j - 1] != run[j]) { same = 0; break }
        if (same) { next_line = i + size; size = 0; return }
      }
      print "these lines of the page are not in what the command printed, one after the other and in this order:" > "/dev/stderr"
      for (j = 1; j <= size; j++) print "  " run[j] > "/dev/stderr"
      failed = 1; size = 0
    }
    BEGIN { next_line = 1 }
    FILENAME == ARGV[1] { printed[++count] = $0; next }
    $0 == "..." { if (size) place(); next }
    { run[++size] = $0 }
    END { if (size) place(); exit failed }
  ' "$scratch/printed" "$scratch/shown" || fail "what the block printed is not what the output $2 of $1 shows"
}

# run PAGE NAME [OUTPUT] runs the block in a bash of its own, in the working directory of the blocks, with the helm of this
# script first on its PATH, and fails when it fails. What it printed is kept in $scratch/last, and held to the output of
# the page called OUTPUT when one is given.
run() {
  local page="$1" name="$2" text
  text="$(block "$page" "$name")"
  [ -n "$text" ] || fail "$page has no sh block marked tested: $name, or it is not closed"
  printf '%s\n' "$page:$name" >>"$scratch/ran"
  step "$page: $name"
  printf '%s\n' "$text" | sed 's/^/    /'
  if ! (cd "$work" && PATH="$shim:$PATH" bash -euo pipefail -c "$text") >"$scratch/last" 2>"$scratch/last.err"; then
    cat "$scratch/last" "$scratch/last.err"
    fail "the block $name of $page failed"
  fi
  cat "$scratch/last" "$scratch/last.err"
  if [ -n "${3:-}" ]; then
    holds "$page" "$3" "$scratch/last"
  fi
}

# run_until SECONDS PAGE NAME OUTPUT runs the block until it passes and prints what the output of the page shows, as a
# reader runs it again until the change is there, for up to SECONDS.
run_until() {
  local limit="$1" page="$2" name="$3" shown="$4" deadline text
  deadline=$((SECONDS + limit))
  text="$(block "$page" "$name")"
  [ -n "$text" ] || fail "$page has no sh block marked tested: $name, or it is not closed"
  printf '%s\n' "$page:$name" >>"$scratch/ran"
  step "$page: $name (until it shows $shown)"
  printf '%s\n' "$text" | sed 's/^/    /'
  while true; do
    if (cd "$work" && PATH="$shim:$PATH" bash -euo pipefail -c "$text") >"$scratch/last" 2>"$scratch/last.err"; then
      (holds "$page" "$shown" "$scratch/last") 2>/dev/null && break
      if [ "$SECONDS" -ge "$deadline" ]; then
        cat "$scratch/last" "$scratch/last.err"
        holds "$page" "$shown" "$scratch/last"
      fi
    elif [ "$SECONDS" -ge "$deadline" ]; then
      cat "$scratch/last" "$scratch/last.err"
      fail "the block $name of $page still failed after ${limit}s"
    fi
    sleep 2
  done
  cat "$scratch/last" "$scratch/last.err"
  printf '%s\n' "$page:$shown" >>"$scratch/held"
}

# The images that the operator runs and that it gives the sync pods.
check_images() {
  local operator agent
  operator="$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}')"
  agent="$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AGENT_IMAGE")].value}')"
  [ "$operator" = "$OPERATOR_IMAGE" ] || fail "the operator runs the image '$operator', want '$OPERATOR_IMAGE'"
  [ "$agent" = "$AGENT_IMAGE" ] || fail "the operator has the agent image '$agent', want '$AGENT_IMAGE'"
  echo "operator image: $operator"
  echo "agent image:    $agent"
}

# Every sh block of the pages is marked, every block and output that is marked was run and held, and no name is used twice
# in a page.
check_coverage() {
  local page name pages
  pages="$(find docs/examples -name '*.md' | sort)"
  [ "$pages" = "$(printf '%s\n' "${PAGES[@]:1}" | sort)" ] ||
    fail "docs/examples has the pages '$pages', and this script runs '${PAGES[*]:1}': run every example here"
  for page in "${PAGES[@]}"; do
    awk -v page="$page" '
      /^```sh$/ && previous !~ /^<!-- (tested|not tested): .+ -->$/ { printf "%s:%d: an sh block that is not marked\n", page, FNR; bad = 1 }
      { previous = $0 }
      END { exit bad }
    ' "$page" || fail "$page has an sh block that is marked neither tested nor not tested"
    for kind in tested output; do
      name="$(sed -n "s/^<!-- $kind: \(.*\) -->$/\1/p" "$page" | sort | uniq -d)"
      [ -z "$name" ] || fail "$page marks more than one block $kind: $name"
    done
    while read -r name; do
      grep -qxF "$page:$name" "$scratch/ran" || fail "$page has the block $name, which this script does not run"
    done < <(sed -n 's/^<!-- tested: \(.*\) -->$/\1/p' "$page")
    while read -r name; do
      grep -qxF "$page:$name" "$scratch/held" || fail "$page has the output $name, which this script does not hold"
    done < <(sed -n 's/^<!-- output: \(.*\) -->$/\1/p' "$page")
  done
}

# What the cluster says on a failure: the operator, every Treebind and its sync pod, and the Git server.
diagnose() {
  echo
  echo "=== what the cluster says"
  "$HELM" status treebind -n "$NAMESPACE" || true
  "$KUBECTL" get pods -A -o wide || true
  "$KUBECTL" -n "$NAMESPACE" logs -l control-plane=controller-manager --prefix --tail=40 || true
  "$KUBECTL" -n demo describe treebind || true
  for pod in $("$KUBECTL" -n demo get pods -o name 2>/dev/null | grep -- '-sync-0$'); do
    "$KUBECTL" -n demo logs "$pod" --tail=60 || true
  done
  "$KUBECTL" -n git-server logs deploy/git-server --tail=20 || true
  "$KUBECTL" get events -A --sort-by=.lastTimestamp | tail -40 || true
}

# Takes away what a run leaves in the cluster, so that the script can run on the same cluster again. A run that passed has
# done it with the last block of the quickstart, and this finds nothing.
cleanup() {
  local status=$?
  set +e
  [ "$status" -eq 0 ] || diagnose
  "$KUBECTL" delete namespace "${PAGE_NAMESPACES[@]}" --ignore-not-found --timeout=180s >/dev/null 2>&1
  "$HELM" uninstall treebind -n "$NAMESPACE" --wait --timeout 120s >/dev/null 2>&1
  "$KUBECTL" delete crd "$CRD" --ignore-not-found --timeout=120s >/dev/null 2>&1
  "$KUBECTL" delete namespace "$NAMESPACE" --ignore-not-found --timeout=180s >/dev/null 2>&1
  rm -rf "${scratch:-}"
  exit "$status"
}

install_helm
VERSION="$(hack/quickstart-version.sh)"

scratch="$(mktemp -d)"
work="$scratch/work"
shim="$scratch/bin"
mkdir -p "$work" "$shim"
: >"$scratch/ran"
: >"$scratch/held"
export KUBECONFIG="$scratch/kubeconfig"
"$KIND" export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"

step "checking that the cluster is fresh"
for namespace in "$NAMESPACE" "${PAGE_NAMESPACES[@]}"; do
  if "$KUBECTL" get namespace "$namespace" >/dev/null 2>&1; then
    rm -rf "$scratch"
    fail "the namespace $namespace is in the cluster already: run this on a fresh cluster"
  fi
done
if "$KUBECTL" get crd "$CRD" >/dev/null 2>&1; then
  rm -rf "$scratch"
  fail "the CRD $CRD is in the cluster already: run this on a fresh cluster"
fi
# From here on, what this makes is taken away again, and nothing else.
trap cleanup EXIT

# The kubectl of the blocks is the one of this script.
ln -s "$(command -v "$KUBECTL")" "$shim/kubectl"
if [ "$RELEASED" = 1 ]; then
  step "running the pages against the published release $VERSION"
  OPERATOR_IMAGE="ghcr.io/mohanedmoh/treebind-operator:v$VERSION"
  AGENT_IMAGE="ghcr.io/mohanedmoh/treebind-agent:v$VERSION"
  ln -s "$HELM" "$shim/helm"
else
  step "building the operator and the agent image into the cluster"
  platform="linux/$(go env GOARCH)"
  OPERATOR_IMAGE="$(KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "$KO" build --base-import-paths --platform="$platform" ./cmd/operator | tail -n 1)"
  AGENT_IMAGE="$(KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "$KO" build --base-import-paths --platform="$platform" ./cmd/agent | tail -n 1)"
  [[ "$OPERATOR_IMAGE" == kind.local/* && "$AGENT_IMAGE" == kind.local/* ]] ||
    fail "ko did not print the references of the images: '$OPERATOR_IMAGE' '$AGENT_IMAGE'"
  # The helm of the blocks. It is written here, and reads what it replaces from the environment.
  export REAL_HELM="$HELM" PUBLISHED_CHART VERSION CHECKOUT_CHART="$root/charts/treebind" OPERATOR_IMAGE AGENT_IMAGE
  cat >"$shim/helm" <<'HELM'
#!/usr/bin/env bash
# Runs Helm as the block asks, except that the published chart of `helm install` and `helm upgrade` is the chart of the
# checkout, with the images that hack/test-quickstart.sh built.
set -euo pipefail
case "${1:-}" in
  install | upgrade) ;;
  *) exec "$REAL_HELM" "$@" ;;
esac
asked="$*"
args=()
chart=
version=
while [ $# -gt 0 ]; do
  case "$1" in
    "$PUBLISHED_CHART") chart=1; args+=("$CHECKOUT_CHART") ;;
    --version) version="${2:-}"; shift ;;
    *) args+=("$1") ;;
  esac
  shift
done
if [ -z "$chart" ] || [ "$version" != "$VERSION" ]; then
  echo "helm (hack/test-quickstart.sh): the page has to install $PUBLISHED_CHART --version $VERSION, and runs helm $asked" >&2
  exit 1
fi
exec "$REAL_HELM" "${args[@]}" \
  --set "manager.image.repository=${OPERATOR_IMAGE%:*}" --set "manager.image.tag=${OPERATOR_IMAGE##*:}" \
  --set "agent.image.repository=${AGENT_IMAGE%:*}" --set "agent.image.tag=${AGENT_IMAGE##*:}"
HELM
  chmod +x "$shim/helm"
fi

# The quickstart: the operator, the Git server, the one manifest, a file from the application on the branch, a commit from
# Git in the application, and the status after each.
run "$QUICKSTART" install
check_images
run "$QUICKSTART" make-the-key
run "$QUICKSTART" start-the-git-server
run "$QUICKSTART" read-the-host-key
grep -qE '^git-server\.git-server\.svc\.cluster\.local ssh-ed25519 [A-Za-z0-9+/=]+$' "$work/known_hosts" ||
  fail "known_hosts is not one ed25519 key of the Git server: $(cat "$work/known_hosts")"
run "$QUICKSTART" make-the-repository
run "$QUICKSTART" give-the-namespace-the-key
run "$QUICKSTART" apply-the-manifest
run "$QUICKSTART" wait-until-ready the-file-of-the-repository
run "$QUICKSTART" write-in-the-pod
run_until "$CHANGE_BOUND" "$QUICKSTART" see-the-commit the-commit-of-the-agent
run "$QUICKSTART" read-the-status the-status
pushed="$("$KUBECTL" -n demo get treebind quickstart -o jsonpath='{.status.lastSyncedCommit}')"
run "$QUICKSTART" push-from-git
run_until "$CHANGE_BOUND" "$QUICKSTART" see-the-file-in-the-pod the-file-from-git
run "$QUICKSTART" read-the-status the-status
[ "$("$KUBECTL" -n demo get treebind quickstart -o jsonpath='{.status.lastSyncedCommit}')" != "$pushed" ] ||
  fail "the status names the commit of the agent after the commit from Git was pulled"

# Dashboards that people edit in the application: the author and the message of the Treebind, a file that is not
# included, and a conflict that followLocal resolves.
run "$DASHBOARDS" make-the-repository
run "$DASHBOARDS" apply-the-manifest
run "$DASHBOARDS" wait-until-ready the-dashboard
run "$DASHBOARDS" save-a-dashboard
run_until "$CHANGE_BOUND" "$DASHBOARDS" see-the-commit the-commit-of-the-application
run "$DASHBOARDS" change-it-on-both-sides
run "$DASHBOARDS" wait-for-the-resolution
run "$DASHBOARDS" see-the-resolution the-resolution
run "$DASHBOARDS" read-the-conflict the-conflict-condition
run "$DASHBOARDS" clean-up

# A static site that Git deploys and nginx serves: a pull, and nothing committed.
run "$SITE" make-the-repository
run "$SITE" apply-the-manifest
run "$SITE" wait-until-ready version-1
run "$SITE" push-version-2
run_until "$CHANGE_BOUND" "$SITE" see-version-2 version-2
run "$SITE" see-the-branch the-branch
run "$SITE" read-the-status the-treebind
run "$SITE" clean-up

# One directory of a monorepo: the files of the directory only, a commit of the directory only, a file that is excluded.
run "$MONOREPO" make-the-repository
run "$MONOREPO" apply-the-manifest
run "$MONOREPO" wait-until-ready the-directory
run "$MONOREPO" change-the-configuration
run_until "$CHANGE_BOUND" "$MONOREPO" see-the-commit the-commit-of-the-application
run "$MONOREPO" push-from-git
run_until "$CHANGE_BOUND" "$MONOREPO" see-the-settings the-new-settings
run "$MONOREPO" clean-up

# The clean-up of the quickstart takes away everything that the pages made.
run "$QUICKSTART" clean-up
for namespace in "$NAMESPACE" "${PAGE_NAMESPACES[@]}"; do
  ! "$KUBECTL" get namespace "$namespace" >/dev/null 2>&1 || fail "the namespace $namespace is still there after the clean-up"
done
! "$KUBECTL" get crd "$CRD" >/dev/null 2>&1 || fail "the CRD is still there after the clean-up"

step "checking that every block of the pages was run"
check_coverage

step "passed"
