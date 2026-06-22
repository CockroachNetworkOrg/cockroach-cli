<!--
  Concise PR template. Replace each section with your own text — keep it tight.
  This project is issue-first: open/claim an issue and get a maintainer
  go-ahead BEFORE coding. See CONTRIBUTING.md.
-->

## Linked issue

<!-- Required. Use a closing keyword so the issue auto-closes on merge. -->

Fixes #

## Summary

<!-- One paragraph: what changes, and why. Scope should match the linked issue. -->

## Type of change

- [ ] feat — new command / behavior
- [ ] fix — bug fix
- [ ] docs — README / comments only
- [ ] chore — refactor / deps / CI / release tooling

## Public SDK impact

<!-- Anything under pkg/ is a semver-stable public SDK other products import. -->

- [ ] No change to `pkg/` exported API
- [ ] Backwards-compatible addition to `pkg/` (new export, no break)
- [ ] **Breaking** `pkg/` change — explain the migration and the version bump below

## Testing done

<!-- `go build ./...`, `go vet ./...`, `go test ./...`, manual CLI runs, what you observed.
     Note if DB-backed tests were skipped (no Postgres) vs run. -->

## Checklist

- [ ] I have read [`CONTRIBUTING.md`](../CONTRIBUTING.md) and [`CODE_OF_CONDUCT.md`](../CODE_OF_CONDUCT.md)
- [ ] This PR resolves a tracked issue and a maintainer gave the go-ahead
- [ ] Branch follows the `issues/<issue-number>/<short-slug>` convention
- [ ] `go build ./...`, `go vet ./...`, and `go test ./...` pass locally; CI is green
- [ ] Tests added or updated (or N/A — explain)
- [ ] Commits signed off (DCO `-s`)
- [ ] No secrets, credentials, or PII committed
