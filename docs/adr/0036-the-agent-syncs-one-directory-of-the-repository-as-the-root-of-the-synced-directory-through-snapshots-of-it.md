# 0036. The agent syncs one directory of the repository as the root of the synced directory, through snapshots of it

- Status: accepted
- Date: 2026-11-19
- Issue: #44
- Extends: [0016](0016-the-git-directory-lives-on-the-claim-beside-the-synced-directory.md), [0017](0017-the-first-checkout-never-writes-over-data-it-did-not-put-there.md), [0020](0020-the-agent-pulls-by-fast-forward-and-never-writes-over-a-local-edit.md), [0022](0022-the-agent-decides-what-it-syncs-with-gitignore-patterns-of-its-own.md), [0026](0026-the-agent-refuses-an-update-that-removes-many-files-and-keeps-the-commit-it-came-from.md), [0033](0033-the-agent-pushes-the-commits-as-a-fast-forward-and-is-ready-after-its-first-push.md)

## Context

`spec.target.subPath` is the directory of the volume that is synced. `spec.repository.path` is a directory of the repository,
and the API says "Only that directory is synced with the volume. The whole repository is used when it is empty." FR-10 asks for
"a sub-path of the repo and of the volume". Nothing in the operator or the agent read the field: the operator passed the URL, the
branch and the credentials and nothing else, so a Treebind that set it synced the whole repository.

Honouring it changes what every Treebind that sets it does to a volume that already holds data, which is the case that FR-18
and R-2 are about, and the question of where Git writes the files has two answers. Both were tried on real Git, with the agent's
own `layout`, `checkout`, `pull`, `scan`, `commit` and `push` packages and its Runner, on Git 2.34.1 and on 2.56.1, the Git of the
agent image, before this record was written.

### What was tried

**The field today.** A checkout of a repository with `grafana/dashboards`, `grafana/alerts`, `other` and a `README.md` puts all
of it on the volume. An application that writes `grafana/dashboards/new.json`, `other/new.txt` and `outside.txt` has all three
scanned, committed and pushed, in one commit. With `spec.paths.include` set to `grafana/dashboards/**` the other two are left out
(`NotIncluded`). A repository that removes `grafana/dashboards` (two files) is applied by the pull, and the volume loses them.

**Sparse checkout.** It materializes the files at the same relative path, so the application that mounts `subPath: dashboards`
would see `dashboards/grafana/dashboards/x.json`. Non-cone mode needs `info/sparse-checkout` and `-c core.sparseCheckout=true` on
every command, and no key in the config file; cone mode also writes the files directly in the root of the repository. ADR 0019
refuses `info/sparse-checkout` (the agent keeps `info` empty), `git sparse-checkout set` is not a subcommand the Runner allows and
writes `config.worktree` and `extensions.worktreeConfig`, which the look refuses too. The commands the agent already runs do not
fit: a plain `read-tree HEAD`, which the adoption, the tree of a resolution and the repair use, drops the skip-worktree bit, and
afterwards `status` lists every file outside the path as deleted, on both Gits. A file that the application writes at a name the
repository has outside the path is invisible on 2.34.1 and is a modification that makes `merge --ff-only` and `checkout -B`
refuse on 2.56.1. A partial fetch (`--filter=blob:none`) works through the Runner, and writes `[remote "<url>"] promisor` and
`partialclonefilter` into the config, which stores the URL that ADR 0017 keeps out of it and which the look refuses.

**Root to root, a projected checkout.** `read-tree <commit>:<path>` writes the files at the root of the work tree, and with the
real commit as `HEAD`, `status` says `D README.md` and `R grafana/dashboards/x.json -> x.json`. What works is a `HEAD` that is a
snapshot: a commit whose tree is `<commit>:<path>`, which `commit-tree` makes and the Runner allows. Snapshots of a repository's
history, one for each real commit that changed the directory, were put in a second repository and the unchanged packages were run
against it: the first checkout writes the files at the root of the volume; a change outside the directory is `UpToDate`; one inside
is `Advanced`; an edit of the application that the update changes is `Blocked`; a repository that removes the directory is
`TooManyDeletions` ("no file at all", the snapshot's tree is empty); a volume that holds exactly the files of the directory is
`Adopted` and one that holds the whole repository is `DirectoryNotEmpty` with every file where it was; a rewritten branch is
`Diverged` with no policy and `Resolved` with either. A commit of the agent on the snapshots was made again as a commit of the
repository with the subcommands the Runner allows, `cat-file tree`, `hash-object -w -t tree <file>` and `commit-tree`, and the tree
was the one that Git makes with `read-tree --prefix` in an index of its own, for names with a space, a newline and non-ASCII
letters, an executable, a link, and siblings that sort next to the directory.

**Existing data.** A Git directory of the old agent, which holds the whole repository, and a Treebind that now sets the path was
`RepositoryMismatch` with "no history in common ... it is of another repository", and so was a changed path and one that was taken
away. Every file stayed where it was in each case. The check was there by accident and said something that is not true.

**The state directory.** With no `subPath` the state directory is inside the synced directory. A directory of the repository that
holds a `.treebind` is `ReservedPath`; a path that is `.treebind/cfg` is a directory like another; a repository that has `.treebind`
at its root and a path elsewhere is not refused.

## Decision

### The files of the directory are the files of the synced directory

The directory named by `spec.repository.path` is the root of the synced directory: `grafana/dashboards/x.json` is `x.json` on the
volume, with `target.subPath` and without it. This is what the sub path of the volume already is for the volume and what FR-10 says.
Sparse checkout is not used: it gives the other layout, needs the Git directory and the Runner opened for a file and a subcommand,
and breaks the commands that the agent runs.

### A branch of snapshots

The checkout is of a branch of snapshots (`internal/agent/subtree`). The repository's own branch is fetched, as it always was, into
`refs/treebind/fetched/<branch>`, and a snapshot of its directory is made when the directory differs from the one of the snapshot that
`refs/remotes/origin/<branch>` holds. Only then is `refs/treebind/real/<branch>` moved to the tip, and it is the last write of a
projection: the pull fetches only when the tip that the repository has is not the one that the real ref holds, so a real ref that is
ahead of its snapshot, which a projection that failed after the fetch would leave if the fetch wrote it, would be taken for a pull
with nothing to do and the update would never come in. A projection that fails moves neither, the next pull fetches again, and the
reasons of a refused projection make the wait grow, so that a Git host is not asked more for it (R-19). The pull, the scan, the commit, the filter, the conflict policies, the cap and the recovery
refs work on the snapshot branch as they are: the files of the volume are the tree of the snapshot, and every rule about the files
of the branch is about the files of the directory.

- A snapshot has the tree of the directory and a message that ends with two trailers, `Treebind-Source: <commit>` and
  `Treebind-Path: "<path>"` (quoted: the CRD takes a name with a newline in it). A message is a snapshot's only when the two are
  its whole last paragraph, and the source is believed only when the directory at that commit is the snapshot's tree, so that a
  message template cannot make a commit a snapshot.
- It continues the snapshot before it when the repository's branch continues the commit that one is of, and has no parent when it
  does not: a rewritten branch is two histories that have nothing in common, `Diverged` as a force-push is today.
- It is made only when the directory differs. A change outside the directory moves `refs/treebind/real/<branch>` and nothing else.
- One is made for each pull that changed the directory, and not for each commit of the repository. The first checkout makes one
  and walks no history.
- A directory that the branch does not have is refused for the first checkout (`PathNotFound`) and is an empty snapshot after it,
  which the cap on deletions judges. Something that is not a directory (a file, a link, a submodule) is refused with the same
  reason.
- The snapshots are made by a Runner of their own with a fixed identity, `treebind-snapshot`. A pull that only pulls has no
  identity, `commit-tree` needs one, and the identity of the agent's commits is not used because a resolution refuses to run
  without a valid one (ADR 0034).

### The checkout is for one directory

A checkout is made for one directory, or for the whole repository, and is not changed to the other (FR-18). The directory is
written in `.treebind/repository-path`, with the name in quotes, before the first file of a first checkout, and a checkout of the
whole repository has no such file, which is also what a checkout from before the field was read is. It is not in the Git directory,
which the agent keeps to what Git makes (ADR 0019), and not in a commit, because a repository that has no commit has no snapshot to
say it in. A start judges it after the look at the Git directory and before the repository is asked, when the checkout has a commit, and refuses
a checkout made for another directory, for none, or for one when the Treebind names none, with the reason `PathMismatch`, so that nothing goes
over the network for a checkout that is refused and a host that is down is not what the Treebind is told. Nothing is changed. The message says how to keep what is there (remove or set back the path) and how to start again (empty the
synced directory and the Git directory), and the second loses the commits that were not pushed. A Git directory with no commit is a first checkout that did not
finish, which is judged afresh whatever it was made for, with the rules that protect a volume that holds data (ADR 0017): a start
that was stopped between the making of the Git directory and the writing of the marker is not refused for a marker that was never
written. A first checkout writes the file when there is a path and removes one that is left when there is none, so that starting
again by emptying the Git directory is what it says.

A Treebind that already sets `spec.repository.path`, and whose sync pod is replaced when the operator is upgraded, is in this case:
its checkout is of the whole repository. It is refused, not migrated. A migration would remove every file that is not in the
directory and move the others, which is the destruction that ADR 0017 forbids, and the application is not told. The way back is to
remove the field.

### The pull

A pull compares the tip that `ls-remote` says with `refs/treebind/real/<branch>`, and fetches only when they differ, so a change
outside the directory costs one question and writes nothing. The snapshot is then the tip that the rest of the pull compares
the checkout with. The result names the commits of the repository beside the snapshots, and the checks say the repository's, which a
person can look up.

### The push

A commit of the agent on the snapshots is sent as a commit of the repository. Each commit that is waiting, and is not a snapshot, is
made again on the real counterpart of its parent: a snapshot's, from its trailer, and the tip of the repository for the snapshot at
the tip. Its tree is the tree of that commit with the directory replaced, which is made with the trees along the way and nothing else
changed, and its message and its author are the commit's, the author being the one of `spec.commit`. A commit of the agent cannot
change a file outside the directory, and a change that someone else makes there never stands in its way: the commit is made on the tip
that was fetched, and after a pull it is sent again on the new tip with no conflict. A merge of a `followLocal` resolution is a merge.
A commit that the agent sends has the date of the push: the Runner takes no date.

After the push the checkout is put at the snapshot of the commit that was sent, on the old snapshot and with the tree it already has,
so that every commit the checkout holds beside its own is a snapshot, and no commit is sent twice. It moves, in this order, the ref of
the snapshot, the branch of the checkout and the ref of the repository's tip, behind the writer lock. The repository's tip is last:
the pull fetches only when it is not the tip that the repository has, so a tip that is held with no snapshot of it would never be put
right. A kill after each of the writes is tried with a stand-in for Git, with both policies: the volume is as it was, nothing is sent
again, and the next pull ends with a snapshot at the tip. A commit and a snapshot with the same tree are a resolution that moves to
the tip with no commit (ADR 0035).

### The settings

The operator passes `TREEBIND_REPO_PATH` only when the spec sets the path, so that no sync pod is replaced for a Treebind that does
not, and a Treebind that does has its pod replaced once, as for every setting. Each `$` is written as `$$`, which the kubelet turns
back into one, and so it is for the sub path, the mount path, the branch and the address of the repository since #208.
The agent reads the path as it is written and judges it with the rules of the CRD, which a test holds together for a
list of paths, and exits with code 2 on one that it cannot use. The field is not immutable: removing it is the way back for a
Treebind that sets it today, and an immutability rule would make it delete and recreate its Treebind. The text of the field says
where the files are written. `PathNotFound` and `PathMismatch` are new reasons and are API (ADR 0004).

## Consequences

- A Treebind that sets the path has the files of that directory on its volume and a checkout of snapshots. A Treebind that does not is
  not touched: no setting, no file, no ref, no command.
- A Treebind that sets the path today is refused at its next start, with the application's files where they are, until its author
  chooses one of the two ways out.
- The first fetch brings the whole repository. A partial clone would bring the directory only and cannot be had with the Git directory
  that the agent allows.
- The history in `.treebind/git` is of snapshots, one for each pull that changed the directory, and not of the repository. The
  repository's history is there too, in the objects that were fetched.
- A branch that was rewritten is `Diverged`, as for the whole repository. A branch that was rewound to an older commit is `Diverged`
  when the directory differs, where the whole repository is `Ahead`, because the older snapshot is not a continuation of the newer one.
  When two snapshots of the same directory, source and parent are made in the same second they are the same commit, and a rewind to
  the first of them is `Ahead`.
- The commits that the agent sends have the date of the push and not of the application's write.
- A kill between the push and the settling is put right by the pull and makes, at most, one resolution of two commits with the same
  tree. Commits of the application that are made between the kill and that pull are resolved by the policy, and the commit that was
  sent can then be made again in the merge that the resolution sends. This is not wrong, and it was not made to happen.
- A repository that has no commit, with a path, takes the first commit of the application as a root commit with the directory in it. If
  someone else then pushes content that has no such directory before the agent does, the pull says `PathNotFound` until the directory
  is made in the repository. It was not tried.
- Two records say what a checkout was made for: the file in the state directory, which is read at a start and holds for a checkout that
  has no snapshot, and the trailers of the snapshots, which the projection and the translation check before they trust a commit.
  They can disagree only when something other than the agent changed one of them, and each refuses the checkout when they do.
- The file that a tree is written to for Git to read is in the state directory. It is made through a root that does not follow a link
  and exclusively, after a leftover of a killed run was removed, so that a link planted where it goes is not written through. Git
  then reads it by name, and a change between the two is not ruled out, for an application that can write the state directory.
- The cost of a push is a few Runner commands for each commit that waits, which was not measured.
- A directory of the repository with so many entries that its tree is over the 16 MiB that the Runner takes of the output of a command
  fails the push with its cause. The arithmetic says some hundreds of thousands of names; it was not tried.

### What was not tried

Volumes that fold case, keep no mode bits or are not a local file system; a real Git host; the Gits between 2.34.1 and 2.56.1; a
repository with a long history, where only the size of the first fetch could matter, since nothing here walks the history; a kill in
a cluster; and the cost of the reset that the commit step makes in a checkout of many files, which is as it was.
