# Releasing

Releases are cut from `main` by pushing an annotated tag. The
[release workflow](../.github/workflows/release.yml) does the rest. Only the
repository owner can create or move release tags, and, like every workflow, the
release waits for the owner's approval before it starts.

## Versions and tags

| What | Rule | Example |
| :-- | :-- | :-- |
| Release tag | `vMAJOR.MINOR.PATCH`, annotated, created from `main` | `v0.1.0` |
| Pre-release | `-alpha.N`, `-beta.N` or `-rc.N`, marked as a pre-release | `v0.1.0-rc.1` |
| One version | Both images, the Helm chart (version and appVersion, without the `v`) and `install.yaml` carry the same version | chart `0.1.0`, images `v0.1.0` |

While the API is `v1alpha1`, a minor bump may break and a patch is fixes only.
Never delete or move a published tag. If a release is wrong, publish a fixed
patch and say so in the notes of the bad release.

## Release checklist

1. Every issue in the milestone is closed or moved, and `main` is green.
2. Open the draft release notes (see [release.yml](../.github/release.yml) for
   how pull requests are sorted). Each breaking change says what to do instead.
3. Create an annotated tag on `main` and push it:
   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```
4. Approve the **Release** run on the Actions tab (**Review pending
   deployments**, then `ci`), and watch it finish.
5. Verify the signatures (below).
6. On a clean kind cluster, install once from `install.yaml` and once from the
   Helm chart, and apply the sample.
7. Announce in Discussions with the release link and any upgrade notes.

## What the workflow does

1. Checks that the tag is `v` plus a semantic version and that its commit is on
   `main`.
2. Runs lint, the tests and the end-to-end suite again on the tagged commit.
3. Checks Chainguard's signature on the agent's base image, then builds the
   operator and agent images with [ko](https://ko.build) for
   `linux/amd64` and `linux/arm64`, and pushes them to
   `ghcr.io/mohanedmoh/treebind-operator` and `ghcr.io/mohanedmoh/treebind-agent`,
   tagged with the version and referred to by digest. ko attaches an SPDX SBOM to
   each image.
4. Signs every image digest with cosign, keyless, using the workflow's identity.
5. Packages the chart with the release version, pushes it to
   `oci://ghcr.io/mohanedmoh/charts`, and signs it.
6. Renders `install.yaml` from `config/default` with the image digests filled in.
7. Creates a **draft** GitHub release with notes generated from pull request
   labels, attaches `install.yaml`, the SBOMs and `checksums.txt`, and only then
   publishes it. A release that is already published cannot change once
   immutable releases are on, which is why the order matters. A tag with a suffix
   such as `-rc.1` is marked as a pre-release.

## Verify a release

Images and the chart are signed by the release workflow's identity. Replace the
digest with the one from the release page:

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/mohanedmoh/treebind/\.github/workflows/release\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/mohanedmoh/treebind-operator@sha256:<digest>
```

The same command works for `treebind-agent` and for
`ghcr.io/mohanedmoh/charts/treebind@sha256:<digest>`. Check the downloaded
assets against `checksums.txt`:

```sh
sha256sum --check --ignore-missing checksums.txt
```

Pin images by digest in production. The tag is a convenience and can be
re-pointed by whoever can push to the registry; the digest cannot.

## The agent image

The agent runs Git and ssh, so unlike the operator it is not built on distroless.
`.ko.yaml` gives it Chainguard's `cgr.dev/chainguard/git` as its base, pinned by
the digest of its multi-platform index. The image runs as UID 65532 with a passwd
entry and carries Git, OpenSSH and the CA certificates. For every package in it
there is an SPDX SBOM under `/var/lib/db/sbom`, with the license and the source
of the package. Git is GPL-2.0-only. The reasons for this base, and what they
cost, are in [ADR 0010](adr/0010-the-agent-image-is-built-on-chainguards-git-image.md).

Two scripts keep it honest. CI runs both on every pull request, and the release
runs the first before it publishes:

- `hack/verify-agent-base.sh` checks that Chainguard signed the digest in
  `.ko.yaml`. It needs cosign.
- `hack/check-agent-image.sh <image>` runs the agent image with the settings of
  the sync pod, a read-only root filesystem, no capabilities and no privilege
  escalation. It checks that the user is numeric and not root, that Git is not
  older than `MIN_GIT`, that ssh finds the user in the passwd file, and that Git
  can make a commit. `make check-agent-image` builds the image and runs it.

### When Git ships a security release

The agent carries the Git of its base image, so a Git fix reaches users in a new
release of the agent:

1. Look up the digest of the current image. Chainguard rebuilds the image as its
   packages are updated. The `Digest:` line of the index is the one to pin:
   ```sh
   docker buildx imagetools inspect cgr.dev/chainguard/git:latest
   ```
2. Put it in `.ko.yaml`, under `baseImageOverrides`.
3. Raise `MIN_GIT` in `hack/check-agent-image.sh` to the release that has the fix,
   so that a base with an older Git fails.
4. Run `hack/verify-agent-base.sh` and `make check-agent-image`, and open the pull
   request.
5. After it merges, cut a patch release. A published image never changes.

## Packages

The workflow publishes three GHCR packages: `treebind-operator`,
`treebind-agent` and `charts/treebind`. A package that a workflow creates is
linked to this repository and takes its visibility, so while the repository is
public the packages are public too. After the first release, check each one
under your profile's **Packages**. If one is private, open it, then **Package
settings**, then **Change visibility**, then **Public**; GHCR has no API for
that setting.

Right after GHCR creates a new package it can briefly refuse the next write, so
the workflow retries registry writes (the image builds, the chart push and the
signatures) with a growing delay.

## Maintainers: changing the release tools

The release workflow downloads ko, cosign and Helm at pinned versions and checks
each download against a pinned SHA-256. To update one, change its version and
checksum together in `release.yml`, and run the release against a throwaway
pre-release tag first.
