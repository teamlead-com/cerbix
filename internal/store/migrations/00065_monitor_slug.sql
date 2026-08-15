-- +goose NO TRANSACTION
-- Project-unique, immutable monitor slug (spec func-service-reliability §15.3; D-0159).
--
-- A Service must be able to name a monitor in YAML, and today it cannot: monitors have no
-- project-unique name — the only unique indexes on them are `push_token_hash` and, on
-- heartbeats, `(monitor_id, ts)`. UUIDs are unusable as an authoring contract, and
-- restricting membership to file-owned monitors would contradict the coexistence matrix, so
-- the slug is the reference key.
--
-- EXPAND → BACKFILL → CONTRACT, in that order and in one file, because a half-applied
-- version of this leaves the column nullable in production while the code assumes otherwise.
--
-- The backfill prefers a PORTABLE source over a local one, which is the part worth reading:
-- a file-owned row takes its provider `source_uid` — exactly the key its bundle already uses
-- — so the same Git-tracked bundle yields the same slug on every installation. A UUID-derived
-- suffix would differ per install and quietly break MaC portability, which is the whole point
-- of declaring services in files.

-- +goose Up
-- +goose StatementBegin

-- EXPAND.
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS slug text;

-- BACKFILL. Deterministic and collision-safe, and it changes no id, name or history.
DO $$
DECLARE
    m RECORD;
    base text;
    candidate text;
    n int;
BEGIN
    FOR m IN
        SELECT mo.id, mo.project_id, mo.name, mm.source_uid
          FROM monitors mo
          LEFT JOIN managed_monitors mm ON mm.monitor_id = mo.id
         WHERE mo.slug IS NULL
         ORDER BY mo.created_at, mo.id
    LOOP
        -- A file-owned row uses the key its own bundle already names it by.
        IF m.source_uid IS NOT NULL AND m.source_uid <> '' THEN
            base := m.source_uid;
        ELSE
            base := m.name;
        END IF;

        base := lower(base);
        base := regexp_replace(base, '[^a-z0-9]+', '-', 'g');
        base := regexp_replace(base, '(^-+|-+$)', '', 'g');
        base := left(base, 55);
        base := regexp_replace(base, '-+$', '', 'g');
        IF base = '' OR base !~ '^[a-z]' THEN
            base := 'monitor-' || base;
            base := regexp_replace(base, '-+$', '', 'g');
        END IF;

        candidate := base;
        n := 0;
        -- On collision append a stable short suffix from the monitor's own uuid: same input,
        -- same output, on every run and every replica.
        WHILE EXISTS (SELECT 1 FROM monitors WHERE project_id = m.project_id AND slug = candidate) LOOP
            n := n + 1;
            candidate := base || '-' || left(replace(m.id::text, '-', ''), 4 + n);
        END LOOP;

        UPDATE monitors SET slug = candidate WHERE id = m.id;
        IF candidate <> base THEN
            RAISE NOTICE 'monitor % : slug % (collision on %)', m.id, candidate, base;
        END IF;
    END LOOP;
END $$;
-- +goose StatementEnd

-- CONTRACT.
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS monitors_project_slug_uniq ON monitors (project_id, slug);
ALTER TABLE monitors ALTER COLUMN slug SET NOT NULL;
ALTER TABLE monitors ADD CONSTRAINT monitors_slug_shape
    CHECK (slug ~ '^[a-z][a-z0-9-]{0,62}$');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_slug_shape;
DROP INDEX IF EXISTS monitors_project_slug_uniq;
ALTER TABLE monitors DROP COLUMN IF EXISTS slug;
-- +goose StatementEnd
