#!/usr/bin/env bash
# Shared helpers for the tracked git hooks. Sourced by pre-commit.
#
# These hooks mirror .github/workflows/ci.yml so a commit that would fail CI
# fails locally first. Bypass with `git commit --no-verify` when you know what
# you're doing.

set -euo pipefail

# Repo root = Go module root (this module's root IS the repo root).
ROOT="$(git rev-parse --show-toplevel)"

# ── pretty output ──────────────────────────────────────────────────────
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YLW=$'\033[33m'
  C_DIM=$'\033[2m'; C_BLD=$'\033[1m'; C_RST=$'\033[0m'
else
  C_RED=''; C_GRN=''; C_YLW=''; C_DIM=''; C_BLD=''; C_RST=''
fi

step() { printf '%s▸ %s%s\n' "$C_BLD" "$1" "$C_RST"; }
ok()   { printf '%s  ✓ %s%s\n' "$C_GRN" "$1" "$C_RST"; }
skip() { printf '%s  ⤼ %s%s\n' "$C_YLW" "$1" "$C_RST"; }
note() { printf '%s    %s%s\n' "$C_DIM" "$1" "$C_RST"; }

# fail "<which CI check>" "<how to fix>"
fail() {
  printf '\n%s✗ %s would fail CI.%s\n' "$C_RED$C_BLD" "$1" "$C_RST" >&2
  if [ -n "${2:-}" ]; then
    printf '%s  Fix: %s%s\n' "$C_YLW" "$2" "$C_RST" >&2
  fi
  printf '%s  (bypass with --no-verify if you must)%s\n' "$C_DIM" "$C_RST" >&2
  exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }
