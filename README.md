# Treebind

Treebind is a Kubernetes operator that keeps a Git repository and an
application's files in sync, in both directions, through a shared volume. You
declare a `Treebind` that names the repository, an existing PVC and the sync
policy. The operator runs a sync agent that pulls changes, commits the
application's edits and optionally pushes them. The application is whatever
program reads and writes those files: a dashboard tool, a workflow engine, a
website.

> **Status: alpha, pre-release.** This repository currently holds the project
> scaffold and the `Treebind` `v1alpha1` API skeleton. Nothing syncs yet; the
> first working release is tracked by milestone
> [M1 MVP](https://github.com/mohanedmoh/treebind/milestones). Do not use it with
> real data.

## Quickstart

These commands install the API and run the operator against your current
cluster. They will do more once the sync engine lands.

```sh
git clone https://github.com/mohanedmoh/treebind.git && cd treebind
make install        # install the Treebind CRD into the current cluster
make run            # run the operator from your machine (the reconciler is a stub today)
kubectl apply -f config/samples/treebind_v1alpha1_treebind.yaml
kubectl get treebind
```

You need Go 1.26 or later, `kubectl` and a cluster such as
[kind](https://kind.sigs.k8s.io). See [docs/development.md](docs/development.md)
for the full setup.

## Storage support

Which storage classes Treebind works on, and how that was tested, will be
listed in the [storage support matrix](docs/storage-support.md).

## Deleting a Treebind

Deleting a Treebind removes its sync pod and Service. It never touches your
volume or the data on it; see
[docs/deleting-a-treebind.md](docs/deleting-a-treebind.md).

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md), and
see [GOVERNANCE.md](GOVERNANCE.md) for how decisions are made. Questions and
ideas go to
[Discussions](https://github.com/mohanedmoh/treebind/discussions). Report
security problems privately, as described in [SECURITY.md](SECURITY.md).

## License

Treebind is licensed under the Apache License, Version 2.0. See
[LICENSE](LICENSE) and [NOTICE](NOTICE).
