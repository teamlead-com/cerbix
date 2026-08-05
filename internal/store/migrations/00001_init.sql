-- +goose Up
-- Tenant hierarchy (organizations -> projects), users, and role memberships.

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    slug       text NOT NULL,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug),
    -- Composite unique target so memberships can FK (project_id, org_id) and
    -- thereby guarantee a project-scoped membership names the project's own org.
    UNIQUE (id, org_id)
);

CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub    text NOT NULL UNIQUE,
    email           text NOT NULL,
    display_name    text NOT NULL DEFAULT '',
    is_global_admin boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id     uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    project_id uuid,
    role       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- When project_id is set it must belong to org_id (MATCH SIMPLE skips the FK
    -- when project_id IS NULL, i.e. for org-level grants).
    FOREIGN KEY (project_id, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE,

    -- Role must be valid for its scope (mirrors domain.Role.ValidForScope).
    CONSTRAINT memberships_role_scope_chk CHECK (
        (project_id IS NULL     AND role IN ('org_admin', 'editor', 'viewer'))
        OR (project_id IS NOT NULL AND role IN ('project_admin', 'editor', 'viewer'))
    )
);

-- One grant per (user, org) at org scope, and one per (user, org, project) at
-- project scope. Partial indexes because project_id is nullable.
CREATE UNIQUE INDEX memberships_user_org_uniq
    ON memberships (user_id, org_id)
    WHERE project_id IS NULL;

CREATE UNIQUE INDEX memberships_user_org_project_uniq
    ON memberships (user_id, org_id, project_id)
    WHERE project_id IS NOT NULL;

CREATE INDEX memberships_user_idx ON memberships (user_id);
CREATE INDEX projects_org_idx ON projects (org_id);

-- +goose Down
DROP TABLE memberships;
DROP TABLE users;
DROP TABLE projects;
DROP TABLE organizations;
