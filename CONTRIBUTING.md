# Contributing

Thanks for considering a contribution to `cockroach-cli` — the standalone CLI
and the shared Go SDK (`pkg/`) for the Cockroach Network ecosystem.

This is an **independent open-source project**, Apache-2.0. By contributing you
agree your contribution is licensed under the [LICENSE](LICENSE) and you sign
off each commit per the DCO (below).

## Build & test

The module root **is** the repo root — no `backend/` subdir. Everything runs
with the standard Go toolchain (Go 1.25, see [`go.mod`](go.mod)):

```bash
go build ./...        # compile the CLI + the pkg/ SDK
go vet ./...          # static checks (a CI gate)
gofmt -l .            # must print nothing (a CI gate)
go test ./...         # unit tests
```

Most tests are **DB-free**: the importer tests run `InstallPack` in `--dry-run`
mode (read/download → sha256+size verify → gunzip → row-count, then stop before
any COPY). A few assertion tests touch Postgres — they read `TEST_DATABASE_URL`,
fall back to the dev compose DSN, and **skip cleanly** when no database is
reachable. To run them locally, point at a throwaway Postgres:

```bash
export TEST_DATABASE_URL="postgres://cockroach:cockroach@localhost:5432/cockroach_test?sslmode=disable"
go test ./...
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs
build + gofmt + vet + test against a Postgres service on every PR.

## Git hooks — catch CI failures locally

The repo ships a tracked pre-commit hook (`.githooks/`) that mirrors the CI
gofmt + vet gates on staged Go files, so a commit that would fail CI fails on
your machine first. Enable it once:

```bash
git config core.hooksPath .githooks
```

Bypass with `git commit --no-verify` when you must; CI is the backstop.

## The `pkg/` SDK is a public, semver-stable contract

`pkg/pack` (and anything else under `pkg/`) is imported by **other products**
(e.g. Cockroach Reporters) — that's the whole reason this module exists. Treat
its exported API as a published SDK:

- Additive changes (new exported function/type/field) are fine.
- **Breaking** an exported signature, removing an export, or changing behavior
  in a way callers can observe needs a clear rationale, a migration note in the
  PR, and a major/minor version bump. Don't break it lightly.
- `internal/` and `cmd/` are not part of the contract — refactor freely.

When in doubt, open an issue and discuss the surface before coding.

## Adding a command

The CLI ([`cmd/cockroach-cli`](cmd/cockroach-cli)) is deliberately thin: it
owns help text + arg parsing and calls straight into the `pkg/` public API —
no second copy of the loader logic. To add a subcommand:

1. Put the reusable logic in `pkg/…` (with table-driven tests) — never in `cmd/`.
2. Wire a `case` in `main.go`'s dispatch, plus a `cmd…` handler that parses
   flags with the stdlib `flag` package (house style — no cobra) and calls the
   `pkg/` API.
3. Add the one-line usage to the `usage()` text and the package doc comment.
4. Keep output scriptable and exit codes consistent (usage errors exit 2).

## Issue-first workflow

Every code change starts from an issue, and you wait for a maintainer go-ahead
before writing the code — this surfaces design objections cheaply and gives the
change a stable number.

1. **Find or open an issue** with the [bug](.github/ISSUE_TEMPLATE/bug.yml) or
   [feature](.github/ISSUE_TEMPLATE/feature.yml) template. For an open-ended
   "should we…?" question, open a GitHub **Discussion** instead.
2. **Claim it** — comment that you'd like to take it and wait for the go-ahead
   (a 👍, an `accepted` label, or a reply) on anything non-trivial.
3. **Then branch and build.**

Trivial, obvious fixes (a typo, a broken link) can skip straight to a PR.

## PR process

1. **Branch off `main` using the issue number.** The convention is
   **`issues/<issue-number>/<short-slug>`**:

   ```
   issues/42/pack-info-json-output
   issues/57/fix-checksum-mismatch-message
   ```

2. **Commit messages**: [Conventional Commits](https://www.conventionalcommits.org/)
   — `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`. Imperative
   subject under ~72 chars; the *why* goes in the body.
3. **Sign off** every commit (DCO, below).
4. **`go build ./...`, `go vet ./...`, `go test ./...` pass** before review.
5. **Open the PR** against `main`, link the issue with a closing keyword
   (`Fixes #42`), and fill in the [PR template](.github/PULL_REQUEST_TEMPLATE.md).
   Keep the PR scoped to the issue — no drive-by refactors.
6. **CI must be green.** Squash-on-merge is the default, so your branch history
   need not be pristine.

## Sign-off — DCO

Every commit must carry a `Signed-off-by:` line — the
[Developer Certificate of Origin](https://developercertificate.org/), a
lightweight assertion that you have the right to submit the code under the
project's license. We use DCO instead of a CLA, so no entity ever owns the
copyright wholesale.

```bash
git commit -s -m "feat: add pack info --json output"
```

That `-s` appends `Signed-off-by: Your Name <you@example.com>` from your
`git config user.name` / `user.email`. Forgot it? `git commit --amend -s --no-edit`.

## Code of Conduct & Security

All participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
(Contributor Covenant 2.1). Do **not** open public issues for security
vulnerabilities — see [SECURITY.md](SECURITY.md) for private disclosure.

## Questions

- **General / "should we…?":** open a GitHub Discussion.
- **Bugs / features:** open a GitHub Issue.
- **Security:** see [SECURITY.md](SECURITY.md).
