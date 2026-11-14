#!/usr/bin/env bash
# Starts an ssh Git remote in the dev cluster and makes what a Treebind needs to reach it: a
# deploy key in a Secret, the host key of the remote in a ConfigMap, and a sample Treebind.
#
# Usage: hack/dev-ssh.sh [namespace]    (the namespace of the Treebind, "default" without one)
# KUBECTL names the kubectl to use, and CLAIM the claim the sample Treebind names.
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE="${1:-default}"
CLAIM="${CLAIM:-dev-claim}"
HOST="git-remote-ssh.treebind-dev.svc.cluster.local"

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

ssh-keygen -q -t ed25519 -N "" -C treebind-dev -f "$dir/id_ed25519"

# The server reads the key it lets in when it starts, so the Secret comes first.
"$KUBECTL" get namespace treebind-dev >/dev/null 2>&1 || "$KUBECTL" create namespace treebind-dev
running="$("$KUBECTL" -n treebind-dev get deployment git-remote-ssh -o name 2>/dev/null || true)"
"$KUBECTL" -n treebind-dev create secret generic git-remote-ssh-authorized \
  --from-file=authorized_keys="$dir/id_ed25519.pub" --dry-run=client -o yaml | "$KUBECTL" apply -f -
"$KUBECTL" apply -k "$(dirname "$0")/../config/dev-ssh"
# A new key means a new start of a server that is running, because the pod has copied the old one.
if [ -n "$running" ]; then
  "$KUBECTL" -n treebind-dev rollout restart deployment/git-remote-ssh
fi
"$KUBECTL" -n treebind-dev rollout status deployment/git-remote-ssh --timeout=240s

hostkey="$("$KUBECTL" -n treebind-dev exec deploy/git-remote-ssh -- cat /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f1,2)"
echo "$HOST $hostkey" >"$dir/known_hosts"

"$KUBECTL" -n "$NAMESPACE" create secret generic dev-deploy-key \
  --from-file=id_ed25519="$dir/id_ed25519" --dry-run=client -o yaml | "$KUBECTL" apply -f -
"$KUBECTL" -n "$NAMESPACE" create configmap dev-known-hosts \
  --from-file=known_hosts="$dir/known_hosts" --dry-run=client -o yaml | "$KUBECTL" apply -f -

cat <<TREEBIND
The ssh remote is ready. A Treebind that uses it, and a claim for it to sync:

---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $CLAIM
  namespace: $NAMESPACE
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 100Mi
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: dev-ssh
  namespace: $NAMESPACE
spec:
  repository:
    url: ssh://git@$HOST/srv/sample.git
    branch: main
    auth:
      sshKeySecretRef: {name: dev-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: dev-known-hosts, key: known_hosts}
  target:
    claimName: $CLAIM
    subPath: dashboards
  sync:
    direction: bidirectional
TREEBIND
