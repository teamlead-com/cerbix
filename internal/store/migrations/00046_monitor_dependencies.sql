-- +goose Up
-- Monitor dependency graph (D-0100): edges child → parent. While any (transitive)
-- parent is down, the child's alerts are suppressed at delivery time — the data
-- (heartbeats/status/incidents/SLA) keeps recording honestly. Roles are relative:
-- "parent" is not a monitor property, only the direction of an edge; cycle
-- rejection (recursive CTE in the store) keeps the graph a DAG.
CREATE TABLE monitor_dependencies (
    monitor_id    uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    depends_on_id uuid NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (monitor_id, depends_on_id),
    CONSTRAINT monitor_dependencies_no_self CHECK (monitor_id <> depends_on_id)
);
CREATE INDEX monitor_dependencies_parent_idx ON monitor_dependencies (depends_on_id);

-- +goose Down
DROP TABLE monitor_dependencies;
