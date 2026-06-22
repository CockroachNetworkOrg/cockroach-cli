// Package pack implements the country reference-data pack manager used by
// `cockroach-cli pack` (CLI) and, in time, the admin "Data & Languages" panel.
//
// This is a public, reusable package (the registry client + pack/boundary
// loader: fetch static registry → download → sha256-verify → COPY-load) — the
// reuse seam other Cockroach products import.
//
// # THE MODEL — empty core, packs from a registry
//
// The framework core ships EMPTY: no country data baked into the binary, no
// gazetteer rows shipped in migrations. Each country's reference data
// (administrative hierarchy, postal_codes, departments, emergency numbers) lives in
// versioned, checksummed gzipped-CSV packs published as GitHub Release assets
// of that country's `cockroach-<cc>-seed` repo. A central, static `registry.json`
// indexes them — for each country a `version`, the `release_url`, and per pack
// the target `table`, `ON CONFLICT` key, `sha256`, expected row count, and size.
//
// The CLI fetches the registry over plain HTTPS, resolves the requested pack to
// a concrete download URL, verifies the digest, stages the CSV in a temp table,
// and applies it to the live table via an idempotent UPSERT. Same flow runs on
// dev, stage, and prod — by design, the exact bytes load everywhere.
//
// The default registry URL is hard-coded for the common case; an operator can
// point at a fork or an internal mirror by exporting COCKROACH_REGISTRY_URL.
package pack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultRegistryURL is the canonical, mirrorable, static index of every
// known Cockroach Reporters country pack. Override per-environment with
// $COCKROACH_REGISTRY_URL (the env var wins).
const DefaultRegistryURL = "https://raw.githubusercontent.com/cockroachnetworkorg/cockroach-registry/main/registry.json"

// SupportedSchema is the registry schema version this CLI understands. The
// `schema` field in registry.json is checked verbatim — bumping it is a
// deliberate breaking change that forces older CLIs to upgrade.
const SupportedSchema = "cockroach-country-seed/1"

// Registry is the parsed shape of registry.json. The country code key is the
// ISO 3166-1 alpha-2 (e.g. "PS", "IN") in upper case. The language code key is
// the ISO 639-1/3 code (lower case) — `Langs` is the Phase-2 extension; older
// registries that omit it just unmarshal to a nil map and everything else still
// works. `Boundaries` is keyed by the same ISO 3166-1 alpha-2 codes as
// `Countries` and is also a Phase-2 additive extension (per-jurisdiction
// boundary GeoJSON URLs; see plan/bootstrap-and-jurisdiction.md).
type Registry struct {
	Schema      string                  `json:"schema"`
	GeneratedAt string                  `json:"generated_at,omitempty"`
	Countries   map[string]CountryEntry `json:"countries"`
	Langs       map[string]Lang         `json:"langs,omitempty"`
	Boundaries  map[string]Boundary     `json:"boundaries,omitempty"`
}

// CountryEntry is one country's listing in the registry.
type CountryEntry struct {
	Name       string      `json:"name"`        // human label, e.g. "Palestine"
	Version    string      `json:"version"`     // e.g. "2026.05.27" (calendar-versioned by convention)
	ReleaseURL string      `json:"release_url"` // base URL — each pack's `file` is appended to it
	Packs      []Pack      `json:"packs"`       // FK-dependency order (kinds → units → …)
	License    string      `json:"license,omitempty"`
	Sources    string      `json:"sources,omitempty"`
	Assertions *Assertions `json:"assertions,omitempty"` // optional data-integrity gate; nil = no-op (back-compat)
}

// Assertions is an optional, additive data-integrity contract a publisher pins
// in the registry. When present, the importer evaluates it against the STAGING
// table of the matching pack (Assertions.Table) BEFORE the live UPSERT and
// rolls the transaction back on any mismatch — so a regression in the published
// data (a dropped row class, a garbage code) can never reach a live DB. A
// registry entry that omits `assertions` (every publisher before Wave A) leaves
// this nil and the importer skips the checks entirely.
//
// All three checks are independent and all are enforced:
//   - ByKind:           kind → exact row count over the whole staged table.
//   - ByKindState:      state_code → kind → exact row count (catches a single
//     mis-attributed seat that the grand totals would hide).
//   - ForbidCodePattern: a POSIX regex that the `code` column must NOT match
//     (e.g. "-0$" rejects the ac-X-0 placeholder class).
type Assertions struct {
	// Table the assertions apply to (e.g. "gazetteer_units"). The checks run only for
	// the pack whose target table equals this; other packs in the entry skip them.
	Table string `json:"table"`
	// ByKind maps a `kind` value to the exact number of rows that must carry it.
	ByKind map[string]int64 `json:"by_kind,omitempty"`
	// ByKindState maps state_code → (kind → exact count).
	ByKindState map[string]map[string]int64 `json:"by_kind_state,omitempty"`
	// ForbidCodePattern is a POSIX regex; any staged `code` matching it aborts.
	ForbidCodePattern string `json:"forbid_code_pattern,omitempty"`
}

// Pack is one gzipped CSV ready for COPY-load.
type Pack struct {
	File         string `json:"file"`          // e.g. "gazetteer_units.csv.gz"
	Table        string `json:"table"`         // target SQL table, e.g. "gazetteer_units"
	Conflict     string `json:"conflict"`      // ON CONFLICT key as written in SQL: "(code)" / "(country_code, kind)"
	SHA256       string `json:"sha256"`        // hex digest, pinned in the registry
	ExpectedRows int64  `json:"expected_rows"` // sanity check after gunzip
	SizeBytes    int64  `json:"size_bytes"`    // sanity check on the download
}

// Lang is one language's listing in the registry (Phase-2). Mirrors
// CountryEntry's shape but the bundles are per-app UI-translation archives
// published by `cockroach-world-language`, not gzipped CSVs.
//
// The CLI surfaces this metadata via `cockroach-cli pack info --lang <code>` and
// `cockroach-cli pack verify --lang <code>`; the actual install path is not yet
// implemented and `cockroach-cli pack install --lang <code>` redirects operators
// to `cockroach-cli lang import --lang <code>` until the Phase-2 download/staging
// path lands.
type Lang struct {
	Name       string       `json:"name"`                  // English name, e.g. "Hindi"
	NativeName string       `json:"native_name,omitempty"` // endonym, e.g. "हिन्दी"
	Version    string       `json:"version"`               // calendar version, "YYYY.MM.DD"
	ReleaseURL string       `json:"release_url"`           // GH Release base on cockroach-world-language
	License    string       `json:"license,omitempty"`     // SPDX (typically CC-BY-SA-4.0)
	Bundles    []LangBundle `json:"bundles"`               // one per app (web/admin/mobile)
}

// LangBundle is one per-app translation archive — same web/admin/mobile split
// `cockroach-world-language` already packs strings into.
type LangBundle struct {
	File           string `json:"file"`                      // e.g. "hi-web.tar.gz"
	SHA256         string `json:"sha256"`                    // hex digest, pinned
	SizeBytes      int64  `json:"size_bytes"`                // exact byte count
	App            string `json:"app"`                       // "web" | "admin" | "mobile"
	NamespaceCount int    `json:"namespace_count,omitempty"` // informational
	KeyCount       int    `json:"key_count,omitempty"`       // informational
}

// Boundary is one country's listing in the registry's `boundaries` map — a
// community-supplied compliant boundary GeoJSON URL + provenance. The framework
// does not bundle any country's boundary depiction; operators opt in either by
// installing it (`cockroach-cli pack install --boundary <cc>` — UPSERTs polygons
// into admin_units) or by setting `VITE_MAP_BOUNDARY_URL` directly (legacy
// frontend-only mode). See plan/bootstrap-and-jurisdiction.md for the legal
// posture.
//
// Phase-3 — `cockroach-cli pack install --boundary <cc>` is wired. When the entry
// carries a `Files` map (country / states / districts), the installer fetches
// each, verifies sha256, parses the GeoJSON, and UPSERTs the polygons into
// admin_units keyed by ISO 3166-2 (states) / HASC (districts). The single-file
// `URL` / `SHA256` / `SizeBytes` triple stays the entry's headline asset
// (typically the country outline) and is what `VITE_MAP_BOUNDARY_URL` resolves
// to in legacy frontend mode.
type Boundary struct {
	Name       string                  `json:"name"`                  // human label, typically the country name, e.g. "India"
	Version    string                  `json:"version"`               // calendar version, "YYYY.MM.DD"
	URL        string                  `json:"url"`                   // absolute URL of the headline GeoJSON
	SHA256     string                  `json:"sha256"`                // hex digest of the raw GeoJSON bytes at URL
	SizeBytes  int64                   `json:"size_bytes"`            // exact byte count at URL
	License    string                  `json:"license,omitempty"`     // SPDX or human description
	Sources    string                  `json:"sources,omitempty"`     // provenance + community verification
	Notes      string                  `json:"notes,omitempty"`       // caveats — MUST name the jurisdiction this depiction matches
	ReleaseURL string                  `json:"release_url,omitempty"` // optional GitHub Release base; each Files[*].File is appended to it
	Files      map[string]BoundaryFile `json:"files,omitempty"`       // optional multi-file bundle (country / states / districts)
}

// BoundaryFile is one GeoJSON file inside a Boundary's optional multi-file
// bundle. Either `URL` is set (absolute override) OR the installer joins the
// parent boundary's `ReleaseURL` + `File`. Same shape as `Pack` but tuned for
// GeoJSON instead of CSV — no SQL target table; the installer routes the
// polygons into admin_units rows by parsing the GeoJSON properties.
type BoundaryFile struct {
	File      string `json:"file"`           // e.g. "boundary-states.geojson.gz" — appended to Boundary.ReleaseURL when URL is empty
	URL       string `json:"url,omitempty"`  // optional absolute URL override
	SHA256    string `json:"sha256"`         // hex digest of the raw bytes
	SizeBytes int64  `json:"size_bytes"`     // exact byte count
	Role      string `json:"role,omitempty"` // "country" | "states" | "districts" | "subdistricts" — inferred from map key when empty
}

// FileDownloadURL is the absolute URL where the boundary file's bytes live —
// either the absolute override or the parent boundary's `ReleaseURL` + `File`.
func (b Boundary) FileDownloadURL(f BoundaryFile) string {
	if strings.TrimSpace(f.URL) != "" {
		return f.URL
	}
	base := strings.TrimRight(b.ReleaseURL, "/")
	if base == "" {
		// Fallback: derive a base from the headline URL by stripping the last
		// path segment. Lets a publisher omit ReleaseURL and still expose a
		// multi-file bundle with the per-file `file` shorthand.
		if idx := strings.LastIndex(b.URL, "/"); idx > 0 {
			base = b.URL[:idx]
		}
	}
	return base + "/" + f.File
}

// DownloadURL is the absolute URL where the pack's bytes live (release base + file).
func (c CountryEntry) DownloadURL(p Pack) string {
	base := strings.TrimRight(c.ReleaseURL, "/")
	return base + "/" + p.File
}

// DownloadURL is the absolute URL where the bundle's bytes live (release base + file).
// Phase-2 — not yet consumed by an installer; provided so the CLI's `pack info`
// can echo a concrete URL for each bundle.
func (l Lang) DownloadURL(b LangBundle) string {
	base := strings.TrimRight(l.ReleaseURL, "/")
	return base + "/" + b.File
}

// FetchRegistry loads and parses the registry index from a local file
// (file:// or a bare filesystem path) or over http(s). The body is capped at
// 1 MiB because the index is metadata and a runaway response would otherwise be
// unbounded.
func FetchRegistry(ctx context.Context, url string) (*Registry, error) {
	if url == "" {
		url = DefaultRegistryURL
	}
	body, err := fetchRegistryBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	var reg Registry
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("parse registry json: %w", err)
	}
	if reg.Schema != SupportedSchema {
		return nil, fmt.Errorf("registry schema %q is not supported by this CLI (expected %q) — upgrade `cockroach-cli`", reg.Schema, SupportedSchema)
	}
	// Upper-case country keys so callers can be relaxed about case.
	if reg.Countries != nil {
		norm := make(map[string]CountryEntry, len(reg.Countries))
		for k, v := range reg.Countries {
			norm[strings.ToUpper(strings.TrimSpace(k))] = v
		}
		reg.Countries = norm
	}
	// Lower-case language keys for the same reason. ISO 639-1/3 codes are
	// canonically lower case; the BCP-47 region tag (e.g. "pt-BR") preserves
	// its own casing rules, but we normalise the language subtag for lookup.
	if reg.Langs != nil {
		norm := make(map[string]Lang, len(reg.Langs))
		for k, v := range reg.Langs {
			norm[strings.ToLower(strings.TrimSpace(k))] = v
		}
		reg.Langs = norm
	}
	// Upper-case boundary keys — same ISO 3166-1 alpha-2 convention as Countries.
	if reg.Boundaries != nil {
		norm := make(map[string]Boundary, len(reg.Boundaries))
		for k, v := range reg.Boundaries {
			norm[strings.ToUpper(strings.TrimSpace(k))] = v
		}
		reg.Boundaries = norm
	}
	return &reg, nil
}

// fetchRegistryBytes reads the raw registry index. A local source — a `file://`
// URL or a bare filesystem path — is read straight off disk so offline /
// air-gapped operators (and dev against a local registry checkout) work without
// network; everything else is fetched over http(s) with a 30s timeout. Both
// paths cap the read at 1 MiB.
func fetchRegistryBytes(ctx context.Context, url string) ([]byte, error) {
	const maxBytes = 1 << 20

	// Local file: no scheme, or an explicit file:// scheme.
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		path := strings.TrimPrefix(url, "file://")
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open registry file %s: %w — check the path or set $COCKROACH_REGISTRY_URL", path, err)
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxBytes))
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cockroach-cli/pack")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch registry %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound, http.StatusGone:
		return nil, fmt.Errorf("registry not found at %s (HTTP %d) — check the URL or set $COCKROACH_REGISTRY_URL", url, resp.StatusCode)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("registry %s rejected the request (HTTP %d) — private registries need auth headers (not supported in this CLI version)", url, resp.StatusCode)
	default:
		return nil, fmt.Errorf("registry %s returned HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// Country looks up a country entry by ISO 3166-1 alpha-2 code (case-insensitive).
func (r *Registry) Country(cc string) (CountryEntry, bool) {
	if r == nil || r.Countries == nil {
		return CountryEntry{}, false
	}
	v, ok := r.Countries[strings.ToUpper(strings.TrimSpace(cc))]
	return v, ok
}

// CountryCodes returns the sorted list of ISO codes the registry knows about.
func (r *Registry) CountryCodes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Countries))
	for k := range r.Countries {
		out = append(out, k)
	}
	// stable order makes `cockroach-cli pack list` output deterministic.
	sortStrings(out)
	return out
}

// Lang looks up a language entry by ISO 639-1/3 code (case-insensitive). Phase-2
// — the registry's `langs` field is optional; a registry that omits it (every
// publisher today) returns (Lang{}, false) for every code.
func (r *Registry) Lang(code string) (Lang, bool) {
	if r == nil || r.Langs == nil {
		return Lang{}, false
	}
	v, ok := r.Langs[strings.ToLower(strings.TrimSpace(code))]
	return v, ok
}

// LangCodes returns the sorted list of language codes the registry knows about.
// Returns nil when no languages are published — `cockroach-cli pack list --langs`
// uses that to print the "still ship via lang import for now" guidance.
func (r *Registry) LangCodes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Langs))
	for k := range r.Langs {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// Boundary looks up a boundary entry by ISO 3166-1 alpha-2 code
// (case-insensitive). Phase-2 — the registry's `boundaries` field is optional;
// a registry that omits it (every publisher today) returns (Boundary{}, false)
// for every code.
func (r *Registry) Boundary(code string) (Boundary, bool) {
	if r == nil || r.Boundaries == nil {
		return Boundary{}, false
	}
	v, ok := r.Boundaries[strings.ToUpper(strings.TrimSpace(code))]
	return v, ok
}

// BoundaryCodes returns the sorted list of ISO codes for which the registry
// catalogs a recommended boundary GeoJSON. Returns nil when none are published
// — `cockroach-cli pack list --boundaries` uses that to print the "(none yet)"
// guidance.
func (r *Registry) BoundaryCodes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Boundaries))
	for k := range r.Boundaries {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings — local to avoid pulling in "sort" at call sites; kept here to
// keep the package's import footprint tight in one place.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
