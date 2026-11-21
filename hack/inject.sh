#!/usr/bin/env bash
# Causes a failure in a container of a pod, from the node that runs it, for the end-to-end specs that test how the sync
# recovers (the label failure in test/e2e) and for a contributor who wants to see it on `make dev`.
#
# Usage: hack/inject.sh kill|freeze|thaw <namespace> <pod> [container]
#        hack/inject.sh kill-at <namespace> <pod> <container> <seconds> <marker>...
#        hack/inject.sh kill-git-at <namespace> <pod> <container> <seconds> <word> <marker>...
#        hack/inject.sh tmpfs-mount <node> <name> <mebibytes>
#        hack/inject.sh tmpfs-fill <node> <name> <bytes-left>
#        hack/inject.sh tmpfs-free|tmpfs-unmount|tmpfs-df <node> <name>
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
#   tmpfs-mount  mounts a tmpfs of <mebibytes> (at most 64) in the mount namespace of the node, at
#                /var/lib/treebind-inject/<name>, for a static PersistentVolume to point at: a volume that fills without
#                filling the disk of the machine that runs the cluster. tmpfs-fill writes zeros into a file at its root
#                until at most <bytes-left> bytes (rounded up to a block) are free, tmpfs-free removes that file,
#                tmpfs-unmount unmounts the tmpfs and waits for what the kubelet mounted of it to go, and tmpfs-df says how
#                big it is and how much is free. A claim of a storage class that makes directories, as local-path does on
#                kind, is a directory on the disk of the node, which on kind is the disk of the host, and has no size to
#                fill: never fill one.
#   cleanup      unmounts every tmpfs that tmpfs-mount made, and removes the privileged pods and their namespace.
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
# The tmpfs verbs are for a volume that fills without filling a disk, and two guards come before any of them writes or
# unmounts anything, from facts that no argument gives. First, the directory, which the script makes from the name, has to
# be the mount point of a tmpfs, by the mount table of the node (its PID 1) and by the file system type that statfs reports,
# and a tmpfs of more than 64 MiB is refused, as a /dev/shm or a fraction of the memory would be. Second, no verb writes
# more than the tmpfs holds: what it writes comes from the free blocks that statfs reports, in at most five rounds that
# together never pass the blocks of the tmpfs, and the one file that it writes, .treebind-filler, is at the root of the mount
# point and is refused when it is a symbolic link. The name is lowercase letters, digits and dashes, so no path leaves the
# directory. The filler is written by the privileged pod and not through the agent, because the pages of a tmpfs count
# against the memory of whoever writes them, and the memory of the agent is limited.
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
  echo "       $0 tmpfs-mount <node> <name> <mebibytes>" >&2
  echo "       $0 tmpfs-fill <node> <name> <bytes-left>" >&2
  echo "       $0 tmpfs-free|tmpfs-unmount|tmpfs-df <node> <name>" >&2
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
  # level by default.
  "$KUBECTL" apply -f - >&2 <<NAMESPACE
apiVersion: v1
kind: Namespace
metadata:
  name: $INJECT_NAMESPACE
  labels:
    app.kubernetes.io/name: treebind-inject
    pod-security.kubernetes.io/enforce: privileged
NAMESPACE
  # A pod is refused until the namespace has its default service account, which the cluster makes a moment after the
  # namespace, and that is a moment that the first verb on a cluster that was just made can meet.
  for _ in $(seq 1 60); do
    "$KUBECTL" -n "$INJECT_NAMESPACE" get serviceaccount default >/dev/null 2>&1 && break
    sleep 1
  done
  # busybox, whose shell, grep, sed and kill the verbs use, is pulled only when it is not on the node.
  "$KUBECTL" apply -f - >&2 <<POD
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

# The script that the tmpfs verbs run in the privileged pod, with the verb, the name and the node, and for tmpfs-mount the
# mebibytes and for tmpfs-fill the bytes to leave. It reaches the files of the node through /proc/1/root, which is the
# root of the node's mount namespace as the pod sees it, and mounts and unmounts in that namespace with nsenter, so that
# the kubelet sees the tmpfs. The mount table is read from /proc/1/mountinfo, which is the node's too.
# shellcheck disable=SC2016
tmpfs_script='
set -eu
verb="$1" name="$2" node="$3"
shift 3
base=/var/lib/treebind-inject
dir="$base/$name"
node_root=/proc/1/root
filler=.treebind-filler
max_bytes=$((64 * 1048576))
die() {
  echo "inject: $*" >&2
  exit 1
}
# mount_type prints the type of the last file system mounted at the path $1 in the mount table of the node, or nothing.
# A line of mountinfo is "<id> <parent> <dev> <root> <mount point> <options> [<tags>...] - <type> <source> <options>".
mount_type() {
  found=""
  while read -r _ _ _ _ point _ rest; do
    [ "$point" = "$1" ] || continue
    rest=" $rest"
    rest="${rest#* - }"
    found="${rest%% *}"
  done </proc/1/mountinfo
  echo "$found"
}
check_name() {
  case "$name" in
  "" | -* | *[!a-z0-9-]*) die "the name \"$name\" is not of lowercase letters, digits and dashes, or begins with a dash" ;;
  esac
}
# measure sets blocks, avail and bs from the file system of the mount point, as statfs reports them now, and size and
# free in bytes from them.
measure() {
  read -r blocks avail bs <<MEASURE
$(stat -f -c "%b %a %S" "$node_root$dir")
MEASURE
  size=$((blocks * bs))
  free=$((avail * bs))
}
# guard refuses, before anything is written or unmounted, a directory that is not the mount point of a tmpfs of a size that
# this script fills: it is the mount table and the file system that say so, and not the caller.
guard() {
  check_name
  type="$(mount_type "$dir")"
  if [ "$type" != tmpfs ]; then
    mounted="nothing is mounted there"
    [ -z "$type" ] || mounted="a file system of type $type is mounted there"
    die "$dir on the node $node is not the mount point of a tmpfs ($mounted), and nothing is written or unmounted there"
  fi
  [ ! -L "$node_root$dir" ] || die "$dir on the node $node is a symbolic link"
  fs="$(stat -f -c "%T" "$node_root$dir")"
  [ "$fs" = tmpfs ] || die "$dir on the node $node is on a file system of type \"$fs\", not a tmpfs"
  measure
  [ "$size" -le "$max_bytes" ] ||
    die "the tmpfs at $dir on the node $node holds $size bytes, more than the $((max_bytes / 1048576)) MiB that this script fills"
}
# the file that is written is made here, at the root of the mount point, and is not followed when it is a link.
filler_path() {
  path="$node_root$dir/$filler"
  [ ! -L "$path" ] && { [ ! -e "$path" ] || [ -f "$path" ]; } ||
    die "$dir/$filler on the node $node is not a regular file, and is not written or removed"
}
filler_size() {
  if [ -e "$path" ]; then stat -c %s "$path"; else echo 0; fi
}
# mounts_of prints the mount points, in the mount table of the node, of the tmpfs that tmpfs-mount made for the name $1: its
# own, and the ones that the kubelet makes of it for the pods that use a volume on it, which are gone only when those pods
# are cleaned up, a moment after they are deleted. The memory of the tmpfs is not freed before the last of them.
mounts_of() {
  while read -r _ _ _ _ point _ rest; do
    rest=" $rest"
    rest="${rest#* - }"
    case "$rest" in
    "tmpfs treebind-inject-$1 "*) echo "$point" ;;
    esac
  done </proc/1/mountinfo
}
# unmount unmounts the tmpfs at $dir, and waits for the mounts of it that the kubelet made to be gone as well, so that
# nothing of it is left on the node.
unmount() {
  if nsenter -t 1 -m -- umount "$dir"; then
    echo "unmounted $dir on the node $node"
  else
    # Something holds it, such as a container that still has a file open in it: it is detached now and goes when the last
    # of them lets go, so that it is not left in the mount table of the node.
    nsenter -t 1 -m -- umount -l "$dir"
    echo "unmounted $dir on the node $node lazily, because it was busy"
  fi
  rmdir "$node_root$dir" 2>/dev/null || :
  for _ in $(seq 1 60); do
    [ -z "$(mounts_of "$name")" ] && return 0
    sleep 0.5
  done
  die "the tmpfs of $name on the node $node is still mounted at $(mounts_of "$name" | tr "\n" " ") by what uses it"
}
case "$verb" in
tmpfs-mount)
  mib="$1"
  check_name
  case "$mib" in
  "" | 0* | *[!0-9]*) die "the size \"$mib\" is not a whole number of mebibytes above 0" ;;
  esac
  [ "$mib" -le $((max_bytes / 1048576)) ] ||
    die "a tmpfs of $mib MiB is more than the $((max_bytes / 1048576)) MiB that this script mounts"
  [ -z "$(mount_type "$dir")" ] || die "$dir on the node $node already has a file system mounted"
  nsenter -t 1 -m -- mkdir -p "$dir"
  nsenter -t 1 -m -- mount -t tmpfs -o "size=${mib}m,mode=1777" "treebind-inject-$name" "$dir"
  guard
  echo "mounted a tmpfs at $dir on the node $node: size=$size available=$free"
  ;;
tmpfs-fill)
  left="$1"
  case "$left" in
  "" | *[!0-9]*) die "the bytes to leave, \"$left\", are not a whole number" ;;
  esac
  guard
  filler_path
  [ "$left" -le "$size" ] || die "the $left bytes to leave are more than the $size bytes of the tmpfs at $dir"
  keep=$(((left + bs - 1) / bs))
  # Never more than the tmpfs holds, in all of the rounds together; a round writes what is free beyond what is to be left.
  budget=$blocks wrote=0
  for _ in 1 2 3 4 5; do
    measure
    n=$((avail - keep))
    [ "$n" -gt 0 ] || break
    [ "$n" -le "$budget" ] || n=$budget
    [ "$n" -gt 0 ] || break
    # The write stops at the first block the file system refuses, which is what a fill to the last block comes to.
    dd if=/dev/zero bs="$bs" count="$n" >>"$path" 2>/dev/null || :
    budget=$((budget - n))
    wrote=$((wrote + n))
  done
  measure
  [ "$avail" -le "$keep" ] ||
    die "the tmpfs at $dir on the node $node has $free bytes free after the fill, more than the $left asked for"
  echo "filled the tmpfs at $dir on the node $node: size=$size available=$free filler=$(filler_size) (blocks written: $wrote)"
  ;;
tmpfs-free)
  guard
  filler_path
  before="$(filler_size)"
  rm -f "$path"
  measure
  echo "freed the filler of $before bytes in the tmpfs at $dir on the node $node: size=$size available=$free"
  ;;
tmpfs-df)
  guard
  filler_path
  echo "the tmpfs at $dir on the node $node: size=$size available=$free filler=$(filler_size)"
  ;;
tmpfs-unmount)
  check_name
  type="$(mount_type "$dir")"
  if [ -z "$type" ]; then
    rmdir "$node_root$dir" 2>/dev/null || :
    echo "nothing is mounted at $dir on the node $node"
    exit 0
  fi
  [ "$type" = tmpfs ] ||
    die "$dir on the node $node has a file system of type \"$type\" mounted, not a tmpfs, and it is not unmounted"
  unmount
  ;;
tmpfs-unmount-all)
  # What cleanup does: every tmpfs under the directory of this script, as the mount table of the node has it.
  mounted=""
  while read -r _ _ _ _ point _ rest; do
    case "$point" in
    "$base"/*) mounted="$mounted $point" ;;
    esac
  done </proc/1/mountinfo
  for point in $mounted; do
    [ "$(mount_type "$point")" = tmpfs ] || die "$point on the node $node is not a tmpfs, and is not unmounted"
    name="${point#"$base"/}"
    dir="$point"
    unmount
  done
  [ -n "$mounted" ] || echo "no tmpfs of this script is mounted on the node $node"
  ;;
esac
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
  # The tmpfs are unmounted first, from the pods that mounted them: a pod that is gone cannot, and its node keeps them
  # until the cluster is.
  status=0
  pods="$("$KUBECTL" -n "$INJECT_NAMESPACE" get pods -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' \
    --ignore-not-found 2>/dev/null || true)"
  while read -r pod; do
    [ -n "$pod" ] || continue
    "$KUBECTL" -n "$INJECT_NAMESPACE" exec "$pod" -- sh -c "$tmpfs_script" sh tmpfs-unmount-all - "${pod#inject-}" || status=1
  done <<<"$pods"
  "$KUBECTL" delete namespace "$INJECT_NAMESPACE" --ignore-not-found --wait=true --timeout=3m
  exit "$status"
  ;;
tmpfs-mount | tmpfs-fill | tmpfs-free | tmpfs-unmount | tmpfs-df)
  if [ "$verb" = tmpfs-mount ] || [ "$verb" = tmpfs-fill ]; then
    [ $# -eq 4 ] || usage
  else
    [ $# -eq 3 ] || usage
  fi
  [[ "$2" =~ ^[a-z0-9.-]+$ ]] || fail "the node '$2' is not a node name"
  check_cluster
  "$KUBECTL" get node "$2" >/dev/null || fail "the cluster has no node '$2'"
  injecting="$(injector "$2")"
  "$KUBECTL" -n "$INJECT_NAMESPACE" exec "$injecting" -- sh -c "$tmpfs_script" sh "$verb" "$3" "$2" "${@:4}"
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
