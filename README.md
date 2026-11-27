# Treebind

[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/treebind)](https://artifacthub.io/packages/helm/treebind/treebind)

Treebind is a Kubernetes operator that keeps a Git repository and an
application's files in sync, in both directions, through a shared volume. You
declare a `Treebind` that names the repository, an existing PVC and the sync
policy. The operator runs a sync agent that pulls changes, commits the
application's edits and optionally pushes them. The application is whatever
program reads and writes those files: a dashboard tool, a workflow engine, a
website.

> **Status: alpha.** A Treebind syncs in both directions, and releases are
> published as pre-releases. The API is `v1alpha1`, and its group,
> `treebind.example.org`, is a placeholder that will change before a stable
> release, and with it the `apiVersion` of every Treebind. Its tests run on kind.
> `v0.0.1-alpha.5` was also tried by hand on a cluster of two nodes, with local and
> NFS volumes. No storage class is listed as supported yet. Do not use it with data
> that you cannot lose.

## Quickstart

The [quickstart](docs/quickstart.md) takes you from an empty kind cluster to a
Treebind that syncs in both directions, with a Git server in the same cluster, in
about ten minutes: install the chart, apply one manifest, edit a file in a pod and
see the commit, push a change from Git and see the file, and read both outcomes in
`kubectl describe`. Three worked examples follow it: dashboards that people edit in
an application, a static site that Git deploys and nginx serves, and one directory
of a monorepo. CI runs every command of these pages on a kind cluster.

Each release publishes the Helm chart, `oci://ghcr.io/mohanedmoh/charts/treebind`,
and both images, all of them signed, and an `install.yaml` for `kubectl apply`.
The chart is listed on [Artifact Hub](https://artifacthub.io/packages/helm/treebind/treebind).
The [chart's README](charts/treebind/README.md) says what it installs, and
[releasing](docs/releasing.md#verify-a-release) how to check a release. To work on
Treebind itself, see [docs/development.md](docs/development.md).

## Storage support

Which storage classes Treebind works on, and how that was tested, will be
listed in the [storage support matrix](docs/storage-support.md).

## Operator availability

The operator runs as two replicas with leader election: one reconciles, and the
other takes over within seconds if it goes away. How a failover works and how long
it takes, where the replicas are placed, and how to run one replica for
development are in [docs/operator-replicas.md](docs/operator-replicas.md).

## What the operator may do

Its roles are the calls that it makes and no more, and it never reads a Secret. It can be limited to the
namespaces you name. What it may do, how to limit it and what that costs are in [docs/rbac.md](docs/rbac.md).

## Pod Security and file ownership

Both pods run under the `restricted` Pod Security level. What they set, and what
a volume that another user wrote to needs, is in
[docs/pod-security.md](docs/pod-security.md).

## Deleting a Treebind

Deleting a Treebind removes its sync pod and Service. It never touches your
volume or the data on it; see
[docs/deleting-a-treebind.md](docs/deleting-a-treebind.md).

## When the sync stops

A conflict that the agent cannot resolve, or a push that the Git host refuses, shows in the conditions `Synced` and `Conflict`. How to
read them, what the agent does by itself and what to do are in
[docs/recovering-from-a-conflict-or-a-rejected-push.md](docs/recovering-from-a-conflict-or-a-rejected-push.md).

## Metrics and events

Each change of `Synced` and `Conflict` is also an event on the Treebind and a Prometheus metric, and an alert can say that a
Treebind has not synced for several of its intervals. The events, the metrics and the expressions to alert on are in
[docs/metrics-and-events.md](docs/metrics-and-events.md).

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md), and
see [GOVERNANCE.md](GOVERNANCE.md) for how decisions are made. Questions and
ideas go to
[Discussions](https://github.com/mohanedmoh/treebind/discussions). Report
security problems privately, as described in [SECURITY.md](SECURITY.md).

## License

Treebind is licensed under the Apache License, Version 2.0. See
[LICENSE](LICENSE) and [NOTICE](NOTICE).
