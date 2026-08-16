-- +goose Up
-- +goose StatementBegin

-- iter-0127: the projection carries BOTH conserved axes, and pre-upgrade tokens die here.

-- ── Health beside availability ──────────────────────────────────────────────────────────
--
-- A mutation can move health without moving availability at all: an exclusion landing
-- entirely inside already-degraded time changes health history and leaves good/bad exactly
-- where they were. A preview that stored only the availability axis showed "no change" for
-- that change, which is the confident falsehood the preview exists to prevent.
ALTER TABLE maintenance_preview_services
    ADD COLUMN IF NOT EXISTS before_healthy_us        bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS before_degraded_us       bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS before_down_us           bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS before_health_unknown_us bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_healthy_us         bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_degraded_us        bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_down_us            bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_health_unknown_us  bigint NOT NULL DEFAULT 0;

-- ── Pre-upgrade tokens are not confirmable ───────────────────────────────────────────────
--
-- 00072 added `projected` with DEFAULT true and `after_*` with DEFAULT 0, which quietly
-- GRANDFATHERED every token issued before the upgrade: a preview whose operator was shown
-- only the "before" kept `coverage = complete` and stayed confirmable for up to its ten
-- minutes. A default is not evidence — a token is confirmable only if the operator saw what
-- it authorizes, and nobody saw a projection that did not exist yet. Expire them all; the
-- operator re-previews and this time is shown both sides.
UPDATE maintenance_previews SET expires_at = now() WHERE expires_at > now();
UPDATE maintenance_preview_services SET projected = false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE maintenance_preview_services
    DROP COLUMN IF EXISTS after_health_unknown_us,
    DROP COLUMN IF EXISTS after_down_us,
    DROP COLUMN IF EXISTS after_degraded_us,
    DROP COLUMN IF EXISTS after_healthy_us,
    DROP COLUMN IF EXISTS before_health_unknown_us,
    DROP COLUMN IF EXISTS before_down_us,
    DROP COLUMN IF EXISTS before_degraded_us,
    DROP COLUMN IF EXISTS before_healthy_us;
-- The expiry of pre-upgrade previews is not undone: they were never valid evidence.
-- +goose StatementEnd
