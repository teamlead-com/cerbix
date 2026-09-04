-- +goose Up
-- FR-032 phase A (D-0237): what a monitor's configuration GENERATION actually was.
--
-- `monitors.execution_revision` has existed since 00055 as a bare counter: it says a monitor is on
-- generation 7 and nothing anywhere records what generation 7 WAS. That is enough for the D-0142
-- fence, which only has to compare two numbers, and useless for the expected-run ledger, which must
-- attribute a window's interval, timeout and retry count to the configuration in force for THAT
-- window rather than to the monitor's current fields (spec invariant 13).
--
-- One row per (monitor, generation). `UpdateMonitor` bumps the revision on ANY write — deliberately
-- coarse, see internal/domain/execsemantics.go — so rows are identical-but-renumbered whenever a
-- write touched nothing cadence-related. That is accepted: a duplicate row costs a few bytes, and
-- deduplicating would reintroduce the field allowlist that file warns against.
CREATE TABLE IF NOT EXISTS monitor_execution_revisions (
    project_id               uuid        NOT NULL,
    monitor_id               uuid        NOT NULL,
    execution_revision       bigint      NOT NULL,
    interval_seconds         int         NOT NULL,
    confirm_interval_seconds int         NOT NULL,
    timeout_seconds          int         NOT NULL,
    retries                  int         NOT NULL,
    effective_from           timestamptz NOT NULL,
    PRIMARY KEY (monitor_id, execution_revision),
    -- Tenant-safe composite FK, the form 00060/00061/00064/00080 already use: a single-column
    -- reference would let a row outlive its tenant's isolation boundary.
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE
);

-- Backfill: exactly one row per monitor, at its CURRENT revision, from its CURRENT fields.
--
-- The limitation is real and is stated in the spec rather than discovered later: revisions that
-- already exist cannot be reconstructed, so the timeline begins at migration time and every window
-- before it is `unknown` — never `covered`. `effective_from` is `monitors.updated_at` because that
-- is the only instant the row can honestly claim: it is when the current configuration was last
-- written. No monitor is excluded — not disabled ones, not retired ones — because a disabled
-- monitor's history is still history, and a retired monitor can be restored (00060's restore path
-- bumps the revision, and a generation with no row is a window whose configuration cannot be
-- described).
INSERT INTO monitor_execution_revisions
    (project_id, monitor_id, execution_revision,
     interval_seconds, confirm_interval_seconds, timeout_seconds, retries, effective_from)
SELECT m.project_id, m.id, m.execution_revision,
       m.interval_seconds, m.confirm_interval_seconds, m.timeout_seconds, m.retries, m.updated_at
  FROM monitors m
ON CONFLICT (monitor_id, execution_revision) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS monitor_execution_revisions;
