-- +goose Up
-- +goose StatementBegin

-- iter-0135: the sampler's pending/running probes must be bounded by ROWS EXAMINED, not by
-- rows returned. 00067's claim index carries state only in its predicate (both states in one
-- partial index), so counting "pending" with a large running set — or the common idle shape,
-- a large pending queue with running = 0 — filtered every entry of the shared index to prove
-- the other state's count. One predicate per sampled state makes each probe scan exactly the
-- rows it reports, saturation cap included.
CREATE INDEX IF NOT EXISTS service_repair_ranges_pending_idx
    ON service_repair_ranges (state) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS service_repair_ranges_running_idx
    ON service_repair_ranges (state) WHERE state = 'running';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS service_repair_ranges_running_idx;
DROP INDEX IF EXISTS service_repair_ranges_pending_idx;
-- +goose StatementEnd
