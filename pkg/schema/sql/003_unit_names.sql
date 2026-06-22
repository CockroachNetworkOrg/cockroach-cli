-- 139_gazetteer_unit_names.sql
--
-- Multilingual place names for gazetteer units (RENAME-SPEC §3a). A unit can
-- carry many localized names (BCP-47 lang, optional ISO 15924 script), each
-- flagged official / preferred. This is ADDITIVE: gazetteer_units.local_name +
-- languages[] stay for back-compat; this table is the normalized, queryable
-- multi-name store.
--
-- ADDITIVE / IDEMPOTENT. The migration runner wraps each file in one
-- transaction, so no BEGIN/COMMIT and no CONCURRENTLY here.
--
-- FK NOTE: the all-FKs-in-012 invariant only holds for the baseline (≤012)
-- tables, which all exist before 012 runs. This is a POST-baseline table
-- (created here, after 012 has already executed), so its FK must be declared
-- inline alongside the table — 012 cannot reference a table that does not yet
-- exist when 012 runs. The FK is guarded so a re-run never errors.

CREATE TABLE IF NOT EXISTS public.gazetteer_unit_names (
    id           BIGSERIAL PRIMARY KEY,
    unit_code    TEXT NOT NULL,                    -- FK→gazetteer_units(code), declared below
    lang         TEXT NOT NULL,                    -- BCP-47 (e.g. hi, mr, bn-IN)
    value        TEXT NOT NULL,
    is_official  BOOLEAN NOT NULL DEFAULT false,
    is_preferred BOOLEAN NOT NULL DEFAULT false,
    script       TEXT,                             -- ISO 15924 (optional)
    source       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (unit_code, lang, value)
);
CREATE INDEX IF NOT EXISTS idx_gazetteer_unit_names_unit ON public.gazetteer_unit_names(unit_code);
CREATE INDEX IF NOT EXISTS idx_gazetteer_unit_names_lang ON public.gazetteer_unit_names(lang);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'gazetteer_unit_names_unit_code_fkey'
    ) THEN
        ALTER TABLE ONLY public.gazetteer_unit_names
            ADD CONSTRAINT gazetteer_unit_names_unit_code_fkey
            FOREIGN KEY (unit_code) REFERENCES public.gazetteer_units(code) ON DELETE CASCADE;
    END IF;
END $$;
