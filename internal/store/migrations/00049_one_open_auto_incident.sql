-- +goose Up
-- At most one OPEN auto-incident per monitor. ingest.openAutoIncident checks
-- "is one already open?" then creates — a TOCTOU race two concurrent down
-- transitions can both pass, double-opening. A partial unique index closes it at
-- the database; the create path maps the violation to a benign "already open".
--
-- First resolve any pre-existing duplicates (keep the earliest-started open
-- auto-incident per monitor, resolve the rest) so the unique index can be built.
-- +goose StatementBegin
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY monitor_id ORDER BY started_at, id) AS rn
      FROM incidents
     WHERE source = 'auto' AND status <> 'resolved' AND monitor_id IS NOT NULL
)
UPDATE incidents i
   SET status = 'resolved', resolved_at = COALESCE(i.resolved_at, now()), updated_at = now()
  FROM ranked r
 WHERE i.id = r.id AND r.rn > 1;
-- +goose StatementEnd

CREATE UNIQUE INDEX IF NOT EXISTS incidents_one_open_auto
    ON incidents (monitor_id)
    WHERE source = 'auto' AND status <> 'resolved' AND monitor_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS incidents_one_open_auto;
