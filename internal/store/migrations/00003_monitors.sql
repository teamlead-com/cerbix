-- +goose Up
-- Monitors (the checks) and their heartbeats (time-series results).

CREATE TABLE monitors (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name             text NOT NULL,
    type             text NOT NULL CHECK (type IN ('http', 'tcp', 'push')),
    target           text NOT NULL DEFAULT '',
    method           text NOT NULL DEFAULT 'GET',
    interval_seconds int NOT NULL DEFAULT 60,
    timeout_seconds  int NOT NULL DEFAULT 10,
    retries          int NOT NULL DEFAULT 0,
    grace_seconds    int NOT NULL DEFAULT 0,
    conditions       text[] NOT NULL DEFAULT '{}',
    config           jsonb NOT NULL DEFAULT '{}',
    enabled          boolean NOT NULL DEFAULT true,
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'up', 'down')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX monitors_project_idx ON monitors (project_id);
CREATE INDEX monitors_enabled_idx ON monitors (enabled) WHERE enabled;

-- Plain table for now; converted to a TimescaleDB hypertable in the SLA iteration.
CREATE TABLE heartbeats (
    monitor_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    ts         timestamptz NOT NULL DEFAULT now(),
    up         boolean NOT NULL,
    latency_ms bigint NOT NULL DEFAULT 0,
    code       int NOT NULL DEFAULT 0,
    msg        text NOT NULL DEFAULT ''
);

CREATE INDEX heartbeats_monitor_ts_idx ON heartbeats (monitor_id, ts DESC);

-- +goose Down
DROP TABLE heartbeats;
DROP TABLE monitors;
