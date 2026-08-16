-- +goose Up
-- +goose StatementBegin

-- iter-0126: two schema defects, one of them introduced by iter-0125's own fix pass.

-- ── 1. The composite owner FK made deletion impossible ──────────────────────────────────
--
-- 00069 wrote `FOREIGN KEY (escalation_policy_id, project_id) ... ON DELETE SET NULL`.
-- Postgres applies the action to EVERY referencing column, so deleting a policy tried to
-- null `project_id` too — and `project_id` is NOT NULL. The result: a policy a service
-- references could not be deleted at all.
--
--   ERROR:  null value in column "project_id" of relation "services"
--   CONTEXT: UPDATE ... SET "escalation_policy_id" = NULL, "project_id" = NULL
--
-- The column-list form says WHICH column to clear, which is what was meant: losing the
-- reference is a routing gap, losing the tenant key is nonsense.
ALTER TABLE services
    DROP CONSTRAINT IF EXISTS services_escalation_policy_tenant_fkey,
    DROP CONSTRAINT IF EXISTS services_oncall_schedule_tenant_fkey;

ALTER TABLE services
    ADD CONSTRAINT services_escalation_policy_tenant_fkey
        FOREIGN KEY (escalation_policy_id, project_id)
        REFERENCES escalation_policies (id, project_id)
        ON DELETE SET NULL (escalation_policy_id),
    ADD CONSTRAINT services_oncall_schedule_tenant_fkey
        FOREIGN KEY (oncall_schedule_id, project_id)
        REFERENCES oncall_schedules (id, project_id)
        ON DELETE SET NULL (oncall_schedule_id);

-- ── 2. Deleting a monitor destroyed retained maintenance provenance ─────────────────────
--
-- `maintenance_windows.monitor_id` cascaded from `monitors`. While deleting a declared
-- monitor was rejected outright this was unreachable; making that delete SUCCEED is what
-- exposed it. Elapsed and archived windows now vanish with the monitor, and the next
-- unrelated recompute no longer sees the exclusion — so it silently rewrites sealed history
-- with no preview, no fence and no audit. §10.9 requires the opposite: an archived row is
-- retained for at least the fact horizon, and ONLY an annul removes a span.
--
-- The FK is dropped rather than switched to SET NULL, because NULL already means something
-- else here: "the whole project". A window that lost its monitor is not a project-wide
-- window — it is a window over a monitor that no longer exists, and its id stays as the
-- retained scoped identity that historical facts were computed against. UUIDs are never
-- reissued, so the reference cannot be captured by a later monitor.
ALTER TABLE maintenance_windows
    DROP CONSTRAINT IF EXISTS maintenance_windows_monitor_id_fkey;

-- The reducer and every suppression query filter by monitor_id, so the lookup keeps its index
-- now that the FK's implicit one is gone.
CREATE INDEX IF NOT EXISTS maintenance_windows_monitor_idx
    ON maintenance_windows (monitor_id) WHERE monitor_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS maintenance_windows_monitor_idx;
-- The monitors FK is not restored: rows may now reference monitors that no longer exist, and
-- adding it back would fail on exactly the provenance this migration exists to retain.

ALTER TABLE services
    DROP CONSTRAINT IF EXISTS services_escalation_policy_tenant_fkey,
    DROP CONSTRAINT IF EXISTS services_oncall_schedule_tenant_fkey;

ALTER TABLE services
    ADD CONSTRAINT services_escalation_policy_tenant_fkey
        FOREIGN KEY (escalation_policy_id, project_id)
        REFERENCES escalation_policies (id, project_id) ON DELETE SET NULL,
    ADD CONSTRAINT services_oncall_schedule_tenant_fkey
        FOREIGN KEY (oncall_schedule_id, project_id)
        REFERENCES oncall_schedules (id, project_id) ON DELETE SET NULL;
-- +goose StatementEnd
