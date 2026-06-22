-- 002_jurisdiction.sql  (civic-core)
--
-- The CORE half of the jurisdiction hierarchy: the self-contained civic-schema
-- reference + audit tables that every product instance needs, with NO
-- dependency on any product table. Implements plan/jurisdiction-hierarchy-locked.md
-- §2 (schema contract), CORE subset.
--
-- The PRODUCT half — the platform_settings scope envelope (scope_level/state/
-- district/polygon/bbox/mode + CHECKs + recompute_bbox trigger + backfill) and
-- the reports.admin_unit_id/archived_* columns + FK + indexes — lives in the
-- product's own migration (reporters backend/db/migrations/078_jurisdiction_hierarchy.sql),
-- which runs AFTER both this core file and the product tables it mutates.
--
-- Three things this core file delivers:
--
--   2b. admin_units: hierarchical, ID-stable (ISO 3166 / HASC) reference table.
--   2d. scope_changes: append-only audit log for every scope operation.
--   2e. installed_boundary_packs: provenance ledger for imported polygons
--       (mirror of installed_packs from 069 so CLI patterns stay uniform).

-- ── 2b. admin_units — hierarchical reference table ─────────────────────────

CREATE TABLE IF NOT EXISTS public.admin_units (
    id              TEXT PRIMARY KEY,                 -- stable code: 'IN', 'IN-DL', 'IN.DL.ND'
    parent_id       TEXT REFERENCES public.admin_units(id) ON DELETE RESTRICT,
    level           SMALLINT NOT NULL,                -- OSM admin_level: 2/4/6/8/…
    name            TEXT NOT NULL,
    name_native     TEXT,
    country_code    CHAR(2) NOT NULL,                 -- ISO 3166-1 alpha-2; denormalised
    iso_3166_2      TEXT,                             -- e.g. 'IN-DL'
    hasc            TEXT,                             -- e.g. 'IN.DL.ND'
    polygon         JSONB,                            -- simplified GeoJSON (~50KB)
    polygon_full_url TEXT,                            -- optional URL to full-precision GeoJSON
    bbox            JSONB NOT NULL,                   -- [minLng, minLat, maxLng, maxLat]
    population      BIGINT,
    source          TEXT NOT NULL,                    -- 'osm' | 'gadm' | 'geoboundaries' | 'pack:<cc>'
    source_version  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (country_code, iso_3166_2),
    UNIQUE (hasc)
);

CREATE INDEX IF NOT EXISTS admin_units_parent
    ON public.admin_units(parent_id);
CREATE INDEX IF NOT EXISTS admin_units_country_level
    ON public.admin_units(country_code, level);

COMMENT ON TABLE public.admin_units IS
    'Hierarchical admin units (country → state → district → …), keyed by stable codes (ISO 3166-2 / HASC). The polygon-resolver service walks parent_id to find the leaf-most unit containing a point. See plan/jurisdiction-hierarchy-locked.md §2b.';

-- ── 2d. scope_changes — append-only audit ──────────────────────────────────

CREATE TABLE IF NOT EXISTS public.scope_changes (
    id                     BIGSERIAL PRIMARY KEY,
    changed_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    changed_by             TEXT NOT NULL,
    change_type            TEXT NOT NULL,
    from_scope             JSONB NOT NULL,
    to_scope               JSONB NOT NULL,
    reports_affected_count INT NOT NULL,
    strategy               TEXT,
    export_path            TEXT,
    confirmation_token     TEXT NOT NULL,
    notes                  TEXT,
    CONSTRAINT scope_changes_change_type_check
        CHECK (change_type IN ('expand', 'shrink', 'lateral', 'initial', 'boundary_refresh'))
);

CREATE INDEX IF NOT EXISTS scope_changes_changed_at
    ON public.scope_changes(changed_at DESC);

COMMENT ON TABLE public.scope_changes IS
    'Append-only audit log of every scope change. from_scope/to_scope carry {level, country, state, district, polygon_sha256}. confirmation_token echoes the operator-typed replay-protection token. See plan/jurisdiction-hierarchy-locked.md §2d + §3.';

-- ── 2e. installed_boundary_packs — provenance ledger ───────────────────────

CREATE TABLE IF NOT EXISTS public.installed_boundary_packs (
    country_code  CHAR(2) PRIMARY KEY,
    url           TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    installed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installed_by  TEXT NOT NULL,
    version       TEXT,
    license       TEXT,
    notes         TEXT
);

COMMENT ON TABLE public.installed_boundary_packs IS
    'Records which boundary GeoJSON pack is currently installed per country (URL + sha256 + license). Mirrors installed_packs from 069 so the CLI pattern is uniform. See plan/jurisdiction-hierarchy-locked.md §2e.';
