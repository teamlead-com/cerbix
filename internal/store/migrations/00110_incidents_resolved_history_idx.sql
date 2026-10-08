-- +goose Up
-- iter-0203 (FR-039 / NFR-033, func-status-pages-incidents.md §13.4): a status page's past
-- incidents and its month-paged incident history read the RESOLVED incidents of the page's
-- projects newest first, by (resolved_at, id). No index covered resolved_at before; this partial
-- index is in exactly that order. Incidents are an ordinary table in both storage modes.
CREATE INDEX IF NOT EXISTS incidents_resolved_history_idx
    ON incidents (project_id, resolved_at DESC, id DESC)
    WHERE status = 'resolved';

-- +goose Down
DROP INDEX IF EXISTS incidents_resolved_history_idx;
