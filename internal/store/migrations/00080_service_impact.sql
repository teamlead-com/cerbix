-- +goose Up
-- FR-021 phase 3 (spec §14, design APPROVED [269]): the service impact graph, the
-- incident-service impact relation, and the outbox claim fence.

-- ── The graph ────────────────────────────────────────────────────────────────────────────
-- One row per directed edge "child depends on parent". Both composite FKs share ONE
-- project_id column, so a cross-project edge is unrepresentable — the 00060/00061 tenant
-- pattern, not an application-level filter (invariant 48). Edges are OUTSIDE the
-- declaration axes: no definition revision, no epoch, no canonical-hash entry (§14.2).
CREATE TABLE service_dependencies (
    service_id    uuid NOT NULL,
    depends_on_id uuid NOT NULL,
    project_id    uuid NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    text NOT NULL DEFAULT '',
    PRIMARY KEY (service_id, depends_on_id),
    CHECK (service_id <> depends_on_id),
    FOREIGN KEY (service_id, project_id)    REFERENCES services (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (depends_on_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);
CREATE INDEX service_dependencies_parent_idx ON service_dependencies (depends_on_id);

-- graph_generation is the edge set's own concurrency token (§14.2): edges are outside
-- definition revisions, so the declaration CAS cannot protect them. Bumped only by a
-- non-no-op replace-set; read+required by the UI PUT, 409 on mismatch.
ALTER TABLE services ADD COLUMN graph_generation bigint NOT NULL DEFAULT 0;

-- ── Tenant-safe incident anchoring (§14.3, invariant 48) ─────────────────────────────────
-- incidents gains the (id, project_id) unique target so link rows can composite-reference
-- it, and monitor_id — promoted by correlation from a UI convenience into a background-query
-- anchor — becomes a composite FK so a cross-project anchor is unrepresentable, not merely
-- unqueried. SET NULL is column-targeted (PG15+): losing the monitor nulls the anchor, never
-- the tenant.
ALTER TABLE incidents ADD CONSTRAINT incidents_id_project_key UNIQUE (id, project_id);
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_monitor_id_fkey;
ALTER TABLE incidents ADD CONSTRAINT incidents_monitor_project_fkey
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id)
    ON DELETE SET NULL (monitor_id);

-- ── The impact relation (§14.3) ──────────────────────────────────────────────────────────
-- Structured links, tenancy schema-enforced on BOTH endpoints via one shared project_id.
-- path is the canonical ROOT-FIRST, endpoint-inclusive slug sequence: shortest, then
-- lexicographic tie-break; max length = depth cap (10) + 1 = 11 (invariant 55).
CREATE TABLE incident_service_impacts (
    incident_id uuid NOT NULL,
    service_id  uuid NOT NULL,
    project_id  uuid NOT NULL,
    role        text NOT NULL CHECK (role IN ('probable_root', 'affected')),
    path        text[] NOT NULL CHECK (array_length(path, 1) BETWEEN 2 AND 11),
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (incident_id, service_id, role),
    FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (service_id, project_id)  REFERENCES services (id, project_id) ON DELETE CASCADE
);
CREATE INDEX incident_service_impacts_service_idx ON incident_service_impacts (service_id);

-- ── The outbox claim fence, over the WHOLE lifecycle (§14.3, invariant 61) ──────────────
-- ClaimDueOutbox in every deployed binary selects WHERE status = 'pending' with no topic
-- predicate, so during a rolling deployment an old owner would claim a topic it cannot
-- dispatch, burn attempts and dead-letter a durable fact. The barrier is the schema:
--   * fenced is IMMUTABLE from enqueue and survives claim, failure, dead and replay;
--   * a fenced row's claimable state is 'pending_fenced' — invisible to the legacy claim;
--   * the demotion CHECK makes legacy 'pending' UNREPRESENTABLE for a fenced row, so the
--     old replay SQL (SET status='pending' WHERE status='dead') fails closed on the
--     constraint instead of silently unfencing the row.
ALTER TABLE outbox_events ADD COLUMN fenced boolean NOT NULL DEFAULT false;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_status_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'pending_fenced', 'delivered', 'dead'));
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_fence_check
    CHECK (NOT fenced OR status <> 'pending');
CREATE INDEX outbox_events_due_fenced_idx ON outbox_events (next_attempt_at)
    WHERE status = 'pending_fenced';

-- incident_correlation joins the whitelist; it is enqueued in the incident's opening
-- transaction and carried on its own delivery envelope so webhook death never blocks
-- correlation and a correlation failure never blocks incident delivery (invariant 52).
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'region_worker_alert', 'escalation_step', 'subscriber_confirm',
                     'incident_correlation'));

-- +goose Down
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_topic_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_topic_check
    CHECK (topic IN ('incident_event', 'monitor_transition', 'slo_burn_alert', 'sla_report',
                     'region_worker_alert', 'escalation_step', 'subscriber_confirm'));
DROP INDEX IF EXISTS outbox_events_due_fenced_idx;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_fence_check;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_status_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'delivered', 'dead'));
ALTER TABLE outbox_events DROP COLUMN IF EXISTS fenced;
DROP TABLE IF EXISTS incident_service_impacts;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_monitor_project_fkey;
ALTER TABLE incidents ADD CONSTRAINT incidents_monitor_id_fkey
    FOREIGN KEY (monitor_id) REFERENCES monitors (id) ON DELETE SET NULL;
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_id_project_key;
ALTER TABLE services DROP COLUMN IF EXISTS graph_generation;
DROP TABLE IF EXISTS service_dependencies;
