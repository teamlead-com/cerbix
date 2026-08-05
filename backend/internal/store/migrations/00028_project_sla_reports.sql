-- +goose Up
-- Opt-in weekly SLA report per project (Tier 3). The scheduler leader enqueues a
-- report once every 7 days for each enabled project; sla_report_last_at is the
-- send watermark so a report goes out once per period, not once per tick.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS sla_report_weekly  boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS sla_report_last_at timestamptz;

-- Allow the new report outbox topic through the whitelist CHECK.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert'));
ALTER TABLE projects
    DROP COLUMN IF EXISTS sla_report_weekly,
    DROP COLUMN IF EXISTS sla_report_last_at;
