# cockroach-cli — guide (use · context · extend · reuse)

Both a **tutorial** (read top-to-bottom to understand the tool) and a **reference**
(jump to a heading). `cockroach-cli` is two things in one repo: a **standalone CLI**
and the **shared Go SDK** (`pkg/`) that every Cockroach product reuses.

---

## 1. What the commands look like

There are **two flavours of the same binary**, built from the same core:

| Flavour | Where | Commands |
|---|---|---|
| **Core** (cross-product) | `brew`/`curl`/`go install` → this repo | `version`, `pack`, `lang` |
| **Product** (e.g. Reporters) | bundled in the product image | the core **plus** the product's own (`init`, `quickstart`, `migrate`, `seed`, `setup`, `status`, `doctor`, `scope`, `mobile`, `update`, `logs`, `rollback`, …) |

```
cockroach-cli <command> [subcommand] [--flags]

# core (works for ANY product / standalone)
cockroach-cli version
cockroach-cli pack list                     # browse the registry catalogue
cockroach-cli pack info IN
cockroach-cli pack install --country IN     # download → sha256-verify → load
cockroach-cli pack install --boundary IN
cockroach-cli pack verify IN
cockroach-cli lang list | stats | enable <code> | import --country IN
```

`--help` works at every level (`cockroach-cli pack --help`, `… pack install --help`).
Output is **scriptable** (stable columns / ids); **exit codes** are a contract:
`0` ok · `1` runtime error · `2` usage error.

---

## 2. How context is set (no hidden global state)

A command never guesses what to operate on — context is **explicit, via environment
+ flags** (12-factor). The whole context surface:

| Context | Set by | Default |
|---|---|---|
| **Which database** | `DATABASE_URL` | local dev DSN (refuses to default in `APP_ENV=production`) |
| **Which registry** (real / fork / air-gapped mirror) | `COCKROACH_REGISTRY_URL` | the public `cockroach-registry` raw URL |
| **Local pack source** | `COCKROACH_PACK_DIR` / `pack install --from-dir` | — |
| **Env file** | auto-loads `.env` / `backend/.env` (exported vars win) | — |
| **Which version** to install | `pack install --version vX` | the registry's latest |

So the same `cockroach-cli pack install --country IN` works against **any** product's
database (it just needs the gazetteer tables) and **any** registry — point the two
env vars and go. That is the cross-product context model.

---

## 3. Is it idempotent? — Yes.

Re-running a command is safe and converges to the same state (the correct meaning of
*idempotent*). Concretely:

- **`pack install`** — each pack is **sha256-verified**; an already-installed pack at
  the same version is **skipped** (the `installed_packs` ledger), and the load itself
  is **staging → `ON CONFLICT` UPSERT**, so re-running never duplicates or corrupts
  rows. Interrupted? Re-run — it resumes.
- **`migrate`** (product) — a **ledger** (`schema_migrations`) records each file;
  applied migrations are skipped. Re-invoking is a no-op.
- **`lang import`** — UPSERT keyed by `(locale, namespace, key)`; never clobbers an
  operator `override` row.
- **`quickstart` / `bootstrap`** — composed from the idempotent steps above, safe to
  re-run.

The design rule: **declarative, verify-then-apply, ledgered** — so a command is a
safe button, not a one-shot.

---

## 4. Extending it — adding a command

The CLI is **thin**: it owns help + flag parsing and calls the `pkg/` SDK — the logic
never lives in `cmd/`. To add a **core** (cross-product) command:

1. Put the reusable logic in `pkg/…` with table-driven tests.
2. Add a `clibuild.Command{Name, Summary, Run}` and include it in `clibuild.CoreCommands()`.
3. Add the one-line usage + a `--help`.

To add a **product-specific** command, you don't touch this repo at all — see §5.

> Industry-standard note: today commands use the stdlib `flag` package + a hand-rolled
> registry (`clibuild`). The planned DX upgrade is **cobra** (kubectl/gh/helm use it)
> for shell **completions** + a richer `AddCommand` / **plugin** seam — it layers over
> the same `clibuild` core without changing how commands are invoked.

---

## 5. How a NEW PRODUCT builds on it (cross-product reuse)

A product (Reporters today; Budgets/Atlas tomorrow) **imports the module** and builds
its own flavoured binary on the shared core — the `kubectl`-core + extension model:

```go
// product's cmd/cockroach-cli/main.go
import "github.com/cockroachnetworkorg/cockroach-cli/clibuild"

cmds := append(clibuild.CoreCommands(), productCommands()...) // core ⊕ product
os.Exit(clibuild.Dispatch(clibuild.ProgName(), cmds, os.Args[1:]))
```

The product gets `pack`/`lang`/`version` **for free** (identical UX everywhere) and
adds its own `seed`/`setup`/domain commands. It also imports the SDK directly:

```go
import "github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"  // the loader/registry client
```

So country data + translations + the registry are written **once** and reused by every
product. The product's image bundles its flavoured `cockroach-cli`; operators run one
tool that does everything for that product. (Versions stay aligned via a `go.mod` pin.)

---

## 6. Does it follow industry standards? — Yes, with one upgrade pending

| Standard | Here |
|---|---|
| Static, checksummed registry (shadcn/Helm/Krew) | ✅ `registry.json` + sha256 |
| Verify-on-download, fail-closed | ✅ sha256 + size, before any load |
| Env-based config, no hidden state (12-factor) | ✅ §2 |
| Idempotent, ledgered operations | ✅ §3 |
| Scriptable output + consistent exit codes (0/1/2) | ✅ |
| Importable, **semver-stable SDK** | ✅ `pkg/` (see CONTRIBUTING) |
| Core-tool + product-extension model (kubectl/gh) | ✅ `clibuild` |
| Shell completions + plugin model | ⏳ pending **cobra** migration |
| Cross-platform install (brew/curl/go install + Windows scoop/winget) | ✅ |

---

See also: [`CONTRIBUTING.md`](../CONTRIBUTING.md) (build/test, the SDK contract, the
PR flow) and, in the `reporters` repo, `plan/dedicated-cli-and-core-architecture.md`
(the full topology + the staged roadmap).
