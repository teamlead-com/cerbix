-- +goose Up
-- +goose StatementBegin

-- iter-0126: a preview has to name its TARGET and show what would change
-- (func-service-reliability §10.8).

-- ── The target ──────────────────────────────────────────────────────────────────────────
--
-- 00068 bound a token to monitor + range + kind, which is not enough for an annul. Two
-- windows over the same monitor and the same range are DIFFERENT mutations: with both in
-- place, annulling one may change nothing while annulling the other changes the number, so a
-- token issued for one must not confirm the other. Annul is identified by the window.
ALTER TABLE maintenance_previews
    ADD COLUMN IF NOT EXISTS target_id uuid;

-- ── The projection ──────────────────────────────────────────────────────────────────────
--
-- The preview summed the CURRENT good/bad and called it a preview. A before with no after is
-- not one: the operator is being asked to authorize a change to sealed numbers and was shown
-- only what they already are.
--
-- Both axes are carried, not just availability. A mutation can move health without moving
-- good/bad at all — an exclusion that lands entirely inside already-degraded time — and a
-- projection that showed only the first would report "nothing changes" for a change.
ALTER TABLE maintenance_preview_services
    ADD COLUMN IF NOT EXISTS before_unknown_us  bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS before_excluded_us bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_good_us      bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_bad_us       bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_unknown_us   bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS after_excluded_us  bigint NOT NULL DEFAULT 0,
    -- Set when the after-state could not be computed for this service (the range exceeded
    -- the projection bound). The preview's coverage then reads `approximate`, and a confirm
    -- refuses it — an unprojectable change is not one an operator can be said to have seen.
    ADD COLUMN IF NOT EXISTS projected boolean NOT NULL DEFAULT true;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE maintenance_preview_services
    DROP COLUMN IF EXISTS projected,
    DROP COLUMN IF EXISTS after_excluded_us,
    DROP COLUMN IF EXISTS after_unknown_us,
    DROP COLUMN IF EXISTS after_bad_us,
    DROP COLUMN IF EXISTS after_good_us,
    DROP COLUMN IF EXISTS before_excluded_us,
    DROP COLUMN IF EXISTS before_unknown_us;
ALTER TABLE maintenance_previews DROP COLUMN IF EXISTS target_id;
-- +goose StatementEnd
