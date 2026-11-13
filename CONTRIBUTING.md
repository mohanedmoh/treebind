# Contributing to Treebind

## Welcome

Treebind keeps a Git repository and an application's files in sync through a
shared volume. Code, docs, tests, storage reports and bug reports all help. You
do not need to know Kubernetes internals to start: the issues labelled
`good first issue` are written so that you can finish them from the issue text
alone.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Find something to work on

- Browse issues labelled `good first issue` for a first change, or
  `help wanted` for larger ones.
- Comment on the issue and wait for a maintainer to assign it to you. This stops
  two people doing the same work.
- An assigned issue with no activity for 14 days goes back to the pool. Say so
  if you need more time.
- Open an issue and agree on the approach before you write code if the change
  touches the API (`api/`), adds a dependency, or will be more than about 200
  lines.
- Questions and ideas go to
  [GitHub Discussions](https://github.com/mohanedmoh/treebind/discussions), not
  to issues.

## Make a change

1. Fork the repository and clone your fork. Follow
   [docs/development.md](docs/development.md) to get a working setup.
2. Create a branch named `<type>/<issue>-<short-description>`, for example
   `fix/42-lock-heartbeat-race`.
3. Make the change, with tests and any doc updates.
4. Run `make lint test`, and `make generate manifests` if you touched `api/`.
5. Commit with a sign-off: `git commit -s`.
6. Push to your fork and open a pull request against `main`. Put
   `Fixes #<issue>` in the description.
7. Open it as a draft until CI is green, then mark it ready.

## Commit messages and pull request titles

We use [Conventional Commits](https://www.conventionalcommits.org). The pull
request title is what becomes the commit on `main`, because pull requests are
squash-merged, so it must follow the format too. CI checks it.

```
type(scope): short summary in the imperative
```

- Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`,
  `chore`.
- Scopes follow the area labels: `operator`, `agent`, `api`, `git`, `volume`,
  `lock`, `chart`, `adapters`, `ci`, `docs`.
- Keep the summary within 72 characters.
- Mark a breaking change with `!` after the scope and add a `BREAKING CHANGE:`
  line in the body that says what to do instead.

## Sign-off (DCO)

Every commit needs a `Signed-off-by: Your Name <you@example.com>` line, which
`git commit -s` adds. It certifies the
[Developer Certificate of Origin](https://developercertificate.org): that you
wrote the change or have the right to submit it under the project's licence. A
bot checks it, and the pull request cannot merge without it. To add it
afterwards, run `git rebase --signoff upstream/main` and force-push your branch.

## Pull request checklist

- The change does one thing. Aim for no more than about 400 changed lines, not
  counting generated code. Split larger work into a series.
- Tests cover the change. A bug fix includes a test that fails without the fix.
- Docs and the sample manifest are updated if behaviour changed.
- `make generate manifests` leaves no diff.
- A breaking change carries the `breaking-change` label and a line for the
  release notes.

## Review

- A maintainer gives a first response within three working days. If you hear
  nothing after that, mention the maintainers in a comment.
- Reply to each review comment, or push a change and resolve the thread. All
  conversations must be resolved before the merge.
- Push new commits while the review is going on. The squash merge tidies the
  history, so you do not need to rewrite it.
- Disagreement is fine. If a review comment is a preference and not a
  requirement, the reviewer will say so.

## Code standards

- Run `gofmt` and `goimports`. golangci-lint settings are in `.golangci.yml` and
  CI enforces them.
- Wrap errors with `%w` and add context at each layer. Do not panic in reconcile
  or agent loops.
- Use structured logging. Never log a credential, token, or a URL that carries
  one.
- Run Git only through `internal/agent/git`. No other package calls the `git` binary.
- Confine every file operation to the volume root and reject paths that escape
  it, including through symlinks.
- Write table-driven tests for logic with many cases.
- Justify a new dependency in the pull request. Prefer the standard library and
  the Kubernetes libraries already in use.
- Keep comments for the why. The code says the what.

## Tests

| Kind | Where | Use it for |
| :-- | :-- | :-- |
| Unit | Next to the code | Logic with no cluster |
| envtest | `internal/controller` | Reconciler behaviour against a real API server |
| End-to-end | `test/e2e` | The whole sync loop on kind |
| Failure injection | `test/e2e` | Crashes, conflicts, a lost lock, a full disk |
| Storage conformance (planned) | `test/conformance` | A storage class against the sync guarantees. Once it exists, reports are welcome: run `make conformance STORAGE_CLASS=<name>` and open a conformance report issue. |

## Changing the API

The Treebind API is the project's contract with its users.

- Open an issue first. A change that adds, renames or removes a field needs a
  short written decision in `docs/adr/`.
- New fields get validation (CEL rules or markers), a default where one makes
  sense, documentation and a sample.
- While the API is `v1alpha1` a breaking change is allowed but needs the
  `breaking-change` label and a release note. From phase 2 on, changes within a
  released version must stay backward compatible.

## Security

Do not open a public issue for a vulnerability. Use the private report form
under the Security tab, as described in [SECURITY.md](SECURITY.md). Expect an
acknowledgement within three working days.

## Licensing and clean-room rules

Treebind is Apache-2.0 and written from scratch.

- Submit only code you wrote or have the right to license under Apache-2.0.
- Do not copy code, history, data, credentials or configuration from a
  proprietary or employer project, including any earlier private implementation
  of a similar idea.
- If you include code from another Apache-2.0-compatible project, say so in the
  pull request and add the attribution to [NOTICE](NOTICE).
- You are responsible for everything you submit. Check it as you would your own
  work.

The full rules are in [docs/clean-room.md](docs/clean-room.md).

## Getting help

Ask in
[GitHub Discussions](https://github.com/mohanedmoh/treebind/discussions). For
anything about conduct, use the contact in
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Thank you for helping.
