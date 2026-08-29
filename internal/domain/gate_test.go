package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// D8a: the floor of max_seal_lag_seconds is DERIVED, not chosen. The assertion is the formula
// over the constants it depends on — all three resolved from this package — and only then the
// number the spec writes beside it, so a change to either constant fails here rather than
// leaving a literal 300 that no longer means anything.
func TestMinSealLagIsTheFormulaOverTheConstants(t *testing.T) {
	if MinSealLag != LateArrivalGrace+CanonicalBucket+2*CanonicalBucket {
		t.Fatalf("MinSealLag = %s, want LateArrivalGrace + CanonicalBucket + 2*CanonicalBucket = %s",
			MinSealLag, LateArrivalGrace+CanonicalBucket+2*CanonicalBucket)
	}
	if MinSealLag != 300*time.Second {
		t.Fatalf("MinSealLag = %s; D8a states 120 + 60 + 120 = 300 s, so a constant moved without the spec", MinSealLag)
	}
	if LateArrivalGrace != 2*time.Minute || CanonicalBucket != time.Minute {
		t.Fatalf("the constants the formula rests on moved: grace=%s bucket=%s", LateArrivalGrace, CanonicalBucket)
	}
	if MaxSealLag != 86400*time.Second {
		t.Fatalf("MaxSealLag = %s, want 86400 s (D14)", MaxSealLag)
	}
	if MinSealLag%GateSealLagStep != 0 || MaxSealLag%GateSealLagStep != 0 {
		t.Fatalf("the bounds themselves must be whole minutes: min=%s max=%s", MinSealLag, MaxSealLag)
	}
}

// D11: the vocabulary is closed, ordered and does NOT contain coverage_not_armed.
func TestGateClausesV1IsClosedAndOrdered(t *testing.T) {
	want := []GateClause{"budget_exhausted", "budget_consumed", "page_burn_firing", "ticket_burn_firing", "service_incident_open"}
	if len(GateClausesV1) != len(want) {
		t.Fatalf("GateClausesV1 has %d clauses, want %d", len(GateClausesV1), len(want))
	}
	for i, c := range want {
		if GateClausesV1[i] != c {
			t.Fatalf("GateClausesV1[%d] = %q, want %q", i, GateClausesV1[i], c)
		}
	}
	for _, c := range GateClausesV1 {
		if c == "coverage_not_armed" {
			t.Fatal("coverage_not_armed is not in the vocabulary (D11); coverage is evidence, never a clause")
		}
	}
	if got := GateClausesFor(GatePolicySchemaV1); len(got) != len(want) {
		t.Fatalf("GateClausesFor(1) = %v", got)
	}
	if got := GateClausesFor(2); got != nil {
		t.Fatalf("GateClausesFor(2) = %v, want nil for an unknown version", got)
	}
	// The returned slice is a copy: a caller cannot mutate the vocabulary.
	got := GateClausesFor(GatePolicySchemaV1)
	got[0] = "tampered"
	if GateClausesV1[0] != ClauseBudgetExhausted {
		t.Fatal("GateClausesFor handed out the vocabulary itself")
	}
}

func validGateDoc() GatePolicyDocument {
	return GatePolicyDocument{
		SchemaVersion: GatePolicySchemaV1,
		Window:        "30d",
		Clauses: []GateClauseEntry{
			{ClauseBudgetExhausted, ClauseAssignBlock},
			{ClauseBudgetConsumed, ClauseAssignWarn},
			{ClausePageBurnFiring, ClauseAssignBlock},
			{ClauseTicketBurnFiring, ClauseAssignWarn},
			{ClauseServiceIncidentOpen, ClauseAssignWarn},
		},
		BudgetConsumedPercent: 90,
		MaxSealLagSeconds:     900,
		UnknownBehavior:       GateUnknownWarn,
	}
}

func TestValidateGatePolicyV1AcceptsTheShippedTemplate(t *testing.T) {
	clauses, err := ValidateGatePolicyV1(validGateDoc())
	if err != nil {
		t.Fatalf("the shipped template was refused: %v", err)
	}
	if len(clauses) != len(GateClausesV1) {
		t.Fatalf("clause map has %d entries, want %d", len(clauses), len(GateClausesV1))
	}
	if clauses[ClauseBudgetExhausted] != ClauseAssignBlock || clauses[ClauseServiceIncidentOpen] != ClauseAssignWarn {
		t.Fatalf("clause map = %v", clauses)
	}
	// The bounds themselves are accepted: a bound is a bound, not a ban.
	for _, lag := range []int{int(MinSealLag / time.Second), int(MaxSealLag / time.Second)} {
		d := validGateDoc()
		d.MaxSealLagSeconds = lag
		if _, err := ValidateGatePolicyV1(d); err != nil {
			t.Errorf("max_seal_lag_seconds = %d (a bound) was refused: %v", lag, err)
		}
	}
	for _, pct := range []int{1, 100} {
		d := validGateDoc()
		d.BudgetConsumedPercent = pct
		if _, err := ValidateGatePolicyV1(d); err != nil {
			t.Errorf("budget_consumed_percent = %d (a bound) was refused: %v", pct, err)
		}
	}
	// A stored policy re-read as a document is a valid document (the D14 no-op comparison).
	p := GatePolicy{SchemaVersion: 1, Window: "30d", Clauses: clauses, BudgetConsumedPercent: 90,
		MaxSealLagSeconds: 900, UnknownBehavior: GateUnknownWarn}
	if _, err := ValidateGatePolicyV1(p.Document()); err != nil {
		t.Fatalf("a stored policy's own document was refused: %v", err)
	}
}

// D11/D14: every refusal names the FIELD and states the range or the rule. Each case mutates
// one thing about the shipped template.
func TestValidateGatePolicyV1RefusesByName(t *testing.T) {
	minLag := int(MinSealLag / time.Second)
	cases := []struct {
		name  string
		mut   func(*GatePolicyDocument)
		field string
		words []string
	}{
		{"unknown schema version", func(d *GatePolicyDocument) { d.SchemaVersion = 2 }, "schema_version", []string{"got 2", "1"}},
		{"empty window", func(d *GatePolicyDocument) { d.Window = "" }, "window", nil},
		{"padded window", func(d *GatePolicyDocument) { d.Window = " 30d" }, "window", nil},
		{"unknown clause", func(d *GatePolicyDocument) {
			d.Clauses = append(d.Clauses, GateClauseEntry{"coverage_not_armed", ClauseAssignBlock})
		}, "clauses.coverage_not_armed", []string{"unknown clause", "coverage_not_armed"}},
		{"missing clause", func(d *GatePolicyDocument) { d.Clauses = d.Clauses[:4] }, "clauses.service_incident_open", []string{"missing", "service_incident_open"}},
		{"duplicate clause", func(d *GatePolicyDocument) {
			d.Clauses = append(d.Clauses, GateClauseEntry{ClauseBudgetConsumed, ClauseAssignIgnore})
		}, "clauses.budget_consumed", []string{"more than once", "budget_consumed"}},
		{"no clauses at all", func(d *GatePolicyDocument) { d.Clauses = nil }, "clauses.budget_exhausted", []string{"missing"}},
		{"bad assignment", func(d *GatePolicyDocument) { d.Clauses[2].Assignment = "maybe" }, "clauses.page_burn_firing", []string{"block|warn|ignore", `"maybe"`}},
		{"threshold below", func(d *GatePolicyDocument) { d.BudgetConsumedPercent = 0 }, "budget_consumed_percent", []string{"between 1 and 100", "got 0"}},
		{"threshold above", func(d *GatePolicyDocument) { d.BudgetConsumedPercent = 101 }, "budget_consumed_percent", []string{"between 1 and 100", "got 101"}},
		// The §7 case: a write of 240 is refused naming the derived floor, 300.
		{"seal lag below the floor", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 240 }, "max_seal_lag_seconds", []string{"300", "86400", "got 240"}},
		{"seal lag one minute under", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = minLag - 60 }, "max_seal_lag_seconds", []string{"300"}},
		{"seal lag above the ceiling", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 86460 }, "max_seal_lag_seconds", []string{"86400", "got 86460"}},
		{"seal lag not whole minutes", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 330 }, "max_seal_lag_seconds", []string{"whole number of minutes", "60"}},
		{"seal lag zero", func(d *GatePolicyDocument) { d.MaxSealLagSeconds = 0 }, "max_seal_lag_seconds", []string{"300"}},
		{"no unknown_behavior", func(d *GatePolicyDocument) { d.UnknownBehavior = "" }, "unknown_behavior", []string{"warn|block", "no default"}},
		{"bad unknown_behavior", func(d *GatePolicyDocument) { d.UnknownBehavior = "allow" }, "unknown_behavior", []string{"warn|block", `"allow"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validGateDoc()
			tc.mut(&d)
			_, err := ValidateGatePolicyV1(d)
			if err == nil {
				t.Fatal("accepted; the server would have filled in what the caller did not write")
			}
			var pe *GatePolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("error is %T, want *GatePolicyError so the transport can name the field: %v", err, err)
			}
			if pe.Field != tc.field {
				t.Fatalf("refusal names field %q, want %q: %v", pe.Field, tc.field, err)
			}
			if !strings.HasPrefix(err.Error(), tc.field+": ") {
				t.Fatalf("Error() does not lead with the field: %q", err.Error())
			}
			for _, w := range tc.words {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not say %q", err.Error(), w)
				}
			}
		})
	}
}

// The seal-lag refusal quotes the domain's own floor, not a literal: if MinSealLag moved, the
// message would move with it.
func TestSealLagRefusalQuotesTheDerivedFloor(t *testing.T) {
	d := validGateDoc()
	d.MaxSealLagSeconds = int(MinSealLag/time.Second) - 1
	_, err := ValidateGatePolicyV1(d)
	if err == nil {
		t.Fatal("one second under the floor was accepted")
	}
	want := "between " + itoa(int(MinSealLag/time.Second)) + " and " + itoa(int(MaxSealLag/time.Second))
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not carry the derived range %q", err.Error(), want)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestGateUnknownBehaviorResolvesToItsAction(t *testing.T) {
	if GateUnknownWarn.Action() != GateActionWarn || GateUnknownBlock.Action() != GateActionBlock {
		t.Fatal("unknown_behavior does not resolve to its own action (D4 step 3)")
	}
	if !ClauseAssignBlock.Constrains() || !ClauseAssignWarn.Constrains() || ClauseAssignIgnore.Constrains() {
		t.Fatal("only block and warn constrain a decision (D4 step 1)")
	}
	for _, s := range []GateState{GateStateAllow, GateStateWarn, GateStateBlock, GateStateUnknown, GateStateNotConfigured} {
		if !ValidGateState(s) {
			t.Errorf("%q should be a valid state", s)
		}
	}
	if ValidGateState("PASS") || ValidGateAction("UNKNOWN") || ValidClauseAssignment("skip") || ValidGateRevokedReason("timeout") {
		t.Fatal("an unknown enum value was accepted")
	}
}

// D13a: the status is a function over SEMANTIC FACTS, table-tested over every combination of
// revoked_reason × expiry × revision match. Expected values are written out, not derived, so
// the table is an oracle rather than a second copy of the function.
func TestGateOverrideStatusPrecedenceTable(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	live := int64(7)
	other := int64(8)
	type revision struct {
		name string
		live *int64
	}
	revisions := []revision{{"match", &live}, {"mismatch", &other}, {"tombstoned", nil}}
	type expiry struct {
		name string
		at   time.Time
	}
	expiries := []expiry{{"future", now.Add(time.Hour)}, {"past", now.Add(-time.Hour)}}
	reasons := []GateRevokedReason{"", GateRevokedManual, GateRevokedExpired, GateRevokedPolicyChanged, GateRevokedPolicyDeleted}

	// expected[reason][expiry][revision]
	expected := map[GateRevokedReason]map[string]map[string]GateOverrideStatus{
		"": {
			"future": {"match": GateOverrideActive, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
			"past":   {"match": GateOverrideExpired, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
		},
		GateRevokedManual: {
			"future": {"match": GateOverrideRevoked, "mismatch": GateOverrideRevoked, "tombstoned": GateOverrideRevoked},
			"past":   {"match": GateOverrideRevoked, "mismatch": GateOverrideRevoked, "tombstoned": GateOverrideRevoked},
		},
		GateRevokedExpired: {
			"future": {"match": GateOverrideExpired, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
			"past":   {"match": GateOverrideExpired, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
		},
		GateRevokedPolicyChanged: {
			"future": {"match": GateOverrideInert, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
			"past":   {"match": GateOverrideInert, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
		},
		GateRevokedPolicyDeleted: {
			"future": {"match": GateOverrideInert, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
			"past":   {"match": GateOverrideInert, "mismatch": GateOverrideInert, "tombstoned": GateOverrideInert},
		},
	}

	n := 0
	for _, reason := range reasons {
		for _, ex := range expiries {
			for _, rev := range revisions {
				n++
				o := GateOverride{PolicyRevision: live, ExpiresAt: ex.at, RevokedReason: reason}
				if reason != "" {
					at := now.Add(-time.Minute)
					o.RevokedAt = &at
				}
				want := expected[reason][ex.name][rev.name]
				if got := GateOverrideStatusAt(o, now, rev.live); got != want {
					t.Errorf("reason=%q expiry=%s revision=%s: status %q, want %q", reason, ex.name, rev.name, got, want)
				}
			}
		}
	}
	if n != 30 {
		t.Fatalf("the table covered %d combinations, want 5 × 2 × 3 = 30", n)
	}
}

// The overlapping case, asserted on BOTH sides of its closure (review round 12 P1-1): an
// unrevoked, expired, revision-mismatched row reads inert; a later POST closes it as
// revoked_reason: expired; it still reads inert. The mutation that ranks a recorded `expired`
// above the mismatch flips the second read and fails the stable pair.
func TestGateOverrideStatusIsStableAcrossTheExpiredClosure(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	live := int64(9)
	o := GateOverride{PolicyRevision: 8, ExpiresAt: now.Add(-time.Minute)} // expired, mismatched, unrevoked

	before := GateOverrideStatusAt(o, now, &live)
	if before != GateOverrideInert {
		t.Fatalf("before closure: %q, want inert (the revision moved, whatever the clock says)", before)
	}
	closedAt := now
	o.RevokedAt = &closedAt
	o.RevokedReason = GateRevokedExpired
	after := GateOverrideStatusAt(o, now, &live)
	if after != GateOverrideInert {
		t.Fatalf("after the closure wrote `expired`: %q, want inert — the same persisted override changed status "+
			"because an unrelated override was created", after)
	}
	if after == GateOverrideExpired {
		t.Fatal("a recorded `expired` outranked the revision mismatch: the precedence-swap mutation")
	}

	// And a manual revoke outranks everything, including a mismatch and an expiry.
	o.RevokedReason = GateRevokedManual
	if got := GateOverrideStatusAt(o, now, nil); got != GateOverrideRevoked {
		t.Fatalf("manual revoke on a mismatched, expired row: %q, want revoked", got)
	}

	// The boundary: expires_at == now is expired (`expires_at <= now()`), one second later is active.
	live = 8
	o = GateOverride{PolicyRevision: 8, ExpiresAt: now}
	if got := GateOverrideStatusAt(o, now, &live); got != GateOverrideExpired {
		t.Fatalf("expires_at == now: %q, want expired", got)
	}
	o.ExpiresAt = now.Add(time.Second)
	if got := GateOverrideStatusAt(o, now, &live); got != GateOverrideActive {
		t.Fatalf("expires_at one second ahead: %q, want active", got)
	}
	if !o.Open() {
		t.Fatal("an unrevoked row is open (the slot predicate)")
	}
}
