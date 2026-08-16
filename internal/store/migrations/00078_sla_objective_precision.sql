-- +goose Up
-- FR-021 phase 2 (iter-0141): the documented objective contract is (0,100], but
-- numeric(6,4) tops out at 99.9999 — a literal 100 raised numeric_value_out_of_range and
-- surfaced as HTTP 500. numeric(7,4) makes 100.0000 representable; the canonical scale is
-- FOUR decimal places, owned by domain.CanonicalObjective (one rule for the monitor and the
-- service scope alike), so the stored value and the API's answer are always the same number.
ALTER TABLE sla_targets ALTER COLUMN objective TYPE numeric(7,4);

-- +goose Down
-- Narrowing back would re-break objective=100; refuse any stored value the old type cannot
-- hold rather than silently corrupting it.
ALTER TABLE sla_targets ALTER COLUMN objective TYPE numeric(6,4);
