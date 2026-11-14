# 0013. Git runs only through a Runner with a fixed configuration

- Status: accepted
- Date: 2026-11-13
- Issue: #81

## Context

The agent runs Git on a volume that an application can write (NFR-15). Git runs
programs and reads files on the strength of what it is configured to: hooks, a
credential helper, an ssh command, a filter, a signing program, a file system
monitor, an ext:: address, a URL rewrite, an alias. It reads that configuration
from the repository's own `config`, from `.gitattributes` in the work tree, from
the user's and the system's files and from the environment. Whatever an
application can write there runs with the agent's credentials (R-17), and a
symlink or a submodule that the application plants turns the agent into a confused
deputy (R-23).

This is also the "timeout on every Git call" half of #77. The reachability probe
and its condition stay there.

## Decision

`internal/agent/git` has a `Runner`, and it is the only code that starts the
`git` binary. The caller gives it the Git directory and the work tree (where the
Git directory lives is decided in ADR 0016), and for ssh the key and the known hosts
file. Every command
gets the same configuration on its command line (`-c`), an environment that is
built from nothing, and only the subcommands the agent needs.

**The environment** holds a fixed `PATH`, `HOME=/nonexistent`, `LC_ALL=C`,
`GIT_DIR` and `GIT_WORK_TREE` as given, `GIT_CONFIG_GLOBAL` and
`GIT_CONFIG_SYSTEM` set to `/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_EDITOR=:`,
`GIT_PAGER=cat`, `GIT_SSH_COMMAND`, `GIT_ALLOW_PROTOCOL=ssh:https`,
`GIT_ATTR_SOURCE` set to the empty tree, and `GIT_SSL_CAINFO`, the CA bundle that
was given or else the system's, found in the usual places. Nothing else is inherited, so no
`GIT_*` variable, proxy, trace or askpass of the agent's own environment reaches
Git.

**Where the Git directory is.** A config that an application can write is the
weakest point of everything below (see the audit). The Runner therefore refuses a
Git directory that is inside the work tree, with the links in both paths followed,
unless the caller sets `AllowGitDirInWorkTree`, which is not a default. ADR 0016
keeps the Git directory beside the synced directory, and inside the work tree only
for a Treebind without a `subPath`, which `layout.Layout.GitDirInWorkTree` says; the
sync code that runs Git (#32) sets the option from it. The Runner does
not check who can write the directory: the agent runs with umask 0002 so that
files are group-writable on purpose (R-12), and it does not know the application's
user.

**Allowed subcommands** are the ones the sync needs: `add`, `cat-file`,
`checkout`, `checkout-index`, `commit`, `commit-tree`, `diff-index`, `diff-tree`,
`fetch`, `for-each-ref`, `hash-object`, `init`, `ls-files`, `ls-remote`,
`ls-tree`, `merge`, `merge-base`, `push`, `read-tree`, `reset`, `rev-list`,
`rev-parse`, `show-ref`, `status`, `symbolic-ref`, `update-index`, `update-ref`,
`version` and `write-tree`. A clone is `init`, `fetch` and `checkout`. A caller
that needs another one adds it in the same change that uses it, where it is
reviewed. The list is also why the first argument can never be a global option
like `-c` or `--exec-path`. Left out on purpose: `diff`, `log` and `show` (external
diff programs, text converters), `submodule`, `clone` and `pull` (submodules),
`gc` and `maintenance`, `tag`, `rebase`, and everything that starts an editor or a
merge tool.

**Options that sign, verify, run a converter or name a command are refused** for the
commands that take them: `-S` and `--gpg-sign` (also as a start of the name, `--gpg`,
and in a cluster, `-qS`) for `commit`, `commit-tree` and `merge`, `--verify-signatures`
for `merge`, `--trailer` for `commit`, `--signed`, `--receive-pack` and `--exec` for
`push`, `--upload-pack` and `--exec` for `fetch` and `ls-remote`, `--ext-diff`,
`--textconv` and `--output` for `diff-index` and `diff-tree`, `--textconv` and
`--filters` for `cat-file`. Nothing after `--` is an option, and what follows a short
option that takes a value in the same word is that value (`-mSync`). The Runner does
not vet any other option: the arguments are the agent's own, and one that comes from
outside is put after `--` or given as `--name=value`, which is how a text that starts
with a dash is given (`--message=<text>`).

**ssh** runs as `ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes
-o UpdateHostKeys=no -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile=<file>
-o IdentitiesOnly=yes -i <key>`. The host is verified only against the known hosts
file that was given (ADR 0005), and with none given the file is `/dev/null`, so
every connection fails. Nothing is read from the system's or the user's ssh
configuration, and with no key given the identity file is `/dev/null`, because
`IdentitiesOnly` leaves the default identity files in use and ssh finds those
through the passwd file and not through `HOME`.

**Every call has a timeout**, a default of two minutes that the Runner can change
and a call can override, for a fetch of a large repository. When it ends, or the
caller's context does, the whole process group is killed, so an ssh that Git
started does not outlive the call. A child that left the group cannot hold the
call either: Wait gives up on its pipes after two seconds, and a Git that ended
with status 0 is a success then. The standard output is bounded (16 MiB by
default) and a command that writes more is stopped and fails, because a short
answer would be taken for a whole one. Of the standard error the last 64 KiB are
kept.

**Errors** have fixed text that names the subcommand and what happened, never the
arguments, and end with the last lines of the standard error, on one line. The
userinfo and the query of any URL in it are replaced by `<redacted>`, up to the last
`@` of the word because a password may hold a `/` that Git prints as it is, and a
line that the 64 KiB limit cut is dropped whole, so that no part of a URL is left
with its credential and without its scheme. The standard output is returned as it
was written, because it is data.

**Symlinks are not followed** means three things:

1. Git never reads through a link in the work tree. It stages a link as the link,
   the path that it points to, and refuses a path behind one ("beyond a symbolic
   link"). It does not write through one on checkout, and it does not follow a
   `.gitignore` that is a link (Git 2.32 and later). The Runner pins
   `core.symlinks=true`, `core.attributesFile` and `core.excludesFile`, so that a
   repository config cannot change that or make Git read a file of its choosing.
2. A staged blob is the bytes of the file (#94): `core.autocrlf=false`, and no
   attribute from the work tree applies.
3. Reading and writing files outside Git stays with the file layer that confines
   every operation to the volume root (CONTRIBUTING). A link that came from the
   repository is checked out as a link, so that layer has to meet it and refuse
   it.

**`safe.directory`** is set to the work tree and nothing else, never `*`. It is a
second line: Git does not check who owns a directory when `GIT_DIR` is set, which
the Runner always does, so the value is read only by a command that looks for its
repository from the current directory. The test makes Git see every directory as
owned by someone else (`GIT_TEST_ASSUME_DIFFERENT_OWNER`) and shows that the work
tree is accepted and another directory is not. A directory that another user really
owns cannot be had in a test that does not run as root.

## The audit: every key that makes Git run a program or read a file of the repository's choosing

Each trap is planted with plain Git first, to show that it springs, and then the
Runner runs the command, where it must not. Each runs twice, with the Git directory
next to the work tree and inside it. "Name" means that the repository chooses the
name of the key, so no `-c` can list it.

| Key or file | Neutralised by | Test |
| :-- | :-- | :-- |
| `core.hooksPath`, `$GIT_DIR/hooks` | `-c core.hooksPath=/dev/null`; also from an included file | post-checkout, pre-commit, post-commit, post-merge; the key in the config, through `include.path` and through `includeIf` |
| `core.fsmonitor` | `-c` with an empty value | status |
| `core.sshCommand`, ssh's own config | `GIT_SSH_COMMAND` comes before it; `-F /dev/null` | ls-remote; `ssh -G` of the command |
| `core.askPass`, `GIT_ASKPASS`, `SSH_ASKPASS` | `-c` empty, environment from nothing | askpass trap on a 401 |
| `credential.helper`, `credential.<url>.helper` | an empty `credential.helper` resets every helper set before it, scoped to a URL or not, because Git clears its list at any scope | both together, and a helper scoped to the URL and no other |
| `core.editor`, `sequence.editor` | `GIT_EDITOR=:` comes before `core.editor`; `sequence.editor` is only read by `rebase -i`, which is not allowed | commit with no message |
| `core.pager`, `pager.<command>` | never started: Git pages only to a terminal, and the Runner's output is a pipe; the caller cannot pass `--paginate`; `GIT_PAGER=cat` | none: a trap cannot be armed without a terminal |
| `diff.external`, `diff.<name>.textconv`, `.command` | `diff`, `log` and `show` are not allowed; `diff-index` and `diff-tree` run none of them unless given `--ext-diff` or `--textconv`, which are refused; the attributes that select a driver are empty | all three planted, `git diff` springs them, `diff-index -p` does not |
| `merge.<name>.driver` (name) | selected only by an attribute, and those come from an empty tree and `core.attributesFile=/dev/null` | a driver selected from the work tree's `.gitattributes` and one from `core.attributesFile`, on a merge |
| `filter.<name>.clean`, `.smudge`, `.process` (name) | same | add, checkout, both from `.gitattributes` and from `core.attributesFile` |
| `$GIT_DIR/info/attributes` | **not neutralised**: Git reads it whatever `GIT_ATTR_SOURCE` says | none; see below |
| `core.gitProxy` | used only for `git://`, which is refused before Git connects | ls-remote `git://` |
| `url.<base>.insteadOf`, `.pushInsteadOf` (name) | a rewrite to `file`, `git`, `ext` or `http` is refused, because the allowed protocols are checked after the rewrite. **Not neutralised** for a rewrite to another `ssh` or `https` host, which cannot be listed or reset | ext:: and file:// for fetch, ext:: for push |
| `include.path`, `includeIf` | an included file is read in place, so each key in it is overridden like any other | `core.hooksPath` through both |
| `uploadpack.*`, `receive.*`, `remote.<name>.uploadpack`, `.receivepack`, server hooks | run on the server, or locally only through the `file` transport, which is refused; the Runner is given URLs, not names of remotes | file:// refused |
| `alias.<name>` (name) | Git runs a built-in before it looks for an alias, and a subcommand that is not allowed is refused before Git starts | an alias for each allowed name, and one for a name that is not |
| `commit.gpgSign`, `tag.gpgSign`, `push.gpgSign`, `merge.verifySignatures`, `gpg.format`, `gpg.program`, `gpg.openpgp.program`, `gpg.ssh.program`, `gpg.x509.program`, `gpg.ssh.defaultKeyCommand` | `-c`: signing and verifying off, the format fixed, every program `/dev/null` | commit and merge with a program planted; a commit that asks for `-S` runs neither `gpg.program` nor `gpg.openpgp.program`; the options above are refused |
| `gc.auto`, `maintenance.auto` | `-c gc.auto=0`, `-c maintenance.auto=false`: a commit, merge or fetch starts no gc, which would detach from the call and from its process group | a commit with two packs and a limit of one |
| `init.templateDir` | not needed: Git 2.54.0 reads it only from the user's, the system's and the command line's config, so a repository config cannot set it. Git 2.34.1, below the floor, does read it | tried by hand on both |
| `http.sslVerify`, `http.proxy`, `http.extraHeader` | `-c` | a certificate, a proxy and a header that the config names |
| `http.sslCAInfo` | `GIT_SSL_CAINFO` comes after it: the given bundle, or the system's found at start. An empty `-c http.sslCAInfo=` is not an option: it makes Git fail on every host ("error adding trust anchors from file", tried on 2.54.0, 2.56.1 and 2.34.1) | a bundle that the config names, with a system bundle |
| `http.sslCAPath` | **not neutralised**: the same empty value breaks https, and `GIT_SSL_CAPATH` would need a directory of our own | none; see below |
| `http.<url>.*` (the URL) | **not neutralised**: a key scoped to a URL beats the same key from `-c`. Tried by hand on Git 2.34.1 with `http.<url>.sslVerify=false` | none; see below |
| `core.worktree` | `GIT_WORK_TREE` comes before it | `rev-parse --show-toplevel` |
| `core.symlinks`, `core.autocrlf`, `core.excludesFile`, `core.attributesFile` | `-c` | a link checked out as a link, a CRLF file staged as it is, an excludes file that is not read |
| `submodule.recurse`, `fetch.recurseSubmodules`, `diff.ignoreSubmodules` | `-c`; `submodule`, `clone` and `pull` are not allowed | fetch, reset, status in a repository with a populated submodule whose remote is reachable |
| `protocol.<name>.allow` | `GIT_ALLOW_PROTOCOL` replaces all of it; `-c protocol.allow=never` and an allow for the two as a second line | refused with a config that allows everything, and with the variable removed |
| `trailer.<token>.command` | only read for `commit --trailer`, which is refused | the option is refused |

**What protects against a key whose name is not known in advance** (`filter.*`,
`merge.*.driver`, `diff.*.textconv`, `alias.*`, `url.*`): not a `-c`, which cannot
list it. It is the thing that selects it: a filter, a diff driver or a merge driver
does nothing unless an attribute names it, and the Runner reads attributes only from
an empty tree and from `/dev/null`; an alias is never consulted for a name that the
allowlist lets through. What is left is `$GIT_DIR/info/attributes`, a rewrite of a
URL to another `ssh` or `https` host, a key scoped to a URL and `http.sslCAPath`,
which nothing in the Runner can neutralise. All three are files or keys inside the Git directory,
and the protection is that **the Git directory, and so its config, is not writable
by the application** (ADR 0016, R-17). The Runner enforces the part that it can see: a
Git directory inside the work tree is refused unless the caller allows it. ADR 0016
puts the Git directory inside what the application can see only for a Treebind without
a `subPath`, and for that case a check that refuses to run on a config with keys
outside a short list is the next step.

**Git versions.** The image carries Git 2.56.1 and enforces 2.50.1 as the floor
(`MIN_GIT` in `hack/check-agent-image.sh`, ADR 0010). The newest option the
Runner uses is `GIT_ATTR_SOURCE`, Git 2.40, and `CheckVersion` refuses an older
Git at start, because Git ignores what it does not know and the protections would
be missing without a failure. `core.hooksPath` is from 2.9, `submodule.recurse`
from 2.14, `protocol.allow` from 2.12 (`GIT_ALLOW_PROTOCOL` is older),
`GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` from 2.32, `gpg.ssh.defaultKeyCommand`
from 2.34 and `safe.directory` from 2.35.2. The tests ran against Git 2.56.1 in the
agent image (as UID 65532 on a read-only root filesystem), 2.54.0 and 2.34.1. The
tests that need `GIT_ATTR_SOURCE` skip on a Git older than 2.40.

## Consequences

- Nothing calls the Runner yet. The sync code (#32, #33, #36 and others) will, and
  the Git ownership check of #31 can use it as it is. It calls `CheckVersion` once.
- **The repository's `config` and `info/` must not be writable by an application.**
  See the audit. With a `subPath`, and an application that mounts only that, the
  Git directory is out of its reach (ADR 0016). For a Treebind without a `subPath`
  the four gaps in the audit are open until the check of the config keys exists.
- The API admits `http://` repositories (the CRD accepts `ssh`, `https` and
  `http`), and the Runner refuses them, as NFR-15 says. One of the two has to
  change before an `http://` repository can sync.
- The integration tests of #52 use a local bare repository, which is a `file`
  URL. They can use the ssh transport with a stand-in for `ssh`, as the tests of
  this package do.
- Only SHA-1 repositories work: the empty tree in `GIT_ATTR_SOURCE` is the SHA-1
  one, and a SHA-256 repository would fail closed.
- A subcommand the agent does not use cannot be run by accident, and a new one is
  a small, reviewed change.
- The standard output is not redacted, because it is data. `Redact` is exported for
  a caller that logs it.
- `CheckVersion` is called by the agent at start and not by `New`, so that a
  machine with an older Git can still run the code that does not need one, as the
  tests do. The image enforces the floor (ADR 0010).
- The tests need `git`, and some need `ssh` or `setsid` and skip without them.
  The agent image has no `touch`, `cat`, `sleep` or `basename`, so the tests only
  use what a shell has built in, except the three that prepare a submodule and the
  ones that need `setsid`, which skip there.
