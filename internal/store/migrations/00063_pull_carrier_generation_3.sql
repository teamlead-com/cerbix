-- +goose Up
-- Carrier generation 3 is the pull half of the envelope-v2 rollout (§4.7, D-0160). The
-- CHECK is widened rather than dropped: an unknown generation must still be rejected at
-- the row level, because the claim predicate selects by `protocol_version <= capability`
-- and a row with a generation nobody declares would be invisible to every agent — a silent
-- blackhole of exactly the kind this amendment exists to prevent.
--
-- Nothing emits generation 3 yet; the emitter switches per region only once that region's
-- existential readiness check passes for capability 2.
ALTER TABLE pull_jobs
    DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2, 3));

ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2, 3));

-- +goose Down
-- FAIL CLOSED while generation-3 rows exist. The first draft said "drain first" and then
-- unconditionally DELETEd them, which is a destructive write-off wearing the words of a
-- safe rollback: a rollback issued before the TTL would silently discard pending jobs and
-- in-flight Test Connections. D-0160 makes draining an explicit OPERATOR step — stop
-- emission, wait out max job TTL, then purge or record the write-off — so this migration
-- refuses rather than performing it silently.
-- +goose StatementBegin
DO $$
DECLARE
    pending integer;
BEGIN
    SELECT (SELECT count(*) FROM pull_jobs  WHERE protocol_version = 3)
         + (SELECT count(*) FROM pull_tests WHERE protocol_version = 3)
      INTO pending;
    IF pending > 0 THEN
        RAISE EXCEPTION
            'refusing to roll back carrier generation 3: % pending row(s). Stop emission, wait out the job TTL, then purge them explicitly (D-0160).',
            pending;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE pull_jobs
    DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2));

ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2));
