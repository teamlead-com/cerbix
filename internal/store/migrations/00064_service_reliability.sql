-- +goose NO TRANSACTION
-- Service reliability domain (spec func-service-reliability §6, §10; D-0159).
--
-- NO TRANSACTION because the fact table is partitioned and its partitions are created in a
-- guarded DO block, following the pattern 00043 established for the plain heartbeat mode.
--
-- Two structural decisions are worth reading before the DDL, because both were design
-- findings rather than preferences:
--
--   * The fact table uses NATIVE UTC RANGE PARTITIONS in BOTH storage modes — no
--     hypertable, no compression. The reason is specific to this table: by design it
--     rewrites SEALED rows weeks old under an audited recompute, which is exactly the
--     access pattern compressed chunks serve badly. One code path and retention by
--     partition drop are worth more here than reusing the heartbeat machinery.
--
--   * Historical revision members carry NO foreign key to monitors, while the current
--     reference table does. A single FK cannot serve both: history must survive the
--     deletion of a monitor it once named, and the delete guard must still fire for the
--     revision currently in force. Splitting them is what makes both true.

-- +goose Up
-- +goose StatementBegin

-- ── The resource ────────────────────────────────────────────────────────────────────────
--
-- slug is project-unique and immutable: it is the MaC reference key and the URL segment.
-- The owner is a REFERENCE to existing routing primitives, never a free-text team label, so
-- "who is responsible" is actionable. ON DELETE SET NULL matches monitors.escalation_policy_id:
-- losing the reference is a routing gap, not a correctness problem, and phase 1 does not
-- alert on services at all.
CREATE TABLE services (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    slug                 text NOT NULL,
    name                 text NOT NULL,
    description          text NOT NULL DEFAULT '',
    escalation_policy_id uuid REFERENCES escalation_policies (id) ON DELETE SET NULL,
    oncall_schedule_id   uuid REFERENCES oncall_schedules (id) ON DELETE SET NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug),
    -- FK target for every tenant-safe composite reference below (the 00060/00061 pattern).
    UNIQUE (id, project_id)
);

CREATE INDEX services_project_idx ON services (project_id);

-- ── Axis 1: the declaration ─────────────────────────────────────────────────────────────
--
-- created_at is when the write happened; effective_at is when the row starts to GOVERN
-- buckets, and it is CEILED to a canonical bucket boundary so a boundary never splits a
-- bucket. The two are separate columns because conflating them is what made an earlier
-- draft contradict itself about which instant a revision takes effect.
--
-- state exists for the same-boundary race: two writes seconds apart both target the next
-- boundary, and immutable rows plus a half-open interval leave no order between them. The
-- later write marks the earlier one superseded_before_effect in the same transaction; the
-- row is retained for audit, is never referenced by a fact, and contributes no validity
-- interval. The partial unique index below is what enforces "exactly one winner".
CREATE TABLE service_definition_revisions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id   uuid NOT NULL,
    project_id   uuid NOT NULL,
    revision     bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT statement_timestamp(),
    effective_at timestamptz NOT NULL,
    state        text NOT NULL DEFAULT 'effective'
                 CHECK (state IN ('effective', 'superseded_before_effect')),
    policies     jsonb NOT NULL DEFAULT '{}',
    created_by   text NOT NULL DEFAULT '',
    UNIQUE (service_id, revision),
    UNIQUE (id, project_id),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX service_definition_revisions_effective_uniq
    ON service_definition_revisions (service_id, effective_at)
    WHERE state = 'effective';

CREATE INDEX service_definition_revisions_lookup_idx
    ON service_definition_revisions (service_id, effective_at DESC)
    WHERE state = 'effective';

-- Historical membership. Deliberately WITHOUT a foreign key to monitors: a revision from
-- three months ago must remain readable after the monitor it named is deleted, or the
-- timeline loses the very boundary that explains why the number changed. monitor_name is
-- the snapshot that keeps such a row legible; the live guard lives in service_member_refs.
CREATE TABLE service_definition_members (
    revision_id  uuid NOT NULL,
    project_id   uuid NOT NULL,
    monitor_id   uuid NOT NULL,
    monitor_name text NOT NULL DEFAULT '',
    -- 'context' is operational membership (monitors[]); 'sli' is a declared reliability
    -- input. They are separate rows because they are separately declared: adding a monitor
    -- for diagnostics must never silently redefine what availability means.
    role         text NOT NULL CHECK (role IN ('context', 'sli')),
    PRIMARY KEY (revision_id, monitor_id, role),
    FOREIGN KEY (revision_id, project_id)
        REFERENCES service_definition_revisions (id, project_id) ON DELETE CASCADE
);

-- The CURRENT effective membership, normalized — the guarded-reference contract FR-020
-- built for secret refs, reused rather than reinvented. The deferred monitor FK is the
-- commit-time delete guard: deleting a monitor an in-force SLI names fails at COMMIT and is
-- mapped to 409, while a project delete stays order-independent because both sides go.
CREATE TABLE service_member_refs (
    service_id uuid NOT NULL,
    project_id uuid NOT NULL,
    monitor_id uuid NOT NULL,
    role       text NOT NULL CHECK (role IN ('context', 'sli')),
    PRIMARY KEY (service_id, monitor_id, role),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id)
        ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED
);

-- The ingest handshake resolves "which services declare this monitor as an SLI member" on
-- every inserted heartbeat, so that lookup gets its own index.
CREATE INDEX service_member_refs_monitor_idx ON service_member_refs (monitor_id, role);

-- ...but the handshake asks that question AS OF the heartbeat's own bucket, which means
-- reading HISTORICAL membership rather than the current refs. Most monitors have never been
-- a reliability input for anything, and this index is what turns that overwhelmingly common
-- case into one probe instead of a scan over the project's revision history.
CREATE INDEX service_definition_members_monitor_idx
    ON service_definition_members (monitor_id, role);

-- ── Axis 2: the observed execution semantics ────────────────────────────────────────────
--
-- An epoch is a system-authored immutable projection of what the evaluator READS. It never
-- changes a declaration, which is why an execution-property change creates epochs for every
-- referencing service regardless of who owns that service.
--
-- EVERY definition revision gets a matching epoch, unconditionally: a revision with no
-- epoch is an unsatisfiable reference, since a fact points at the epoch alone. The
-- snapshot_hash no-op rule applies only to epochs driven by a monitor execution write.
CREATE TABLE service_evaluation_epochs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id    uuid NOT NULL,
    project_id    uuid NOT NULL,
    epoch_seq     bigint NOT NULL,
    revision_id   uuid NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT statement_timestamp(),
    effective_at  timestamptz NOT NULL,
    state         text NOT NULL DEFAULT 'effective'
                  CHECK (state IN ('effective', 'superseded_before_effect')),
    -- One entry per declared SLI member: the evaluation-semantics projection plus the
    -- resolved staleness deadline. Secret MATERIAL never appears here; credential identity
    -- and generation do.
    snapshot      jsonb NOT NULL DEFAULT '[]',
    snapshot_hash text NOT NULL,
    UNIQUE (service_id, epoch_seq),
    UNIQUE (id, project_id),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (revision_id, project_id)
        REFERENCES service_definition_revisions (id, project_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX service_evaluation_epochs_effective_uniq
    ON service_evaluation_epochs (service_id, effective_at)
    WHERE state = 'effective';

CREATE INDEX service_evaluation_epochs_lookup_idx
    ON service_evaluation_epochs (service_id, effective_at DESC)
    WHERE state = 'effective';

-- ── The facts ───────────────────────────────────────────────────────────────────────────
--
-- Durations are integer MICROseconds, because every boundary derives from a timestamptz and
-- that is its precision: storing milliseconds would need an undocumented rounding rule with
-- a conservation correction inside an error budget.
--
-- The two CHECK constraints are the point of the table. Both axes must account for the
-- whole bucket, and a reducer that silently loses time produces a number nobody can
-- reconcile. excluded_us is shared: declared-out-of-scope time is the same time under
-- either reading.
CREATE TABLE service_reliability_buckets (
    service_id               uuid NOT NULL,
    project_id               uuid NOT NULL,
    epoch_id                 uuid NOT NULL,
    bucket_start             timestamptz NOT NULL,
    bucket_size_us           bigint NOT NULL CHECK (bucket_size_us > 0),

    good_us                  bigint NOT NULL DEFAULT 0 CHECK (good_us >= 0),
    bad_us                   bigint NOT NULL DEFAULT 0 CHECK (bad_us >= 0),
    unknown_us               bigint NOT NULL DEFAULT 0 CHECK (unknown_us >= 0),
    excluded_us              bigint NOT NULL DEFAULT 0 CHECK (excluded_us >= 0),

    healthy_us               bigint NOT NULL DEFAULT 0 CHECK (healthy_us >= 0),
    degraded_us              bigint NOT NULL DEFAULT 0 CHECK (degraded_us >= 0),
    down_us                  bigint NOT NULL DEFAULT 0 CHECK (down_us >= 0),
    health_unknown_us        bigint NOT NULL DEFAULT 0 CHECK (health_unknown_us >= 0),

    state                    text NOT NULL DEFAULT 'provisional'
                             CHECK (state IN ('provisional', 'sealed')),
    sealed_at                timestamptz,
    -- The ingest generation observed under the row lock at seal time. It is what makes the
    -- seal a compare-and-swap rather than a hopeful read.
    sealed_ingest_generation bigint,
    -- The declared-exclusion generation this fact was computed under. Every repair batch
    -- commits only if it is still current, so two batches of one range cannot read two
    -- different "current" maintenance declarations.
    maintenance_generation   bigint NOT NULL DEFAULT 0,
    provenance               jsonb NOT NULL DEFAULT '{}',
    computed_at              timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (service_id, bucket_start),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE,
    -- The epoch reference is DEFERRABLE INITIALLY DEFERRED for the same reason the secret
    -- refs of 00061 are: a service or project cascade removes facts and epochs in an order
    -- Postgres chooses, and a mid-cascade check would fail on a perfectly consistent final
    -- state. Deferring it keeps the guard (an epoch cannot be dropped out from under a
    -- fact) without making deletion order-dependent.
    FOREIGN KEY (epoch_id, project_id) REFERENCES service_evaluation_epochs (id, project_id)
        ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT service_buckets_availability_conserves
        CHECK (good_us + bad_us + unknown_us + excluded_us = bucket_size_us),
    CONSTRAINT service_buckets_health_conserves
        CHECK (healthy_us + degraded_us + down_us + health_unknown_us + excluded_us = bucket_size_us),
    CONSTRAINT service_buckets_sealed_has_stamp
        CHECK ((state = 'sealed') = (sealed_at IS NOT NULL))
) PARTITION BY RANGE (bucket_start);

-- A DEFAULT partition means an insert can never fail for want of a partition, exactly as
-- 00043 does for plain-mode heartbeats. Losing a fact because nobody pre-created a month
-- would be a silent hole in a watermark defined by contiguity.
CREATE TABLE service_reliability_buckets_default
    PARTITION OF service_reliability_buckets DEFAULT;

CREATE INDEX service_reliability_buckets_epoch_idx
    ON service_reliability_buckets (service_id, epoch_id, bucket_start);

-- ── Materialization state, kept apart from the declaration ──────────────────────────────
--
-- sealed_through is defined by CONTIGUITY: the greatest boundary such that every bucket
-- before it exists and is sealed. A materialization hole HOLDS it rather than being jumped
-- over, which is what lets one scalar answer "did we materialize the window" honestly, and
-- what makes a stalled service visible as a lagging timestamp instead of a plausible chart.
CREATE TABLE service_materialization (
    service_id            uuid PRIMARY KEY,
    project_id            uuid NOT NULL,
    materialization_start timestamptz NOT NULL,
    sealed_through        timestamptz,
    -- Set when an audited operation retracts the watermark, so a window that got shorter is
    -- distinguishable from a bug.
    retracted_at          timestamptz,
    retracted_to          timestamptz,
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- ── The seal/ingest handshake ───────────────────────────────────────────────────────────
--
-- Every heartbeat that is ACTUALLY INSERTED upserts this row for each service whose SLI
-- declares its monitor, in the heartbeat's own transaction. The seal upserts and locks the
-- row for every bucket in its range before computing: locking only rows that happen to
-- exist would leave a phantom for a bucket that received no heartbeat, and a concurrent
-- ingest could insert one and commit inside the seal's window.
--
-- Ingest rows carry no history — the FACT's state is the authority — so they are pruned
-- with their buckets once sealed.
CREATE TABLE service_bucket_ingest (
    service_id        uuid NOT NULL,
    project_id        uuid NOT NULL,
    bucket_start      timestamptz NOT NULL,
    ingest_generation bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, bucket_start),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- Late arrivals are AGGREGATED per (service, bucket, monitor), not one row per event: a
-- single historical agent batch landing after a seal, multiplied by the per-monitor service
-- fan-out, would otherwise create millions of retained rows. The unique key makes
-- redelivery idempotent, so a genuinely late row cannot multiply the evidence.
CREATE TABLE service_late_arrivals (
    service_id        uuid NOT NULL,
    project_id        uuid NOT NULL,
    bucket_start      timestamptz NOT NULL,
    monitor_id        uuid NOT NULL,
    arrivals          bigint NOT NULL DEFAULT 1,
    first_received_at timestamptz NOT NULL DEFAULT now(),
    last_received_at  timestamptz NOT NULL DEFAULT now(),
    examples          jsonb NOT NULL DEFAULT '[]',
    overflow          bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, bucket_start, monitor_id),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- ── Durable work: RANGES, not "the current job" ─────────────────────────────────────────
--
-- A newer epoch queues its own disjoint range and never cancels unfinished historical work.
-- Cancelling it would strand buckets no later job is scoped to fill, and the contiguity
-- watermark would stall at that hole permanently. The epoch is resolved per BUCKET, not per
-- range, so a range spanning a boundary evaluates each part under the epoch in force there.
CREATE TABLE service_repair_ranges (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id             uuid NOT NULL,
    project_id             uuid NOT NULL,
    range_start            timestamptz NOT NULL,
    range_end              timestamptz NOT NULL,
    reason                 text NOT NULL
                           CHECK (reason IN ('declaration', 'epoch', 'late_data', 'maintenance', 'admin', 'backfill')),
    state                  text NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending', 'running', 'complete', 'error', 'superseded')),
    cursor_at              timestamptz,
    maintenance_generation bigint NOT NULL DEFAULT 0,
    attempts               integer NOT NULL DEFAULT 0,
    next_attempt_at        timestamptz NOT NULL DEFAULT now(),
    last_error             text NOT NULL DEFAULT '',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CHECK (range_end > range_start),
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

CREATE INDEX service_repair_ranges_claim_idx
    ON service_repair_ranges (next_attempt_at, service_id)
    WHERE state IN ('pending', 'running');

-- ── Maintenance becomes a retroactive DECLARATION ───────────────────────────────────────
--
-- archived_at hides a window from active inventory and from future applicability.
-- cancel_effective_at truncates an active window prospectively, at the EXACT statement time
-- of the cancelling transaction — the reducer handles arbitrary edges, so rounding to a
-- bucket boundary would silently extend or shorten a real exclusion by up to a bucket.
--
-- Neither removes the window's effect on already-sealed time: the evaluator reads a
-- retained row over [starts_at, min(ends_at, cancel_effective_at)) REGARDLESS of
-- archived_at. Only an explicit annul does that, which is why annul — and not the ordinary
-- delete — carries the preview, the audit and the raw-availability fence.
ALTER TABLE maintenance_windows
    ADD COLUMN archived_at         timestamptz,
    ADD COLUMN cancel_effective_at timestamptz;

CREATE INDEX maintenance_windows_active_idx
    ON maintenance_windows (project_id, starts_at, ends_at)
    WHERE archived_at IS NULL;

-- Bumped in the same transaction as any maintenance mutation. Every repair batch records
-- the value it read and commits only if it is still current.
CREATE TABLE project_maintenance_generation (
    project_id uuid PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    generation bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ── Preview / confirm for a retroactive mutation ────────────────────────────────────────
--
-- The affected services live in a COMPLETE relation, never a bounded array: re-reading the
-- generations of the services already known proves those rows did not move, and proves
-- nothing about the set. A truncated array would let a confirm pass while a service it
-- never checked is mutated.
CREATE TABLE maintenance_previews (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id             uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    requested_start        timestamptz NOT NULL,
    requested_end          timestamptz NOT NULL,
    maintenance_generation bigint NOT NULL,
    -- The earliest instant still recomputable when the preview ran. It is CAS'd as a
    -- monotonic predicate ("the requested start is still >= the current floor"), never for
    -- equality: the floor advances continuously with retention, so equality would make
    -- every token stale by construction.
    raw_floor              timestamptz NOT NULL,
    coverage               text NOT NULL CHECK (coverage IN ('complete', 'approximate')),
    computed_at            timestamptz NOT NULL DEFAULT now(),
    expires_at             timestamptz NOT NULL,
    created_by             text NOT NULL DEFAULT '',
    UNIQUE (id, project_id),
    CHECK (requested_end > requested_start)
);

CREATE TABLE maintenance_preview_services (
    preview_id            uuid NOT NULL,
    project_id            uuid NOT NULL,
    service_id            uuid NOT NULL,
    definition_generation bigint NOT NULL,
    before_good_us        bigint NOT NULL DEFAULT 0,
    before_bad_us         bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (preview_id, service_id),
    FOREIGN KEY (preview_id, project_id) REFERENCES maintenance_previews (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- +goose StatementEnd

-- Monthly partitions around today, plus the DEFAULT above. Guarded and idempotent so a
-- re-run is a no-op, per the 00043 pattern.
-- +goose StatementBegin
DO $$
DECLARE
    m date := date_trunc('month', now() - interval '3 months')::date;
    stop date := date_trunc('month', now() + interval '6 months')::date;
BEGIN
    WHILE m < stop LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS service_reliability_buckets_%s PARTITION OF service_reliability_buckets FOR VALUES FROM (%L) TO (%L)',
            to_char(m, 'YYYYMM'), m, (m + interval '1 month')::date);
        m := (m + interval '1 month')::date;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS maintenance_preview_services;
DROP TABLE IF EXISTS maintenance_previews;
DROP TABLE IF EXISTS project_maintenance_generation;
DROP INDEX IF EXISTS maintenance_windows_active_idx;
ALTER TABLE maintenance_windows
    DROP COLUMN IF EXISTS cancel_effective_at,
    DROP COLUMN IF EXISTS archived_at;
DROP TABLE IF EXISTS service_repair_ranges;
DROP TABLE IF EXISTS service_late_arrivals;
DROP TABLE IF EXISTS service_bucket_ingest;
DROP TABLE IF EXISTS service_materialization;
DROP TABLE IF EXISTS service_reliability_buckets;
DROP TABLE IF EXISTS service_evaluation_epochs;
DROP TABLE IF EXISTS service_member_refs;
DROP TABLE IF EXISTS service_definition_members;
DROP TABLE IF EXISTS service_definition_revisions;
DROP TABLE IF EXISTS services;
-- +goose StatementEnd
