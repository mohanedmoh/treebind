# 0015. The operator reads the agent's status

- Status: accepted
- Date: 2026-11-13
- Issue: #99

## Context

The agent holds no Kubernetes credential, so the operator goes to it for the
state of a sync (ADR 0004). ADR 0011 settled what the agent checks: a
short-lived token of the operator's own service account, with the Treebind as
audience, verified against the cluster's public keys in a file that the sync
pod mounts from a ConfigMap. It left to the operator everything that makes the
exchange happen: where the ConfigMap comes from, how the operator gets a token,
how often it asks, what it says when it cannot get an answer, and how an old
answer counts. The operator may fail to reach the agent for reasons that are not
a failed sync: a default-deny network policy, or a service mesh that does not let
the call through (R-44). Those must not look like sync errors.

Two limits shape the answer. The operator never reads a Secret (NFR-21), and the
permissions it gains must be as narrow as Kubernetes allows. And a headless
Service publishes the name of a pod only once the pod is ready, while the agent is
not ready until it has synced.

## Decision

### The poller is a second controller

The poller is a controller of its own in the operator's manager, not a requeue in
the reconcile of the spec and not a separate runnable. Like the reconcile it runs
only on the leader (ADR 0014). It has its own queue, retry and number of workers,
so an agent that does not answer cannot hold up the reconcile of a spec, and a
Treebind that is deleted drops out by itself.

- Each Treebind is polled every 30 seconds, and every minute while the last poll
  failed, with 20 % jitter so that Treebinds created together do not come
  together. At most 8 polls run at once.
- A poll is one `GET /status` to
  `http://<name>-sync-0.<name>-sync.<namespace>.svc:8080`. It has a timeout of 5
  seconds, uses no proxy and follows no redirect. An answer of more than 256 KiB
  is refused, and so is a report with a `version` other than 1, or one that names
  another Treebind.
- Status is written only when a condition changed, so polling does not write
  every 30 seconds.
- A message is a fixed sentence for its cause. No token, no text of a network
  error and nothing from the agent's answer goes into a message or a log.

### The name of the sync pod resolves while it is not ready

The headless Service sets `publishNotReadyAddresses`. On a kind cluster the name
of a pod that is not ready gave NXDOMAIN behind the default headless Service and
resolved once the field was set. The agent is not ready before its first sync
(ADR 0011), so without the field the operator could not reach an agent that is up
and has nothing to sync yet. The Service has no port and nothing else uses it.
This is a change to the Service and not to the pod template, so no sync pod is
replaced.

### The operator publishes the keys

The operator reads `/openid/v1/jwks`, which every service account may read, at
most once a minute for all Treebinds, and writes the document to the ConfigMap
`<name>-sync-keys` under the key `jwks.json`, which the pod template already
mounts.

- **What is published.** Only a document that the agent's own parser accepts, so
  that a page that is not a key set never replaces keys that work. When a read
  fails, the last good copy is used for an hour, which is how long the agents
  trust keys they cannot refresh themselves (ADR 0011), and the error after that.
- **When.** The reconcile creates the ConfigMap before the StatefulSet. On a kind
  cluster a ConfigMap that existed before the pod was in the pod as soon as it
  ran, whereas one that appears after the pod started is shown by the kubelet
  only after a delay, which was 58 seconds when it was measured. The Treebind is
  its controller, so deleting the Treebind deletes it. It is written again when the keys change, and every 10 minutes
  regardless, which puts back a ConfigMap that someone edited or deleted. The
  reconcile of a Treebind comes back about every 5 minutes for that, with jitter,
  because nothing in the cluster says that the keys changed.
- **The permission** is `create` and `patch` on ConfigMaps in every namespace, and
  nothing else: no get, list, watch, update or delete. The operator reads no
  ConfigMap, and the garbage collector removes the ones it made. It cannot be
  narrower by name, because `create` cannot be limited by `resourceNames` and the
  names follow the Treebinds.
- **Whose ConfigMap.** The operator sends a JSON patch that starts with a test of
  the owner's UID, which the API server checks in the same step as the change, and
  creates the ConfigMap when the patch finds none. The patch comes first because
  it is the call of every later write, and of every Treebind at once after a
  restart or a change of leader. On a kind cluster, with a
  service account that held only `create` and `patch`, a ConfigMap owned by the
  Treebind was updated, and one with no owner or another owner was refused with
  422 and kept its data. So the operator does not rewrite a ConfigMap of someone
  else that happens to be called `<name>-sync-keys`, and needs no read to know it.
  The test is on the first owner reference, so a ConfigMap that lists another owner
  before the Treebind is refused as well.
- **A failure changes neither `Ready` nor the sync objects.** The Service and the
  StatefulSet are still created when the keys cannot be read or written, and
  `Ready` is computed as ever, because the sync does not need the keys, only the
  operator's reading of the agent does, and `Ready` is about the sync. The failure
  is logged and the Treebind is looked at again after about a minute. The
  poller, which puts the keys where the agent reads them as part of each poll, shows
  it in `AgentUnreachable` with the reason `KeysUnavailable`. `ReconcileFailed`
  stays for the objects of the sync that the operator cannot write (ADR 0008).

### The operator's token

One token per Treebind, for the operator's own service account, with the audience
`<group>/<namespace>/<name>` and the lifetime of 10 minutes, which is the least
the API server grants. It is kept in memory, renewed when 5 minutes are left, and
dropped when the agent refuses it, so that the next poll asks for a new one.

The permission is a Role in the operator's own namespace: `create` on
`serviceaccounts/token`, with `resourceNames` set to the operator's service
account. On a kind cluster that was enough to get a token for that account, and a
token for any other account was forbidden. Nothing is cluster-wide.

### `AgentUnreachable`

A new condition, written by the status poller. The reconcile adds it as Unknown
with reason `AwaitingAgentReport`, with `Synced` and `Conflict`, and never
changes it afterwards.

| Status | Reason | Means |
| :-- | :-- | :-- |
| `False` | `ReportReceived` | the agent answered with a report |
| `True` | `ConnectionFailed` | the connection was refused or nothing answered |
| `True` | `TokenRefused` | the agent answered 401 |
| `True` | `UnexpectedResponse` | the agent answered with any other status |
| `True` | `ReportUnreadable` | the answer is too large, is not JSON, has another version, or is another Treebind's |
| `Unknown` | `PollFailing` | polls have been failing for less than 3 minutes |
| `Unknown` | `AgentNotRunning` | the name of the sync pod does not resolve |
| `Unknown` | `TokenUnavailable` | the operator could not get a token, which is not the agent's fault |
| `Unknown` | `KeysUnavailable` | the operator could not put the cluster's keys where the agent reads them, so the agent will refuse its token |

A failure is first `Unknown` with `PollFailing`, and turns `True` only when the
polls have failed for 3 minutes in a row, counted from the first of them. The
poller keeps that time in memory, so a restart of the operator or a new leader
starts the count again, and a condition that is already `True` stays `True` while
the polls fail. The wait covers a pod that has just started, whose kubelet has not
yet shown the keys, and a short outage during a rollout, and keeps the condition
from flapping. A poll that gets a report, or whose name does not resolve, or that
the operator cannot make for want of a token, ends the count: none of them is the
agent failing to answer. A name that does not resolve is the pod that does not
exist yet, which `Ready` already says, so it is never `True`. A pod that is still
pulling its image can show `ConnectionFailed` once its name resolves, until its
container runs, and `Ready` says why.

The messages say which of those it is and nothing taken from the network or the
agent: a status code, a size, a version, a fixed phrase.

`AgentUnreachable` does not change `Ready`, and neither does a failure to publish
the keys. The sync can work while the operator cannot read it, and ADR 0004 says
that no report is not evidence of failure. `Ready` is the answer that tools such as
`kubectl wait` and the health checks of a GitOps controller act on, so it keeps one
meaning, the data path, and the reasons the operator cannot see the agent have a
condition of their own, with a message that says what to check.

### A stale report

ADR 0004 left open how a `Synced` that was written before a spec change counts,
and the report has no generation yet. The rule, for when it has one:

- The poller sets the `observedGeneration` of `Synced` and `Conflict` to the
  generation the report says the agent ran with, and `Ready` compares it with the
  Treebind's: a `Synced` of an earlier generation counts as `Unknown`.
- While `AgentUnreachable` is `True`, `Synced` and `Conflict` are `Unknown` with
  the reason `AgentUnreachable`, because their last values are no longer known to
  hold. The next report puts them back.

Version 1 of the report holds no state of the sync, so nothing writes `Synced` or
`Conflict` past the first `Unknown` yet, and this rule has no effect until the
report grows. It is recorded here so that the change that adds the sync state to
the report does not have to decide it again.

## Consequences

- The operator may create and patch ConfigMaps in every namespace. That is the one
  widening of the cluster-wide role, and a test keeps it from growing into reads
  or into Secrets. A namespace with a quota of ConfigMaps spends one per Treebind.
- A ConfigMap of the name `<name>-sync-keys` that the Treebind did not create is
  left alone, and the keys are not published for that Treebind. The operator logs
  it and the message says to delete the ConfigMap; the poller shows it in
  `AgentUnreachable`. The sync is not affected.
- A tenant who can edit the ConfigMap can put other keys in it and so forge a token
  for their own agent. That tenant already controls the pod, which the agent trusts
  (ADR 0011), and the operator puts the keys back.
- The kubelet shows a changed ConfigMap in a running pod only after a delay of
  about a minute or more, so a rotation of the cluster's keys can refuse the
  operator's token for a few minutes. The poller renews the keys when an agent
  refuses its token, and `PollFailing` covers the wait.
- The operator's token goes over plain HTTP on the pod network, as ADR 0011
  accepts. A token read on the way is good for 10 minutes and only for the report
  of one Treebind.
- The poller needs the network policy of a namespace to let the operator's pod
  reach port 8080 of the sync pod. Examples are #117.
- This record is put into effect in several changes. The first publishes the keys
  and sets `publishNotReadyAddresses` on the Service. The client for the
  endpoint, the token, the poller and the condition follow, and until they do the
  operator does not poll.
