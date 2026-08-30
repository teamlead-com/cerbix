-- +goose Up

-- FR-025 — change intelligence (func-change-intelligence §5, D2, D3, D7, D9, D10, D12; iter-0165).
--
-- Two tables and one column. A CHANGE is an event — at instant T a change of kind K in phase P
-- happened to service S under the external identity (source, external_id) — never a catalog
-- entry (D1): no repository, owner, environment or artefact, only the bounded opaque `ref` and
-- `url`. Phases are append-only rows keyed UNIQUE (service_id, source, external_id, phase); the
-- phase ORDER is the domain's, the per-identity serialization is a transaction-scoped advisory
-- lock in the store (D3, D4). A LINK row ties an incident to the change that preceded it, with
-- the anchored phase's instant and the lag copied and never updated (D7). Nothing here is a
-- hypertable and nothing depends on the storage mode.

-- ── incidents UNIQUE (id, project_id): the FK target, added only if absent — FIRST ───────
--
-- 00080 already added `incidents_id_project_key` for the impact relation, so on every database
-- that ran it this block is a no-op that says so. The catalog is consulted for the KEY SHAPE,
-- not for a constraint name, so a hand-built database with the same key under another name is
-- also left alone. The Down block removes the constraint only when THIS migration added it.
-- It runs BEFORE the tables because incident_changes' foreign key needs the key to exist.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM pg_constraint c
         WHERE c.conrelid = 'incidents'::regclass
           AND c.contype IN ('u', 'p')
           AND (SELECT array_agg(a.attname::text ORDER BY a.attname)
                  FROM unnest(c.conkey) AS k(attnum)
                  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum)
               = ARRAY['id', 'project_id']
    ) THEN
        RAISE NOTICE 'change intelligence: incidents already carries UNIQUE (id, project_id) — nothing added';
    ELSE
        ALTER TABLE incidents ADD CONSTRAINT incidents_id_project_change_key UNIQUE (id, project_id);
        RAISE NOTICE 'change intelligence: incidents_id_project_change_key UNIQUE (id, project_id) added';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- ── service_changes: one row per phase ───────────────────────────────────────────────────
--
-- `id` is a UUIDv7 built from the database instant the row was recorded at (the store reads
-- statement_timestamp() inside the write transaction and derives the id from it, exactly as
-- the gate's decision ids are built — §5 "v7 from the DB instant"). `kind` is stored PER ROW so
-- a replay with a different kind is a detectable 409; the store refuses a phase whose kind
-- differs from the group's (409 kind_mismatch), so a group has one kind in practice.
--
-- The text CHECKs enforce what a CHECK can — length in characters and the absence of ASCII
-- control characters [\x00-\x1F\x7F] — as a last line against direct SQL. They do NOT enforce
-- Unicode Cf, NFC or the U+2028/U+2029 line separators, and claim no more than that: the
-- domain validator (domain.ValidateChangeText) is the single Unicode authority, and the store
-- writes only through the one function that calls it (D2, invariant 23). `source` is a slug by
-- its class; `external_id` is case-sensitive (D2, owner question 7).
--
-- `decision_id` is validated on write against the gate's ledger and stored WITHOUT a foreign
-- key: the ledger's daily partitions are dropped by age (FR-024 D10), and the timeline says
-- "aged out" when the row is gone (D11).
--
-- The actor is server-derived and stored twice (D5): the immutable label plus the typed pair.
-- Recording is not an audit event — the row IS the record.
CREATE TABLE service_changes (
    id            uuid PRIMARY KEY,
    project_id    uuid NOT NULL,
    service_id    uuid NOT NULL,
    source        text NOT NULL,
    external_id   text NOT NULL,
    kind          text NOT NULL,
    phase         text NOT NULL,
    ref           text NOT NULL DEFAULT '',
    url           text NOT NULL DEFAULT '',
    occurred_at   timestamptz NOT NULL,
    decision_id   uuid,
    actor_label   text NOT NULL,
    actor_user_id uuid,
    via_token     boolean NOT NULL,
    recorded_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT service_changes_source_chk
        CHECK (source ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    CONSTRAINT service_changes_external_id_chk
        CHECK (char_length(external_id) BETWEEN 1 AND 128 AND external_id !~ '[\x00-\x1F\x7F]'),
    CONSTRAINT service_changes_kind_chk
        CHECK (kind IN ('deploy', 'rollback', 'flag')),
    CONSTRAINT service_changes_phase_chk
        CHECK (phase IN ('started', 'succeeded', 'failed', 'cancelled')),
    CONSTRAINT service_changes_ref_chk
        CHECK (char_length(ref) <= 128 AND ref !~ '[\x00-\x1F\x7F]'),
    CONSTRAINT service_changes_url_chk
        CHECK (char_length(url) <= 512 AND (url = '' OR url LIKE 'https://%') AND url !~ '[\x00-\x1F\x7F]'),
    -- The append-only phase key (D3): a phase row is never updated or deleted by any route.
    CONSTRAINT service_changes_identity_phase_key
        UNIQUE (service_id, source, external_id, phase),
    -- The target of incident_changes' composite foreign key (D7, invariant 22).
    CONSTRAINT service_changes_id_project_key
        UNIQUE (id, project_id),
    -- Tenant-composite, and CASCADE: a change is the service's fact and goes with it (D10 —
    -- deliberately NOT the ledger's outlive rule; the incident note remains as text).
    CONSTRAINT service_changes_service_fkey
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
);

-- The timeline's grouped subquery (D6) and the correlation's window read (D7).
CREATE INDEX service_changes_service_occurred_idx
    ON service_changes (service_id, occurred_at DESC, id DESC);
-- Retention by group age (D9).
CREATE INDEX service_changes_project_occurred_idx
    ON service_changes (project_id, occurred_at);

-- ── incident_changes: the link table, tenant integrity the database's ─────────────────────
--
-- One row per (incident, change): `change_id` is the group's latest phase KNOWN at the
-- incident's `opened` delivery; its `occurred_at` and the lag are copied here and never
-- updated — a terminal phase recorded after the open rewrites neither the link nor the note
-- (D7). BOTH references are composite through the shared `project_id`, so a cross-project link
-- cannot be inserted by any path, direct SQL included (review P0-1, invariant 22). The links
-- cascade with either endpoint.
CREATE TABLE incident_changes (
    incident_id uuid NOT NULL,
    change_id   uuid NOT NULL,
    project_id  uuid NOT NULL,
    role        text NOT NULL,
    occurred_at timestamptz NOT NULL,
    lag_seconds integer NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (incident_id, change_id),
    CONSTRAINT incident_changes_role_chk
        CHECK (role IN ('own_service', 'upstream')),
    CONSTRAINT incident_changes_lag_chk
        CHECK (lag_seconds >= 0),
    CONSTRAINT incident_changes_incident_fkey
        FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE,
    CONSTRAINT incident_changes_change_fkey
        FOREIGN KEY (change_id, project_id) REFERENCES service_changes (id, project_id) ON DELETE CASCADE
);

-- The change side of the symmetric read (a group's incidents[]); the incident side is the PK.
CREATE INDEX incident_changes_change_idx
    ON incident_changes (change_id);

-- ── api_tokens.actions: the per-token allow-list (D12) ────────────────────────────────────
--
-- NULL means what it means today — the token's role decides. A non-null list is an ALLOW-LIST
-- intersected with the role inside authz.Can and nowhere else; it is validated against the
-- central action catalogue on create and is immutable after (a different list is a new token).
ALTER TABLE api_tokens ADD COLUMN actions text[];

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS incident_changes;
DROP TABLE IF EXISTS service_changes;
ALTER TABLE api_tokens DROP COLUMN IF EXISTS actions;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'incidents_id_project_change_key') THEN
        ALTER TABLE incidents DROP CONSTRAINT incidents_id_project_change_key;
    END IF;
END $$;
-- +goose StatementEnd
