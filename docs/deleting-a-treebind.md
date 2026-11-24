# Deleting a Treebind

Deleting a Treebind stops the sync and removes what Treebind created for it. It
never deletes your volume or the data on it.

## What is removed

- The sync StatefulSet, `<name>-sync`, and its pod.
- The headless Service `<name>-sync`.

Kubernetes removes both through owner references, so they go even when the
operator is not running.

## What is kept

- The PersistentVolumeClaim and everything on it: your files, the checkout the
  agent kept there, and any work that was not pushed yet.
- The repository on the Git host.
- The Secret and the ConfigMap the Treebind named for its SSH key and known
  hosts.

## Why the operator cannot delete your claim

The operator does not create or own claims, and its roles give it no permission
to change or delete PersistentVolumeClaims or PersistentVolumes. You can ask the
API server. These are the namespace and service account of the default install:

```sh
kubectl auth can-i delete persistentvolumeclaims -n <namespace> \
  --as=system:serviceaccount:treebind-system:treebind-controller-manager
```

The answer is `no`. The claims of [the quickstart](quickstart.md#clean-up) go
because their namespace is deleted, and its examples delete theirs with
`kubectl delete pvc` after the Treebind.

## Creating the Treebind again

A new Treebind for the same claim finds the checkout that is already there and
opens it, when it is of the same repository and branch. A directory that has
files and no checkout is taken over when it holds exactly the files of the
branch: the agent writes no file, and the directory is the checkout. A directory
with other files is left as it is, and the agent says `DirectoryNotEmpty` in its
check `Checkout` until the directory is emptied or holds exactly the files of the
branch. [The checkout](checkout.md) has the rules.
