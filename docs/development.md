# Local development

A contributor needs a laptop, a container runtime and no cloud account.
[CONTRIBUTING.md](../CONTRIBUTING.md) links here.

## Prerequisites

| Tool | Version | Why |
| :-- | :-- | :-- |
| Go | 1.26 or later (the module declares `go 1.26.0`) | Build and test |
| Git | A current release | The agent shells out to it only through `internal/agent/git` |
| Docker or Podman | Any current release | Builds images and runs kind |
| kind | A current release | Local clusters for `make dev` and the end-to-end tests |
| kubectl | Within one minor version of the cluster | Talks to the cluster |
| ssh-keygen | Any OpenSSH client | `make dev-ssh` and the end-to-end suite make the deploy key of the ssh remote with it |
| make | GNU make | Every task has a target |
| kubebuilder | v4.16, only when you scaffold a new API | Code generation for new types |
| Helm (optional) | 4.x | Lint and render the chart in `charts/treebind` (`helm lint charts/treebind`) |
| ko | Pinned in the Makefile and `release.yml` | `make dev` downloads it into `bin/`; the release workflow pins the same version |
| cosign | Pinned in `release.yml` | Used only by the release workflow; see [releasing.md](releasing.md) |

The Kubernetes versions under test are 1.35 to 1.37. The CI matrix moves up as
versions leave support (1.35 reaches end of life on 28 February 2027). Tools such
as `controller-gen` and `setup-envtest` are downloaded by `make` into `bin/` on
first use.

## First run

```sh
git clone https://github.com/<you>/treebind.git
cd treebind
git remote add upstream https://github.com/mohanedmoh/treebind.git

make install        # install the CRD into the current cluster
make run            # run the operator from your machine
kubectl apply -f config/samples/treebind_v1alpha1_treebind.yaml
kubectl get treebind -w
```

`make run` needs a cluster to talk to. If you do not have one, the next
section gives you everything, operator included, in a single command.

## The dev cluster

`make dev` needs only Docker and kind. It creates a kind cluster named
`treebind-dev` if it does not exist, builds the operator and agent images with
ko straight into the cluster, installs the CRD, deploys the operator and
starts a throwaway Git remote in the `treebind-dev` namespace, which the sync
agent will use once it exists, so the loop needs no Git host account. Run it
again after a code change: the cluster is reused and the images are rebuilt.

```sh
make dev
kubectl apply -f config/samples/treebind_v1alpha1_treebind.yaml
kubectl get treebind,statefulset,service,pod
kubectl -n treebind-system logs -l control-plane=controller-manager --prefix -f
```

The operator runs as two replicas here, as it does everywhere, and the one that
holds the lease reconciles. The other waits for the lease and reconciles nothing,
which is why the command above reads the logs of both. To work with a single
replica, see [operator-replicas.md](operator-replicas.md#running-one-replica).

The operator creates a one-replica StatefulSet and a headless Service, both named
`treebind-sample-sync`, and reports `Ready` in the Treebind's status. The sample
names a claim, a key Secret and a known hosts ConfigMap that the dev cluster does
not have, so the sync pod stays `Pending` until you create them. Once it runs, the
agent makes the checkout and pulls, and `Ready` turns `True` after the first pull that
passes ([the probes](agent-status.md#the-probes)). The sample still names a GitHub
repository.

The Git remote serves one empty bare repository, `sample.git`, over the git
protocol. A Treebind that syncs from it is not ready while it has no commit: the
pull says `BranchGone`, and `Ready` stays `False` until a commit is pushed to it, by you or, for a Treebind that
syncs in both directions, by the agent with the first commit of what the application writes. Reach it
from the terminal through a port-forward:

```sh
kubectl -n treebind-dev port-forward svc/git-remote 9418:9418 &
git clone git://127.0.0.1/sample.git
cd sample
git commit --allow-empty -m "First commit"
git push origin HEAD:main
cd ..
kill %1
```

The remote accepts anonymous pushes, so keep it behind the port-forward and do
not expose it. The repository lives in an emptyDir, so it is empty again after
the pod is recreated. The `git://` URL is for your terminal only: the sync agent
speaks ssh and https and refuses the rest, so a Treebind needs the ssh remote below.

### An ssh remote for a Treebind

```sh
make dev-ssh
```

starts a second remote, `git-remote-ssh`, that speaks ssh, and makes what a Treebind
needs to reach it: a deploy key in the Secret `dev-deploy-key`, the host key of the remote
in the ConfigMap `dev-known-hosts`, both in the namespace `default` (`hack/dev-ssh.sh
<namespace>` takes another), and it prints a claim and a Treebind to apply. The pod runs
sshd as root and installs it with `apk` when it starts, so it needs the network and is for
the dev cluster only. The repository is `/srv/sample.git`, empty until you put something in it:

```sh
kubectl -n treebind-dev exec deploy/git-remote-ssh -- su git -c '
  git init -q --initial-branch=main /tmp/seed && cd /tmp/seed &&
  git config user.name dev && git config user.email dev@example.com &&
  echo hello > hello.txt && git add -A && git commit -q -m first &&
  git push -q /srv/sample.git main'
```

The sync agent then clones it into the claim, and its log and report say so:

```sh
kubectl logs dev-ssh-sync-0 | grep Checkout
```

```sh
make dev-down
```

deletes the cluster.

## The end-to-end tests

`make test-e2e` runs the suite in `test/e2e` on a kind cluster. Besides the specs of the operator itself, it runs the definition
of the MVP with the real agent: a Treebind that syncs in both directions, a pod that mounts the claim with the same `subPath` as an
application does, and the ssh remote that `make dev-ssh` starts. A file written in the pod has to be a commit on the remote, a
commit pushed from Git has to be a file in the pod, and both have to go on after the pod of the sync is deleted with and without its
grace period and after the leader of the operator is deleted. The last spec reads the outcomes as a user does, with `kubectl
describe`: after each of the two changes `Synced` is `True` with the reason `InSync`, `Conflict` is `False`, `Ready` stays `True`,
and `Last Synced Commit` is the full id of the tip of the remote ([the conditions](agent-status.md#what-synced-and-conflict-say)).
The operator reads the report of the agent every 30 seconds, so that spec waits for the status for up to two minutes after the
change has arrived.

The suite builds the agent image with ko into the cluster, as `make dev` does, so `make ko` downloads it the first time. It needs
the network for what the cluster pulls (the image of the remote, `busybox` for the pod of the application) and for the `apk` that the
pod of the remote runs when it starts. To run only the definition of done against a cluster that is there, give the focus
`Definition of done` to the `go test` command that the target runs.

The specs that cause a failure on purpose carry the Ginkgo label `failure`. `make test-e2e` leaves them out, and
`make test-e2e-failures` runs only them, on a kind cluster that it creates and removes as `make test-e2e` does, with a timeout of
an hour. They need more of the cluster than the other specs do. The agent is the first process of its container, which no process
of the container can kill or stop, so they kill and freeze it from its node, through a privileged pod with `hostPID` that runs
`busybox`: the cluster has to admit such a pod, as kind does, and the node has to use cgroup v2, whose freezer they use, as
GitHub's runners and current Linux distributions do. The specs remove that pod when they end. The first of them kills the agent
between two steps, as an out-of-memory kill does, and checks that its container comes back in the same pod, waits a TTL of the
writer lock that the killed agent did not give up, takes it with a higher epoch, and brings in what was written on both sides while
it was down ([#55](https://github.com/mohanedmoh/treebind/issues/55)). The next ones kill the agent in the middle of `git add` and
in the middle of the files of an update, and kill a Git that the agent started in the middle of `git add`, each on a burst of 3000
small files, which with their objects take about 25 MB of the node's disk until the spec removes its claim and its repository.

Another takes the ssh remote away while the application writes a file and a commit is pushed from Git in the pod of the remote, and
brings it back. The remote keeps its repositories in an `emptyDir` and makes its host keys when it starts, so it is not stopped:
its Service is given a selector that matches no pod, so that kube-proxy refuses the connections of the agent, and then the selector
it had. The spec checks that `Synced` says `FetchFailed` while `Ready` stays `True`, and that once the remote is back each change is
on the other side, added by one commit, within the five minutes that the back-off of the agent's pulls calls for. It needs nothing
beyond what the other failure specs need.

CI runs them in a job of their own, `e2e-failures`, on a kind cluster of its own and in parallel with the `e2e` job, on every pull
request that changes code, so that a pull request waits for the longer of the two and not for their sum. The nightly workflow runs
them as well on each Kubernetes version of the support window.

`hack/inject.sh` is what kills and freezes, and you can run it against `make dev` to see a failure yourself:
`hack/inject.sh kill <namespace> <pod of the sync>` kills the agent, `freeze` and `thaw` stop and start it, and
`hack/inject.sh cleanup` removes the privileged pod. It refuses a kubectl context that is not a kind cluster.
`hack/inject.sh kill-at <namespace> <pod> agent <seconds> +/work/.treebind/updating` kills the agent in a moment that files on
its volume mark (`+` for a file that has to be there, `-` for one that must not): it freezes the container when they all hold, and
kills it only when they still hold once it is frozen. `kill-git-at` does the same to a Git that the agent started, from inside its
container, and the agent runs on.

## Make targets

| Target | What it does | Status |
| :-- | :-- | :-- |
| `make test` | Generates code, formats, vets, then runs unit tests and envtest | Available |
| `make lint` | golangci-lint (`make lint-config` checks its configuration) | Available |
| `make dev-ssh` | Starts an ssh Git remote in the dev cluster with a deploy key and known hosts, and prints a Treebind for it | Available |
| `make generate manifests` | Regenerates deepcopy code, CRDs, the chart's copy of the CRD and RBAC from the Go types | Available |
| `make test-e2e` | End-to-end suite on a kind cluster, created if missing, without the specs that cause a failure | Available |
| `make test-e2e-failures` | Only the end-to-end specs that cause a failure (label `failure`), on a kind cluster, created if missing | Available |
| `make cleanup-test-e2e` | Deletes the kind cluster used by the e2e suite | Available |
| `make install` / `make uninstall` | Installs or removes the CRD in the current cluster | Available |
| `make build` | Builds the operator (`cmd/operator`) and agent (`cmd/agent`) binaries into `bin/` | Available |
| `make run` | Runs the operator from your machine | Available |
| `make docker-build` | Builds the operator image | Available |
| `make dev` | kind cluster, images built and loaded, operator deployed, dev Git remote started | Available |
| `make dev-down` | Deletes the kind cluster used by `make dev` | Available |
| `make check-agent-image` | Builds the agent image with ko and checks that it runs Git and ssh as its user on a read-only root filesystem | Available |
| `make conformance STORAGE_CLASS=<name>` | Storage conformance suite against a storage class | Planned |
| `make docs-check` | Link and example check for the docs | Planned |

Run `make lint test` before you push. Run `make generate manifests` after you
change anything under `api/` or the RBAC markers in the controller, then commit
the result, because CI fails on a dirty diff.

The chart's copies of the CRD and of the operator's role are generated from
`config/`; do not edit them by hand. The chart's manager Deployment is written
by hand. `make test` compares it with `config/manager/manager.yaml` and names
the field that differs, so change both when you change one: the arguments, the
probes and ports, the security contexts, the resources, the replica count, the
rolling update strategy and the topology spread. It does the same for the
PodDisruptionBudget and the leader election role. It does not look at the agent
image or the metrics flags.

## Working with Git

- Fork the repository, work on a branch in your fork, and open the pull request
  against `main`.
- Keep your fork current with `git fetch upstream` and `git rebase upstream/main`.
- Sign off every commit with `git commit -s`. To add it to commits you already
  made: `git rebase --signoff upstream/main`.
- Set `user.name` and `user.email` to the identity you want in the project
  history. The sign-off line uses them.

## Troubleshooting

| Symptom | Likely cause |
| :-- | :-- |
| An image cannot be pulled or loaded into kind | Docker is not running, or kind cannot reach the registry. Rerun after `docker info` works. |
| CRD changes do not appear in the cluster | Run `make manifests` and `make install` again, then check `kubectl get crd`. |
| envtest fails to start | The test binaries are missing. `make test` downloads them on first run, so a proxy or offline machine needs them fetched once. |
| e2e leaves clusters behind | Run `make cleanup-test-e2e`, or `kind get clusters` and `kind delete cluster --name <name>`. |
