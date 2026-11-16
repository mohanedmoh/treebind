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

The agent's probes need no token: `/healthz` and `/readyz` on the same port.

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
| `Pull` | The last pull of the commits that the repository gained: at the tip, advanced, or why it did not work. It is skipped until the first pull | [Pulling](pull.md) |

A check that cannot be tried because one before it failed, or that waits, is `Skipped`, with the reason
`NotChecked` and the check that failed in the message. The operator does not show the checks as
conditions yet: they are in the report and in the agent's log.
