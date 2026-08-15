-- +goose Up
-- Secret inventory (spec func-secret-inventory §4.1/§4.3). project_secrets is the
-- per-project credential inventory: slug-named, AEAD-encrypted at rest with AAD bound to
-- (project_id, id) — stable identifiers only, so rename never re-encrypts. The extra
-- UNIQUE (id, project_id) is the FK target that lets monitor_secret_refs reference a
-- secret tenant-safely (the composite FK proves the referenced secret lives in the same
-- project as the referencing monitor, mirroring 00059/00060).
CREATE TABLE project_secrets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name            text NOT NULL,
    value_encrypted text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    rotated_at      timestamptz,
    UNIQUE (project_id, name),
    UNIQUE (id, project_id)
);

-- One row per (monitor, setting key) credential reference (spec §4.3). Tenant safety is
-- schema-enforced two ways: the composite FK (monitor_id, project_id) → monitors proves
-- the referencing monitor lives in the ref's project (target unique added by 00060), and
-- the composite FK (secret_id, project_id) → project_secrets proves the referenced secret
-- does too. The secret FK is ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED: it is the
-- commit-time delete guard (a secret delete that would orphan refs fails at COMMIT →
-- mapped to 409 secret_in_use), while a project delete stays order-independent — the
-- projects cascade removes secrets and monitors (→ refs) in either order and the deferred
-- check passes at commit because both sides are gone.
CREATE TABLE monitor_secret_refs (
    monitor_id  uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    project_id  uuid NOT NULL,
    setting_key text NOT NULL,
    secret_id   uuid NOT NULL,
    PRIMARY KEY (monitor_id, setting_key),
    FOREIGN KEY (monitor_id, project_id) REFERENCES monitors (id, project_id) ON DELETE CASCADE,
    FOREIGN KEY (secret_id, project_id) REFERENCES project_secrets (id, project_id)
        ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED
);

-- Guard-count lookups: delete/rename guards and used-by counts scan refs by secret.
CREATE INDEX monitor_secret_refs_secret_idx ON monitor_secret_refs (secret_id);

-- +goose Down
DROP TABLE IF EXISTS monitor_secret_refs;
DROP TABLE IF EXISTS project_secrets;
