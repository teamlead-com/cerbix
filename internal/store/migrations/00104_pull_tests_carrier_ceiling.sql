-- +goose Up
-- A7: narrow `pull_tests` back to the generations a test carrier can actually ride.
--
-- 00101 widened BOTH pull tables to admit protocol version 4, and only one of them needed it.
-- Generation 4 is real on the job path — the scheduler enqueues `pull_jobs` rows at it and agents
-- claim them — but no generation-4 TEST carrier exists on any surface: `testsQueueForGeneration`
-- maps generations 1, 2 and 3 and refuses everything else, and the pull test path mirrors it. The
-- widened CHECK therefore admits a row nothing writes and nothing could consume.
--
-- That is not merely unused. The claim predicate selects by `protocol_version <= capability`, so a
-- row carrying a generation no agent declares is invisible to every agent — the silent blackhole
-- 00101's own header says the CHECK exists to prevent. Leaving it open with a comment saying the
-- value is "reserved" would put a sentence where a constraint belongs, which is the shape the whole
-- of this package is about.
--
-- A NEW forward migration and not an edit to 00101: 00101 is applied, so rewriting its text would
-- move only the fresh-install schema and leave every deployed database exactly as it is. This is an
-- upgrade-path statement.
--
-- `pull_jobs` is deliberately NOT touched. The asymmetry IS the item: generation 4 is reachable on
-- the job path and reserved-and-unreachable on the test path.
--
-- Narrowing would fail on its own — ADD CONSTRAINT validates the rows already present — but the
-- bare message names no table, no count and no procedure. The guard below refuses with all three
-- and deletes nothing, which is the pattern 00101's own Down established. A generation-4 row on the
-- test path can only be a payload no build emits, so this refusal is a diagnosis rather than an
-- operational chore.
-- +goose StatementBegin
DO $$
DECLARE
    pending_tests integer;
BEGIN
    SELECT count(*) INTO pending_tests FROM pull_tests WHERE protocol_version = 4;
    IF pending_tests > 0 THEN
        RAISE EXCEPTION
            'refusing to narrow the pull_tests carrier ceiling: % row(s) at protocol_version 4. No build emits a generation-4 test carrier, so these rows were not written by this product; inspect them, then delete them explicitly. This migration will not discard them for you.',
            pending_tests;
    END IF;
END
$$;
-- +goose StatementEnd

-- The drop and the add are ONE statement, so a failed narrowing rolls back atomically and the WIDE
-- constraint survives: a refused upgrade never leaves the table unconstrained. Re-runnable
-- unchanged, which is what `IF EXISTS` on the drop is for.
ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2, 3));

-- +goose Down
-- Widening validates nothing and can never fail, so the rollback contract here is the easy
-- direction. Worth stating precisely, because 00101's Down is the hard one and a reader arriving
-- from it would expect a guard: there is nothing to guard against when the admitted set only grows.
ALTER TABLE pull_tests
    DROP CONSTRAINT IF EXISTS pull_tests_protocol_version_check,
    ADD CONSTRAINT pull_tests_protocol_version_check CHECK (protocol_version IN (1, 2, 3, 4));
