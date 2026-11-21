#!/usr/bin/env bash
# Causes a failure in a container of a pod, from the node that runs it, for the end-to-end specs that test how the sync
# recovers (the label failure in test/e2e) and for a contributor who wants to see it on `make dev`.
#
# Usage: hack/inject.sh kill|freeze|thaw <namespace> <pod> [container]
#        hack/inject.sh cleanup
#
#   kill     sends SIGKILL to the first process of the container, as an out-of-memory kill does. The kubelet starts the
#            container again in the same pod.
#   freeze   freezes every process of the container with the freezer of cgroup v2, and thaw lets them run again.
#   cleanup  removes the privileged pods and their namespace.
#
# The container is the first of the pod when none is named. The first process of a container is PID 1 of its PID
# namespace, and the kernel does not let a process of that namespace send it SIGKILL or SIGSTOP: on kind, `kubectl exec
# <sync pod> -- sh -c 'kill -9 1; kill -STOP 1'` exits 0 and the agent runs on, ready and not restarted. A process in a
# parent namespace can, so the verbs run in a privileged pod with hostPID on the node of the pod, in the namespace
# INJECT_NAMESPACE (treebind-inject). That pod is started when there is none and stops by itself after three hours;
# cleanup removes it sooner. The verbs find the container by the ID in the status of the pod, in /proc/<pid>/cgroup of
# the node's processes, and its cgroup under /sys/fs/cgroup, which a privileged pod sees whole. The script needs
# kubectl, a node with cgroup v2 and a cluster that admits a privileged pod with hostPID, as kind does.
#
# A privileged pod that kills processes is for a throwaway cluster, so the script refuses to run unless the current
# context of kubectl is a kind cluster (a name that begins with kind-). INJECT_ANY_CLUSTER=yes lets it run on another.
# KUBECTL names the kubectl to use, and INJECT_IMAGE the image of the privileged pod (busybox).
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
INJECT_NAMESPACE="${INJECT_NAMESPACE:-treebind-inject}"
INJECT_IMAGE="${INJECT_IMAGE:-busybox}"

usage() {
  echo "usage: $0 kill|freeze|thaw <namespace> <pod> [container]" >&2
  echo "       $0 cleanup" >&2
  exit 2
}

fail() {
  echo "inject: $*" >&2
  exit 1
}

check_cluster() {
  local context
  context="$("$KUBECTL" config current-context 2>/dev/null || true)"
  case "$context" in
  kind-*) ;;
  *)
    [ "${INJECT_ANY_CLUSTER:-}" = yes ] ||
      fail "the context of kubectl is '$context', not a kind cluster (kind-*); INJECT_ANY_CLUSTER=yes runs it anyway"
    ;;
  esac
}

# injector prints the name of the privileged pod on a node, and starts it first when it is not running.
injector() {
  local node="$1" pod phase
  pod="inject-$node"
  phase="$("$KUBECTL" -n "$INJECT_NAMESPACE" get pod "$pod" -o 'jsonpath={.status.phase}' --ignore-not-found)"
  if [ "$phase" = Running ]; then
    echo "$pod"
    return
  fi
  if [ -n "$phase" ]; then
    # It has ended, after its three hours or with its node.
    "$KUBECTL" -n "$INJECT_NAMESPACE" delete pod "$pod" --wait=true >&2
  fi
  # The label lets the namespace hold a privileged pod also on a cluster whose Pod Security admission enforces a
  # level by default. busybox, whose shell, grep, sed and kill the verbs use, is pulled only when it is not on the node.
  "$KUBECTL" apply -f - >&2 <<POD
apiVersion: v1
kind: Namespace
metadata:
  name: $INJECT_NAMESPACE
  labels:
    app.kubernetes.io/name: treebind-inject
    pod-security.kubernetes.io/enforce: privileged
---
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $INJECT_NAMESPACE
  labels:
    app.kubernetes.io/name: treebind-inject
spec:
  nodeName: $node
  hostPID: true
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  tolerations:
  - operator: Exists
  containers:
  - name: inject
    image: $INJECT_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sleep", "10800"]
    securityContext:
      privileged: true
POD
  "$KUBECTL" -n "$INJECT_NAMESPACE" wait --for=condition=Ready "pod/$pod" --timeout=180s >&2
  echo "$pod"
}

# The script that runs in the privileged pod, with the verb, the ID of the container and the name of the node. It finds
# the processes whose cgroup names the container, and of them the one whose PID in its own namespace (the last of
# NSpid) is 1: the other processes of the container are what it started, such as a Git.
# shellcheck disable=SC2016
node_script='
set -eu
verb="$1" id="$2" node="$3"
first=""
for file in /proc/[0-9]*/cgroup; do
  grep -q "$id" "$file" 2>/dev/null || continue
  pid="${file#/proc/}"
  pid="${pid%/cgroup}"
  nspid="$(sed -n "s/^NSpid:[[:space:]]*//p" "/proc/$pid/status" 2>/dev/null)" || continue
  if [ "${nspid##*[[:space:]]}" = 1 ]; then
    first="$pid"
    break
  fi
done
[ -n "$first" ] || { echo "inject: no first process of the container ${id} on the node ${node}" >&2; exit 1; }
cgroup="$(sed -n "s/^0:://p" "/proc/$first/cgroup")"
dir="/sys/fs/cgroup$cgroup"
case "$verb" in
kill)
  kill -KILL "$first"
  for _ in $(seq 1 50); do
    [ -d "/proc/$first" ] || break
    sleep 0.2
  done
  [ ! -d "/proc/$first" ] || { echo "inject: pid $first on the node ${node} is still there after SIGKILL" >&2; exit 1; }
  echo "killed pid $first (pid 1 of the container ${id}) on the node ${node}, cgroup ${cgroup}"
  ;;
freeze | thaw)
  [ -n "$cgroup" ] && [ -f "$dir/cgroup.freeze" ] ||
    { echo "inject: no freezer of cgroup v2 for the container ${id} (cgroup \"${cgroup}\")" >&2; exit 1; }
  want=1
  [ "$verb" = freeze ] || want=0
  echo "$want" >"$dir/cgroup.freeze"
  for _ in $(seq 1 50); do
    grep -qx "frozen $want" "$dir/cgroup.events" && break
    sleep 0.2
  done
  grep -qx "frozen $want" "$dir/cgroup.events" ||
    { echo "inject: the cgroup ${cgroup} did not get to frozen ${want}" >&2; exit 1; }
  echo "${verb} of the container ${id} (pid $first) on the node ${node}: cgroup ${cgroup} is frozen ${want}"
  ;;
esac
'

verb="${1:-}"
case "$verb" in
cleanup)
  [ $# -eq 1 ] || usage
  check_cluster
  "$KUBECTL" delete namespace "$INJECT_NAMESPACE" --ignore-not-found --wait=true --timeout=3m
  ;;
kill | freeze | thaw)
  [ $# -eq 3 ] || [ $# -eq 4 ] || usage
  check_cluster
  namespace="$2" pod="$3" container="${4:-}"
  if [ -z "$container" ]; then
    container="$("$KUBECTL" -n "$namespace" get pod "$pod" -o 'jsonpath={.spec.containers[0].name}')"
  fi
  node="$("$KUBECTL" -n "$namespace" get pod "$pod" -o 'jsonpath={.spec.nodeName}')"
  id="$("$KUBECTL" -n "$namespace" get pod "$pod" \
    -o "jsonpath={.status.containerStatuses[?(@.name==\"$container\")].containerID}")"
  # The ID is that of the runtime after its scheme, as in containerd://<64 hex digits>.
  id="${id##*://}"
  [[ "$node" =~ ^[a-z0-9.-]+$ ]] || fail "the pod $namespace/$pod is not on a node"
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || fail "the container $container of the pod $namespace/$pod has not started (ID '$id')"
  injecting="$(injector "$node")"
  "$KUBECTL" -n "$INJECT_NAMESPACE" exec "$injecting" -- sh -c "$node_script" sh "$verb" "$id" "$node"
  ;;
*)
  usage
  ;;
esac
