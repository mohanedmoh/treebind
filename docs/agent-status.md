# Reading the sync agent's status

The sync agent holds no Kubernetes credential, so the operator goes to it for the
state of a sync: every 30 seconds it asks the agent of each Treebind for its report
(`GET /status` on port 8080 of the sync pod) with a short-lived token of its own
service account. The decisions behind this are in
[ADR 0011](adr/0011-the-agent-status-endpoint.md) and
[ADR 0015](adr/0015-the-operator-reads-the-agents-status.md).

Whether the operator can read the report is the condition `AgentUnreachable`. It is
about the operator's view of the agent, not about the sync, so it never changes
`Ready`: the sync can work while the operator cannot read it.

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
| `Unknown` | `AgentNotRunning` | The name of the sync pod does not resolve | The pod does not exist yet: `Ready` and `kubectl describe pod` say why (an unbound claim, an image that cannot be pulled) |
| `Unknown` | `TokenUnavailable` | The operator could not get a token for the agent | The operator's log. The operator needs permission to create a token of its own service account, which the Role `token-role` in its namespace gives it. With `--status-subject` naming another account, that Role has to name the account too |
| `Unknown` | `KeysUnavailable` | The operator could not put the cluster's public keys where the agent reads them, so the agent refuses its token | The operator's log, and whether a ConfigMap called `<name>-sync-keys` that the operator did not create is in the way |
| `True` | `ConnectionFailed` | The polls failed for 3 minutes: the connection was refused or nothing answered | A network policy or a service mesh must let the operator reach port 8080 of the sync pod |
| `True` | `TokenRefused` | The agent answers 401 | The ConfigMap `<name>-sync-keys` may not have reached the pod yet, or may hold other keys |
| `True` | `UnexpectedResponse` | The agent answers with a status that is not a report | The log of the sync pod |
| `True` | `ReportUnreadable` | The answer is too large, is not JSON, has another version or is another Treebind's | The log of the sync pod, and whether the operator and the agent image are of the same release |

A failure is `Unknown` with the reason `PollFailing` and turns `True` only when the
polls have failed for 3 minutes in a row. It goes back to `False` at the first poll
that works. After a failed poll the operator tries again in a minute, not in 30
seconds.

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
branch (`UpToDate`), was brought to it (`Advanced`), or has commits that the repository does not have yet (`Ahead`). For a
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
not ready. The `Pull` check says what the last pull found. A container that restarts starts not ready, because nothing in its
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
| The container restarts, and `kubectl describe pod` shows `Liveness probe failed: HTTP probe failed with statuscode: 503` | The loop was stuck: a step took longer than it promised, most often a call on a volume that does not answer | The log of the container before the restart (`kubectl logs --previous`), for the line about the loop and how late it was, and the volume |

While the pod is not ready, `kubectl describe pod` shows the event `Readiness probe failed: HTTP probe failed with
statuscode: 503`. That is the probe doing its work and not a fault of its own. A repository that has no commit yet says
`BranchGone` with the words "has no branch ... any more", which is also what a branch that was deleted says.

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
| `Checkout` | The checkout of the repository is made, or the one that is there is opened | [The checkout](checkout.md) |
| `Pull` | The last pull of the commits that the repository gained: at the tip, advanced, or why it did not work, for example `TooManyDeletions`. It is skipped until the first pull | [Pulling](pull.md), [the deletion guard](deletion-guard.md) |
| `Changes` | The last scan of your directory for what the application changed: how many files, how many have settled, what is left out, and what the events of the file system cannot do. Only for a Treebind that syncs in both directions. It is skipped until the first scan | [Local changes](local-changes.md) |
| `Commit` | The last commit of the files that the scans found settled: how many and the short id, that there was nothing to commit, or why it failed (`CommitFailed`). Only for a Treebind that syncs in both directions. It is skipped until a file settles | [Local changes](local-changes.md) |
| `Push` | The last push of the commits of the checkout: how many and the two ids, that there is nothing to push, or why it failed (`NonFastForward`, `PushRejected` with the words of the host, `PushFailed`). Only for a Treebind that syncs in both directions. It is skipped until the first scan | [Local changes](local-changes.md) |

A check that cannot be tried because one before it failed, or that waits, is `Skipped`, with the reason
`NotChecked` and the check that failed in the message. The operator does not show the checks as
conditions yet: they are in the report and in the agent's log.
