# 0022. The agent decides what it syncs with gitignore patterns of its own

- Status: accepted
- Date: 2026-11-15
- Issue: #43

## Context

The agent is going to commit what the application changes on the volume (#34, #36). FR-9 asks it to
respect `.gitignore` and the include and exclude patterns of `spec.paths`. R-1 is the risk behind it:
an application writes a token or a password into a file, and the agent commits and pushes it. Its
mitigation is a set of excludes that the agent enforces from its own rules, and not only from a
`.gitignore`, which belongs to the repository and to the application, and which either can change.

Git runs only through the Runner (ADR 0013), which points `core.excludesFile` at `/dev/null`, and the
agent refuses a Git directory with anything in `info/` (ADR 0019), so `info/exclude` is never read.
The CRD takes up to 64 patterns in each list, of 1 to 512 characters, relative and without `..`.

## Decision

### Who decides

`internal/agent/paths` decides, in Go. The caller asks Git which paths changed with one
`git status --porcelain=v2 -z --untracked-files=all`, which lists the tracked files that changed and the
untracked files that no `.gitignore` hides, asks `Filter.Reason` about each path, and stages only those
that are `Synced`. It gives them to Git as literal paths (`:(literal)`), because `git add` reads its
arguments as pathspecs, in which `*`, `?` and `[` are wildcards. The patterns are not turned into
pathspecs: those have rules of their own, and the file events of #34 and the blob check of #94 need the
same answer in Go before Git is asked.

### What `.gitignore` means

What it means to Git. It hides an untracked file, and a tracked file that changes is listed whatever a
`.gitignore` says. Only the `.gitignore` files of the work tree count. An include cannot bring back a
file that a `.gitignore` hides, because status does not list it.

### The patterns

A pattern of `spec.paths` has the syntax and the meaning of a line of a `.gitignore` file, so that the
two mean the same: `*`, `?`, a class in `[...]`, `\` and `**`. A pattern without a slash matches a name
at any depth, one with a slash is relative to the synced directory, one with a slash at the end matches
only a directory, and a directory that matches takes every file in it. The patterns are a list and not
a file, so `#` and spaces at the end are plain, and there is no negation. Matching is on bytes, as in
Git, and takes time in proportion to the length of the pattern times that of the path. With
`Options.FoldCase` an ASCII letter matches in either case, as Git does with `core.ignoreCase`; where Git
then matches nothing for an upper-case letter in a class or after `\`, the filter matches it in either
case. A test holds the meaning to Git's on a corpus of files.

`Check` refuses a pattern that the CRD refuses, that Git would never match, that would mean something
else than in a `.gitignore`, or that says nothing a simpler one does not: an empty pattern, a NUL, more
than 512 characters, a leading `/` or `!`, an empty, `.` or `..` segment, a `\` at the end or before a
`/`, a class that is not closed, a `[` or a `/` inside a class, which leaves out classes like
`[:alpha:]`, and a `**` that is not a whole segment, which Git's documentation reads as `*` and releases of Git
read differently (`ab**/c` takes `abx/y/c` in Git 2.34 and does not in 2.56). The CRD is
to refuse the same, in the next change, with a test that holds the two together.

### The rules, in order

`Filter.Reason` gives the first rule that leaves a path out, or `Synced`:

| Reason | The path | Why |
| --- | --- | --- |
| `NestedRepository` | ends with a slash | Status lists a directory only when it is a repository of its own, and `git add` would stage it as a gitlink, a submodule by another name (R-23). |
| `GitPath` | has a segment that is `.git` in any case, or `.git` or `git~1` followed by dots or spaces, or by a `:` or a `\` and anything | Git refuses these names on Linux too (`core.protectNTFS`), and one such file makes `git add -A` fail and stage nothing. |
| `StateDirectory` | starts with the state directory, in any case, when it is in the work tree (`layout.Layout.NeverSynced`) | Without a `subPath` the Git directory is `.treebind/git` in the work tree, and status lists its files. |
| `Transient` | is a temporary, swap or lock file (`transient.IsTransient`, #35) | An editor or an application leaves it beside the real file for a moment (R-10). |
| `Secret` | has a segment that is a secret by its name, in any case | R-1, below. |
| `NotIncluded` | is taken by no include pattern, when there are some | The include list of R-1. |
| `Excluded` | is taken by an exclude pattern | |

So an exclude wins over an include, and neither can bring back a path that the rules above them leave
out. The reasons are for the agent's log and its events. Once something reports them they are API in
the sense of ADR 0004.

The secret names are `.git-credentials`, `.netrc`, `_netrc` and `.pgpass`, `.env` and `.env.*` except
`.env.example`, `.env.sample` and `.env.template`, which a repository shares on purpose, the private ssh
keys `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, `id_ecdsa_sk` and `id_ed25519_sk`, and the names
that end with `.key`, `.p12`, `.pfx`, `.jks` or `.keystore`. A directory with such a name is left out
with everything in it, as for a pattern without a slash. `*.pem` is not on the list: a certificate is
public, and a private key in a PEM file, like a secret in a file with any other name, is for the scan
before a push (#65). The exception for the templates is code and not a pattern, because patterns have no
negation.

### What the caller passes

Each path that status lists, as it lists it: relative to the synced directory, with slashes, and with a
slash at the end for a directory. That is every change the agent would stage, a new file, an edit or a
deletion, whether Git tracks the file or not, and both paths of a rename. A symbolic link named
`.gitmodules` makes `git add -A` fail too, and a path does not say whether it is a link, so the stager
leaves out such a link itself.

### What is not filtered

The pull. The checkout and the pull write the branch as it is (ADR 0017, ADR 0020). Git's way to leave
files out of a checkout is sparse checkout, which reads its patterns from `info/sparse-checkout`, and
`info/` stays empty (ADR 0019). A volume that holds exactly the branch is also what the adoption of a
checkout and `lastSyncedCommit` rely on.

## Consequences

- A template that the repository tracks and the application fills with a password is never committed.
  An excluded file that is tracked and edited on the volume blocks the pulls that change it (`Blocked`,
  ADR 0020), until the edit is undone or the file is no longer excluded. Removing it from the repository
  does not help: a pull that deletes a file with a local edit is refused as well (ADR 0020).
- A path is decided on its own, so one file that Git would refuse is left out and the rest is committed.
- A single name cannot be matched at the top of the synced directory only, because a leading `/` is
  refused. Classes like `[:alpha:]` and a `**` inside a name are refused; `[a-zA-Z]` and `*` do the same.
- The secret names are a fixed list, and nothing in the spec turns them off. A field for that would be
  added to the API, not changed in it.
- Until the CRD has the rules of `Check`, the API server takes patterns that the agent refuses. Nothing
  reads `spec.paths` yet, and the next change of #43 closes the gap.
- Nothing calls the filter yet: the scan of #34 and the commit of #36 will, and #94 adds the live
  database files to the rules.
