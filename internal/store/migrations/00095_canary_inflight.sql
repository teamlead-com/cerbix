-- +goose Up
-- FR-029 D9 / D9a: the in-flight record that keeps ONE canary execution per monitor and bounds how
-- many run in a region at once.
--
-- A canary holds its delivery for the whole journey (D2), so two things the ordinary types never
-- needed become necessary. First, a second dispatch of the same monitor while the first is still
-- running would submit a SECOND external transaction — a real side effect at someone else's expense,
-- which no idempotency key can undo if the target does not honour one. Second, a region with several
-- executors has no worker-local way to bound how many long journeys it runs at once; prefetch is per
-- consumer and the count has to live where dispatch is decided.
--
-- Both are enforced in the CORE, at the scheduler's dispatch decision, which is already serialized:
-- the scheduler is leader-elected through a Postgres advisory lock and is the only writer here. The
-- rows exist for CRASH RECOVERY and for the executor's ack, not for mutual exclusion between
-- schedulers — which is why a plain count under the inserting transaction is enough and no semaphore
-- appears anywhere.
--
-- `monitor_id` is the primary key: one row per monitor is the per-monitor lease. `expires_at` is what
-- makes a crashed executor recoverable without operator action — the slot returns on its own, and the
-- runbook states the number rather than leaving it to arithmetic.
CREATE TABLE IF NOT EXISTS canary_inflight (
    monitor_id  uuid PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    region      text        NOT NULL,
    run_key     text        NOT NULL,
    claimed_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

-- The region cap is a count over unexpired rows, so that predicate is the index.
CREATE INDEX IF NOT EXISTS canary_inflight_region_idx ON canary_inflight (region, expires_at);

-- +goose Down
DROP INDEX IF EXISTS canary_inflight_region_idx;
DROP TABLE IF EXISTS canary_inflight;
