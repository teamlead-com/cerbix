-- +goose Up
-- observed_at is the RAW client/probe observation timestamp, kept for audit/diagnostics
-- distinct from heartbeats.ts (the effective/ordering timestamp). For a scheduled result
-- the wire `ts` maps to both (observed_at == ts). For push, ts = server received_at while
-- observed_at = the client-supplied timestamp (currently always absent → NULL). Nullable:
-- legacy rows and push without a client timestamp have no raw observation, and a synthetic
-- zero must never be written (see spec func-result-protocol §4).
ALTER TABLE heartbeats ADD COLUMN IF NOT EXISTS observed_at timestamptz;

-- +goose Down
ALTER TABLE heartbeats DROP COLUMN IF EXISTS observed_at;
