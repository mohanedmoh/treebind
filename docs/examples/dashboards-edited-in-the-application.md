# Example: dashboards that people edit in the application

An application keeps its dashboards as JSON files, and its users edit them in its interface. A repository holds the same
dashboards, so that every change has a history, can be reviewed, and can be made in Git as well. This Treebind syncs the two in
both directions: the commits of the application's edits carry its name and a message of their own, only the dashboards are
committed and nothing else that the application writes, and when a dashboard is changed in the application and in Git at the
same time, the application's version wins and the one from Git stays in the history. A busybox pod stands in for the
application.

It starts where the [quickstart](../quickstart.md) ends: the operator installed, the Git server of its step 2 running, and the
namespace `demo` with the Secret `git-deploy-key` and the ConfigMap `git-known-hosts`.

## The repository

A repository with one dashboard, made in the Git server's pod:

<!-- tested: make-the-repository -->
```sh
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
git init -q --bare /srv/git/dashboards.git
git init -q ~/dashboards && cd ~/dashboards
git remote add origin /srv/git/dashboards.git
echo '{"title": "Sales", "panels": 2}' > sales.json
git add sales.json
git commit -q -m "Add the sales dashboard"
git push -q -u origin main
EOF
```

## The Treebind

<!-- tested: apply-the-manifest -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: dashboards-data, namespace: demo}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 100Mi}
---
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata: {name: dashboards, namespace: demo}
spec:
  repository:
    url: ssh://git@git-server.git-server.svc.cluster.local/srv/git/dashboards.git
    branch: main
    auth:
      sshKeySecretRef: {name: git-deploy-key, key: id_ed25519}
      knownHostsConfigMapRef: {name: git-known-hosts, key: known_hosts}
  target:
    claimName: dashboards-data
    subPath: dashboards
  sync:
    direction: bidirectional
    interval: 15s
    settleTime: 15s
    conflictPolicy: followLocal
  commit:
    authorName: dashboard-sync
    authorEmail: dashboard-sync@example.org
    messageTemplate: "dashboards: save {{.Count}} file(s) from the application"
  paths:
    include: ["*.json"]
---
apiVersion: v1
kind: Pod
metadata: {name: dashboards-app, namespace: demo}
spec:
  containers:
  - name: app
    image: busybox:1.38
    command: [sh, -c, 'trap "exit 0" TERM; sleep infinity & wait']
    volumeMounts:
    - {name: data, mountPath: /var/lib/dashboards, subPath: dashboards}
  volumes:
  - name: data
    persistentVolumeClaim: {claimName: dashboards-data}
EOF
```

What is new in it, against the quickstart:

- **`conflictPolicy: followLocal`.** When the directory and the branch each changed the same file, the directory's version wins,
  and the agent makes a merge commit whose second parent is the branch, so the version from Git stays in the history. The
  default, `followRemote`, makes the repository win instead and keeps the directory's commits under a ref of the agent's own
  ([conflicts](../conflicts.md)).
- **`commit`.** Every commit of the agent has this author, who is also its committer, and the message of the template, in which
  `{{.Count}}` is the number of files ([local changes](../local-changes.md#it-commits-as-one-author-with-the-message-you-choose)).
- **`paths.include`.** The agent commits only the files that match a pattern, here the dashboards. A pattern is a line of a
  `.gitignore` file, and `*.json` matches in every directory. The files of the repository are written to the volume whatever the
  patterns say.
- **`settleTime: 15s`.** A file is committed once it has not changed for 15 seconds, for an application that writes a dashboard
  in several steps. It also keeps the walk-through below in order.

Wait until it is ready, and read the dashboard in the application's pod:

<!-- tested: wait-until-ready -->
```sh
kubectl -n demo wait pod/dashboards-app --for=condition=Ready --timeout=3m
kubectl -n demo wait treebind/dashboards --for=condition=Ready --timeout=5m
kubectl -n demo exec dashboards-app -- cat /var/lib/dashboards/sales.json
```

<!-- output: the-dashboard -->
```
{"title": "Sales", "panels": 2}
```

## The application saves a dashboard

The application adds a panel to the dashboard, and writes a cache file beside it:

<!-- tested: save-a-dashboard -->
```sh
kubectl -n demo exec -i dashboards-app -- sh -c 'cat > /var/lib/dashboards/sales.json' <<'EOF'
{"title": "Sales", "panels": 3}
EOF
kubectl -n demo exec dashboards-app -- sh -c 'echo "rendered at startup" > /var/lib/dashboards/render-cache.txt'
```

About 15 seconds later the agent commits the dashboard and pushes it. Look at the branch, and run this again if the commit is
not there yet:

<!-- tested: see-the-commit -->
```sh
kubectl -n git-server exec deploy/git-server -- su git -c '
  git -C /srv/git/dashboards.git log --format="%h %an: %s" main
  git -C /srv/git/dashboards.git ls-tree --name-only main'
```

<!-- output: the-commit-of-the-application -->
```
068521f dashboard-sync: dashboards: save 1 file(s) from the application
0358498 Developer: Add the sales dashboard
sales.json
```

The commit has the author and the message of the Treebind. `render-cache.txt` is not in the repository, because it does not
match `*.json`: it stays on the volume, and each scan of the agent that finds it says `left out: NotIncluded 1` in the agent's
log (`kubectl -n demo logs dashboards-sync-0`).

## A dashboard changed in the application and in Git

Now the application renames the dashboard and, before the agent commits that, someone pushes another name from Git:

<!-- tested: change-it-on-both-sides -->
```sh
kubectl -n demo exec -i dashboards-app -- sh -c 'cat > /var/lib/dashboards/sales.json' <<'EOF'
{"title": "Sales (edited in the application)", "panels": 3}
EOF
kubectl -n git-server exec -i deploy/git-server -- su git -c sh <<'EOF'
cd ~/dashboards
git pull -q
echo '{"title": "Sales (edited in Git)", "panels": 3}' > sales.json
git commit -q -am "Rename the sales dashboard"
git push -q
EOF
```

What the agent does, in this order:

1. A pull that comes before the application's edit is committed, if one does, finds the edit in the way of the commit from
   Git and writes nothing: the agent never writes over a file that has an edit that is not committed. The pull says
   `Blocked`, and the next one tries again.
2. Once the edit has not changed for 15 seconds, the agent commits it. It does not push the commit, because the branch has a
   commit that the directory lacks, so the push would not be a fast-forward, and the agent never forces one. The push says
   `NonFastForward`.
3. The next pull finds that both sides changed `sales.json` and resolves it by the policy. With `followLocal` the application's
   version wins, in a merge commit, and the push that follows is a fast-forward.

It takes about half a minute. These commands wait until the agent has resolved the conflict and pushed the merge, as the
status of the Treebind says:

<!-- tested: wait-for-the-resolution -->
```sh
kubectl -n demo wait treebind/dashboards --for=jsonpath='{.status.conditions[?(@.type=="Conflict")].reason}'=ConflictResolved --timeout=3m
kubectl -n demo wait treebind/dashboards --for=condition=Synced --timeout=3m
```

The dashboard in the application, and the branch:

<!-- tested: see-the-resolution -->
```sh
kubectl -n demo exec dashboards-app -- cat /var/lib/dashboards/sales.json
kubectl -n git-server exec deploy/git-server -- su git -c 'git -C /srv/git/dashboards.git log --graph --format="%h %an: %s" main'
```

<!-- output: the-resolution -->
```
{"title": "Sales (edited in the application)", "panels": 3}
*   0d4a535 dashboard-sync: treebind: merge the branch main of the repository (5f44e1b7ce8b)
|\
| * 5f44e1b Developer: Rename the sales dashboard
* | f87fdfb dashboard-sync: dashboards: save 1 file(s) from the application
|/
* 068521f dashboard-sync: dashboards: save 1 file(s) from the application
* 0358498 Developer: Add the sales dashboard
```

The application's dashboard is the one on the branch, and the commit from Git is the second parent of the merge, where
`git show 5f44e1b:sales.json` finds it. The merge has a message of its own, which names the commit of the branch it took in.
`kubectl describe` says that a conflict was resolved:

<!-- tested: read-the-conflict -->
```sh
kubectl -n demo describe treebind dashboards
```

<!-- output: the-conflict-condition -->
```
...
    Message:               The last conflict between the volume and the repository that the sync agent found since it started was resolved by spec.sync.conflictPolicy. The log of the sync pod says how
    Observed Generation:   1
    Reason:                ConflictResolved
    Status:                False
    Type:                  Conflict
...
```

`Conflict` is `False`, because nothing waits for a person: the agent applied the policy and went on. It says so until the next
conflict, and the log of the sync pod has how many commits each side had and how many files took the winner's version. Among
the events of the Treebind there can also be a `Warning` for the pull that waited or for the push that was not sent, when the
operator read the agent's report while it said so. A conflict that the policy cannot resolve, such as one in a file with an edit that is not
committed, makes `Conflict` `True`: [recovering when the sync stops](../recovering-from-a-conflict-or-a-rejected-push.md#a-conflict-that-was-not-resolved)
says what to do then.

## Clean up

<!-- tested: clean-up -->
```sh
kubectl -n demo delete pod/dashboards-app treebind/dashboards pvc/dashboards-data
```

The repository stays on the Git server until its pod goes.
