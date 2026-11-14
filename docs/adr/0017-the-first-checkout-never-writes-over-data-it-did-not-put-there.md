# 0017. The first checkout never writes over data it did not put there

- Status: accepted
- Date: 2026-11-14
- Issue: #32

## Context

On the first start the agent has to put the repository's branch on the claim, and on
every later start it has to find that checkout again and trust it (FR-1). The directory
it is given is not always empty. An application may have been using the volume for
years, a Treebind may be pointed at a claim that holds a copy of the files, a pod may
have been killed in the middle of the first checkout, or the Treebind may have been
edited to name another repository. A pull or a reset that follows the remote would
delete or replace the application's data in each of those cases (R-2, FR-18).

The Runner of #81 does not allow `git clone`, because a clone fetches submodules. The
agent has to build the checkout from the commands it does allow, and every command it
runs goes through the Runner and its fixed configuration (ADR 0013).

## Decision

### How the checkout is made

`internal/agent/checkout` builds it from `init`, `ls-remote`, `fetch`, `read-tree`,
`checkout` and `update-ref`, with the Git directory and the work tree of the layout
(ADR 0016). `git init` runs with an empty template, so no sample hook is copied into the
Git directory, and writes only `core.repositoryformatversion`, `core.filemode`,
`core.bare`, `core.logallrefupdates` and `core.worktree`. The check of the config that
follows #81 can take these as the keys that the agent itself puts there.

The URL goes on the command line of each command that needs it. It is not stored with
`remote add`, so it is not in the config next to anything else, a change of it in the
Treebind is picked up on the next start, and there is no remote name for a config of the
repository to redefine. The branch is fetched into `refs/remotes/origin/<branch>`, with
no tags, because a tag is a name that the repository chooses, and with no `FETCH_HEAD`,
which would name the URL. A fetch still names the URL in the reflog line it writes. The CRD
does not admit a credential in a URL, so there is none to leak.

When `repository.branch` is empty the agent follows the default branch, which it finds
with `ls-remote --symref`. A repository whose `HEAD` names a branch it does not have is
not an empty one, and the agent says so and asks for `repository.branch`, because
treating it as empty would start a checkout of nothing next to a repository that has
files.

### The rules

The agent never writes over data it did not put there. What it does depends on what it
finds:

| What is there | What the agent does | Result |
| --- | --- | --- |
| No Git directory, the work tree is empty | fetches the branch and checks it out | `Cloned` |
| No Git directory, the work tree matches the branch exactly | makes the checkout without writing a file | `Adopted` |
| No Git directory, the work tree has other files | changes nothing and takes back what it fetched | `DirectoryNotEmpty` |
| No Git directory, the repository has no commits | makes an empty checkout; the first sync commits what is there | `Cloned` or `Adopted` |
| A checkout that shares history with the branch | opens it | `Reopened` |
| A Git directory of another repository, or on another branch | changes nothing | `RepositoryMismatch` |
| The branch is not on the repository | changes nothing | `BranchNotFound` |
| A branch with a file named like the state directory, and no `subPath` | changes nothing | `ReservedPath` |
| The repository cannot be reached | changes nothing | `FetchFailed` |
| The agent cannot write to the volume | | `CheckoutFailed` |

The work tree is empty when it has nothing in it, not counting `.treebind` when that is
inside it. It matches when, with the index set to the tip of the branch, no file is
modified, missing or extra. The comparison is of the bytes on the volume, with no
filter and no conversion, because the Runner reads attributes from an empty tree. Files
that Git does not track are extra whether or not an ignore file would hide them: an
ignored file is overwritten silently by a later checkout of a tracked file with the same
name. An empty directory is not a file and holds no data, so it does not count. The message of a refusal says how many files differ and names three of them, and
what to do.

An empty repository takes any directory. Nothing in it can be overwritten, and the first
sync commits what is there. It is the natural way to back up a volume that already has
data in it.

### Reopening

A checkout from an earlier start is opened only when it belongs to the repository and
branch that the Treebind names. Its `HEAD` has to be on the branch, and its branch has to
share a commit with the branch of the repository, which the agent fetches to find out.
The test is on history and not on a stored address, so changing `ssh://` to `https://` for
the same repository keeps working, and a repository whose branch was rewritten is still
the same repository. A Git directory of another repository would have a sync that follows
the remote overwrite local work, which is the case R-2 names. A checkout that follows
another branch is not switched, because it may hold changes. That includes the default
branch of a repository that is renamed after the checkout was made with no
`repository.branch`: the message says to set `repository.branch` to the branch the checkout
follows, which keeps it, before it says to empty the Git directory, which loses the commits
that were not pushed.

The remote has to answer for a checkout to be opened. A start that cannot reach it
reports `FetchFailed` and tries again, with the checkout and the application's files as
they are. The application does not depend on it, and the agent cannot sync without it.

### A crash in the first checkout (NFR-2)

The first checkout is made so that a branch with a commit means it finished. The agent
writes a marker in `.treebind`, with the commit it is about to check out, after it has
seen the directory empty and before it writes the first file. `checkout -b` makes the
branch after the files. On the next start:

- **A branch with a commit** is a finished checkout, whatever is left of the marker.
- **No commit, and a marker for the branch the Treebind names**: the files in the directory
  are the agent's own, so it removes the lock of the index, which only a crash leaves because
  one agent writes at a time (NFR-1), and checks out the commit of the marker again with
  `--force`. If that commit is gone it fetches the tip and checks that out instead, with the
  same refusal of a reserved path, because the files are the agent's own whichever commit
  they are from.
- **No commit, and no marker**: the agent cannot tell its files from somebody else's, so it
  judges the directory afresh, as on a first start, whatever branch `HEAD` names. It clears
  the Git directory only when it looks like what the agent makes: the entries `HEAD`, `config`,
  `objects`, `refs`, `logs`, `index` and `index.lock`, and no ref outside `refs/remotes/origin`.
  A repository made with a default template, or with a branch, a tag or a stash in it, is
  somebody else's, and is refused with `RepositoryMismatch` and left as it is.

### A branch that writes into the state directory

Without a `subPath` the state directory is inside the work tree, so a branch with files
under `.treebind` would make the checkout write them into the Git directory, a config
that the repository chooses among them, which is what ADR 0013 says nothing can
neutralise. The agent refuses a branch that has a file at the root named `.treebind`, in
any case, because a volume may fold case. With a `subPath` the state directory is beside
the work tree and the paths of a branch cannot reach it. The pull of #33 has to check the
same on every commit it brings in.

### The reasons

The outcomes and the reasons above are strings that users and tooling will match on, and
are API in the sense of ADR 0004. A new cause gets a new reason, and one is not
reworded.

## Consequences

- A Treebind pointed at a volume with data in it does not sync until the directory is
  emptied, or holds exactly the files of the branch. That is the point of FR-18, and it
  can be a surprise, so the message says what to do. An empty repository is the way to
  back up an existing volume.
- The agent needs the remote to answer at every start. A longer outage keeps the sync
  pod from reporting that it has synced, and nothing else.
- A first clone fetches the whole history, with no depth limit, and the checkout writes
  every file in one command. Its time limit is 30 minutes. How long and how big it may be
  is #50.
- Nothing calls the package yet. The settings that name the repository, the branch and the
  credentials come in the next change, and the agent runs it after the volume checks in the
  one after.
- Still to do elsewhere: the recovery ref and the cap on deletions are #40, the pull is #33,
  and the check of what is in the Git directory is #161.
