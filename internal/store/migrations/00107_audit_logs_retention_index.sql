-- +goose Up
CREATE INDEX IF NOT EXISTS audit_logs_created_at_id_idx ON audit_logs (created_at, id);

-- +goose Down
DROP INDEX IF EXISTS audit_logs_created_at_id_idx;
