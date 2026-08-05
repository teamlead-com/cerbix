-- +goose Up
-- HTTP-pull transport: for regions served by a pull agent (no broker access from that
-- geo), the scheduler enqueues check jobs here instead of publishing to RabbitMQ; the
-- agent claims them over HTTPS. A job carries a monitor snapshot and a TTL (expires_at),
-- so a job for a region with no live agent expires rather than piling up.
CREATE TABLE pull_jobs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    region     text NOT NULL,
    payload    jsonb NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pull_jobs_region_idx ON pull_jobs (region, created_at);

-- Agents heartbeat per region so the region picker and region-worker alert can treat a
-- pull region as live (there is no RabbitMQ consumer to observe for it).
CREATE TABLE agent_heartbeats (
    region   text NOT NULL,
    agent_id text NOT NULL,
    seen_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (region, agent_id)
);

-- +goose Down
DROP TABLE IF EXISTS agent_heartbeats;
DROP TABLE IF EXISTS pull_jobs;
