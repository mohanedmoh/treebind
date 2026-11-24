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
| Helm (optional) | Pinned in `release.yml` | Lint and render the chart in `charts/treebind` (`helm lint charts/treebind`). `make test-chart` downloads the pinned version into `bin/` itself (linux/amd64, checked against its SHA-256) |
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
starts a throwaway Git remote in the `treebind-dev` namespace, so the loop needs
no Git host account. Run it
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

Another puts a second Treebind on the claim and the `subPath` of the first, which the operator accepts and whose agent
waits for the writer lock for as long as the first agent holds it, and freezes the first agent with the freezer of cgroup v2 for
longer than the lock's TTL while the application writes a file. The second agent takes the lock over and commits the file; the
spec thaws the first as soon as it sees that, checks that it stops without writing and comes back as the standby, and deletes the
second Treebind, after which the first takes the lock again and syncs in both directions. It adds a pod that mounts the whole claim
read-only, to read the lock's slots and look for files that a step leaves, which the pod of the application cannot see.

Another fills the volume of a Treebind until its agent cannot write to it, and frees it again. Its claim is on a tmpfs of 16 MiB
that the privileged pod mounts on the node, so it needs a node where that pod can mount a tmpfs, as the node of a kind cluster is, and
it never fills the node's disk: a claim of local-path is a directory on that disk, and the script writes only into a tmpfs, and never
more than the tmpfs holds.

CI runs them in a job of their own, `e2e-failures`, on a kind cluster of its own and in parallel with the `e2e` job, on every pull
request that changes code, so that a pull request waits for the longer of the two and not for their sum. The nightly workflow runs
them as well on each Kubernetes version of the support window.

`hack/inject.sh` is what kills, freezes and fills, and you can run it against `make dev` to see a failure yourself:
`hack/inject.sh kill <namespace> <pod of the sync>` kills the agent, `freeze` and `thaw` stop and start it, and
`hack/inject.sh cleanup` unmounts the tmpfs and removes the privileged pod. It refuses a kubectl context that is not a kind cluster.
`hack/inject.sh kill-at <namespace> <pod> agent <seconds> +/work/.treebind/updating` kills the agent in a moment that files on
its volume mark (`+` for a file that has to be there, `-` for one that must not): it freezes the container when they all hold, and
kills it only when they still hold once it is frozen. `kill-git-at` does the same to a Git that the agent started, from inside its
container, and the agent runs on. `tmpfs-mount <node> <name> <mebibytes>` mounts a tmpfs on a node for a static PersistentVolume of type
`local` to point at, `tmpfs-fill <node> <name> <bytes-left>` fills it with zeros until that many bytes are free, and `tmpfs-free`,
`tmpfs-df` and `tmpfs-unmount` take the filler away, say how much is free and unmount it.

## The Helm chart

`make test-chart` installs the chart in `charts/treebind` on a kind cluster that it creates and deletes, as `make test-e2e` does, and
proves that a Treebind syncs through it: what the definition of done above proves for the kustomize deployment. The script is
`hack/test-chart.sh`, and CI runs it as the job `chart-install`, with the same node image as `e2e`, so a contributor runs the same
thing as CI. It needs Docker, kind, `kubectl`, `ssh-keygen`, the network, and `make ko` (it downloads ko into `bin/`).

It downloads the Helm that `release.yml` pins, by version and SHA-256, into `bin/`, so it needs no Helm of its own; on a machine that is
not linux/amd64, set `HELM` to a Helm of that version. It builds the operator and the agent image from the checkout with ko into the
nodes of the cluster, as `make dev` does, and installs the chart with `--set` for both images, since a release has not published them.
It then checks that the operator rolls out as two replicas, that the CRD is `Established` and that the operator has the agent image
(`AGENT_IMAGE`, from `agent.image`), starts the ssh remote of `make dev-ssh`, and waits until a Treebind that syncs in both directions
is `Ready` and `Synced` with the commit that the remote has. A file that the remote was seeded with has to be in a pod that mounts the
claim, and a file that this pod writes has to be a commit on the remote, which is the pull and the push. A `helm upgrade` with the
same values must not replace the operator's pods. After `helm uninstall` the operator is gone, and the CRD, the Treebind and its sync
pod are still there, because the chart keeps the CRD (`crd.keep`): the script checks that and installs the chart again over what was
kept. It takes about four minutes on a machine with a warm Go cache, and most of that is the kind cluster, the two image builds and the wait
for the status of the Treebind, which the operator reads from the agent every 30 seconds. Cleanup is always done, and a failure prints
the state of the cluster before it.

The cluster has to be fresh: the script stops if the Treebind CRD or the namespace `treebind-system` is there, as they are after
`make deploy` or `make dev`.

To install the chart in a cluster of your own from a checkout, build the two images into it and pass them to the chart. The chart of a
checkout has the version `0.0.0-dev`, whose images no release publishes, so without them the operator, and the sync pods that it makes,
cannot pull theirs. To install a release instead, follow [the quickstart](quickstart.md#1-install-the-operator). ko prints
`kind.local/<name>:<tag>`, which the chart takes as a repository and a tag:

```sh
make ko
export KO_DOCKER_REPO=kind.local KIND_CLUSTER_NAME=kind   # the name of your kind cluster
operator="$(bin/ko build --base-import-paths --platform="linux/$(go env GOARCH)" ./cmd/operator)"
agent="$(bin/ko build --base-import-paths --platform="linux/$(go env GOARCH)" ./cmd/agent)"
helm install treebind charts/treebind --namespace treebind-system --create-namespace \
  --set manager.image.repository="${operator%:*}" --set manager.image.tag="${operator##*:}" \
  --set agent.image.repository="${agent%:*}" --set agent.image.tag="${agent##*:}"
kubectl -n treebind-system rollout status deployment/treebind-controller-manager
```

A release publishes both images with the tag `v<chart version>`, which the chart uses when `manager.image.tag` and `agent.image.tag` are
not set, so an install from the released chart needs no `--set`.

## The quickstart

`make test-quickstart` runs the commands of [the quickstart](quickstart.md) and of the three worked examples in `docs/examples/` on a
kind cluster that it creates and deletes, as a reader runs them, so that the pages cannot say what the code does not do. The script
is `hack/test-quickstart.sh`, and CI runs it as the job `quickstart` with the same node image as `e2e`, for a pull request that
changes code and for one that changes only those pages. It needs what `make test-chart` needs, and Perl, and takes about eight
minutes on a machine with a warm Go cache, a third of it the two image builds and most of the rest the waits that the pages ask for.

Each command of the pages is in a fenced `sh` block that follows a line `<!-- tested: <name> -->`. The script takes the blocks out
of the pages and runs each as it is written, in a bash of its own and in one working directory, in the order of the pages, as a
reader pastes them into one terminal; so a block may use the files that an earlier block wrote, and not its shell variables. Between
the blocks the script checks what the page says the reader sees. What a command prints is shown in a block that follows a line
`<!-- output: <name> -->`, and the script holds it to what the command printed, with commit ids, times and ages left out and runs of
spaces taken as one; a line `...` stands for lines that the page leaves out. A block that a reader runs again until a change is
there, such as a look at the branch after a write, is run until it prints what the page shows. A block that the test cannot run,
such as `kind create cluster` or the commands for a repository of your own, follows a line `<!-- not tested: <why> -->`. The
script fails when an `sh` block of the pages has neither line, when a page marks a block or an output that the script does not
run or hold, and when `docs/examples/` has a page that it does not run, so a command that is added to a page has to be added to
the script as well.

The pages install the published chart, and a pull request is not a release. So the script builds the operator and the agent image
from the checkout with ko into the cluster, as `make test-chart` does, and puts a `helm` first on the PATH of the blocks that runs the
pinned Helm with the same arguments, except that in `helm install` the chart has to be `oci://ghcr.io/mohanedmoh/charts/treebind`
with the version that the page names, which it replaces with `charts/treebind` and `--set` for the two images. Nothing else of any
command changes. `make test-quickstart RELEASED=1` runs the pages exactly as they are written, against the published chart and
images, which is how a release is checked ([releasing](releasing.md#release-checklist)). The version that the page installs is
printed by `hack/quickstart-version.sh`, and the release workflow refuses a tag that is not that version.

When a change to the code changes what a page shows, such as the message of a condition, change the page in the same pull request.

## Make targets

| Target | What it does | Status |
| :-- | :-- | :-- |
| `make test` | Generates code, formats, vets, then runs unit tests and envtest | Available |
| `make lint` | golangci-lint (`make lint-config` checks its configuration) | Available |
| `make dev-ssh` | Starts an ssh Git remote in the dev cluster with a deploy key and known hosts, and prints a Treebind for it | Available |
| `make generate manifests` | Regenerates deepcopy code, CRDs, the chart's copy of the CRD and RBAC from the Go types | Available |
| `make test-e2e` | End-to-end suite on a kind cluster, created if missing, without the specs that cause a failure | Available |
| `make test-e2e-failures` | Only the end-to-end specs that cause a failure (label `failure`), on a kind cluster, created if missing | Available |
| `make test-chart` | Installs the Helm chart on a kind cluster, created if missing and deleted after, and syncs a Treebind through it (`hack/test-chart.sh`) | Available |
| `make test-quickstart` | Runs the commands of the quickstart and of the worked examples on a kind cluster, created if missing and deleted after, with the chart and images of the checkout, or of the release with `RELEASED=1` (`hack/test-quickstart.sh`) | Available |
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
`config/`; do not edit them by hand. Helm reads every `{{` of a template as the start of an action, so
`hack/sync-chart-crd.sh` writes each `{{` and `}}` of the CRD as a Helm string, and the chart job of CI renders the CRD with Helm and
compares it with the generated CRD. The chart's manager Deployment is written
by hand. `make test` compares it with `config/manager/manager.yaml` and names
the field that differs, so change both when you change one: the arguments, the
probes and ports, the security contexts, the resources, the replica count, the
rolling update strategy and the topology spread. It does the same for the
PodDisruptionBudget and the leader election role. It checks that the chart sets
`AGENT_IMAGE` from `agent.image` and that the default repository is the one in
`config/manager`, and it does not look at the metrics flags. `make test-chart`
installs the chart for real.

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
