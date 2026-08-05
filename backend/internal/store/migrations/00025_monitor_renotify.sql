-- +goose Up
-- Re-notify: while a monitor stays down, re-send its alert every renotify_seconds
-- (0 = off, the default). last_notified_at tracks the last time we notified for the
-- current down episode (set on the down flip, bumped by each reminder, cleared on
-- recovery); NULL means "not currently alerting" (up, or suppressed by maintenance).

ALTER TABLE monitors ADD COLUMN IF NOT EXISTS renotify_seconds int NOT NULL DEFAULT 0;
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS last_notified_at timestamptz;

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS renotify_seconds;
ALTER TABLE monitors DROP COLUMN IF EXISTS last_notified_at;
