package pack

// Boundary installer — the bytes-to-admin_units half of
// `cockroach-cli pack install --boundary <cc>`.
//
// Flow per boundary entry:
//
//   1. Resolve the registry entry to its concrete asset list. A single-file
//      entry is treated as one country-role asset; a multi-file entry walks
//      every `files[*]` (country / states / districts) in fixed order.
//   2. For each asset: GET → enforce size cap + size_bytes match → sha256
//      verify (LOUD ABORT on mismatch — never write partial data) → gunzip if
//      `.geojson.gz`.
//   3. Parse the GeoJSON as a Feature (country role) or FeatureCollection
//      (states/districts) and validate each polygon (closed rings, coord
//      ranges).
//   4. Build admin_units row tuples keyed by `id` (HASC for districts;
//      ISO 3166-2 for states; country code for country). Resolve parent_id
//      pointers within the batch.
//   5. UPSERT into admin_units in a single transaction per role, refreshing
//      the polygon + bbox + source_version. The country-code column is
//      denormalised; the bbox is computed in Go (no PostGIS).
//   6. Record provenance in `installed_boundary_packs` (URL + sha256 of the
//      HEADLINE asset, version, license, notes — the per-file ledger isn't
//      needed because the registry pins those once and re-runs are
//      idempotent).
//
// Loud-abort contract: a sha256 mismatch, a size_bytes mismatch, an unparseable
// GeoJSON, or a feature missing its keying property (state_code / hasc) ABORTS
// the whole install with a clear error. Nothing partial is ever written to
// admin_units. This matches `cockroach-cli pack install --country`'s contract.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxBoundaryBytes caps a single GeoJSON download (gzipped). India's districts
// pack is ~1.2 MiB today; 256 MiB is enormous headroom while still refusing a
// rogue mirror that swaps the URL for a bottomless body.
const maxBoundaryBytes int64 = 256 << 20

// maxBoundaryDecompressed caps the gunzipped GeoJSON to defend against zip
// bombs. 4 × the gzipped cap is enough for any realistic GeoJSON.
const maxBoundaryDecompressed int64 = maxBoundaryBytes * 8

// BoundaryInstallOptions controls one `cockroach-cli pack install --boundary`
// invocation.
type BoundaryInstallOptions struct {
	DryRun      bool         // fetch + verify + parse, but write nothing
	VerifyOnly  bool         // fetch + verify only; skip parse + write (faster sha-only check)
	InstalledBy string       // ledger attribution: "cli", "cli-bootstrap", "admin:<uid>", …
	RegistryURL string       // for the installed_boundary_packs.notes audit (informational)
	HTTPClient  *http.Client // optional override (tests inject httptest server clients)
}

// BoundaryAssetResult is the per-file report InstallBoundary returns.
type BoundaryAssetResult struct {
	Role         string // "country" | "states" | "districts"
	URL          string
	BytesFetched int64
	SHA256       string
	FeatureCount int   // GeoJSON features parsed (0 for verify-only mode)
	RowsUpserted int64 // admin_units rows written (0 on dry-run / verify-only)
	DurationMs   int64
}

// BoundaryInstallResult is the rolled-up report for one install.
type BoundaryInstallResult struct {
	Country  string
	Version  string
	URL      string // headline asset URL (used as the installed_boundary_packs.url value)
	SHA256   string // headline asset sha256 (used as the installed_boundary_packs.sha256 value)
	Assets   []BoundaryAssetResult
	Skipped  bool   // true when already installed at this sha256
	SkipNote string // populated when Skipped is true
}

// BoundaryInstaller wraps the DB pool + HTTP client a boundary install needs.
// Mirror of `Installer` for the country-pack flow; kept separate so the two
// can evolve independently (country packs are CSV/COPY-driven; boundaries are
// JSON/UPSERT-driven and there's no temp-table staging step).
type BoundaryInstaller struct {
	DB     *pgxpool.Pool
	Client *http.Client
}

// NewBoundaryInstaller returns an installer with a sensible default HTTP
// client (10-minute timeout — boundary downloads are 1-10 MB and a slow CDN
// shouldn't false-fail).
func NewBoundaryInstaller(db *pgxpool.Pool) *BoundaryInstaller {
	return &BoundaryInstaller{DB: db, Client: &http.Client{Timeout: httpFetchTimeout}}
}

// InstallBoundary executes the install for one country's boundary entry.
// Single-file entries (no `Files` map) install just the headline country
// polygon. Multi-file entries install country + states + districts in that
// fixed order (FK-style ordering — districts reference states' parent_id, so
// states must land first).
func (bi *BoundaryInstaller) InstallBoundary(
	ctx context.Context, cc string, b Boundary, opts BoundaryInstallOptions,
) (BoundaryInstallResult, error) {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	res := BoundaryInstallResult{
		Country: cc,
		Version: b.Version,
		URL:     b.URL,
		SHA256:  strings.ToLower(b.SHA256),
	}

	// Short-circuit: when the headline sha256 already matches what's in the
	// ledger AND we're not dry-running / verify-only, treat as a no-op. Saves
	// a network round-trip on idempotent re-runs.
	if !opts.DryRun && !opts.VerifyOnly && bi.DB != nil {
		var have string
		_ = bi.DB.QueryRow(ctx,
			`SELECT sha256 FROM installed_boundary_packs WHERE country_code=$1`,
			cc,
		).Scan(&have)
		if have != "" && strings.EqualFold(have, b.SHA256) {
			res.Skipped = true
			res.SkipNote = "already at sha256 " + short(b.SHA256)
			return res, nil
		}
	}

	assets, err := resolveBoundaryAssets(b)
	if err != nil {
		return res, err
	}

	for _, a := range assets {
		started := time.Now()
		ar := BoundaryAssetResult{Role: a.Role, URL: a.URL}

		raw, err := bi.fetchAndVerify(ctx, a)
		if err != nil {
			return res, fmt.Errorf("%s (%s): %w", a.Role, a.URL, err)
		}
		ar.BytesFetched = int64(len(raw))
		ar.SHA256 = strings.ToLower(a.ExpectedSHA256)

		// VerifyOnly stops here — bytes verified, no parse, no write.
		if opts.VerifyOnly {
			ar.DurationMs = time.Since(started).Milliseconds()
			res.Assets = append(res.Assets, ar)
			continue
		}

		body, err := maybeGunzip(raw, a.Gzipped)
		if err != nil {
			return res, fmt.Errorf("%s (%s): gunzip: %w", a.Role, a.URL, err)
		}

		units, err := parseBoundaryGeoJSON(body, cc, a.Role)
		if err != nil {
			return res, fmt.Errorf("%s (%s): parse: %w", a.Role, a.URL, err)
		}
		ar.FeatureCount = len(units)

		if !opts.DryRun {
			n, err := bi.upsertAdminUnits(ctx, units, b.Version, cc, a.Role)
			if err != nil {
				return res, fmt.Errorf("%s (%s): upsert: %w", a.Role, a.URL, err)
			}
			ar.RowsUpserted = n
		}
		ar.DurationMs = time.Since(started).Milliseconds()
		res.Assets = append(res.Assets, ar)
	}

	// Record provenance — only on a real (non-dry, non-verify) install.
	if !opts.DryRun && !opts.VerifyOnly {
		if err := bi.recordInstalled(ctx, cc, b, opts); err != nil {
			return res, fmt.Errorf("record installed_boundary_packs: %w", err)
		}
	}
	return res, nil
}

// boundaryAsset is one resolved (URL, sha256, role) tuple ready for fetch.
type boundaryAsset struct {
	Role           string
	URL            string
	ExpectedSHA256 string
	ExpectedSize   int64
	Gzipped        bool
}

// resolveBoundaryAssets walks a Boundary entry into its ordered asset list.
// Multi-file entries install country → states → districts (FK-style); a
// single-file entry yields one country-role asset.
func resolveBoundaryAssets(b Boundary) ([]boundaryAsset, error) {
	if len(b.Files) == 0 {
		// Single-file mode: the headline URL is the country outline.
		if strings.TrimSpace(b.URL) == "" {
			return nil, fmt.Errorf("boundary entry has no URL and no files map")
		}
		return []boundaryAsset{{
			Role:           "country",
			URL:            b.URL,
			ExpectedSHA256: b.SHA256,
			ExpectedSize:   b.SizeBytes,
			Gzipped:        strings.HasSuffix(strings.ToLower(b.URL), ".gz"),
		}}, nil
	}

	// Multi-file mode: walk the files map in a fixed role order so districts
	// land after states (FK invariant: a district's parent_id points at a
	// state we just inserted).
	want := []string{"country", "states", "districts", "subdistricts"}
	seen := make(map[string]bool, len(b.Files))
	var assets []boundaryAsset
	for _, role := range want {
		for key, f := range b.Files {
			r := f.Role
			if r == "" {
				r = key
			}
			if r != role || seen[key] {
				continue
			}
			seen[key] = true
			url := b.FileDownloadURL(f)
			if url == "" {
				return nil, fmt.Errorf("boundary file %q (role=%s) has no resolvable URL", key, role)
			}
			assets = append(assets, boundaryAsset{
				Role:           role,
				URL:            url,
				ExpectedSHA256: f.SHA256,
				ExpectedSize:   f.SizeBytes,
				Gzipped:        strings.HasSuffix(strings.ToLower(f.File), ".gz"),
			})
		}
	}
	// Any leftover files with a non-standard role get appended at the tail —
	// keeps the schema additive (a future "blocks" role still installs).
	keys := make([]string, 0, len(b.Files))
	for k := range b.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		f := b.Files[key]
		r := f.Role
		if r == "" {
			r = key
		}
		url := b.FileDownloadURL(f)
		if url == "" {
			return nil, fmt.Errorf("boundary file %q (role=%s) has no resolvable URL", key, r)
		}
		assets = append(assets, boundaryAsset{
			Role:           r,
			URL:            url,
			ExpectedSHA256: f.SHA256,
			ExpectedSize:   f.SizeBytes,
			Gzipped:        strings.HasSuffix(strings.ToLower(f.File), ".gz"),
		})
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("boundary entry has files map but no resolvable assets")
	}
	return assets, nil
}

// fetchAndVerify downloads a boundary asset, enforces size bounds, and
// rejects a sha256 mismatch. LOUD ABORT on any mismatch — same contract as
// the country-pack fetcher. The whole body is buffered because we need the
// digest before we trust the bytes.
func (bi *BoundaryInstaller) fetchAndVerify(ctx context.Context, a boundaryAsset) ([]byte, error) {
	client := bi.Client
	if client == nil {
		client = &http.Client{Timeout: httpFetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "cockroach-cli/pack")
	req.Header.Set("Accept", "application/json, application/geo+json, application/gzip, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBoundaryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBoundaryBytes {
		return nil, fmt.Errorf("body exceeds %s cap", humanByteSize(maxBoundaryBytes))
	}
	if a.ExpectedSize > 0 && int64(len(body)) != a.ExpectedSize {
		return nil, fmt.Errorf("size mismatch: got %d bytes, registry pinned %d", len(body), a.ExpectedSize)
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	want := strings.ToLower(strings.TrimSpace(a.ExpectedSHA256))
	if want == "" {
		return nil, fmt.Errorf("registry entry missing sha256 — refusing to install unpinned bytes")
	}
	if got != want {
		// LOUD ABORT — never write a partial install on a digest mismatch.
		return nil, fmt.Errorf("sha256 mismatch (REFUSING TO INSTALL):\n  got    %s\n  expect %s", got, want)
	}
	return body, nil
}

// maybeGunzip transparently decompresses .geojson.gz payloads. Plain
// .geojson is returned unchanged.
func maybeGunzip(raw []byte, gzipped bool) ([]byte, error) {
	if !gzipped {
		return raw, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(io.LimitReader(gz, maxBoundaryDecompressed))
}

// parsedAdminUnit is what parseBoundaryGeoJSON returns — one row ready to
// UPSERT into admin_units.
type parsedAdminUnit struct {
	ID          string
	ParentID    *string
	Level       int16
	Name        string
	NameNative  *string
	CountryCode string
	ISO31662    *string
	HASC        *string
	Polygon     json.RawMessage // raw geometry — UPSERTed as JSONB
	BBox        [4]float64      // [minLng, minLat, maxLng, maxLat]
}

// boundaryFeature is the minimal GeoJSON-Feature projection the parser cares
// about. We keep `Properties` as a raw map so the role-specific extractors
// can pluck the keying property they need (state_code / hasc / etc.) without
// coupling the parser to a frozen property schema.
type boundaryFeature struct {
	Type       string          `json:"type"`
	Properties map[string]any  `json:"properties"`
	Geometry   json.RawMessage `json:"geometry"`
}

// boundaryFeatureCollection is a GeoJSON FeatureCollection. The "_*" fields
// are the publisher-side metadata cockroach-india-seed embeds at the top
// level; we tolerate any extras (json.RawMessage on Features ignores them).
type boundaryFeatureCollection struct {
	Type     string            `json:"type"`
	Features []boundaryFeature `json:"features"`
}

// parseBoundaryGeoJSON parses one role's GeoJSON into a list of admin_units
// rows. The country role yields one row keyed by the country code; the
// states role yields one row per feature keyed by `state_code` (or
// `iso_3166_2`, `hasc`); the districts role yields one row per feature
// keyed by `hasc` (or `district_code`).
func parseBoundaryGeoJSON(body []byte, countryCode, role string) ([]parsedAdminUnit, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	cc := strings.ToUpper(strings.TrimSpace(countryCode))

	// Peek at the top-level "type" without re-parsing twice.
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}

	switch head.Type {
	case "Feature":
		// Country role only — a single polygon for the whole country.
		var f boundaryFeature
		if err := json.Unmarshal(body, &f); err != nil {
			return nil, fmt.Errorf("parse Feature: %w", err)
		}
		row, err := buildCountryUnit(f, cc)
		if err != nil {
			return nil, err
		}
		return []parsedAdminUnit{row}, nil
	case "FeatureCollection":
		var fc boundaryFeatureCollection
		if err := json.Unmarshal(body, &fc); err != nil {
			return nil, fmt.Errorf("parse FeatureCollection: %w", err)
		}
		var out []parsedAdminUnit
		for i, f := range fc.Features {
			var (
				row parsedAdminUnit
				err error
			)
			switch role {
			case "country":
				row, err = buildCountryUnit(f, cc)
			case "states":
				row, err = buildStateUnit(f, cc)
			case "districts":
				row, err = buildDistrictUnit(f, cc)
			case "subdistricts":
				row, err = buildSubdistrictUnit(f, cc)
			default:
				return nil, fmt.Errorf("unknown role %q (expected country/states/districts/subdistricts)", role)
			}
			if err != nil {
				return nil, fmt.Errorf("feature %d (%s): %w", i, role, err)
			}
			out = append(out, row)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected GeoJSON top-level type %q (want Feature or FeatureCollection)", head.Type)
	}
}

// buildCountryUnit shapes the country polygon as an admin_units row keyed
// by the country code. parent_id is NULL (root of the hierarchy).
func buildCountryUnit(f boundaryFeature, cc string) (parsedAdminUnit, error) {
	if err := validateGeometry(f.Geometry); err != nil {
		return parsedAdminUnit{}, err
	}
	bbox, err := computeBBox(f.Geometry)
	if err != nil {
		return parsedAdminUnit{}, fmt.Errorf("bbox: %w", err)
	}
	name := stringProp(f.Properties, "name", "country", "country_name")
	if name == "" {
		name = cc
	}
	iso := cc
	return parsedAdminUnit{
		ID:          cc,
		ParentID:    nil,
		Level:       2, // OSM admin_level for country
		Name:        name,
		CountryCode: cc,
		ISO31662:    &iso,
		Polygon:     f.Geometry,
		BBox:        bbox,
	}, nil
}

// buildStateUnit shapes one state feature. The state_code property is the
// canonical key (e.g. "IN-AP"); we accept iso_3166_2 / hasc as fallbacks so
// publishers with slightly different schemas still install.
func buildStateUnit(f boundaryFeature, cc string) (parsedAdminUnit, error) {
	if err := validateGeometry(f.Geometry); err != nil {
		return parsedAdminUnit{}, err
	}
	bbox, err := computeBBox(f.Geometry)
	if err != nil {
		return parsedAdminUnit{}, fmt.Errorf("bbox: %w", err)
	}
	state := stringProp(f.Properties, "state_code", "iso_3166_2", "iso3166_2")
	if state == "" {
		state = stringProp(f.Properties, "code")
	}
	if state == "" {
		return parsedAdminUnit{}, fmt.Errorf("missing state_code / iso_3166_2 property")
	}
	state = strings.ToUpper(state)
	hasc := stringProp(f.Properties, "hasc")
	if hasc == "" {
		// Synthesise from state code: "IN-AP" → "IN.AP".
		hasc = strings.ReplaceAll(state, "-", ".")
	}
	name := stringProp(f.Properties, "name", "state_name")
	if name == "" {
		name = state
	}
	parentID := cc
	isoP := state
	hascP := hasc
	return parsedAdminUnit{
		ID:          state,
		ParentID:    &parentID,
		Level:       4, // OSM admin_level for state/province
		Name:        name,
		CountryCode: cc,
		ISO31662:    &isoP,
		HASC:        &hascP,
		Polygon:     f.Geometry,
		BBox:        bbox,
	}, nil
}

// buildDistrictUnit shapes one district feature. The HASC property is the
// canonical key (e.g. "IN.AP.ADILABAD"); district_code / state_code are used
// to derive parent_id when needed.
func buildDistrictUnit(f boundaryFeature, cc string) (parsedAdminUnit, error) {
	if err := validateGeometry(f.Geometry); err != nil {
		return parsedAdminUnit{}, err
	}
	bbox, err := computeBBox(f.Geometry)
	if err != nil {
		return parsedAdminUnit{}, fmt.Errorf("bbox: %w", err)
	}
	hasc := stringProp(f.Properties, "hasc")
	if hasc == "" {
		return parsedAdminUnit{}, fmt.Errorf("missing hasc property")
	}
	hasc = strings.ToUpper(hasc)
	state := stringProp(f.Properties, "state_code", "iso_3166_2")
	if state == "" {
		// Derive from HASC: "IN.AP.ADILABAD" → "IN-AP".
		parts := strings.SplitN(hasc, ".", 3)
		if len(parts) >= 2 {
			state = parts[0] + "-" + parts[1]
		}
	}
	state = strings.ToUpper(state)
	name := stringProp(f.Properties, "name", "district_name")
	if name == "" {
		name = hasc
	}
	parentID := state
	hascP := hasc
	return parsedAdminUnit{
		ID:          hasc,
		ParentID:    &parentID,
		Level:       6, // OSM admin_level for district
		Name:        name,
		CountryCode: cc,
		HASC:        &hascP,
		Polygon:     f.Geometry,
		BBox:        bbox,
	}, nil
}

// buildSubdistrictUnit shapes one subdistrict / taluka / tehsil feature.
// Schema-tolerant: any of `hasc`, `subdistrict_code`, or `code` is accepted
// as the canonical ID. Parent district is derived from the HASC prefix.
func buildSubdistrictUnit(f boundaryFeature, cc string) (parsedAdminUnit, error) {
	if err := validateGeometry(f.Geometry); err != nil {
		return parsedAdminUnit{}, err
	}
	bbox, err := computeBBox(f.Geometry)
	if err != nil {
		return parsedAdminUnit{}, fmt.Errorf("bbox: %w", err)
	}
	hasc := stringProp(f.Properties, "hasc", "subdistrict_code", "code")
	if hasc == "" {
		return parsedAdminUnit{}, fmt.Errorf("missing hasc / subdistrict_code property")
	}
	hasc = strings.ToUpper(hasc)
	parent := stringProp(f.Properties, "district_code", "parent_hasc")
	if parent == "" {
		parts := strings.Split(hasc, ".")
		if len(parts) >= 3 {
			parent = strings.Join(parts[:3], ".")
		}
	}
	name := stringProp(f.Properties, "name", "subdistrict_name")
	if name == "" {
		name = hasc
	}
	parentID := parent
	hascP := hasc
	return parsedAdminUnit{
		ID:          hasc,
		ParentID:    &parentID,
		Level:       8, // OSM admin_level for subdistrict / municipality
		Name:        name,
		CountryCode: cc,
		HASC:        &hascP,
		Polygon:     f.Geometry,
		BBox:        bbox,
	}, nil
}

// stringProp returns the first non-empty string value among the named
// properties. Tolerant of `map[string]any` JSON decoding (number-shaped IDs
// are coerced to their string form).
func stringProp(props map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := props[k]
		if !ok {
			continue
		}
		switch s := v.(type) {
		case string:
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		case float64:
			// numbers can appear here for census codes etc.
			return strings.TrimSpace(fmt.Sprintf("%v", s))
		}
	}
	return ""
}

// validateGeometry confirms the GeoJSON geometry is a Polygon or
// MultiPolygon, that every ring closes (first point == last point), and
// that every coordinate is in WGS84 range. The check is structural — we
// don't do self-intersection here (the typical publisher pipeline already
// validates upstream; doing it again is expensive and out of scope).
func validateGeometry(raw json.RawMessage) error {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("geometry not JSON: %w", err)
	}
	switch g.Type {
	case "Polygon":
		var rings [][][]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return fmt.Errorf("Polygon coordinates: %w", err)
		}
		return validateRings(rings)
	case "MultiPolygon":
		var polys [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &polys); err != nil {
			return fmt.Errorf("MultiPolygon coordinates: %w", err)
		}
		for _, rings := range polys {
			if err := validateRings(rings); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported geometry type %q (want Polygon or MultiPolygon)", g.Type)
	}
}

// validateRings confirms each ring has ≥4 points, closes, and every coord
// is in WGS84 range.
func validateRings(rings [][][]float64) error {
	for ri, ring := range rings {
		if len(ring) < 4 {
			return fmt.Errorf("ring %d: only %d points (need ≥4 for a closed polygon)", ri, len(ring))
		}
		first, last := ring[0], ring[len(ring)-1]
		if len(first) < 2 || len(last) < 2 {
			return fmt.Errorf("ring %d: coords missing lng/lat", ri)
		}
		if first[0] != last[0] || first[1] != last[1] {
			return fmt.Errorf("ring %d: not closed (first %v vs last %v)", ri, first, last)
		}
		for ci, c := range ring {
			if len(c) < 2 {
				return fmt.Errorf("ring %d coord %d: missing lng/lat", ri, ci)
			}
			lng, lat := c[0], c[1]
			if math.IsNaN(lng) || math.IsNaN(lat) || math.IsInf(lng, 0) || math.IsInf(lat, 0) {
				return fmt.Errorf("ring %d coord %d: non-finite", ri, ci)
			}
			if lng < -180.0 || lng > 180.0 {
				return fmt.Errorf("ring %d coord %d: lng %g out of [-180, 180]", ri, ci, lng)
			}
			if lat < -90.0 || lat > 90.0 {
				return fmt.Errorf("ring %d coord %d: lat %g out of [-90, 90]", ri, ci, lat)
			}
		}
	}
	return nil
}

// computeBBox returns [minLng, minLat, maxLng, maxLat] for the geometry.
// Matches the format the platform_settings trigger writes — denormalised
// for cheap point-in-bbox filtering.
func computeBBox(raw json.RawMessage) ([4]float64, error) {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return [4]float64{}, err
	}
	minLng, minLat := math.Inf(1), math.Inf(1)
	maxLng, maxLat := math.Inf(-1), math.Inf(-1)
	walk := func(rings [][][]float64) {
		for _, ring := range rings {
			for _, c := range ring {
				if len(c) < 2 {
					continue
				}
				if c[0] < minLng {
					minLng = c[0]
				}
				if c[0] > maxLng {
					maxLng = c[0]
				}
				if c[1] < minLat {
					minLat = c[1]
				}
				if c[1] > maxLat {
					maxLat = c[1]
				}
			}
		}
	}
	switch g.Type {
	case "Polygon":
		var rings [][][]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return [4]float64{}, err
		}
		walk(rings)
	case "MultiPolygon":
		var polys [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &polys); err != nil {
			return [4]float64{}, err
		}
		for _, p := range polys {
			walk(p)
		}
	default:
		return [4]float64{}, fmt.Errorf("bbox: unsupported geometry type %q", g.Type)
	}
	if math.IsInf(minLng, 1) {
		return [4]float64{}, fmt.Errorf("bbox: empty geometry")
	}
	return [4]float64{minLng, minLat, maxLng, maxLat}, nil
}

// upsertAdminUnits writes one role's parsed units into admin_units inside a
// single transaction. The country-code is denormalised; source is "pack:<cc>".
// The UPSERT key is the primary key `id`; secondary unique keys
// (`country_code, iso_3166_2`) and (`hasc`) line up because every row in
// this batch is keyed by the same canonical code.
func (bi *BoundaryInstaller) upsertAdminUnits(
	ctx context.Context, units []parsedAdminUnit, version, cc, role string,
) (int64, error) {
	if len(units) == 0 {
		return 0, nil
	}
	if bi.DB == nil {
		return 0, fmt.Errorf("nil DB pool")
	}
	tx, err := bi.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	source := "pack:" + cc
	var n int64
	for _, u := range units {
		bboxJSON, err := json.Marshal(u.BBox[:])
		if err != nil {
			return 0, fmt.Errorf("marshal bbox: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO admin_units (
				id, parent_id, level, name, name_native,
				country_code, iso_3166_2, hasc,
				polygon, bbox, source, source_version
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8,
				$9, $10, $11, $12
			)
			ON CONFLICT (id) DO UPDATE SET
				parent_id      = EXCLUDED.parent_id,
				level          = EXCLUDED.level,
				name           = EXCLUDED.name,
				name_native    = EXCLUDED.name_native,
				country_code   = EXCLUDED.country_code,
				iso_3166_2     = EXCLUDED.iso_3166_2,
				hasc           = EXCLUDED.hasc,
				polygon        = EXCLUDED.polygon,
				bbox           = EXCLUDED.bbox,
				source         = EXCLUDED.source,
				source_version = EXCLUDED.source_version
		`,
			u.ID, u.ParentID, u.Level, u.Name, u.NameNative,
			u.CountryCode, u.ISO31662, u.HASC,
			[]byte(u.Polygon), bboxJSON, source, version,
		)
		if err != nil {
			return 0, fmt.Errorf("upsert %s (%s): %w", u.ID, role, err)
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return n, nil
}

// recordInstalled writes (or refreshes) the installed_boundary_packs ledger
// row. Mirrors the country-pack ledger contract: country_code is the PK so
// re-installs UPSERT atomically.
func (bi *BoundaryInstaller) recordInstalled(
	ctx context.Context, cc string, b Boundary, opts BoundaryInstallOptions,
) error {
	if bi.DB == nil {
		return nil
	}
	notes := b.Notes
	if opts.RegistryURL != "" {
		if notes != "" {
			notes = notes + " | registry: " + opts.RegistryURL
		} else {
			notes = "registry: " + opts.RegistryURL
		}
	}
	installedBy := opts.InstalledBy
	if installedBy == "" {
		installedBy = "cli"
	}
	_, err := bi.DB.Exec(ctx, `
		INSERT INTO installed_boundary_packs (
			country_code, url, sha256, installed_at, installed_by,
			version, license, notes
		) VALUES ($1, $2, $3, NOW(), $4, $5, $6, $7)
		ON CONFLICT (country_code) DO UPDATE SET
			url          = EXCLUDED.url,
			sha256       = EXCLUDED.sha256,
			installed_at = NOW(),
			installed_by = EXCLUDED.installed_by,
			version      = EXCLUDED.version,
			license      = EXCLUDED.license,
			notes        = EXCLUDED.notes
	`,
		cc, b.URL, strings.ToLower(b.SHA256), installedBy,
		nullable(b.Version), nullable(b.License), nullable(notes),
	)
	return err
}

// InstalledBoundaryPack is one row of the installed_boundary_packs ledger,
// surfaced by ListInstalledBoundaries.
type InstalledBoundaryPack struct {
	CountryCode string
	URL         string
	SHA256      string
	Version     string
	License     string
	Notes       string
	InstalledAt time.Time
	InstalledBy string
}

// ListInstalledBoundaries reads the installed_boundary_packs ledger ordered
// by country code. Pair with `cockroach-cli pack list --installed` to surface
// boundary installs alongside country-pack installs.
func ListInstalledBoundaries(ctx context.Context, db *pgxpool.Pool) ([]InstalledBoundaryPack, error) {
	rows, err := db.Query(ctx, `
		SELECT country_code, url, sha256,
		       COALESCE(version, ''), COALESCE(license, ''), COALESCE(notes, ''),
		       installed_at, installed_by
		  FROM installed_boundary_packs
		 ORDER BY country_code
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstalledBoundaryPack
	for rows.Next() {
		var p InstalledBoundaryPack
		if err := rows.Scan(
			&p.CountryCode, &p.URL, &p.SHA256,
			&p.Version, &p.License, &p.Notes,
			&p.InstalledAt, &p.InstalledBy,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// humanByteSize formats a byte count for error messages — local to this
// file so it doesn't compete with the CLI's humanBytes formatter.
func humanByteSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
