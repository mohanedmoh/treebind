# Quickstart

This page takes you from an empty kind cluster to a Treebind that keeps a directory of a pod and a Git repository in sync in
both directions. You install the operator with Helm, start a small Git server in the same cluster, apply one manifest, edit a
file in the pod and see the commit on the server, push a change from Git and see the file in the pod, and read both outcomes
in `kubectl describe`. It takes about ten minutes, most of it spent pulling images. Every command on the page is run by a test
on a fresh kind cluster, as it is written, for every change to the code and to this page
([the development guide](development.md#the-quickstart) says how).

Treebind is alpha software. Read [what this page does not show](#what-this-page-does-not-show) before you use it with
anything that matters.

## Before you start

You need Docker (or another runtime that kind supports), [kind](https://kind.sigs.k8s.io), `kubectl`,
[Helm](https://helm.sh) (the test uses Helm 4.3.1, which is the Helm that publishes the chart) and `ssh-keygen`, which comes
with the OpenSSH client. You need neither Git nor a clone of this repository: Git runs in the cluster. Work in an empty
directory, because the commands write a key and the host key of the Git server into the current directory.

The chart needs Kubernetes 1.35 or later. kind v0.33 makes a cluster of Kubernetes 1.37, and `kubectl version` says which one
a cluster has. To make one:

<!-- not tested: the test makes its cluster itself -->
```sh
kind create cluster --name treebind
```

## 1. Install the operator

<!-- tested: install -->
```sh
helm install treebind oci://ghcr.io/mohanedmoh/charts/treebind --version 0.0.1-alpha.5 \
  --namespace treebind-system --create-namespace --wait
```

This installs a published release of the chart: the `Treebind` custom resource definition, the operator as a Deployment of two
replicas with leader election, and the roles it needs. The operator image and the sync agent image are pulled from
`ghcr.io/mohanedmoh` with the tag of the release, for amd64 and arm64. The chart and the images are signed, and
[releasing](releasing.md#verify-a-release) says how to check them. `--wait` returns once the operator is ready.

## 2. Start a Git server in the cluster

The Treebind needs a repository that it can push to. This step starts one in the cluster, so that you need no account on a Git
host. To use a repository of your own instead, skip this step and the next, and follow
[using your own repository](#using-your-own-repository).

The agent reaches a repository over ssh with a key, so first make one. The private key goes to the Treebind, and the server
lets the public one in:

<!-- tested: make-the-key -->
```sh
ssh-keygen -q -t ed25519 -N '' -C treebind-quickstart -f deploy-key
```

Then start the server: a pod that installs OpenSSH and Git from Alpine's packages when it starts, so the cluster has to reach
the internet, and lets the user `git` log in with that key.

<!-- tested: start-the-git-server -->
```sh
kubectl create namespace git-server
kubectl -n git-server create secret generic git-server-authorized-keys --from-file=authorized_keys=deploy-key.pub
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: git-server, namespace: git-server}
spec:
  replicas: 1
  selector:
    matchLabels: {app: git-server}
  template:
    metadata:
      labels: {app: git-server}
    spec:
      containers:
      - name: sshd
        image: alpine:3.22
        command:
        - sh
        - -ec
        - |
          apk add --no-cache --quiet openssh-server git
          adduser -D -h /home/git -s /bin/sh git
          sed -i 's/^git:!:/git:*:/' /etc/shadow   # sshd refuses a locked account, even for a key
          mkdir -p /home/git/.ssh /srv/git
          cp /etc/authorized-keys/authorized_keys /home/git/.ssh/
          chmod 700 /home/git/.ssh && chmod 600 /home/git/.ssh/authorized_keys
          su git -c 'git config --global user.name Developer && git config --global user.email developer@example.org &&
            git config --global init.defaultBranch main'
          chown -R git:git /home/git /srv/git
          ssh-keygen -A
          exec /usr/sbin/sshd -D -e -p 2222 -o PasswordAuthentication=no -o PermitRootLogin=no
        ports:
        - {name: ssh, containerPort: 2222}
        readinessProbe:
          tcpSocket: {port: ssh}
        volumeMounts:
        - {name: authorized-keys, mountPath: /etc/authorized-keys, readOnly: true}
        - {name: repositories, mountPath: /srv/git}
      volumes:
      - name: authorized-keys
        secret: {secretName: git-server-authorized-keys}
      - name: repositories
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata: {name: git-server, namespace: git-server}
spec:
  selector: {app: git-server}
  ports:
  - {name: ssh, port: 22, targetPort: ssh}
EOF
kubectl -n git-server rollout status deployment/git-server --timeout=5m
```

The agent checks the host key of every server it connects to and refuses one it does not know, so it needs the key of this
one. Read through `kubectl`, the key comes from the pod itself and not over the network, so you can trust it:

<!-- tested: read-the-host-key -->
```sh
echo "git-server.git-server.svc.cluster.local $(kubectl -n git-server exec deploy/git-server -- cat /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f1,2)" > known_hosts
```

The server is for trying Treebind and for nothing else. It runs sshd as root, keeps its repositories in an `emptyDir` and makes
a new host key each time it starts, so when its pod is replaced the repositories are gone and the Treebind refuses the new key.

## 3. Put a first commit in the repository

These commands run in the server's pod as the user `git`. They make the bare repository `/srv/git/quickstart.git` and a clone
of it in `~/quickstart`, from which you push in step 7 as you would from a clone of your own. The server names the author of
these commits `Developer`.

<!-- tested: make-the-repository -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
git init -q --bare /srv/git/quickstart.git
git init -q ~/quickstart && cd ~/quickstart
git remote add origin /srv/git/quickstart.git
echo "Hello from Git" > README.md
git add README.md
git commit -q -m "First commit"
git push -q -u origin main
EOF
```

## 4. Apply one manifest

A Treebind reads its key and its known hosts from a Secret and a ConfigMap in its own namespace. Its sync pod mounts them, and
the operator never reads the Secret.

<!-- tested: give-the-namespace-the-key -->
```sh
kubectl create namespace demo
kubectl -n demo create secret generic git-deploy-key --from-file=id_ed25519=deploy-key
kubectl -n demo create configmap git-known-hosts --from-file=known_hosts=known_hosts
```

Then the one manifest: a claim, the Treebind, and a pod that stands in for your application.

<!-- tested: apply-the-manifest -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: app-data, namespace: demo}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 100Mi}
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata: {name: quickstart, namespace: demo}
spec:
  repository:
    url: ssh://git@git-server.git-server.svc.cluster.local/srv/git/quickstart.git
    branch: main
    auth:
      sshKeySecretRef: {name: git-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: git-known-hosts, key: known_hosts}
  target:
    claimName: app-data
    subPath: files
  sync:
    direction: bidirectional
    interval: 10s
    settleTime: 2s
---
apiVersion: v1
kind: Pod
metadata: {name: app, namespace: demo}
spec:
  containers:
  - name: app
    image: busybox:1.38
    command: [sh, -c, 'trap "exit 0" TERM; sleep infinity & wait']
    volumeMounts:
    - {name: data, mountPath: /data, subPath: files}
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: app-data}
EOF
```

What the Treebind says:

- **`repository`** is the address of the repository, with its scheme, the branch to follow, and the Secret and the ConfigMap
  of the key and the known hosts. An `ssh://` address needs both.
- **`target`** is the claim, which has to exist, and `subPath`, the directory of the claim that is synced. The agent keeps its
  Git directory beside it, in `.treebind` at the root of the claim, so an application that mounts the `subPath`, as this pod
  does, never sees it ([the Git directory](git-directory.md)).
- **`sync`** is `bidirectional`: the agent pulls what the repository gains and commits and pushes what the application writes.
  It asks the repository for new commits every `interval`, 30 seconds when it is not set, and commits a file once it has not
  changed for `settleTime`, 5 seconds when it is not set. The short values here make the page quick.

The operator makes a StatefulSet `quickstart-sync` with one pod, which mounts the claim, clones the repository into `files/` and
then syncs it. A `ReadWriteOnce` claim can be mounted on one node only, so the sync pod and the application have to run on the
same node, which on a cluster of one node, as here, they always do. On more nodes, name the application's pods in
`spec.placement.appSelector`, which is newer than the release that this page installs
([where the sync pod runs](storage-support.md#access-modes-and-where-the-sync-pod-runs)). The application here runs as root;
an application that runs as another user needs the settings of
[Pod Security and file ownership](pod-security.md#what-the-volume-needs), so that it and the agent can change each other's
files.

## 5. See the files of the repository in the pod

The Treebind is `Ready` once its sync pod has cloned the repository and its first pull has passed, which takes about half a
minute the first time, most of it pulling the agent image:

<!-- tested: wait-until-ready -->
```sh
kubectl -n demo wait pod/app --for=condition=Ready --timeout=3m
kubectl -n demo wait treebind/quickstart --for=condition=Ready --timeout=5m
kubectl -n demo exec app -- cat /data/README.md
```

The last command prints the file of the first commit:

<!-- output: the-file-of-the-repository -->
```
Hello from Git
```

## 6. Edit a file in the pod and see the commit

Write a file as the application would:

<!-- tested: write-in-the-pod -->
```sh
kubectl -n demo exec app -- sh -c 'echo "Written by the application" > /data/from-the-app.txt'
```

The agent sees the new file, waits until it has not changed for the `settleTime` of two seconds, commits it and pushes the
commit, all within a few seconds. Look at the branch on the server, and run the command again if the commit is not there yet:

<!-- tested: see-the-commit -->
```sh
kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/quickstart.git log --format="%h %an: %s" main'
```

<!-- output: the-commit-of-the-agent -->
```
d3de362 treebind-bot: treebind: sync 1 file(s)
a8b732d Developer: First commit
```

`treebind-bot` and the message are the defaults, and `spec.commit` sets both, as
[the page on local changes](local-changes.md#it-commits-as-one-author-with-the-message-you-choose) says, with what the agent
leaves out of a commit whatever you write.

## 7. Read the outcome in kubectl describe

The operator asks the agent for its report every 30 seconds and writes what it says in the status of the Treebind: the
conditions `Synced` and `Conflict`, and the commit at which the volume and the repository last agreed. So the status can be up
to a minute behind the branch. These commands wait until the status names the commit at the tip of the branch, and then show it:

<!-- tested: read-the-status -->
```sh
tip=$(kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/quickstart.git rev-parse main')
kubectl -n demo wait treebind/quickstart --for=jsonpath='{.status.lastSyncedCommit}'="$tip" --timeout=3m
kubectl -n demo describe treebind quickstart
```

The end of what `kubectl describe` prints, with some lines left out:

<!-- output: the-status -->
```
Status:
  Conditions:
...
    Message:               No step of the sync failed. The volume and the repository last agreed at commit d3de362c7f76
    Observed Generation:   1
    Reason:                InSync
    Status:                True
    Type:                  Synced
    Last Transition Time:  2026-11-23T09:05:38Z
    Message:               The sync agent knows of no conflict between the volume and the repository that waits to be resolved, and has resolved none since it started
    Observed Generation:   1
    Reason:                NoConflict
    Status:                False
    Type:                  Conflict
...
  Last Sync Time:          2026-11-23T09:05:02Z
  Last Synced Commit:      d3de362c7f767512370c8f7925836405289a79d2
```

`Synced` `True` with `InSync` says that no step of the sync failed and that the volume and the repository agree at
`Last Synced Commit`, the commit that the agent pushed. `Conflict` `False` says that nothing waits for a person. `kubectl get
treebind -n demo` shows `Ready`, `Synced` and `Conflict` in its columns, and each change of `Synced` and `Conflict` is also an
event of the Treebind, which `kubectl -n demo events --for treebind/quickstart` lists ([metrics and events](metrics-and-events.md)).
[Reading the sync agent's status](agent-status.md#what-synced-and-conflict-say) says what every reason means, and
[recovering when the sync stops](recovering-from-a-conflict-or-a-rejected-push.md) what to do when a condition is not what it
is here.

## 8. Push a change from Git and see it in the pod

Commit a file in the server's clone and push it. `git pull` first brings in the commit of the agent, as you would before you
push from a clone of your own:

<!-- tested: push-from-git -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
cd ~/quickstart
git pull -q
echo "Pushed from Git" > from-git.txt
git add from-git.txt
git commit -q -m "Add a file from Git"
git push -q
EOF
```

Within the `interval` of ten seconds the agent pulls the commit and writes the file. Run this again if the file is not there
yet:

<!-- tested: see-the-file-in-the-pod -->
```sh
kubectl -n demo exec app -- cat /data/from-git.txt
```

<!-- output: the-file-from-git -->
```
Pushed from Git
```

Then run the commands of [step 7](#7-read-the-outcome-in-kubectl-describe) again. They wait until the status names the commit
that you pushed, and `kubectl describe` shows `Synced` `True` with `InSync` at it.

That is the whole loop: what the application writes becomes a commit on the branch, what is pushed to the branch becomes files
in the application's directory, and the Treebind says in its status where the two last agreed. The agent never forces a push,
never writes over a file that the application changed and that is not committed yet, and keeps the commit that the directory
was at before each update, so that the files that an update removes can be got back
([pulling](pull.md), [the deletion guard](deletion-guard.md)).

## The worked examples

Each example starts where step 8 ends: the operator installed, the Git server of step 2 running, and the namespace `demo` with
the Secret `git-deploy-key` and the ConfigMap `git-known-hosts`. Each removes what it made at its end.

- [Dashboards that people edit in the application](examples/dashboards-edited-in-the-application.md): both directions, the
  author and the message of the commits, the files that are committed, and a dashboard that was changed in the application
  and in Git at the same time, which `conflictPolicy` resolves.
- [A static site that Git deploys and nginx serves](examples/a-static-site-served-by-nginx.md): `direction: pull`, a
  read-only mount, and a push that is served within the interval.
- [One directory of a monorepo](examples/one-directory-of-a-monorepo.md): `spec.repository.path`, a commit that changes only
  that directory, and `spec.paths.exclude`.

## Using your own repository

Any Git host that takes an ssh key works. Use a deploy key, which has access to one repository and to nothing else. Skip
steps 2 and 3, and run these instead of the first block of step 4, with the name of your host:

<!-- not tested: needs an account on a Git host -->
```sh
ssh-keygen -q -t ed25519 -N '' -C treebind -f deploy-key
ssh-keyscan -t ed25519 github.com > known_hosts
ssh-keygen -lf known_hosts
kubectl create namespace demo
kubectl -n demo create secret generic git-deploy-key --from-file=id_ed25519=deploy-key
kubectl -n demo create configmap git-known-hosts --from-file=known_hosts=known_hosts
```

- **The deploy key.** Add `deploy-key.pub` to the repository as a deploy key (on GitHub, under the repository's settings),
  with write access for a `bidirectional` Treebind. A Treebind whose direction is `pull` needs read access only.
- **The known hosts.** `ssh-keyscan` asks the host for its key over the network and trusts whatever answers, which is trust
  on first use: on a network that someone else controls, it can be their key. Compare the fingerprint that
  `ssh-keygen -lf known_hosts` prints with the one that your host publishes (GitHub and GitLab publish theirs in their
  documentation) before you create the ConfigMap. For a host on another port, `ssh-keyscan -p <port>` writes the entry as
  `[host]:port`, which is how ssh looks it up.
- **The manifest.** In the Treebind of step 4, set `url` to the ssh address of the repository with its scheme, such as
  `ssh://git@github.com/<owner>/<repository>.git` (the short form `git@github.com:<owner>/<repository>.git` is refused), and
  `branch` to a branch that it has. The Secret and the ConfigMap keep the names and the keys of step 4, which the Treebind
  names in `auth`.

The repository of a `bidirectional` Treebind can be empty: the agent then makes the branch with its first commit, of what the
application wrote. The directory of the claim has to be empty,
or hold exactly the files of the branch, or the agent changes nothing and says `DirectoryNotEmpty`
([the checkout](checkout.md)). The commands of steps 3 and 8 that run Git in the server's pod are the ones that you run in a
clone of yours.

## Clean up

Delete the cluster:

<!-- not tested: the test deletes its cluster itself -->
```sh
kind delete cluster --name treebind
```

Or, to keep the cluster, remove what this page made. The namespaces go first, while the operator still runs:

<!-- tested: clean-up -->
```sh
kubectl delete namespace demo git-server
helm uninstall treebind --namespace treebind-system
kubectl delete crd treebinds.treebind.example.org
kubectl delete namespace treebind-system
```

Deleting a Treebind never deletes its claim ([deleting a Treebind](deleting-a-treebind.md)); here the claims go with their
namespace. The chart keeps the CRD when it is uninstalled, so that the Treebinds and their sync pods outlive an uninstall, and
deleting the CRD deletes every Treebind of the cluster.

## What this page does not show

- **Treebind is alpha.** Its releases are pre-releases, and its API is `v1alpha1`. The API group, `treebind.example.org`, is a
  placeholder that will change before a stable release ([#15](https://github.com/mohanedmoh/treebind/issues/15)), and with it
  the `apiVersion` of every Treebind, which you will then have to create again. Do not use it with data that you cannot lose.
- **Its tests run on kind.** This page and the end-to-end tests run on kind clusters of one node, with kind's `local-path`
  storage. `v0.0.1-alpha.5` was also tried once by hand on a kubeadm cluster of two nodes, with `local-path` and NFS volumes,
  which no test repeats. No storage class is listed as supported yet ([storage support](storage-support.md)), and managed
  clusters and cloud storage are not tested.
- **The release that this page installs does not place the sync pod next to the application.** With a `ReadWriteOnce` claim
  on a cluster of several nodes, the scheduler can put the sync pod on another node than the application, where it cannot
  mount the claim. `spec.placement.appSelector`, which a later release has, keeps them on one node
  ([where the sync pod runs](storage-support.md#access-modes-and-where-the-sync-pod-runs)). Even with it, the sync pod does
  not follow an application that moves later ([#113](https://github.com/mohanedmoh/treebind/issues/113)).
- **Some fields are accepted and ignored.** `spec.placement.mode: sidecar`, `spec.agent.fsGroup` and
  `spec.agent.resources.claims` are not built yet, and a Treebind that sets one says so in its condition `SpecSupported`.
- **The only credential is an ssh key.** An `https://` address is accepted, but the agent has no credentials to send with it,
  and tokens for https are planned ([#58](https://github.com/mohanedmoh/treebind/issues/58)).
- **The agent polls.** It asks the repository at every interval; webhooks from Git hosts are planned
  ([#61](https://github.com/mohanedmoh/treebind/issues/61)).
