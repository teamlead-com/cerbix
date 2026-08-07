-- +goose Up
-- Lease-based claim for pull jobs. Previously a claim DELETEd the row (at-most-once):
-- if the agent crashed, or the claim HTTP response was lost in flight, the job was
-- gone and the monitor silently missed a probe cycle. Now a claim LEASES the row
-- (sets claim_token + lease_expires_at) and the agent acks it — via the tokens echoed
-- on its results POST — which deletes it. A crashed/disconnected agent's lease simply
-- expires and the job becomes claimable again (until its outer TTL, expires_at, purges
-- it). Duplicate re-delivery is safe: RecordResult dedups heartbeats and freshness-gates
-- live status (migration 00051).
ALTER TABLE pull_jobs ADD COLUMN IF NOT EXISTS claim_token uuid;
ALTER TABLE pull_jobs ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;

-- +goose Down
ALTER TABLE pull_jobs DROP COLUMN IF EXISTS lease_expires_at;
ALTER TABLE pull_jobs DROP COLUMN IF EXISTS claim_token;
