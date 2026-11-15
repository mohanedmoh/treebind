# What the agent does with your directory at start

When the sync pod starts, the agent first checks the volume (the checks `Mount`, `Writable` and
`GitDir`, see [the Git directory](git-directory.md)). Once those pass it takes the writer lock of the
volume, which waits while another agent holds it (the check `Lock`), and then makes the checkout of the
repository on your claim, or opens the one that is there. An agent that loses the lock to another stops
with the exit code 3, and its pod's restart waits for the lock again. The rules never write over data the agent
did not put there. The decisions behind them are in
[ADR 0017](adr/0017-the-first-checkout-never-writes-over-data-it-did-not-put-there.md) and
[ADR 0018](adr/0018-the-operator-tells-the-agent-which-repository-to-sync-with.md).

The result is the check `Checkout` in the agent's report, and a line in its log:

```
kubectl logs <name>-sync-0
kubectl describe treebind <name>
```

The agent does not report itself ready after the checkout. It is ready after the first sync.

## What it does

| What the agent finds | What it does | Result |
| :-- | :-- | :-- |
| An empty directory | Fetches the branch and checks it out | cloned |
| A directory with exactly the files of the branch | Makes it the checkout without writing a file | adopted |
| A directory with other files | Changes nothing | `DirectoryNotEmpty` |
| A repository with no commits yet | Makes an empty checkout. The first sync commits what is in the directory | cloned or adopted |
| A checkout from an earlier start of the same repository and branch | Opens it | reopened |
| A checkout of another repository, or of another branch | Changes nothing | `RepositoryMismatch` |

"Exactly the files of the branch" means that no file differs, is missing or is extra. Empty
directories do not count, and `.treebind` is not counted when the whole claim is synced.

## When it does not work

The agent stays up, says why in the `Checkout` check, and tries again with a longer wait, up to a
minute. It carries on once the cause is put right, with no restart.

| Reason | What it means | What to do |
| :-- | :-- | :-- |
| `DirectoryNotEmpty` | The directory holds files that are not the branch's. Nothing was touched | Empty the directory, or make it hold exactly the files of the branch. To publish what is there, point the Treebind at an empty repository: the first sync commits it |
| `RepositoryMismatch` | The checkout in `.treebind/git` is of another repository, follows another branch, or is not one the agent made | To keep following the branch it has, set `spec.repository.branch` to it. To start again, empty `.treebind/git`; commits that were not pushed are lost with it |
| `BranchNotFound` | The repository has no such branch, or no default branch to follow | Set `spec.repository.branch` to a branch it has |
| `ReservedPath` | The branch has a file named `.treebind` at its root, where the agent keeps its state when the whole claim is synced | Use a `subPath`, or rename the file |
| `FetchFailed` | The repository cannot be reached, the key is refused, or the host key is not in the known hosts | The message has what Git said. Check the address, the deploy key and the known hosts |
| `GitDirUnsafe` | The Git directory holds something the agent did not put there | See [the Git directory](git-directory.md) |
| `CheckoutFailed` | The agent could not write to the volume, or its settings are wrong | The message says which |

## Things to know

- **A volume with data in it does not sync until the directory is emptied or matches the branch.**
  That is on purpose. To back up an existing volume, point the Treebind at an empty repository.
- **`http://` addresses are refused.** Git over plain http sends everything in the clear. The agent
  exits at start with the reason in its log. Use `https://`.
- **Changing the branch or the address of a Treebind replaces its sync pod.** The new pod reopens the
  checkout and refuses one that follows another branch or belongs to another repository.
- **The repository has to answer at every start.** The agent checks that the checkout is of the same
  repository by comparing history with the branch it fetches. Your application does not depend on it.
- **A crash in the middle of the first clone is finished on the next start.**
