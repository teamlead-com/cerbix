-- +goose Up
-- One-off "Test connection" probes for a pull-served region. The API enqueues a test
-- (a monitor snapshot) here; the region's pull agent claims it, runs the probe, and
-- writes the result back; the API polls for the result and returns it synchronously.
-- This is the pull-transport analogue of the RabbitMQ test-RPC used for AMQP workers.
CREATE TABLE pull_tests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    region     text NOT NULL,
    payload    jsonb NOT NULL,       -- CheckJob snapshot to probe
    result     jsonb,               -- Heartbeat, set by the agent when done
    claimed_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pull_tests_region_idx ON pull_tests (region, created_at);

-- +goose Down
DROP TABLE IF EXISTS pull_tests;
