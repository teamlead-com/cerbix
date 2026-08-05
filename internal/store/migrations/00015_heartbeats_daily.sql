-- +goose Up
-- Per-day availability rollup: cheap long-range (90d) reads without scanning all
-- raw heartbeats. On plain Postgres this is a table maintained by a periodic job;
-- on TimescaleDB it can later be swapped for a continuous aggregate (D-0017).

CREATE TABLE heartbeats_daily (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    day        date NOT NULL,
    up         bigint NOT NULL DEFAULT 0,
    total      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (monitor_id, day)
);

CREATE INDEX heartbeats_daily_day_idx ON heartbeats_daily (day);

-- +goose Down
DROP TABLE heartbeats_daily;
