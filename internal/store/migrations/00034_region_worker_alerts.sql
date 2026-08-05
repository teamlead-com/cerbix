-- +goose Up
-- region_worker_alerts latches, per worker-pool region, whether that region is
-- currently missing a live worker while it still has enabled monitors. The scheduler
-- (leader) evaluates it against the live-consumer set from the RabbitMQ management API
-- and enqueues an alert on each edge, so a region whose worker dies does not stall its
-- monitors silently. One row per region; absence means "not currently missing".
CREATE TABLE region_worker_alerts (
    region      text PRIMARY KEY,
    missing     boolean     NOT NULL,
    since       timestamptz NOT NULL DEFAULT now(),
    notified_at timestamptz
);

-- Allow the new region-worker outbox topic through the whitelist CHECK.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report', 'region_worker_alert'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report'));
DROP TABLE IF EXISTS region_worker_alerts;
