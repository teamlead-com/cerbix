-- +goose Up
-- +goose StatementBegin

-- FR-024 — the reliability gate (func-reliability-gate §5, D9, D10, D13a; iter-0163).
--
-- Four tables and one function. A POLICY per service says which reliability clauses block a
-- release, warn or are ignored; an OVERRIDE lets a privileged operator lift a BLOCK for a
-- bounded time; every decision is one immutable row in a daily-partitioned LEDGER; and a
-- REGISTRY records which ledger partitions are ours, so maintenance validates ownership
-- against a marker rather than trusting a name. Nothing here is a hypertable and nothing
-- depends on the storage mode: the ledger is a plain RANGE-partitioned table in both.

-- ── gate_uuid_ms: the first 48 bits of a UUIDv7, as milliseconds ─────────────────────────
--
-- A decision id is a UUIDv7 whose 48-bit timestamp is the millisecond of `evaluated_at`, the
-- DB clock the row is partitioned by. The binding is enforced by the DATABASE through the
-- CHECK below, not trusted to the writer, so an id's day and its row's partition cannot
-- disagree and a duplicate id across days is structurally impossible. The function reads the
-- twelve leading hex digits of the canonical text form; bit(48) → bigint is a zero-extended
-- cast, so the full unsigned range (0 … 281 474 976 710 655) comes back positive.
CREATE FUNCTION gate_uuid_ms(u uuid) RETURNS bigint
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN ('x' || substr(replace(u::text, '-', ''), 1, 12))::bit(48)::bigint;

COMMENT ON FUNCTION gate_uuid_ms(uuid) IS
    'cerbix: the first 48 bits of a UUIDv7 as an unsigned millisecond count (func-reliability-gate §5)';

-- ── The policy: one per service, a generation counter that is never reused ───────────────
--
-- `revision` is DB-owned and MONOTONIC per service. The row is never deleted: a DELETE sets
-- deleted_at and bumps revision in the same statement, and a re-create is an UPDATE that
-- clears deleted_at with revision + 1 (D13a). Without that, a delete-and-recreate would hand
-- out revision 1 twice and a stale screen holding the old 1 could PUT over — or POST an
-- override against — a different policy. The store owns those statements; the schema allows
-- them: `deleted_at` is the tombstone.
--
-- The window column is `window_name`, as `sla_targets` already spells it (00077): `window`
-- is a reserved word in PostgreSQL. The API field stays `window`.
--
-- `max_seal_lag_seconds` carries the DERIVED floor of D8a: domain.MinSealLag =
-- LateArrivalGrace + CanonicalBucket + 2 × CanonicalBucket = 300 s. The domain test asserts
-- the formula; this CHECK is the same number where the data lives.
CREATE TABLE service_gate_policies (
    service_id              uuid PRIMARY KEY,
    project_id              uuid NOT NULL,
    window_name             text NOT NULL,
    schema_version          integer NOT NULL,
    clauses                 jsonb NOT NULL,
    budget_consumed_percent integer NOT NULL,
    max_seal_lag_seconds    integer NOT NULL,
    unknown_behavior        text NOT NULL,
    revision                bigint NOT NULL,
    deleted_at              timestamptz,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- The writer's immutable server-derived label (the principal's AuditLabel), the spelling
    -- every service-scoped record keeps (service_definition_revisions.created_by).
    updated_by              text NOT NULL DEFAULT '',
    CONSTRAINT service_gate_policies_window_chk
        CHECK (window_name <> ''),
    CONSTRAINT service_gate_policies_schema_version_chk
        CHECK (schema_version >= 1),
    CONSTRAINT service_gate_policies_clauses_chk
        CHECK (jsonb_typeof(clauses) = 'object'),
    CONSTRAINT service_gate_policies_budget_consumed_percent_chk
        CHECK (budget_consumed_percent BETWEEN 1 AND 100),
    CONSTRAINT service_gate_policies_max_seal_lag_chk
        CHECK (max_seal_lag_seconds BETWEEN 300 AND 86400 AND max_seal_lag_seconds % 60 = 0),
    CONSTRAINT service_gate_policies_unknown_behavior_chk
        CHECK (unknown_behavior IN ('warn', 'block')),
    CONSTRAINT service_gate_policies_revision_chk
        CHECK (revision >= 1),
    -- Tenant-composite, as every service-scoped table since 00085: a policy cannot name a
    -- service of another project, and it goes with its service.
    CONSTRAINT service_gate_policies_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- ── The override: both triples, a bounded reason, an expiry, and a readable history ──────
--
-- The actor and the revoker are each stored as the COMPLETE typed triple — a nullable user
-- id, a via-token flag and an immutable server-derived label — because for a machine
-- principal the typed half is `NULL + true`, which after commit reads as "some token", and
-- the evidence has to name which one for as long as the row exists (D9, invariant 17). The
-- user references follow audit_logs (00018): ON DELETE SET NULL keeps the row and the label
-- keeps the name.
--
-- Every closure — human or system — sets revoked_at AND revoked_reason; only a `manual`
-- closure carries attribution, and the three attribution fields are null for `expired`,
-- `policy_changed` and `policy_deleted` (D13a). The two CHECKs make that true of the data.
--
-- At most ONE row per service with revoked_at IS NULL — but that is enforced by the store
-- under the service lock, which first closes an unrevoked row whose expires_at has passed
-- (revoked_reason: expired) and then inserts: a partial unique index cannot consult now(),
-- and one that ignored expiry would let a single expired override refuse every later one
-- forever (D9).
CREATE TABLE service_gate_overrides (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id         uuid NOT NULL,
    project_id         uuid NOT NULL,
    -- The policy revision the caller saw. Not a foreign key: revision is a generation, not a
    -- key, and the override must stay readable as history after the policy moves on.
    policy_revision    bigint NOT NULL,
    actor_user_id      uuid REFERENCES users (id) ON DELETE SET NULL,
    via_token          boolean NOT NULL,
    actor_label        text NOT NULL,
    reason             text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    revoked_at         timestamptz,
    revoked_reason     text,
    revoked_by_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    revoked_via_token  boolean,
    revoked_by_label   text,
    CONSTRAINT service_gate_overrides_actor_label_chk
        CHECK (actor_label <> ''),
    CONSTRAINT service_gate_overrides_reason_chk
        CHECK (char_length(reason) BETWEEN 1 AND 500),
    CONSTRAINT service_gate_overrides_revoked_reason_chk
        CHECK (revoked_reason IN ('manual', 'expired', 'policy_changed', 'policy_deleted')),
    CONSTRAINT service_gate_overrides_close_chk
        CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT service_gate_overrides_revoker_chk
        CHECK (
            (revoked_reason = 'manual'
                AND revoked_via_token IS NOT NULL
                AND revoked_by_label IS NOT NULL AND revoked_by_label <> '')
            OR (revoked_reason IS DISTINCT FROM 'manual'
                AND revoked_by_user_id IS NULL
                AND revoked_via_token IS NULL
                AND revoked_by_label IS NULL)),
    CONSTRAINT service_gate_overrides_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- The history list's order, so a hot service never sorts its history and equal timestamps
-- order deterministically (D13a: the newest 50 by created_at DESC, id DESC).
CREATE INDEX service_gate_overrides_history_idx
    ON service_gate_overrides (service_id, created_at DESC, id DESC);

-- ── The ledger: append-only, daily RANGE partitions, NO default partition ─────────────────
--
-- A decision row is an immutable snapshot: it stores the service slug and name, the policy
-- clauses and revision, and the full evidence at the time, so it remains readable after the
-- service is renamed and survives its deletion — `service_id` nullable with the column-list
-- SET NULL that service_alert_episodes (00082) uses, so the DB clears ONLY service_id and the
-- tenant key stays. It is cascaded with its project.
--
-- PostgreSQL enforces a primary key on a RANGE-partitioned table only when the partition key
-- is part of it, so the key is (evaluated_at, id); within a day, each partition carries a
-- LOCAL UNIQUE (id) created on the standalone table before attach (a partitioned unique index
-- would have to include the partition key; a per-child one need not). There is deliberately
-- NO parent (id) index: day pruning plus the per-partition unique make it redundant, so a
-- child carries exactly FOUR indexes — the PK, the local unique, the two listing paths.
--
-- NO DEFAULT partition (unlike heartbeats): the row is written in the decision transaction,
-- so a decision the ledger cannot hold is not a decision — the insert fails with SQLSTATE
-- 23514 and the API answers 503 ledger_unwritable (D10, invariant 19).
--
-- `override_id` is not a foreign key: overrides cascade with their service and the decision
-- outlives it, and "present exactly when an override was applied" must stay true afterwards.
CREATE TABLE service_gate_decisions (
    id              uuid NOT NULL,
    project_id      uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    service_id      uuid,
    service_slug    text NOT NULL,
    service_name    text NOT NULL,
    state           text NOT NULL,
    action          text,
    reasons         jsonb NOT NULL,
    evidence        jsonb NOT NULL,
    policy_revision bigint,
    window_name     text,
    policy_snapshot jsonb,
    override_id     uuid,
    evaluated_at    timestamptz NOT NULL,
    sealed_through  timestamptz,
    PRIMARY KEY (evaluated_at, id),
    CONSTRAINT service_gate_decisions_state_chk
        CHECK (state IN ('ALLOW', 'WARN', 'BLOCK', 'UNKNOWN', 'NOT_CONFIGURED')),
    CONSTRAINT service_gate_decisions_action_chk
        CHECK (action IN ('ALLOW', 'WARN', 'BLOCK')),
    -- The D7 presence table as a fact of the data: NOT_CONFIGURED has no action and no policy
    -- fields; every other state has all of them.
    CONSTRAINT service_gate_decisions_policy_presence_chk
        CHECK ((state = 'NOT_CONFIGURED') = (action IS NULL)
           AND (state = 'NOT_CONFIGURED') = (policy_revision IS NULL)
           AND (state = 'NOT_CONFIGURED') = (window_name IS NULL)
           AND (state = 'NOT_CONFIGURED') = (policy_snapshot IS NULL)),
    CONSTRAINT service_gate_decisions_reasons_chk
        CHECK (jsonb_typeof(reasons) = 'array'),
    CONSTRAINT service_gate_decisions_evidence_chk
        CHECK (jsonb_typeof(evidence) = 'object'),
    -- The id is bound to the row: its embedded millisecond IS evaluated_at's millisecond.
    -- extract(epoch …) is numeric in PostgreSQL 14+, so floor(… * 1000) is exact.
    CONSTRAINT service_gate_decisions_id_binds_evaluated_at_chk
        CHECK (gate_uuid_ms(id) = floor(extract(epoch FROM evaluated_at) * 1000)),
    -- Bytes, not only rows (§5a): evidence ≤ 4 KiB, reasons ≤ 1 KiB, policy snapshot ≤ 4 KiB.
    -- The writer never truncates; a row that would exceed this is a bug and fails.
    CONSTRAINT service_gate_decisions_payload_chk
        CHECK (octet_length(evidence::text) <= 4096
           AND octet_length(reasons::text) <= 1024
           AND (policy_snapshot IS NULL OR octet_length(policy_snapshot::text) <= 4096)),
    CONSTRAINT service_gate_decisions_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id)
        ON DELETE SET NULL (service_id)
) PARTITION BY RANGE (evaluated_at);

-- The listing's keyset path and the per-service listing path (§5). Partitioned indexes: each
-- child gets its own, and a standalone child built with LIKE … INCLUDING INDEXES has them
-- already, so ATTACH matches rather than builds.
CREATE INDEX service_gate_decisions_project_idx
    ON service_gate_decisions (project_id, evaluated_at DESC, id DESC);
CREATE INDEX service_gate_decisions_project_service_idx
    ON service_gate_decisions (project_id, service_id, evaluated_at DESC, id DESC);

-- ── The registry: ownership is the marker, not the OID ───────────────────────────────────
--
-- `IF NOT EXISTS` is not idempotence — PostgreSQL does not guarantee an existing relation
-- resembles the one that would have been created — and a standalone table left by a crash
-- has no parent link, so a name alone proves nothing. Every partition has a row here: the
-- deterministic relname, an owner_token minted at creation and written as the table's
-- COMMENT ('cerbix:gate-ledger:<token>'), `relid` as a runtime LOCATOR refreshed when the
-- marker matches a new OID (pg_dump/restore), and a state. Every transition PostgreSQL allows
-- inside a transaction commits with its registry write; only DETACH … CONCURRENTLY runs
-- behind a committed `detaching` intent (D10).
CREATE TABLE service_gate_decision_partitions (
    day         date PRIMARY KEY,
    relname     text NOT NULL UNIQUE,
    owner_token uuid NOT NULL UNIQUE,
    relid       oid NOT NULL,
    state       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    attached_at timestamptz,
    detached_at timestamptz,
    dropped_at  timestamptz,
    CONSTRAINT service_gate_decision_partitions_state_chk
        CHECK (state IN ('created', 'attached', 'detaching', 'detached', 'dropped'))
);

-- +goose StatementEnd

-- ── Bootstrap: [today, today + 7 d] built standalone, marked, registered, attached ────────
--
-- Each day is built exactly the way the maintenance pass builds one (D10): CREATE TABLE …
-- (LIKE parent INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES) plus a bounds
-- CHECK so the attach needs no scan, the LOCAL UNIQUE (id), the COMMENT marker, the registry
-- row as `created`, then ATTACH PARTITION and `attached`. UTC days with explicit +00 bounds,
-- independent of the session TimeZone (the 00064 lesson). Idempotent: a day already in the
-- registry is skipped, and says so, so a re-run over a half-built registry converges.
-- +goose StatementBegin
-- gate-ledger-bootstrap:begin
DO $$
DECLARE
    today date := (now() AT TIME ZONE 'UTC')::date;
    d     date;
    rel   text;
    tok   uuid;
    rid   oid;
    lo    text;
    hi    text;
BEGIN
    d := today;
    WHILE d <= today + 7 LOOP
        rel := 'service_gate_decisions_p' || to_char(d, 'YYYYMMDD');
        IF EXISTS (SELECT 1 FROM service_gate_decision_partitions WHERE day = d) THEN
            RAISE NOTICE 'gate ledger: % already registered — skipped', rel;
            d := d + 1;
            CONTINUE;
        END IF;
        lo := to_char(d, 'YYYY-MM-DD') || ' 00:00:00+00';
        hi := to_char(d + 1, 'YYYY-MM-DD') || ' 00:00:00+00';
        tok := gen_random_uuid();

        EXECUTE format(
            'CREATE TABLE %I (LIKE service_gate_decisions INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES, '
            || 'CONSTRAINT %I CHECK (evaluated_at >= %L::timestamptz AND evaluated_at < %L::timestamptz))',
            rel, rel || '_day_chk', lo, hi);
        EXECUTE format('CREATE UNIQUE INDEX %I ON %I (id)', rel || '_id_uniq', rel);
        EXECUTE format('COMMENT ON TABLE %I IS %L', rel, 'cerbix:gate-ledger:' || tok::text);
        rid := to_regclass(rel)::oid;
        INSERT INTO service_gate_decision_partitions (day, relname, owner_token, relid, state, created_at)
            VALUES (d, rel, tok, rid, 'created', now());

        EXECUTE format('ALTER TABLE service_gate_decisions ATTACH PARTITION %I FOR VALUES FROM (%L) TO (%L)',
            rel, lo, hi);
        UPDATE service_gate_decision_partitions
           SET state = 'attached', attached_at = now()
         WHERE day = d;
        RAISE NOTICE 'gate ledger: % created and attached for %', rel, d;
        d := d + 1;
    END LOOP;
END $$;
-- gate-ledger-bootstrap:end
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE r record;
BEGIN
    -- Detached partitions are standalone tables the parent's DROP would not reach.
    FOR r IN SELECT relname FROM service_gate_decision_partitions WHERE state <> 'dropped' LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I', r.relname);
    END LOOP;
END $$;
DROP TABLE IF EXISTS service_gate_decision_partitions;
DROP TABLE IF EXISTS service_gate_decisions;
DROP TABLE IF EXISTS service_gate_overrides;
DROP TABLE IF EXISTS service_gate_policies;
DROP FUNCTION IF EXISTS gate_uuid_ms(uuid);
-- +goose StatementEnd
