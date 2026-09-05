"""Fixture tests for the FR-024 stale-spelling guard in check-docs-references.py.

A guard that is not itself tested is a sentence about a guard. Each case below is a shape one review
round actually found, or the hole the guard's own first draft had.
"""
import ast
import contextlib
import glob
import io
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


class CheckTypeLabelMap(unittest.TestCase):
    """The bridge between the README's display names and the MonitorType values (reviewer [206])."""

    def test_the_map_covers_exactly_the_monitor_types(self):
        self.assertEqual(set(cdr.CHECK_TYPE_LABELS), set(cdr.monitor_type_values().values()))

    def test_every_label_is_non_empty_and_distinct(self):
        labels = list(cdr.CHECK_TYPE_LABELS.values())
        self.assertTrue(all(labels))
        self.assertEqual(len(labels), len(set(labels)),
                         "two types sharing a label make an exactly-once count meaningless")


class PartialResidualClaims(unittest.TestCase):
    """A document may not announce a residual its discharge map does not have (reviewer [206]).

    The sentence that shipped twice contains the word "no" about something else, so these cases
    pin that the guard enumerates POSITIVE phrasings rather than looking for a missing negation.
    """

    def claimed(self, line):
        return any(r.search(line) for r in cdr.PARTIAL_CLAIM_RES)

    def test_the_two_phrasings_that_actually_shipped_are_claims(self):
        self.assertTrue(self.claimed("where **one row is `PARTIAL` and names what is missing**."))
        self.assertTrue(self.claimed("one traceability row is `PARTIAL` and names what it lacks"))
        self.assertTrue(self.claimed("The remaining `PARTIAL` is invariant 13's AUDIT-TRAIL clause"))

    def test_a_negation_is_not_a_claim(self):
        self.assertFalse(self.claimed("where **no row is `PARTIAL` any more**."))
        self.assertFalse(self.claimed("Nothing in the FR-029 map is `PARTIAL` now."))
        self.assertFalse(self.claimed("both former residuals closed, so no FR-029 row is `PARTIAL`"))

    def test_a_sentence_negating_something_else_is_still_a_claim(self):
        self.assertTrue(
            self.claimed("The remaining `PARTIAL` is the audit clause: a write leaves no row today"),
            "a negation about an unrelated noun must not excuse the claim")

    def test_partial_rows_counts_rows_and_not_prose(self):
        rows = cdr.traceability_partial_rows()
        self.assertIn("FR-029", rows)
        self.assertEqual(rows["FR-029"][0], 0,
                         "the FR-029 preamble denies a PARTIAL row; counting the word would invert it")
        self.assertIn("func-async-canary.md", rows["FR-029"][1])

    def test_the_repository_announces_no_phantom_residual(self):
        self.assertEqual(cdr.check_partial_claims(), [])


class CheckTypeListComparison(unittest.TestCase):
    """The same-count mutations as FIXTURES, not edits to the working tree (reviewer P2 at [208]).

    The earlier evidence for these was a harness that rewrote README.md and restored it. That
    proves the guard works on one tree at one moment; it is not a test, and describing it as one
    was the gap. A tiny fixture set stands in for the real 17 types so the cases stay readable.
    """

    WIRE = {"http", "grpc", "ssh", "tcp"}
    LABELS = {"http": "HTTP", "grpc": "gRPC", "ssh": "SSH", "tcp": "TCP"}
    GOOD = "HTTP, TCP, gRPC and SSH"

    def find(self, count, listed, labels=None):
        return cdr.check_type_list_findings(count, listed, self.WIRE, labels or self.LABELS)

    def test_a_correct_list_is_silent(self):
        self.assertEqual(self.find(4, self.GOOD), [])

    def test_same_count_substitution_is_rejected(self):
        out = self.find(4, "HTTP, TCP, GraphQL and SSH")
        self.assertTrue(any("`grpc`" in m and "found 0" in m for m in out), out)

    def test_same_count_duplication_is_rejected(self):
        out = self.find(4, "HTTP, TCP, gRPC and TCP")
        self.assertTrue(any("`ssh`" in m and "found 0" in m for m in out), out)
        self.assertTrue(any("`tcp`" in m and "found 2" in m for m in out), out)

    def test_a_mention_inside_a_parenthetical_is_not_a_second_entry(self):
        self.assertEqual(self.find(4, "HTTP, TCP, gRPC and SSH (tunnels HTTP too)"), [],
                         "prose about one type names others; that is not an entry")

    def test_a_wrong_count_beside_a_correct_list_is_rejected(self):
        self.assertTrue(any("says 3 check types" in m for m in self.find(3, self.GOOD)))

    def test_a_type_with_no_label_is_rejected(self):
        out = self.find(4, self.GOOD, {"http": "HTTP", "grpc": "gRPC", "ssh": "SSH"})
        self.assertTrue(any("`tcp`" in m and "no entry in CHECK_TYPE_LABELS" in m for m in out), out)

    def test_a_label_for_a_type_that_does_not_exist_is_rejected(self):
        out = self.find(4, self.GOOD + " and Gopher", dict(self.LABELS, gopher="Gopher"))
        self.assertTrue(any("`gopher`" in m and "not a MonitorType" in m for m in out), out)

    def test_the_real_readme_and_the_real_constants_agree(self):
        self.assertEqual([m for (_, _, k, m) in cdr.check_enumerations() if k == "enum"], [])


class FR032DischargeCitation(unittest.TestCase):
    """§17.3's citation and BOTH of §17.2's counts, against the artefacts they summarise.

    Every case below calls the PRODUCTION function `cdr.check_fr032_discharge`. My first version
    re-implemented its regexes in the test, so deleting the guard would have left six of seven cases
    passing — reviewer [321]. A test that models a mechanism instead of invoking it is evidence of
    nothing, and `TestBreakingTheGuardBreaksTheseTests` now proves these reach it.
    """

    ROW = ("| 13a | A | behavioural + **source scan** | **DISCHARGED** | "
           "{t} tests, {m} mutations killed (§17.3) |")

    def body(self, cited, row_tests, row_muts, mutation_items):
        """A minimal spec body in the shape the guard reads."""
        return ("### 17.3 fixture\n"
                + "\n".join(f"`{c}`" for c in cited) + "\n\n"
                + "\n".join(f"{i + 1}. mutation {i + 1}" for i in range(mutation_items)) + "\n\n"
                + "### 17.4 next\n" + self.ROW.format(t=row_tests, m=row_muts) + "\n")

    def source(self, declared):
        return "package store\n" + "\n".join(f"func {d}(t *testing.T) {{}}" for d in declared)

    def find(self, cited, declared, row_tests, row_muts, mutation_items):
        return cdr.check_fr032_discharge(self.body(cited, row_tests, row_muts, mutation_items),
                                        self.source(declared))

    def test_an_agreeing_map_is_silent(self):
        self.assertEqual(self.find(["TestA", "TestB"], ["TestA", "TestB"], 2, 3, 3), [])

    def test_an_uncited_declaration_is_reported(self):
        got = self.find(["TestA"], ["TestA", "TestB"], 2, 3, 3)
        self.assertTrue(any("declares TestB" in m for m in got), got)

    def test_a_citation_with_no_declaration_is_reported(self):
        got = self.find(["TestA", "TestGhost"], ["TestA"], 1, 3, 3)
        self.assertTrue(any("cites TestGhost" in m for m in got), got)

    def test_a_stale_test_count_is_reported(self):
        got = self.find(["TestA", "TestB"], ["TestA", "TestB"], 9, 3, 3)
        self.assertTrue(any("says 9 tests" in m for m in got), got)

    # The case [318] found: the first guard checked the test count and left this one free.
    def test_a_stale_mutation_count_is_reported(self):
        got = self.find(["TestA"], ["TestA"], 1, 8, 3)
        self.assertTrue(any("says 8 mutations" in m for m in got), got)

    def test_both_counts_can_be_wrong_at_once(self):
        self.assertEqual(len(self.find(["TestA"], ["TestA"], 5, 8, 3)), 2)

    # The degenerate case: no list at all means the row's count is compared against nothing.
    def test_a_missing_mutation_list_is_reported(self):
        got = self.find(["TestA"], ["TestA"], 1, 7, 0)
        self.assertTrue(any("checked against nothing" in m for m in got), got)

    # Proof that these cases reach the production guard rather than a copy of it: with the guard
    # neutralised, the cases above cannot fail, so at least one must notice.
    # A missing section used to return silently, so deleting §17.3 or §17.5 wholesale left the row
    # still advertising "N tests, M mutations killed" and the guard reporting nothing. The absence
    # is innocent only while nothing claims to be discharged by it.
    def test_a_row_claiming_counts_for_a_section_that_does_not_exist_is_reported(self):
        got = cdr.check_fr032_discharge(self.ROW.format(t=5, m=8), self.source(["TestA"]))
        self.assertTrue(any("which is not in the document" in m for m in got), got)

    def test_a_missing_section_with_no_row_claiming_it_stays_silent(self):
        self.assertEqual(cdr.check_fr032_discharge("nothing here", self.source(["TestA"])), [])

    def test_the_guard_reads_a_second_section_and_row_when_asked(self):
        """10j's discharge reuses this one mechanism rather than a copy of it."""
        body = ("### 17.5 fixture\n`TestOnly`\n\n1. one\n2. two\n\n## 18 next\n"
                + "| 10j | B1 | migration | **DISCHARGED** | 1 tests, 9 mutations killed |\n")
        got = cdr.check_fr032_discharge(body, self.source(["TestOnly"]),
                                        section="### 17.5", ends="## 18", row="10j")
        self.assertTrue(any("10j row says 9 mutations; \u00a717.5 enumerates 2" in m for m in got), got)

    def test_breaking_the_guard_breaks_these_tests(self):
        real = cdr.check_fr032_discharge
        try:
            cdr.check_fr032_discharge = lambda *a, **k: []
            self.assertEqual(self.find(["TestA"], ["TestA"], 9, 9, 3), [],
                             "a neutralised guard must report nothing — this documents the shape")
        finally:
            cdr.check_fr032_discharge = real
        self.assertTrue(self.find(["TestA"], ["TestA"], 9, 9, 3),
                        "with the real guard restored the same input must be reported")

    def test_the_repository_itself_agrees(self):
        self.assertEqual(cdr.check_fr032_discharge(
            cdr.read("docs/specs/func-expected-run-ledger.md"),
            cdr.read("internal/store/revisiontimeline_internal_test.go")), [])


class FR032AuditTotals(unittest.TestCase):
    """§17.2's stated totals against the rows they summarise.

    The section read "65 invariants ... 30 to specify" beside a table already holding 66 rows and
    28, through 13a's addition and §17.4's specifications: a total typed next to its own table is
    the one number nobody re-derives. Every case here calls the production
    `cdr.check_fr032_audit_totals`, and `test_breaking_the_guard_breaks_these_tests` proves it.
    """

    # 7 invariants — 1 covered, 1 specified, 1 discharged, 1 withdrawn, 3 to specify (2 in B2, 1 in C).
    ROWS = ("| 1 | B2 | behavioural | **TO SPECIFY** | note |\n"
            "| 2 | B2 | behavioural | **TO SPECIFY** | note |\n"
            "| 3 | C | schema | **TO SPECIFY** | note |\n"
            "| 4 | A | behavioural | **covered** | note |\n"
            "| 5 | A | source scan | **SPECIFIED (§17.4)** | note |\n"
            "| 6 | A | behavioural | **DISCHARGED** | note |\n"
            "| 7 | B1 | — | **n/a, a withdrawal** | note |\n")

    HEADING = ("The discharge audit — 7 invariants: 1 covered, 1 specified, 1 discharged, "
               "1 withdrawn, 3 to specify")
    RESULT = ("7 invariants — 1 covered, 1 SPECIFIED by §17.4, 1 DISCHARGED by phase A, "
              "1 withdrawn, 3 to specify (2 in B2, 1 in C).")

    def find(self, heading=None, result=None, rows=None, prose="", bold=True):
        """A minimal §17.2 in the shape the guard reads — then the guard itself, not a model of it."""
        span = ("**Result: %s**" if bold else "Result: %s") % (self.RESULT if result is None else result)
        body = ("### 17.2 " + (self.HEADING if heading is None else heading) + "\n\n"
                + "it asks one question of each invariant.\n\n" + span + "\n" + prose + "\n\n"
                + "| # | Phase | Kind | Status | Note |\n| --- | --- | --- | --- | --- |\n"
                + (self.ROWS if rows is None else rows) + "\n### 17.3 next\n")
        return cdr.check_fr032_audit_totals(body, "fixture.md")

    def test_totals_that_match_their_table_are_silent(self):
        self.assertEqual(self.find(), [])

    def test_a_stale_total_in_the_result_span_is_reported(self):
        got = self.find(result=self.RESULT.replace("7 invariants", "6 invariants"))
        self.assertTrue(any("**Result:** span says 6 invariants; its table holds 7" in m for m in got), got)

    def test_a_stale_total_in_the_heading_is_reported(self):
        got = self.find(heading=self.HEADING.replace("7 invariants", "6 invariants"))
        self.assertTrue(any("heading says 6 invariants; its table holds 7" in m for m in got), got)

    # The heading and the Result span are checked SEPARATELY. Requiring each tally merely
    # "somewhere in §17.2" let one region lose a number while the other still satisfied the guard.
    def test_a_tally_dropped_from_only_one_region_is_reported(self):
        got = self.find(result=self.RESULT.replace(", 1 withdrawn", ""))
        self.assertTrue(any('**Result:** span no longer states' in m and 'withdrawn' in m for m in got), got)
        self.assertFalse(any('heading no longer states' in m and 'withdrawn' in m for m in got), got)

    def test_the_two_regions_disagreeing_with_each_other_is_reported(self):
        got = self.find(heading=self.HEADING.replace("1 covered", "2 covered"))
        self.assertTrue(any("heading says 2 covered; its table holds 1" in m for m in got), got)

    def test_a_wrong_phase_breakdown_is_reported(self):
        got = self.find(result=self.RESULT.replace("(2 in B2, 1 in C)", "(1 in B2, 1 in C)"))
        self.assertTrue(any('breaks "to specify" down as' in m for m in got), got)

    def test_a_phase_missing_from_the_breakdown_is_reported(self):
        got = self.find(result=self.RESULT.replace("(2 in B2, 1 in C)", "(2 in B2)"))
        self.assertTrue(any('breaks "to specify" down as' in m for m in got), got)

    def test_a_row_whose_status_is_unreadable_is_reported_by_id(self):
        got = self.find(rows=self.ROWS.replace("| 5 | A | source scan | **SPECIFIED (§17.4)** |",
                                               "| 5 | A | source scan | **SPECIFED (§17.4)** |"))
        self.assertTrue(any("row(s) 5 carry no status" in m for m in got), got)

    def test_removing_the_result_span_is_reported(self):
        got = self.find(bold=False)
        self.assertTrue(any("no bold **Result: ...** span" in m for m in got), got)

    # The case that made the guard scan a DECLARED region instead of all of §17.2. This spec keeps
    # every rejection, so its prose quotes the superseded totals on purpose; a guard reading all the
    # prose reported the very drift the paragraph was explaining.
    def test_prose_quoting_the_superseded_totals_does_not_trip_the_guard(self):
        self.assertEqual(self.find(prose='\nthey read "65 invariants ... 30 to specify" while the '
                                         'table already held 66 rows and 28.\n'), [])

    def test_breaking_the_guard_breaks_these_tests(self):
        real = cdr.check_fr032_audit_totals
        try:
            cdr.check_fr032_audit_totals = lambda *a, **k: []
            self.assertEqual(self.find(bold=False), [],
                             "a neutralised guard must report nothing — this documents the shape")
        finally:
            cdr.check_fr032_audit_totals = real
        self.assertTrue(self.find(bold=False),
                        "with the real guard restored the same input must be reported")

    def test_the_repository_itself_agrees(self):
        self.assertEqual(cdr.check_fr032_audit_totals(
            cdr.read("docs/specs/func-expected-run-ledger.md")), [])


class TheSuiteRunsWhollyHoweverItIsInvoked(unittest.TestCase):
    """`unittest.main()` sat in the MIDDLE of this file, with seven classes defined after it.

    `python3 -m unittest scripts/check_docs_references_test.py` imports the module and collected
    all 74; `python3 scripts/check_docs_references_test.py` executed the guard first, exited before
    those classes were even defined, and reported `Ran 26 tests ... OK`. A third of the suite,
    including BOTH FR-032 guard classes, and green either way. Moving the block is not the fix —
    appending a class after it is the natural next edit, so the shape is asserted instead.
    """

    def test_nothing_is_defined_after_the_main_guard(self):
        source = pathlib.Path(__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        guards = [n for n in tree.body
                  if isinstance(n, ast.If) and "__main__" in ast.dump(n.test)]
        self.assertEqual(len(guards), 1, "exactly one __main__ guard is expected")
        after = [n for n in tree.body
                 if isinstance(n, (ast.ClassDef, ast.FunctionDef)) and n.lineno > guards[0].lineno]
        self.assertEqual([n.name for n in after], [],
                         "these are defined after `unittest.main()`, so running this file as a "
                         "script exits before they exist and reports a green partial suite")


class FR032DrainSurfaces(unittest.TestCase):
    """§13.0's rollback-and-drain rule against the surfaces and terminal paths in the tree.

    The approved paragraph named `pull_jobs` TTL expiry as "the" drain and generalized a job-shaped
    sentence to all "in-flight V4 work", while V4-capable `pull_tests` rows exist too and each
    surface has TWO terminal paths (reviewer [387]). Every case calls the production
    `cdr.check_fr032_drain_surfaces`; the last one proves they reach it.
    """

    MIGS = {"00101.sql": (
        "ALTER TABLE pull_jobs ADD CONSTRAINT pull_jobs_protocol_version_check "
        "CHECK (protocol_version IN (1, 2, 3, 4));\n"
        "ALTER TABLE pull_tests ADD CONSTRAINT pull_tests_protocol_version_check "
        "CHECK (protocol_version IN (1, 2, 3, 4));\n"
        "-- +goose Down\n"
        "ALTER TABLE pull_jobs ADD CONSTRAINT pull_jobs_protocol_version_check "
        "CHECK (protocol_version IN (1, 2, 3));\n"
        "ALTER TABLE pull_tests ADD CONSTRAINT pull_tests_protocol_version_check "
        "CHECK (protocol_version IN (1, 2, 3));\n")}

    STORES = {"pull.go": (
        "func (s *Store) AckPullJobs() {\n_ = `DELETE FROM pull_jobs WHERE claim_token = x`\n}\n"
        "func (s *Store) PurgeExpiredPullJobs() {\n_ = `DELETE FROM pull_jobs WHERE expires_at`\n}\n"
        "func (s *Store) GetPullTestResult() {\n_ = `DELETE FROM pull_tests WHERE result`\n}\n"
        "func (s *Store) PurgeExpiredPullTests() {\n_ = `DELETE FROM pull_tests WHERE expires_at`\n}\n")}

    GOOD = ("**Rollback and drain.** `pull_jobs` terminates by `AckPullJobs` or "
            "`PurgeExpiredPullJobs`; `pull_tests` by `GetPullTestResult` or "
            "`PurgeExpiredPullTests`.\n\n### 13.1 next\n")

    def find(self, para=None, migs=None, stores=None):
        return cdr.check_fr032_drain_surfaces(para if para is not None else self.GOOD,
                                              migs or self.MIGS, stores or self.STORES, "fixture.md")

    def test_a_rule_naming_every_surface_and_path_is_silent(self):
        self.assertEqual(self.find(), [])

    def test_a_surface_left_out_entirely_is_reported(self):
        got = self.find(self.GOOD.replace("`pull_tests` by `GetPullTestResult` or "
                                          "`PurgeExpiredPullTests`.", "."))
        self.assertTrue(any("never names `pull_tests`" in m for m in got), got)

    # The defect [387] found: expiry standing in for the whole drain.
    def test_expiry_as_the_only_route_is_reported(self):
        got = self.find(self.GOOD.replace("`AckPullJobs` or ", ""))
        self.assertTrue(any("does not cite `AckPullJobs`" in m for m in got), got)

    def test_a_consuming_read_left_uncited_is_reported(self):
        got = self.find(self.GOOD.replace("`GetPullTestResult`", "the caller"))
        self.assertTrue(any("does not cite `GetPullTestResult`" in m for m in got), got)

    def test_deleting_the_paragraph_is_reported(self):
        got = self.find("### 13.1 nothing here\n")
        self.assertTrue(any("no longer carries" in m for m in got), got)

    def _with_probe_surface(self, versions):
        migs = dict(self.MIGS)
        migs["00102.sql"] = ("ALTER TABLE pull_probes ADD CONSTRAINT "
                             f"pull_probes_protocol_version_check CHECK (protocol_version IN ({versions}));")
        stores = dict(self.STORES)
        stores["probe.go"] = "func (s *Store) PurgePullProbes() {\n_ = `DELETE FROM pull_probes WHERE x`\n}\n"
        return migs, stores

    # The set is DERIVED, not hardcoded: a third surface that can hold a V4 row must be demanded.
    def test_a_new_surface_admitting_generation_4_is_reported_until_the_rule_names_it(self):
        migs, stores = self._with_probe_surface("1, 4")
        got = self.find(migs=migs, stores=stores)
        self.assertTrue(any("never names `pull_probes`" in m for m in got), got)

    # The pair the reviewer required at [390]. Carrying a protocol_version CHECK is NOT the same as
    # being V4-capable, and this guard's first version conflated them — its own "third surface"
    # mutation added `IN (1)` and called it V4-capable, so the evidence rested on a false premise.
    def test_a_new_surface_capped_below_4_is_NOT_required(self):
        migs, stores = self._with_probe_surface("1, 2, 3")
        self.assertEqual(self.find(migs=migs, stores=stores), [])

    # Reviewer [402]: order by PARSED NUMERIC version, not path. With 9 and 10 the two disagree —
    # lexically "10" sorts before "9", so a lexical scan would let version 9's definition win.
    def test_the_later_numeric_version_wins_even_unpadded(self):
        migs = dict(self.MIGS)
        migs["9_widen.sql"] = ("ALTER TABLE pull_probes ADD CONSTRAINT "
                               "pull_probes_protocol_version_check CHECK (protocol_version IN (1, 4));")
        migs["10_narrow.sql"] = ("ALTER TABLE pull_probes ADD CONSTRAINT "
                                 "pull_probes_protocol_version_check CHECK (protocol_version IN (1, 2, 3));")
        stores = {**self.STORES,
                  "probe.go": "func (s *Store) PurgePullProbes() {\n_ = `DELETE FROM pull_probes WHERE x`\n}\n"}
        # Version 10 narrows below 4, so pull_probes is NOT a V4 surface and §13.0 must not be
        # required to name it. Lexical order would read version 9 last and demand it.
        self.assertEqual(self.find(migs=migs, stores=stores), [])

    def test_the_later_numeric_version_wins_when_it_widens(self):
        """The same ordering in the other direction, so the fixture cannot pass by always-silence."""
        migs = dict(self.MIGS)
        migs["9_narrow.sql"] = ("ALTER TABLE pull_probes ADD CONSTRAINT "
                                "pull_probes_protocol_version_check CHECK (protocol_version IN (1, 2, 3));")
        migs["10_widen.sql"] = ("ALTER TABLE pull_probes ADD CONSTRAINT "
                                "pull_probes_protocol_version_check CHECK (protocol_version IN (1, 4));")
        stores = {**self.STORES,
                  "probe.go": "func (s *Store) PurgePullProbes() {\n_ = `DELETE FROM pull_probes WHERE x`\n}\n"}
        got = self.find(migs=migs, stores=stores)
        self.assertTrue(any("never names `pull_probes`" in m for m in got), got)

    # 00101's own Down narrows both CHECKs back to 3. Reading Down halves empties the derived set,
    # and an empty set would pass on ANY wording — so the emptiness must itself be reported.
    def test_reading_the_down_half_empties_the_set_and_is_reported(self):
        migs = {p: t.replace("-- +goose Down", "-- (not a down marker)")
                for p, t in self.MIGS.items()}
        got = self.find(migs=migs)
        self.assertTrue(any("checked against an empty set" in m for m in got), got)

    def test_a_surface_whose_column_is_later_dropped_leaves_the_set(self):
        migs, stores = self._with_probe_surface("1, 4")
        migs["00103.sql"] = "ALTER TABLE pull_probes DROP COLUMN IF EXISTS protocol_version;"
        self.assertEqual(self.find(migs=migs, stores=stores), [])

    def test_a_surface_no_function_deletes_from_is_reported(self):
        """Otherwise the guard would pass on any wording for that surface."""
        got = self.find(self.GOOD, stores={"empty.go": "package store\n"})
        self.assertTrue(any("no store function deletes from" in m for m in got), got)

    def test_no_generation_4_constraint_in_the_migrations_is_reported(self):
        got = self.find(migs={"none.sql": "SELECT 1;"})
        self.assertTrue(any("checked against an empty set" in m for m in got), got)

    # TRUNCATE is scanned too, with ONE allowlisted exclusion: the exact (*Store).TruncateAll in
    # internal/store/store.go. "The scan pattern happens not to match it" is not a decision
    # (reviewer [392]).
    ALLOWED = {"internal/store/store.go":
               "package store\nfunc (s *Store) TruncateAll() {\n_ = `TRUNCATE pull_jobs, pull_tests`\n}\n"}

    def test_the_allowlisted_test_helper_is_not_a_terminal_path(self):
        self.assertEqual(self.find(stores={**self.STORES, **self.ALLOWED}), [])

    def test_an_operational_truncate_elsewhere_is_reported(self):
        got = self.find(stores={**self.STORES, **self.ALLOWED,
                                "internal/store/reaper.go":
                                "package store\nfunc (s *Store) ReapRegion() {\n_ = `TRUNCATE pull_jobs`\n}\n"})
        self.assertTrue(any("ReapRegion TRUNCATEs `pull_jobs`" in m for m in got), got)

    # The allowlist is the exact receiver AND function, not the file.
    def test_another_function_in_the_same_file_truncating_a_surface_is_reported(self):
        allowed = dict(self.ALLOWED)
        allowed["internal/store/store.go"] += ("func (s *Store) WipePull() {\n"
                                               "_ = `TRUNCATE pull_tests`\n}\n")
        got = self.find(stores={**self.STORES, **allowed})
        self.assertTrue(any("WipePull TRUNCATEs `pull_tests`" in m for m in got), got)

    def test_the_same_helper_name_on_another_receiver_is_reported(self):
        got = self.find(stores={**self.STORES, **self.ALLOWED,
                                "internal/store/other.go":
                                "package store\nfunc (h *Helper) TruncateAll() {\n_ = `TRUNCATE pull_jobs`\n}\n"})
        self.assertTrue(any("(*Helper).TruncateAll" in m for m in got), got)

    # Reviewer [401]: `endswith` allowlisted any prefix carrying that tail. Only the literal
    # repo-relative path is silent; a suffix collision is reported.
    def test_a_suffix_colliding_path_is_not_allowlisted(self):
        got = self.find(stores={**self.STORES, **self.ALLOWED,
                                "other/internal/store/store.go":
                                "package store\nfunc (s *Store) TruncateAll() {\n_ = `TRUNCATE pull_jobs`\n}\n"})
        self.assertTrue(any("other/internal/store/store.go" in m for m in got), got)
        self.assertFalse(any(m.startswith("internal/store/store.go") for m in got),
                         f"the literal allowlisted path must stay silent: {got}")

    # Reviewer [407]: `.lstrip("./")` strips a character SET, not a prefix, so a traversal path
    # collapsed to the allowlisted literal and walked past the check.
    def test_a_traversal_path_is_not_allowlisted(self):
        got = self.find(stores={**self.STORES, **self.ALLOWED,
                                "../internal/store/store.go":
                                "package store\nfunc (s *Store) TruncateAll() {\n_ = `TRUNCATE pull_jobs`\n}\n"})
        self.assertTrue(any("../internal/store/store.go" in m for m in got), got)
        self.assertFalse(any(m.startswith("internal/store/store.go") for m in got),
                         f"the literal allowlisted path must stay silent: {got}")

    def test_the_allowlisted_path_is_recognised_when_written_with_a_dot_prefix(self):
        """Normalized comparison: `./internal/store/store.go` is the same file."""
        self.assertEqual(self.find(stores={**self.STORES,
                                           "./internal/store/store.go": self.ALLOWED["internal/store/store.go"]}), [])

    def test_a_truncate_of_a_table_that_cannot_hold_v4_is_ignored(self):
        self.assertEqual(self.find(stores={**self.STORES, **self.ALLOWED,
                                           "internal/store/x.go":
                                           "package store\nfunc (s *Store) Wipe() {\n_ = `TRUNCATE sessions, users`\n}\n"}), [])

    def test_breaking_the_guard_breaks_these_tests(self):
        real = cdr.check_fr032_drain_surfaces
        try:
            cdr.check_fr032_drain_surfaces = lambda *a, **k: []
            self.assertEqual(self.find("### 13.1 nothing here\n"), [],
                             "a neutralised guard must report nothing — this documents the shape")
        finally:
            cdr.check_fr032_drain_surfaces = real
        self.assertTrue(self.find("### 13.1 nothing here\n"),
                        "with the real guard restored the same input must be reported")

    def test_the_repository_itself_agrees(self):
        migs = {p: cdr.read(p) for p in sorted(glob.glob("internal/store/migrations/*.sql"))}
        stores = {p: cdr.read(p) for p in sorted(glob.glob("internal/store/*.go"))
                  if not p.endswith("_test.go")}
        self.assertEqual(cdr.check_fr032_drain_surfaces(
            cdr.read("docs/specs/func-expected-run-ledger.md"), migs, stores), [])


class FR032TransportMatrix(unittest.TestCase):
    """§16.1's half-deployment rows must say WHICH transports they apply to.

    They need a WIRE between core and executor, so they cannot arise for a pure in-process
    `role=all` — where the safety proof is the FLAG (invariant 10k), not a withheld announcement
    there is nobody to make. Stated generally the matrix promised a protection absent from the most
    common deployment, the same defect §13.0 had (reviewer [415], confirming [413]).
    """

    ROWS = ("| Transport | Half-deploy rows 2 and 3 | What makes V4 safe there |\n"
            "| --- | --- | --- |\n"
            "| AMQP | apply | its workers announce V4 and `ledger.carrier_enabled` is on |\n"
            "| pull | apply | its agents announce V4 and `ledger.carrier_enabled` is on |\n"
            "| in-process (pure `role=all`) | **impossible** | the FLAG alone: with "
            "`ledger.carrier_enabled` false, invariant **10k** proves the resolved map stamps "
            "nothing above 3 |\n"
            "| `role=all` with `pull.regions` | apply, for those regions only | its agents "
            "announce V4 and `ledger.carrier_enabled` is on; the local executor is excluded |\n")

    def find(self, rows=None):
        body = "### 16.1 fixture\n\nprose\n\n" + (self.ROWS if rows is None else rows) + "\n## 17 next\n"
        return cdr.check_fr032_transport_matrix(body, "fixture.md")

    def test_a_fully_scoped_matrix_is_silent(self):
        self.assertEqual(self.find(), [])

    def test_no_applicability_table_at_all_is_reported(self):
        self.assertEqual(len(self.find("prose only, no table\n")), 1)
        self.assertIn("no transport-applicability table", self.find("prose only, no table\n")[0])

    def test_a_missing_context_is_reported_by_name(self):
        got = self.find(self.ROWS.replace("| in-process (pure `role=all`) | **impossible** | the FLAG alone: with "
                                          "`ledger.carrier_enabled` false, invariant **10k** proves the resolved map "
                                          "stamps nothing above 3 |\n", ""))
        self.assertTrue(any('does not cover "in-process"' in m for m in got), got)

    def test_the_role_all_pull_context_is_required_too(self):
        rows = "\n".join(l for l in self.ROWS.split("\n") if "pull.regions" not in l)
        got = self.find(rows)
        self.assertTrue(any('does not cover "role=all with pull regions"' in m for m in got), got)

    # The finding itself: in-process cannot host a half-deployed cluster.
    def test_in_process_claiming_the_rows_apply_is_reported(self):
        got = self.find(self.ROWS.replace("| **impossible** |", "| apply |"))
        self.assertTrue(any("require a wire" in m for m in got), got)

    def test_the_in_process_row_must_cite_the_flag(self):
        got = self.find(self.ROWS.replace("`ledger.carrier_enabled` false", "the setting false"))
        self.assertTrue(any("does not cite the flag" in m for m in got), got)

    def test_the_in_process_row_must_cite_10k(self):
        got = self.find(self.ROWS.replace("invariant **10k** proves", "it is proven"))
        self.assertTrue(any("does not cite invariant 10k" in m for m in got), got)

    def test_a_wire_transport_marked_impossible_is_reported(self):
        got = self.find(self.ROWS.replace("| AMQP | apply |", "| AMQP | impossible |"))
        self.assertTrue(any("that transport has a wire" in m for m in got), got)

    def test_a_context_with_no_safety_mechanism_is_reported(self):
        got = self.find(self.ROWS.replace(
            "| pull | apply | its agents announce V4 and `ledger.carrier_enabled` is on |",
            "| pull | apply |  |"))
        self.assertTrue(any('context "pull" names no safety mechanism' in m for m in got), got)

    # Reviewer [417]: role=all with pull.regions is itself a WIRE context. It may explain the local
    # exclusion, but only after stating announcement AND gate — a nonempty cell is not that claim.
    def test_a_wire_row_losing_its_announcement_is_reported(self):
        got = self.find(self.ROWS.replace(
            "| `role=all` with `pull.regions` | apply, for those regions only | its agents "
            "announce V4 and `ledger.carrier_enabled` is on; the local executor is excluded |",
            "| `role=all` with `pull.regions` | apply, for those regions only | the pull rule, "
            "not the local one; `ledger.carrier_enabled` gates it |"))
        self.assertTrue(any("does not say WHO announces V4" in m for m in got), got)

    def test_a_wire_row_losing_its_gate_is_reported(self):
        got = self.find(self.ROWS.replace(
            "| `role=all` with `pull.regions` | apply, for those regions only | its agents "
            "announce V4 and `ledger.carrier_enabled` is on; the local executor is excluded |",
            "| `role=all` with `pull.regions` | apply, for those regions only | its agents "
            "announce V4; the local executor is excluded |"))
        self.assertTrue(any("does not name `ledger.carrier_enabled`" in m for m in got), got)

    # The label is not the mechanism: this is the shape my first version passed on.
    def test_the_word_announcement_in_a_label_does_not_satisfy_the_check(self):
        got = self.find(self.ROWS.replace(
            "| AMQP | apply | its workers announce V4 and `ledger.carrier_enabled` is on |",
            "| AMQP | apply | announcement plus the gate: `ledger.carrier_enabled` and a queue "
            "prefix its workers have |"))
        self.assertTrue(any("does not say WHO announces V4" in m for m in got), got)

    # Found by probing rather than asked for: `rows[key] = …` let a second row for the same context
    # overwrite the first, so a duplicate that AGREED would be absorbed unseen.
    def test_a_duplicated_context_is_reported(self):
        got = self.find(self.ROWS + "| AMQP | apply | announcement plus the gate |\n")
        self.assertTrue(any('lists "AMQP" more than once' in m for m in got), got)

    def test_breaking_the_guard_breaks_these_tests(self):
        real = cdr.check_fr032_transport_matrix
        try:
            cdr.check_fr032_transport_matrix = lambda *a, **k: []
            self.assertEqual(self.find("prose only, no table\n"), [],
                             "a neutralised guard must report nothing — this documents the shape")
        finally:
            cdr.check_fr032_transport_matrix = real
        self.assertTrue(self.find("prose only, no table\n"),
                        "with the real guard restored the same input must be reported")

    def test_the_repository_itself_agrees(self):
        self.assertEqual(cdr.check_fr032_transport_matrix(
            cdr.read("docs/specs/func-expected-run-ledger.md")), [])


class FR032CarrierGateContract(unittest.TestCase):
    """"Defaults false" describes ABSENCE. It says nothing about an operator supplying `true`.

    Accepting it lets a B1 binary publish V4 before `DueAt` exists (10h); coercing it to false
    silently is the self-healing AGENTS.md forbids. So the contract states both phases explicitly,
    names the validating owner and the construction path, and this guard requires all of it
    (reviewer [423]).
    """

    TABLE = ("| Phase | `ledger.carrier_enabled` absent or `false` | `ledger.carrier_enabled: true` |\n"
             "| --- | --- | --- |\n"
             "| **B1** | accepted, inert | **REFUSED at startup.** `(*Config).Validate` "
             "(`internal/config/config.go:552`) errors, naming the key |\n"
             "| **B2** | accepted | accepted; the same snapshot reaches selection in the atomic "
             "payload change |\n"
             "\nowned through `internal/cli/cli.go:1067`\n")

    def find(self, body=None):
        return cdr.check_fr032_carrier_gate_contract(self.TABLE if body is None else body, "fixture.md")

    def test_a_complete_contract_is_silent(self):
        self.assertEqual(self.find(), [])

    def test_no_contract_table_is_reported(self):
        got = self.find("`ledger.carrier_enabled` defaults to false.\n")
        self.assertTrue(any("no `ledger.carrier_enabled` phase contract table" in m for m in got), got)

    def test_b1_accepting_true_is_reported(self):
        got = self.find(self.TABLE.replace("**REFUSED at startup.**", "Accepted."))
        self.assertTrue(any("does not say B1 REFUSES" in m for m in got), got)

    def test_a_refusal_with_no_owner_is_reported(self):
        got = self.find(self.TABLE.replace("`(*Config).Validate` (`internal/config/config.go:552`) errors, naming the key",
                                           "the process errors"))
        self.assertTrue(any("names no validating owner" in m for m in got), got)

    def test_b2_untied_from_the_atomic_change_is_reported(self):
        got = self.find(self.TABLE.replace("in the atomic payload change", "when convenient"))
        self.assertTrue(any("does not tie the gate to the ATOMIC" in m for m in got), got)

    def test_a_missing_phase_row_is_reported(self):
        rows = "\n".join(l for l in self.TABLE.split("\n") if not l.startswith("| **B2**"))
        got = self.find(rows)
        self.assertTrue(any("has no B2 row" in m for m in got), got)

    # Citations are checked in the contract's OWN section: scanning the whole document let an
    # unrelated mention of the same path satisfy the check.
    def test_an_uncited_scheduler_path_is_reported(self):
        got = self.find(self.TABLE.replace("owned through `internal/cli/cli.go:1067`", "owned somewhere"))
        self.assertTrue(any("scheduler construction path" in m for m in got), got)

    def test_breaking_the_guard_breaks_these_tests(self):
        real = cdr.check_fr032_carrier_gate_contract
        try:
            cdr.check_fr032_carrier_gate_contract = lambda *a, **k: []
            self.assertEqual(self.find("nothing\n"), [],
                             "a neutralised guard must report nothing — this documents the shape")
        finally:
            cdr.check_fr032_carrier_gate_contract = real
        self.assertTrue(self.find("nothing\n"),
                        "with the real guard restored the same input must be reported")

    def test_the_repository_itself_agrees(self):
        self.assertEqual(cdr.check_fr032_carrier_gate_contract(
            cdr.read("docs/specs/func-expected-run-ledger.md")), [])


class LineCitationsLandOnSomething(unittest.TestCase):
    """The `file.go:NNN` guard, tested on the shapes the drift it was written for actually took.

    FR-032's spec cited thirty-three exact lines; by the time its last phase landed, thirteen of
    them pointed at whitespace, a closing brace or an unrelated statement, and this checker passed
    the whole time because it validated the PATH and never the LINE. The guard is deliberately weak
    — it cannot tell a citation that landed on the WRONG statement from one that landed on the right
    one — so its two positive cases and its two negative ones are asserted separately, or "weak"
    would quietly become "does nothing".
    """

    def run_guard(self, doc_text, code_text):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "internal" / "store").mkdir(parents=True)
            (root / "internal" / "store" / "sample.go").write_text(code_text, encoding="utf-8")
            doc = root / "docs"
            doc.mkdir()
            (doc / "runbook.md").write_text(doc_text, encoding="utf-8")
            cwd = os.getcwd()
            living = cdr.LIVING
            try:
                os.chdir(root)
                cdr.LIVING = ["docs/runbook.md"]
                return [m for (_, _, _, m) in cdr.check_line_citations()]
            finally:
                cdr.LIVING = living
                os.chdir(cwd)

    CODE = "package store\n\nfunc Thing() {\n\treturn\n}\n"

    def test_a_citation_on_a_real_statement_passes(self):
        self.assertEqual(
            self.run_guard("See `internal/store/sample.go:3`.\n", self.CODE), [])

    def test_a_citation_on_a_bare_closing_brace_is_flagged(self):
        found = self.run_guard("See `internal/store/sample.go:5`.\n", self.CODE)
        self.assertEqual(len(found), 1, found)
        self.assertIn("bare delimiter", found[0])

    def test_a_citation_on_a_blank_line_is_flagged(self):
        found = self.run_guard("See `internal/store/sample.go:2`.\n", self.CODE)
        self.assertEqual(len(found), 1, found)

    def test_a_citation_past_end_of_file_is_flagged(self):
        found = self.run_guard("See `internal/store/sample.go:900`.\n", self.CODE)
        self.assertEqual(len(found), 1, found)
        self.assertIn("past end of file", found[0])

    def test_a_span_citation_checks_its_first_line(self):
        self.assertEqual(
            self.run_guard("See `internal/store/sample.go:3-5`.\n", self.CODE), [])
        self.assertEqual(
            len(self.run_guard("See `internal/store/sample.go:5-9`.\n", self.CODE)), 1)

    def test_a_path_that_does_not_exist_is_left_to_the_path_guard(self):
        # Two guards reporting one defect is two lines an operator has to reconcile; the path
        # check above already names a missing file, so this one stays silent about it.
        self.assertEqual(
            self.run_guard("See `internal/store/gone.go:3`.\n", self.CODE), [])


class TheLivingSpecCitesNoDeadLine(unittest.TestCase):
    """The guard applied to the tree, so a future edit that reintroduces the drift fails here."""

    def test_every_line_citation_in_the_living_documents_resolves(self):
        self.assertEqual([m for (_, _, _, m) in cdr.check_line_citations()], [])



class IterationFindingCountTest(unittest.TestCase):
    """`check_iteration_finding_counts` — an iteration's count, derived from its own table.

    The guard exists because `iter-0177`'s count drifted three times in one evening, always in a
    document and never in code: the line that opened the iteration said three, the line that closed
    it said four while leaving the opening clause standing, and the traceability head said three —
    written while the fourth finding was being fixed. `docs-check` was green over all of it, because
    a citation guard validates that what IS written resolves and cannot see a number that has
    stopped agreeing with its table.

    The third case below is the one that keeps the guard usable: a document may quote "three
    findings" about something ELSE — an older iteration, a decision record's history — and the guard
    must not fire on a sentence that does not name this iteration. A guard that cannot be lived with
    gets disabled, which is the failure mode worse than the drift.
    """

    TABLE = (
        "# iter-0177\n\n"
        "| # | Finding | Class | State |\n"
        "| --- | --- | --- | --- |\n"
        "| 0 | the mapping stopped where the work continued | docs | fixed |\n"
        "| 1 | a vocabulary closed in prose and open at the write boundary | prose vs mechanism | fixed |\n"
        "| 2 | ledger_from outranked the verdict in the fill and not the words | prose vs mechanism | fixed |\n"
        "| 3 | a count that named one thing and measured another | naming | fixed |\n"
    )

    def run_guard(self, reader_text, table=None):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "docs" / "iterations").mkdir(parents=True)
            iteration = root / "docs" / "iterations" / "iter-0177.md"
            iteration.write_text(table if table is not None else self.TABLE, encoding="utf-8")
            reader = root / "docs" / "status.md"
            reader.write_text(reader_text, encoding="utf-8")
            cwd = os.getcwd()
            try:
                os.chdir(root)
                return cdr.check_iteration_finding_counts(
                    "docs/iterations/iter-0177.md", ("docs/status.md",))
            finally:
                os.chdir(cwd)

    def test_a_reader_that_agrees_with_the_table_passes(self):
        self.assertEqual(
            self.run_guard("See [iter-0177](iterations/iter-0177.md): four findings, all fixed.\n"),
            [])

    def test_the_digit_form_agrees_too(self):
        self.assertEqual(self.run_guard("iter-0177 — 4 findings, all fixed.\n"), [])

    def test_a_reader_stating_the_stale_count_is_flagged(self):
        found = self.run_guard("iter-0177 / D-0245, three findings, all fixed.\n")
        self.assertEqual(len(found), 1, found)
        self.assertIn("says three findings", found[0])
        self.assertIn("holds 4", found[0])

    def test_the_digit_form_drifts_too(self):
        found = self.run_guard("iter-0177 — 3 findings.\n")
        self.assertEqual(len(found), 1, found)

    def test_a_count_about_something_else_is_left_alone(self):
        # The line does not name this iteration, so it is none of the guard's business: a decision
        # record quoting an older arc, or iter-0165's own sentence about three findings.
        self.assertEqual(
            self.run_guard("another argument for the shared owner these three findings kept "
                           "pointing at.\n"), [])

    def test_the_iterations_own_line_is_checked_as_well(self):
        # The first drift was INSIDE the iteration document, so the guard reads it too rather than
        # trusting the file it derives from.
        table = self.TABLE.replace("# iter-0177\n", "# iter-0177 — three findings\n")
        found = self.run_guard("iter-0177: four findings.\n", table=table)
        self.assertTrue(any("iter-0177.md says three findings" in m for m in found), found)

    def test_a_table_with_no_rows_is_reported_rather_than_passing_silently(self):
        found = self.run_guard("iter-0177: four findings.\n", table="# iter-0177\n\nno table here\n")
        self.assertEqual(len(found), 1, found)
        self.assertIn("no numbered findings table", found[0])

class FindingCountCountsFindings(unittest.TestCase):
    """"N findings TABLE" is a count of tables, and the guard must not read it as a count of
    findings — it reported the very sentence explaining how it derives its number."""

    def _iteration(self, rows, reader_line):
        d = tempfile.mkdtemp()
        it = os.path.join(d, "iter-0999.md")
        pathlib.Path(it).write_text("# iter-0999\n\n" + rows + "\n")
        rd = os.path.join(d, "reader.md")
        pathlib.Path(rd).write_text(reader_line + "\n")
        return cdr.check_iteration_finding_counts(it, (rd,))

    def test_a_findings_table_is_not_a_finding_count(self):
        rows = "| 1 | a |\n| 2 | b |"
        self.assertEqual(
            self._iteration(rows, "iter-0999 keeps one findings table and not two."), [])

    def test_a_real_drift_is_still_caught(self):
        rows = "| 1 | a |\n| 2 | b |"
        msgs = self._iteration(rows, "iter-0999 recorded five findings.")
        self.assertEqual(len(msgs), 1, msgs)
        self.assertIn("five findings", msgs[0])


class EveryGuardIsActuallyReached(unittest.TestCase):
    """The guards are WIRED, not merely defined.

    `check_fr032_drain_surfaces`, `check_fr032_transport_matrix` and
    `check_fr032_carrier_gate_contract` were indented INTO the body of
    `for msg in check_iteration_finding_counts():`, so they ran only when that guard FAILED — that
    is, never, because it was green. Their own fixture tests passed throughout, because a fixture
    test calls the function directly; the success line of the checker named all three by name. The
    same class as a builder that is defined, documented and never called.

    So this asserts EXECUTION rather than definition: every `check_*` in the module is wrapped in a
    proxy that records it and calls through, `check_enumerations()` is run against the real tree,
    and any guard the run never reached is reported by name.
    """

    def test_the_entry_point_reaches_every_guard(self):
        os.chdir(os.path.dirname(HERE))
        names = [n for n in dir(cdr)
                 if n.startswith("check_") and callable(getattr(cdr, n))
                 and n != "check_enumerations"]
        self.assertGreater(len(names), 5, "the scan found almost no guards; it is looking wrong")
        seen = set()
        originals = {}

        def proxy(name, fn):
            def wrapped(*a, **kw):
                seen.add(name)
                return fn(*a, **kw)
            return wrapped

        for n in names:
            originals[n] = getattr(cdr, n)
            setattr(cdr, n, proxy(n, originals[n]))
        # The ENTRY POINT, not one of its halves: `main()` is what `make docs-check` runs, and
        # "wired" means reachable from there. Running `check_enumerations()` alone would have
        # declared nine guards dead that `main` reaches by another route.
        buf = io.StringIO()
        try:
            with contextlib.redirect_stdout(buf):
                cdr.main()
        finally:
            for n, fn in originals.items():
                setattr(cdr, n, fn)

        missing = sorted(set(names) - seen)
        self.assertEqual(
            missing, [],
            "guards defined and never reached by main(): " + ", ".join(missing),
        )


if __name__ == "__main__":
    unittest.main()
