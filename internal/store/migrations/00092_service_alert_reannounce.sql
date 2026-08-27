-- +goose Up

-- An announcement that reached nobody gets ANOTHER chance, once there is somebody to tell (D-0187).
--
-- D-0179 stopped the swallow: coverage now requires a delivery that succeeded, so members keep
-- paging while an announcement is undelivered. What it deliberately did not do is tell anyone
-- afterwards. The latch stays firing, the evaluator sees no edge, and when the operator fixes the
-- channel the service's own alert is still never sent — the outage is real, the incident is open, and
-- the only people who know are the members paging for themselves.
--
-- `undelivered_seq` is the fact the evaluator was missing: not "this has not been delivered YET",
-- which is also true of an event still sitting in the outbox, but "this announcement is KNOWN dead".
-- The worker sets it only on the terminal paths — an empty recipient snapshot, or a delivery that
-- resolved nobody — never while a retry is still owed. Re-announcing on the weaker fact would send a
-- second copy of an event that was merely slow.
ALTER TABLE service_alert_state
    ADD COLUMN IF NOT EXISTS undelivered_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE service_burn_alert_state
    ADD COLUMN IF NOT EXISTS undelivered_seq bigint NOT NULL DEFAULT 0;

-- The re-announcement supersedes the episode nobody heard, and the record has to say WHY.
--
-- The onset path already closes an open episode before opening the next one, and it did so as
-- `policy_changed` — which would be a lie here, because nothing about the policy changed. An episode
-- closed under this reason is one whose announcement was never received: its recipient snapshot names
-- people who could not be reached, and the episode that replaces it carries whoever can be.
-- +goose StatementBegin
DO $$
BEGIN
    ALTER TABLE service_alert_episodes DROP CONSTRAINT IF EXISTS service_alert_episodes_close_chk;
    ALTER TABLE service_alert_episodes ADD CONSTRAINT service_alert_episodes_close_chk CHECK (
        (closed_at IS NULL AND close_reason IS NULL)
        OR (closed_at IS NOT NULL AND close_reason IN (
            'recovered', 'visibility_lost', 'entered_maintenance', 'ownership_disabled',
            'policy_changed', 'burn_disabled', 'rule_removed', 'service_deleted',
            'undelivered')));
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    UPDATE service_alert_episodes SET close_reason = 'policy_changed'
     WHERE close_reason = 'undelivered';
    ALTER TABLE service_alert_episodes DROP CONSTRAINT IF EXISTS service_alert_episodes_close_chk;
    ALTER TABLE service_alert_episodes ADD CONSTRAINT service_alert_episodes_close_chk CHECK (
        (closed_at IS NULL AND close_reason IS NULL)
        OR (closed_at IS NOT NULL AND close_reason IN (
            'recovered', 'visibility_lost', 'entered_maintenance', 'ownership_disabled',
            'policy_changed', 'burn_disabled', 'rule_removed', 'service_deleted')));
END
$$;
-- +goose StatementEnd
ALTER TABLE service_alert_state DROP COLUMN IF EXISTS undelivered_seq;
ALTER TABLE service_burn_alert_state DROP COLUMN IF EXISTS undelivered_seq;
