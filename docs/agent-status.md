# Reading the sync agent's status

The sync agent holds no Kubernetes credential, so the operator goes to it for the
state of a sync: every 30 seconds it asks the agent of each Treebind for its report
(`GET /status` on port 8080 of the sync pod) with a short-lived token of its own
service account. The decisions behind this are in
[ADR 0011](adr/0011-the-agent-status-endpoint.md),
[ADR 0015](adr/0015-the-operator-reads-the-agents-status.md) and
[ADR 0037](adr/0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md).

Whether the operator can read the report is the condition `AgentUnreachable`. It is
about the operator's view of the agent, not about the sync, so it never changes
`Ready`: the sync can work while the operator cannot read it. What the report says of
the sync is in the conditions `Synced` and `Conflict` and in `status.lastSyncedCommit`
and `status.lastSyncTime`, which [a section of their own](#what-synced-and-conflict-say)
describes.

```
kubectl describe treebind <name>
kubectl get treebind <name> -o jsonpath='{.status.conditions[?(@.type=="AgentUnreachable")]}'
```

## What the condition says

| Status | Reason | Means | What to check |
| :-- | :-- | :-- | :-- |
| `False` | `ReportReceived` | The operator read the report | Nothing |
| `Unknown` | `AwaitingAgentReport` | The operator has not polled yet | Wait a minute |
| `Unknown` | `PollFailing` | The polls have failed for less than 3 minutes | Wait: a pod that has just started, or a rollout, looks like this |
| `Unknown` | `AgentNotRunning` | The name of the sync pod does not resolve | The pod does not exist yet, or has no address: `Ready` and `kubectl describe pod` say why (`SyncPodUnschedulable` for a pod that the scheduler cannot place, as after a drain of the node of a claim of local-path; an unbound claim). It is never `True`, but after 3 minutes `Synced` is `Unknown` if a report was read before |
| `Unknown` | `TokenUnavailable` | The operator could not get a token for the agent | The operator's log. The operator needs permission to create a token of its own service account, which the Role `token-role` in its namespace gives it. With `--status-subject` naming another account, that Role has to name the account too |
| `Unknown` | `KeysUnavailable` | The operator could not put the cluster's public keys where the agent reads them, so the agent refuses its token | The operator's log, and whether a ConfigMap called `<name>-sync-keys` that the operator did not create is in the way |
| `True` | `ConnectionFailed` | The polls failed for 3 minutes: the connection was refused or nothing answered | A network policy or a service mesh must let the operator reach port 8080 of the sync pod. When `Ready` says `SyncPodNodeNotReady`, the node of the sync pod is not ready, and the message says so instead: `Ready` names the node and what to do |
| `True` | `TokenRefused` | The agent answers 401 | The ConfigMap `<name>-sync-keys` may not have reached the pod yet, or may hold other keys |
| `True` | `UnexpectedResponse` | The agent answers with a status that is not a report | The log of the sync pod |
| `True` | `ReportUnreadable` | The answer is too large, is not JSON, has another version or is another Treebind's | The log of the sync pod, and whether the operator and the agent image are of the same release |

A failure is `Unknown` with the reason `PollFailing` and turns `True` only when the
polls have failed for 3 minutes in a row. It goes back to `False` at the first poll
that works. After a failed poll the operator tries again in a minute, not in 30
seconds.

## What `Synced` and `Conflict` say

From the same report the operator writes two more conditions and two fields of the status, in the same write as
`AgentUnreachable` and only when something changed. `kubectl describe treebind <name>` shows them, and `kubectl get treebind`
shows `Ready`, `Synced` and `Conflict`. The decisions behind them are in
[ADR 0037](adr/0037-the-operator-writes-synced-and-conflict-from-the-agents-report-and-ready-follows-only-a-failure-that-waits-for-a-person.md).
Each change of `Synced` and `Conflict` is also an event on the Treebind, and every condition is a metric:
[metrics and events](metrics-and-events.md). [The quickstart](quickstart.md#7-read-the-outcome-in-kubectl-describe) shows them on
a Treebind that syncs, and [an example](examples/dashboards-edited-in-the-application.md) after a conflict that the policy resolved.

```
kubectl get treebind <name> -o jsonpath='{.status.conditions[?(@.type=="Synced")]}'
kubectl get treebind <name> -o jsonpath='{.status.lastSyncedCommit} {.status.lastSyncTime}'
```

`status.lastSyncedCommit` is the full id of the commit of the repository at which the agent last saw the checkout and the
branch agree, after a pull or a push. With `spec.repository.path` it is the repository's commit and not the checkout's
snapshot of it ([the directory of the repository](repository-path.md)). `status.lastSyncTime` is when the agent first saw
them agree at that commit, by its clock and in whole seconds. The two change together, when the commit changes: a pull that
finds nothing new changes neither, so a Treebind with nothing to sync keeps its time. A restart of the sync pod, a step that
fails and a poll that fails clear nothing.

### Synced

| Status | Reason | Means | What to check |
| :-- | :-- | :-- | :-- |
| `True` | `InSync` | No step of the sync has failed, and the agent has seen the checkout and the branch agree since it started, at `status.lastSyncedCommit` | Nothing |
| `False` | The reason of the step that failed, as its check spells it: `FetchFailed`, `Blocked`, `TooManyDeletions`, `PushRejected` and the others of [the checks](#what-the-report-holds) | A step failed. The message says which step, what the reason means, and at which commit the volume and the repository last agreed | The check of the same name in the report and the log of the sync pod, which say which file and what the Git host said. The page of the step says what to do: [the checkout](checkout.md), [pulling](pull.md), [local changes](local-changes.md). For a push that the host refused and for a conflict that was not resolved, [recovering from a conflict or a rejected push](recovering-from-a-conflict-or-a-rejected-push.md) starts from these conditions |
| `False` | `SyncFailed` | A step failed with a reason that this release of the operator does not define, as an agent of another release can report | The log of the sync pod, and whether the operator and the agent image are of the same release |
| `Unknown` | `AwaitingAgentReport` | The operator has not polled yet | Wait a minute |
| `Unknown` | `SyncPending` | No step has failed, and the agent has not seen the volume and the repository agree since it started: the lock, the checkout, the first pull or the first push is still to come, as after every start of the sync pod | Wait: the first pull comes within one interval of the checkout |
| `Unknown` | `AgentOutdated` | The report is of another pod template than the spec gives now, as while the sync pod is replaced after a change of the spec, or of an agent of the release before, which reports none. A change of only the pod affinity (`spec.placement.appSelector`) does not count: it changes nothing that the agent does | Wait for the new pod. If it lasts, `Ready` says whether the sync objects could not be updated (`ReconcileFailed`), and `kubectl describe pod` says why the new pod does not run |
| `Unknown` | `AgentUnreachable` | The operator has not read a report for 3 minutes, whatever the reason, so it does not know what the sync does: `AgentUnreachable` is `True`, or it is `Unknown` because the sync pod does not run (`AgentNotRunning`) or the operator cannot make the call (`TokenUnavailable`, `KeysUnavailable`). For the second, the 3 minutes count only once a report has been read: a Treebind that was never read keeps `AwaitingAgentReport`. The next report puts it back | `AgentUnreachable` says why, and `Ready` says why the sync pod does not run |

- **It follows the steps, not the files.** `True` says that no step failed and that the agent has seen the two agree, and not
  that nothing waits. Files that the application wrote and that have not settled, files that have settled and are not
  committed yet, and commits of the checkout that are not pushed yet are no failure, so they do not make `Synced` `False`.
  `status.lastSyncedCommit` says at which commit the volume and the repository last agreed.
- **The reason is of the first step that failed.** The order is the one of the report. A reason that needs a person comes
  before one that waits for the Git host, so that a host that does not answer does not hide a file that someone has to move.
- **A restart makes it `Unknown`.** The agent keeps what it saw in memory, so after every start of the sync pod `Synced` says
  `SyncPending` until the first pull or push that finds the two in agreement, or `False` if a step fails first. It is `True`
  again after that, with the commit and the time of the status as they were if nothing new came.
- **The messages are fixed.** A message is a sentence for the step and the reason, the first 12 digits of
  `status.lastSyncedCommit`, and where to read more. Nothing that the agent wrote is in it, because everyone who can read the
  Treebind reads it, and the text of a check names files and quotes the Git host.
- **How current it is.** The operator reads the report every 30 seconds, a fifth more or less, every minute after a poll that
  failed, and at once after a change of the spec. A change shows in the status that long after the agent made it, and a change
  from Git also waits for the pull. The conditions carry the generation of the spec they were made for.

### Conflict

| Status | Reason | Means | What to check |
| :-- | :-- | :-- | :-- |
| `True` | The reason of the update that could not be made: `Blocked`, `TooManyDeletions`, `TooManyCommits`, `Refused` or `PullFailed` | The last pull found that your directory and the repository each have commits that the other lacks, and could not resolve it with `spec.sync.conflictPolicy`. Local work waits for a person | The message, the `Pull` and `Conflict` checks of the report, [conflicts](conflicts.md) and [what to do](recovering-from-a-conflict-or-a-rejected-push.md#a-conflict-that-was-not-resolved). A pull that passes without a conflict puts it back to `False` |
| `False` | `ConflictResolved` | The last conflict that the agent found since it started was resolved by the policy, also one by `followRemote`, which keeps your version of a file in a ref that is never pushed | The `Conflict` check names the policy, the counts and where the commits are kept |
| `False` | `NoConflict` | The agent knows of no conflict that waits, and has resolved none since it started. It is also the answer for a `pull` Treebind, which has no conflict to resolve: an update that it cannot take, `Diverged` among them, makes `Synced` `False` | Nothing |
| `Unknown` | As `Synced` | `Conflict` is `Unknown` whenever `Synced` is, for a Treebind that only pulls too, with the same reason and message | See `Synced` |

The agent keeps what it knows of a conflict in memory. After a restart the `Conflict` check is skipped until a conflict has
been resolved, so `Conflict` can say `NoConflict` before a pull has run, while `Synced` is `False` for another step, even though the pull
before the restart could not resolve a conflict. The next pull that finds the conflict again makes it `True`.

### Ready

When the sync pod is ready, `Ready` is `False` with the reason `NotSynced` only when `Synced` is `False` for the current spec
and its reason is one that a person puts right. The message names the reason of `Synced`. For the reasons that the sync puts
right by itself `Ready` stays `True`, while `Synced` still says them, because `Ready` is what `kubectl wait` and the health
checks of a GitOps controller act on, and a Git host that is down should not turn every Treebind that uses it not ready
([ADR 0030](adr/0030-the-agent-is-ready-after-its-first-pull-and-stays-ready-while-it-runs.md)).

| Reason of `Synced` `False` | `Ready` |
| :-- | :-- |
| `FetchFailed`, `PushFailed` | Stays `True`: the Git host did not answer, and the agent asks again with a wait that grows |
| `NonFastForward` and `Blocked`, for a Treebind that syncs in both directions | Stays `True`: the next pull brings the commits that the push lacked, and the next scan commits the edit that was in the way of the update |
| Any other, `SyncFailed` too, and `Blocked` for a Treebind that only pulls | `False` with `NotSynced`: a file, a branch, the volume, a limit of the spec or a rule of the repository has to change |

`Unknown` never makes `Ready` `False`, and neither does a `Synced` of an earlier generation, which is what a change of the spec
leaves in place until the next poll. `Conflict` is not an input: a conflict that cannot be resolved is a failed step, so
`Synced` says its reason, and `Ready` follows that.

### Upgrading the operator

Every sync pod is replaced once when the operator is upgraded to the release that writes these conditions. The pod template
gains the variable `TREEBIND_TEMPLATE_HASH`, the hash of the rest of the template, which the agent reports so that the operator
can tell a report of the current spec from one of the spec before. While a pod is replaced `Ready` says `SyncPodNotReady`, and
`Synced` and `Conflict` turn `Unknown` until the new pod has pulled: with `AgentOutdated` while the operator reads the old pod,
and with `SyncPending` once it reads the new one. After that no pod is replaced for a change that the agent does not see, such as
writing out `mountPath: /work`, which is the default.

A setting of the operator that changes the pod template of every Treebind replaces every sync pod once too, in the same way:
a new agent image, and turning `AGENT_ARBITRARY_USER_ID` (the chart value `agent.arbitraryUserID`) on or off
([any user ID](pod-security.md#any-user-id)).

The operators before this release accepted `spec.agent` and ignored it. From this release on it applies
([the overrides](pod-security.md#the-overrides-of-specagent)), so a Treebind that sets any field of it but `fsGroup` and
`resources.claims` gets a new pod template at the upgrade, and its sync pod is replaced:

- A `runAsUser` that is not 65532 runs the new pod as that user, with the `passwd` init container. On a claim that the agent
  used before as 65532 the agent refuses its state directory with `GitDirUnsafe`
  ([when the user ID changes](pod-security.md#when-the-user-id-changes)). The init container needs an agent image of this
  release or newer, which `helm upgrade --reuse-values` and a pinned `agent.image` or `AGENT_IMAGE` do not give.
- `runAsUser: 0` turns `Ready` `False` with `UnsupportedAgentUser`, and the old pod is left running.
- A label value that a label cannot have is refused by the API server: `Ready` turns `False` with `ReconcileFailed`, and the
  old pod runs on.
- A priority class that does not exist is refused only when the new pod is made: the old pod is gone, no new one is made, and
  the events of the StatefulSet say `FailedCreate`.

Read `spec.agent` of your Treebinds before the upgrade: `kubectl get treebinds -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.spec.agent}{"\n"}{end}'`.

## The cluster has to allow the call

The operator calls the sync pod on port 8080. In a namespace with a default-deny
network policy, or in a service mesh that does not let the call through, that call
fails, and the condition says `ConnectionFailed` after 3 minutes. The sync itself is
not affected, and the logs and metrics of the sync pod keep working. Examples of
policies that allow the operator are tracked in #117.

## See what the operator sees

The endpoint answers only a token of the operator's service account whose audience is
the Treebind, so a plain request gets 401. To read the report by hand, ask for such a
token and call the pod from a pod in the cluster:

```
TOKEN=$(kubectl -n <operator namespace> create token <operator service account> \
  --audience=<API group of the CRD>/<namespace>/<name> --duration=10m)
# from any pod in the cluster that has curl or wget:
curl -H "Authorization: Bearer $TOKEN" \
  http://<name>-sync-0.<name>-sync.<namespace>.svc:8080/status
```

The agent's probes need no token: `/healthz` and `/readyz` on the same port, which [the next section](#the-probes)
describes.

## The probes

The kubelet asks the sync pod two questions on port 8080 every 10 seconds, with a timeout of 2 seconds, and a probe that
fails three times in a row counts as failed. Neither needs a token and neither says more than its word.

| Probe | Path | Answers 200 | Answers 503 |
| :-- | :-- | :-- | :-- |
| Readiness | `/readyz` | A pull has passed since the agent started, or, for a Treebind that syncs in both directions, a push has worked | Neither has happened yet |
| Liveness | `/healthz` | The loop of the agent has been heard from within what it promised | It has not |

**Ready means the agent has synced.** The first pull that ends `Passed` makes it ready: the checkout was at the tip of the
branch (`UpToDate`), was brought to it (`Advanced`), has commits that the repository does not have yet (`Ahead`), or had a conflict with it that the policy resolved (`Resolved`). For a
Treebind that syncs in both directions the first push that works makes it ready as well, whichever of the two comes first,
so a Treebind on an empty repository is ready as soon as its first commit is pushed, and does not wait for a second pull. A
push that is rejected, that fails or that finds nothing to push does not make it ready. The `Pull` and `Push` checks in the
report say `Passed` before the pod is ready, and the agent logs one line when it turns ready. The first pull comes up to one
interval after the checkout ([pulling](pull.md)), and the kubelet sees the agent ready at its next probe, up to 10 seconds
after that. The operator turns the `Ready` condition of the Treebind on when the sync pod is ready, so
`kubectl wait --for=condition=Ready treebind/<name>` waits for the first pull or the first push.
[ADR 0033](adr/0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md) has the reasons.

**It stays ready while the agent runs.** A pull that fails later (the repository does not answer, a file is in the way,
the branches diverged, the update would remove too many files) does not make the pod not ready, because the files on the
volume are as they were, and a Git host that is down for an hour should not turn every Treebind that uses it
not ready. The `Pull` check says what the last pull found, and `Synced` says it in the Treebind: it turns `Ready` `False` only
for a reason that a person puts right ([below](#ready)). A container that restarts starts not ready, because nothing in its
memory says that the one before it synced, and is ready again after its first pull.

**Alive means the loop is not stuck.** The loop of the agent beats, and each beat says how long it is until the next:
the length of the wait before a pull, a scan or a retry; the timeout of a Git command while one runs; and 5 minutes for the
code between them. `/healthz` answers 503 when the loop has been silent for longer than it promised and a grace of 30
seconds, and the kubelet restarts the container, 20 to 30 seconds later. A long interval, a long wait after failed pulls, a
fetch of a large repository, and a wait for the writer lock that another agent holds are all promised, so none of them is
taken for a stuck loop. A pull that fails, or a volume check that fails, is not either: the loop goes on, and the report
says what failed. An agent that has nothing more to do (no lock, checkout or pulls to run) is alive for good. The log
says `the loop of the agent did not beat within what it promised`, with how late it is, at most once a minute.
[ADR 0030](adr/0030-the-agent-is-ready-after-its-first-pull-and-stays-ready-while-it-runs.md) has the reasons.

| What you see | What it means | What to check |
| :-- | :-- | :-- |
| The pod is `0/1`, and `Ready` is `False` with `SyncPodNotReady`, for less than one interval after it started | The first pull is not due yet | Wait |
| The same, for longer | No pull has passed | The `Pull` check: its reason says what is in the way, and [pulling](pull.md) says what to do |
| The same, with `BranchGone` | The repository has no such branch, or no commit at all yet | Put a commit on the branch, or set `spec.repository.branch` to one that exists. A Treebind that syncs in both directions turns ready by itself when its first commit is pushed |
| The same, and the `Pull` check is `Skipped` with `NotChecked` | The volume, the lock or the checkout has not passed | The check that failed, in the same report |
| There is no sync pod, and `Ready` is `False` with `UnsupportedRepositoryScheme` | `spec.repository.url` has the scheme `http://`, which the agent refuses, so the operator creates no sync pod. A sync pod that the Treebind had is left as it was, and `Synced` says `AgentOutdated` for it | Use an `ssh://` or `https://` address: [the checkout](checkout.md#things-to-know) says why |
| There is no new sync pod, and `Ready` is `False` with `UnsupportedAgentUser` | `spec.agent.runAsUser` is `0`, and the sync pod does not run as root. The operator creates or changes no sync object, and a sync pod that the Treebind had is left as it was and runs on: `Synced` and `Conflict` turn `Unknown` with `AgentOutdated`, since its reports are of the template before | Set another user, or leave `runAsUser` out ([the overrides of the sync pod](pod-security.md#the-overrides-of-specagent)) |
| `Ready` is `False` with `ReconcileFailed`, and the message holds `spec.template` | The API server refused the StatefulSet for what `spec.agent` gives the pod, such as a label value that a label cannot have, or a quantity that it does not take. The message gives its words | The field that the message names, under `spec.agent` |
| `Ready` is `False` with `ReconcileFailed`, and the message holds `spec.placement.appSelector` | The selector is one that Kubernetes does not take, such as one with an unknown operator, `In` with no value or a label value with a space. No sync pod is made or changed | The selector, as the message says ([where the sync pod runs](storage-support.md#access-modes-and-where-the-sync-pod-runs)) |
| `Ready` is `False` with `ReconcileFailed`, and the message holds `PersistentVolumeClaim` | The operator could not read the claim of a Treebind with `spec.placement.appSelector`: it may not (`forbidden`), or no sync pod was made yet and the read failed. A sync pod that there is keeps its affinity | The `get` on `persistentvolumeclaims` in the role of the operator ([permissions](rbac.md#what-it-may-do)), and what else the message says |
| The pod is not there, `Ready` is `False` with `SyncPodNotReady`, and `kubectl describe statefulset <name>-sync` shows `FailedCreate` | The StatefulSet was taken, but its pod was refused when it was made: a priority class that does not exist, a quota, or the pod security admission of the namespace | What the event says. A priority class has to exist before the pod is made |
| The pod is `1/1`, and `Ready` is `False` with `NotSynced` | A step of the sync failed for a reason that a person puts right | `Synced`: its reason and message, then the check of the same name in the report ([`Synced` and `Ready`](#ready)) |
| The container restarts, and `kubectl describe pod` shows `Liveness probe failed: HTTP probe failed with statuscode: 503` | The loop was stuck: a step took longer than it promised, most often a call on a volume that does not answer | The log of the container before the restart (`kubectl logs --previous`), for the line about the loop and how late it was, and the volume |
| The container restarts, and `Ready` is `False` with `SyncPodOOMKilled` | The container was killed for using more memory than its limit, which the message names. Git needed it, for a large file, and every restart is killed again on the same pull | [Large files](#large-files) |
| The pod is `Pending`, and `Ready` is `False` with `SyncPodUnschedulable` | The scheduler cannot place the sync pod, and the message gives its words. After a drain of the node of a claim of local-path they say `volume node affinity conflict` and `were unschedulable`: only that node can mount the volume, and it is cordoned. Nothing is synced while the pod waits: `AgentUnreachable` says `AgentNotRunning`, and after 3 minutes `Synced` is `Unknown` with `AgentUnreachable` | What the message says no node has. For a drained node, `kubectl uncordon <node>` once it may run pods again |
| The same, and the message says that the pod may only run on the node of a pod that `spec.placement.appSelector` selects | The sync pod has a required pod affinity to the application ([where the sync pod runs](storage-support.md#access-modes-and-where-the-sync-pod-runs)). The scheduler's words follow: `didn't match pod affinity rules` for nodes with no selected pod, and `persistentvolumeclaim ... not found` for a claim that is not there | That a pod that `appSelector` selects runs in the namespace (`kubectl get pods -l <selector> -o wide`), and that its node takes the sync pod: room, taints and `spec.agent.nodeSelector` |
| The application's pod stays in `ContainerCreating` with `Multi-Attach error` after it moved to another node | The sync pod stays on the node it was scheduled to, with the volume mounted, and does not follow ([#113](https://github.com/mohanedmoh/treebind/issues/113)) | Delete the sync pod (`kubectl delete pod <name>-sync-0`): with `appSelector`, its replacement goes to the application's node |
| The pod is `Running`, or `Terminating`, on a node that is lost, and `Ready` is `False` with `SyncPodNodeNotReady` | The node of the sync pod is not ready, and the message names the node and the pod. The pod cannot end there, so it is not replaced. After 3 minutes `AgentUnreachable` is `True` with `ConnectionFailed`, and `Synced` is `Unknown` with `AgentUnreachable` | Bring the node back. If it is shut down, give it the taint `node.kubernetes.io/out-of-service` with the effect `NoExecute`: Kubernetes deletes the pod and makes a new one, which a claim that only that node can mount keeps `Pending` (`SyncPodUnschedulable`). Take the taint away once the node is back |

While the pod is not ready, `kubectl describe pod` shows the event `Readiness probe failed: HTTP probe failed with
statuscode: 503`. That is the probe doing its work and not a fault of its own. A repository that has no commit yet says
`BranchGone` with the words "has no branch ... any more", which is also what a branch that was deleted says.

## Large files

The sync container has a memory limit of 128 MiB unless `spec.agent.resources.limits.memory` sets another
([the overrides of the sync pod](pod-security.md#the-overrides-of-specagent)). Git streams a file larger than 16 MiB between the Git host and the
disk instead of holding it in memory, so a file that is new to the branch needs no more memory for being large: one of 200 MB
syncs with the default limit. A file larger than 16 MiB is not stored as a delta, so every change to it is sent whole on a
push. A change of a large file that a pull brings can need more memory: when the Git host sends the new version as a delta of
the old one, Git builds it in memory from the old one, with about four times the size of the file in all, so with the default limit
such a change works for a file of up to about 25 MB. When Git needs more than the limit, the container is killed, every restart
is killed again on the same pull, and `Ready` is `False` with the reason `SyncPodOOMKilled` and a message that names the limit.
While no sync pod is ready the operator looks at the pod about every 30 seconds, so `Ready` says it within about half a minute
of the kill, and it names a kill that ended at most 10 minutes before for a container that runs again and is not ready. A later
commit that removes the file does not help, because a pull fetches every commit of the branch.

## How the keys reach the agent

The agent checks the operator's token against the cluster's public signing keys. The
operator reads them from the API server and writes them to the ConfigMap
`<name>-sync-keys`, which the Treebind owns and which the sync pod mounts. It writes
the keys again when they change and every 10 minutes. When an agent refuses the
token, the operator reads the keys again at once, because that is what a rotation of
the cluster's keys looks like before the new keys have reached the pod. The kubelet
takes about a minute to show a changed ConfigMap in a running pod, so after a
rotation `PollFailing` for a short time is normal.

The operator may create and patch ConfigMaps in every namespace for this, and nothing
else: it reads no ConfigMap and changes only one that the Treebind owns. A ConfigMap
of the name `<name>-sync-keys` that someone else made is left alone, and the
condition says `KeysUnavailable` when that is why the agent refuses the token.

## What the report holds

The report lists the agent's checks, in the order the agent runs them, each with an outcome
(`Passed`, `Failed` or `Skipped`), a reason when it did not pass, and a message:

| Check | What it is | More |
| :-- | :-- | :-- |
| `Mount` | The claim is a mounted volume | [Pod security](pod-security.md) |
| `Writable` | The agent can write to it | [Pod security](pod-security.md) |
| `GitDir` | The directories where the agent keeps its Git directory are its own and private | [The Git directory](git-directory.md) |
| `Lock` | The agent holds the writer lock of the volume, so that no other agent writes to it. It waits while another holds it | [ADR 0023](adr/0023-the-agent-holds-the-writer-lock-from-before-the-checkout.md) |
| `Checkout` | The checkout of the repository, or of the directory of it that `spec.repository.path` names, is made, or the one that is there is opened | [The checkout](checkout.md), [the directory of the repository](repository-path.md) |
| `Pull` | The last pull of the commits that the repository gained: at the tip, advanced, or why it did not work, for example `TooManyDeletions`. It is skipped until the first pull | [Pulling](pull.md), [the deletion guard](deletion-guard.md) |
| `Changes` | The last scan of your directory for what the application changed: how many files, how many have settled, what is left out, and what the events of the file system cannot do. Only for a Treebind that syncs in both directions. It is skipped until the first scan | [Local changes](local-changes.md) |
| `Commit` | The last commit of the files that the scans found settled: how many and the short id, that there was nothing to commit, or why it failed (`CommitFailed`, or `TooManyCommits` when the limit of commits in an hour is reached). Only for a Treebind that syncs in both directions. It is skipped until a file settles, and while an update that was cut off waits for a pull to put it right | [Local changes](local-changes.md), [echo loops](echo-loops.md) |
| `Push` | The last push of the commits of the checkout: how many and the two ids, that there is nothing to push, or why it failed (`NonFastForward`, `PushRejected` with the words of the host, `PushFailed`). Only for a Treebind that syncs in both directions. It is skipped until the first scan | [Local changes](local-changes.md) |
| `Conflict` | What the last resolution of a conflict between your directory and the repository did: the policy, how many commits each side had, how many files had the winner's version, how many were kept from the other side, the commit that was made and, for `followRemote`, the ref that keeps the commits of the directory. It is `Passed` with the reason `ConflictResolved`, and stays so until the next resolution. Only for a Treebind that syncs in both directions. It is skipped, saying that no conflict has been resolved since the agent started, until a conflict has been resolved. A resolution that could not be made makes it `Failed`, with the reason of the update (`Blocked`, `TooManyDeletions`, `TooManyCommits`, `Refused`, `PullFailed`), and the `Pull` check fails with the same reason and a message that says that the conflict was not resolved. The next pull that passes without a conflict puts the check back to what it said before: the last resolution, or `Skipped`. It does not change whether the agent is ready, and the operator derives the `Conflict` condition from it | [Conflicts](conflicts.md) |

A check that cannot be tried because one before it failed, or that waits, is `Skipped`, with the reason
`NotChecked` and the check that failed in the message. The operator shows what the checks say of the
sync as the conditions `Synced` and `Conflict` ([above](#what-synced-and-conflict-say)). The checks themselves,
with their messages, which name files and quote the Git host, are in the report and in the agent's log.
