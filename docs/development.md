# Local development

A contributor needs a laptop, a container runtime and no cloud account.
[CONTRIBUTING.md](../CONTRIBUTING.md) links here.

## Prerequisites

| Tool | Version | Why |
| :-- | :-- | :-- |
| Go | 1.26 or later (the module declares `go 1.26.0`) | Build and test |
| Git | A current release | The agent shells out to it only through `internal/git` |
| Docker or Podman | Any current release | Builds images and runs kind |
| kind | A current release | Local clusters for `make dev` and the end-to-end tests |
| kubectl | Within one minor version of the cluster | Talks to the cluster |
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
kubectl -n treebind-system logs deploy/treebind-controller-manager -f
```

The operator creates a one-replica StatefulSet and a headless Service, both named
`treebind-sample-sync`, and reports `Ready` in the Treebind's status. The sample
names a claim, a key Secret and a known hosts ConfigMap that the dev cluster does
not have, so the sync pod stays `Pending` until you create them. Once it runs,
the sync agent is still a stub that exits, so the pod restarts and `Ready` stays
`False` until the agent lands. The sample still names a GitHub repository.

The Git remote serves one empty bare repository, `sample.git`, over the git
protocol. Reach it from the terminal through a port-forward:

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
the pod is recreated. The `git://` URL is for your terminal only; once the sync agent
lands, the dev remote grows smart HTTP so a Treebind can point at it with an
`http://` URL.

```sh
make dev-down
```

deletes the cluster.

## Make targets

| Target | What it does | Status |
| :-- | :-- | :-- |
| `make test` | Generates code, formats, vets, then runs unit tests and envtest | Available |
| `make lint` | golangci-lint (`make lint-config` checks its configuration) | Available |
| `make generate manifests` | Regenerates deepcopy code, CRDs, the chart's copy of the CRD and RBAC from the Go types | Available |
| `make test-e2e` | End-to-end suite on a kind cluster, created if missing | Available |
| `make cleanup-test-e2e` | Deletes the kind cluster used by the e2e suite | Available |
| `make install` / `make uninstall` | Installs or removes the CRD in the current cluster | Available |
| `make build` | Builds the operator (`cmd/operator`) and agent (`cmd/agent`) binaries into `bin/` | Available |
| `make run` | Runs the operator from your machine | Available |
| `make docker-build` | Builds the operator image | Available |
| `make dev` | kind cluster, images built and loaded, operator deployed, dev Git remote started | Available |
| `make dev-down` | Deletes the kind cluster used by `make dev` | Available |
| `make conformance STORAGE_CLASS=<name>` | Storage conformance suite against a storage class | Planned |
| `make docs-check` | Link and example check for the docs | Planned |

Run `make lint test` before you push. Run `make generate manifests` after you
change anything under `api/` or the RBAC markers in the controller, then commit
the result, because CI fails on a dirty diff.

The chart's copies of the CRD and of the operator's role are generated from
`config/`; do not edit them by hand. The chart's manager Deployment is written
by hand. `make test` compares it with `config/manager/manager.yaml` and names
the field that differs, so change both when you change one: the arguments, the
probes and ports, the security contexts, the resources and the replica count. It
does not look at the agent image or the metrics flags.

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
