-- +goose Up
-- +goose StatementBegin

-- iter-0126: a service that stops declaring reliability inputs and later declares them again
-- must be able to recover its watermark (func-service-reliability §10.5).
--
-- The contiguity rule says a hole HOLDS `sealed_through`, and that is right for a hole caused
-- by a stall: the system owes those buckets and has not produced them. It is wrong for a
-- period the service DECLARED it was measuring nothing. There are no facts there because a
-- human said there should be none, and the watermark stopping in front of that gap forever
-- means a re-enabled service can never report anything again — every phase-2 window anchors
-- at a timestamp from before the gap.
--
-- Invariant 34 covers a service that was ALWAYS empty. This is the nonempty → empty → nonempty
-- transition, which is a declared DISCONTINUITY rather than a gap, and it needs to be recorded
-- as one.
--
-- `era_start` is where the current contiguous era begins. `materialization_start` keeps its
-- meaning — the earliest instant this service ever had facts for, which never moves — so the
-- two answer different questions and neither has to lie.
ALTER TABLE service_materialization
    ADD COLUMN IF NOT EXISTS era_start timestamptz;

UPDATE service_materialization SET era_start = materialization_start WHERE era_start IS NULL;

ALTER TABLE service_materialization
    ALTER COLUMN era_start SET NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE service_materialization DROP COLUMN IF EXISTS era_start;
-- +goose StatementEnd
