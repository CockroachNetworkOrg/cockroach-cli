package pack

// Unit tests for the boundary installer. The DB-touching paths are exercised
// indirectly — `upsertAdminUnits` requires a real Postgres pool so it stays
// out of the pure-Go suite — but the network + verify + parse + validate
// surface is the bulk of the failure surface and that's all covered here
// with `httptest.NewServer`.
//
// Test strategy:
//   - resolveBoundaryAssets: single-file vs multi-file ordering, missing URLs.
//   - fetchAndVerify: happy path, sha256 mismatch (LOUD ABORT), size mismatch,
//     HTTP error, oversized body cap.
//   - validateGeometry: closed-ring requirement, coord-range bounds, NaN.
//   - parseBoundaryGeoJSON: country Feature, states/districts
//     FeatureCollection, property variants, bad role.
//   - end-to-end install (VerifyOnly): httptest server returning the three
//     India-shaped bundles + a synthetic FeatureCollection, asserting the
//     per-asset result list.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── resolveBoundaryAssets ─────────────────────────────────────────────────────

func TestResolveBoundaryAssetsSingleFile(t *testing.T) {
	b := Boundary{
		URL:       "https://example.org/IN/country.geojson.gz",
		SHA256:    "abc",
		SizeBytes: 100,
	}
	assets, err := resolveBoundaryAssets(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if assets[0].Role != "country" {
		t.Errorf("expected role=country, got %q", assets[0].Role)
	}
	if !assets[0].Gzipped {
		t.Errorf("expected gzipped=true for .gz suffix, got false")
	}
	if assets[0].URL != b.URL {
		t.Errorf("URL = %q, want %q", assets[0].URL, b.URL)
	}
}

func TestResolveBoundaryAssetsOrdering(t *testing.T) {
	// Map iteration order is randomised; the walker must still emit
	// country → states → districts in that fixed order.
	b := Boundary{
		ReleaseURL: "https://example.org/IN",
		URL:        "https://example.org/IN/boundary-country.geojson.gz",
		SHA256:     "aaa",
		SizeBytes:  10,
		Files: map[string]BoundaryFile{
			"districts": {File: "boundary-districts.geojson.gz", SHA256: "ddd", SizeBytes: 30, Role: "districts"},
			"states":    {File: "boundary-states.geojson.gz", SHA256: "sss", SizeBytes: 20, Role: "states"},
			"country":   {File: "boundary-country.geojson.gz", SHA256: "ccc", SizeBytes: 10, Role: "country"},
		},
	}
	assets, err := resolveBoundaryAssets(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("expected 3 assets, got %d", len(assets))
	}
	wantRoles := []string{"country", "states", "districts"}
	for i, want := range wantRoles {
		if assets[i].Role != want {
			t.Errorf("asset[%d].Role = %q, want %q", i, assets[i].Role, want)
		}
	}
	if assets[0].URL != "https://example.org/IN/boundary-country.geojson.gz" {
		t.Errorf("country URL = %q, want joined release_url + file", assets[0].URL)
	}
}

func TestResolveBoundaryAssetsEmptyEntry(t *testing.T) {
	_, err := resolveBoundaryAssets(Boundary{})
	if err == nil {
		t.Fatal("expected error for empty entry")
	}
}

// ── fetchAndVerify ────────────────────────────────────────────────────────────

func TestFetchAndVerifyHappyPath(t *testing.T) {
	body := []byte(`{"type":"Feature","geometry":{"type":"Polygon","coordinates":[]}}`)
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	bi := &BoundaryInstaller{Client: srv.Client()}
	got, err := bi.fetchAndVerify(context.Background(), boundaryAsset{
		Role:           "country",
		URL:            srv.URL,
		ExpectedSHA256: hex.EncodeToString(sum[:]),
		ExpectedSize:   int64(len(body)),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch")
	}
}

func TestFetchAndVerifySHA256MismatchAborts(t *testing.T) {
	body := []byte(`{"type":"Feature"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	bi := &BoundaryInstaller{Client: srv.Client()}
	_, err := bi.fetchAndVerify(context.Background(), boundaryAsset{
		Role:           "country",
		URL:            srv.URL,
		ExpectedSHA256: strings.Repeat("0", 64), // never matches
		ExpectedSize:   int64(len(body)),
	})
	if err == nil {
		t.Fatal("expected sha256-mismatch abort, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected 'sha256 mismatch' in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "REFUSING TO INSTALL") {
		t.Errorf("expected loud-abort wording; got %v", err)
	}
}

func TestFetchAndVerifySizeMismatchAborts(t *testing.T) {
	body := []byte(`{"type":"Feature"}`)
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	bi := &BoundaryInstaller{Client: srv.Client()}
	_, err := bi.fetchAndVerify(context.Background(), boundaryAsset{
		Role:           "country",
		URL:            srv.URL,
		ExpectedSHA256: hex.EncodeToString(sum[:]),
		ExpectedSize:   int64(len(body)) + 999, // wrong
	})
	if err == nil {
		t.Fatal("expected size-mismatch error")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("expected 'size mismatch' in error, got %v", err)
	}
}

func TestFetchAndVerifyHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	bi := &BoundaryInstaller{Client: srv.Client()}
	_, err := bi.fetchAndVerify(context.Background(), boundaryAsset{
		URL:            srv.URL,
		ExpectedSHA256: strings.Repeat("a", 64),
		ExpectedSize:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected HTTP 500 error, got %v", err)
	}
}

func TestFetchAndVerifyRefusesUnpinnedBytes(t *testing.T) {
	body := []byte(`x`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	bi := &BoundaryInstaller{Client: srv.Client()}
	_, err := bi.fetchAndVerify(context.Background(), boundaryAsset{
		URL:            srv.URL,
		ExpectedSHA256: "", // missing pin
		ExpectedSize:   int64(len(body)),
	})
	if err == nil || !strings.Contains(err.Error(), "missing sha256") {
		t.Errorf("expected 'missing sha256' error, got %v", err)
	}
}

// ── validateGeometry ──────────────────────────────────────────────────────────

func TestValidateGeometryAcceptsClosedSquare(t *testing.T) {
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}`)
	if err := validateGeometry(raw); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestValidateGeometryRejectsUnclosedRing(t *testing.T) {
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1]]]}`)
	if err := validateGeometry(raw); err == nil {
		t.Error("expected unclosed-ring error")
	}
}

func TestValidateGeometryRejectsOutOfRange(t *testing.T) {
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[200,0],[200,100],[0,100],[0,0]]]}`)
	if err := validateGeometry(raw); err == nil {
		t.Error("expected out-of-range error")
	}
}

func TestValidateGeometryRejectsTooFewPoints(t *testing.T) {
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[0,0],[1,0],[0,0]]]}`)
	if err := validateGeometry(raw); err == nil {
		t.Error("expected too-few-points error")
	}
}

func TestValidateGeometryRejectsUnsupportedType(t *testing.T) {
	raw := json.RawMessage(`{"type":"Point","coordinates":[0,0]}`)
	if err := validateGeometry(raw); err == nil {
		t.Error("expected unsupported-type error")
	}
}

// ── computeBBox ───────────────────────────────────────────────────────────────

func TestComputeBBoxPolygon(t *testing.T) {
	raw := json.RawMessage(`{"type":"Polygon","coordinates":[[[1,2],[3,2],[3,5],[1,5],[1,2]]]}`)
	bbox, err := computeBBox(raw)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := [4]float64{1, 2, 3, 5}
	if bbox != want {
		t.Errorf("bbox = %v, want %v", bbox, want)
	}
}

// ── parseBoundaryGeoJSON ──────────────────────────────────────────────────────

func TestParseCountryFeature(t *testing.T) {
	body := []byte(`{"type":"Feature","properties":{"name":"India"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)
	units, err := parseBoundaryGeoJSON(body, "IN", "country")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("expected 1 unit, got %d", len(units))
	}
	u := units[0]
	if u.ID != "IN" || u.Level != 2 || u.Name != "India" {
		t.Errorf("country unit shape wrong: %+v", u)
	}
	if u.ParentID != nil {
		t.Errorf("country unit should have NULL parent_id, got %v", *u.ParentID)
	}
}

func TestParseStatesFeatureCollection(t *testing.T) {
	body := []byte(`{
		"type":"FeatureCollection",
		"features":[
			{"type":"Feature","properties":{"state_code":"IN-DL","name":"Delhi","hasc":"IN.DL"},
			 "geometry":{"type":"Polygon","coordinates":[[[77,28],[78,28],[78,29],[77,29],[77,28]]]}},
			{"type":"Feature","properties":{"state_code":"IN-MH","name":"Maharashtra","hasc":"IN.MH"},
			 "geometry":{"type":"Polygon","coordinates":[[[73,18],[74,18],[74,19],[73,19],[73,18]]]}}
		]
	}`)
	units, err := parseBoundaryGeoJSON(body, "IN", "states")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("expected 2 units, got %d", len(units))
	}
	for _, u := range units {
		if u.Level != 4 {
			t.Errorf("expected level=4 for states, got %d", u.Level)
		}
		if u.ParentID == nil || *u.ParentID != "IN" {
			t.Errorf("expected parent_id=IN, got %v", u.ParentID)
		}
	}
	if units[0].ID != "IN-DL" || units[1].ID != "IN-MH" {
		t.Errorf("state IDs wrong: %s, %s", units[0].ID, units[1].ID)
	}
}

func TestParseDistrictsFeatureCollection(t *testing.T) {
	body := []byte(`{
		"type":"FeatureCollection",
		"features":[
			{"type":"Feature","properties":{"hasc":"IN.DL.ND","name":"New Delhi","state_code":"IN-DL"},
			 "geometry":{"type":"Polygon","coordinates":[[[77.1,28.5],[77.3,28.5],[77.3,28.7],[77.1,28.7],[77.1,28.5]]]}}
		]
	}`)
	units, err := parseBoundaryGeoJSON(body, "IN", "districts")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("expected 1 unit, got %d", len(units))
	}
	u := units[0]
	if u.ID != "IN.DL.ND" || u.Level != 6 {
		t.Errorf("district unit shape wrong: %+v", u)
	}
	if u.ParentID == nil || *u.ParentID != "IN-DL" {
		t.Errorf("expected parent_id=IN-DL, got %v", u.ParentID)
	}
}

func TestParseDistrictsDerivesParentFromHASC(t *testing.T) {
	// No state_code property — installer should derive parent from HASC.
	body := []byte(`{
		"type":"FeatureCollection",
		"features":[
			{"type":"Feature","properties":{"hasc":"IN.AP.ADILABAD","name":"Adilabad"},
			 "geometry":{"type":"Polygon","coordinates":[[[78,19],[79,19],[79,20],[78,20],[78,19]]]}}
		]
	}`)
	units, err := parseBoundaryGeoJSON(body, "IN", "districts")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if units[0].ParentID == nil || *units[0].ParentID != "IN-AP" {
		t.Errorf("expected derived parent_id=IN-AP, got %v", units[0].ParentID)
	}
}

func TestParseStatesRejectsMissingKey(t *testing.T) {
	body := []byte(`{
		"type":"FeatureCollection",
		"features":[{"type":"Feature","properties":{"name":"x"},
		 "geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}]
	}`)
	_, err := parseBoundaryGeoJSON(body, "IN", "states")
	if err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestParseUnknownRole(t *testing.T) {
	// One feature is enough to exercise the role switch (empty features array
	// never enters the loop and so doesn't fail — that's fine; the failure
	// triggers as soon as a real feature needs routing).
	body := []byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}]}`)
	_, err := parseBoundaryGeoJSON(body, "IN", "wat")
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("expected unknown-role error, got %v", err)
	}
}

func TestParseBadJSON(t *testing.T) {
	_, err := parseBoundaryGeoJSON([]byte(`not json`), "IN", "country")
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

// ── end-to-end (VerifyOnly) install ──────────────────────────────────────────

// TestInstallBoundaryVerifyOnly drives the installer's full asset walker
// against a httptest server that returns a synthetic three-asset bundle.
// VerifyOnly mode skips the DB write so the test stays pure-Go (no Postgres).
func TestInstallBoundaryVerifyOnly(t *testing.T) {
	country := []byte(`{"type":"Feature","properties":{"name":"Testland"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)
	states := []byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"state_code":"TT-01","name":"Alpha","hasc":"TT.01"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}]}`)
	districts := []byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"hasc":"TT.01.AA","name":"AlphaDistrict","state_code":"TT-01"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}]}`)

	mux := http.NewServeMux()
	mux.HandleFunc("/boundary-country.geojson", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(country) })
	mux.HandleFunc("/boundary-states.geojson", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(states) })
	mux.HandleFunc("/boundary-districts.geojson", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(districts) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cSum := sha256.Sum256(country)
	sSum := sha256.Sum256(states)
	dSum := sha256.Sum256(districts)

	b := Boundary{
		Name:       "Testland",
		Version:    "2026.05.29",
		ReleaseURL: srv.URL,
		URL:        srv.URL + "/boundary-country.geojson",
		SHA256:     hex.EncodeToString(cSum[:]),
		SizeBytes:  int64(len(country)),
		Files: map[string]BoundaryFile{
			"country":   {File: "boundary-country.geojson", SHA256: hex.EncodeToString(cSum[:]), SizeBytes: int64(len(country)), Role: "country"},
			"states":    {File: "boundary-states.geojson", SHA256: hex.EncodeToString(sSum[:]), SizeBytes: int64(len(states)), Role: "states"},
			"districts": {File: "boundary-districts.geojson", SHA256: hex.EncodeToString(dSum[:]), SizeBytes: int64(len(districts)), Role: "districts"},
		},
	}

	bi := &BoundaryInstaller{Client: srv.Client()}
	res, err := bi.InstallBoundary(context.Background(), "TT", b, BoundaryInstallOptions{VerifyOnly: true})
	if err != nil {
		t.Fatalf("install verify-only failed: %v", err)
	}
	if res.Skipped {
		t.Fatalf("verify-only should never short-circuit on the ledger")
	}
	if len(res.Assets) != 3 {
		t.Fatalf("expected 3 assets, got %d", len(res.Assets))
	}
	wantRoles := []string{"country", "states", "districts"}
	for i, r := range res.Assets {
		if r.Role != wantRoles[i] {
			t.Errorf("asset[%d].Role = %q, want %q", i, r.Role, wantRoles[i])
		}
		if r.BytesFetched == 0 {
			t.Errorf("asset[%d] had zero bytes — fetch path didn't run", i)
		}
		if r.RowsUpserted != 0 {
			t.Errorf("verify-only must not upsert rows, got %d for %s", r.RowsUpserted, r.Role)
		}
		if r.FeatureCount != 0 {
			t.Errorf("verify-only must skip parse, FeatureCount should be 0, got %d", r.FeatureCount)
		}
	}
}

// TestInstallBoundaryDryRunParses confirms the dry-run path runs the parser
// (so feature counts surface) but writes nothing.
func TestInstallBoundaryDryRunParses(t *testing.T) {
	country := []byte(`{"type":"Feature","properties":{"name":"Testland"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(country)
	}))
	defer srv.Close()
	sum := sha256.Sum256(country)

	b := Boundary{
		Name:      "Testland",
		Version:   "2026.05.29",
		URL:       srv.URL,
		SHA256:    hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(country)),
	}
	bi := &BoundaryInstaller{Client: srv.Client()}
	res, err := bi.InstallBoundary(context.Background(), "TT", b, BoundaryInstallOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	if len(res.Assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(res.Assets))
	}
	if res.Assets[0].FeatureCount != 1 {
		t.Errorf("expected FeatureCount=1 on dry-run, got %d", res.Assets[0].FeatureCount)
	}
	if res.Assets[0].RowsUpserted != 0 {
		t.Errorf("dry-run must not upsert rows, got %d", res.Assets[0].RowsUpserted)
	}
}

// TestInstallBoundaryOversizedAborts confirms that a body bigger than the
// pinned size is rejected — defends against a mirror swapping the URL for a
// runaway payload.
func TestInstallBoundaryOversizedAborts(t *testing.T) {
	body := []byte(`{"type":"Feature"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server lies — declares small in registry, returns much larger.
		extra := strings.Repeat("x", 4096)
		_, _ = fmt.Fprint(w, string(body)+extra)
	}))
	defer srv.Close()
	sum := sha256.Sum256(body)

	b := Boundary{
		URL:       srv.URL,
		SHA256:    hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(body)), // pinned small
	}
	bi := &BoundaryInstaller{Client: srv.Client()}
	_, err := bi.InstallBoundary(context.Background(), "TT", b, BoundaryInstallOptions{VerifyOnly: true})
	if err == nil {
		t.Fatal("expected size-mismatch abort on inflated body")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("expected size-mismatch wording; got %v", err)
	}
}

// TestRegistryRoundTripBoundaries confirms a registry.json carrying a
// `boundaries:` map round-trips through FetchRegistry-style unmarshalling.
// We don't hit a server — just exercise the unmarshal + lookup contract.
func TestRegistryRoundTripBoundaries(t *testing.T) {
	raw := []byte(`{
		"schema":"cockroach-country-seed/1",
		"countries":{},
		"boundaries":{
			"IN":{
				"name":"India","version":"2026.05.29",
				"url":"https://example.org/IN/country.geojson.gz",
				"sha256":"227d8d04d6626abbcdf8889eb4a97fe3fe879cdc6f08a1d8d5059abe4b1aff9b",
				"size_bytes":34745,
				"release_url":"https://example.org/IN",
				"files":{
					"country":{"file":"country.geojson.gz","sha256":"aaaa","size_bytes":10,"role":"country"},
					"states":{"file":"states.geojson.gz","sha256":"bbbb","size_bytes":20,"role":"states"}
				}
			}
		}
	}`)
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, ok := reg.Boundaries["IN"]
	if !ok {
		t.Fatal("missing IN boundary entry")
	}
	if len(b.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(b.Files))
	}
	url := b.FileDownloadURL(b.Files["states"])
	if url != "https://example.org/IN/states.geojson.gz" {
		t.Errorf("FileDownloadURL = %q, want joined release_url+file", url)
	}
}
