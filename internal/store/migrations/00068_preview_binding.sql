-- +goose Up
-- +goose StatementBegin

-- Phase-1 review fixes: bind a preview token to the mutation it authorizes, and stop a
-- cascade from editing the evidence (func-service-reliability §10.8, §15.4).

-- ── The token has to name what it approved ──────────────────────────────────────────────
--
-- The preview stored `requested_start`/`requested_end` and the confirm read NEITHER. It
-- checked the maintenance generation, the affected-set generations and expiry — all real
-- checks, none of which say anything about WHICH mutation the operator was shown. So a token
-- issued for a two-minute window on monitor X authorized a twelve-hour window on monitor Y,
-- as long as both happened to touch the same services: exactly the retroactive rewrite the
-- preview exists to gate.
ALTER TABLE maintenance_previews
    ADD COLUMN IF NOT EXISTS monitor_id uuid,
    ADD COLUMN IF NOT EXISTS mutation   text NOT NULL DEFAULT 'create'
        CHECK (mutation IN ('create', 'annul'));

-- ── The affected set is a SNAPSHOT, and a snapshot cannot be edited ─────────────────────
--
-- `maintenance_preview_services` cascaded from `services`, so deleting a service ERASED it
-- from the stored set. The confirm then compared a set that had quietly shrunk against the
-- current one, found them equal, and passed — the deletion, which is precisely a change to
-- the affected set, made the staleness check agree instead of disagree.
--
-- Dropping the FK is the whole fix: these rows record what was true when the preview ran, and
-- what was true then does not stop having been true because a row was removed since. With the
-- snapshot intact the ordinary set comparison sees the difference by itself, which is why no
-- second "invalidated" flag is carried — a mechanism that never fires is not a safeguard.
ALTER TABLE maintenance_preview_services
    DROP CONSTRAINT IF EXISTS maintenance_preview_services_service_id_project_id_fkey;

CREATE INDEX IF NOT EXISTS maintenance_preview_services_service_idx
    ON maintenance_preview_services (service_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS maintenance_preview_services_service_idx;
-- The FK is not restored: rows may now reference services that no longer exist, and adding
-- it back would fail on exactly the history this migration exists to preserve.
ALTER TABLE maintenance_previews
    DROP COLUMN IF EXISTS mutation,
    DROP COLUMN IF EXISTS monitor_id;
-- +goose StatementEnd
