// Package schema is the shared civic-foundation schema for the Cockroach
// Network framework — the gazetteer + jurisdiction-hierarchy tables that every
// product instance needs before any product-specific migration runs.
//
// It is a self-applying, embedded set of ordered SQL files (see the sql/
// directory) plus a tiny migration runner, so any consumer can stand up the
// civic-core schema with one call:
//
//	pool, _ := pgxpool.New(ctx, dsn)
//	if err := schema.Apply(ctx, pool); err != nil { … }
//
// The core set is tracked in its OWN ledger table — core_schema_migrations —
// kept deliberately separate from a product's schema_migrations ledger so the
// two version independently. Apply is idempotent: an already-applied file is
// skipped, and an existing database that was migrated under the OLD interleaved
// scheme (gazetteer_units already present, empty core ledger) is BASELINED —
// every core file is recorded as applied without re-running it — so wiring this
// into an existing deployment is a clean no-op.
//
// Self-contained: the embedded SQL has NO dependency on any product table.
// 001_gazetteer.sql creates the pg_trgm extension it needs, and
// 002_jurisdiction.sql holds only the CORE civic-schema tables (admin_units /
// scope_changes / installed_boundary_packs) — the PRODUCT mutations that the
// reporters jurisdiction migration formerly interleaved (the platform_settings
// scope envelope + the reports.admin_unit_id/archive columns) now live in the
// product's own migration (reporters 078), which runs AFTER this core set.
// Consequently Apply runs cleanly standalone-first against a truly empty
// database. A product wires it into its migrate entry points to run BEFORE the
// product migrations, which then depend on the tables this core set creates.
package schema

import (
	"context"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// coreFS holds the ordered civic-core SQL files, the self-contained civic
// schema relocated out of the reporters backend (001_gazetteer →
// 002_jurisdiction [CORE half only] → 003_unit_names) as a standalone ordered
// set. The product retains only the rows/columns that touch its own tables.
//
//go:embed sql/*.sql
var coreFS embed.FS

// version is the civic-core schema version. Bump when the embedded set of core
// migrations changes (a file added/removed). It is NOT a per-file ledger value
// — that is the filename — it is the identity of the core set as a whole.
const version = "1"

// Version returns the civic-core schema version — the identity of the embedded
// ordered set as a whole. Bump it whenever the set of core SQL files changes.
func Version() string { return version }

// sentinelTable is the marker table whose presence means "this database was
// already migrated under the old interleaved scheme". When the core ledger is
// empty but this table exists, Apply baselines (records every core file as
// applied without re-running it) instead of replaying the DDL.
const sentinelTable = "gazetteer_units"

// Apply applies every embedded civic-core SQL file EXACTLY ONCE against pool,
// tracked in the core_schema_migrations ledger. Each file runs in its own
// transaction so a failure rolls back cleanly and the ledger never records a
// partially-applied file.
//
// Baseline: if the ledger is empty but the sentinel table (gazetteer_units)
// already exists — a database migrated under the OLD interleaved scheme — every
// core file is RECORDED as applied without re-running it, so an existing
// prod/dev database is a clean no-op.
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := coreFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("schema: no embedded core migrations found")
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS core_schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("schema: ensure core ledger: %w", err)
	}

	// Baseline a database migrated under the old interleaved scheme: empty core
	// ledger + sentinel table present => record every core file as applied
	// WITHOUT running it.
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core_schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("schema: count core ledger: %w", err)
	}
	if applied == 0 {
		var hasSentinel bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('public.'||$1) IS NOT NULL`, sentinelTable,
		).Scan(&hasSentinel); err != nil {
			return fmt.Errorf("schema: probe %s: %w", sentinelTable, err)
		}
		if hasSentinel {
			for _, f := range files {
				if _, err := pool.Exec(ctx,
					`INSERT INTO core_schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`,
					f); err != nil {
					return fmt.Errorf("schema: baseline %s: %w", f, err)
				}
			}
			return nil
		}
	}

	for _, f := range files {
		var done bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM core_schema_migrations WHERE version=$1)`, f).Scan(&done); err != nil {
			return fmt.Errorf("schema: check %s: %w", f, err)
		}
		if done {
			continue
		}
		sqlBytes, err := coreFS.ReadFile(path.Join("sql", f))
		if err != nil {
			return fmt.Errorf("schema: read %s: %w", f, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("schema: begin %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("schema: apply %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO core_schema_migrations(version) VALUES ($1)`, f); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("schema: record %s: %w", f, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("schema: commit %s: %w", f, err)
		}
	}
	return nil
}

// coreFiles returns the embedded core SQL filenames (base names, no dir),
// lexically sorted — the order they must apply in.
func coreFiles() ([]string, error) {
	entries, err := coreFS.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("schema: read embedded sql dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	return files, nil
}
