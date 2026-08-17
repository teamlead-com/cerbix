-- +goose Up
-- FR-021 phase 5 (spec §16, D-0168): alerting ownership.
--
-- Written AFTER two design rounds rejected the shape this file first had. The three things that
-- changed the schema, each because it could lose a page:
--
--   * delegation is per SIGNAL and must be ARMED — quotable, routable, generation-matched and fresh —
--     so arming needs generations and a DB-clock lease, not a boolean;
--   * a CLOSE must survive the deletion of what fired and must reach the people the ONSET reached, so
--     an onset creates a durable EPISODE with an immutable recipient snapshot;
--   * the service burn latch cannot live in `sla_targets.burn_rules` (an operator edits that array
--     wholesale), so it is normalized per rule — while the MONITOR latch stays exactly where it is,
--     because phase 5 changes no monitor behaviour.

-- ── The declaration ──────────────────────────────────────────────────────────────────────
ALTER TABLE services ADD COLUMN owns_paging boolean NOT NULL DEFAULT false;

-- `page_on` is a subset of {down, degraded}. `unknown` is deliberately NOT expressible here: it has
-- its own switch below, because one list would let a UI offering "all states" enable "we cannot see
-- it" while meaning "it is broken".
ALTER TABLE services ADD COLUMN page_on text[] NOT NULL DEFAULT '{down}';
ALTER TABLE services ADD CONSTRAINT services_page_on_chk
    CHECK (page_on <@ ARRAY['down', 'degraded']::text[]);
ALTER TABLE services ADD COLUMN page_on_unknown boolean NOT NULL DEFAULT false;

ALTER TABLE services ADD COLUMN confirm_evaluations integer NOT NULL DEFAULT 2;
ALTER TABLE services ADD CONSTRAINT services_confirm_evaluations_chk
    CHECK (confirm_evaluations BETWEEN 1 AND 10);

-- Every paging-config change bumps this, and a bump DIS-ARMS delegation until the new generation has
-- been evaluated. That is the safe direction: dis-armed means the member monitors page for
-- themselves. It is SERVER-OWNED — rejected in request bodies and excluded from the MaC canonical
-- hash, or a bundle whose hash moved because an alert fired would reapply forever.
ALTER TABLE services ADD COLUMN alert_config_generation bigint NOT NULL DEFAULT 0;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION service_alert_config_bump() RETURNS trigger AS $$
BEGIN
    IF NEW.owns_paging IS DISTINCT FROM OLD.owns_paging
       OR NEW.page_on IS DISTINCT FROM OLD.page_on
       OR NEW.page_on_unknown IS DISTINCT FROM OLD.page_on_unknown
       OR NEW.confirm_evaluations IS DISTINCT FROM OLD.confirm_evaluations THEN
        NEW.alert_config_generation := OLD.alert_config_generation + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- The generation is owned by the DATABASE, not by whichever write path remembered: an API PATCH, a
-- MaC apply and a direct UPDATE must all dis-arm, and "every writer remembers" is the assumption
-- phase 4 had to remove twice.
CREATE TRIGGER service_alert_config_bump_trg
    BEFORE UPDATE ON services
    FOR EACH ROW EXECUTE FUNCTION service_alert_config_bump();

-- ── The live signal's state ──────────────────────────────────────────────────────────────
--
-- Five distinct facts, deliberately five columns, because conflating any two of them is how a
-- notifier ends up silent or repetitive:
--
--   observed/candidate/streak  the confirmation counter — noise must not page;
--   live_firing                the LEVEL, which `state != emitted_state` cannot express: with
--                              page_on={down}, healthy→degraded must not fire and degraded→healthy
--                              must not emit a recovery;
--   emitted_state/_seq         what was ENQUEUED (not delivered — the outbox is at-least-once) and
--                              its order, so a retried onset cannot re-announce a state already left;
--   config_generation/revision what this evaluation was ABOUT, so arming cannot rest on a verdict
--                              for a configuration or a membership that no longer applies;
--   evaluated_at/lease_until   freshness, on the DATABASE's clock so a skewed leader cannot extend
--                              its own arming.
CREATE TABLE service_alert_state (
    service_id        uuid PRIMARY KEY,
    project_id        uuid NOT NULL,
    observed_state    text NOT NULL,
    candidate_state   text NOT NULL,
    streak            integer NOT NULL DEFAULT 1 CHECK (streak >= 1),
    live_firing       boolean NOT NULL DEFAULT false,
    -- NULL means "nothing has ever been announced", which differs from "healthy was announced": a
    -- service that starts life healthy must not emit a recovery.
    emitted_state     text,
    emitted_seq       bigint NOT NULL DEFAULT 0,
    emitted_at        timestamptz,
    config_generation bigint NOT NULL,
    revision_id       uuid,
    evaluated_at      timestamptz NOT NULL,
    lease_until       timestamptz NOT NULL,
    last_error        text,
    CONSTRAINT service_alert_state_observed_chk
        CHECK (observed_state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    CONSTRAINT service_alert_state_candidate_chk
        CHECK (candidate_state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    CONSTRAINT service_alert_state_emitted_chk
        CHECK (emitted_state IS NULL
               OR emitted_state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    CONSTRAINT service_alert_state_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- The evaluator claims services by keyset over a bounded slice, so it orders by freshness.
CREATE INDEX service_alert_state_lease_idx ON service_alert_state (lease_until, service_id);

-- ── The sealed signal: lift EXACTLY the phase-2 rejection, then normalize its latch ──────
ALTER TABLE sla_targets DROP CONSTRAINT sla_targets_service_no_burn_chk;

-- A target generation, for the same reason services have one: editing rules must dis-arm.
ALTER TABLE sla_targets ADD COLUMN alert_generation bigint NOT NULL DEFAULT 0;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sla_target_alert_generation_bump() RETURNS trigger AS $$
BEGIN
    -- Only the DECLARED fields count. `burn_rules` carries the MONITOR path's latch inside its JSON,
    -- so a monitor alert firing must not look like a configuration change.
    IF NEW.service_id IS NOT NULL
       AND (NEW.burn_alert_enabled IS DISTINCT FROM OLD.burn_alert_enabled
            OR NEW.objective IS DISTINCT FROM OLD.objective
            OR NEW.window_name IS DISTINCT FROM OLD.window_name) THEN
        NEW.alert_generation := OLD.alert_generation + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER sla_target_alert_generation_bump_trg
    BEFORE UPDATE ON sla_targets
    FOR EACH ROW EXECUTE FUNCTION sla_target_alert_generation_bump();

-- One row per rule, keyed by a CANONICAL key over the rule's declared fields only. The key excludes
-- `firing` and every other server-owned value: a rule that changed identity by firing would orphan
-- its own latch and re-arm coverage on every edge.
CREATE TABLE service_burn_alert_state (
    service_id        uuid NOT NULL,
    project_id        uuid NOT NULL,
    sla_target_id     uuid NOT NULL REFERENCES sla_targets (id) ON DELETE CASCADE,
    rule_key          text NOT NULL,
    firing            boolean NOT NULL DEFAULT false,
    last_verdict      text NOT NULL,
    -- The §16.4 HOLD reason. A HOLD is a SUCCESSFUL evaluation that cannot speak, which is why it
    -- DIS-ARMS burn coverage: a rule that cannot fire is not a replacement for a member's alert.
    last_reason       text,
    target_generation bigint NOT NULL,
    config_generation bigint NOT NULL,
    emitted_seq       bigint NOT NULL DEFAULT 0,
    emitted_at        timestamptz,
    evaluated_at      timestamptz NOT NULL,
    lease_until       timestamptz NOT NULL,
    last_error        text,
    PRIMARY KEY (service_id, project_id, sla_target_id, rule_key),
    CONSTRAINT service_burn_alert_state_verdict_chk
        CHECK (last_verdict IN ('fire', 'clear', 'hold')),
    CONSTRAINT service_burn_alert_state_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

CREATE INDEX service_burn_alert_state_lease_idx ON service_burn_alert_state (lease_until, service_id);

-- ── The firing episode: what makes a CLOSE possible at all ───────────────────────────────
--
-- An ONSET creates an episode. Two properties are the whole point, and neither is achievable from a
-- state row that holds only the current level:
--
--   * the RECIPIENT SNAPSHOT is immutable. A schedule rotates; re-resolving at close time would page
--     somebody who never heard the onset and leave the person who did holding an open alert;
--   * the episode SURVIVES its service. `service_id` is nullable and the project, signal, recipients
--     and sequence outlive it, so every destructive path can enqueue its close in the same
--     transaction and the close is still deliverable after the service, target or rule is gone.
CREATE TABLE service_alert_episodes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id    uuid,
    project_id    uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- Kept as text, not an FK: the name is part of the historical record and must still read
    -- correctly after the service is gone.
    service_name  text NOT NULL,
    signal        text NOT NULL,
    rule_key      text,
    -- Target identity as a SNAPSHOT, which is why it is named that way and carries no foreign key.
    -- It is needed because 00077 keys service targets by (service_id, window_name), so ONE service
    -- may hold a 7d and a 30d target whose burn rules share a canonical key (the key is
    -- severity/windows/threshold and contains no window name); without the target here those two
    -- rules are ONE episode, the second onset unique-violates and a close binds the wrong one.
    -- A LIVE reference is the wrong tool: `rule_removed` and target deletion close THROUGH this row,
    -- and an FK — cascading or nulling — would erase the identity at exactly the moment the close
    -- needs it. `service_id` below is the live reference; these two are the historical record.
    target_snapshot_id uuid,
    -- The window name at onset, snapshotted for the record and for the page text: two targets
    -- carrying the identical rule differ ONLY by their window, so a page without it is unreadable.
    target_window text,
    state         text NOT NULL,
    started_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz,
    close_reason  text,
    recipients    jsonb NOT NULL DEFAULT '[]'::jsonb,
    emitted_seq   bigint NOT NULL,
    CONSTRAINT service_alert_episodes_signal_chk CHECK (signal IN ('health', 'burn')),
    CONSTRAINT service_alert_episodes_close_chk CHECK (
        (closed_at IS NULL AND close_reason IS NULL)
        OR (closed_at IS NOT NULL AND close_reason IN (
            'recovered', 'visibility_lost', 'entered_maintenance', 'ownership_disabled',
            'policy_changed', 'burn_disabled', 'rule_removed', 'service_deleted'))),
    -- The burn signal is per rule OF A TARGET; the health signal has neither. The three travel
    -- together because a rule key without its target does not identify a rule.
    CONSTRAINT service_alert_episodes_rule_chk
        CHECK ((signal = 'burn') = (rule_key IS NOT NULL)
           AND (signal = 'burn') = (target_snapshot_id IS NOT NULL)
           AND (signal = 'burn') = (target_window IS NOT NULL)),
    -- The survival promise is the DATABASE's, not the delete path's. Lifecycle code still closes and
    -- enqueues in the same transaction as a removal, but a direct SQL DELETE, a cascade from the
    -- project, or a path nobody has written yet must not be able to leave an episode pointing at a
    -- service that no longer exists. PostgreSQL 15's column-list SET NULL is exactly this promise:
    -- the DB clears ONLY service_id and keeps project_id, so the tenant, the recipients, the name
    -- and the target snapshot all outlive the service and the close stays deliverable. (Every
    -- cerbix deployment target is Postgres 16 — dev, prod compose, CI and docs all pin it.)
    -- On the way IN the same constraint refuses an episode whose (service, project) pair is not a
    -- real service: a cross-tenant episode would be a page addressed to the wrong customer.
    CONSTRAINT service_alert_episodes_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id)
        ON DELETE SET NULL (service_id)
);

-- At most ONE open episode per (service, signal, TARGET, rule): a second would make "the close"
-- ambiguous. The target belongs in the key — see the column comment: two targets of one service may
-- legally carry the same canonical rule key, and they are two independent alerts.
CREATE UNIQUE INDEX service_alert_episodes_open_uniq
    ON service_alert_episodes
       (service_id, signal, COALESCE(target_snapshot_id::text, ''), COALESCE(rule_key, ''))
    WHERE closed_at IS NULL AND service_id IS NOT NULL;
CREATE INDEX service_alert_episodes_open_idx ON service_alert_episodes (project_id)
    WHERE closed_at IS NULL;

-- ── Suppression history: countable, idempotent, bounded ──────────────────────────────────
--
-- A suppressed delivery leaves no trace in `outbox_events` (the row is marked delivered) and none in
-- the channels, so without this table "did anyone get told?" is answerable only from process logs —
-- which is the first question asked after an incident.
--
-- ONE ROW PER (event, owner): a monitor muted by two services records both. The UNIQUE key is what
-- makes a redelivery idempotent — the outbox is at-least-once, so without it an audit-shaped table
-- grows a duplicate row on every retry.
CREATE TABLE alert_suppressions (
    outbox_event_id uuid NOT NULL REFERENCES outbox_events (id) ON DELETE CASCADE,
    monitor_id      uuid NOT NULL,
    project_id      uuid NOT NULL,
    service_id      uuid NOT NULL,
    topic           text NOT NULL,
    reason          text NOT NULL,
    suppressed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (outbox_event_id, service_id, topic),
    CONSTRAINT alert_suppressions_reason_chk CHECK (reason IN ('service_delegation')),
    CONSTRAINT alert_suppressions_topic_chk
        CHECK (topic IN ('monitor_transition', 'escalation_step', 'slo_burn_alert')),
    CONSTRAINT alert_suppressions_monitor_fkey
        FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE,
    CONSTRAINT alert_suppressions_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

CREATE INDEX alert_suppressions_monitor_idx ON alert_suppressions (monitor_id, suppressed_at DESC);
-- Reminders and escalation repeats make this table unbounded by construction, so it is purged on the
-- existing retention sub-tick.
CREATE INDEX alert_suppressions_purge_idx ON alert_suppressions (suppressed_at);

-- ── The new alert topic, FENCED ──────────────────────────────────────────────────────────
--
-- Fenced, like `incident_correlation`: a topic whitelisted in the schema but unknown to an old worker
-- would be claimed and dead-lettered during a rolling upgrade. The application half is
-- `domain.FencedTopics()`.
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    -- CUMULATIVE: this list REPLACES the constraint, so it must name every topic the binary can
    -- still enqueue, not only the ones this phase cares about. Dropping a live topic here does not
    -- fail a migration or a build — it fails every INSERT of that topic at runtime, which is how an
    -- escalation ladder, a region-worker alert and a subscriber confirmation stop silently.
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'region_worker_alert', 'escalation_step', 'subscriber_confirm',
                     'incident_correlation', 'service_alert'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    -- The pre-phase-5 list, exactly as 00080 left it.
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'region_worker_alert', 'escalation_step', 'subscriber_confirm',
                     'incident_correlation'));
DROP TABLE IF EXISTS alert_suppressions;
DROP TABLE IF EXISTS service_alert_episodes;
DROP TABLE IF EXISTS service_burn_alert_state;
DROP TRIGGER IF EXISTS sla_target_alert_generation_bump_trg ON sla_targets;
DROP FUNCTION IF EXISTS sla_target_alert_generation_bump();
ALTER TABLE sla_targets DROP COLUMN IF EXISTS alert_generation;
ALTER TABLE sla_targets ADD CONSTRAINT sla_targets_service_no_burn_chk CHECK (
    service_id IS NULL OR (burn_alert_enabled = false AND burn_rules = '[]'::jsonb));
DROP TABLE IF EXISTS service_alert_state;
DROP TRIGGER IF EXISTS service_alert_config_bump_trg ON services;
DROP FUNCTION IF EXISTS service_alert_config_bump();
ALTER TABLE services DROP COLUMN IF EXISTS alert_config_generation;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_confirm_evaluations_chk;
ALTER TABLE services DROP COLUMN IF EXISTS confirm_evaluations;
ALTER TABLE services DROP COLUMN IF EXISTS page_on_unknown;
ALTER TABLE services DROP CONSTRAINT IF EXISTS services_page_on_chk;
ALTER TABLE services DROP COLUMN IF EXISTS page_on;
ALTER TABLE services DROP COLUMN IF EXISTS owns_paging;
