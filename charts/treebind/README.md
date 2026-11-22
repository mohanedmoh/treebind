# Treebind

Treebind keeps a Git repository and the files of an application in sync through a shared
PersistentVolumeClaim. This chart installs the operator that runs one sync agent for each `Treebind`.

The documentation is at [github.com/mohanedmoh/treebind](https://github.com/mohanedmoh/treebind). The conditions that a
`Treebind` reports, and what to do when one is not `True`, are in
[agent-status.md](https://github.com/mohanedmoh/treebind/blob/main/docs/agent-status.md) and
[recovering-from-a-conflict-or-a-rejected-push.md](https://github.com/mohanedmoh/treebind/blob/main/docs/recovering-from-a-conflict-or-a-rejected-push.md).

## Install

The chart and both images are published by a release, and signed. Install a release with its version (the chart version has
no leading `v`, the images have):

```sh
helm install treebind oci://ghcr.io/mohanedmoh/charts/treebind --version <version> \
  --namespace treebind-system --create-namespace
```

By default the images are `ghcr.io/mohanedmoh/treebind-operator` and `ghcr.io/mohanedmoh/treebind-agent`, tagged `v<version>`.
Pin a digest by putting it in the repository value (`manager.image.repository=<name>@sha256:...`). How to check the signatures
of the chart and the images is in
[releasing.md](https://github.com/mohanedmoh/treebind/blob/main/docs/releasing.md#verify-a-release).

From a checkout, which has no published images, build them into your cluster and set them as values:
[development.md](https://github.com/mohanedmoh/treebind/blob/main/docs/development.md#the-helm-chart) has the commands.

## What it installs

- The operator, as a Deployment of two replicas with leader election, so that one reconciles and the other takes over.
- The `Treebind` custom resource definition. It is kept when the release is uninstalled (`crd.keep`), and with it every
  `Treebind`, its sync pod and its claim.
- The roles that the operator needs, cluster-wide or in the namespaces that you name.
- The metrics Service of the operator, which is served over HTTPS and to callers that may read `/metrics`, and, with
  `prometheus.enabled`, a ServiceMonitor for the Prometheus Operator.

## The values that most installs set

| Value | Default | What it does |
| :-- | :-- | :-- |
| `manager.image.repository`, `manager.image.tag` | `ghcr.io/mohanedmoh/treebind-operator`, `v` and the chart's `appVersion` | The operator image. |
| `agent.image.repository`, `agent.image.tag` | `ghcr.io/mohanedmoh/treebind-agent`, `v` and the chart's `appVersion` | The image that the operator makes the sync pods from. |
| `manager.replicas` | `2` | Operator replicas. Keep `--leader-elect` while it is above 1. |
| `rbac.namespaced`, `rbac.namespaces` | `false` | Serve only the Treebinds of the namespaces listed, with a Role in each ([rbac.md](https://github.com/mohanedmoh/treebind/blob/main/docs/rbac.md)). |
| `crd.keep` | `true` | Keep the CRD when the release is uninstalled. |
| `metrics.secure`, `metrics.port` | `true`, `8443` | The metrics endpoint. |
| `prometheus.enabled` | `false` | Install a ServiceMonitor. |
| `networkPolicy.enabled` | `false` | Restrict ingress to the operator. |

The other values, with their comments, are in `values.yaml`.

## Uninstall

```sh
helm uninstall treebind --namespace treebind-system
```

The CRD stays, and with it every Treebind, its sync pod and its claim. The sync pods go on running, but nothing reconciles the
Treebinds or reads their status until an operator is installed again. Deleting the CRD deletes every Treebind; what deleting one
does to its pod and its claim is in [deleting-a-treebind.md](https://github.com/mohanedmoh/treebind/blob/main/docs/deleting-a-treebind.md).
