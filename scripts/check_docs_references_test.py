"""Fixture tests for the FR-024 stale-spelling guard in check-docs-references.py.

A guard that is not itself tested is a sentence about a guard. Each case below is a shape one review
round actually found, or the hole the guard's own first draft had.
"""
import ast
import importlib.util
import os
import pathlib
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
CHECKER = os.path.join(HERE, "check-docs-references.py")
spec = importlib.util.spec_from_file_location("cdr", CHECKER)
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


TABLE = """## H

| # | invariant | discharge |
| - | --------- | --------- |
| 1 | one | `TestOne` |
| 2 | two | `TestTwo` |
"""


class DischargeParsing(unittest.TestCase):
    """Review [49]/[50]: the parser wrote straight into a dict, so a SECOND row for the same number
    replaced the first and disappeared — a table could carry two rows for invariant 1, be one
    invariant short, and still satisfy both a count and a required-key check. The same loop only
    asked whether every required number was present, so an EXTRA row above the count was invisible
    too. Both are now returned, and these probes fail if either regresses."""

    def test_a_second_row_for_the_same_number_is_reported_not_overwritten(self):
        rows, dups = cdr.discharge_rows(TABLE + "| 1 | one again | `TestThree` |\n", "## H")
        self.assertEqual(dups, [1])
        self.assertEqual(rows[1], "`TestOne`")    # the FIRST row is kept, the duplicate is named
        self.assertEqual(sorted(rows), [1, 2])

    def test_a_clean_table_reports_no_duplicates(self):
        rows, dups = cdr.discharge_rows(TABLE, "## H")
        self.assertEqual(dups, [])
        self.assertEqual(sorted(rows), [1, 2])

    def test_a_missing_table_is_distinguishable_from_an_empty_one(self):
        rows, dups = cdr.discharge_rows(TABLE, "## NOT THERE")
        self.assertIsNone(rows)
        self.assertEqual(dups, [])

    def test_a_row_numbered_above_the_matrix_size_is_visible_to_the_caller(self):
        rows, _ = cdr.discharge_rows(TABLE + "| 10 | extra | `TestTen` |\n", "## H")
        # `check_discharge` compares this key set with range(1, count+1) EXACTLY; before review [50]
        # it only looked up the required keys, so this row rode along unnoticed.
        self.assertEqual(sorted(set(rows) - set(range(1, 3))), [10])


class TheCheckerClosesWhatItOpens(unittest.TestCase):
    """Review [54]: every call site read documents with a bare `open(...).read()`, leaking the
    descriptor until the collector noticed — 46 ResourceWarning lines from one verbose run, over the
    temporary fixtures and every scanned living document alike. They are all routed through `read()`
    now, and this asserts it structurally: an AST walk, not a grep, so the sentence describing the
    old habit in that function's own docstring cannot satisfy or trip the check."""

    def test_no_open_call_escapes_a_with_statement(self):
        tree = ast.parse(pathlib.Path(CHECKER).read_text(encoding="utf-8"))
        managed = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.With):
                for item in node.items:
                    for sub in ast.walk(item.context_expr):
                        managed.add(id(sub))
        stray = [
            node.lineno
            for node in ast.walk(tree)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Name)
            and node.func.id == "open"
            and id(node) not in managed
        ]
        self.assertEqual(stray, [], f"open() outside a `with` at line(s) {stray} — use read()")


class DecisionsAreVocabularyGuarded(unittest.TestCase):
    """Review [49]: FR-025 §10 promises the retired spellings are refused in every LIVING document,
    and AGENTS lists docs/decisions.md among those edited in place — but it was in neither the
    scanned list nor, therefore, the guard. Reference checking stays off for it; the vocabulary
    guard does not."""

    def test_decisions_is_in_the_scanned_set_and_not_in_the_reference_checked_one(self):
        self.assertIn(cdr.DECISIONS_DOC, cdr.change_guard_docs())
        self.assertNotIn(cdr.DECISIONS_DOC, cdr.LIVING)  # not reference-checked, deliberately

    def test_a_retired_spelling_in_a_decisions_style_document_is_refused(self):
        """The functional probe, not a list membership: give the guard a file that says
        `change_events` and it must object. The first version of this test asserted on the
        function's docstring, which is a sentence that cannot fail."""
        with tempfile.TemporaryDirectory() as d:
            bad_doc = os.path.join(d, "decisions.md")
            with open(bad_doc, "w", encoding="utf-8") as fh:
                fh.write("## D-9999\n\nThe pipeline writes a row into `change_events` and sets `caused_by`.\n")
            found = cdr.check_change_stale_spellings([bad_doc])
            # BOTH retired spellings on that line are named, not just the first: a reader fixing one
            # and re-running should not discover the other on the next pass.
            self.assertEqual(len(found), 2, found)
            self.assertIn("service_changes", found[0][3])
            self.assertIn("preceded", found[1][3])

            good_doc = os.path.join(d, "clean.md")
            with open(good_doc, "w", encoding="utf-8") as fh:
                fh.write("## D-9999\n\nThe pipeline writes a row into `service_changes` and the note says preceded.\n")
            self.assertEqual(cdr.check_change_stale_spellings([good_doc]), [])

    def test_the_real_decisions_document_carries_no_retired_spelling(self):
        bad = [b for b in cdr.check_change_stale_spellings() if b[0] == cdr.DECISIONS_DOC]
        self.assertEqual(bad, [])


if __name__ == "__main__":
    unittest.main()


class ResolutionAndTestTokens(unittest.TestCase):
    """The two lookups that carried the checker's whole runtime (79 s → 0.3 s, 2026-09-03).

    Both were rewritten from a per-citation scan to a single indexed pass, and both have a shape
    that a speedup can silently break: the path index has to undo the `../` a doc writes because
    it cites relative to its OWN directory, and the test-name set has to keep answering the
    question the old substring test answered. The first of those broke in the first version of
    the rewrite and reported 352 healthy citations as missing.
    """

    def test_a_path_cited_relative_to_the_docs_directory_resolves(self):
        # `docs/status.md` cites the tree as `../internal/...`, which is the common case.
        self.assertTrue(cdr.resolves("../internal/store/monitors.go"))
        self.assertTrue(cdr.resolves("internal/store/monitors.go"))

    def test_a_path_cited_by_suffix_resolves(self):
        self.assertTrue(cdr.resolves("store/monitors.go"))

    def test_a_path_that_does_not_exist_does_not_resolve(self):
        self.assertFalse(cdr.resolves("internal/store/definitely_not_here.go"))
        self.assertFalse(cdr.resolves("../internal/store/definitely_not_here.go"))

    def test_a_declared_test_name_is_found(self):
        self.assertIn("TestScenarioBindingsRefusals", cdr.test_tokens(cdr.source_text()))

    def test_a_prefix_of_a_real_test_name_is_NOT_found(self):
        # The old check was `name in src`, a substring test, so a doc citing `TestScenarioBinding`
        # passed on the strength of `TestScenarioBindingsRefusals` existing. Verified against the
        # pre-rewrite implementation before this test was written: it passed there, and a citation
        # that only ever passed that way is exactly the stale evidence this checker exists to catch.
        self.assertNotIn("TestScenarioBinding", cdr.test_tokens(cdr.source_text()))


class EnumerationParsers(unittest.TestCase):
    """The parsers behind check_enumerations().

    Each is a regex over source it does not own. The failure that matters is not a wrong answer
    but an EMPTY one: a shape change upstream makes the pattern match nothing, and a guard that
    compares two empty sets passes while checking nothing. These pin the shapes.
    """

    def test_monitor_type_values_reads_constant_and_wire_value(self):
        src = '''const (
\tMonitorHTTP MonitorType = "http"
\tMonitorAsyncCanary MonitorType = "async_canary"
)'''
        self.assertEqual(cdr.monitor_type_values(src),
                         {"MonitorHTTP": "http", "MonitorAsyncCanary": "async_canary"})

    def test_monitor_type_values_finds_the_real_constants(self):
        vals = cdr.monitor_type_values()
        self.assertIn("http", vals.values())
        self.assertGreater(len(vals), 10, "an empty or tiny parse means the constant shape moved")

    def test_setnull_migrations_are_found_and_numbered(self):
        mig = cdr.setnull_migrations()
        self.assertTrue(mig, "no migration matched — the guard would compare nothing")
        self.assertTrue(all(m.isdigit() and len(m) == 5 for m in mig), mig)

    def test_mac_supported_types_are_wire_values_not_constants(self):
        types = cdr.mac_supported_types()
        self.assertTrue(types)
        self.assertIn("http", types)
        self.assertNotIn("MonitorHTTP", types)

    def test_mac_supported_types_returns_none_when_the_map_is_gone(self):
        self.assertIsNone(cdr.mac_supported_types("package fileprovider\n"))

    def test_flatten_joins_a_wrapped_go_comment(self):
        self.assertEqual(cdr.flatten("// six migrations use\n\t// the form (00070, 00093)"),
                         "// six migrations use the form (00070, 00093)")

    def test_the_repository_itself_satisfies_every_enumeration(self):
        self.assertEqual(cdr.check_enumerations(), [])
