-- +goose NO TRANSACTION
-- +goose Up
-- FR-032 phase F (spec revision 33, §7.4): the publish boundary.
--
-- A running instance recorded that runs had not happened when they had. `AdvanceExpectations`
-- deadlocked against §8.3's own fill — six times in seven minutes of ordinary load — and the
-- scheduler logged the error and DISCARDED the batch's payload, including the identity of jobs it
-- had already published. The next tick reloaded the unmoved `next_due_at`, published a job carrying
-- a window that had already been answered, and the result was refused by §8.1 as it must be; the
-- window the run really answered was then materialized as a gap. Every `expected_never_issued` fact
-- on that instance was false, and `make dev-test` passed throughout.
--
-- The answer is ordering, not retrying: the ledger records the window BEFORE the job is handed to a
-- transport, so an instant is either recorded or the schedule never passed it, and a gap can no
-- longer cover an instant a job was published for. That needs a state the model did not have — a
-- window whose identity is minted and committed but whose dispatch is not recorded. It may read
-- neither absence verdict: `expected_never_issued` asserts the opposite of what is known, and
-- `issued_never_claimed` asserts a publish that has not happened.
--
-- NEITHER column is indexed, and that is load-bearing rather than an omission: `expected_runs` is
-- `fillfactor = 70` so its updates stay HOT (invariant 12), and the CONFIRM writes `issued_at` on
-- every reserved row in the ordinary path. An index on either column would make that update
-- non-HOT and invert the storage argument §11 rests on.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                    WHERE table_name = 'expected_runs' AND column_name = 'reserved_at') THEN
        -- The instant the core minted this window's identity and committed it, BEFORE any
        -- transport saw the job. It is also the instant the job carries on the wire, so an
        -- executor's echo can never introduce a value the core did not already own — invariant 20e
        -- held structurally rather than by a merge expression.
        ALTER TABLE expected_runs ADD COLUMN reserved_at timestamptz;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                    WHERE table_name = 'expected_runs' AND column_name = 'withheld_reason') THEN
        -- Why a reserved window never became a published one. Empty on every other row, so the
        -- column is a fact about withholding and never a NULL that means three different things.
        ALTER TABLE expected_runs ADD COLUMN withheld_reason text NOT NULL DEFAULT '';
    END IF;
END
$$;
-- +goose StatementEnd

-- TWO rules on one column, and both are enforced rather than documented.
--
-- A withheld reason with no reservation describes nothing: the state it annotates is the interval
-- between RESERVE and CONFIRM, which cannot exist without the reservation, and the pair is written
-- by two different statements.
--
-- And the VOCABULARY is closed, exactly as `skip_reason`'s is in 00102. It was described as closed
-- in three places — the `WithheldPublishFailed` constant, `openapi.yaml`'s enum and the runbook —
-- while the write boundary admitted any string at all, so an internal caller could persist a value
-- the API contract does not define and no reader could interpret. A vocabulary that is closed in
-- prose and open in the schema is the defect class this arc keeps producing, found here by the
-- final-tree audit.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'expected_runs_withheld_needs_reservation') THEN
        ALTER TABLE expected_runs ADD CONSTRAINT expected_runs_withheld_needs_reservation
            CHECK (withheld_reason IN ('', 'publish_failed')
                   AND (withheld_reason = '' OR reserved_at IS NOT NULL));
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- The DOWN drops the state, which means every window currently between RESERVE and CONFIRM becomes
-- indistinguishable from one that was published — the exact conflation phase F exists to end. It is
-- accepted here and NOT guarded the way 00101's is, because the rows are not lost: a reserved
-- window that a rollback re-reads as `issued_never_claimed` over-claims a publish, while 00101's
-- rollback would have DISCARDED in-flight work. Rolling back this migration means accepting the
-- pre-phase-F reading of the ledger, and that is what rolling back a truth-carrying column is.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE conname = 'expected_runs_withheld_needs_reservation') THEN
        ALTER TABLE expected_runs DROP CONSTRAINT expected_runs_withheld_needs_reservation;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE expected_runs DROP COLUMN IF EXISTS withheld_reason;
ALTER TABLE expected_runs DROP COLUMN IF EXISTS reserved_at;
