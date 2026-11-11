# Local development

A contributor needs a laptop, a container runtime and no cloud account.
[CONTRIBUTING.md](../CONTRIBUTING.md) links here.

## Prerequisites

| Tool | Version | Why |
| :-- | :-- | :-- |
| Go | 1.26 or later (the module declares `go 1.26.0`) | Build and test |
| Git | A current release | The agent shells out to it only through `internal/git` |
| Docker or Podman | Any current release | Builds images and runs kind |
| kind | A current release | Local clusters for the end-to-end tests |
| kubectl | Within one minor version of the cluster | Talks to the cluster |
| make | GNU make | Every task has a target |
| kubebuilder | v4.16, only when you scaffold a new API | Code generation for new types |
| Helm (optional) | 4.x | Lint and render the chart in `charts/treebind` (`helm lint charts/treebind`) |
| ko, cosign | Pinned in `release.yml` | Used only by the release workflow; see [releasing.md](releasing.md) |

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

A single command that creates a kind cluster, builds and loads the images,
deploys the operator and starts an in-cluster Git remote (`make dev`) is
planned, so the sample works with no Git host account.

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
| `make dev` | kind cluster, images built and loaded, operator deployed | Planned |
| `make conformance STORAGE_CLASS=<name>` | Storage conformance suite against a storage class | Planned |
| `make docs-check` | Link and example check for the docs | Planned |

Run `make lint test` before you push. Run `make generate manifests` after you
change anything under `api/`, then commit the result, because CI fails on a
dirty diff.

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
