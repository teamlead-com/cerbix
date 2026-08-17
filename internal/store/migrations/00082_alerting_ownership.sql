-- +goose Up
-- FR-021 phase 5 (spec §16, owner decisions of 2026-08-17, D-0168): alerting ownership.
--
-- §13 held the rule as intent because monitor burn alerting already pages, so turning on service
-- alerts without an ownership rule pages twice for one failure. The owner's decisions: ownership is
-- DECLARED at the service and defaults to OFF, a service alerts on a LIVE health transition AND a
-- SEALED burn breach, the alert notifies without opening an incident, and service burn rules are
-- the same `BurnRule` monitors already use.

-- ── The declaration ──────────────────────────────────────────────────────────────────────
--
-- Ownership is a column on the SERVICE, defaulting to false: an installation that already pages
-- from its monitors keeps paging from its monitors until an operator says otherwise. The same rule
-- §17 applies to public output applies to who gets woken.
ALTER TABLE services ADD COLUMN owns_paging boolean NOT NULL DEFAULT false;

-- Which STATES page is declared, not assumed. `down` is the default; `degraded` is opt-in, because
-- a service that declared a degraded band usually declared it to distinguish "worse" from "wake
-- me". UNKNOWN is deliberately NOT a member of this array — see below.
ALTER TABLE services ADD COLUMN page_on text[] NOT NULL DEFAULT '{down}';
ALTER TABLE services ADD CONSTRAINT services_page_on_chk
    CHECK (page_on <@ ARRAY['down', 'degraded']::text[]);

-- UNKNOWN gets its OWN switch rather than a slot in `page_on`, and it defaults to false. Announcing
-- "we cannot see it" as an outage is the confident falsehood this whole feature exists to remove;
-- never mentioning a service nobody can measure is the opposite failure. A separate, explicit,
-- off-by-default switch is the only shape that refuses both.
ALTER TABLE services ADD COLUMN page_on_unknown boolean NOT NULL DEFAULT false;

-- Confirmation is in EVALUATIONS, over a fixed cadence, so the worst-case delay is computable and
-- the UI can print it instead of leaving an operator to multiply.
ALTER TABLE services ADD COLUMN confirm_evaluations integer NOT NULL DEFAULT 2;
ALTER TABLE services ADD CONSTRAINT services_confirm_evaluations_chk
    CHECK (confirm_evaluations BETWEEN 1 AND 10);

-- ── The live signal's state ──────────────────────────────────────────────────────────────
--
-- The live evaluation needs stored state for THREE distinct reasons, and conflating them is how a
-- notifier ends up either silent or repetitive:
--
--   `state`/`streak`  — the confirmation counter: a transition notifies only after N consecutive
--                       evaluations, so member noise does not page;
--   `notified_state`  — what the last DELIVERED notification actually announced, which is what
--                       makes the emission edge-triggered rather than level-triggered. It is
--                       deliberately NOT the same column as `state`: the current state changes on
--                       every evaluation, while "what we have told the operator" changes only when
--                       something was enqueued;
--   `evaluated_at`    — the liveness of the evaluator itself. A stalled leader is then visible as a
--                       stale row instead of as an absence of alerts, which is indistinguishable
--                       from "nothing is wrong".
CREATE TABLE service_alert_state (
    service_id     uuid PRIMARY KEY REFERENCES services (id) ON DELETE CASCADE,
    project_id     uuid NOT NULL,
    state          text NOT NULL,
    since          timestamptz NOT NULL,
    streak         integer NOT NULL DEFAULT 1 CHECK (streak >= 1),
    -- NULL means "nothing has ever been announced for this service", which is different from
    -- "healthy was announced": a service that starts life healthy must not emit a recovery.
    notified_state text,
    notified_at    timestamptz,
    evaluated_at   timestamptz NOT NULL,
    CONSTRAINT service_alert_state_state_chk
        CHECK (state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    CONSTRAINT service_alert_state_notified_chk
        CHECK (notified_state IS NULL
               OR notified_state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    -- The project is carried for the tenant-composite FK, so a row cannot describe a service in
    -- another project even if written directly.
    CONSTRAINT service_alert_state_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- The evaluator claims the alerting services of ALL projects in one pass, so the index it needs is
-- on the freshness of the row, not on the project.
CREATE INDEX service_alert_state_evaluated_idx ON service_alert_state (evaluated_at);

-- ── The sealed signal: lift EXACTLY the phase-2 rejection ────────────────────────────────
--
-- 00077 rejected a service-scoped burn alert at the schema itself, so that no application bug
-- could enable paging semantics the spec had deliberately left unowned. §16 owns them now, so the
-- CHECK goes — and nothing else about that table changes: same `burn_rules` shape, same
-- `burn_alert_enabled` flag, same three-way scope exclusivity.
ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_service_no_burn_chk;

-- ── The new alert topic ──────────────────────────────────────────────────────────────────
--
-- A service alert is its OWN topic. Riding on `slo_burn_alert` would have made the delegation rule
-- unwritable — the worker could no longer tell whose alert it was holding — and riding on
-- `monitor_transition` would have needed a monitor id that does not exist.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'incident_correlation', 'service_alert'));

-- ── Suppression must be countable, not only loggable ─────────────────────────────────────
--
-- A suppressed delivery leaves no trace in `outbox_events` (the row is marked delivered) and none
-- in the notification channels, so without this table "did anyone get told?" is answerable only
-- from process logs — which is exactly the question an operator asks after an incident. One row per
-- suppressed delivery, bounded by retention like every other operational fact.
CREATE TABLE alert_suppressions (
    id           bigserial PRIMARY KEY,
    monitor_id   uuid NOT NULL,
    project_id   uuid NOT NULL,
    -- The service that owned the paging. NULL is impossible for `service_delegation` and is
    -- allowed only so a future reason can reuse this table without a migration.
    service_id   uuid,
    topic        text NOT NULL,
    reason       text NOT NULL,
    suppressed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alert_suppressions_reason_chk CHECK (reason IN ('service_delegation')),
    CONSTRAINT alert_suppressions_topic_chk
        CHECK (topic IN ('monitor_transition', 'slo_burn_alert')),
    CONSTRAINT alert_suppressions_monitor_fkey
        FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE,
    CONSTRAINT alert_suppressions_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE SET NULL (service_id)
);

CREATE INDEX alert_suppressions_monitor_idx ON alert_suppressions (monitor_id, suppressed_at DESC);
CREATE INDEX alert_suppressions_purge_idx ON alert_suppressions (suppressed_at);

-- +goose Down
DROP TABLE IF EXISTS alert_suppressions;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'incident_correlation'));
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_service_no_burn_chk CHECK (
    service_id IS NULL OR (burn_alert_enabled = false AND burn_rules = '[]'::jsonb));
DROP TABLE IF EXISTS service_alert_state;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_confirm_evaluations_chk;
ALTER TABLE services DROP COLUMN IF EXISTS confirm_evaluations;
ALTER TABLE services DROP COLUMN IF EXISTS page_on_unknown;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_page_on_chk;
ALTER TABLE services DROP COLUMN IF EXISTS page_on;
ALTER TABLE services DROP COLUMN IF EXISTS owns_paging;
