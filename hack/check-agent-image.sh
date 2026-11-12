#!/usr/bin/env bash
# Checks that the agent image can run Git and ssh as the user it runs as, with the
# settings of the sync pod: a read-only root filesystem, a writable /tmp, no
# capabilities and no privilege escalation.
#
# Usage: hack/check-agent-image.sh <image>
#
# The image has to be in the local Docker. `make check-agent-image` builds it with
# ko and runs this. MIN_GIT is the oldest Git the image may carry; raise it when
# Git ships a security release, so that a base image with an older Git fails here.
set -euo pipefail

image="${1:?usage: check-agent-image.sh <image>}"
min_git="${MIN_GIT:-2.50.1}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# The pod runs the image's own user, with runAsNonRoot.
user="$(docker image inspect "$image" --format '{{.Config.User}}')"
[[ "$user" =~ ^[1-9][0-9]*(:[0-9]+)?$ ]] || fail "the image runs as '$user', not as a numeric non-root user"

entrypoint="$(docker image inspect "$image" --format '{{json .Config.Entrypoint}}')"
[ "$entrypoint" = '["/ko-app/agent"]' ] || fail "the entrypoint is $entrypoint, not the agent"

run=(docker run --rm --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges)

version="$("${run[@]}" --entrypoint git "$image" --version | awk '{print $3}')" || fail "git does not run"
oldest="$(printf '%s\n%s\n' "$min_git" "$version" | sort -V | head -n 1)"
[ "$oldest" = "$min_git" ] || fail "git $version is older than $min_git"

out="$("${run[@]}" --entrypoint ssh "$image" -V 2>&1)" || fail "ssh does not run: $out"
# ssh looks the user up in the passwd file and refuses to run without an entry.
out="$("${run[@]}" --entrypoint ssh "$image" -G localhost 2>&1 > /dev/null)" ||
  fail "ssh -G failed. It needs a passwd entry for the user $user in the image: $out"

"${run[@]}" --entrypoint sh "$image" -c \
  'cd /tmp && git init -q repo && cd repo && git -c user.name=check -c user.email=check@example.com commit -q --allow-empty -m check' ||
  fail "git cannot create a repository and a commit on a read-only root filesystem with a writable /tmp"

echo "ok: $image runs as $user with git $version and ssh, on a read-only root filesystem"
