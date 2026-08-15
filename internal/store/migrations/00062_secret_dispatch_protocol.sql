-- +goose Up
-- Physical/claim wire barrier and executor diagnostics for FR-020 (§4.7).
ALTER TABLE pull_jobs
    ADD COLUMN protocol_version integer NOT NULL DEFAULT 1,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2));

ALTER TABLE pull_tests
    ADD COLUMN protocol_version integer NOT NULL DEFAULT 1,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2));

CREATE INDEX pull_jobs_region_protocol_claim_idx
    ON pull_jobs (region, protocol_version, created_at);
CREATE INDEX pull_tests_region_protocol_claim_idx
    ON pull_tests (region, protocol_version, created_at);

ALTER TABLE agent_heartbeats
    ADD COLUMN capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN credential_ready boolean NOT NULL DEFAULT false;

ALTER TABLE monitors
    ADD COLUMN last_probe_error_reason text,
    ADD COLUMN last_probe_error_at timestamptz,
    ADD COLUMN last_probe_error_job_id text;

-- +goose Down
ALTER TABLE monitors
    DROP COLUMN IF EXISTS last_probe_error_job_id,
    DROP COLUMN IF EXISTS last_probe_error_at,
    DROP COLUMN IF EXISTS last_probe_error_reason;
ALTER TABLE agent_heartbeats
    DROP COLUMN IF EXISTS credential_ready,
    DROP COLUMN IF EXISTS capabilities;
DROP INDEX IF EXISTS pull_tests_region_protocol_claim_idx;
DROP INDEX IF EXISTS pull_jobs_region_protocol_claim_idx;
ALTER TABLE pull_tests DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check;
ALTER TABLE pull_tests DROP COLUMN IF EXISTS protocol_version;
ALTER TABLE pull_jobs DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check;
ALTER TABLE pull_jobs DROP COLUMN IF EXISTS protocol_version;
