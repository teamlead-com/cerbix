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
-- Refuse to narrow the CHECK while generation-3 rows exist: dropping to (1,2) with such a
-- row present would fail the constraint validation anyway, and doing it silently would
-- strand work. Drain generation 3 first (stop emission, wait out the TTL) and re-run.
DELETE FROM pull_jobs WHERE protocol_version = 3;
DELETE FROM pull_tests WHERE protocol_version = 3;

ALTER TABLE pull_jobs
    DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2));

ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2));
