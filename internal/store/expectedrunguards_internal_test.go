package store

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// FR-032 phase B2 — the SOURCE SCANS. The audit (§17.2) classifies seven invariants as properties
// of the code's SHAPE rather than of its behaviour, and the distinction is not pedantry: "exactly
// one statement advances next_due_at" is not observable from any single run, and a behavioural
// test that tried would pass against a tree with two.
//
// The precedent is this repository's own — `internal/api/monitordoors_test.go`,
// `internal/api/incidentdoors_test.go` and `internal/domain/canarybounds_test.go` all parse real
// files, and the last kills source-shape mutations — so each guard sits beside the package it
// guards and no second global mechanism is built.

// Invariant 2a — there is exactly ONE statement that moves an expectation forward.
//
// A second one is the party [229] defect by construction: revision 4 had the issue path and the
// policy-skip path as separate statements, and they diverged on the obligation they shared. The
// advance OVERWRITES the only record of the old expectation, so a second writer that forgot the
// gap rows would destroy the evidence with the very write that ends the gap.
//
// The mutation that must kill this: add any second `UPDATE monitor_schedule ... SET next_due_at`.
// The test names every statement it found, so the failure identifies the newcomer rather than
// reporting a count.
func TestExactlyOneStatementAdvancesTheExpectation(t *testing.T) {
	var advancers []string
	for file, src := range storePackageSources(t) {
		for decl, statements := range sqlStatementsIn(src, "UPDATE monitor_schedule") {
			for _, stmt := range statements {
				if setsColumn(stmt, "next_due_at") {
					advancers = append(advancers, file+":"+decl)
				}
			}
		}
	}
	sort.Strings(advancers)
	want := []string{"expectedruns.go:reserveExpectationsSQL"}
	if strings.Join(advancers, ",") != strings.Join(want, ",") {
		t.Fatalf("the statements that move next_due_at are %v, want exactly %v (§7.1).\n"+
			"Two statements sharing the gap-and-fence obligation will diverge on it, and did — "+
			"party [229]: revision 4's policy-skip path argued its way out of gap materialization "+
			"and lost the gap exactly as [225] had.", advancers, want)
	}
}

// Invariant 25c, refined by implementation — every `INSERT INTO expected_runs` names
// `interval_seconds`, and the SET of inserts naming `carrier_generation` is ENUMERATED.
//
// 25c as written says every write names the carrier, and the tree falsifies it: the never-issued
// window's insert names neither the carrier nor a job, and it is correct not to — §6.1's CHECK is
// a biconditional, so a window with no job must have no carrier. The property that actually
// protects the CHECK is the pairing, and the property that protects the LATENESS THRESHOLD is
// `interval_seconds`, which revision 15 omitted from the prose exactly as revision 7 omitted the
// carrier.
//
// A SET rather than a count, because a same-count edit walks straight through a count.
func TestEveryWindowInsertNamesTheColumnsThatCannotBeDefaulted(t *testing.T) {
	withCarrier := []string{}
	inserts := 0
	for _, src := range storePackageSources(t) {
		for decl, statements := range sqlStatementsIn(src, "INSERT INTO expected_runs") {
			for _, stmt := range statements {
				inserts++
				columns := insertColumnList(stmt)
				if columns == "" {
					t.Errorf("%s: an INSERT INTO expected_runs with no column list at all — the "+
						"column ORDER then decides what a row means", decl)
					continue
				}
				if !strings.Contains(columns, "interval_seconds") {
					t.Errorf("%s: an INSERT INTO expected_runs omits interval_seconds, so the "+
						"window's lateness threshold is a column default: %s", decl, columns)
				}
				if strings.Contains(columns, "carrier_generation") {
					withCarrier = append(withCarrier, decl)
				}
			}
		}
	}
	if inserts < 3 {
		t.Fatalf("the guard found only %d inserts into expected_runs; the tree has three, and a "+
			"guard that stopped seeing its own subject reports green forever", inserts)
	}
	sort.Strings(withCarrier)
	want := []string{"fillExpectedRunTerminalSQL", "reserveExpectationsSQL"}
	if strings.Join(withCarrier, ",") != strings.Join(want, ",") {
		t.Fatalf("the inserts naming carrier_generation are %v, want exactly %v.\n"+
			"A new write that can carry a job must name the carrier or it fails §6.1's CHECK on "+
			"the one case reconciliation exists for (party [235]); a new write that cannot must "+
			"say so by omitting both.", withCarrier, want)
	}
}

// Invariant 7a — EVERY event statement uses §8.1's ONE admissibility predicate, verbatim.
//
// Revision 5 had two event statements with two different guards and the claim one was wrong: it
// admitted `job_id IS NULL`, which is also true of a deliberately SKIPPED window, and carried no
// revision predicate at all — so a late or stale claim could mark a window cerbix chose not to run
// as claimed, breaking invariants 7 and 10b through the SQL that was supposed to uphold them
// (reviewer P1-1 at party [231]). A statement with its own variant of the boundary is that defect
// by construction, which is why this is asserted on the code's SHAPE and not on behaviour: a
// behavioural test can only ever show that the two guards agree TODAY.
//
// The mutation that must kill this: inline the predicate into either event statement, however
// faithfully. The scan reports the statement that stopped calling the shared expression.
func TestEveryEventStatementUsesTheOneAdmissibilityPredicate(t *testing.T) {
	const shared = "expectedRunAdmissibleSQL"
	// The event statements, ENUMERATED. A count would be a proxy a same-count edit walks through,
	// and this set is small and deliberate: §8.1 says "every event", and there are two events that
	// may touch a window they did not create — the claim and the terminal. The REFUSAL is
	// deliberately absent and its absence is asserted below.
	want := []string{"fillExpectedRunTerminalSQL", "recordExpectedRunClaimSQL"}

	var callers []string
	for _, src := range storePackageSources(t) {
		for _, decl := range strings.Split(src, "\nvar ") {
			name := strings.SplitN(decl, " ", 2)[0]
			if !strings.HasSuffix(name, "SQL") || !strings.Contains(decl, shared) {
				continue
			}
			callers = append(callers, name)
		}
	}
	sort.Strings(callers)
	if strings.Join(callers, ",") != strings.Join(want, ",") {
		t.Fatalf("the statements built on %s are %v, want exactly %v.\n"+
			"Two statements sharing an obligation diverge on it — party [231] found exactly that "+
			"between the claim and the terminal, and invariant 7a exists so the boundary has one "+
			"expression rather than two that happen to agree.", shared, callers, want)
	}

	// The REFUSAL statement must NOT use it, and that is not an omission (invariant 10f). Its
	// guard is `job_id = $n` ALONE, without the no-job disjunct: a refusal proves only that
	// something was DELIVERED, and adopting a window would assert that a run happened there.
	for file, src := range storePackageSources(t) {
		for decl, statements := range sqlStatementsIn(src, "UPDATE expected_runs") {
			for _, stmt := range statements {
				if !strings.Contains(stmt, "refused_reason") {
					continue
				}
				if strings.Contains(stmt, "job_id IS NULL") {
					t.Errorf("%s:%s — the refusal statement admits a window with no job. A refusal "+
						"may only ever annotate the row recording its OWN job (invariant 10f); "+
						"adopting one asserts that a run happened where nothing was issued.",
						file, decl)
				}
			}
		}
	}
}

// Invariant 21 — nothing in the ledger is read to decide what to probe. `expected_runs` is
// EVIDENCE, not a job queue, and the distinction is what keeps the residual error pointing at
// withholding: a scheduler that consulted it could be talked into re-probing a window, and a
// window is a record of what already happened.
//
// Scanned in the store because that is where every ledger query lives; the scheduler and prober
// halves are asserted beside those packages. The SQL comments are stripped first, for the reason
// stripSQLComments gives — the table's name appears in the prose beside every statement.
func TestNoStoreReadOfTheLedgerFeedsADispatchDecision(t *testing.T) {
	// The files entitled to name the table, ENUMERATED. A new one is a decision: either the
	// statement belongs beside the ledger's own file, or it is a dispatch path reading evidence.
	allowed := map[string]bool{
		"expectedruns.go":         true, // the primitive, the terminal, the refusal, the claim
		"monitorschedule.go":      true, // §10's configuration boundary
		"monitors.go":             true, // the ingest transaction, which fills a terminal behind its gate
		"expectedrunretention.go": true, // phase D: retention, ledger_from, the paged read
	}
	for name, src := range storePackageSources(t) {
		if allowed[name] {
			continue
		}
		if strings.Contains(stripSQLComments(src), "expected_runs") {
			t.Errorf("%s references expected_runs in SQL. Every ledger statement belongs beside the "+
				"ledger's own file: the reason is invariant 21 — the ledger is evidence and "+
				"nothing reads it to decide what to probe, and a query in a dispatch file is one "+
				"refactor away from being that.\n"+
				"If this file is a NEW ledger surface, add it to the allowlist above with a comment "+
				"saying which part of the spec it implements — the guard's job is to make that a "+
				"decision rather than a diff nobody reads. It caught phase D's own retention file "+
				"on the first full run, which is the outcome it exists for.", name)
		}
	}
}

// storePackageSources reads the package's non-test files. Text rather than AST, because the
// subject is SQL inside string literals and an AST gives no more purchase on it than a reader
// does — while `go/ast` IS used by phase A's guard, where the subject is a Go identifier.
func storePackageSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list store sources: %v", err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files[name] = string(src)
	}
	if len(files) == 0 {
		t.Fatal("no store sources were read; the guard would report green over nothing")
	}
	return files
}

// stripSQLComments removes every `--` comment to end of line.
//
// It runs BEFORE any column is looked for, and that ordering is the whole trick: the identifiers
// these guards are about — `next_due_at`, `carrier_generation` — appear in the comments beside the
// SQL that uses them, deliberately and at length. Phase A's guard learned the same thing from the
// other side and parses Go rather than grepping it: a guard that miscounts its own subject fails
// on a correct tree and gets deleted by whoever is unblocking CI.
func stripSQLComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

var declRe = regexp.MustCompile(`(?m)^(?:var|func)(?:\s+\([^)]*\))?\s+([A-Za-z_][A-Za-z0-9_]*)`)

// sqlStatementsIn returns each SQL statement beginning with the given prefix, labelled by the
// top-level declaration it appears in — so a failure names `advanceExpectationsSQL` rather than a
// file, which is what makes the enumerated SET below legible.
//
// The statement's end is coarse on purpose: the next occurrence of a statement keyword, or the
// literal's closing backtick. A guard that parsed SQL properly would be a second SQL parser to
// maintain, and every property here is answered by "does this statement assign this column".
func sqlStatementsIn(src, prefix string) map[string][]string {
	src = stripSQLComments(src)
	decls := declRe.FindAllStringSubmatchIndex(src, -1)
	declAt := func(offset int) string {
		name := "<file scope>"
		for _, d := range decls {
			if d[0] <= offset {
				name = src[d[2]:d[3]]
			} else {
				break
			}
		}
		return name
	}
	out := map[string][]string{}
	for i := 0; ; {
		j := strings.Index(src[i:], prefix)
		if j < 0 {
			return out
		}
		start := i + j
		end := len(src)
		for _, terminator := range []string{"`", "\nWITH ", "\nINSERT INTO ", "\nUPDATE ", "\nDELETE FROM "} {
			if k := strings.Index(src[start+len(prefix):], terminator); k >= 0 {
				if candidate := start + len(prefix) + k; candidate < end {
					end = candidate
				}
			}
		}
		name := declAt(start)
		out[name] = append(out[name], src[start:end])
		i = start + len(prefix)
	}
}

var setColumnRe = regexp.MustCompile(`(?m)^\s*(?:SET\s+)?([a-z_]+)\s*=`)

// setsColumn reports whether an UPDATE assigns the named column, reading the SET clause's
// left-hand sides rather than searching for the name anywhere in the statement.
func setsColumn(stmt, column string) bool {
	body := stmt
	if i := strings.Index(body, "SET "); i >= 0 {
		body = body[i:]
	}
	if i := strings.Index(body, "\n WHERE"); i >= 0 {
		body = body[:i]
	}
	for _, m := range setColumnRe.FindAllStringSubmatch(body, -1) {
		if m[1] == column {
			return true
		}
	}
	return false
}

// insertColumnList returns the parenthesised column list of an INSERT, flattened.
func insertColumnList(stmt string) string {
	open := strings.Index(stmt, "(")
	if open < 0 {
		return ""
	}
	depth, end := 0, -1
	for i := open; i < len(stmt) && end < 0; i++ {
		switch stmt[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		return ""
	}
	return strings.Join(strings.Fields(stmt[open+1:end]), " ")
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Phase A's AST helpers — referencesIn, insertsMonitor, parseStorePackage, fenceSites — live in
// revisiontimeline_internal_test.go and are reused by the pairing guard in
// monitorschedule_internal_test.go rather than reimplemented. The scheduler-side scans
// (invariants 2d, 20d and 21's dispatch half) sit beside their own package, per party [286].
