# What the operator may do

The operator's permissions are the calls its code makes, and no more. This page lists them, says what it can
never do, and says how to install it so that it works in the namespaces you choose and not in all of them.
[ADR 0032](adr/0032-the-operator-may-do-what-it-does-and-can-be-limited-to-namespaces.md) has the reasons.

## What it may do

| Resource | Verbs | Why |
| :-- | :-- | :-- |
| `treebinds` | get, list, watch | It reads your Treebinds. It never creates, changes or deletes one |
| `treebinds/status` | update | It writes the conditions of the Treebind |
| `treebinds/finalizers` | update | What it creates names the Treebind as its owner, and that needs this on clusters that enforce owner references |
| `statefulsets`, `services` | get, list, watch, create, update | The one-replica StatefulSet and the headless Service of each Treebind |
| `configmaps` | create, patch | The public keys that the sync agent checks the operator's token against ([ADR 0015](adr/0015-the-operator-reads-the-agents-status.md)). It never reads one |
| `events` of `events.k8s.io` | create, patch | An event on the Treebind for each change of `Synced` and `Conflict` ([metrics and events](metrics-and-events.md)). It reads none |
| `pods` | get | The sync pod of a Treebind, while its StatefulSet has no ready pod, to say in `Ready` whether its container was killed for memory ([large files](agent-status.md#large-files)). The role cannot name the sync pods, because their names are made from the names of the Treebinds, so it may get any pod where the role applies: in every namespace with the ClusterRole of a cluster-wide install, and only in the namespaces of `rbac.namespaces` (the release namespace when it is empty) with the Roles of `rbac.namespaced=true`. It lists and watches no pod |
| `leases` (its own namespace) | get, create, update | Leader election ([operator replicas](operator-replicas.md)) |
| `events` of the core group (its own namespace) | create, patch | The events that leader election records |
| `serviceaccounts/token` (its own namespace) | create, for its own account only | The token that it calls the sync agents with |

It deletes nothing. The StatefulSet, the Service and the ConfigMap of a Treebind carry it as their owner, and the
garbage collector removes them when you delete the Treebind. Your claim and its data are not owned by anything
the operator makes, and stay (see [deleting a Treebind](deleting-a-treebind.md)).

## What it can never do

- **Read a Secret.** No role of the operator reaches Secrets, by name or through a wildcard. The SSH key of a
  repository is mounted into the sync pod by the pod spec, and the operator never sees it.
- **Write a claim or a volume.** The most that any role gives on them is a read.
- **Mint a token for any account but its own.**

Tests hold each of these on every role that the repository ships, in the kustomize base and in the chart, and
hold each list above exactly, so a verb cannot be added by accident.

## Install in some namespaces only

By default the operator serves the Treebinds of every namespace, and its role is a ClusterRole. With the chart
you can limit it to the namespaces you name:

```sh
helm install treebind charts/treebind --namespace treebind-system --create-namespace \
  --set rbac.namespaced=true \
  --set 'rbac.namespaces={apps,tools}'
```

The namespaces have to exist before you install: the chart makes a Role in each, and Helm stops with the error of
the API server for one that is not there. The chart then makes a Role and a RoleBinding in each of those
namespaces, and starts the operator with `--watch-namespaces=apps,tools`, which makes it read only there. Both come from the same list, so the operator
cannot watch a namespace that it has no Role in. With `rbac.namespaced=true` and no list, it is the namespace of
the release.

The chart checks the list and fails the install, with a message that names the entry, for a list without
`rbac.namespaced=true`, because the operator would otherwise get a ClusterRole where you expected Roles; for an
entry that is not the name of a namespace (lowercase letters, digits and dashes, at most 63 characters, and no
blanks); for an empty entry; and for a namespace that comes twice. The operator checks the names too, and stops
at its start for one that is not a name, so a list that passes the install is a list that it takes.

The chart passes `--watch-namespaces` after the arguments of `manager.args`, and a flag that comes twice takes
the last, so the namespaces of the Roles win over a `--watch-namespaces` of your own. In a cluster-wide install
the chart passes no such flag, and one in `manager.args` is used as it is.

What this costs, and what stays cluster-wide:

- **A Treebind in another namespace is not looked at.** The operator never sees it, so its status stays empty and
  no condition says why. If a Treebind does nothing, check that its namespace is in the list.
- **Adding a namespace is an upgrade of the release.** It makes a new Role, and it restarts the operator, because
  its arguments change.
- **The person who installs the chart needs the right to create Roles in each of the namespaces.**
- **The CRD is cluster-scoped.** So are the two APIs that the authentication of the metrics endpoint calls
  (TokenReview and SubjectAccessReview). The ClusterRole for those stays while `metrics.secure` is on, and it
  reaches nothing else.
- **The helper roles** for people (`rbac.helpers.enabled`) follow `rbac.namespaced` in the release namespace only.

The kustomize base is cluster-wide, and so is `make deploy`. It has the narrowed verbs of the table above, and no
namespaced variant yet.

## Read volumes and storage classes (optional)

A read-only ClusterRole on `persistentvolumes` and on `storageclasses`, and nothing else. Nothing reads these
objects yet. The volume details of the status of a Treebind will (#98), and say the claim alone when this role is
not there. It is off by default:

```sh
# the chart
helm upgrade treebind charts/treebind --namespace treebind-system --reuse-values --set rbac.volumeRead.enabled=true
```

For kustomize, uncomment the two lines under `[VOLUME READ]` in `config/default/kustomization.yaml`, which add
`config/components/volume-read`. It is a ClusterRole in both, whatever `rbac.namespaced` says, because volumes and
storage classes have no namespace.

## Checking what it can do

```sh
OPERATOR=system:serviceaccount:treebind-system:treebind-controller-manager
kubectl auth can-i list treebinds --as=$OPERATOR -n apps      # yes, if apps is served
kubectl auth can-i list treebinds --as=$OPERATOR -n other     # no, in a namespaced install
kubectl auth can-i get secrets --as=$OPERATOR -n apps         # no, always
kubectl auth can-i create events.events.k8s.io --as=$OPERATOR -n apps   # yes, if apps is served
```

The log of the operator says which namespaces it serves, in one line at its start: `Serving only the Treebinds of
these namespaces`. With no such line, it serves all of them.

## When it says forbidden

An error of the cache in the log of the operator, such as `treebinds.treebind.example.org is forbidden: User
"system:serviceaccount:..." cannot list resource "treebinds" in API group "treebind.example.org" at the cluster
scope`, means that the operator reads at cluster scope with only Roles. It happens when the roles of a namespaced
install are applied without `--watch-namespaces`, for example in a hand-made manifest. The probes of the pod do
not look at the cache, so the pod stays `Running` and ready while its cache lists nothing: look at the log and
not only at the pod. A namespace that the flag names
and that has no Role gives the same error for that namespace.
