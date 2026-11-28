# Storage support matrix

Treebind syncs files through a PVC, so how well it works depends on the storage
behind that PVC: locking, atomic rename, file events and the executable bit all
differ between local disks, NFS and cloud file systems.

**Status: not measured yet.** No storage class has been tested, so nothing is
listed as supported. The matrix below fills in as the conformance suite lands
and as people report results.

| Storage | Access mode | Result | Treebind version | Date | Tested on | Report |
| :-- | :-- | :-- | :-- | :-- | :-- | :-- |
| local-path | ReadWriteOnce | Not tested | | | | |
| NFS (in-cluster) | ReadWriteMany | Not tested | | | | |
| CephFS / RBD | | Not tested | | | | |
| AWS EFS / EBS | | Not tested | | | | |
| Azure Files / Disk | | Not tested | | | | |
| GCP Filestore / PD | | Not tested | | | | |

## Access modes and where the sync pod runs

A `ReadWriteOnce` claim is mounted on one node at a time, so the sync pod and the application have to run on the same
node. Tell the operator which pods are the application's, and it gives the sync pod a **required pod affinity** to them,
on `kubernetes.io/hostname`. The scheduler chooses the node: the operator never sets `nodeName`.

```yaml
spec:
  target:
    claimName: grafana-data
  placement:
    appSelector:
      matchLabels:
        app.kubernetes.io/name: grafana
```

`appSelector` is a label selector for pods in the namespace of the Treebind, and needs an entry in `matchLabels` or
`matchExpressions`. It is newer than 0.0.1-alpha.5. The operator reads the access modes that the claim asks for
(`spec.accessModes`), and not those of the volume that it is bound to:

| The claim lists | The sync pod |
| :-- | :-- |
| `ReadWriteMany`, with or without others | Runs anywhere |
| `ReadWriteOnce`, with or without `ReadOnlyMany` | Runs on the node of a selected pod |
| `ReadWriteOncePod` | Gets the same affinity, but only one pod can use such a claim: [see below](#readwriteoncepod) |
| `ReadOnlyMany` alone | Runs anywhere. The agent has to write to the volume, which such a claim does not promise |
| The claim is not there yet | Waits as for `ReadWriteOnce`, until the operator has read the claim, which it asks again about every 30 seconds while the sync pod is not ready |

Without `appSelector` the operator does not read the claim, and the scheduler puts the sync pod anywhere, as before.
That is right for a `ReadWriteMany` claim. For a `ReadWriteOnce` claim on a cluster of several nodes the sync pod can
land away from the application, and one of the two cannot mount the claim.

### Choosing the selector

- **Name pods that mount the claim, and only those.** One selected pod is enough for a node, so a label that the whole
  application shares, such as one on a web tier and on a database with a claim of its own, can send the sync pod to the
  wrong node. For one claim of a StatefulSet, select its one pod by `statefulset.kubernetes.io/pod-name`.
- **Several selected pods** on several nodes: the sync pod may go to any of those nodes. Pods that mount one
  `ReadWriteOnce` claim can all run only on one node, so selected pods that run on different nodes do not all mount this
  claim, and the selector is too wide.
- **Sync pods never count.** The operator adds `app.kubernetes.io/managed-by NotIn (treebind-operator)` to the
  selector, so a selector that also matches the labels of a sync pod, its own through `spec.agent.labels` included,
  cannot be satisfied by that pod itself.
- **Set it on every Treebind of the claim.** Each sync pod follows the application and not another sync pod.

### When there is no application pod

The sync pod is `Pending`, `Ready` is `False` with `SyncPodUnschedulable`, and nothing is synced until a selected pod
has a node, as for an application that is scaled to zero when the sync pod is scheduled. A selected pod does not have to
be ready, so an application that waits in an init container for the files of the repository gets them. On a volume that is bound at first use, the application's node also decides where the volume
is made.

The node also has to take the sync pod: it needs room for it, and `spec.agent.tolerations` for a taint that the
application tolerates. A `spec.agent.nodeSelector` that the application's node does not match leaves it `Pending`.

### When the application moves

The affinity holds when the sync pod is scheduled, and not afterwards. **The sync pod does not follow an application
that moves to another node** ([#113](https://github.com/mohanedmoh/treebind/issues/113)): it stays, with the volume
mounted, and an application pod on another node cannot mount it (for a volume that is attached to one node at a time,
`kubectl describe pod` shows `Multi-Attach error`). A rollout can do that, and so can an application that is scaled to
zero and comes back elsewhere while the sync pod runs on. Delete the sync pod, and its replacement is scheduled beside a
selected pod:

```sh
kubectl -n <namespace> delete pod <treebind>-sync-0
```

### ReadWriteOncePod

Only one pod of the cluster can use a `ReadWriteOncePod` claim, so the sync pod and the application cannot both use it,
wherever they run, and Treebind cannot sync such a claim while the application uses it. Use a `ReadWriteOnce` claim.
With `appSelector`, and a selector that names the pod that uses the claim, the sync pod is not scheduled before that pod,
so it does not take the claim first and leave the application `Pending`: it stays `Pending` itself. Without
`appSelector`, whichever pod is scheduled first has the claim, and the other is `Pending`.

## Status values

- **Supported**: The conformance suite has successfully passed on that storage
  class. This is the only condition under which a storage class may be marked
  Supported.
- **Partial**: Some checks passed, but others failed. The row must explicitly
  identify which checks failed.
- **Failed**: The conformance run failed.
- **Not tested**: No valid conformance result is available.

A storage class is never advertised as Supported without a passing conformance
run. Other evidence, benchmarks or manual checks do not qualify.

## Seen by hand

These notes are from runs by hand. They are not conformance results and change no
row of the matrix.

- **NFS mounted with `vers=4.1` failed to list directories.** With the userspace
  NFS-Ganesha server of `registry.k8s.io/sig-storage/nfs-provisioner:v4.0.8` and
  nodes that run Linux 7.0, listings of a directory of a volume failed time and again
  with `Remote I/O error`, on both nodes. Volumes of the same server that were mounted
  with `vers=4.0` or `vers=3` listed every time. If a directory of an NFS volume fails
  to list now and then, try `vers=4.0` in the `mountOptions` of the StorageClass or of
  the PersistentVolume
  ([#303](https://github.com/mohanedmoh/treebind/issues/303)).

## Report a result

Once the suite exists (`make conformance STORAGE_CLASS=<name>`), run it against
a cluster that uses your storage class and open a
[storage conformance report](https://github.com/mohanedmoh/treebind/issues/new?template=conformance_report.yml).
Please remove credentials and private hostnames from the output first.

Community results arrive through that form. Use the date the report issue was
opened as the `Date` value, and link that report issue in the `Report` column so
anyone can check the result. The form has no separate date field; the report
issue is the source of the date.
