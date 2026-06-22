package pack

// Pack importer — the bytes-to-rows half of `cockroach-cli pack install`.
//
// Flow per pack: download → verify sha256 → gunzip → COPY into a temp staging
// table → INSERT … SELECT … ON CONFLICT DO NOTHING into the target → record in
// installed_packs. The staging-table indirection is what makes the load
// idempotent: COPY itself has no ON CONFLICT, but the post-COPY INSERT does,
// so a re-run adds zero rows.
//
// Packs are processed in the order they appear in registry.json (kinds first,
// then units that FK them, then postal_codes, …). The installer never reorders.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxPackBytes caps the in-memory size of a single pack download. India's
// largest pack today is ~7 MiB; this gives ~70× headroom for years of growth
// before we'd need to switch to a streaming/temp-file path.
const maxPackBytes int64 = 512 << 20 // 512 MiB

// httpFetchTimeout is the per-pack download budget. Generous so a slow CDN
// doesn't false-fail; the context cancellation still cuts it short on Ctrl-C.
const httpFetchTimeout = 10 * time.Minute

// InstallOptions controls one `cockroach-cli pack install` invocation.
type InstallOptions struct {
	DryRun      bool   // count rows + verify checksums, but write nothing
	InstalledBy string // ledger attribution: "cli", "admin:<user-id>", …
	RegistryURL string // for the installed_packs.registry_url audit field
	HTTPClient  *http.Client
	// FromDir, when non-empty, makes the installer read each pack's bytes from a
	// LOCAL directory (filepath.Join(FromDir, Pack.File)) instead of fetching the
	// registry's release_url over HTTP. The SAME sha256 + size verification still
	// runs against the registry entry — the registry stays the source of truth for
	// expected hashes/rows — so a local pack that doesn't match aborts loudly. Used
	// by pack authors / local dev to install before publishing a GitHub release.
	FromDir string
}

// PackResult is what InstallPack reports for one csv.gz.
type PackResult struct {
	Pack         Pack
	BytesFetched int64
	RowsInCSV    int64 // gunzipped data-row count (header excluded)
	RowsInserted int64 // (insertions actually committed; equals RowsInCSV minus existing on first run, 0 on second run)
	DurationMs   int64
	Skipped      bool   // true if pack was already at this sha256 in installed_packs
	SkipReason   string // populated when Skipped is true
}

// Installer carries the dependencies a pack install needs: a DB pool and an
// HTTP client. Construct one per session.
type Installer struct {
	DB     *pgxpool.Pool
	Client *http.Client
}

// NewInstaller returns an Installer with sane defaults (the shared HTTP client
// with a generous timeout).
func NewInstaller(db *pgxpool.Pool) *Installer {
	return &Installer{
		DB:     db,
		Client: &http.Client{Timeout: httpFetchTimeout},
	}
}

// Install fetches and applies every pack in a CountryEntry, in registry order.
// On dry-run, no rows are written and no ledger row is inserted; the report
// still reflects what would have happened. The first per-pack error aborts the
// run — leaving later packs unapplied is intentional, since they often FK the
// earlier ones (units → kinds).
func (im *Installer) Install(ctx context.Context, cc string, entry CountryEntry, opts InstallOptions) ([]PackResult, error) {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	out := make([]PackResult, 0, len(entry.Packs))
	for _, p := range entry.Packs {
		res, err := im.InstallPack(ctx, cc, entry.Version, p, opts, entry)
		out = append(out, res)
		if err != nil {
			return out, fmt.Errorf("pack %s: %w", p.File, err)
		}
	}
	return out, nil
}

// InstallPack handles one csv.gz end-to-end. The `entry` is threaded through
// to compute the download URL and to write the ledger row.
func (im *Installer) InstallPack(
	ctx context.Context, cc, version string, p Pack,
	opts InstallOptions, entry CountryEntry,
) (PackResult, error) {
	started := time.Now()
	res := PackResult{Pack: p}

	// Short-circuit: already installed at this sha256 and we're not dry-running.
	// Saves a network round-trip on `cockroach-cli pack install` re-runs.
	if !opts.DryRun {
		var have string
		_ = im.DB.QueryRow(ctx,
			`SELECT sha256 FROM installed_packs WHERE country_code=$1 AND pack_file=$2`,
			cc, p.File,
		).Scan(&have)
		if have == p.SHA256 {
			res.Skipped = true
			res.SkipReason = "already at sha256 " + short(p.SHA256)
			res.DurationMs = time.Since(started).Milliseconds()
			return res, nil
		}
	}

	// Source the bytes locally (pack-author / pre-release dev) or over HTTP. Both
	// paths run the identical sha256 + size verification below.
	url := entry.DownloadURL(p)
	var raw []byte
	var err error
	if opts.FromDir != "" {
		path := filepath.Join(opts.FromDir, p.File)
		url = path // recorded in installed_packs.source_url for the local install
		raw, err = readAndVerify(path, p)
	} else {
		raw, err = im.fetchAndVerify(ctx, url, p)
	}
	if err != nil {
		return res, err
	}
	res.BytesFetched = int64(len(raw))

	csvBytes, err := gunzip(raw)
	if err != nil {
		return res, fmt.Errorf("gunzip: %w", err)
	}
	res.RowsInCSV = countDataRows(csvBytes)

	if p.ExpectedRows > 0 && res.RowsInCSV != p.ExpectedRows {
		return res, fmt.Errorf("row count mismatch: got %d, registry pinned %d", res.RowsInCSV, p.ExpectedRows)
	}

	if opts.DryRun {
		res.DurationMs = time.Since(started).Milliseconds()
		return res, nil
	}

	inserted, err := im.loadCSV(ctx, p, csvBytes, entry.Assertions)
	if err != nil {
		return res, err
	}
	res.RowsInserted = inserted

	if err := im.recordInstalled(ctx, cc, version, p, opts, url, inserted); err != nil {
		return res, fmt.Errorf("record installed: %w", err)
	}
	res.DurationMs = time.Since(started).Milliseconds()
	return res, nil
}

// fetchAndVerify downloads url, enforces size bounds, and rejects a sha256
// mismatch. The whole body is buffered because we need the digest before we
// trust the bytes (streaming gzip would commit data before authentication).
func (im *Installer) fetchAndVerify(ctx context.Context, url string, p Pack) ([]byte, error) {
	if im.Client == nil {
		im.Client = &http.Client{Timeout: httpFetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "cockroach-cli/pack")
	resp, err := im.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPackBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read pack body: %w", err)
	}
	if err := verifyPackBytes(body, p); err != nil {
		return nil, err
	}
	return body, nil
}

// readAndVerify reads a pack's bytes from a LOCAL file and runs the EXACT same
// size + sha256 checks as the HTTP path — the registry stays the source of
// truth for the expected digest, so a local pack that doesn't match aborts
// loudly. Used by InstallPack when InstallOptions.FromDir is set.
func readAndVerify(path string, p Pack) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read local pack %s: %w", path, err)
	}
	if err := verifyPackBytes(body, p); err != nil {
		return nil, err
	}
	return body, nil
}

// verifyPackBytes enforces the cap, the registry's pinned size, and the pinned
// sha256 on a fully-buffered pack body. Shared by the HTTP and --from-dir paths
// so both verify identically (fail loud on any mismatch).
func verifyPackBytes(body []byte, p Pack) error {
	if int64(len(body)) > maxPackBytes {
		return fmt.Errorf("pack exceeds %d bytes cap (got > %d)", maxPackBytes, maxPackBytes)
	}
	if p.SizeBytes > 0 && int64(len(body)) != p.SizeBytes {
		return fmt.Errorf("size mismatch: got %d bytes, registry pinned %d", len(body), p.SizeBytes)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, p.SHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, registry pinned %s", got, p.SHA256)
	}
	return nil
}

// loadCSV stages the rows in a temp table, then UPSERTs into the live table.
// The staging table is created via LIKE so its column list mirrors the target
// exactly (INCLUDING DEFAULTS). COPY uses an EXPLICIT column list parsed from
// the CSV header — `HEADER TRUE` only skips the header row, it does NOT map
// columns by name — so a pack that predates an additively-added column (a
// later migration's `ADD COLUMN … DEFAULT …`, per the additive-only rule) still
// loads: the absent column fills from its default instead of breaking COPY. The
// post-COPY INSERT uses the registry-supplied conflict key, so a re-run adds
// zero rows.
//
// Done in a single transaction so a malformed pack doesn't leave the live
// table half-populated.
func (im *Installer) loadCSV(ctx context.Context, p Pack, csvBytes []byte, asserts *Assertions) (int64, error) {
	tx, err := im.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	staging := "stg_" + sanitiseIdent(p.Table)
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP`,
		staging, quoteIdent(p.Table),
	)); err != nil {
		return 0, fmt.Errorf("create staging: %w", err)
	}

	// COPY from a reader — pgx's PgConn exposes the low-level CopyFrom that
	// streams bytes directly into the connection without parsing/marshalling.
	// Explicit column list from the CSV header so the COPY targets exactly the
	// columns the pack carries; any additively-added column the pack predates
	// fills from its DEFAULT instead of erroring. Columns are publisher-authored
	// identifiers; quote them defensively all the same.
	cols, err := csvHeaderColumns(csvBytes)
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}

	conn := tx.Conn()
	tag, err := conn.PgConn().CopyFrom(ctx,
		bytes.NewReader(csvBytes),
		fmt.Sprintf(`COPY %s (%s) FROM STDIN WITH (FORMAT CSV, HEADER TRUE)`, staging, strings.Join(quoted, ", ")),
	)
	if err != nil {
		return 0, fmt.Errorf("COPY into staging: %w", err)
	}
	_ = tag // row count from CopyFrom is informational; we re-count via the INSERT below.

	// Data-integrity gate. The publisher can pin per-kind / per-kind-state row
	// counts and a forbidden-code pattern in the registry; we evaluate them
	// against the freshly-staged rows BEFORE touching the live table. Any
	// mismatch returns an error, the deferred Rollback fires, and nothing is
	// committed. A nil/empty assertions block (every pre-Wave-A pack) is a no-op.
	if err := checkAssertions(ctx, tx, staging, p.Table, asserts); err != nil {
		return 0, err
	}

	// Idempotent move into the live table. The registry's `conflict` field is
	// SQL written by a trusted publisher; we surface it verbatim into the
	// INSERT (no user input crosses this boundary).
	upsertSQL := fmt.Sprintf(
		`INSERT INTO %s SELECT * FROM %s ON CONFLICT %s DO NOTHING`,
		quoteIdent(p.Table), staging, p.Conflict,
	)
	tag2, err := tx.Exec(ctx, upsertSQL)
	if err != nil {
		return 0, fmt.Errorf("upsert into %s: %w", p.Table, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return tag2.RowsAffected(), nil
}

// csvHeaderColumns returns the column names from the first line of a CSV byte
// slice — comma-separated, with surrounding whitespace and a trailing CR (CRLF
// packs) trimmed. Used to build an explicit COPY column list so a pack survives
// columns added to the target table after the pack was published.
func csvHeaderColumns(b []byte) ([]string, error) {
	nl := bytes.IndexByte(b, '\n')
	if nl < 0 {
		nl = len(b)
	}
	header := strings.TrimRight(string(b[:nl]), "\r")
	if strings.TrimSpace(header) == "" {
		return nil, fmt.Errorf("empty CSV header")
	}
	parts := strings.Split(header, ",")
	cols := make([]string, len(parts))
	for i, p := range parts {
		cols[i] = strings.TrimSpace(p)
	}
	return cols, nil
}

// checkAssertions enforces the registry's optional data-integrity contract
// against the staged rows, inside the caller's open transaction. It is a no-op
// when the entry carries no assertions, or when the assertions target a
// different table than the pack being loaded (the contract is pinned to one
// table — e.g. gazetteer_units — and other packs in the same country skip it).
//
// All checks query the STAGING temp table (its name is a sanitised identifier
// synthesised by loadCSV, never user input). On the first failure it returns a
// descriptive error; loadCSV's deferred Rollback then discards everything.
func checkAssertions(ctx context.Context, tx pgx.Tx, staging, table string, a *Assertions) error {
	if a == nil {
		return nil
	}
	// Assertions are pinned to a specific table; only gate the matching pack.
	if a.Table != "" && a.Table != table {
		return nil
	}

	// 1) Forbidden code pattern — reject the whole load if any staged `code`
	//    matches (e.g. "-0$" catches the ac-X-0 placeholder class).
	if a.ForbidCodePattern != "" {
		var bad int64
		var sample string
		err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*), COALESCE(min(code), '') FROM %s WHERE code ~ $1`, staging,
		), a.ForbidCodePattern).Scan(&bad, &sample)
		if err != nil {
			return fmt.Errorf("assertion forbid_code_pattern: query: %w", err)
		}
		if bad > 0 {
			return fmt.Errorf(
				"assertion failed: %d staged %s row(s) match forbidden code pattern %q (e.g. %q)",
				bad, table, a.ForbidCodePattern, sample)
		}
	}

	// 2) by_kind — exact row count per `kind` over the whole staged table.
	for kind, want := range a.ByKind {
		var got int64
		err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*) FROM %s WHERE kind = $1`, staging,
		), kind).Scan(&got)
		if err != nil {
			return fmt.Errorf("assertion by_kind[%s]: query: %w", kind, err)
		}
		if got != want {
			return fmt.Errorf(
				"assertion failed: by_kind[%s] expected %d staged %s row(s), got %d",
				kind, want, table, got)
		}
	}

	// 3) by_kind_state — exact row count per (state_code, kind).
	for state, kinds := range a.ByKindState {
		for kind, want := range kinds {
			var got int64
			err := tx.QueryRow(ctx, fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE state_code = $1 AND kind = $2`, staging,
			), state, kind).Scan(&got)
			if err != nil {
				return fmt.Errorf("assertion by_kind_state[%s][%s]: query: %w", state, kind, err)
			}
			if got != want {
				return fmt.Errorf(
					"assertion failed: by_kind_state[%s][%s] expected %d staged %s row(s), got %d",
					state, kind, want, table, got)
			}
		}
	}
	return nil
}

// recordInstalled writes (or refreshes) the installed_packs ledger row.
func (im *Installer) recordInstalled(
	ctx context.Context, cc, version string, p Pack,
	opts InstallOptions, sourceURL string, rowsLoaded int64,
) error {
	_, err := im.DB.Exec(ctx, `
		INSERT INTO installed_packs (
			country_code, pack_file, target_table, version, sha256,
			rows_loaded, source_url, registry_url, installed_at, installed_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9)
		ON CONFLICT (country_code, pack_file) DO UPDATE SET
			target_table = EXCLUDED.target_table,
			version      = EXCLUDED.version,
			sha256       = EXCLUDED.sha256,
			rows_loaded  = EXCLUDED.rows_loaded,
			source_url   = EXCLUDED.source_url,
			registry_url = EXCLUDED.registry_url,
			installed_at = NOW(),
			installed_by = EXCLUDED.installed_by
	`,
		cc, p.File, p.Table, version, strings.ToLower(p.SHA256),
		rowsLoaded, nullable(sourceURL), nullable(opts.RegistryURL),
		nullable(opts.InstalledBy),
	)
	return err
}

// InstalledPack is one row of the installed_packs ledger.
type InstalledPack struct {
	CountryCode string
	PackFile    string
	TargetTable string
	Version     string
	SHA256      string
	RowsLoaded  int64
	InstalledAt time.Time
}

// ListInstalled returns every recorded pack install, ordered by country then file.
func ListInstalled(ctx context.Context, db *pgxpool.Pool) ([]InstalledPack, error) {
	rows, err := db.Query(ctx, `
		SELECT country_code, pack_file, target_table, version, sha256, rows_loaded, installed_at
		  FROM installed_packs
		 ORDER BY country_code, pack_file
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstalledPack
	for rows.Next() {
		var p InstalledPack
		if err := rows.Scan(&p.CountryCode, &p.PackFile, &p.TargetTable, &p.Version, &p.SHA256, &p.RowsLoaded, &p.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func gunzip(b []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	// Bound the decompressed size at 8× the gzip cap so a zip-bomb can't OOM us.
	return io.ReadAll(io.LimitReader(gz, maxPackBytes*8))
}

// countDataRows counts the data rows (header excluded) in CSV bytes. Quoted
// newlines inside fields would inflate the count, so we use the standard csv
// reader rather than a newline scan.
func countDataRows(b []byte) int64 {
	// A streaming line count is fine for the typical CSV (no embedded newlines
	// in gazetteer codes/names). If a pack ever uses multiline-quoted fields,
	// the registry's expected_rows guards against a silent mismatch.
	var n int64
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if n > 0 && b[len(b)-1] != '\n' {
		n++ // final line without trailing newline
	}
	if n > 0 {
		n-- // exclude header
	}
	return n
}

// sanitiseIdent strips anything that isn't safe to drop into a SQL identifier
// (used only for the temp staging table name we synthesise from p.Table).
func sanitiseIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "t"
	}
	return b.String()
}

// quoteIdent wraps a table name in double quotes and escapes embedded ones.
// We trust registry publishers, but quote anyway in case a table name ever
// starts with a digit or collides with a reserved word.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// short returns a 12-char hex prefix for human-readable logs.
func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// nullable returns nil for empty strings so the column persists as NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
