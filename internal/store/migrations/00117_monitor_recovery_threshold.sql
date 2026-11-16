-- +goose Up
-- FR-043 / NFR-036 (func-recovery-threshold.md §5, iter-0214, D-0281): a monitor leaves `down` only
-- after `recovery_threshold` consecutive successful checks. Additive; existing rows get 1 / 0 / NULL,
-- which is today's behaviour exactly (I1). `heartbeats` is not touched, so the storage mode does not
-- matter to this migration.
--
--   recovery_threshold     1..10 (D3), and exactly 1 for push and composite (D8). The store writes the
--                          value it is given — nothing clamps — so these CHECKs back Monitor.Validate
--                          instead of being hidden by a default (§4.5).
--   consecutive_successes  the live run, server-owned; non-zero only while the monitor is `down`.
--   recovery_started_at    the database clock of the run's first success (D6), stamped after the row
--                          lock is held; NULL exactly when there is no run (I13).
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS recovery_threshold integer NOT NULL DEFAULT 1;
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS consecutive_successes integer NOT NULL DEFAULT 0;
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS recovery_started_at timestamptz NULL;

ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_threshold_range;
ALTER TABLE monitors ADD CONSTRAINT monitors_recovery_threshold_range
    CHECK (recovery_threshold BETWEEN 1 AND 10);

ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_threshold_type;
ALTER TABLE monitors ADD CONSTRAINT monitors_recovery_threshold_type
    CHECK (type NOT IN ('push', 'composite') OR recovery_threshold = 1);

ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_run_state;
ALTER TABLE monitors ADD CONSTRAINT monitors_recovery_run_state
    CHECK ((consecutive_successes = 0 AND recovery_started_at IS NULL)
        OR (status = 'down' AND consecutive_successes > 0 AND recovery_started_at IS NOT NULL));

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_run_state;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_threshold_type;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_recovery_threshold_range;
ALTER TABLE monitors DROP COLUMN IF EXISTS recovery_started_at;
ALTER TABLE monitors DROP COLUMN IF EXISTS consecutive_successes;
ALTER TABLE monitors DROP COLUMN IF EXISTS recovery_threshold;
