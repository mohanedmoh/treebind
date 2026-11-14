# 0019. The agent refuses a Git directory that holds more than it put there

- Status: accepted
- Date: 2026-11-14
- Issue: #161

## Context

The Runner passes a fixed configuration to every Git command (ADR 0013), but four things
in a repository's own files cannot be neutralised from the command line: `info/attributes`,
a `url.<base>.insteadOf` to another ssh or https host, a key scoped to a URL such as
`http.<url>.sslVerify`, and `http.sslCAPath`. A key scoped to a URL beats the same key
from `-c`, so an override of ours does not close it. Other files make Git read configuration
or objects from a place that is not the Git directory: `commondir`, `objects/info/alternates`,
`shallow`, an include. The protection that ADR 0013 relies on is that the application cannot
write the Git directory.

ADR 0016 gives that protection for a Treebind with a `subPath`, where the application
mounts only that directory and never sees `.treebind`. A Treebind without a `subPath` has
the Git directory inside what the application mounts. It is mode 0700 and the agent's, which
keeps out an application that runs as another user, and not one that runs as the agent's
own user or as root.

## Decision

The agent looks at the Git directory before it starts Git, and refuses to start it when
what it finds is not what the agent makes. The check is `git.VerifyGitDir`, and the Runner
runs it before every command when `Options.VerifyGitDir` is set, which the checkout sets. It
is in the Runner and not in the callers, so that no command can forget it. A refusal is an
`*git.GitDirError` that names the file and what is wrong with it, and `errors.Is(err,
git.ErrGitDirUntrusted)` is true.

It is an allowlist, because a file that a later Git reads and that this check does not know
of is the case an allowlist is for. A refusal names the file, and the list grows by one name
when Git turns out to write one.

- **The entries at the top of the Git directory** are the ones that `init` with an empty
  template and the subcommands the Runner allows write: `HEAD`, `config`, `index`,
  `FETCH_HEAD`, `ORIG_HEAD`, `COMMIT_EDITMSG`, `MERGE_HEAD`, `MERGE_MSG`, `MERGE_MODE`,
  `AUTO_MERGE`, `objects`, `refs`, `logs`, `hooks` and `info`, and a `.lock` file. A link is
  refused in any of them. `hooks` and `info` have to be empty, which covers `info/attributes`,
  `info/exclude` and `info/grafts`. Anything else is refused, among it `commondir`, `gitdir`,
  `shallow`, `packed-refs`, `worktrees`, `modules` and `config.worktree`, which send Git
  somewhere else or change the history it sees.
- **`objects/info/alternates` and `objects/info/http-alternates`** have to be absent.
- **The config** is read by a small parser that takes what `git init` writes. The sections are
  `core` and `extensions`, with no subsection, and the keys are `repositoryformatversion`,
  `filemode`, `bare`, `logallrefupdates`, `worktree`, `symlinks`, `ignorecase`,
  `precomposeunicode` and, in `extensions`, `objectformat`. Each has a check of its value:
  `core.worktree` is the work tree, `core.bare` is false, the object format is `sha1` or
  `sha256`. A subsection, a continued line, a setting with no value, a larger file than a few
  lines or any other key is refused with its name and line. That covers `url.<base>.insteadOf`,
  `http.<url>.*`, `http.sslCAPath`, `core.sshCommand`, an include and an alias at once.

Replace refs are not looked at but switched off, with `GIT_NO_REPLACE_OBJECTS=1` in the
environment of every command, because one can be in the refs or in `packed-refs`, and
Git would then see another object where a commit, a tree or a blob is.

The preflight check `GitDir` is to run the same function after the directory checks, so that
the refusal is reported as `GitDirUnsafe` with the file in the agent's log and status report,
and the agent keeps checking and carries on once it is put right, and the checkout is to run
it first and return the same reason. Both are the next change; this one adds the check and the
option.

## Consequences

- The four gaps of ADR 0013 are closed for a Treebind without a `subPath` as well, for an
  application that cannot write inside the Git directory. For one that can, which is one that
  runs as the agent's own user or as root, the check cannot do more than shorten the window:
  a change between the look and the start of Git is not ruled out, and the application could
  write what it likes into a file that Git reads after the check.
- Git is not run at all against a Git directory that was not made with an empty template. A
  directory made with the default template has hooks, an `info/exclude` and a description, and
  is refused. The checkout makes its own with an empty one.
- A new release of Git that writes a file at the top of the Git directory that is not on the
  list makes the agent refuse the directory, with the name of the file, until the name is added.
  The tests run the subcommands that write there, `fetch`, `update-ref`, `read-tree`, `add`,
  `commit` and `checkout`, and check that the directory is accepted afterwards, so the day it
  happens is the day a test says so.
- The objects and the refs are not looked at, and the check does not read what is inside a
  `logs` file or `index`, which Git parses as data and never runs.
