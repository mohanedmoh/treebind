#!/usr/bin/env bash
# Checks that the agent image can run Git and ssh with the settings of the sync pod: a
# read-only root filesystem, no capabilities and no privilege escalation. First as the user
# the image names, then as a user ID the image does not know, as a platform such as
# OpenShift gives the pod one, with group 0, with the passwd file that the operator adds
# with AGENT_ARBITRARY_USER_ID=true (docs/adr/0042).
#
# Usage: hack/check-agent-image.sh <image>
#
# The image has to be in the local Docker. `make check-agent-image` builds it with
# ko and runs this. MIN_GIT is the oldest Git the image may carry; raise it when
# Git ships a security release, so that a base image with an older Git fails here.
#
# The second part starts a throwaway ssh server from SSHD_IMAGE, which installs OpenSSH and
# Git from the network.
set -euo pipefail

image="${1:?usage: check-agent-image.sh <image>}"
min_git="${MIN_GIT:-2.50.1}"
sshd_image="${SSHD_IMAGE:-alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f}"

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

# A user ID that the image has no entry for, from 100000 up, and group 0.
uid=$((100000 + ((RANDOM << 15) | RANDOM)))
names="treebind-check-$$"
network="$names" sshd="$names-sshd" agent="$names-agent"
keys="$names-keys" claim="$names-claim"
# Docker mounts a subpath of a volume only when it is a directory, so the volume of the passwd
# file is a directory of the host, writable by anyone as an emptyDir is.
passwd="$(mktemp -d)"
cleanup() {
  docker rm -f "$sshd" "$agent" > /dev/null 2>&1 || true
  docker volume rm "$keys" "$claim" > /dev/null 2>&1 || true
  docker network rm "$network" > /dev/null 2>&1 || true
  rm -rf "$passwd"
}
trap cleanup EXIT
chmod 0777 "$passwd"
docker network create "$network" > /dev/null
docker volume create "$keys" > /dev/null
docker volume create "$claim" > /dev/null

# The sync pod as the operator makes it with AGENT_ARBITRARY_USER_ID=true
# (internal/controller/passwd.go), as the user uid, in the working directory /.
pod=(--read-only --cap-drop ALL --security-opt no-new-privileges --user "$uid:0" --network "$network" -w /)
passwd_mount=(-v "$passwd/passwd:/etc/passwd:ro")

out="$(docker run --rm "${pod[@]}" --entrypoint ssh "$image" -G localhost 2>&1 > /dev/null)" &&
  fail "ssh -G ran as uid $uid without a passwd entry, so this check proves nothing"
case "$out" in
  *"No user exists for uid $uid"*) ;;
  *) fail "ssh -G as uid $uid failed for another reason than the passwd entry: $out" ;;
esac

# The server, with a key for the agent, its own host key and a repository with a commit. The
# key is root's and 0444, as the kubelet mounts a Secret with the mode the operator gives it.
# The claim is writable by anyone.
docker run -d --name "$sshd" --network "$network" -v "$keys:/keys" -v "$claim:/claim" \
  "$sshd_image" sh -ec '
    chmod 0777 /claim
    for try in 1 2 3 4; do apk add --no-cache --quiet openssh-server git && break; [ "$try" = 4 ] && exit 1; sleep 10; done
    adduser -D -h /home/git -s /bin/sh git
    sed -i "s/^git:!:/git:*:/" /etc/shadow
    ssh-keygen -q -t ed25519 -N "" -C check -f /keys/id_ed25519
    chmod 0444 /keys/id_ed25519
    mkdir -p /home/git/.ssh
    cp /keys/id_ed25519.pub /home/git/.ssh/authorized_keys
    chmod 700 /home/git/.ssh
    chmod 600 /home/git/.ssh/authorized_keys
    ssh-keygen -A
    echo "['"$sshd"']:2222 $(cut -d" " -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)" > /keys/known_hosts
    git init -q --bare --initial-branch=main /srv/check.git
    git clone -q /srv/check.git /tmp/seed
    cd /tmp/seed
    echo check > check.txt
    git add check.txt
    git -c user.name=check -c user.email=check@example.com commit -q -m check
    git push -q origin main
    chown -R git:git /home/git /srv
    touch /ready
    exec /usr/sbin/sshd -D -e -p 2222 -o PasswordAuthentication=no -o AuthenticationMethods=publickey' > /dev/null
for _ in $(seq 120); do
  docker exec "$sshd" test -f /ready 2> /dev/null && break
  docker inspect -f '{{.State.Running}}' "$sshd" 2> /dev/null | grep -q true ||
    fail "the ssh server did not start: $(docker logs "$sshd" 2>&1 | tail -n 20)"
  sleep 1
done
docker exec "$sshd" test -f /ready || fail "the ssh server is not ready after 120 seconds"

# The init container writes the passwd file into the volume, and the sync container sees it at
# /etc/passwd.
out="$(docker run --rm "${pod[@]}" -v "$passwd:/run/treebind" "$image" write-passwd /run/treebind/passwd 2>&1)" ||
  fail "the agent cannot write the passwd file as uid $uid: $out"
out="$(docker run --rm "${pod[@]}" "${passwd_mount[@]}" --entrypoint ssh "$image" -G localhost 2>&1 > /dev/null)" ||
  fail "ssh -G failed as uid $uid with the passwd file of the init container: $out"

# The agent clones the repository over ssh, with nothing writable but the claim: no /tmp and no
# home directory.
docker run -d --name "$agent" "${pod[@]}" "${passwd_mount[@]}" \
  -v "$keys:/etc/treebind/ssh:ro" -v "$claim:/work" \
  -e TREEBIND_NAME=check -e TREEBIND_NAMESPACE=check -e TREEBIND_POD_NAME=check-sync-0 \
  -e TREEBIND_MOUNT_PATH=/work -e TREEBIND_SYNC_DIRECTION=pull \
  -e "TREEBIND_REPO_URL=ssh://git@$sshd:2222/srv/check.git" -e TREEBIND_REPO_BRANCH=main \
  -e TREEBIND_SSH_KEY_FILE=/etc/treebind/ssh/id_ed25519 -e TREEBIND_KNOWN_HOSTS_FILE=/etc/treebind/ssh/known_hosts \
  -e TREEBIND_STATUS_ADDR=:8080 -e TREEBIND_STATUS_ISSUER=https://kubernetes.default.svc \
  -e TREEBIND_STATUS_AUDIENCE=treebind.example.org/check/check \
  -e TREEBIND_STATUS_SUBJECT=system:serviceaccount:treebind-system:treebind-controller-manager \
  "$image" > /dev/null
for _ in $(seq 60); do
  logs="$(docker logs "$agent" 2>&1)"
  case "$logs" in
    *"check=Checkout outcome=Passed"*) break ;;
    *"check=Checkout outcome=Failed"*) fail "the agent cannot clone over ssh as uid $uid: $logs" ;;
  esac
  sleep 1
done
case "$logs" in
  *"check=Checkout outcome=Passed"*) ;;
  *) fail "the agent did not clone over ssh as uid $uid within 60 seconds: $logs" ;;
esac
docker exec "$sshd" ls /claim/check.txt > /dev/null 2>&1 || fail "the clone has no check.txt in the claim"

echo "ok: $image runs as uid $uid and group 0 with the passwd file of its init container, and clones over ssh"
