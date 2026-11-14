# Where the agent keeps its Git directory

The sync agent works in a Git checkout on your claim. The Git directory of that checkout
holds the repository's config and hooks, and Git runs what they say with the agent's
credentials. So it must not be somewhere the application can write. The reasons are in
[ADR 0016](adr/0016-the-git-directory-lives-on-the-claim-beside-the-synced-directory.md).

The agent mounts the whole claim and keeps its own state in the directory `.treebind` at
the root of the claim, the Git directory in `.treebind/git`. It syncs the directory named
by `spec.target.subPath`, or the whole claim when there is none.

## Use a `subPath`, and mount only that

```yaml
apiVersion: treebind.example.org/v1alpha1
kind: Treebind
metadata:
  name: dashboards
spec:
  repository:
    url: ssh://git@example.com/team/dashboards.git
    branch: main
  target:
    claimName: grafana-data
    subPath: dashboards
  sync:
    direction: bidirectional
```

With a `subPath`, `.treebind` is beside the synced directory. Mount the claim in your
application with the same `subPath`, and the application sees the synced files and
nothing else:

```yaml
containers:
  - name: grafana
    volumeMounts:
      - name: data
        mountPath: /var/lib/grafana/dashboards
        subPath: dashboards
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: grafana-data
```

Nothing in the cluster can make an application mount only the `subPath`: the mount is in
your own pod. If it mounts the whole claim, it can see `.treebind`.

## If a `subPath` mount is not an option where you run

Treebind's own sync pod never uses a `subPath` mount: it mounts the whole claim, and
`target.subPath` is only a directory that the agent works in. The `subPath` mount is the one
in *your* application, and it is a bind mount that the kubelet makes. Some platforms advise
against such mounts, for example where the kubelet runs in a container and can be restarted,
or where the volume under a running pod can be remounted. If that holds where you run, there
are two other ways, and neither needs a change in Treebind:

- **A volume whose root already is the directory.** Give the application a claim to a volume
  that starts at the synced directory, for example a static PersistentVolume for NFS whose
  `path` is that directory. The application then mounts that claim whole and cannot see
  `.treebind`. The agent keeps using the main claim with a `subPath`. It takes static
  provisioning, which Treebind does not do for you.
- **No `subPath` at all.** The protection below applies. It is weaker, and the agent works.

## Without a `subPath`

The whole claim is synced, so `.treebind` is inside what the application mounts. That works,
with less protection: the directory is closed to other users (mode 0700, owned by the agent),
and it is never committed. An application that runs as the same user as the agent, or as
root, can still read and write it. A `subPath` cannot be `.treebind` or start with it.

## What the agent checks

At start the agent makes `.treebind` and `.treebind/git` with mode 0700 and checks them. It
refuses a directory that is a link, is not a directory, belongs to another user than the one the
agent runs as, or is open to group or others. It refuses a link on the way to the synced
directory too, because a link there could point the work tree into `.treebind`. It follows no
link in `.treebind` and `.treebind/git`, and refuses one of them that was swapped for a link, or
for another directory, while it was being checked. The way to the synced directory is looked at
once, at start.

A directory that fails is reported by the check `GitDir` with the reason `GitDirUnsafe`, and Git
is not run against it. The agent keeps running and checks again, with a longer wait each time
up to a minute, so it carries on once the directory is put right. A claim that cannot be written
to fails the check with `ReadOnlyVolume`, `NotWritable` or `WriteFailed`, as the check
`Writable` does. The result is in the agent's log, once for each change, and in its status
report, which [the status page](agent-status.md) shows how to read by hand. The operator does not show it as
a condition yet.

To put it right, remove `.treebind` or `.treebind/git` when the message names one of them, or
make it a directory of the user the agent runs as with mode 0700. When the message names a link
or a file on the way to the synced directory, replace it with a real directory. The agent does
not change a directory that is not its own.

## Room on the claim

The Git directory holds the history of the branch, so the claim needs room for it besides
the files.
