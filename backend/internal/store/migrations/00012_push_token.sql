-- +goose Up
-- Push monitors carry a secret token; the passive heartbeat endpoint looks them
-- up by it (FR: push / dead-man's-switch checks).

ALTER TABLE monitors ADD COLUMN push_token text;

CREATE UNIQUE INDEX monitors_push_token_uniq ON monitors (push_token) WHERE push_token IS NOT NULL;

-- +goose Down
DROP INDEX monitors_push_token_uniq;
ALTER TABLE monitors DROP COLUMN push_token;
