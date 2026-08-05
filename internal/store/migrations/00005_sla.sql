-- +goose Up
-- SLO targets and maintenance windows for SLA computation.

CREATE TABLE sla_targets (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    monitor_id uuid REFERENCES monitors (id) ON DELETE CASCADE,
    project_id uuid REFERENCES projects (id) ON DELETE CASCADE,
    objective   numeric(6, 4) NOT NULL,
    window_name text NOT NULL DEFAULT '30d', -- "window" is a reserved word
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- Exactly one of monitor_id / project_id must be set.
    CONSTRAINT sla_targets_scope_chk CHECK ((monitor_id IS NULL) <> (project_id IS NULL)),
    CONSTRAINT sla_targets_objective_chk CHECK (objective > 0 AND objective <= 100)
);

CREATE UNIQUE INDEX sla_targets_monitor_uniq ON sla_targets (monitor_id, window_name) WHERE monitor_id IS NOT NULL;
CREATE UNIQUE INDEX sla_targets_project_uniq ON sla_targets (project_id, window_name) WHERE project_id IS NOT NULL;

CREATE TABLE maintenance_windows (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    monitor_id uuid REFERENCES monitors (id) ON DELETE CASCADE, -- NULL = whole project
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    reason     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT maintenance_windows_range_chk CHECK (ends_at > starts_at)
);

CREATE INDEX maintenance_windows_project_idx ON maintenance_windows (project_id);
CREATE INDEX maintenance_windows_monitor_idx ON maintenance_windows (monitor_id);
CREATE INDEX maintenance_windows_span_idx ON maintenance_windows (starts_at, ends_at);

-- +goose Down
DROP TABLE maintenance_windows;
DROP TABLE sla_targets;
