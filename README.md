# cockroach-cli

The operator + developer CLI for the **Cockroach Network** ecosystem — and the
shared Go SDK that Cockroach products build on.

It is **product-agnostic**: it installs civic **country data packs** and **boundary
GeoJSON** from a static, checksummed registry into any Postgres database, and the
same `pkg/` libraries are imported by products (e.g. Cockroach Reporters) so the
seed-data + registry machinery is written once and reused everywhere.

## Why this repo exists

The reusable civic foundation (the pack loader, the registry client) used to live
inside one product. Go forbids cross-module import of `internal/…`, so it could not
be reused. It now lives here as an importable module, and `cockroach-cli` is a
standalone, installable tool — not something borrowed from a product repo. See
the [architecture overview](https://docs.cockroachnetwork.org/reporters/for-developers/architecture-overview/)
for the full picture.

## Install

```sh
# Homebrew (macOS / Linux)
brew install cockroachnetworkorg/tap/cockroach-cli

# Scoop (Windows)
scoop bucket add cockroachnetwork https://github.com/cockroachnetworkorg/scoop-bucket
scoop install cockroach-cli

# winget (Windows)
winget install CockroachNetwork.CockroachCLI

# go install
go install github.com/cockroachnetworkorg/cockroach-cli/cmd/cockroach-cli@latest

# curl installer (checksum-verified)
curl -fsSL https://raw.githubusercontent.com/cockroachnetworkorg/cockroach-cli/main/scripts/install-cli.sh | sh
```

### Build from source

A fresh clone needs Go 1.25+ (matches `go.mod`):

```sh
git clone https://github.com/cockroachnetworkorg/cockroach-cli
cd cockroach-cli
go build -o cockroach-cli ./cmd/cockroach-cli
./cockroach-cli version
```

The CLI is also **bundled into the Cockroach Reporters image**, so operators on a
server already have it (with the product-specific commands added) — version-matched
via a `go.mod` pin.

## Usage

```sh
cockroach-cli version
cockroach-cli pack list                       # browse the registry catalogue
cockroach-cli pack install --country IN       # download → sha256-verify → load
cockroach-cli pack install --boundary IN      # boundary GeoJSON
```

Point it at a fork / air-gapped mirror with `COCKROACH_REGISTRY_URL`. Target a
database with `DATABASE_URL`.

## Use as a library (SDK)

```go
import "github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
```

`pkg/pack` is the registry client + pack/boundary loader (fetch static registry →
download → sha256-verify → COPY-load). Other Cockroach products import it directly.

## License

Apache-2.0 — see [LICENSE](LICENSE).
