# Syncing one directory of the repository

`spec.repository.path` names a directory of the repository. The files of that directory are the files of your
synced directory, at the root of it, and nothing else of the repository is written to the volume. The decision behind
it, and what was tried, is in
[ADR 0036](adr/0036-the-agent-syncs-one-directory-of-the-repository-as-the-root-of-the-synced-directory-through-snapshots-of-it.md).

```yaml
spec:
  repository:
    url: https://github.com/acme/platform.git
    path: grafana/dashboards     # the files of grafana/dashboards in the repository
  target:
    claimName: grafana-data
    subPath: dashboards          # are the files of dashboards/ on the volume
```

With this, `grafana/dashboards/overview.json` in the repository is `dashboards/overview.json` on the volume. The
application that mounts the `subPath` sees `overview.json` at the root of what it mounts, and never the rest of the
repository. Without a `subPath` the files are at the root of the claim, beside `.treebind`.

## What it does

- **A change that the repository makes outside the directory is no change of your volume.** The agent still asks the
  repository one question at every interval, as [pulling](pull.md) says, and fetches only when the branch moved. A
  fetch brings the whole repository: its history and its files are on the claim, in `.treebind/git`, and a repository
  that is large makes that directory large.
- **The patterns of `spec.paths` are relative to the directory.** `**/*.json` and `secrets/` mean the files and the
  directory of the same name in the directory you named, and not in the repository.
- **What the agent commits changes the files of the directory and nothing else.** A `bidirectional` Treebind sends each
  commit of its own as a commit of the repository, on the tip it fetched, with the files of the directory as your
  volume has them and every other file of the repository as the repository has it. A change that someone else makes
  outside the directory never stands in the way of a push: after a pull the commit is sent again on the new tip.
- **The commits that the checks name are the repository's.** The checkout in `.treebind/git` holds snapshots of the
  directory, one for each pull that changed it, and the checks say the commits of the repository, which you can look
  up. The date of a commit that the agent sends is the date of the push.
- **A directory that the repository removes is the same as a branch that was emptied.** The update is refused by
  [the deletion guard](deletion-guard.md), with every file where it is, because it would leave the volume with no file.

## The directory has to be there

The branch has to have the directory when the Treebind first starts, and it has to be a directory: a file, a link or
a submodule at that path is refused. This is on purpose. A directory that is only to come is as often a mistake in a
name as it is a directory to come, and the agent would commit what your application writes into a new directory of the
repository for it. Make the directory in the repository first, or point the Treebind at an empty repository, which
takes any directory and commits it with the first sync.

## A volume that already holds data

A checkout is made for one directory, or for the whole repository, and the agent does not change it to the other: the
files that the other would write over or remove may be your application's. Before this was supported the agent ignored
`spec.repository.path` and synced the whole repository, so **a Treebind that already sets the field, and is upgraded, has
a checkout of the whole repository.** Its new sync pod refuses it with the reason `PathMismatch`. Every file is where it
was, the application is not touched, and the agent stays up and says why in the `Checkout` check.

There are two ways out, and the message says both:

- **Keep what is there.** Remove `spec.repository.path`, and the Treebind goes on with the whole repository as it did.
  The same for a directory that was changed: set it back.
- **Start again with the directory.** Empty the synced directory and `.treebind/git` (stop the application first,
  and copy what you want to keep), and the next start makes the checkout of the directory. Commits that were not
  pushed are lost with `.treebind/git`.

A volume that holds exactly the files of the directory, and no checkout, is adopted as any volume that holds exactly the
files of the branch is.

## When it does not work

The reasons are the ones of [the checkout](checkout.md) and of [pulling](pull.md), with two more:

| Reason | What it means | What to do |
| :-- | :-- | :-- |
| `PathNotFound` | The branch has no directory at `spec.repository.path`, or has something there that is not one. Nothing was written | Check the name, or make the directory in the repository |
| `PathMismatch` | The checkout was made for another directory, or for the whole repository, or the Treebind now names none and the checkout was made for one. Nothing was changed | See above |

## Things to know

- **A branch that was rewritten is `Diverged`, as for the whole repository.** A branch that was rewound to an older
  commit is `Diverged` too when the directory differs, where the whole repository says `Ahead`.
- **`spec.repository.path` can be changed, and the agent refuses the change.** The field is not immutable, because
  removing it is the way back for a Treebind that sets it today.
- **The first fetch brings the whole repository.** A partial clone, which would bring only the directory, is not
  possible with the configuration that the agent allows in its Git directory.
- **A directory with a very large number of entries is not tried.** The agent reads the trees on the way to the
  directory through Git, and the Runner takes at most 16 MiB of what a command writes, which some hundreds of thousands
  of names in one directory of the repository would pass. The step would fail with its cause and the commits would stay
  on the volume. This was worked out and not run.
