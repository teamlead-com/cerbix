package domain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// D8a / §7 Domain: "the domain test asserts the FORMULA against the constants, not the literal
// 300". A VALUE comparison cannot do that: `const MinSealLag = 300 * time.Second` is equal to
// LateArrivalGrace + CanonicalBucket + 2*CanonicalBucket, so every runtime assertion — including
// TestMinSealLagIsTheFormulaOverTheConstants — passes on the literal mutant (confirmed by
// applying it). The only witness of "derived, not chosen" is the declaration itself, so this test
// reads it from service.go and asserts two things about the initializer expression:
//
//  1. its shape: it references LateArrivalGrace once and CanonicalBucket twice, carries no numeric
//     literal other than the 2 of the headroom term, and no time.* selector — a literal 300, a
//     `5 * time.Minute`, or a formula over other names all fail here;
//  2. its behaviour: evaluated with CanonicalBucket doubled, the SAME expression yields a
//     different number — the floor MOVES when the bucket moves, which is what "derived" means and
//     what no test of the compiled constant can show.
func TestMinSealLagIsDeclaredAsTheFormulaNotALiteral(t *testing.T) {
	expr := constInitializer(t, "service.go", "MinSealLag")

	idents := map[string]int{}
	var lits []string
	var selectors []string
	ast.Inspect(expr, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			selectors = append(selectors, exprString(x))
			return false // do not count time / Second as identifiers of the formula
		case *ast.Ident:
			idents[x.Name]++
		case *ast.BasicLit:
			lits = append(lits, x.Value)
		}
		return true
	})
	if len(selectors) != 0 {
		t.Fatalf("MinSealLag's initializer %q reaches outside the domain's constants (%v); D8a wants it derived from LateArrivalGrace and CanonicalBucket alone", exprString(expr), selectors)
	}
	if idents["LateArrivalGrace"] != 1 || idents["CanonicalBucket"] != 2 || len(idents) != 2 {
		t.Fatalf("MinSealLag's initializer %q references %v; want LateArrivalGrace once and CanonicalBucket twice and nothing else (grace + bucket + 2 × bucket)", exprString(expr), idents)
	}
	if len(lits) != 1 || lits[0] != "2" {
		t.Fatalf("MinSealLag's initializer %q carries literals %v; the only literal the formula has is the 2 of the headroom term — a literal seconds count is exactly what D8a forbids", exprString(expr), lits)
	}

	// The expression, evaluated as written, is the constant the binary carries …
	at := func(grace, bucket time.Duration) time.Duration {
		v, err := evalDurationExpr(expr, map[string]time.Duration{"LateArrivalGrace": grace, "CanonicalBucket": bucket})
		if err != nil {
			t.Fatalf("evaluating %q: %v", exprString(expr), err)
		}
		return v
	}
	if got := at(LateArrivalGrace, CanonicalBucket); got != MinSealLag {
		t.Fatalf("the declared expression evaluates to %s but MinSealLag is %s", got, MinSealLag)
	}
	// … and it MOVES with each constant it is derived from.
	if at(LateArrivalGrace, 2*CanonicalBucket) == at(LateArrivalGrace, CanonicalBucket) {
		t.Fatalf("doubling CanonicalBucket leaves the expression at %s: the floor is not derived from the bucket", MinSealLag)
	}
	if at(2*LateArrivalGrace, CanonicalBucket) == at(LateArrivalGrace, CanonicalBucket) {
		t.Fatalf("doubling LateArrivalGrace leaves the expression at %s: the floor is not derived from the grace", MinSealLag)
	}
	// And with both moved, it is still grace + bucket + 2 × bucket of the NEW values.
	g, b := 3*time.Minute, 2*time.Minute
	if want := g + b + 2*b; at(g, b) != want {
		t.Fatalf("with grace = %s and bucket = %s the expression gives %s, want %s (grace + bucket + 2 × bucket)", g, b, at(g, b), want)
	}
}

// constInitializer returns the initializer expression of the named constant in the package file.
func constInitializer(t *testing.T, file, name string) ast.Expr {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name == name && i < len(vs.Values) {
					return vs.Values[i]
				}
			}
		}
	}
	t.Fatalf("%s is not declared as a const with its own initializer in %s", name, file)
	return nil
}

// evalDurationExpr evaluates a constant expression over +, -, * with the named durations bound
// and integer literals as scalars — enough for D8a's formula and nothing more.
func evalDurationExpr(e ast.Expr, env map[string]time.Duration) (time.Duration, error) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return evalDurationExpr(x.X, env)
	case *ast.Ident:
		v, ok := env[x.Name]
		if !ok {
			return 0, fmt.Errorf("unbound identifier %q", x.Name)
		}
		return v, nil
	case *ast.BasicLit:
		if x.Kind != token.INT {
			return 0, fmt.Errorf("non-integer literal %s", x.Value)
		}
		var n int64
		if _, err := fmt.Sscan(x.Value, &n); err != nil {
			return 0, err
		}
		return time.Duration(n), nil
	case *ast.BinaryExpr:
		l, err := evalDurationExpr(x.X, env)
		if err != nil {
			return 0, err
		}
		r, err := evalDurationExpr(x.Y, env)
		if err != nil {
			return 0, err
		}
		switch x.Op {
		case token.ADD:
			return l + r, nil
		case token.SUB:
			return l - r, nil
		case token.MUL:
			return l * r, nil
		}
		return 0, fmt.Errorf("operator %s", x.Op)
	}
	return 0, fmt.Errorf("unsupported node %T (%s)", e, exprString(e))
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.ParenExpr:
		return "(" + exprString(x.X) + ")"
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.BinaryExpr:
		return exprString(x.X) + " " + x.Op.String() + " " + exprString(x.Y)
	}
	return fmt.Sprintf("%T", e)
}

// D14: a duplicate clause is refused BY NAME even when both copies carry the SAME assignment —
// a "harmless" repeat is still a document that does not mean exactly one thing — and wherever
// the repeat sits. The remaining rows are the boundary values §7/D8a name that the shipped
// table did not already pin: 299 and 301 around the floor, 86401 past the ceiling, and a
// behaviour word outside the pair.
func TestValidateGatePolicyV1RefusesTheRemainingBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*GatePolicyDocument)
		field string
		words []string
	}{
		{"duplicate clause, same assignment, appended", func(d *GatePolicyDocument) {
			d.Clauses = append(d.Clauses, GateClauseEntry{ClauseBudgetConsumed, ClauseAssignWarn}) // template already says warn
		}, "clauses.budget_consumed", []string{"more than once", "budget_consumed"}},
		{"duplicate clause, same assignment, leading", func(d *GatePolicyDocument) {
			d.Clauses = append([]GateClauseEntry{{ClauseBudgetExhausted, ClauseAssignBlock}}, d.Clauses...)
		}, "clauses.budget_exhausted", []string{"more than once", "budget_exhausted"}},
		{"missing clause named when one is swapped for an unknown", func(d *GatePolicyDocument) {
			d.Clauses[4] = GateClauseEntry{"incident_open", ClauseAssignWarn}
		}, "clauses.incident_open", []string{"unknown clause", "incident_open"}},
		{"seal lag 299", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 299 }, "max_seal_lag_seconds", []string{"between 300 and 86400", "got 299"}},
		{"seal lag 301 (in range, not a whole minute)", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 301 }, "max_seal_lag_seconds", []string{"whole number of minutes", "multiple of 60", "got 301"}},
		{"seal lag 86401", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 86401 }, "max_seal_lag_seconds", []string{"between 300 and 86400", "got 86401"}},
		{"seal lag negative", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = -60 }, "max_seal_lag_seconds", []string{"between 300 and 86400", "got -60"}},
		{"unknown_behavior maybe", func(d *GatePolicyDocument) { d.UnknownBehavior = "maybe" }, "unknown_behavior", []string{"warn|block", `"maybe"`}},
		{"unknown_behavior upper-case", func(d *GatePolicyDocument) { d.UnknownBehavior = "WARN" }, "unknown_behavior", []string{"warn|block", `"WARN"`}},
		{"window of spaces", func(d *GatePolicyDocument) { d.Window = "   " }, "window", nil},
		{"threshold negative", func(d *GatePolicyDocument) { d.BudgetConsumedPercent = -1 }, "budget_consumed_percent", []string{"between 1 and 100", "got -1"}},
		{"schema_version 0", func(d *GatePolicyDocument) { d.SchemaVersion = 0 }, "schema_version", []string{"got 0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validGateDoc()
			tc.mut(&d)
			clauses, err := ValidateGatePolicyV1(d)
			if err == nil {
				t.Fatalf("accepted with clauses %v", clauses)
			}
			pe, ok := err.(*GatePolicyError)
			if !ok {
				t.Fatalf("error is %T, want *GatePolicyError: %v", err, err)
			}
			if pe.Field != tc.field {
				t.Fatalf("refusal names field %q, want %q: %v", pe.Field, tc.field, err)
			}
			for _, w := range tc.words {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not say %q", err.Error(), w)
				}
			}
		})
	}
}

// The clause set is checked as a SET: the same five clauses in any order are one policy, and
// the canonical map the validator returns is what a stored `clauses` column holds — so a
// reordered write compares equal to the stored document (the D14 no-op).
func TestValidateGatePolicyV1IsOrderInsensitiveOverTheClauseSet(t *testing.T) {
	d := validGateDoc()
	reversed := make([]GateClauseEntry, len(d.Clauses))
	for i, e := range d.Clauses {
		reversed[len(d.Clauses)-1-i] = e
	}
	d.Clauses = reversed
	got, err := ValidateGatePolicyV1(d)
	if err != nil {
		t.Fatalf("a reordered but complete clause list was refused: %v", err)
	}
	want, _ := ValidateGatePolicyV1(validGateDoc())
	if len(got) != len(want) {
		t.Fatalf("maps differ in size: %d vs %d", len(got), len(want))
	}
	for c, a := range want {
		if got[c] != a {
			t.Fatalf("clause %s: %q vs %q", c, got[c], a)
		}
	}
	// A stored policy built from that map documents itself in vocabulary order.
	p := GatePolicy{SchemaVersion: 1, Window: "30d", Clauses: got, BudgetConsumedPercent: 90, MaxSealLagSeconds: 900, UnknownBehavior: GateUnknownWarn}
	doc := p.Document()
	for i, c := range GateClausesV1 {
		if doc.Clauses[i].Clause != c {
			t.Fatalf("Document() clause %d is %q, want %q (vocabulary order)", i, doc.Clauses[i].Clause, c)
		}
	}
}

// D13a rows 1 and 2, on the row the two mutations of the precedence order touch: a MANUAL
// revocation outranks a revision mismatch (mutating "manual below inert" reads inert here),
// and a recorded system closure is inert even with the revision still matching and the clock
// still ahead (a stored policy_changed can only have come from a policy edit — the fact and
// the record agree).
func TestGateOverrideStatusManualOutranksInertAndSystemClosuresAreInert(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	closed := now.Add(-time.Minute)
	live := int64(3)

	manualMismatched := GateOverride{PolicyRevision: 2, ExpiresAt: now.Add(time.Hour), RevokedAt: &closed, RevokedReason: GateRevokedManual}
	if got := GateOverrideStatusAt(manualMismatched, now, &live); got != GateOverrideRevoked {
		t.Fatalf("manual revocation on a revision-mismatched row: %q, want revoked (row 1 outranks row 2)", got)
	}
	manualTombstoned := manualMismatched
	if got := GateOverrideStatusAt(manualTombstoned, now, nil); got != GateOverrideRevoked {
		t.Fatalf("manual revocation with the policy tombstoned: %q, want revoked", got)
	}
	for _, r := range []GateRevokedReason{GateRevokedPolicyChanged, GateRevokedPolicyDeleted} {
		o := GateOverride{PolicyRevision: live, ExpiresAt: now.Add(time.Hour), RevokedAt: &closed, RevokedReason: r}
		if got := GateOverrideStatusAt(o, now, &live); got != GateOverrideInert {
			t.Fatalf("recorded %s with a matching live revision: %q, want inert", r, got)
		}
		if o.Open() {
			t.Fatalf("a %s-closed row reports Open()", r)
		}
	}
	// Row 3 is reached only once rows 1 and 2 are excluded: a matching, unrevoked row past its
	// expiry is expired, not active, however the store's housekeeping is behind.
	stale := GateOverride{PolicyRevision: live, ExpiresAt: now.Add(-time.Nanosecond)}
	if got := GateOverrideStatusAt(stale, now, &live); got != GateOverrideExpired {
		t.Fatalf("unrevoked row one nanosecond past expiry: %q, want expired", got)
	}
	if !stale.Open() {
		t.Fatal("an expired-but-unclosed row is still Open(): the slot predicate is revoked_at IS NULL, not the clock")
	}
}
