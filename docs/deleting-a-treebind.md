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

The answer is `no`.

## Creating the Treebind again

A new Treebind for the same claim finds the checkout that is already there.
Taking over a directory that already has files is not implemented yet.
