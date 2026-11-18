-- FR-041a (func-phone-alerts §8, rows marked (a)): the schema of the SMS and voice channels, and a
-- short reference on every incident for the phone (D21).
--
--   instance_settings     the telephony group (R1): a jsonb group plus the typed columns the SQL
--                         predicates read; every write sets them in one statement (the third,
--                         telephony_can_place_calls, is generated from telephony_provider and the
--                         `telephony` jsonb's twilio.from)
--   user_phones           a person's own number, encrypted, with a keyed hash for lookups (R2)
--   phone_verifications   short-lived code rows, kept for the send limits, purged by the scheduler
--   notification_channels typed phone columns (NOT keys of the config map, §4.3) and the type check
--                         widened with sms and voice (the 00013 pattern)
--   phone_deliveries      one row per phone delivery attempt, the trace of §4.5
--   incidents.ref         eight Crockford base32 characters from a database DEFAULT (D21, D27)
--
-- Nothing else of parts (b) and (c) is here: no ACK or snooze column, no fallback, no quota, no
-- provider callback column. The one exception is user_phones.quiet_hours, created because §8's (a)
-- row lists it, and unused until FR-041c (R8). `heartbeats` is untouched: every table below is an ordinary
-- table in both storage modes.
--
-- The whole file runs in goose's transaction. That matters for `incidents.ref`: ALTER TABLE holds the
-- table's ACCESS EXCLUSIVE lock until commit, so no insert by a running older binary can land between
-- the backfill and SET NOT NULL — it waits, and then takes the DEFAULT.

-- +goose Up
-- gen_random_bytes. 00053 already created the extension; naming it here keeps the dependency where
-- it is used. pgcrypto ships with PostgreSQL itself, so it is present in both storage modes.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- R1. The provider is a closed set; the secret fields live encrypted inside `telephony` (D29).
ALTER TABLE instance_settings ADD COLUMN telephony jsonb NOT NULL DEFAULT '{}';
ALTER TABLE instance_settings ADD COLUMN telephony_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE instance_settings ADD COLUMN telephony_provider text NULL;
ALTER TABLE instance_settings ADD CONSTRAINT instance_settings_telephony_provider_chk
    CHECK (telephony_provider IN ('twilio', 'generic_http'));
-- D3 (revision 8): "the provider can place calls", the ONE condition every voice rule reads (channel
-- creation and enabling, `deliverable`, the route predicates, the 409 of a settings save, the send).
-- It is false for generic_http and for twilio whose `from` is a Messaging Service SID (`MG…`, which
-- sends messages only). Generated from the same row, so no write can leave it disagreeing with the
-- provider and `from` it describes, and a save's own transaction reads the PROPOSED value.
ALTER TABLE instance_settings ADD COLUMN telephony_can_place_calls boolean NOT NULL
    GENERATED ALWAYS AS (
        telephony_provider IS DISTINCT FROM 'generic_http'
        AND NOT (telephony_provider IS NOT DISTINCT FROM 'twilio'
                 AND COALESCE(telephony #>> '{twilio,from}', '') LIKE 'MG%')
    ) STORED;

-- A number belongs to one person: the keyed hash is unique across users. quiet_hours is
-- listed by §8's (a) row; nothing reads or writes it until FR-041c.
CREATE TABLE user_phones (
    user_id       uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    number_enc    text        NOT NULL,
    number_hash   text        NOT NULL,
    number_masked text        NOT NULL,
    verified_at   timestamptz NULL,
    language      text        NULL,
    quiet_hours   jsonb       NULL,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_phones_number_hash_key UNIQUE (number_hash)
);

-- The subject is a user (their own phone) or a channel (a `number` channel); it has no foreign key
-- because it points at one of two tables. `requested_by` is the principal who asked for the code (a
-- user id, or an API token's "apitoken:<id>"): the hourly limit is counted on it, across every number
-- it verifies (FR-041a review P2-3). The subject index serves the latest-code lookup; the other two
-- are the two send limits (R6).
CREATE TABLE phone_verifications (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind text        NOT NULL,
    subject_id   uuid        NOT NULL,
    requested_by text        NOT NULL,
    number_hash  text        NOT NULL,
    code_hash    text        NOT NULL,
    attempts     int         NOT NULL DEFAULT 0,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT phone_verifications_subject_kind_chk CHECK (subject_kind IN ('user', 'channel'))
);
CREATE INDEX phone_verifications_subject_idx ON phone_verifications (subject_kind, subject_id, created_at);
CREATE INDEX phone_verifications_requester_idx ON phone_verifications (requested_by, created_at);
CREATE INDEX phone_verifications_number_idx ON phone_verifications (number_hash, created_at);

-- The phone columns of a channel. `recipient_user_id` is SET NULL on a user's deletion: the channel
-- survives, disabled with `recipient_deleted` by the deleting transaction first (§4.3). The CHECK below
-- therefore does NOT require a user in `user` mode — a deletion by an older binary, which does not
-- disable the channel first, must still succeed.
ALTER TABLE notification_channels ADD COLUMN recipient text NULL;
ALTER TABLE notification_channels ADD COLUMN recipient_user_id uuid NULL REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE notification_channels ADD COLUMN number_enc text NULL;
ALTER TABLE notification_channels ADD COLUMN number_hash text NULL;
ALTER TABLE notification_channels ADD COLUMN number_masked text NULL;
ALTER TABLE notification_channels ADD COLUMN number_verified_at timestamptz NULL;
ALTER TABLE notification_channels ADD COLUMN disabled_reason text NULL;

ALTER TABLE notification_channels DROP CONSTRAINT notification_channels_type_chk;
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_type_chk
    CHECK (type IN ('webhook', 'slack', 'telegram', 'email', 'sms', 'voice'));

ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_disabled_reason_chk
    CHECK (disabled_reason IN ('recipient_deleted', 'number_unverified'));

-- The four existing types carry none of the phone columns.
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_phone_columns_chk
    CHECK (type IN ('sms', 'voice') OR (
        recipient IS NULL AND recipient_user_id IS NULL AND number_enc IS NULL AND number_hash IS NULL
        AND number_masked IS NULL AND number_verified_at IS NULL AND disabled_reason IS NULL));

-- A phone channel states its recipient, and the mode decides which columns may be set. COALESCE
-- makes a NULL `recipient` (CASE falls to ELSE) a refusal rather than a pass.
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_phone_recipient_chk
    CHECK (type NOT IN ('sms', 'voice') OR COALESCE(
        CASE recipient
            WHEN 'number' THEN number_enc IS NOT NULL AND number_hash IS NOT NULL
                               AND number_masked IS NOT NULL AND recipient_user_id IS NULL
            WHEN 'user'   THEN number_enc IS NULL AND number_hash IS NULL
                               AND number_masked IS NULL AND number_verified_at IS NULL
            ELSE false
        END, false));

CREATE INDEX notification_channels_recipient_user_idx ON notification_channels (recipient_user_id)
    WHERE recipient_user_id IS NOT NULL;

-- The delivery record (§4.5). `outbox_event_id` is a plain uuid, not a foreign key (D33): outbox rows
-- are purged long before the 90-day trace. `number_hash`/`number_masked` are NULL for a
-- `not_deliverable` row of a user with no verified number (R9). The provider code keeps the shape D34
-- allows, so response text cannot be stored here even by mistake. `service_state` and
-- `escalation_step` say what the alert announced (revision 7, §4.5): the paging state of a service's
-- health onset, in the five-value vocabulary of service_alert_state, and the 1-based escalation step
-- that handed it over; NULL for every other alert.
CREATE TABLE phone_deliveries (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    channel_id      uuid        NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    outbox_event_id uuid        NULL,
    incident_id     uuid        NULL REFERENCES incidents (id) ON DELETE SET NULL,
    monitor_id      uuid        NULL,
    monitor_seq     bigint      NULL,
    service_id      uuid        NULL,
    user_id         uuid        NULL,
    number_hash     text        NULL,
    number_masked   text        NULL,
    kind            text        NOT NULL,
    alert_kind      text        NOT NULL,
    reason          text        NOT NULL,
    notes           text[]      NOT NULL DEFAULT '{}',
    provider_status int         NULL,
    provider_code   text        NULL,
    provider_ref    text        NULL,
    segments        int         NOT NULL DEFAULT 0,
    attempts        int         NOT NULL DEFAULT 1,
    service_state   text        NULL,
    escalation_step int         NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT phone_deliveries_kind_chk CHECK (kind IN ('sms', 'voice')),
    CONSTRAINT phone_deliveries_reason_chk CHECK (reason IN (
        'sending', 'sent', 'provider_unreachable', 'provider_rejected', 'outcome_unknown',
        'not_deliverable', 'voice_unsupported_by_provider', 'telephony_disabled', 'storm_limited')),
    CONSTRAINT phone_deliveries_provider_code_chk CHECK (provider_code ~ '^[A-Za-z0-9_.-]{1,32}$'),
    CONSTRAINT phone_deliveries_counts_chk CHECK (segments >= 0 AND attempts >= 1),
    CONSTRAINT phone_deliveries_service_state_chk CHECK (service_state IN ('healthy', 'degraded', 'down', 'unknown', 'excluded')),
    CONSTRAINT phone_deliveries_escalation_step_chk CHECK (escalation_step >= 1)
);
-- A redelivered event cannot phone a channel twice (D33).
CREATE UNIQUE INDEX phone_deliveries_event_channel_key ON phone_deliveries (outbox_event_id, channel_id)
    WHERE outbox_event_id IS NOT NULL;
-- The storm guard (§6.2).
CREATE INDEX phone_deliveries_number_idx ON phone_deliveries (number_hash, created_at);
CREATE INDEX phone_deliveries_incident_idx ON phone_deliveries (incident_id);
CREATE INDEX phone_deliveries_channel_idx ON phone_deliveries (channel_id, created_at DESC);
-- A project's (and an organization's) cascade finds its rows without a scan of the table (FR-041a
-- review P1-3).
CREATE INDEX phone_deliveries_project_idx ON phone_deliveries (project_id);
-- A monitor alert sent before its incident existed, found again when the incident is known.
CREATE INDEX phone_deliveries_monitor_unlinked_idx ON phone_deliveries (monitor_id) WHERE incident_id IS NULL;
-- Rows a crash left in `sending`, for the sweep that settles them as `outcome_unknown`.
CREATE INDEX phone_deliveries_sending_idx ON phone_deliveries (updated_at) WHERE reason = 'sending';

-- incidents.ref (D21, D27). The draw is split from the uniqueness loop so that each can be read on its
-- own: the candidate is eight characters, one per random byte — 256 is a multiple of 32, so `& 31` is
-- uniform — and the loop returns the first candidate no incident holds. Two concurrent draws of one
-- value out of 2^40 can still meet at the unique index; no retry is added for that (§8).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cerbix_incident_ref_candidate() RETURNS text AS $$
DECLARE
    alphabet constant text := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    b bytea := gen_random_bytes(8);
    ref text := '';
BEGIN
    FOR i IN 0..7 LOOP
        ref := ref || substr(alphabet, (get_byte(b, i) & 31) + 1, 1);
    END LOOP;
    RETURN ref;
END;
$$ LANGUAGE plpgsql VOLATILE;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cerbix_new_incident_ref() RETURNS text AS $$
DECLARE
    candidate text;
BEGIN
    LOOP
        candidate := cerbix_incident_ref_candidate();
        EXIT WHEN NOT EXISTS (SELECT 1 FROM incidents WHERE ref = candidate);
    END LOOP;
    RETURN candidate;
END;
$$ LANGUAGE plpgsql VOLATILE;
-- +goose StatementEnd

-- The unique index comes BEFORE the backfill: each draw's lookup is then an index probe rather than
-- a scan, and NULLs (the rows not yet backfilled) do not conflict.
ALTER TABLE incidents ADD COLUMN ref text NULL;
CREATE UNIQUE INDEX incidents_ref_key ON incidents (ref);

-- One UPDATE per row, so each draw sees the refs handed out before it rather than relying on what a
-- single statement's own earlier rows look like to a function it calls.
-- +goose StatementBegin
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN SELECT id FROM incidents WHERE ref IS NULL LOOP
        UPDATE incidents SET ref = cerbix_new_incident_ref() WHERE id = r.id;
    END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE incidents ALTER COLUMN ref SET DEFAULT cerbix_new_incident_ref();
ALTER TABLE incidents ALTER COLUMN ref SET NOT NULL;

-- +goose Down
-- Phone channels are refused, not deleted: a rollback must not silently remove someone's pager.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM notification_channels WHERE type IN ('sms', 'voice')) THEN
        RAISE EXCEPTION 'migration 00120 down: % sms/voice notification channel(s) exist; delete them first',
            (SELECT count(*) FROM notification_channels WHERE type IN ('sms', 'voice'));
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE incidents ALTER COLUMN ref DROP DEFAULT;
DROP INDEX IF EXISTS incidents_ref_key;
ALTER TABLE incidents DROP COLUMN IF EXISTS ref;
DROP FUNCTION IF EXISTS cerbix_new_incident_ref();
DROP FUNCTION IF EXISTS cerbix_incident_ref_candidate();

DROP TABLE IF EXISTS phone_deliveries;

DROP INDEX IF EXISTS notification_channels_recipient_user_idx;
ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_phone_recipient_chk;
ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_phone_columns_chk;
ALTER TABLE notification_channels DROP CONSTRAINT IF EXISTS notification_channels_disabled_reason_chk;
ALTER TABLE notification_channels DROP CONSTRAINT notification_channels_type_chk;
ALTER TABLE notification_channels ADD CONSTRAINT notification_channels_type_chk
    CHECK (type IN ('webhook', 'slack', 'telegram', 'email'));
ALTER TABLE notification_channels DROP COLUMN IF EXISTS disabled_reason;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS number_verified_at;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS number_masked;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS number_hash;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS number_enc;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS recipient_user_id;
ALTER TABLE notification_channels DROP COLUMN IF EXISTS recipient;

DROP TABLE IF EXISTS phone_verifications;
DROP TABLE IF EXISTS user_phones;

ALTER TABLE instance_settings DROP COLUMN IF EXISTS telephony_can_place_calls;
ALTER TABLE instance_settings DROP CONSTRAINT IF EXISTS instance_settings_telephony_provider_chk;
ALTER TABLE instance_settings DROP COLUMN IF EXISTS telephony_provider;
ALTER TABLE instance_settings DROP COLUMN IF EXISTS telephony_enabled;
ALTER TABLE instance_settings DROP COLUMN IF EXISTS telephony;
