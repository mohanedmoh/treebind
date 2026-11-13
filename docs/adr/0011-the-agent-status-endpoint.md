# 0011. The agent status endpoint

- Status: accepted
- Date: 2026-11-12
- Issue: #104

## Context

The agent holds no Kubernetes credentials, in either placement (NFR-16). The
state of a sync still has to reach the status of the Treebind, so the operator
reads it from the agent over an endpoint (ADR 0004). The endpoint tells a caller
paths and error text, so it must not answer any pod that can reach it (R-44).

How the operator proves who it is was left open, with two constraints. The
operator never reads a Secret (#30, R-16), so the two sides cannot share a token
that lives in one. And the agent cannot ask the API server whether a token is
good, because that takes a credential.

## Decision

### The operator authenticates with its own service-account token

The operator calls the agent with a short-lived Kubernetes service-account token
whose audience names the Treebind: `treebind.example.org/<namespace>/<name>`. The
agent verifies the token itself, with no call to the API server:

- the signature, against the cluster's public signing keys (the JWKS), with
  RS256 or ES256 and no other algorithm;
- the issuer, which is the cluster's;
- the audience, which is this agent's Treebind;
- the subject, which is the operator's service account. Any pod can get a token
  for its own service account with any audience, so the audience alone proves
  nothing;
- the expiry and the not-before time.

The issuer, the audience, the subject and the JWKS are public. The agent reads
them from its configuration, and the JWKS from a file that it reads again when a
token names a key it does not know, so a key rotation needs no restart. It reads
the file again at most once in ten seconds, because a stranger chooses the key a
token names and must not be able to make the agent read a file on every request.
It also reads it again when the keys are five minutes old, so that a key taken
out of the file stops being trusted. A file that cannot be read or parsed leaves
the keys that were loaded in place, so a fault in the file does not shut the
operator out, but only for an hour: keys that have gone that long without a
successful read are not used, whether or not a read was tried, and every token
is refused until the file is good again. Until a first read has succeeded every token is refused, and the file is
read again each second until one has, so that a file that appears just after a
failed read is used at once. A request that needs a read that another request
has started waits for it, for at most half a second. A read that takes longer than
five seconds counts as failed and is left to finish by itself, and no other read
starts until it has, so a file on a mount that hangs costs one goroutine and does
not hold the requests. A clock that differs by up to
30 seconds from the cluster's is tolerated. Keys that are not RSA of 2048 to
8192 bits or P-256 are skipped, and a key ID that appears twice is an error. The
agent verifies with the Go standard library and gains no dependency.

Nothing secret is shared, so there is nothing to store, read or rotate. On a
kind cluster a token with such an audience was signed with RS256 and lived ten
minutes, the API server refused it as a credential, and every service account
could read the JWKS.

Three other ways were considered. A token in a Secret needs the operator to read
or write Secrets. A token in the pod spec is readable by everyone who can read
pods. Mutual TLS needs certificates issued and rotated for every agent.

### The endpoint is read-only and says little to a stranger

- `GET /status`, or `HEAD`, with the token as a bearer token. A query string has
  no effect. Every other path answers `404` to anyone, and the path is compared
  as it was sent, with nothing cleaned or decoded.
- An accepted caller that uses another method gets `405`.
- A caller that is not accepted gets `401` and the same body whatever the cause,
  whether the token is missing, malformed, expired or forged. The agent can log
  the cause; the caller never sees it.
- The server has timeouts for reading and writing, a limit on the size of the
  headers and a bound on the tokens verified at once. A caller with a well-formed
  token that arrives beyond the bound gets `503` with `Retry-After`, which means
  "ask again" and says nothing about the token. These bound what a burst costs. They do not stop a flood: verifying a token costs about as much as
  answering any request, so keeping strangers away from the port is the job of a
  NetworkPolicy (R-44).
- No answer may be cached.
- The transport is plain HTTP on the pod network. A token that is read on the
  way only reads this agent's report and expires in minutes.

### The agent's settings

The operator puts the settings in the sync pod as environment variables. None is
secret: they are names, paths and public facts about the cluster.

| Variable | Meaning | Default |
| :-- | :-- | :-- |
| `TREEBIND_NAMESPACE`, `TREEBIND_NAME` | the Treebind the agent works for | required |
| `TREEBIND_MOUNT_PATH` | where the claim is mounted | `/work` |
| `TREEBIND_STATUS_ADDR` | where the endpoint and the probes listen | `:8080` |
| `TREEBIND_STATUS_ISSUER` | the `iss` claim of the operator's token | required |
| `TREEBIND_STATUS_AUDIENCE` | the `aud` claim: `<group>/<namespace>/<name>` of the Treebind | required |
| `TREEBIND_STATUS_SUBJECT` | the `sub` claim: the operator's service account | required |
| `TREEBIND_STATUS_KEYS_FILE` | the cluster's public signing keys | `/etc/treebind/status/jwks.json` |

The operator gives the audience whole, so the agent does not know how it is put
together and the API group can change without a change to the agent. An agent
with a setting missing or wrong exits with code 2 and says every problem at once,
because a restart does not cure a wrong pod.

### What the operator puts in the sync pod

- **No service-account token.** The pod does not mount one
  (`automountServiceAccountToken: false`), so the agent can neither call the API
  server nor read anything from it.
- **The port and the probes.** The container has the port `status`, 8080, and
  `TREEBIND_STATUS_ADDR` names the same one. The readiness probe asks `/readyz`
  and the liveness probe `/healthz`, every 10 seconds, with a timeout of 2
  seconds, and a probe counts as failed after three misses in a row.
- **The settings.** The mount path is cleaned first (`/work/` becomes `/work`),
  because the agent refuses a path that is not in its shortest form and the spec
  allows one. The audience is `<group>/<namespace>/<name>` of the Treebind. The
  subject is the service account the operator pod runs as, which the Deployment
  gives it through the downward API (`POD_NAMESPACE` and `POD_SERVICE_ACCOUNT`).
  The issuer is the one the API server publishes at
  `/.well-known/openid-configuration`, which every service account may read. The
  flags `--status-issuer` and `--status-subject` replace both, for a cluster whose
  discovery document is not readable. The operator does not start when it cannot
  find them out, because every agent it created would exit with code 2.
- **The keys.** The pod mounts the ConfigMap `<name>-sync-keys`, key `jwks.json`,
  at `/etc/treebind/status`, and marks it optional. The agent starts without the
  keys and refuses every caller until they arrive, and the kubelet puts the file
  there when the ConfigMap is created. Creating and refreshing it is #99.

A change to any of these changes the pod template, so the sync pods of every
Treebind are replaced once when the operator is upgraded to a version that sets
them.

### The agent stays up

A failed volume check does not end the agent. It serves its report, which says
what failed, and runs the checks again, after 5 seconds and then with a wait that
doubles up to a minute, until none fails. Then it stops checking: the checks are
for before the first sync. It stops cleanly on SIGTERM, which is the only warning
a pod that is being deleted gives (ADR 0006): the endpoint stops taking requests
first, requests that are being answered finish, and a volume check that does not
return in five seconds is left behind, so that a hung mount cannot hold the agent
past the time its pod gives it.

The probes are on the same port and need no token. `/healthz` says the agent is
running, and stays `ok` while a check fails, because a restart would not cure a
volume. `/readyz` says it has synced, which no agent can yet, so it answers `503`
until the sync loop exists. A Treebind therefore does not turn `Ready` before
anything syncs.

A caller that is refused is logged at most once in thirty seconds, with the count
of the ones that were held back, because anyone who can reach the port can cause
refusals.

### The report is a contract

The answer is a JSON document:

| Field | Meaning |
| :-- | :-- |
| `version` | the version of the format, `1` |
| `treebind` | the `namespace` and `name` of the Treebind the agent works for |
| `startedAt` | when the agent process started |
| `time` | when the report was made, by the agent's clock |
| `checkedAt` | when the checks last finished; absent until they first have, and `checks` is then empty because they are still to run, not because nothing failed |
| `checks` | the preflight checks in the order they ran, each with `name`, `outcome`, `reason` and `message` as ADR 0009 defines them |

Within a version, fields are only added, and a reader ignores the ones it does
not know. The version changes when a field is removed or changes its meaning.
Messages follow ADR 0004: no credential, and no text from a Git host that was
not sanitized.

The report will grow as the agent does. The state of the sync joins it with the
sync loop. The generation of the spec the agent runs with joins it when the agent
gets its configuration, and that settles how a `Synced` from before a spec change
counts, which ADR 0004 left to this record.

## Consequences

- The operator needs no Secret and no new permission to authenticate. It has to
  request the token for each Treebind, fetch the JWKS and put it where the agent
  reads it, which is #99, together with polling, the conditions and
  `AgentUnreachable`.
- A token for one Treebind fails at every other agent, so an agent that was
  taken over cannot use the token it saw to read another agent's report, and it
  cannot use it against Kubernetes.
- The agent trusts the keys in its configuration. Whoever can change the sync
  pod can change them, and that person controls the pod anyway.
- Someone who can read traffic on the pod network can read a report. TLS for the
  endpoint is a later step, and a service mesh that encrypts pod traffic covers it
  today.
- The agent stays up to answer, so it no longer exits when a preflight check
  fails. It reports the failure and runs the checks again. That is the wiring
  #31 waited for.
- The kubelet cannot present a token, so the readiness and liveness probes
  (#48) have their own paths, which tell a caller nothing but ready or not.
- Until #99 creates the ConfigMap of keys, the endpoint refuses every caller, so
  the operator cannot read a report yet. The probes and the agent itself work.
