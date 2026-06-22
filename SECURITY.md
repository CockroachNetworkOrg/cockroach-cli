# Security Policy

## Reporting a vulnerability

`cockroach-cli` is the **client that verifies and installs** civic data packs —
it fetches checksummed packs from the central registry and pins them against
their published `sha256`. A flaw in that verification path (or in the install /
release tooling) is a supply-chain concern. Please report it **privately**
rather than opening a public issue. Two channels:

1. **Preferred — GitHub Security Advisory:** open a private advisory at
   <https://github.com/cockroachnetworkorg/cockroach-cli/security/advisories/new>.
   This keeps the discussion confidential until a fix ships.

2. **Email:** the maintainer contact listed in the
   [`reporters` repo's MAINTAINERS.md](https://github.com/cockroachnetworkorg/reporters/blob/main/MAINTAINERS.md)
   (the project's governance home). Use a clear subject line, e.g.
   `[security] <one-line summary>`.

Please include:
- Affected component / file paths (CLI, `pkg/pack`, or release/install tooling)
- Steps to reproduce (a minimal proof of concept is welcome)
- Impact assessment (what an attacker could do)
- A suggested fix, if you have one

## Scope

In scope — anything that lets the CLI install bytes other than the publisher's
audited pack, or that subverts a security control:
- A bypass of the `sha256` / size verification in `pkg/pack` (the install would
  proceed on bytes that don't match the registry's pinned digest).
- A path-traversal, gzip-bomb, or oversized-body issue in pack/boundary
  extraction or parsing.
- Tampering with the release artifacts or the `install-cli.sh` checksum flow
  (so a curl/brew install lands an unverified binary).

Out of scope (normal issue / PR): cosmetic bugs, help-text typos, listing
errors in the upstream registry (report those to `cockroach-registry`).

## What to expect

| Stage | Timeline |
|---|---|
| Acknowledgement of report | ≤ 72 hours |
| Initial assessment (confirmed / not-a-vuln / needs-more-info) | ≤ 7 days |
| Fix development + coordinated disclosure plan | ≤ 90 days for most issues |
| Public disclosure (where applicable) | after the fix ships |

We follow **responsible / coordinated disclosure**. We will not pursue legal
action against researchers who report in good faith and follow this policy.
Researchers who report meaningful vulnerabilities are credited in the release
notes (or kept anonymous on request).

## Out of scope

- Vulnerabilities in third-party dependencies — report those upstream first;
  if the issue is in how *we* use a library, that's in scope.
- Self-XSS, social engineering, or issues requiring physical server access.
- Reports from automated scanners with no demonstrated impact.

## Supported versions

This repository follows a rolling release model on `main`. Security fixes
target `main` and ship in the next tagged release.
