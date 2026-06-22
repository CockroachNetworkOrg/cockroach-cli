package pack

// Tests for the registry data-integrity assertions (Wave A).
//
// Two layers:
//   - Pure (no DB): JSON round-trip of the optional `assertions` block, and the
//     no-op fast-paths (nil assertions / table mismatch) that must not touch the
//     DB at all.
//   - DB-backed: checkAssertions evaluated against a real staging-like temp
//     table. Connection contract mirrors the handler DB tests — TEST_DATABASE_URL
//     first, dev compose DSN fallback, t.Skip when Postgres is unreachable.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const assertDevTestDSN = "postgres://cockroach:cockroach@localhost:55432/cockroach_network?sslmode=disable"

// dialAssertPool connects to the test Postgres or skips the test.
func dialAssertPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = assertDevTestDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("Postgres unreachable (%v) — skipping DB-backed assertions test", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("Postgres ping failed (%v) — skipping DB-backed assertions test", err)
	}
	// Close via Cleanup (not the caller's defer) so it runs AFTER the fixture
	// tx rollback: cleanups are LIFO and stageFixture registers its rollback
	// later, so the connection is released before Close waits on the pool.
	t.Cleanup(pool.Close)
	return pool
}

// stageFixture opens a tx, creates a staging-like temp table, and inserts the
// given (code, kind, state_code) rows. Returns the tx (rolled back via cleanup)
// and the staging table name, mirroring loadCSV's staging flow.
func stageFixture(t *testing.T, pool *pgxpool.Pool, rows [][3]string) (pgx.Tx, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	const staging = "stg_assert_fixture"
	if _, err := tx.Exec(ctx,
		`CREATE TEMP TABLE `+staging+` (code TEXT, kind TEXT, state_code TEXT) ON COMMIT DROP`,
	); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO `+staging+` (code, kind, state_code) VALUES ($1, $2, $3)`,
			r[0], r[1], r[2],
		); err != nil {
			t.Fatalf("insert fixture %v: %v", r, err)
		}
	}
	return tx, staging
}

// passingRows is a small fixture that satisfies the passingAsserts contract:
// 2 PCs (one per state) and 2 ACs (both in s-9), no forbidden codes.
var passingRows = [][3]string{
	{"pc-9-1", "parliamentary_constituency", "s-9"},
	{"pc-4-1", "parliamentary_constituency", "s-4"},
	{"ac-9-1", "assembly_constituency", "s-9"},
	{"ac-9-2", "assembly_constituency", "s-9"},
}

func passingAsserts() *Assertions {
	return &Assertions{
		Table:             "gazetteer_units",
		ForbidCodePattern: "-0$",
		ByKind: map[string]int64{
			"parliamentary_constituency": 2,
			"assembly_constituency":      2,
		},
		ByKindState: map[string]map[string]int64{
			"s-9": {"parliamentary_constituency": 1, "assembly_constituency": 2},
			"s-4": {"parliamentary_constituency": 1, "assembly_constituency": 0},
		},
	}
}

// ── pure (no DB) ─────────────────────────────────────────────────────────────

func TestAssertionsJSONRoundTrip(t *testing.T) {
	const body = `{
	  "name": "India", "version": "2026.06.05", "release_url": "https://example/IN",
	  "packs": [],
	  "assertions": {
	    "table": "gazetteer_units",
	    "forbid_code_pattern": "-0$",
	    "by_kind": { "parliamentary_constituency": 543, "assembly_constituency": 4095 },
	    "by_kind_state": { "s-9": { "parliamentary_constituency": 80, "assembly_constituency": 403 } }
	  }
	}`
	var ce CountryEntry
	if err := json.Unmarshal([]byte(body), &ce); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ce.Assertions == nil {
		t.Fatal("assertions parsed as nil")
	}
	if ce.Assertions.Table != "gazetteer_units" {
		t.Errorf("table = %q, want gazetteer_units", ce.Assertions.Table)
	}
	if ce.Assertions.ForbidCodePattern != "-0$" {
		t.Errorf("forbid_code_pattern = %q", ce.Assertions.ForbidCodePattern)
	}
	if got := ce.Assertions.ByKind["parliamentary_constituency"]; got != 543 {
		t.Errorf("by_kind[pc] = %d, want 543", got)
	}
	if got := ce.Assertions.ByKindState["s-9"]["assembly_constituency"]; got != 403 {
		t.Errorf("by_kind_state[s-9][ac] = %d, want 403", got)
	}
}

func TestAssertionsBackCompatNoBlock(t *testing.T) {
	// A pre-Wave-A entry without an `assertions` field must leave it nil.
	const body = `{ "name": "Palestine", "version": "2026.05.27", "release_url": "https://x/PS", "packs": [] }`
	var ce CountryEntry
	if err := json.Unmarshal([]byte(body), &ce); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ce.Assertions != nil {
		t.Fatal("absent assertions block should unmarshal to nil")
	}
}

func TestCheckAssertionsNilIsNoOp(t *testing.T) {
	// A nil assertions pointer must return nil WITHOUT querying — pass a nil tx
	// to prove the DB is never touched.
	if err := checkAssertions(context.Background(), nil, "ignored", "gazetteer_units", nil); err != nil {
		t.Fatalf("nil assertions should be a no-op, got %v", err)
	}
}

func TestCheckAssertionsTableMismatchIsNoOp(t *testing.T) {
	// Assertions pinned to a different table must skip (and not touch the DB).
	a := &Assertions{Table: "gazetteer_units", ByKind: map[string]int64{"x": 1}}
	if err := checkAssertions(context.Background(), nil, "ignored", "postal_codes", a); err != nil {
		t.Fatalf("table mismatch should be a no-op, got %v", err)
	}
}

// ── DB-backed ────────────────────────────────────────────────────────────────

func TestCheckAssertionsPasses(t *testing.T) {
	pool := dialAssertPool(t)
	tx, staging := stageFixture(t, pool, passingRows)
	if err := checkAssertions(context.Background(), tx, staging, "gazetteer_units", passingAsserts()); err != nil {
		t.Fatalf("expected assertions to pass, got error: %v", err)
	}
}

func TestCheckAssertionsForbidPatternFails(t *testing.T) {
	pool := dialAssertPool(t)
	// Inject a garbage ac-X-0 row that the "-0$" pattern must reject.
	rows := append([][3]string{}, passingRows...)
	rows = append(rows, [3]string{"ac-1-0", "assembly_constituency", "s-1"})
	tx, staging := stageFixture(t, pool, rows)

	err := checkAssertions(context.Background(), tx, staging, "gazetteer_units", passingAsserts())
	if err == nil {
		t.Fatal("expected forbid_code_pattern to fail on ac-1-0, got nil")
	}
	if !strings.Contains(err.Error(), "forbidden code pattern") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckAssertionsByKindCountFails(t *testing.T) {
	pool := dialAssertPool(t)
	tx, staging := stageFixture(t, pool, passingRows)

	// Demand 543 PCs when only 2 are staged.
	a := passingAsserts()
	a.ByKind["parliamentary_constituency"] = 543
	err := checkAssertions(context.Background(), tx, staging, "gazetteer_units", a)
	if err == nil {
		t.Fatal("expected by_kind count mismatch to fail, got nil")
	}
	if !strings.Contains(err.Error(), "by_kind[parliamentary_constituency]") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckAssertionsByKindStateFails(t *testing.T) {
	pool := dialAssertPool(t)
	tx, staging := stageFixture(t, pool, passingRows)

	// s-9 actually has 2 ACs; demand 3 to force a per-state mismatch that the
	// grand totals (which still sum correctly) would not catch.
	a := passingAsserts()
	a.ByKindState["s-9"]["assembly_constituency"] = 3
	err := checkAssertions(context.Background(), tx, staging, "gazetteer_units", a)
	if err == nil {
		t.Fatal("expected by_kind_state mismatch to fail, got nil")
	}
	if !strings.Contains(err.Error(), "by_kind_state[s-9][assembly_constituency]") {
		t.Errorf("unexpected error: %v", err)
	}
}
