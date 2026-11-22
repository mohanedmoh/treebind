#!/usr/bin/env bash
# Installs the Helm chart on a kind cluster and proves that a Treebind syncs through it, which
# is what the kustomize deployment is held to by the definition of done of test/e2e. It builds
# the operator and the agent image from the checkout with ko into the cluster, installs the
# chart with the images set, checks the operator and the CRD, syncs a Treebind against the ssh
# remote of hack/dev-ssh.sh in both directions, upgrades with the same values, and uninstalls.
#
# Usage: hack/test-chart.sh      (make test-chart makes the cluster, runs this and deletes it)
#
# The cluster has to be fresh: no Treebind CRD and no namespace treebind-system, so that what
# the chart makes is all that there is. Environment:
#   KIND_CLUSTER  the kind cluster, treebind-test-e2e when unset
#   KIND, KO, KUBECTL  the programs, `kind`, bin/ko and `kubectl` when unset
#   HELM          a Helm to use instead of the one this downloads
#   HELM_DIR      where the pinned Helm is kept, bin/ of the checkout when unset
# The Helm that this downloads is the one that release.yml pins, by version and SHA-256, and it
# is for linux/amd64 only: set HELM on another platform.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

CLUSTER="${KIND_CLUSTER:-treebind-test-e2e}"
KIND="${KIND:-kind}"
KO="${KO:-$root/bin/ko}"
KUBECTL="${KUBECTL:-kubectl}"
HELM_DIR="${HELM_DIR:-$root/bin}"
export KUBECTL

RELEASE=treebind
NAMESPACE=treebind-system
CRD=treebinds.treebind.example.org
APP_NAMESPACE=chart-smoke
REMOTE_NAMESPACE=treebind-dev
REMOTE_REPOSITORY=/srv/sample.git
BRANCH=main
# Which images the cluster has, and what the chart passes them as, is set by build_images.
OPERATOR_IMAGE=
AGENT_IMAGE=

started=$SECONDS
step() { printf '\n=== [%ss] %s\n' "$((SECONDS - started))" "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# retry for up to $1 seconds until the command that follows succeeds.
wait_for() {
  local limit="$1" what="$2" deadline
  shift 2
  deadline=$((SECONDS + limit))
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "gave up after ${limit}s waiting for $what"
    sleep 2
  done
}

# The pin of Helm is the one of release.yml, which is the Helm that publishes the chart, and
# ci.yml checks that the pin of its chart job is the same.
pin() { sed -n "s/^ *$1: *//p" .github/workflows/release.yml | head -n 1; }

install_helm() {
  if [ -n "${HELM:-}" ]; then
    return
  fi
  local version sha tmp
  version="$(pin HELM_VERSION)"
  sha="$(pin HELM_SHA256)"
  [ -n "$version" ] && [ -n "$sha" ] || fail "release.yml has no HELM_VERSION and HELM_SHA256"
  [ "$(uname -sm)" = "Linux x86_64" ] || fail "the pinned Helm is for linux/amd64: set HELM to a Helm $version"
  HELM="$HELM_DIR/helm-$version/helm"
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

# What the kubectl, the Helm and hack/dev-ssh.sh say they did on a failure: the state of the
# chart's objects and of the Treebind, and the logs.
diagnose() {
  echo
  echo "=== what the cluster says"
  "$HELM" status "$RELEASE" -n "$NAMESPACE" || true
  "$KUBECTL" -n "$NAMESPACE" get all,pdb -o wide || true
  "$KUBECTL" -n "$NAMESPACE" describe deployment || true
  "$KUBECTL" -n "$NAMESPACE" logs -l control-plane=controller-manager --prefix --tail=40 || true
  "$KUBECTL" -n "$APP_NAMESPACE" get treebind,statefulset,pod,pvc -o wide || true
  "$KUBECTL" -n "$APP_NAMESPACE" describe treebind || true
  "$KUBECTL" -n "$APP_NAMESPACE" describe pod || true
  "$KUBECTL" -n "$APP_NAMESPACE" logs smoke-sync-0 --tail=60 || true
  "$KUBECTL" get events -A --sort-by=.lastTimestamp | tail -40 || true
}

# Takes away what a run leaves in the cluster, so that the script can run on the same cluster
# again: the release, the Treebind with its sync pod, the CRD that the chart keeps, and the
# namespaces. A cluster that the target made is deleted afterwards.
cleanup() {
  local status=$?
  set +e
  [ "$status" -eq 0 ] || diagnose
  "$HELM" uninstall "$RELEASE" -n "$NAMESPACE" --wait --timeout 120s >/dev/null 2>&1
  "$KUBECTL" delete treebind --all -n "$APP_NAMESPACE" --timeout=120s >/dev/null 2>&1
  "$KUBECTL" delete crd "$CRD" --ignore-not-found --timeout=120s >/dev/null 2>&1
  "$KUBECTL" delete namespace "$APP_NAMESPACE" "$REMOTE_NAMESPACE" "$NAMESPACE" --ignore-not-found --timeout=180s >/dev/null 2>&1
  rm -rf "${scratch:-}"
  exit "$status"
}

# The operator and the agent image, built from the checkout by ko as the release builds them, into
# the nodes of the cluster, which needs no registry. ko prints kind.local/<name>:<tag>, and the chart
# takes the repository and the tag apart. A node that has an image with a tag other than latest
# never pulls it.
build_images() {
  local platform
  platform="linux/$(go env GOARCH)"
  OPERATOR_IMAGE="$(KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "$KO" build --base-import-paths --platform="$platform" ./cmd/operator | tail -n 1)"
  AGENT_IMAGE="$(KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME="$CLUSTER" "$KO" build --base-import-paths --platform="$platform" ./cmd/agent | tail -n 1)"
  [[ "$OPERATOR_IMAGE" == kind.local/* && "$AGENT_IMAGE" == kind.local/* ]] ||
    fail "ko did not print the references of the images: '$OPERATOR_IMAGE' '$AGENT_IMAGE'"
  echo "operator image: $OPERATOR_IMAGE"
  echo "agent image:    $AGENT_IMAGE"
}

# The Treebind of the smoke test: a claim, the Treebind on the ssh remote that hack/dev-ssh.sh
# makes keys for, and the pod of an application that mounts the claim as an application does.
manifest() {
  cat <<MANIFEST
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: smoke-data, namespace: $APP_NAMESPACE}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 100Mi}}
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata: {name: smoke, namespace: $APP_NAMESPACE}
spec:
  repository:
    url: ssh://git@git-remote-ssh.$REMOTE_NAMESPACE.svc.cluster.local$REMOTE_REPOSITORY
    branch: $BRANCH
    auth:
      sshKeySecretRef: {name: dev-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: dev-known-hosts, key: known_hosts}
  target: {claimName: smoke-data, subPath: dashboards}
  sync: {direction: bidirectional, interval: 5s, settleTime: 1s}
---
apiVersion: v1
kind: Pod
metadata: {name: smoke-app, namespace: $APP_NAMESPACE}
spec:
  containers:
  - name: app
    image: busybox
    command: ["sleep", "3600"]
    volumeMounts: [{name: data, mountPath: /data, subPath: dashboards}]
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: smoke-data}
MANIFEST
}

# The status of one condition of the Treebind, or nothing when there is none yet.
condition() {
  "$KUBECTL" -n "$APP_NAMESPACE" get treebind smoke -o "jsonpath={.status.conditions[?(@.type==\"$1\")].status}" 2>/dev/null || true
}

remote() {
  "$KUBECTL" -n "$REMOTE_NAMESPACE" exec deploy/git-remote-ssh -- su git -c "$1"
}

remote_files() { remote "git -C $REMOTE_REPOSITORY ls-tree -r --name-only $BRANCH"; }
remote_has() { remote_files | grep -qx "$1"; }
remote_tip() { remote "git -C $REMOTE_REPOSITORY rev-parse --verify refs/heads/$BRANCH"; }

# What the operator says of the Treebind once the files agree: it is Ready, Synced, and the commit that
# the volume and the repository last agreed on is the tip of the remote. The operator reads the report
# of the agent every 30 seconds, and a first poll that finds the pod not up yet is tried again a minute
# later, so this takes a few minutes after the pod of the sync is up.
synced_at_the_tip() {
  [ "$(condition Ready)" = True ] && [ "$(condition Synced)" = True ] &&
    [ "$("$KUBECTL" -n "$APP_NAMESPACE" get treebind smoke -o jsonpath='{.status.lastSyncedCommit}')" = "$(remote_tip)" ]
}

operator_pods() {
  "$KUBECTL" -n "$NAMESPACE" get pods -l control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort
}

install_helm

scratch="$(mktemp -d)"
export KUBECONFIG="$scratch/kubeconfig"
"$KIND" export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"

step "checking that the cluster is fresh"
if "$KUBECTL" get crd "$CRD" >/dev/null 2>&1 || "$KUBECTL" get namespace "$NAMESPACE" >/dev/null 2>&1; then
  rm -rf "$scratch"
  fail "the CRD $CRD or the namespace $NAMESPACE is in the cluster already: run this on a fresh cluster"
fi
# From here on, what this makes is taken away again, and nothing else.
trap cleanup EXIT

step "building the operator and the agent image into the cluster"
build_images
values=(
  --set "manager.image.repository=${OPERATOR_IMAGE%:*}" --set "manager.image.tag=${OPERATOR_IMAGE##*:}"
  --set "agent.image.repository=${AGENT_IMAGE%:*}" --set "agent.image.tag=${AGENT_IMAGE##*:}"
)

step "installing the chart"
"$HELM" install "$RELEASE" charts/treebind --namespace "$NAMESPACE" --create-namespace "${values[@]}" --wait --timeout 180s
"$KUBECTL" wait --for=condition=Established "crd/$CRD" --timeout=60s
"$KUBECTL" -n "$NAMESPACE" rollout status deployment/treebind-controller-manager --timeout=120s
ready="$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.status.readyReplicas}')"
[ "$ready" = 2 ] || fail "the operator has $ready ready replicas, want 2"
agent="$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AGENT_IMAGE")].value}')"
[ "$agent" = "$AGENT_IMAGE" ] || fail "the operator has the agent image '$agent', want '$AGENT_IMAGE'"

step "starting the ssh remote and seeding its repository"
"$KUBECTL" create namespace "$APP_NAMESPACE"
hack/dev-ssh.sh "$APP_NAMESPACE" >/dev/null
remote "git init -q --initial-branch=$BRANCH /tmp/seed && cd /tmp/seed &&
  git config user.name dev && git config user.email dev@example.com &&
  echo hello > hello.txt && git add -A && git commit -q -m first && git push -q $REMOTE_REPOSITORY $BRANCH"

step "applying a Treebind, its claim and a pod that mounts it"
manifest | "$KUBECTL" apply -f -

step "syncing in both directions through the operator of the chart"
# The seeded file comes to the pod of the application: the pull. A file that the application writes is a
# commit on the remote: the push. Then the status of the Treebind names the tip of the remote.
wait_for 240 "the seeded file in the pod of the application" \
  "$KUBECTL" -n "$APP_NAMESPACE" exec smoke-app -- grep -qx hello /data/hello.txt
"$KUBECTL" -n "$APP_NAMESPACE" exec smoke-app -- sh -c 'echo written by the application > /data/from-the-app.txt'
wait_for 60 "the file of the application on the remote" remote_has from-the-app.txt
wait_for 300 "the status to say that the Treebind is Ready and Synced at the tip of the remote" synced_at_the_tip

step "upgrading with the same values"
before="$(operator_pods)"
generation="$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.metadata.generation}')"
"$HELM" upgrade "$RELEASE" charts/treebind --namespace "$NAMESPACE" "${values[@]}" --wait --timeout 180s
[ "$(operator_pods)" = "$before" ] || fail "an upgrade with the same values replaced the pods of the operator"
[ "$("$KUBECTL" -n "$NAMESPACE" get deployment treebind-controller-manager -o jsonpath='{.metadata.generation}')" = "$generation" ] ||
  fail "an upgrade with the same values changed the Deployment of the operator"

# The chart keeps the CRD when the release goes (crd.keep), and with it the Treebinds, their sync pods and
# every claim. Everything else that the release made has to go.
step "uninstalling"
"$HELM" get manifest "$RELEASE" -n "$NAMESPACE" >"$scratch/manifest.yaml"
"$HELM" uninstall "$RELEASE" -n "$NAMESPACE" --wait --timeout 120s
left="$("$KUBECTL" get -f "$scratch/manifest.yaml" --ignore-not-found -o name)"
[ "$left" = "customresourcedefinition.apiextensions.k8s.io/$CRD" ] ||
  fail "after the uninstall the release has left '$left', want only the CRD"
"$KUBECTL" -n "$APP_NAMESPACE" get treebind smoke >/dev/null || fail "the Treebind went with the release"
"$KUBECTL" -n "$APP_NAMESPACE" get pod smoke-sync-0 >/dev/null || fail "the sync pod went with the release"

step "installing the chart again over the CRD that was kept"
"$HELM" install "$RELEASE" charts/treebind --namespace "$NAMESPACE" "${values[@]}" --wait --timeout 180s
"$KUBECTL" -n "$NAMESPACE" rollout status deployment/treebind-controller-manager --timeout=120s

step "passed"
