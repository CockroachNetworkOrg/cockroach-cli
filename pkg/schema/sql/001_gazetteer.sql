-- 002_gazetteer.sql
-- Cockroach Network - clean schema-only baseline (migrations-v2).
-- Squash of migrations 001-061; reproduces the live schema exactly.
-- Tables: gazetteer_kinds gazetteer_units constituency_coverage neighbourhood_submissions neighbourhood_submission_votes gazetteer_import_jobs postal_codes postal_code_areas
-- All FOREIGN KEY constraints live in 012_foreign_keys.sql (applied last).

-- The gazetteer trigram index below uses public.gin_trgm_ops, so the extension
-- must exist before this core file runs. Core applies BEFORE any product
-- migration, so we cannot rely on a product's 001_core to have created it yet.
-- IF NOT EXISTS keeps it idempotent with the product's own CREATE EXTENSION.
CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;

--
-- gazetteer_kinds  (TABLE)
--

CREATE TABLE public.gazetteer_kinds (
    country_code text NOT NULL,
    kind text NOT NULL,
    label text NOT NULL,
    label_plural text NOT NULL,
    level integer DEFAULT 0 NOT NULL,
    code_prefix text DEFAULT ''::text NOT NULL,
    parent_kinds text[] DEFAULT '{}'::text[] NOT NULL,
    is_administrative boolean DEFAULT true NOT NULL,
    is_user_curated boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT gazetteer_kinds_country_code_check CHECK ((country_code ~ '^[A-Z]{2}$'::text))
);

--
-- gazetteer_kinds gazetteer_kinds_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.gazetteer_kinds
    ADD CONSTRAINT gazetteer_kinds_pkey PRIMARY KEY (country_code, kind);

--
-- idx_gazetteer_kinds_country_level  (INDEX)
--

CREATE INDEX idx_gazetteer_kinds_country_level ON public.gazetteer_kinds USING btree (country_code, level);

--
-- gazetteer_units  (TABLE)
--

CREATE TABLE public.gazetteer_units (
    code text NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    parent_code text,
    state_code text,
    bbox_min_lat double precision,
    bbox_max_lat double precision,
    bbox_min_lng double precision,
    bbox_max_lng double precision,
    population_2011 bigint,
    source_updated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    country_code text NOT NULL,
    local_name text,
    iso_3166_2 text,
    population_year integer,
    area_sq_km numeric(12,3),
    time_zone text,
    languages text[] DEFAULT '{}'::text[] NOT NULL,
    primary_postal_code text,
    deleted_at timestamp with time zone,
    is_deprecated boolean DEFAULT false NOT NULL,
    merged_into_code text,
    CONSTRAINT gazetteer_units_iso_3166_2_format
        CHECK (iso_3166_2 IS NULL OR iso_3166_2 ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'),
    CONSTRAINT gazetteer_units_population_year_range
        CHECK (population_year IS NULL OR (population_year BETWEEN 1900 AND 2200)),
    CONSTRAINT gazetteer_units_population_2011_range
        CHECK (population_2011 IS NULL OR (population_2011 BETWEEN 0 AND 2000000000)),
    CONSTRAINT gazetteer_units_area_non_negative
        CHECK (area_sq_km IS NULL OR area_sq_km >= 0),
    CONSTRAINT gazetteer_units_bbox_lat_range
        CHECK ((bbox_min_lat IS NULL OR (bbox_min_lat BETWEEN -90 AND 90))
           AND (bbox_max_lat IS NULL OR (bbox_max_lat BETWEEN -90 AND 90))),
    CONSTRAINT gazetteer_units_bbox_lng_range
        CHECK ((bbox_min_lng IS NULL OR (bbox_min_lng BETWEEN -180 AND 180))
           AND (bbox_max_lng IS NULL OR (bbox_max_lng BETWEEN -180 AND 180))),
    CONSTRAINT gazetteer_units_bbox_ordering
        CHECK ((bbox_min_lat IS NULL OR bbox_max_lat IS NULL OR bbox_min_lat <= bbox_max_lat)
           AND (bbox_min_lng IS NULL OR bbox_max_lng IS NULL OR bbox_min_lng <= bbox_max_lng))
);

--
-- gazetteer_units gazetteer_units_country_code_key  (CONSTRAINT)
--

ALTER TABLE ONLY public.gazetteer_units
    ADD CONSTRAINT gazetteer_units_country_code_key UNIQUE (country_code, code);

--
-- gazetteer_units gazetteer_units_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.gazetteer_units
    ADD CONSTRAINT gazetteer_units_pkey PRIMARY KEY (code);

--
-- idx_gazetteer_units_bbox  (INDEX)
--

CREATE INDEX idx_gazetteer_units_bbox ON public.gazetteer_units USING btree (bbox_min_lat, bbox_max_lat, bbox_min_lng, bbox_max_lng);

--
-- idx_gazetteer_units_country_kind  (INDEX)
--

CREATE INDEX idx_gazetteer_units_country_kind ON public.gazetteer_units USING btree (country_code, kind);

--
-- idx_gazetteer_units_country_parent  (INDEX)
--

CREATE INDEX idx_gazetteer_units_country_parent ON public.gazetteer_units USING btree (country_code, parent_code);

--
-- idx_gazetteer_units_kind  (INDEX)
--

CREATE INDEX idx_gazetteer_units_kind ON public.gazetteer_units USING btree (kind);

--
-- idx_gazetteer_units_name_trgm  (INDEX)
--

CREATE INDEX idx_gazetteer_units_name_trgm ON public.gazetteer_units USING gin (name public.gin_trgm_ops);

--
-- idx_gazetteer_units_parent  (INDEX)
--

CREATE INDEX idx_gazetteer_units_parent ON public.gazetteer_units USING btree (parent_code);

--
-- idx_gazetteer_units_state  (INDEX)
--

CREATE INDEX idx_gazetteer_units_state ON public.gazetteer_units USING btree (state_code);

--
-- idx_gazetteer_units_active_country_kind  (INDEX) — partial: active rows only
--

CREATE INDEX idx_gazetteer_units_active_country_kind ON public.gazetteer_units USING btree (country_code, kind) WHERE (deleted_at IS NULL);

--
-- idx_gazetteer_units_merged_into  (INDEX) — partial: reverse-lookup for merge history
--

CREATE INDEX idx_gazetteer_units_merged_into ON public.gazetteer_units USING btree (merged_into_code) WHERE (merged_into_code IS NOT NULL);

--
-- constituency_coverage  (TABLE)
--

CREATE TABLE public.constituency_coverage (
    constituency_code text NOT NULL,
    area_code text NOT NULL,
    source text DEFAULT 'district_name'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- constituency_coverage constituency_coverage_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.constituency_coverage
    ADD CONSTRAINT constituency_coverage_pkey PRIMARY KEY (constituency_code, area_code);

--
-- idx_ccov_area  (INDEX)
--

CREATE INDEX idx_ccov_area ON public.constituency_coverage USING btree (area_code);

--
-- idx_ccov_constituency  (INDEX)
--

CREATE INDEX idx_ccov_constituency ON public.constituency_coverage USING btree (constituency_code);

--
-- neighbourhood_submissions  (TABLE)
--

CREATE TABLE public.neighbourhood_submissions (
    id text NOT NULL,
    submitted_label text NOT NULL,
    normalised_label text NOT NULL,
    parent_unit_code text NOT NULL,
    submitter_count integer DEFAULT 1 NOT NULL,
    first_submitted_at timestamp with time zone DEFAULT now() NOT NULL,
    last_submitted_at timestamp with time zone DEFAULT now() NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    promoted_to_code text,
    promoted_at timestamp with time zone,
    rejected_at timestamp with time zone,
    CONSTRAINT neighbourhood_submissions_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'promoted'::text, 'rejected'::text])))
);

--
-- neighbourhood_submissions neighbourhood_submissions_normalised_label_parent_unit_c_key  (CONSTRAINT)
--

ALTER TABLE ONLY public.neighbourhood_submissions
    ADD CONSTRAINT neighbourhood_submissions_normalised_label_parent_unit_c_key UNIQUE (normalised_label, parent_unit_code);

--
-- neighbourhood_submissions neighbourhood_submissions_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.neighbourhood_submissions
    ADD CONSTRAINT neighbourhood_submissions_pkey PRIMARY KEY (id);

--
-- idx_nbh_count  (INDEX)
--

CREATE INDEX idx_nbh_count ON public.neighbourhood_submissions USING btree (submitter_count DESC);

--
-- idx_nbh_parent  (INDEX)
--

CREATE INDEX idx_nbh_parent ON public.neighbourhood_submissions USING btree (parent_unit_code);

--
-- idx_nbh_status  (INDEX)
--

CREATE INDEX idx_nbh_status ON public.neighbourhood_submissions USING btree (status);

--
-- neighbourhood_submission_votes  (TABLE)
--

CREATE TABLE public.neighbourhood_submission_votes (
    id text NOT NULL,
    submission_id text NOT NULL,
    voter_user_id text NOT NULL,
    vote text NOT NULL,
    weight double precision DEFAULT 1 NOT NULL,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT neighbourhood_submission_votes_vote_check CHECK ((vote = ANY (ARRAY['confirm'::text, 'dispute'::text])))
);

--
-- neighbourhood_submission_votes neighbourhood_submission_votes_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.neighbourhood_submission_votes
    ADD CONSTRAINT neighbourhood_submission_votes_pkey PRIMARY KEY (id);

--
-- neighbourhood_submission_votes neighbourhood_submission_votes_submission_id_voter_user_id_key  (CONSTRAINT)
--

ALTER TABLE ONLY public.neighbourhood_submission_votes
    ADD CONSTRAINT neighbourhood_submission_votes_submission_id_voter_user_id_key UNIQUE (submission_id, voter_user_id);

--
-- idx_neighbourhood_submission_votes_submission  (INDEX)
--

CREATE INDEX idx_neighbourhood_submission_votes_submission ON public.neighbourhood_submission_votes USING btree (submission_id);

--
-- gazetteer_import_jobs  (TABLE)
--

CREATE TABLE public.gazetteer_import_jobs (
    id text NOT NULL,
    filename text NOT NULL,
    format text DEFAULT 'csv'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    rows_total integer DEFAULT 0 NOT NULL,
    rows_inserted integer DEFAULT 0 NOT NULL,
    rows_updated integer DEFAULT 0 NOT NULL,
    rows_skipped integer DEFAULT 0 NOT NULL,
    rows_failed integer DEFAULT 0 NOT NULL,
    row_errors jsonb,
    error_message text,
    dry_run boolean DEFAULT false NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    CONSTRAINT gazetteer_import_jobs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'completed'::text, 'failed'::text])))
);

--
-- gazetteer_import_jobs gazetteer_import_jobs_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.gazetteer_import_jobs
    ADD CONSTRAINT gazetteer_import_jobs_pkey PRIMARY KEY (id);

--
-- idx_gazetteer_import_jobs_created_at  (INDEX)
--

CREATE INDEX idx_gazetteer_import_jobs_created_at ON public.gazetteer_import_jobs USING btree (created_at DESC);

--
-- idx_gazetteer_import_jobs_status  (INDEX)
--

CREATE INDEX idx_gazetteer_import_jobs_status ON public.gazetteer_import_jobs USING btree (status);

--
-- postal_codes  (TABLE)
--

CREATE TABLE public.postal_codes (
    postal_code text NOT NULL,
    area_name text,
    district_name text,
    state_name text,
    circle_name text,
    state_unit_code text,
    district_unit_code text,
    village_unit_code text,
    urban_body_unit_code text,
    area_kind text,
    office_name text,
    locality_count integer DEFAULT 1 NOT NULL,
    country_code text
);

--
-- postal_codes postal_codes_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.postal_codes
    ADD CONSTRAINT postal_codes_pkey PRIMARY KEY (postal_code);

--
-- idx_postal_codes_district  (INDEX)
--

CREATE INDEX idx_postal_codes_district ON public.postal_codes USING btree (district_unit_code);

--
-- idx_postal_codes_district_name  (INDEX)
--

CREATE INDEX idx_postal_codes_district_name ON public.postal_codes USING btree (lower(district_name));

--
-- idx_postal_codes_state  (INDEX)
--

CREATE INDEX idx_postal_codes_state ON public.postal_codes USING btree (state_unit_code);

--
-- idx_postal_codes_state_name  (INDEX)
--

CREATE INDEX idx_postal_codes_state_name ON public.postal_codes USING btree (lower(state_name));

--
-- postal_code_areas  (TABLE)
--

CREATE TABLE public.postal_code_areas (
    postal_code text NOT NULL,
    unit_code text NOT NULL,
    name text,
    kind text NOT NULL
);

--
-- postal_code_areas postal_code_areas_pkey  (CONSTRAINT)
--

ALTER TABLE ONLY public.postal_code_areas
    ADD CONSTRAINT postal_code_areas_pkey PRIMARY KEY (postal_code, unit_code);

--
-- idx_postal_code_areas_postal_code  (INDEX)
--

CREATE INDEX idx_postal_code_areas_postal_code ON public.postal_code_areas USING btree (postal_code);

--
-- gazetteer_units extra-field column documentation (visible via \d+ / introspection)
--

COMMENT ON COLUMN public.gazetteer_units.iso_3166_2          IS 'ISO 3166-2 subdivision code (e.g. IN-MH, PS-WBS). Required only when federating with external standardised datasets.';
COMMENT ON COLUMN public.gazetteer_units.population_year      IS 'Census vintage corresponding to population_2011 / future population columns.';
COMMENT ON COLUMN public.gazetteer_units.area_sq_km           IS 'Area in square kilometres. Enables density = population / area_sq_km.';
COMMENT ON COLUMN public.gazetteer_units.time_zone            IS 'IANA timezone (Asia/Kolkata, Asia/Jerusalem, etc.). For local-time rendering of report timestamps.';
COMMENT ON COLUMN public.gazetteer_units.languages           IS 'BCP-47 language codes spoken in this area (e.g. {hi,en,ur}). For multilingual UI per area.';
COMMENT ON COLUMN public.gazetteer_units.primary_postal_code IS 'Denormalised primary postal code; avoids a postal_code_areas join on display.';
COMMENT ON COLUMN public.gazetteer_units.deleted_at          IS 'Soft-delete timestamp. NULL = active. Soft-deleted rows preserve FK integrity for historical reports / audit logs.';
COMMENT ON COLUMN public.gazetteer_units.is_deprecated       IS 'Marks an area as superseded (administrative boundary change). Combine with merged_into_code to record where the population/responsibility moved.';
COMMENT ON COLUMN public.gazetteer_units.merged_into_code    IS 'When this unit was merged into another, the surviving code. Self-FK to gazetteer_units(code) ON DELETE SET NULL.';
