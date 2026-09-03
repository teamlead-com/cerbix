-- +goose Up
-- FR-029 §4.2: a per-JOB claim lease.
--
-- `pull_jobs` has two clocks and they do different things. `expires_at`, set at enqueue to the
-- monitor's interval, drops a job that is a full cycle late. `lease_expires_at`, set at CLAIM, is how
-- long one agent holds it — and it came from a single hardcoded 30 seconds in the agent endpoint,
-- for the whole batch, regardless of which monitors were in it.
--
-- Thirty seconds is shorter than an async canary's journey, so a second agent would re-claim a job
-- the first is still running and submit a SECOND external transaction. That is the one place where
-- "the probe takes minutes" turns into a side effect at someone else's expense.
--
-- It is also a defect that PREDATES the canary: any pull monitor with `timeout_seconds` > 30 (legal
-- today — the bound is 300) is already re-claimable mid-probe. For the ordinary types the cost is a
-- duplicate request and a duplicate heartbeat, which is why nobody noticed. This column fixes it for
-- every type at once.
--
-- NULL means "use the endpoint's default", so every existing row and every monitor that does not ask
-- for more keeps exactly today's behaviour.
ALTER TABLE pull_jobs ADD COLUMN IF NOT EXISTS lease_seconds integer;

-- +goose Down
ALTER TABLE pull_jobs DROP COLUMN IF EXISTS lease_seconds;
