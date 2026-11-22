# 0032. The operator may do what it does, and can be limited to namespaces

- Status: accepted
- Date: 2026-11-18
- Issue: #30
- Extended by: [0038](0038-the-operator-records-an-event-and-a-metric-where-it-writes-a-condition-of-the-sync.md), which adds `create` and `patch`
  on `events` of `events.k8s.io` to the manager role, for the events of the sync outcome

## Context

NFR-5 asks that the operator's RBAC is least-privilege and namespaced where possible, and R-16 names the risk:
a role broader than the operator needs, for example a cluster-wide read of Secrets. The operator never reads a
Secret (NFR-21, ADR 0015), and its role for ConfigMaps was already as narrow as Kubernetes allows. What was left
was two things. The manager role had come from the scaffold and granted verbs that the code never uses. And the
chart's `rbac.namespaced` turned the ClusterRole into a Role and a RoleBinding in the release namespace, which
the operator could not work with: its manager was built with no limit on its cache, so it listed Treebinds and
StatefulSets at cluster scope, which a Role in one namespace does not allow.

That second claim was run, and not only read. On a kind cluster, with Roles in two namespaces and no
`--watch-namespaces`, both operator pods stayed `Running` and ready for the minute that they were watched, while
the log filled with `statefulsets.apps is forbidden: ... at the cluster scope` and the same for Treebinds: the
cache could list nothing. The probes of the operator do not look at the cache, so the pod gives no sign.

## Decision

### The roles are the calls the operator makes

Every call that the operator makes to the API server was listed, and each role holds exactly that:

| Resource | Verbs |
| :-- | :-- |
| `treebinds` | get, list, watch |
| `treebinds/status`, `treebinds/finalizers` | update |
| `statefulsets`, `services` | get, list, watch, create, update |
| `configmaps` | create, patch |
| `leases` | get, create, update |
| `events` | create, patch |

Gone from the manager role: create, update, patch and delete on Treebinds, delete and patch on StatefulSets and
Services, and get and patch on the status. The operator deletes nothing, because the garbage collector removes
what a deleted Treebind owns; that was run too, on a kind cluster, with the verbs gone. Gone from the
leader-election Role: the ConfigMap verbs that a Lease lock does not use. Two replicas still elected a leader.

Two tests keep it so. One holds each list exactly, in the kustomize base and in the chart. The other walks every
role of both, and the optional one below, and fails on any rule that reaches Secrets by name or by a wildcard of
a group, a resource or a verb.

### The operator can be limited to namespaces

A flag of the operator, `--watch-namespaces=a,b`, limits the cache of the manager to those namespaces, so that it
never asks the API server for anything outside them. Empty means every namespace, as before. The list is read
strictly: a name that is not the name of a namespace, and an empty name between commas, stop the operator at
start.

The chart's `rbac.namespaced` now works with it. A new `rbac.namespaces` lists the namespaces, the release
namespace when it is empty, which is what the value always said. For each of them the chart makes a Role and a
RoleBinding, and the operator is started with the same list from one helper (`treebind.watchNamespaces`), so the
two cannot differ. The helper is the one place that checks the list, and every template reads it through the
helper. It fails the install, naming the entry, for a list without `rbac.namespaced`, which would otherwise leave
a ClusterRole where the person who wrote the list expects Roles, which is the unsafe way to be wrong; for an entry
that is not the name of a namespace, with the rule that the operator uses; for an empty entry; and for a
namespace that comes twice. The chart is the stricter of the two: it takes no blank around a name and no
duplicate, which the operator forgives, and it takes nothing that the operator refuses.

The chart puts its `--watch-namespaces` after `manager.args`. A flag that comes twice takes the last value, so the
namespaces of the Roles win over one that a user put in `manager.args`, which would otherwise make the operator
watch other namespaces than the Roles are for. In a cluster-wide install the chart passes none, and the flag of
the user is used. Nothing changes in the Treebind API.

What it costs:

- A Treebind in a namespace that is not listed is not looked at, and nothing in its status says so, because the
  operator never sees it. This is run and is in the documentation.
- Adding a namespace is a chart upgrade, and restarts the operator, because its arguments change.
- The person who installs the chart needs the right to create Roles in each namespace.
- The CRD, and the two APIs that the authentication of the metrics endpoint calls (TokenReview and
  SubjectAccessReview), are cluster-scoped and stay a ClusterRole, which reaches nothing else.
- The kustomize base stays cluster-wide. Kustomize cannot loop over namespaces, and #51 is where the base is
  reworked.

### An optional read-only ClusterRole on volumes and storage classes

`get`, `list` and `watch` on `persistentvolumes` and `storageclasses`, and nothing else, off by default
(`rbac.volumeRead.enabled` in the chart, `config/components/volume-read` for kustomize). Nothing reads them yet:
the volume details of the status (#98) will, and fall back to the claim alone without it. The role is added
now so that #98 adds only the reading. It is a ClusterRole in a namespaced install too, because volumes and
storage classes have no namespace. Reading claims in a namespace is a rule of the namespaced role, and comes with
#98.

The component's names carry the prefix of `config/default` themselves: kustomize adds a component's resources
after it has applied the overlay's name prefix, and a name without one stayed without (tried).

## Consequences

- The operator can do less than it could, and a change that needs a new verb has to name it in the table, in a
  test that fails until it does. That is the point, and it is also a step that a feature has to take.
- A namespaced install is possible with the chart. Its cost is the list above, and the documentation says so.
- Helm itself was not run when this was written: the download of its binary was refused where the change was made.
  The templates are held by tests that run them with the few functions that they use, and that parse every
  template of the chart, and the objects that they make were applied to a cluster. A job of CI, `chart`, installs
  the Helm that `release.yml` pins, with the same version and checksum, and runs `helm lint --strict`, `helm
  template` for the values of the tests, and tests (`chart_helm_test.go`) that hold the objects and the refusals
  of real Helm to the ones of the renderer. The job and those tests were added without having been run, so their
  first run, on the pull request, is also their first test. The pin is in two files and has to be changed in both.
- The operator's own `Role`s for leader election and for tokens are in the release namespace, so a namespaced
  install adds no cluster-wide permission to them.
- Not decided here: reading claims, volumes and storage classes into the status (#98), a namespaced kustomize
  overlay (#51), and a way to tell a Treebind that no operator serves from one that is waiting.
