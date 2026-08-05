-- +goose Up
-- Incidents, their timeline updates, and postmortems (FR-012 phase 1).

CREATE TABLE incidents (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    title       text NOT NULL,
    status      text NOT NULL DEFAULT 'investigating',
    impact      text NOT NULL DEFAULT 'minor',
    source      text NOT NULL DEFAULT 'manual',
    started_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incidents_status_chk CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    CONSTRAINT incidents_impact_chk CHECK (impact IN ('none', 'minor', 'major', 'critical')),
    CONSTRAINT incidents_source_chk CHECK (source IN ('manual', 'api', 'auto'))
);

CREATE INDEX incidents_project_idx ON incidents (project_id, started_at DESC);

CREATE TABLE incident_updates (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id uuid NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    status      text NOT NULL,
    body        text NOT NULL DEFAULT '',
    author      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incident_updates_status_chk CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved'))
);

CREATE INDEX incident_updates_incident_idx ON incident_updates (incident_id, created_at);

CREATE TABLE postmortems (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id  uuid NOT NULL UNIQUE REFERENCES incidents (id) ON DELETE CASCADE,
    body         text NOT NULL DEFAULT '',
    author       text NOT NULL DEFAULT '',
    published_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE postmortems;
DROP TABLE incident_updates;
DROP TABLE incidents;
