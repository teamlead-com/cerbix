-- +goose Up
-- +goose StatementBegin

-- Phase-1 review fixes: the forward materialization driver and lease recovery
-- (func-service-reliability §10.5, §10.7).
--
-- Two columns, and both exist because the first implementation had no product path at all:
-- rows were created and sealed only by tests calling the store directly.

-- ── The driver cursor ───────────────────────────────────────────────────────────────────
--
-- `sealed_through` is EVIDENCE: the contiguous instant through which sealed facts exist. It
-- is deliberately held back by a hole. `materialized_through` is PROGRESS: how far the
-- driver has walked. They are different questions and one column cannot answer both.
--
-- Without this split the driver has no resumable cursor except the watermark, so a bucket
-- that legitimately produces NO fact — a window in which the service declared an empty SLI,
-- or one governed by no epoch — makes it re-process the same bucket forever while the
-- watermark stays put. Splitting them lets the watermark stay honest about the gap and lets
-- the driver move on.
ALTER TABLE service_materialization
    ADD COLUMN IF NOT EXISTS materialized_through timestamptz;

-- Existing rows: progress is at least as far as the evidence.
UPDATE service_materialization
   SET materialized_through = COALESCE(sealed_through, materialization_start)
 WHERE materialized_through IS NULL;

-- Services due for advancement, cheapest first.
CREATE INDEX IF NOT EXISTS service_materialization_due_idx
    ON service_materialization (materialized_through);

-- ── Lease recovery for claimed work ─────────────────────────────────────────────────────
--
-- A claim commits `state='running'` and the claim query selects only `pending`, so a leader
-- that lost its backend between claim and release stranded the range FOREVER: no successor
-- could see it, and the watermark hole it was queued to fill stayed open with nothing left
-- to fill it. The lease makes the claim recoverable — a running row whose lease has expired
-- is claimable again, by anyone.
--
-- It is a LEASE, not a heartbeat: the worker extends it as it makes progress, and the
-- extension rides the same transaction as the cursor write, so a lease can never outlive the
-- progress it claims to protect.
ALTER TABLE service_repair_ranges
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;

-- Ranges created before this migration that are stuck in `running` have no lease and no
-- owner alive; make them claimable immediately rather than leaving them stranded.
UPDATE service_repair_ranges
   SET lease_expires_at = now()
 WHERE state = 'running' AND lease_expires_at IS NULL;

DROP INDEX IF EXISTS service_repair_ranges_claim_idx;
CREATE INDEX service_repair_ranges_claim_idx
    ON service_repair_ranges (next_attempt_at, service_id)
    WHERE state IN ('pending', 'running');

-- ── Same-boundary supersession must be scoped ───────────────────────────────────────────
--
-- Supersession marked EVERY pending/running range starting at or after the boundary, so a
-- declaration write silently discarded unrelated admin, maintenance and backfill work that
-- happened to start there — the exact opposite of the union preservation coalescing exists
-- to guarantee. A range now records what queued it, and only that origin's own work is
-- displaced when the origin loses a same-boundary race.
ALTER TABLE service_repair_ranges
    ADD COLUMN IF NOT EXISTS origin_id uuid;

CREATE INDEX IF NOT EXISTS service_repair_ranges_origin_idx
    ON service_repair_ranges (origin_id)
    WHERE origin_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS service_repair_ranges_origin_idx;
ALTER TABLE service_repair_ranges DROP COLUMN IF EXISTS origin_id;
ALTER TABLE service_repair_ranges DROP COLUMN IF EXISTS lease_expires_at;
DROP INDEX IF EXISTS service_materialization_due_idx;
ALTER TABLE service_materialization DROP COLUMN IF EXISTS materialized_through;
-- +goose StatementEnd
