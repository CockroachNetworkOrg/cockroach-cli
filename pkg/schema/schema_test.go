package schema

// Fresh-DB apply test for the civic-core schema.
//
// Contract mirrors the pack DB tests — TEST_DATABASE_URL first, dev compose DSN
// fallback, t.Skip when Postgres is unreachable. To keep this hermetic and
// non-destructive to any shared dev database, it CREATEs a throwaway database
// (cn_schema_test_<pid>), applies Apply twice (asserting the core tables exist
// + the second Apply is a no-op), then DROPs the throwaway database.
//
// pg_trgm note: the civic-core gazetteer index uses public.gin_trgm_ops; the
// core 001_gazetteer.sql now creates the extension itself (CREATE EXTENSION IF
// NOT EXISTS pg_trgm), so a fresh empty DB needs no stubs at all — the core set
// is fully self-contained.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const devTestDSN = "postgres://cockroach:cockroach@localhost:55432/cockroach_network?sslmode=disable"

func baseDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return devTestDSN
}

func TestApplyFreshDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Admin connection on the base database to create/drop the throwaway DB.
	admin, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("Postgres unreachable (%v) — skipping fresh-DB schema test", err)
	}
	defer admin.Close(ctx)
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("Postgres ping failed (%v) — skipping fresh-DB schema test", err)
	}

	dbName := fmt.Sprintf("cn_schema_test_%d", os.Getpid())
	// Drop a stale one from a prior crashed run, then create fresh.
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("create throwaway db: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), baseDSN())
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`)
	})

	// Connect to the throwaway DB.
	pool, err := pgxpool.New(ctx, dsnForDB(baseDSN(), dbName))
	if err != nil {
		t.Fatalf("connect throwaway db: %v", err)
	}
	defer pool.Close()

	// The core set is fully self-contained: 001_gazetteer.sql creates pg_trgm,
	// and 002_jurisdiction.sql now holds only the CORE half (admin_units /
	// scope_changes / installed_boundary_packs) with no reference to any product
	// table. No stubs needed — Apply runs cleanly against an empty DB.

	// First Apply: builds the core schema.
	if err := Apply(ctx, pool); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Core tables must exist.
	for _, tbl := range []string{
		"gazetteer_kinds", "gazetteer_units", "postal_codes",
		"constituency_coverage", "neighbourhood_submissions",
		"admin_units", "scope_changes", "installed_boundary_packs",
		"gazetteer_unit_names",
	} {
		var present bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('public.'||$1) IS NOT NULL`, tbl).Scan(&present); err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if !present {
			t.Errorf("expected core table %q to exist after Apply", tbl)
		}
	}

	// Ledger should hold every core file.
	files, err := coreFiles()
	if err != nil {
		t.Fatalf("coreFiles: %v", err)
	}
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core_schema_migrations`).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerCount != len(files) {
		t.Errorf("ledger has %d rows, want %d (one per core file)", ledgerCount, len(files))
	}

	// Second Apply: must be an idempotent no-op (no error, ledger unchanged).
	if err := Apply(ctx, pool); err != nil {
		t.Fatalf("second Apply (idempotency): %v", err)
	}
	var ledgerCount2 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core_schema_migrations`).Scan(&ledgerCount2); err != nil {
		t.Fatalf("re-count ledger: %v", err)
	}
	if ledgerCount2 != ledgerCount {
		t.Errorf("second Apply changed ledger: %d → %d (must be no-op)", ledgerCount, ledgerCount2)
	}
}

// TestApplyBaselinesExistingDB verifies the baseline path: a throwaway DB that
// already has the sentinel table (gazetteer_units) but an empty core ledger is
// recorded — not re-run — by Apply.
func TestApplyBaselinesExistingDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("Postgres unreachable (%v) — skipping baseline test", err)
	}
	defer admin.Close(ctx)
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("Postgres ping failed (%v) — skipping baseline test", err)
	}

	dbName := fmt.Sprintf("cn_schema_baseline_%d", os.Getpid())
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("create throwaway db: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), baseDSN())
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{dbName}.Sanitize()+` WITH (FORCE)`)
	})

	pool, err := pgxpool.New(ctx, dsnForDB(baseDSN(), dbName))
	if err != nil {
		t.Fatalf("connect throwaway db: %v", err)
	}
	defer pool.Close()

	// Simulate an old-scheme DB: the sentinel table exists, but Apply has never
	// run (no core ledger). A bare stub is enough — baseline records, never runs.
	if _, err := pool.Exec(ctx, `CREATE TABLE public.gazetteer_units (code text PRIMARY KEY)`); err != nil {
		t.Fatalf("stub sentinel: %v", err)
	}

	if err := Apply(ctx, pool); err != nil {
		t.Fatalf("Apply (baseline): %v", err)
	}

	// Every core file must be recorded WITHOUT having created the other core
	// tables (baseline records, does not run).
	files, err := coreFiles()
	if err != nil {
		t.Fatalf("coreFiles: %v", err)
	}
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core_schema_migrations`).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerCount != len(files) {
		t.Errorf("baseline recorded %d files, want %d", ledgerCount, len(files))
	}
	// admin_units belongs to 078 — baseline must NOT have created it.
	var adminUnits bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.admin_units') IS NOT NULL`).Scan(&adminUnits); err != nil {
		t.Fatalf("probe admin_units: %v", err)
	}
	if adminUnits {
		t.Error("baseline should not have CREATEd admin_units (it must record, not run)")
	}
}

// dsnForDB rewrites the database name component of a libpq URL DSN.
func dsnForDB(base, dbName string) string {
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		// Fall back to naive replace; the dev DSN is a well-formed URL.
		return base
	}
	cfg.Database = dbName
	// Rebuild a URL DSN from the parsed config's connection params.
	user := cfg.User
	if cfg.Password != "" {
		user += ":" + cfg.Password
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable",
		user, cfg.Host, cfg.Port, dbName)
}
