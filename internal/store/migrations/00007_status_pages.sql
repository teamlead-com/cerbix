-- +goose Up
-- Status pages and their components (FR-012 phase 2a).

CREATE TABLE status_pages (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id     uuid REFERENCES projects (id) ON DELETE CASCADE, -- NULL = org-level page
    slug           text NOT NULL UNIQUE,
    title          text NOT NULL,
    visibility     text NOT NULL DEFAULT 'internal',
    unlisted_token text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT status_pages_visibility_chk CHECK (visibility IN ('public', 'internal', 'unlisted'))
);

CREATE INDEX status_pages_org_idx ON status_pages (org_id);

CREATE TABLE components (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status_page_id uuid NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    name           text NOT NULL,
    description    text NOT NULL DEFAULT '',
    group_name     text NOT NULL DEFAULT '',
    position       integer NOT NULL DEFAULT 0,
    monitor_id     uuid REFERENCES monitors (id) ON DELETE SET NULL, -- NULL = manual component
    manual_status  text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT components_manual_status_chk CHECK (
        manual_status IN ('', 'operational', 'degraded', 'partial_outage', 'major_outage', 'maintenance')
    )
);

CREATE INDEX components_page_idx ON components (status_page_id, position);

-- Email subscribers to a status page (double opt-in: confirmed_at set on confirm).
CREATE TABLE subscribers (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status_page_id uuid NOT NULL REFERENCES status_pages (id) ON DELETE CASCADE,
    email          text NOT NULL,
    confirm_token  text NOT NULL UNIQUE,
    confirmed_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (status_page_id, email)
);

CREATE INDEX subscribers_page_idx ON subscribers (status_page_id);

-- +goose Down
DROP TABLE subscribers;
DROP TABLE components;
DROP TABLE status_pages;
