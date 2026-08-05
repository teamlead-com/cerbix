-- +goose Up
-- Allow the subscriber-confirmation outbox topic through the whitelist CHECK.
-- Status-page subscription confirmation emails are now queued to the outbox and
-- delivered by the outbox worker (off the subscribe request path), so a slow or
-- failing SMTP never blocks or errors the subscribe call.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report', 'region_worker_alert', 'escalation_step', 'subscriber_confirm'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report', 'region_worker_alert', 'escalation_step'));
