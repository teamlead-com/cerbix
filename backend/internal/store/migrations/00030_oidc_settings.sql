-- +goose Up
-- Instance-wide OIDC configuration, editable from the Settings UI (global-admin
-- only). A single row (id = true) holds the override; when it exists it is
-- authoritative and the YAML oidc: block is ignored (UI fully replaces config).
-- client_secret is stored encrypted at rest via the app keyring (secret.Cipher).
CREATE TABLE oidc_settings (
    id              boolean     PRIMARY KEY DEFAULT true,
    enabled         boolean     NOT NULL DEFAULT false,
    issuer          text        NOT NULL DEFAULT '',
    client_id       text        NOT NULL DEFAULT '',
    client_secret   text        NOT NULL DEFAULT '',
    redirect_url    text        NOT NULL DEFAULT '',
    scopes          text[]      NOT NULL DEFAULT '{}',
    post_logout_url text        NOT NULL DEFAULT '',
    button_label    text        NOT NULL DEFAULT '',
    bootstrap_admins text[]     NOT NULL DEFAULT '{}',
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT oidc_settings_singleton CHECK (id)
);

-- +goose Down
DROP TABLE oidc_settings;
