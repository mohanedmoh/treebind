# Example: a static site that Git deploys and nginx serves

A website lives in a repository, and nginx serves it from a claim. The Treebind only pulls: every push to the branch is on the
site within the interval, with no build step and no image to make, and nothing that happens on the volume ever reaches the
repository. nginx mounts the directory read-only, so it cannot change what it serves.

It starts where the [quickstart](../quickstart.md) ends: the operator installed, the Git server of its step 2 running, and the
namespace `demo` with the Secret `git-deploy-key` and the ConfigMap `git-known-hosts`.

## The repository

The first version of the site, made in the Git server's pod:

<!-- tested: make-the-repository -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
git init -q --bare /srv/git/site.git
git init -q ~/site && cd ~/site
git remote add origin /srv/git/site.git
echo '<h1>Version 1</h1>' > index.html
git add index.html
git commit -q -m "Publish version 1"
git push -q -u origin main
EOF
```

## The Treebind and nginx

<!-- tested: apply-the-manifest -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: site-data, namespace: demo}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 100Mi}
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata: {name: site, namespace: demo}
spec:
  repository:
    url: ssh://git@git-server.git-server.svc.cluster.local/srv/git/site.git
    branch: main
    auth:
      sshKeySecretRef: {name: git-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: git-known-hosts, key: known_hosts}
  target:
    claimName: site-data
    subPath: public
  sync:
    direction: pull
    interval: 10s
---
apiVersion: v1
kind: Pod
metadata: {name: site-web, namespace: demo}
spec:
  containers:
  - name: nginx
    image: nginx:1.30-alpine
    ports:
    - {name: http, containerPort: 80}
    volumeMounts:
    - {name: site, mountPath: /usr/share/nginx/html, subPath: public, readOnly: true}
  volumes:
  - name: site
    persistentVolumeClaim: {claimName: site-data}
EOF
```

- **`direction: pull`.** The agent mirrors the branch into the directory, and never commits or pushes. With a Git host of your
  own, its deploy key needs read access only.
- **`readOnly: true`.** nginx cannot write to what it serves. That suits a Treebind that pulls, because the agent does not
  write over a file that was changed on the volume: such a file would hold back every update that changes it
  ([pulling](../pull.md)).
- **`interval: 10s`.** How often the agent asks the repository for a new commit. Most of these questions fetch nothing, so a
  short interval is cheap, but a Git host may limit how often it is asked.

nginx serves the site once the Treebind is ready, which is once the first pull has passed:

<!-- tested: wait-until-ready -->
```sh
kubectl -n demo wait pod/site-web --for=condition=Ready --timeout=3m
kubectl -n demo wait treebind/site --for=condition=Ready --timeout=5m
kubectl -n demo exec site-web -- wget -qO- http://localhost/
```

<!-- output: version-1 -->
```
<h1>Version 1</h1>
```

## Deploy a new version

Push the next version from Git:

<!-- tested: push-version-2 -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
cd ~/site
echo '<h1>Version 2</h1>' > index.html
git commit -q -am "Publish version 2"
git push -q
EOF
```

Within the interval nginx serves it. Run this again if it still says version 1:

<!-- tested: see-version-2 -->
```sh
kubectl -n demo exec site-web -- wget -qO- http://localhost/
```

<!-- output: version-2 -->
```
<h1>Version 2</h1>
```

The agent wrote `index.html` the way Git writes a file in a checkout, and nginx read the new one at its next request. A site
whose pages link to each other can show a mix of the two versions for the moment that the update takes.

## What the branch and the status say

The branch has only the commits that were pushed to it:

<!-- tested: see-the-branch -->
```sh
kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/site.git log --format="%h %an: %s" main'
```

<!-- output: the-branch -->
```
e7a7fa9 Developer: Publish version 2
1bdca0b Developer: Publish version 1
```

The status names the commit of version 2 once the operator has read the agent's report, which these commands wait for:

<!-- tested: read-the-status -->
```sh
tip=$(kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/site.git rev-parse main')
kubectl -n demo wait treebind/site --for=jsonpath='{.status.lastSyncedCommit}'="$tip" --timeout=3m
kubectl -n demo get treebind site
```

<!-- output: the-treebind -->
```
NAME   READY   SYNCED   CONFLICT   AGE
site   True    True     False      36s
```

A Treebind that pulls has no conflicts to resolve, so `Conflict` stays `False`. When the branch is force-pushed, so that the
commit of the directory is not in its history any more, `Synced` says `Diverged` and the agent writes nothing until a person
decides: [recovering when the sync stops](../recovering-from-a-conflict-or-a-rejected-push.md#a-branch-that-was-rewritten-under-a-treebind-that-only-pulls)
says how to make the volume follow the branch.

## Clean up

<!-- tested: clean-up -->
```sh
kubectl -n demo delete pod/site-web treebind/site pvc/site-data
```
