# 0010. The agent image is built on Chainguard's Git image

- Status: accepted
- Date: 2026-11-12
- Issue: #84

## Context

The agent runs Git and ssh. It has to run as a non-root user under the
`restricted` Pod Security level (NFR-4), on a read-only root filesystem, and it
has to carry a current Git that is replaced when Git ships a security fix
(NFR-14, R-42). Both images were built on distroless `static`, which has no Git
and no ssh. ssh also refuses to run for a user that has no passwd entry ("No user
exists for uid"), so the user the agent runs as needs one.

## Decision

The operator stays on distroless. The agent is built on `cgr.dev/chainguard/git`,
pinned by the digest of its multi-platform index, through ko's
`baseImageOverrides` for `cmd/agent` in `.ko.yaml`. Built and run as the sync pod
runs it, the image has Git 2.56.1, OpenSSH 10.6, CA certificates, the user `git`
as UID and GID 65532 with a passwd entry and the home directory `/home/git`,
builds for amd64 and arm64, and an SPDX SBOM for every package.

The pin is checked, not trusted:

- `hack/verify-agent-base.sh` checks Chainguard's cosign signature on the digest in
  `.ko.yaml` against the exact identity of their release workflow
  (`chainguard-images/images`, `release.yaml` on `main`). CI runs it on every pull
  request, so a digest is checked in the pull request that changes it, and the
  release runs it before it publishes. Both retry it, because it needs `cgr.dev`
  and the Sigstore services.
- `hack/check-agent-image.sh` runs the built image with a read-only root
  filesystem, no capabilities and no privilege escalation. It checks for a numeric
  non-root user, the agent's entrypoint, a Git no older than `MIN_GIT` (2.50.1, so
  that a base with an older Git fails), ssh with a resolving passwd entry, and a
  Git commit. CI and `make check-agent-image` run it.

The release overrides the labels that the base image brings (the title, the
authors, the vendor, the URL and the creation time), so the published image does
not name Chainguard as its author.

Keeping Git current is a procedure, in `docs/releasing.md`: take the new digest,
pin it, raise `MIN_GIT`, run the two scripts, and cut a patch release.

Other ways were considered:

- **An image of our own, built with apko or Alpine from packages.** It would
  trim the contents, keep every digest we publish available and carry our own
  signature. It needs a workflow that builds, signs, publishes and pins it, and a
  rebuild cadence. It is where to go if the points below start to hurt.
- **Debian slim.** Its Git lags behind a current release, and it is larger.
- **A community image with Git.** Its origin cannot be verified.

## Consequences

- The agent image is 86 MB, against 2 MB on distroless. A node pulls it once.
- CI runs the amd64 image only, because its runners are amd64. The arm64 image
  is built and its base is signed, but nothing runs it before a release.
- The image has more than the agent needs: git-lfs, gpg, glibc and a shell. That
  is more surface than an image of our own would have.
- The free Chainguard images publish only `latest`. An older digest may stop
  being pullable. CI pulls the base on every run, so it would fail loudly, and the
  way out is a new digest, a mirror in ghcr.io or an image of our own.
- The base image is a vendor's free image, and its terms can change.
- Git is GPL-2.0-only, and the image has other copyleft packages such as glibc.
  The SBOMs in the image give each package's license and source. The
  `org.opencontainers.image.licenses` label stays Apache-2.0, the license of
  this project's own code.
- The home and temporary directories are still read-only. Volumes for them, and
  a passwd entry for an arbitrary user ID, come with #107: the check shows that
  ssh does not run for a user ID the image does not know.
- Dependabot cannot read `.ko.yaml`, and the scheduled workflows here wait for
  approval, so nothing proposes a new digest (#148). A newer Git reaches users
  when a maintainer follows the procedure.
