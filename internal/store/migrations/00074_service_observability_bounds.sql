-- +goose Up
-- +goose StatementBegin

-- iter-0134: the §21 observability surface gets DURABLE, BOUNDED foundations.
--
-- Event counters that fire inside an outer transaction must live OR DIE with it: a Go-side
-- increment before commit counts rolled-back epochs and late arrivals as durable events.
-- The aggregate is one row per kind — bounded by construction — updated in the SAME
-- transaction as the event and sampled by the leader's off-loop sampler, which then exports
-- monotonic counters (the table is only ever incremented).
CREATE TABLE IF NOT EXISTS service_metric_events (
    kind  text PRIMARY KEY,
    value bigint NOT NULL DEFAULT 0
);

-- The sampler's queue counts must be INDEX-SUPPORTED for every sampled state. 00067 indexed
-- the claimable states (pending/running); `error` is terminal and grows with parked history,
-- and counting it each sample was a creeping full scan.
CREATE INDEX IF NOT EXISTS service_repair_ranges_errored_idx
    ON service_repair_ranges (state) WHERE state = 'error';

-- The worst watermark lag is MIN(COALESCE(sealed_through, era_start)) over every declared
-- service; the expression index turns the global max-lag into one index probe instead of a
-- scan that grows with the service population.
CREATE INDEX IF NOT EXISTS service_materialization_watermark_idx
    ON service_materialization ((COALESCE(sealed_through, era_start)));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS service_materialization_watermark_idx;
DROP INDEX IF EXISTS service_repair_ranges_errored_idx;
DROP TABLE IF EXISTS service_metric_events;
-- +goose StatementEnd
