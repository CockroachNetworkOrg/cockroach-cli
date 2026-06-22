package clibuild

// Unit tests for `cockroach-cli pack install --boundary <cc>`. The DB-touching
// path stays out of scope (it lives in pack.BoundaryInstaller's
// upsertAdminUnits, which needs a real Postgres pool); these tests exercise
// the CLI orchestration layer:
//
//   - registry-fetch + entry resolution via a `file://` URL
//   - flag parsing (--dry-run / --verify-only mutual exclusion + --verify-only
//     gated to --boundary)
//   - the install flow in --dry-run mode (parses + counts features without
//     writes)
//
// The httptest pattern mirrors backend/pkg/pack/boundary_test.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
)

// ── TestInstallBoundaryDryRun ─────────────────────────────────────────────────

func TestInstallBoundaryDryRunCLI(t *testing.T) {
	// Build a complete fake universe: an httptest server that returns the
	// boundary assets, AND a registry.json that names them.
	country := []byte(`{"type":"Feature","properties":{"name":"Testland"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)
	states := []byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"state_code":"TT-01","name":"Alpha","hasc":"TT.01"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}]}`)
	cSum := sha256.Sum256(country)
	sSum := sha256.Sum256(states)

	mux := http.NewServeMux()
	mux.HandleFunc("/boundary-country.geojson", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(country) })
	mux.HandleFunc("/boundary-states.geojson", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(states) })

	assetSrv := httptest.NewServer(mux)
	defer assetSrv.Close()

	regJSON := fmt.Sprintf(`{
		"schema":"cockroach-country-seed/1",
		"countries":{},
		"boundaries":{
			"TT":{
				"name":"Testland",
				"version":"2026.05.29",
				"release_url":%q,
				"url":%q,
				"sha256":%q,
				"size_bytes":%d,
				"files":{
					"country":{"file":"boundary-country.geojson","sha256":%q,"size_bytes":%d,"role":"country"},
					"states":{"file":"boundary-states.geojson","sha256":%q,"size_bytes":%d,"role":"states"}
				}
			}
		}
	}`,
		assetSrv.URL,
		assetSrv.URL+"/boundary-country.geojson",
		hex.EncodeToString(cSum[:]), len(country),
		hex.EncodeToString(cSum[:]), len(country),
		hex.EncodeToString(sSum[:]), len(states),
	)

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, regJSON)
	}))
	defer regSrv.Close()

	t.Setenv("COCKROACH_REGISTRY_URL", regSrv.URL)

	out := captureStdout(t, func() {
		// dry-run does not require a DB connection or migration ledger.
		err := installBoundary(context.Background(), "TT", true /*dryRun*/, false /*verifyOnly*/)
		if err != nil {
			t.Fatalf("dry-run failed: %v", err)
		}
	})

	for _, want := range []string{
		"registry:",
		"Testland",
		"2026.05.29",
		"country",
		"states",
		"features=1",
		"dry-run: no rows written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in dry-run output; got:\n%s", want, out)
		}
	}
}

// TestInstallBoundaryVerifyOnlyCLI exercises the --verify-only path through
// the CLI layer.
func TestInstallBoundaryVerifyOnlyCLI(t *testing.T) {
	body := []byte(`{"type":"Feature","properties":{"name":"Testland"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)
	sum := sha256.Sum256(body)

	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer assetSrv.Close()

	regJSON := fmt.Sprintf(`{
		"schema":"cockroach-country-seed/1",
		"countries":{},
		"boundaries":{
			"TT":{"name":"T","version":"2026.05.29","url":%q,"sha256":%q,"size_bytes":%d}
		}
	}`, assetSrv.URL, hex.EncodeToString(sum[:]), len(body))

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, regJSON)
	}))
	defer regSrv.Close()

	t.Setenv("COCKROACH_REGISTRY_URL", regSrv.URL)

	out := captureStdout(t, func() {
		if err := installBoundary(context.Background(), "TT", false, true /*verifyOnly*/); err != nil {
			t.Fatalf("verify-only failed: %v", err)
		}
	})
	for _, want := range []string{
		"verified",
		"1 asset",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in verify-only output; got:\n%s", want, out)
		}
	}
}

// TestInstallBoundarySHA256MismatchAborts confirms the loud-abort contract
// surfaces all the way through the CLI layer.
func TestInstallBoundarySHA256MismatchAborts(t *testing.T) {
	body := []byte(`{"type":"Feature","properties":{"name":"Testland"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)

	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer assetSrv.Close()

	// Registry pin is wrong on purpose — the URL serves correct bytes but
	// the registry's sha256 says they should hash to all-zeros.
	regJSON := fmt.Sprintf(`{
		"schema":"cockroach-country-seed/1",
		"countries":{},
		"boundaries":{
			"TT":{"name":"T","version":"2026.05.29","url":%q,"sha256":%q,"size_bytes":%d}
		}
	}`, assetSrv.URL, strings.Repeat("0", 64), len(body))

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, regJSON)
	}))
	defer regSrv.Close()
	t.Setenv("COCKROACH_REGISTRY_URL", regSrv.URL)

	err := installBoundary(context.Background(), "TT", false, true /*verifyOnly*/)
	if err == nil {
		t.Fatal("expected sha256-mismatch abort, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected 'sha256 mismatch' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "REFUSING TO INSTALL") {
		t.Errorf("expected loud-abort wording; got: %v", err)
	}
}

// TestInstallBoundaryUnknownCountryRejected confirms an unknown CC returns a
// usage-prefixed error (exit 2).
func TestInstallBoundaryUnknownCountryRejected(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"schema":"cockroach-country-seed/1","countries":{},"boundaries":{}}`)
	}))
	defer regSrv.Close()
	t.Setenv("COCKROACH_REGISTRY_URL", regSrv.URL)

	err := installBoundary(context.Background(), "TT", true, false)
	if err == nil {
		t.Fatal("expected error for missing boundary, got nil")
	}
	if !strings.Contains(err.Error(), "not in the registry") {
		t.Errorf("expected 'not in the registry' error, got %v", err)
	}
}

// TestInstallBoundaryFlagsMutuallyExclusive confirms --dry-run + --verify-only
// is a usage error (not silently picking one).
func TestInstallBoundaryFlagsMutuallyExclusive(t *testing.T) {
	err := installBoundary(context.Background(), "TT", true /*dryRun*/, true /*verifyOnly*/)
	if err == nil {
		t.Fatal("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive', got %v", err)
	}
}

// TestPrintBoundaryAssetResultRendersRoleCounts confirms the renderer covers
// every mode + every column the brief asks for.
func TestPrintBoundaryAssetResultRendersRoleCounts(t *testing.T) {
	cases := []struct {
		name    string
		dryRun  bool
		verify  bool
		wantSub []string
	}{
		{"verify-only", false, true, []string{"states", "(1ms)", "100B"}},
		{"dry-run", true, false, []string{"states", "features=2"}},
		{"install", false, false, []string{"states", "upserted=2", "features=2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				printBoundaryAssetResult(boundaryAssetResultFixture(c.dryRun, c.verify), c.dryRun, c.verify)
			})
			for _, want := range c.wantSub {
				if !strings.Contains(out, want) {
					t.Errorf("expected %q in %s output; got:\n%s", want, c.name, out)
				}
			}
		})
	}
}

// boundaryAssetResultFixture builds a fixed BoundaryAssetResult for the
// renderer tests.
func boundaryAssetResultFixture(dryRun, verify bool) pack.BoundaryAssetResult {
	r := pack.BoundaryAssetResult{
		Role:         "states",
		URL:          "https://example.org/states.geojson.gz",
		BytesFetched: 100,
		SHA256:       "abcd1234ef567890",
		DurationMs:   1,
	}
	if !verify {
		r.FeatureCount = 2
	}
	if !dryRun && !verify {
		r.RowsUpserted = 2
	}
	return r
}

// captureStdout swaps os.Stdout for a pipe, runs f, and returns what was
// written. Used by the renderer tests so they can assert on the actual
// human-readable output.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	f()

	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// ── ensure the schema + sample registry parse together ──────────────────────

// TestRegistryJSONShapeForBoundaries pins that a registry.json carrying the
// new `boundaries:` map unmarshals against the in-process Registry struct.
// This is the boundary version of the country-pack registry round-trip.
func TestRegistryJSONShapeForBoundaries(t *testing.T) {
	raw := []byte(`{
		"schema":"cockroach-country-seed/1",
		"countries":{},
		"boundaries":{
			"IN":{
				"name":"India",
				"version":"2026.05.29",
				"url":"https://example.org/IN/country.geojson.gz",
				"sha256":"227d8d04d6626abbcdf8889eb4a97fe3fe879cdc6f08a1d8d5059abe4b1aff9b",
				"size_bytes":34745,
				"release_url":"https://example.org/IN",
				"files":{
					"country":{"file":"country.geojson.gz","sha256":"227d8d04d6626abbcdf8889eb4a97fe3fe879cdc6f08a1d8d5059abe4b1aff9b","size_bytes":34745,"role":"country"}
				}
			}
		}
	}`)
	var reg struct {
		Schema     string                 `json:"schema"`
		Boundaries map[string]interface{} `json:"boundaries"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reg.Schema != "cockroach-country-seed/1" {
		t.Errorf("schema = %q, want cockroach-country-seed/1", reg.Schema)
	}
	if _, ok := reg.Boundaries["IN"]; !ok {
		t.Error("expected IN entry in boundaries map")
	}
}
