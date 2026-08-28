"""Fixture tests for the FR-024 stale-spelling guard in check-docs-references.py.

A guard that is not itself tested is a sentence about a guard. Each case below is a shape one review
round actually found, or the hole the guard's own first draft had.
"""
import importlib.util
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("cdr", os.path.join(HERE, "check-docs-references.py"))
cdr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cdr)

SPEC = "docs/specs/func-reliability-gate.md"


def flagged(path, text):
    return [m for (_, _, _, m) in cdr.gate_stale_findings(path, text.split("\n"))]


class StaleSpellingGuard(unittest.TestCase):
    def test_prose_in_the_spec_is_flagged(self):
        self.assertTrue(flagged(SPEC, "within bounds; `max_seal_lag` is a duration in `1m..24h`."))

    def test_the_normative_schema_fence_is_scanned(self):
        text = "```\nservice_gate_policies (max_seal_lag int CHECK 300..86400)\n```"
        self.assertTrue(flagged(SPEC, text), "a retired spelling inside the schema fence must be flagged")

    def test_only_the_marked_fixture_fence_is_a_quotation(self):
        text = "```retired-spellings\nmax_seal_lag (not followed by _seconds)\n1m..24h\n```"
        self.assertEqual(flagged(SPEC, text), [])

    def test_the_new_spelling_is_not_flagged(self):
        self.assertEqual(flagged(SPEC, "`max_seal_lag_seconds` is an integer in `300..86400`."), [])

    def test_status_rows_for_the_requirement_are_scanned(self):
        row = "| FR-024 | the seal-lag bound (`max_seal_lag`, default 15m) | TODO | x |"
        self.assertTrue(flagged("docs/status.md", row))

    def test_other_status_rows_are_not(self):
        row = "| FR-021 | something mentioning max_seal_lag historically | DONE | x |"
        self.assertEqual(flagged("docs/status.md", row), [])

    def test_decision_sections_are_scoped_by_heading(self):
        late = ("## D-0199 — FR-024 revision 9: something\n\n"
                "the per-policy `max_seal_lag` stays the owner's authority.")
        self.assertTrue(flagged("docs/decisions.md", late), "a late FR-024 decision line must be flagged")
        other = ("## D-0150 — status projection\n\n"
                 "the old `max_seal_lag` idea from another feature.")
        self.assertEqual(flagged("docs/decisions.md", other), [])

    def test_a_supersession_note_may_quote(self):
        note = ("## D-0190 — FR-024 seal lag\n\n"
                "> This record originally said `max_seal_lag` and `1m..24h`.\n"
                "`max_seal_lag_seconds` (named `max_seal_lag` at the time, renamed in revision 4) is the field.")
        self.assertEqual(flagged("docs/decisions.md", note), [])

    def test_mentioning_a_revision_number_is_not_an_exemption(self):
        self.assertTrue(flagged(SPEC, "Revision 3 allowed `max_seal_lag` of one minute, which is wrong."))

    def test_revision_six_vocabulary_is_retired(self):
        # Round 6 P1-4: the guard passed while §7 still required the names §5a had dropped.
        for line in (
            "the purge runs in batches of `decision_purge_batch`",
            "`…_purge_backlog_rows` moves",
            "`…_oldest_eligible_seconds` moves",
            "one partition per calendar month",
            "lives in a monthly RANGE partition",
            "`fact_revision_ids[]` — the revisions",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        self.assertEqual(flagged(SPEC, "`decision_purge_max_partitions`, `fact_revisions`, one partition per UTC day"), [])

    def test_revision_seven_vocabulary_is_retired(self):
        for line in (
            "because a decision id carries no time",
            "373 at the 365-day maximum",
            "the full list is recoverable from the retained",
            "with `CREATE TABLE IF NOT EXISTS … PARTITION OF`",
            "a row lives at most `retention + 1 day` and",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "readable until detach (`retention + <1 day + ≤ purge_every`)",
            "PARTITION BY RANGE (evaluated_at)",
            "`CREATE TABLE … PARTITION OF` takes `ACCESS EXCLUSIVE` on the parent, which is why it is not used",
            "up to `retention + lead + 1 = 396` of them",
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_revision_eight_vocabulary_is_retired(self):
        for line in (
            "lifetime `retention + 1 day + decision_purge_every` under a healthy pass",
            "the read answers 500 `ledger_identity` and never picks one",
            "answers 404 without touching the database",
            "at most `lead + 1` cheap operations, unbudgeted, so",
            '`cerbix_gate_evaluate_errors_total{kind="partition_identity"}` counts it',
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "`retention + <1 day + ≤ decision_purge_every` after `evaluated_at`",
            '`cerbix_gate_maintenance_errors_total{kind="partition_identity"}` and pages',
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_revision_nine_vocabulary_is_retired(self):
        for line in (
            "the two indexes and fill factor sit outside",
            "INDEX (id), INDEX (project_id, evaluated_at DESC);",
            "`detached_at` is older than one `decision_purge_every` AND",
            "the D7 always-present fields plus `state`, `action`, `service_id`",
            "so creation costs at most `2 × 2 s × create_max` of the 30 s pass",
            "within the rate and concurrency bounds above",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "the four indexes per partition (§5: the PK, the local unique id, the two listing paths",
            "`CREATE UNIQUE INDEX (id)` + `COMMENT ON TABLE` — the LOCAL UNIQUE INDEX (id) is current vocabulary",
            "`detached_at <= now() − decision_purge_every` on the database clock",
            "evaluation under rate AND concurrency, ledger reads under concurrency only",
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_revision_ten_vocabulary_is_retired(self):
        for line in (
            "the extra row's existence, not a count, produces `next_cursor`",
            "produces `next_cursor` from the `LIMIT + 1` row and `null`",
            "body {policy_revision, action, reason, expires_at}",
            "and the pool holds one connection fewer after a dead-connection release",
            "Each page is ONE index-range scan (review round 9 P1-3)",
            "Removal therefore begins no later than t = 12 s with budget for at least one",
            "reads as inert in `GET …/override` history",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "encodes the cursor from the last of THOSE — never from the extra row",
            "body {policy_revision, reason, expires_at}",
            "an Append or Merge Append with one scan per surviving child",
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_revision_eleven_vocabulary_is_retired(self):
        for line in (
            "(`lock_timeout + statement_timeout`) fits before the deadline",
            "the six policy/override routes of D13a",
            "revoked_via_token (null until revoked)}",
            "one-active, seven-day regime keeps that small",
            "skips the pass. Each pass is bounded to `subCadenceTimeout`.",
            "each pass bounded to `subCadenceTimeout` |",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "`lock_timeout = min(2 s, statement_timeout)`",
            "the eight policy/override routes of D13a",
            "a pass's whole lifecycle (work ≤ 27 s + cleanup ≤ 3 s) fits `subCadenceTimeout`",
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_revision_twelve_vocabulary_is_retired(self):
        for line in (
            "the revoker triple is non-null for `manual` and null for `expired`",
            "only a `manual` closure has a human revoker",
            "the same row reads the same status before and after that closure",
            "Concurrent inserts cannot duplicate or skip an item: a new row has a later `evaluated_at`",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        for line in (
            "only a `manual` closure carries attribution",
            "a key returned once is never returned again",
        ):
            self.assertEqual(flagged(SPEC, line), [], line)

    def test_pre_approval_lifecycle_wording_is_retired(self):
        for line in (
            "Two gates remain before code: the review's focused confirmation of THIS revision, and",
            "## 6. Acceptance invariants (FR-024) — draft, numbered on acceptance",
        ):
            self.assertTrue(flagged(SPEC, line), line)
        self.assertTrue(flagged("docs/status.md", "| FR-024 | … Ahead of code: the review's focused confirmation and an approved UI mock. | TODO | x |"))
        self.assertEqual(flagged(SPEC, "## 6. Acceptance invariants (FR-024) — approved design contract, discharged on implementation"), [])

    def test_duplicate_schema_headers_are_caught(self):
        text = "service_gate_decisions  (a)\nservice_gate_overrides  (b)\nservice_gate_decisions  (c)\n"
        self.assertEqual(cdr.gate_duplicate_headers(text), ["service_gate_decisions"])


if __name__ == "__main__":
    unittest.main()
