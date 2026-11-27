# Security policy

## Reporting a vulnerability

Please report security problems privately. Do not open a public issue, pull
request or discussion.

Use GitHub's private vulnerability reporting: open the
[Security tab](https://github.com/mohanedmoh/treebind/security) and choose
**Report a vulnerability**, or go straight to the
[private report form](https://github.com/mohanedmoh/treebind/security/advisories/new).

Include what you found, how to reproduce it, the version affected, and the
impact you expect. Do not include real credentials.

## What to expect

- We acknowledge your report within **3 working days**.
- We confirm whether we can reproduce it and tell you how we plan to handle it.
- We agree a disclosure date with you. We aim to release a fix within
  **90 days** of the report, sooner for serious problems, and we credit you in
  the advisory unless you prefer not to be named.
- We keep the report private until a fix is available.

## Supported versions

Treebind is alpha software. Its releases are pre-releases, and only the
**latest release** receives security fixes.

| Version | Supported |
| :-- | :-- |
| Latest release | Yes |
| Older releases | No |

## Scope

Treebind runs a sync agent that holds Git credentials and works on a shared
volume, so reports about credential handling, path traversal out of the volume
root, hostile repository content, and privilege escalation through the
operator are especially welcome.
