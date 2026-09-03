-- +goose Up
-- FR-030 (D-0234): a monitor says what it is for. One optional plain-text field, bounded to 200
-- characters by the domain (counted as code points, the same way the form counts), shown wherever a
-- monitor is listed. NOT NULL DEFAULT '' so every monitor that exists today reads back with an empty
-- description and every surface renders exactly as it did — the whole compatibility promise in one
-- default. No index: nothing reads by it, and the owner declined search on it.
ALTER TABLE monitors ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE monitors DROP COLUMN IF EXISTS description;
