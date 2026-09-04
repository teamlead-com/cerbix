-- +goose Up
-- Carrier generation 4 carries job identity — the fact that a run was EXPECTED (FR-032, D-0237).
-- The CHECK is WIDENED rather than dropped, for the reason 00063 wrote down when it did the same
-- for generation 3: the claim predicate selects by `protocol_version <= capability`, so a row
-- carrying a generation nobody declares would be invisible to every agent — a silent blackhole of
-- exactly the kind that amendment exists to prevent. An unknown generation must still be rejected
-- at the row level.
--
-- Nothing emits generation 4 when this lands, and nothing can: the payload that DEFINES a
-- generation-4 job does not exist yet, and neither does the setting that would select the carrier.
-- This migration widens a boundary and changes no behaviour.
ALTER TABLE pull_jobs
    DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2, 3, 4));

ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2, 3, 4));

-- +goose Down
-- FAIL CLOSED while generation-4 rows exist, and say enough that an operator can act on it.
--
-- 00063 recorded why this shape exists at all: its own first draft "said 'drain first' and then
-- unconditionally DELETEd them, which is a destructive write-off wearing the words of a safe
-- rollback". D-0160 makes draining an explicit OPERATOR step, so this refuses instead of
-- performing it. Nothing below deletes a row.
--
-- The block is NOT what makes the rollback non-destructive: narrowing the CHECK would fail on its
-- own, because ADD CONSTRAINT validates the rows already there. What it adds is a message an
-- operator can act on. Without it the failure reads "check constraint ... is violated by some
-- row" — no count, no table, no procedure — which is why invariant 10j asserts the refusal and
-- its guidance SEPARATELY, and why the count is reported per table: an operator draining needs to
-- know whether the rows are queued jobs or in-flight Test Connections, since those drain
-- differently.
-- +goose StatementBegin
DO $$
DECLARE
    pending_jobs  integer;
    pending_tests integer;
BEGIN
    SELECT count(*) INTO pending_jobs  FROM pull_jobs  WHERE protocol_version = 4;
    SELECT count(*) INTO pending_tests FROM pull_tests WHERE protocol_version = 4;
    IF pending_jobs + pending_tests > 0 THEN
        RAISE EXCEPTION
            'refusing to roll back carrier generation 4: % pending row(s) — % in pull_jobs, % in pull_tests. Stop emission, wait out the job TTL, then purge them explicitly (D-0160); this migration will not discard them for you.',
            pending_jobs + pending_tests, pending_jobs, pending_tests;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE pull_jobs
    DROP CONSTRAINT IF EXISTS pull_jobs_protocol_version_check,
    ADD CONSTRAINT pull_jobs_protocol_version_check CHECK (protocol_version IN (1, 2, 3));

ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2, 3));
