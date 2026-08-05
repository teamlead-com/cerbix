-- +goose Up
-- region is the worker-pool that probes this monitor. Default 'core' = central pool
-- (existing monitors stay on the central workers). A geo/edge worker runs with
-- --region <name> and executes only monitors whose region matches.
ALTER TABLE monitors ADD COLUMN region text NOT NULL DEFAULT 'core';
CREATE INDEX monitors_region_idx ON monitors (region);

-- +goose Down
DROP INDEX IF EXISTS monitors_region_idx;
ALTER TABLE monitors DROP COLUMN IF EXISTS region;
