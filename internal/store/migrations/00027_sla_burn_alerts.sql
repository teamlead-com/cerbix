-- +goose Up
-- Per-target SLO burn-rate alerting (Tier 3). A burn-enabled target is evaluated
-- by the scheduler leader: it measures the bad-heartbeat fraction over a short
-- window and compares the resulting burn rate against a threshold. burn_firing is
-- the edge-trigger latch so an alert (and its recovery) is sent once, not per tick.
ALTER TABLE sla_targets
    ADD COLUMN IF NOT EXISTS burn_alert_enabled  boolean          NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS burn_window_seconds integer          NOT NULL DEFAULT 3600,
    ADD COLUMN IF NOT EXISTS burn_threshold      double precision NOT NULL DEFAULT 14.4,
    ADD COLUMN IF NOT EXISTS burn_firing         boolean          NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS burn_notified_at    timestamptz;

-- Allow the new burn-alert outbox topic through the whitelist CHECK.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition'));
ALTER TABLE sla_targets
    DROP COLUMN IF EXISTS burn_alert_enabled,
    DROP COLUMN IF EXISTS burn_window_seconds,
    DROP COLUMN IF EXISTS burn_threshold,
    DROP COLUMN IF EXISTS burn_firing,
    DROP COLUMN IF EXISTS burn_notified_at;
