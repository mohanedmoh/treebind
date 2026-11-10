# 0001. API group placeholder and name checks

- Status: accepted
- Date: 2026-11-10
- Issue: #15

## Context

The API group is the placeholder `treebind.example.org`. It is part of every
resource's `apiVersion`, so changing it after the first release means a new CRD
and a migration. The group should be a domain the project controls, as Flux and
grafana-operator do. Groups under `k8s.io` and `kubernetes.io` are reserved and
need an approval annotation (KEP-2337).

Buying a domain is not needed to build and test the alpha. The name checks are
free, so they were run now (2026-11-10):

- GitHub repositories and organisations named `treebind`: no conflict.
- Container registries (ghcr.io, Docker Hub, Quay): no conflict.
- Artifact Hub charts: no conflict.
- Language package registries (PyPI, npm, crates.io, pkg.go.dev): no conflict.
- Trademark registers (TMview, USPTO, WIPO Global Brand Database): no conflict.
- Kubernetes API groups and CRDs named `treebind`: no conflict.
- Domain availability: several candidate domains are available to register.

The project is personal. The maintainer's work domain is not used, so the API
group stays independent of any employer.

## Decision

Keep `treebind.example.org` (reserved by RFC 2606, so it cannot clash with a real
group) until the first non-alpha release. Before that release, register a domain
in the maintainer's own name, rewrite the group everywhere it appears (`PROJECT`,
`api/`, `config/`, the Helm chart and the samples) and record the chosen domain
in a new ADR that supersedes this one.

Until then, releases are pre-releases and make no promise that the API group
stays the same.

## Consequences

- No spend is needed now, and contributors can keep building against the
  placeholder.
- Every new CRD, RBAC, chart or sample file adds another place to rewrite, so the
  rewrite should be a single scripted change.
- Anyone who installs an alpha will have to reinstall the CRD when the group
  changes. The release notes must say so.
- The name checks are only valid for the date above and must be repeated before
  the first public release.
