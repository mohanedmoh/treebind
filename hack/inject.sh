#!/usr/bin/env bash
# Causes a failure in a container of a pod, from the node that runs it, for the end-to-end specs that test how the sync
# recovers (the label failure in test/e2e) and for a contributor who wants to see it on `make dev`.
#
# Usage: hack/inject.sh kill|freeze|thaw <namespace> <pod> [container]
#        hack/inject.sh kill-at <namespace> <pod> <container> <seconds> <marker>...
#        hack/inject.sh kill-git-at <namespace> <pod> <container> <seconds> <word> <marker>...
#        hack/inject.sh cleanup
#
#   kill         sends SIGKILL to the first process of the container, as an out-of-memory kill does. The kubelet starts the
#                container again in the same pod.
#   freeze       freezes every process of the container with the freezer of cgroup v2, and thaw lets them run again.
#   kill-at      kills the first process of the container in the moment that the markers say: +<path> is a file that has to
#                be there and -<path> one that must not, as the container sees them, such as +/work/.treebind/updating. It
#                looks at them through /proc/<pid>/root of that process, freezes the whole container when they all hold,
#                looks again, and kills it when they still hold. When the moment has passed while the container froze, it
#                thaws it and waits for the next one. It gives up after <seconds>.
#   kill-git-at  does the same to a Git that the first process started and whose arguments hold <word>, from inside the
#                container with kubectl exec: it stops that Git with SIGSTOP, looks again, and kills it with SIGKILL, or
#                lets it go on with SIGCONT. The first process runs on, and finds that its Git was killed.
#   cleanup      removes the privileged pods and their namespace.
#
# The container is the first of the pod when none is named. The first process of a container is PID 1 of its PID
# namespace, and the kernel does not let a process of that namespace send it SIGKILL or SIGSTOP: on kind, `kubectl exec
# <sync pod> -- sh -c 'kill -9 1; kill -STOP 1'` exits 0 and the agent runs on, ready and not restarted. A process in a
# parent namespace can, so the verbs other than kill-git-at run in a privileged pod with hostPID on the node of the pod,
# in the namespace INJECT_NAMESPACE (treebind-inject). That pod is started when there is none and stops by itself after
# three hours; cleanup removes it sooner. The verbs find the container by the ID in the status of the pod, in
# /proc/<pid>/cgroup of the node's processes, and its cgroup under /sys/fs/cgroup, which a privileged pod sees whole.
# The script needs kubectl, a node with cgroup v2 and a cluster that admits a privileged pod with hostPID, as kind does.
#
# kill-at and kill-git-at print a line that begins with "watching" once they look, so that a caller can cause the
# moment after it, and what they saw when they struck: how long they watched, how often they looked, how long the
# container took to freeze, and how many moments passed before one held still. They look in a loop
# of the shell's own commands, which start no program, so that a look takes microseconds and a moment of a fraction of
# a second is seen many times over.
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
  echo "       $0 kill-at <namespace> <pod> <container> <seconds> <marker>..." >&2
  echo "       $0 kill-git-at <namespace> <pod> <container> <seconds> <word> <marker>..." >&2
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

# check_markers refuses a marker that is not +<path> or -<path> with an absolute path, before anything is started.
check_markers() {
  [ $# -gt 0 ] || usage
  local marker
  for marker in "$@"; do
    [[ "$marker" =~ ^[+-]/[^[:space:]]+$ ]] || fail "the marker '$marker' is not +<path> or -<path> with an absolute path"
  done
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

# The watch of kill-at and kill-git-at, which both shells run: busybox's on the node and the one of the container. It
# uses only what a shell has built in, because the agent's image has no sleep, cat or date, and because a program that
# is started for each look would make a look take a millisecond and not microseconds. The caller sets root, the
# directory that the paths of the markers are in, and the functions hold, strike and release: hold stops what runs at
# the moment and fails when there is nothing to stop, strike kills it, and release lets it go on. The time is read from
# /proc/uptime in hundredths of a second, and the deadline is looked at every thousand looks.
# shellcheck disable=SC2016
watch_script='
tick() {
  read -r up _ </proc/uptime
  t=$((${up%.*} * 100 + 1${up#*.} - 100))
}
held() {
  for marker; do
    case "$marker" in
    +*) [ -e "$root${marker#+}" ] || return 1 ;;
    -*) [ ! -e "$root${marker#-}" ] || return 1 ;;
    esac
  done
}
watch() {
  tick
  start=$t deadline=$((t + seconds * 100)) looks=0 passed=0
  echo "watching for $* for up to ${seconds}s"
  while :; do
    looks=$((looks + 1))
    if held "$@" && hold; then
      if held "$@"; then
        tick
        seen=$((t - start))
        strike
        echo "struck after $((seen / 100)).$((seen % 100 / 10))s of watching, at look ${looks} (about $((seen * 10000 / looks))us a look), after ${passed} moment(s) that passed while ${stopping}"
        return 0
      fi
      release
      passed=$((passed + 1))
    fi
    if [ $((looks % 1000)) -eq 0 ]; then
      tick
      if [ "$t" -ge "$deadline" ]; then
        echo "inject: no moment held still within ${seconds}s: ${looks} looks, ${passed} moment(s) passed while ${stopping}" >&2
        return 1
      fi
    fi
  done
}
'

# The script that runs in the privileged pod, with the verb, the ID of the container and the name of the node, and for
# kill-at the seconds and the markers. It finds the processes whose cgroup names the container, and of them the one whose
# PID in its own namespace (the last of NSpid) is 1: the other processes of the container are what it started, such as
# a Git.
# shellcheck disable=SC2016
node_script='
set -eu
verb="$1" id="$2" node="$3"
shift 3
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
freezer() {
  [ -n "$cgroup" ] && [ -f "$dir/cgroup.freeze" ] ||
    { echo "inject: no freezer of cgroup v2 for the container ${id} (cgroup \"${cgroup}\")" >&2; exit 1; }
}
# frozen says whether cgroup.events says that the cgroup is frozen ($1 = 1) or not ($1 = 0).
frozen() {
  while read -r key value; do
    [ "$key" != frozen ] || [ "$value" != "$1" ] || return 0
  done <"$dir/cgroup.events"
  return 1
}
# set_frozen writes cgroup.freeze and waits, without a pause, until the kernel says that every process of the cgroup is
# as asked, for up to ten seconds.
set_frozen() {
  echo "$1" >"$dir/cgroup.freeze"
  tick
  until_t=$((t + 1000)) waits=0
  until frozen "$1"; do
    waits=$((waits + 1))
    if [ $((waits % 1000)) -eq 0 ]; then
      tick
      [ "$t" -lt "$until_t" ] || { echo "inject: the cgroup ${cgroup} did not get to frozen $1 in 10s" >&2; exit 1; }
    fi
  done
}
# gone waits for the first process to end, for up to ten seconds.
gone() {
  for _ in $(seq 1 50); do
    [ -d "/proc/$first" ] || return 0
    sleep 0.2
  done
  echo "inject: pid $first on the node ${node} is still there after SIGKILL" >&2
  exit 1
}
case "$verb" in
kill)
  kill -KILL "$first"
  gone
  echo "killed pid $first (pid 1 of the container ${id}) on the node ${node}, cgroup ${cgroup}"
  ;;
freeze | thaw)
  freezer
  want=1
  [ "$verb" = freeze ] || want=0
  set_frozen "$want"
  echo "${verb} of the container ${id} (pid $first) on the node ${node}: cgroup ${cgroup} is frozen ${want}"
  ;;
kill-at)
  freezer
  seconds="$1"
  shift
  root="/proc/$first/root" stopping="the container froze" froze=0
  # Whatever ends the watch, the container is not left frozen.
  unfreeze() { [ ! -f "$dir/cgroup.freeze" ] || echo 0 >"$dir/cgroup.freeze" 2>/dev/null || :; }
  trap unfreeze EXIT
  hold() {
    tick
    froze=$t
    set_frozen 1
    tick
    froze=$((t - froze))
  }
  # The kill comes while the container is frozen, so that the moment holds until it ends: SIGKILL ends a frozen process,
  # and the end of the first process of a PID namespace ends every other process of it. The cgroup is thawed after that,
  # so that nothing is left frozen whatever the runtime does with it.
  strike() {
    kill -KILL "$first"
    gone
    unfreeze
    echo "killed pid $first (pid 1 of the container ${id}) on the node ${node} while it was frozen, which took about $((froze * 10))ms"
  }
  release() { set_frozen 0; }
  watch "$@"
  ;;
esac
'

# The script that kill-git-at runs in the container with kubectl exec, with the seconds, the word and the markers. It
# finds the Git by the name of its program and by its arguments, which /proc/<pid>/cmdline holds with a NUL after each
# and which the shell reads as one string, without the NULs.
# shellcheck disable=SC2016
git_script='
set -eu
seconds="$1" word="$2"
shift 2
root="" git="" stopping="the Git stopped"
hold() {
  git=""
  for d in /proc/[0-9]*; do
    { IFS= read -r name <"$d/comm"; } 2>/dev/null || continue
    [ "$name" = git ] || continue
    args=""
    { IFS= read -r args <"$d/cmdline"; } 2>/dev/null || :
    case "$args" in
    *"$word"*)
      git="${d#/proc/}"
      break
      ;;
    esac
  done
  [ -n "$git" ] || return 1
  kill -STOP "$git" 2>/dev/null || return 1
  # SIGSTOP is delivered when the process next runs, so the moment is looked at again only once it says it is stopped.
  reads=0
  while [ "$reads" -lt 100000 ] && { read -r _ _ state _ <"/proc/$git/stat"; } 2>/dev/null; do
    case "$state" in
    T | t) return 0 ;;
    Z) return 1 ;;
    esac
    reads=$((reads + 1))
  done
  release
  return 1
}
strike() {
  kill -KILL "$git"
  echo "killed the Git with pid $git in the container, whose arguments hold ${word}, while it was stopped"
}
release() { kill -CONT "$git" 2>/dev/null || :; }
# A watch that is stopped from outside lets its Git go on.
trap release EXIT
watch "$@"
trap - EXIT
'

# target reads the node of a pod and the ID of its container, which the verbs that act from the node need.
target() {
  namespace="$1" pod="$2" container="${3:-}"
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
}

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
  target "$2" "$3" "${4:-}"
  injecting="$(injector "$node")"
  "$KUBECTL" -n "$INJECT_NAMESPACE" exec "$injecting" -- sh -c "$watch_script$node_script" sh "$verb" "$id" "$node"
  ;;
kill-at)
  [ $# -ge 6 ] || usage
  [[ "$5" =~ ^[1-9][0-9]*$ ]] || fail "the seconds '$5' are not a whole number above 0"
  check_markers "${@:6}"
  check_cluster
  target "$2" "$3" "$4"
  injecting="$(injector "$node")"
  "$KUBECTL" -n "$INJECT_NAMESPACE" exec "$injecting" -- \
    sh -c "$watch_script$node_script" sh "$verb" "$id" "$node" "$5" "${@:6}"
  ;;
kill-git-at)
  [ $# -ge 7 ] || usage
  [[ "$5" =~ ^[1-9][0-9]*$ ]] || fail "the seconds '$5' are not a whole number above 0"
  [ -n "$6" ] || fail "the word that the arguments of the Git hold is empty"
  check_markers "${@:7}"
  check_cluster
  "$KUBECTL" -n "$2" exec "$3" -c "$4" -- sh -c "$watch_script$git_script" sh "$5" "$6" "${@:7}"
  ;;
*)
  usage
  ;;
esac
