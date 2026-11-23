# Example: one directory of a monorepo

A repository holds a whole platform: the code of its services and the configuration of each application, in a directory of
its own. An application should see its own configuration, at the root of what it mounts, and nothing else of the repository,
and the changes that it makes should be commits of that directory only. `spec.repository.path` does that, and
`spec.paths.exclude` keeps the application's local overrides out of the repository. A busybox pod stands in for the
application.

It starts where the [quickstart](../quickstart.md) ends: the operator installed, the Git server of its step 2 running, and the
namespace `demo` with the Secret `git-deploy-key` and the ConfigMap `git-known-hosts`.

## The repository

A repository with a service and the configuration of two applications, made in the Git server's pod:

<!-- tested: make-the-repository -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
git init -q --bare /srv/git/platform.git
git init -q ~/platform && cd ~/platform
git remote add origin /srv/git/platform.git
mkdir -p services/api config/app config/billing
echo 'package main' > services/api/main.go
printf 'replicas: 2\nlogLevel: info\n' > config/app/settings.yaml
printf 'search: false\n' > config/app/features.yaml
printf 'currency: EUR\n' > config/billing/settings.yaml
git add -A
git commit -q -m "Add the platform"
git push -q -u origin main
EOF
```

## The Treebind

<!-- tested: apply-the-manifest -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: app-config, namespace: demo}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 100Mi}
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata: {name: app-config, namespace: demo}
spec:
  repository:
    url: ssh://git@git-server.git-server.svc.cluster.local/srv/git/platform.git
    branch: main
    path: config/app
    auth:
      sshKeySecretRef: {name: git-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: git-known-hosts, key: known_hosts}
  target:
    claimName: app-config
    subPath: config
  sync:
    direction: bidirectional
    interval: 10s
    settleTime: 2s
  paths:
    exclude: ["*.local.yaml"]
---
apiVersion: v1
kind: Pod
metadata: {name: config-app, namespace: demo}
spec:
  containers:
  - name: app
    image: busybox:1.38
    command: [sh, -c, 'trap "exit 0" TERM; sleep infinity & wait']
    volumeMounts:
    - {name: config, mountPath: /etc/app, subPath: config}
  volumes:
  - name: config
    persistentVolumeClaim: {claimName: app-config}
EOF
```

- **`repository.path: config/app`.** The files of `config/app` in the repository are the files of the synced directory, at the
  root of it, so `config/app/settings.yaml` is `/etc/app/settings.yaml` in the application. Nothing else of the repository is
  written to the volume. The branch has to have the directory when the Treebind starts, so that a mistake in its name is not
  taken for a new directory ([syncing one directory of the repository](../repository-path.md)).
- **`paths.exclude`.** The agent never commits a file that matches one of these patterns. They are relative to the directory of
  `path`, so `*.local.yaml` means the files of `config/app` and of the directories below it.

Wait until it is ready, and list what the application sees:

<!-- tested: wait-until-ready -->
```sh
kubectl -n demo wait pod/config-app --for=condition=Ready --timeout=3m
kubectl -n demo wait treebind/app-config --for=condition=Ready --timeout=5m
kubectl -n demo exec config-app -- ls /etc/app
```

<!-- output: the-directory -->
```
features.yaml
settings.yaml
```

## The application changes its configuration

The application turns a feature on, and writes an override that is for this cluster only:

<!-- tested: change-the-configuration -->
```sh
kubectl -n demo exec config-app -- sh -c '
  echo "search: true" > /etc/app/features.yaml
  echo "logLevel: debug" > /etc/app/settings.local.yaml'
```

A few seconds later the agent has committed the change and pushed it. Look at the last commit of the branch, and run this again
if it is still the first:

<!-- tested: see-the-commit -->
```sh
kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/platform.git show --name-only --format="%h %an: %s" main'
```

<!-- output: the-commit-of-the-application -->
```
8d32d38 treebind-bot: treebind: sync 1 file(s)

config/app/features.yaml
```

The commit changes `config/app/features.yaml` and nothing else of the repository: the agent sends each commit of its own as a
commit on the tip of the branch, with the files of the directory as the volume has them and every other file as the
repository has it. `settings.local.yaml` stays on the volume and is not committed.

## Someone changes the repository inside and outside the directory

One commit from Git that changes the service and the configuration of the application:

<!-- tested: push-from-git -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
cd ~/platform
git pull -q
echo '// The API listens on port 8080.' >> services/api/main.go
printf 'replicas: 3\nlogLevel: info\n' > config/app/settings.yaml
git commit -q -am "Scale the app to three replicas"
git push -q
EOF
```

Within the interval the application has the new settings, and still nothing of the rest of the repository. Run this again if
`replicas` still says 2:

<!-- tested: see-the-settings -->
```sh
kubectl -n demo exec config-app -- sh -c 'ls /etc/app && cat /etc/app/settings.yaml'
```

<!-- output: the-new-settings -->
```
features.yaml
settings.local.yaml
settings.yaml
replicas: 3
logLevel: info
```

A commit that changes only files outside the directory changes nothing on the volume. The agent still fetches it, because
it asks for the branch and not for the directory, and the whole repository is on the claim, in `.treebind/git`: a large
repository makes a large Git directory, whatever the size of the directory that is synced.

## Clean up

<!-- tested: clean-up -->
```sh
kubectl -n demo delete pod/config-app treebind/app-config pvc/app-config
```
