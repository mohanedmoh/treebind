# Operator replicas and leader election

The operator runs as two replicas with leader election. This page says what that
gives you, how a failover goes and how long it takes, where the replicas are
placed, and how to run one replica for development.

## What two replicas give

Availability of the control loop. They do not reconcile in parallel: one replica,
the leader, reconciles every Treebind, and the other waits as a standby. If the
leader's pod or node goes away, the standby takes over within seconds, and an
update of the operator or a drain of a node never leaves the cluster without a
replica that can take the lease.

The sync pods that the operator creates do not depend on the operator once they
run, so a failover does not interrupt a sync. What waits during a failover is the
reconciling itself: a Treebind that you create or change in that window is handled
as soon as a leader exists.

Both replicas serve the health probes and the metrics endpoint, and a standby
reports ready: `/readyz` does not depend on holding the lease. A ready pod therefore
does not tell you who leads; the lease does (see [Checking](#checking)). The metrics
Service selects both pods, so a request through the Service reaches either one, and
only the leader's reconcile counters move.

## How failover works

Every replica runs with `--leader-elect`. It asks for a Lease named
`c4984a8a.example.org` in the namespace the operator runs in. The replica that holds
it reconciles and renews it; the others keep trying to take it. The Role that allows
this is namespaced, so leader election adds no cluster-wide permission.

The timings are set in `cmd/operator/main.go`, and a test keeps this table in step
with them:

| Setting | Value | What it does |
| :-- | :-- | :-- |
| Lease duration | 15 seconds | How long a standby waits after the last renewal it saw before it takes the lease. |
| Renew deadline | 10 seconds | How long the leader keeps trying to renew before it gives up and exits. |
| Retry period | 2 seconds | How often every replica tries to acquire or renew. Each wait is lengthened by up to 120%, so a retry comes 2 to 4.4 seconds after the last. |

There are two kinds of failover.

- **A planned handover**: a rolling update, a drain or a deleted pod. The leader
  stops its controllers when it gets the termination signal, and then gives the lease
  up, so a waiting replica takes it at its next retry: within about 4.4 seconds, and
  at once for a replica that has just started. A leader whose controllers are still
  busy when the 10 second termination grace period ends is killed, and then the
  handover is the second kind below. Measured on a kind cluster, the lease
  changed hands in under a second, and a Treebind created right after the delete was
  reconciled about a second later.
- **A crash or a lost node**: the lease is not released, so the standby waits for it
  to run out, 15 seconds after the last renewal it saw, and then one retry. Measured
  on a kind cluster with the leader's process killed, a Treebind created right after
  was reconciled 15.6 to 18.5 seconds later.

The renew deadline is shorter than the lease duration on purpose. A leader that
loses contact with the API server stops reconciling after 10 seconds and exits, and
the kubelet restarts it as a standby; a standby cannot take the lease before 15
seconds have passed, so two replicas never reconcile at the same time.

## Where the replicas run

- **Spread**: a soft topology spread constraint prefers different nodes. It does not
  require them, so both replicas still start on a cluster with one node, such as kind
  or k3s. It counts only the pods of the same revision, which keeps the replicas
  apart across an update too. [ADR 0014](adr/0014-operator-replicas-and-their-placement.md)
  gives the reasons.
- **Rolling update**: `maxSurge: 1` and `maxUnavailable: 0`. An update starts a third
  pod, waits for it to be ready, and only then stops an old one, so two replicas are
  ready throughout.
- **PodDisruptionBudget**: `maxUnavailable: 1`, so a drain evicts one replica at a
  time and waits for it to be replaced before it evicts the other. A replica that is
  not ready can always be evicted, so a broken replica never holds a node. A budget
  of `maxUnavailable` and not `minAvailable` stays at one when you run a single
  replica, so it never blocks a drain.

The Helm chart takes each of these from a value, and `config/manager` carries the
same settings for `kubectl` and `kustomize`:

| Value | Default | What it sets |
| :-- | :-- | :-- |
| `manager.replicas` | `2` | The number of replicas. |
| `manager.strategy` | rolling update, surge 1, unavailable 0 | The Deployment strategy. |
| `manager.podDisruptionBudget.enabled` | `true` | Whether the chart installs the PodDisruptionBudget. |
| `manager.podDisruptionBudget.maxUnavailable` | `1` | How many replicas a drain may evict at once. |
| `manager.topologySpreadConstraints` | soft spread over nodes | Set it to `[]` to leave the placement to the scheduler, or to your own list. |
| `manager.affinity` | none | For a required rule, a pod anti-affinity on `kubernetes.io/hostname`. Use it only on a cluster that always has at least two nodes, and set `manager.strategy` to `maxSurge: 0` and `maxUnavailable: 1`: with two nodes the extra pod of a rolling update would otherwise stay Pending. |

## Running one replica

One replica is enough to develop on, and the operator still works with it: it holds
the lease on its own.

```sh
# a cluster from make dev, or one installed from config/
kubectl -n treebind-system scale deployment/treebind-controller-manager --replicas=1

# the Helm chart
helm upgrade --install treebind charts/treebind --set manager.replicas=1
```

`make deploy` and `make dev` set the count back to two the next time they run.
To run the operator from your machine, use `make run`: it does not pass
`--leader-elect`, so it takes no lease and reconciles at once. Stop the in-cluster
operator first, or both reconcile.

## Checking

```sh
kubectl -n treebind-system get pods -l control-plane=controller-manager
kubectl -n treebind-system get lease    # HOLDER names the pod that leads
kubectl -n treebind-system get pdb      # ALLOWED DISRUPTIONS is 1
kubectl -n treebind-system logs -l control-plane=controller-manager --prefix
```

A standby logs its start-up and `Attempting to acquire leader lease`, and nothing about
reconciling until it leads. When both replicas start at the same moment, the one that loses the race to create
the lease logs `Error initially creating lease lock ... already exists`. That is
expected, and it keeps waiting.
