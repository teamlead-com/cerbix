package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-024 store core (func-reliability-gate §7 — *Decision algebra*, *Owners and parity*, *One
// snapshot*, *Override*, *Presence*).

const gateBudget = 5 * time.Second

func gateDecide(t *testing.T, st *Store, ctx context.Context, f gateFixture) domain.GateDecision {
	t.Helper()
	dec, err := st.DecideGate(ctx, f.projectID, f.serviceID, gateBudget)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return dec
}

// gateArmCoverage seeds a live coverage evaluation (the arming fixture's stand-in), with a
// lease `lease` from the database clock.
func gateArmCoverage(t *testing.T, st *Store, ctx context.Context, f gateFixture, lease time.Duration) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_alert_state
		  (service_id, project_id, observed_state, candidate_state, streak, live_firing,
		   config_generation, revision_id, evaluated_at, lease_until)
		SELECT s.id, s.project_id, 'healthy', 'healthy', 3, false, s.alert_config_generation,
		       (SELECT r.id FROM service_definition_revisions r
		         WHERE r.service_id = s.id AND r.state = 'effective' AND r.effective_at <= now()
		         ORDER BY r.effective_at DESC, r.revision DESC LIMIT 1),
		       now(), now() + $2::interval
		  FROM services s WHERE s.id = $1
		ON CONFLICT (service_id) DO UPDATE SET
		   evaluated_at = EXCLUDED.evaluated_at, lease_until = EXCLUDED.lease_until, last_error = NULL`,
		f.serviceID, pgInterval(lease)); err != nil {
		t.Fatalf("arm coverage: %v", err)
	}
}

func reasonCodes(dec domain.GateDecision) []string {
	out := make([]string, 0, len(dec.Reasons))
	for _, r := range dec.Reasons {
		if r.Clause != "" {
			out = append(out, string(r.Clause)+"="+r.Code)
		} else {
			out = append(out, r.Code)
		}
	}
	sort.Strings(out)
	return out
}

func wantReasons(t *testing.T, dec domain.GateDecision, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := reasonCodes(dec)
	// The evidence-absence entries are not part of the clause algebra; strip them for the
	// clause comparison and assert them where a test cares.
	clauseOnly := got[:0]
	for _, g := range got {
		if strings.Contains(g, "=") {
			clauseOnly = append(clauseOnly, g)
		}
	}
	if strings.Join(clauseOnly, " ") != strings.Join(want, " ") {
		t.Errorf("reasons = %v, want %v", clauseOnly, want)
	}
}

func wantOutcome(t *testing.T, dec domain.GateDecision, state domain.GateState, action domain.GateAction) {
	t.Helper()
	if dec.State != state {
		t.Errorf("state = %s, want %s (reasons %v)", dec.State, state, reasonCodes(dec))
	}
	if dec.Action == nil || *dec.Action != action {
		t.Errorf("action = %v, want %s", dec.Action, action)
	}
}

func actionOf(dec domain.GateDecision) string {
	if dec.Action == nil {
		return "<nil>"
	}
	return string(*dec.Action)
}

// ── Decision algebra (D4; invariants 2–4) ────────────────────────────────────────────────

func TestGateDecisionAlgebraTable(t *testing.T) {
	warnIgnoreTicket := map[domain.GateClause]domain.ClauseAssignment{domain.ClauseTicketBurnFiring: domain.ClauseAssignIgnore}
	cases := []struct {
		name          string
		goodUs, badUs int64
		threshold     int
		assign        map[domain.GateClause]domain.ClauseAssignment
		unknown       domain.GateUnknownBehavior
		pageFiring    bool
		pageLease     time.Duration
		ticketFiring  bool
		ticketLease   time.Duration
		incident      bool
		state         domain.GateState
		action        domain.GateAction
		reasons       []string
	}{
		{name: "intact budget → ALLOW", goodUs: minute, threshold: 90, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateAllow, action: domain.GateActionAllow},
		{name: "page firing → BLOCK, the ticket clause reported too", goodUs: minute, threshold: 90, unknown: domain.GateUnknownWarn,
			pageFiring: true, pageLease: 90 * time.Second, ticketFiring: true, ticketLease: 90 * time.Second,
			state: domain.GateStateBlock, action: domain.GateActionBlock,
			reasons: []string{"page_burn_firing=page_burn_firing", "ticket_burn_firing=ticket_burn_firing"}},
		{name: "budget_consumed only → WARN", goodUs: 45 * minute / 60, badUs: 15 * minute / 60, threshold: 50, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateWarn, action: domain.GateActionWarn, reasons: []string{"budget_consumed=budget_consumed"}},
		{name: "warn + block → BLOCK, both reasons", goodUs: 45 * minute / 60, badUs: 15 * minute / 60, threshold: 50, unknown: domain.GateUnknownWarn,
			pageFiring: true, pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateBlock, action: domain.GateActionBlock,
			reasons: []string{"budget_consumed=budget_consumed", "page_burn_firing=page_burn_firing"}},
		{name: "known BLOCK + stale warn clause → BLOCK with the stale clause unavailable", goodUs: minute, threshold: 90, unknown: domain.GateUnknownWarn,
			pageFiring: true, pageLease: 90 * time.Second, ticketLease: -time.Second,
			state: domain.GateStateBlock, action: domain.GateActionBlock,
			reasons: []string{"page_burn_firing=page_burn_firing", "ticket_burn_firing=facts_stale"}},
		{name: "only an ignored clause unavailable → ALLOW, not UNKNOWN", goodUs: minute, threshold: 90, unknown: domain.GateUnknownBlock,
			assign: warnIgnoreTicket, pageLease: 90 * time.Second, ticketLease: -time.Second,
			state: domain.GateStateAllow, action: domain.GateActionAllow, reasons: []string{"ticket_burn_firing=facts_stale"}},
		{name: "warn clause unavailable under unknown_behavior warn → UNKNOWN/WARN", goodUs: minute, threshold: 90, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: -time.Second,
			state: domain.GateStateUnknown, action: domain.GateActionWarn, reasons: []string{"ticket_burn_firing=facts_stale"}},
		{name: "warn clause unavailable under unknown_behavior block → UNKNOWN/BLOCK", goodUs: minute, threshold: 90, unknown: domain.GateUnknownBlock,
			pageLease: 90 * time.Second, ticketLease: -time.Second,
			state: domain.GateStateUnknown, action: domain.GateActionBlock, reasons: []string{"ticket_burn_firing=facts_stale"}},
		{name: "matching warn beside an unavailable warn → UNKNOWN by step 3, both listed", goodUs: minute, threshold: 90, unknown: domain.GateUnknownWarn,
			incident: true, pageLease: 90 * time.Second, ticketLease: -time.Second,
			state: domain.GateStateUnknown, action: domain.GateActionWarn,
			reasons: []string{"service_incident_open=service_incident_open", "ticket_burn_firing=facts_stale"}},
		{name: "budget exhausted → BLOCK, consumed reported too", goodUs: minute / 2, badUs: minute / 2, threshold: 90, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateBlock, action: domain.GateActionBlock,
			reasons: []string{"budget_consumed=budget_consumed", "budget_exhausted=budget_exhausted"}},
		{name: "BurnedPercent exactly at the threshold matches (>=)", goodUs: 45 * minute / 60, badUs: 15 * minute / 60, threshold: 50, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateWarn, action: domain.GateActionWarn, reasons: []string{"budget_consumed=budget_consumed"}},
		{name: "one above the burned percent does not match", goodUs: 45 * minute / 60, badUs: 15 * minute / 60, threshold: 51, unknown: domain.GateUnknownWarn,
			pageLease: 90 * time.Second, ticketLease: 90 * time.Second,
			state: domain.GateStateAllow, action: domain.GateActionAllow},
		{name: "no evaluation at all for a block clause → UNKNOWN", goodUs: minute, threshold: 90, unknown: domain.GateUnknownBlock,
			pageLease: 0, ticketLease: 90 * time.Second,
			state: domain.GateStateUnknown, action: domain.GateActionBlock, reasons: []string{"page_burn_firing=facts_stale"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := gateStore(t)
			f := gateService(t, st, ctx, 2*time.Minute, tc.goodUs, tc.badUs)
			if tc.pageLease != 0 {
				gateLatch(t, st, ctx, f, gatePageKey, tc.pageFiring, tc.pageLease)
			}
			gateLatch(t, st, ctx, f, gateTicketKey, tc.ticketFiring, tc.ticketLease)
			if tc.incident {
				gateOpenIncident(t, st, ctx, f)
			}
			gateArmCoverage(t, st, ctx, f, 90*time.Second)
			doc := gateDoc(tc.assign)
			doc.BudgetConsumedPercent, doc.UnknownBehavior = tc.threshold, tc.unknown
			gatePut(t, st, ctx, f, nil, doc)

			dec := gateDecide(t, st, ctx, f)
			wantOutcome(t, dec, tc.state, tc.action)
			wantReasons(t, dec, tc.reasons...)
			if dec.UnoverriddenAction != nil || dec.Override != nil || dec.OverrideID != nil {
				t.Errorf("no override was active, yet: %v %v %v", dec.UnoverriddenAction, dec.Override, dec.OverrideID)
			}
		})
	}
}

// The ALLOW decision carries EVERY evidence field of the D7 table (§7 "intact budget → ALLOW
// with every evidence field"), and the ledger reads it back byte for byte.
func TestGateDecisionAllowCarriesEveryEvidenceField(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	if dec.PolicyRevision == nil || *dec.PolicyRevision != rev || dec.Window == nil || *dec.Window != gateWindow ||
		dec.UnknownBehavior == nil || dec.MaxSealLagSeconds == nil || *dec.MaxSealLagSeconds != 900 {
		t.Errorf("policy fields: rev=%v window=%v ub=%v lag=%v", dec.PolicyRevision, dec.Window, dec.UnknownBehavior, dec.MaxSealLagSeconds)
	}
	if dec.TargetID == nil || *dec.TargetID != f.targetID || dec.Objective == nil || *dec.Objective != 50 || dec.ObjectiveUpdatedAt == nil {
		t.Errorf("target fields: %v %v %v", dec.TargetID, dec.Objective, dec.ObjectiveUpdatedAt)
	}
	if dec.SealedThrough == nil || !dec.SealedThrough.Equal(f.sealed) || dec.SealLagSeconds == nil || *dec.SealLagSeconds < 119 || *dec.SealLagSeconds > 130 {
		t.Errorf("sealed fields: through=%v lag=%v (fixture sealed %s)", dec.SealedThrough, dec.SealLagSeconds, f.sealed)
	}
	if dec.FactRevisions == nil || dec.FactRevisions.Count != 1 || dec.FactRevisions.FirstID == nil || *dec.FactRevisions.FirstID != f.revisionID ||
		dec.FactRevisions.LastID == nil || len(dec.FactRevisions.Digest) != 64 {
		t.Errorf("fact_revisions = %+v, want the fixture's one revision %s", dec.FactRevisions, f.revisionID)
	}
	if dec.GoverningRevision == nil || dec.GoverningRevision.ID != f.revisionID || dec.GoverningRevision.Revision != 1 {
		t.Errorf("governing_revision = %+v", dec.GoverningRevision)
	}
	if dec.BurnLeases == nil || len(*dec.BurnLeases) != 2 {
		t.Fatalf("burn_leases = %+v, want one per rule", dec.BurnLeases)
	}
	for _, l := range *dec.BurnLeases {
		if l.Firing == nil || *l.Firing || l.LeaseUntil == nil || !l.Fresh || l.LastVerdict == nil || *l.LastVerdict != "clear" {
			t.Errorf("lease %+v", l)
		}
	}
	if dec.CoverageLeaseUntil == nil || dec.CoverageState == nil || dec.CoverageState.Live.Reason == "" {
		t.Errorf("coverage: until=%v state=%+v", dec.CoverageLeaseUntil, dec.CoverageState)
	}
	if dec.FactsFreshUntil == nil {
		t.Error("facts_fresh_until absent for a constraining policy")
	}
	for _, r := range dec.Reasons {
		t.Errorf("an ALLOW with full evidence has no reason, got %+v", r)
	}

	// The ledger row is the decision: read back, the canonical JSON is identical.
	got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if a, b := gateCanon(t, dec), gateCanon(t, got); a != b {
		t.Errorf("by-id read differs from the decision:\n%s\n%s", a, b)
	}
	// Canonical bytes: what the writer produces has sorted keys and no whitespace (jsonb
	// re-renders on read, so canonicality is a property of the bytes written, and the CHECK
	// bounds are asserted over the database's own rendering).
	written := gateCanon(t, dec.GateDecisionEvidence)
	if strings.ContainsAny(written, " \n\t") {
		t.Errorf("evidence bytes carry whitespace: %s", written)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(written), &top); err != nil {
		t.Fatal(err)
	}
	prev := ""
	for _, k := range strings.Split(strings.Trim(gateTopLevelKeys(written), ","), ",") {
		if k < prev {
			t.Errorf("evidence keys not sorted: %s after %s", k, prev)
		}
		prev = k
	}
	var evidenceLen, reasonsLen, snapshotLen int
	var reasons string
	if err := st.pool.QueryRow(ctx, `
		SELECT octet_length(evidence::text), octet_length(reasons::text), octet_length(policy_snapshot::text), reasons::text
		  FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&evidenceLen, &reasonsLen, &snapshotLen, &reasons); err != nil {
		t.Fatal(err)
	}
	if evidenceLen > 2048 || reasonsLen > 64 || snapshotLen > 1024 {
		t.Errorf("row bytes evidence=%d reasons=%d snapshot=%d — a realistic row should sit well under the 4096/1024/4096 CHECKs", evidenceLen, reasonsLen, snapshotLen)
	}
	if reasons != "[]" {
		t.Errorf("reasons = %s", reasons)
	}
}

// gateTopLevelKeys extracts the top-level object keys of compact JSON in document order.
func gateTopLevelKeys(compact string) string {
	dec := json.NewDecoder(strings.NewReader(compact))
	var keys []string
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' || v == '[' {
				depth++
			} else {
				depth--
			}
		case string:
			// At depth 1 every string token is a key; its value is consumed whole so a
			// string value is never mistaken for a key.
			if depth == 1 {
				keys = append(keys, v)
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					return strings.Join(keys, ",")
				}
			}
		}
	}
	return strings.Join(keys, ",")
}

func gateCanon(t *testing.T, v any) string {
	t.Helper()
	raw, err := canonicalJSONBytes(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func gateJSONKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// D2: the policy's window selects ONE target; the other window's firing rule changes nothing.
func TestGateDecisionPolicyWindowSelectsItsTarget(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	// A second, 7d target whose page rule is FIRING.
	other := f
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1, '7d', 99.9, true, $2::jsonb) RETURNING id`, f.serviceID, "["+gatePageRule+"]").Scan(&other.targetID); err != nil {
		t.Fatal(err)
	}
	gateLatch(t, st, ctx, other, gatePageKey, true, 90*time.Second)

	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	if dec.TargetID == nil || *dec.TargetID != f.targetID {
		t.Errorf("target = %v, want the 24h target %s", dec.TargetID, f.targetID)
	}

	// The same service, a policy for 7d with budget clauses ignored: the 7d page rule blocks.
	sevenDays := gateDoc(map[domain.GateClause]domain.ClauseAssignment{
		domain.ClauseBudgetExhausted: domain.ClauseAssignIgnore, domain.ClauseBudgetConsumed: domain.ClauseAssignIgnore,
	})
	sevenDays.Window = "7d"
	gatePut(t, st, ctx, f, int64p(rev), sevenDays)
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateBlock, domain.GateActionBlock)
	if dec.TargetID == nil || *dec.TargetID != other.targetID || dec.BurnLeases == nil || len(*dec.BurnLeases) != 1 {
		t.Errorf("7d decision target=%v leases=%v", dec.TargetID, dec.BurnLeases)
	}
}

// ── Owners and parity (D1, D8, D8a; invariants 1, 6, 15) ─────────────────────────────────

// The gate's number IS the report's number for the same window: a gate that recomputed
// BurnedPercent locally could not pass this (the fixture's split is not reconstructible from
// anything the gate reads except the report), and the decision path has no heartbeat access
// by construction — asserted over the source text of the gate files.
func TestGateDecisionBudgetParityWithTheReportAndNoHeartbeatAccess(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, 45*minute/60, 15*minute/60)
	gateFreshLatches(t, st, ctx, f)
	doc := gateDoc(nil)
	doc.BudgetConsumedPercent = 50
	gatePut(t, st, ctx, f, nil, doc)

	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateWarn, domain.GateActionWarn)
	w, _ := sla.WindowByName(gateWindow)
	rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, w)
	if err != nil || rep.Budget == nil {
		t.Fatalf("report: %+v %v", rep, err)
	}
	var quoted float64
	for _, r := range dec.Reasons {
		if r.Clause == domain.ClauseBudgetConsumed {
			quoted, _ = r.Value.(float64)
		}
	}
	if quoted != rep.Budget.BurnedPercent || quoted != 50 {
		t.Errorf("gate quoted %v, report says %v (want both exactly 50)", quoted, rep.Budget.BurnedPercent)
	}

	// The report withholds → the gate quotes NO number: budget_withheld, never a zero.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1 AND bucket_start = $2`, f.serviceID, f.sealed.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateUnknown, domain.GateActionWarn)
	wantReasons(t, dec, "budget_exhausted=budget_withheld", "budget_consumed=budget_withheld")
	for _, r := range dec.Reasons {
		if r.Value != nil {
			t.Errorf("a withheld clause quoted a value: %+v", r)
		}
	}

	// Invariant 6, by construction: no gate file touches heartbeats.
	for _, name := range []string{"gate.go", "gatepolicy.go", "gateoverride.go", "gatedecision.go", "gateledger.go"} {
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "heartbeats") {
			t.Errorf("%s mentions heartbeats; the decision path reads only sealed facts", name)
		}
	}
}

// D8a: a QUOTABLE report whose watermark is one second past the bound is seal_stale on every
// budget clause, never ALLOW on budget; five seconds inside the bound it is known.
func TestGateDecisionSealStaleBoundary(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 900*time.Second+time.Second, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil)) // max_seal_lag_seconds = 900

	w, _ := sla.WindowByName(gateWindow)
	if rep, err := st.ServiceReliabilityReport(ctx, f.projectID, f.serviceID, w); err != nil || rep.Budget == nil {
		t.Fatalf("the fixture must be QUOTABLE for the case to mean anything: %+v %v", rep.Status, err)
	}
	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateUnknown, domain.GateActionWarn)
	wantReasons(t, dec, "budget_exhausted=seal_stale", "budget_consumed=seal_stale")
	if dec.SealLagSeconds == nil || *dec.SealLagSeconds <= 900 {
		t.Errorf("seal_lag = %v, want > 900", dec.SealLagSeconds)
	}

	gateReplant(t, st, ctx, &f, 900*time.Second-5*time.Second, minute, 0)
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	if dec.SealLagSeconds == nil || *dec.SealLagSeconds > 900 {
		t.Errorf("seal_lag = %v, want <= 900", dec.SealLagSeconds)
	}
}

// never_sealed: no watermark → both budget clauses unavailable, no sealed fields.
func TestGateDecisionNeverSealed(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	gateUnseal(t, st, ctx, f)

	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateUnknown, domain.GateActionWarn)
	wantReasons(t, dec, "budget_exhausted=never_sealed", "budget_consumed=never_sealed")
	keys := gateJSONKeys(t, dec)
	for _, k := range []string{"sealed_through", "seal_lag", "fact_revisions"} {
		if contains(keys, k) {
			t.Errorf("%s present on a never-sealed decision: %v", k, keys)
		}
	}
	if !contains(keys, "target_id") || !contains(keys, "policy_revision") || !contains(keys, "burn_leases") {
		t.Errorf("target and policy fields must still be present: %v", keys)
	}
}

func contains(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// ── facts_fresh_until (D7; invariant 16) ─────────────────────────────────────────────────

func TestGateDecisionFactsFreshUntilFormula(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	horizon := f.sealed.Add(900 * time.Second) // sealed_through + max_seal_lag_seconds

	// (1) Seal horizon earlier than every constraining lease.
	gateLatch(t, st, ctx, f, gatePageKey, false, 20*time.Minute)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 25*time.Minute)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)
	if dec.FactsFreshUntil == nil || !dec.FactsFreshUntil.Equal(horizon) {
		t.Errorf("(1) facts_fresh_until = %v, want the seal horizon %s", dec.FactsFreshUntil, horizon)
	}

	// (2) The earliest constraining lease otherwise: page at +90s, ticket at +120s.
	gateLatch(t, st, ctx, f, gatePageKey, false, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 120*time.Second)
	var pageLease, ticketLease time.Time
	if err := st.pool.QueryRow(ctx, `SELECT lease_until FROM service_burn_alert_state WHERE sla_target_id = $1 AND rule_key = $2`, f.targetID, gatePageKey).Scan(&pageLease); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT lease_until FROM service_burn_alert_state WHERE sla_target_id = $1 AND rule_key = $2`, f.targetID, gateTicketKey).Scan(&ticketLease); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	if dec.FactsFreshUntil == nil || !dec.FactsFreshUntil.Equal(pageLease) {
		t.Errorf("(2) facts_fresh_until = %v, want the page lease %s", dec.FactsFreshUntil, pageLease)
	}
	// (2b) The page clause ignored: the ticket lease is the earliest CONSTRAINING one.
	rev = gatePut(t, st, ctx, f, int64p(rev), gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClausePageBurnFiring: domain.ClauseAssignIgnore}))
	dec = gateDecide(t, st, ctx, f)
	if dec.FactsFreshUntil == nil || !dec.FactsFreshUntil.Equal(ticketLease) {
		t.Errorf("(2b) facts_fresh_until = %v, want the ticket lease %s", dec.FactsFreshUntil, ticketLease)
	}

	// (3) Burn clauses all ignore, budget block: the seal horizon alone, whatever the leases say.
	rev = gatePut(t, st, ctx, f, int64p(rev), gateDoc(map[domain.GateClause]domain.ClauseAssignment{
		domain.ClausePageBurnFiring: domain.ClauseAssignIgnore, domain.ClauseTicketBurnFiring: domain.ClauseAssignIgnore,
	}))
	dec = gateDecide(t, st, ctx, f)
	if dec.FactsFreshUntil == nil || !dec.FactsFreshUntil.Equal(horizon) {
		t.Errorf("(3) facts_fresh_until = %v, want the seal horizon %s", dec.FactsFreshUntil, horizon)
	}

	// (5) A stale COVERAGE lease moves nothing.
	gateArmCoverage(t, st, ctx, f, -time.Second)
	dec2 := gateDecide(t, st, ctx, f)
	if dec2.FactsFreshUntil == nil || !dec2.FactsFreshUntil.Equal(*dec.FactsFreshUntil) {
		t.Errorf("(5) a stale coverage lease moved facts_fresh_until: %v → %v", dec.FactsFreshUntil, dec2.FactsFreshUntil)
	}
	if dec2.CoverageLeaseUntil == nil || !dec2.CoverageLeaseUntil.Before(dec2.EvaluatedAt) || dec2.CoverageState == nil {
		t.Errorf("(5) the coverage lease must be visibly stale in the evidence: until=%v at=%s", dec2.CoverageLeaseUntil, dec2.EvaluatedAt)
	}

	// (4) Every clause ignore: no facts_fresh_until, and an ALLOW.
	all := map[domain.GateClause]domain.ClauseAssignment{}
	for _, c := range domain.GateClausesV1 {
		all[c] = domain.ClauseAssignIgnore
	}
	gatePut(t, st, ctx, f, int64p(rev), gateDoc(all))
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	if dec.FactsFreshUntil != nil {
		t.Errorf("(4) facts_fresh_until = %v for an all-ignore policy, want absent", dec.FactsFreshUntil)
	}
}

// ── One snapshot (D6a; invariant 7) ──────────────────────────────────────────────────────

// A policy edit, an incident and a latch flip committed by a blocker connection BETWEEN the
// policy read and the remaining reads all land on the far side of evaluated_at: the decision
// is the pre-mutation world entirely; the next decision is the post-mutation world entirely.
func TestGateDecisionIsOneSnapshot(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))

	mutated := false
	gateDecisionHook = func(hctx context.Context, attempt int, phase string, _ pgx.Tx) error {
		if phase != gatePhasePolicyRead || mutated {
			return nil
		}
		mutated = true
		// Another connection, committed while the decision transaction is open.
		if _, _, err := st.PutGatePolicy(hctx, f.projectID, f.serviceID, int64p(rev),
			gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock}), gateActorToken); err != nil {
			return err
		}
		gateOpenIncident(t, st, hctx, f)
		gateLatch(t, st, hctx, f, gatePageKey, true, 90*time.Second)
		return nil
	}
	t.Cleanup(func() { gateDecisionHook = nil })

	before := gateDecide(t, st, ctx, f)
	wantOutcome(t, before, domain.GateStateAllow, domain.GateActionAllow)
	if before.PolicyRevision == nil || *before.PolicyRevision != rev {
		t.Errorf("the decision saw revision %v, want the pre-edit %d", before.PolicyRevision, rev)
	}
	after := gateDecide(t, st, ctx, f)
	wantOutcome(t, after, domain.GateStateBlock, domain.GateActionBlock)
	wantReasons(t, after, "page_burn_firing=page_burn_firing", "service_incident_open=service_incident_open")
	if after.PolicyRevision == nil || *after.PolicyRevision != rev+1 {
		t.Errorf("the next decision saw revision %v, want %d", after.PolicyRevision, rev+1)
	}
	if !before.EvaluatedAt.Before(after.EvaluatedAt) {
		t.Errorf("evaluated_at did not advance: %s vs %s", before.EvaluatedAt, after.EvaluatedAt)
	}
}

// A REAL serialization failure (a locking read after a concurrent committed update of the same
// row, under REPEATABLE READ) is retried once; a second one is ErrGateSnapshotConflict with no
// ledger row written.
func TestGateDecisionRetriesASerializationFailureOnce(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	var attempts []int
	failOn := func(attempt int) bool { return attempt == 0 }
	gateDecisionHook = func(hctx context.Context, attempt int, phase string, tx pgx.Tx) error {
		if phase != gatePhaseReadsDone {
			return nil
		}
		attempts = append(attempts, attempt)
		if !failOn(attempt) {
			return nil
		}
		if _, err := st.pool.Exec(hctx, `UPDATE service_gate_policies SET updated_at = now() WHERE service_id = $1`, f.serviceID); err != nil {
			return err
		}
		_, err := tx.Exec(hctx, `SELECT 1 FROM service_gate_policies WHERE service_id = $1 FOR UPDATE`, f.serviceID)
		if err == nil {
			return errors.New("the locking read did not fail; the fixture did not produce 40001")
		}
		if !isSerializationFailure(err) {
			return fmt.Errorf("expected SQLSTATE 40001, got %v", err)
		}
		return err
	}
	t.Cleanup(func() { gateDecisionHook = nil })

	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	if len(attempts) != 2 || attempts[0] != 0 || attempts[1] != 1 {
		t.Errorf("attempts = %v, want [0 1]", attempts)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE service_id = $1`, f.serviceID).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("ledger rows after a retried decision = %d %v, want exactly 1", rows, err)
	}

	attempts = nil
	failOn = func(int) bool { return true }
	_, err := st.DecideGate(ctx, f.projectID, f.serviceID, gateBudget)
	if !errors.Is(err, ErrGateSnapshotConflict) {
		t.Fatalf("two failures = %v, want ErrGateSnapshotConflict", err)
	}
	if len(attempts) != 2 {
		t.Errorf("attempts = %v, want exactly two (one retry)", attempts)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE service_id = $1`, f.serviceID).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("a snapshot conflict wrote a ledger row: %d %v", rows, err)
	}
}

// The budget: a decision that cannot finish inside it is ErrGateBudgetExceeded, not a decision.
func TestGateDecisionBudgetExceededIsATransportError(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	gateDecisionHook = func(_ context.Context, _ int, phase string, _ pgx.Tx) error {
		if phase == gatePhaseReadsDone {
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	}
	t.Cleanup(func() { gateDecisionHook = nil })
	_, err := st.DecideGate(ctx, f.projectID, f.serviceID, 200*time.Millisecond)
	if !errors.Is(err, ErrGateBudgetExceeded) {
		t.Fatalf("err = %v, want ErrGateBudgetExceeded", err)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE service_id = $1`, f.serviceID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("an exceeded budget wrote a ledger row: %d %v", rows, err)
	}
}

// ── Override (D9; invariants 8, 9, 17) ───────────────────────────────────────────────────

func TestGateDecisionOverrideChangesOnlyTheAction(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateLatch(t, st, ctx, f, gatePageKey, true, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 90*time.Second)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)

	blocked := gateDecide(t, st, ctx, f)
	wantOutcome(t, blocked, domain.GateStateBlock, domain.GateActionBlock)

	ovID, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "hotfix 1.2.3", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	dec := gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateBlock || actionOf(dec) != "ALLOW" || dec.UnoverriddenAction == nil || *dec.UnoverriddenAction != domain.GateActionBlock {
		t.Errorf("overridden: state=%s action=%s unoverridden=%v", dec.State, actionOf(dec), dec.UnoverriddenAction)
	}
	if dec.Override == nil || dec.Override.ID != ovID || dec.Override.ActorLabel != "token:ci" || dec.Override.Reason != "hotfix 1.2.3" ||
		dec.OverrideID == nil || *dec.OverrideID != ovID || !dec.Overridden() {
		t.Errorf("override evidence = %+v id=%v", dec.Override, dec.OverrideID)
	}
	if strings.Join(reasonCodes(dec), " ") != strings.Join(reasonCodes(blocked), " ") {
		t.Errorf("an override changed reasons: %v → %v", reasonCodes(blocked), reasonCodes(dec))
	}
	var storedOverride *string
	if err := st.pool.QueryRow(ctx, `SELECT override_id::text FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&storedOverride); err != nil || storedOverride == nil || *storedOverride != ovID {
		t.Errorf("ledger override_id = %v %v", storedOverride, err)
	}
	// Attribution reads the same label from the override and from the decision (invariant 17).
	rec, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ovID)
	if err != nil || rec.ActorLabel != dec.Override.ActorLabel {
		t.Errorf("labels differ: override %q, decision %q (%v)", rec.ActorLabel, dec.Override.ActorLabel, err)
	}

	// An expired override changes nothing — and a new one can be created after it.
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_overrides SET expires_at = now() - interval '1 second' WHERE id = $1`, ovID); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	if actionOf(dec) != "BLOCK" || dec.Override != nil || dec.OverrideID != nil || dec.UnoverriddenAction != nil {
		t.Errorf("an expired override was applied: action=%s override=%+v", actionOf(dec), dec.Override)
	}
	ov2, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "second", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatalf("the expired override did not release its slot: %v", err)
	}
	if dec = gateDecide(t, st, ctx, f); dec.OverrideID == nil || *dec.OverrideID != ov2 {
		t.Errorf("the new override was not applied: %v", dec.OverrideID)
	}

	// A policy edit revokes it: the next decision is un-overridden.
	rev = gatePut(t, st, ctx, f, int64p(rev), gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock}))
	dec = gateDecide(t, st, ctx, f)
	if actionOf(dec) != "BLOCK" || dec.Override != nil {
		t.Errorf("after a policy edit the override still applied: action=%s override=%+v", actionOf(dec), dec.Override)
	}

	// WARN is left alone by an override.
	gateLatch(t, st, ctx, f, gatePageKey, false, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, true, 90*time.Second)
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "warn case", now.Add(time.Hour), gateActorToken); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateWarn || actionOf(dec) != "WARN" || dec.Override != nil || dec.UnoverriddenAction != nil {
		t.Errorf("WARN under an override: state=%s action=%s override=%+v", dec.State, actionOf(dec), dec.Override)
	}
	// UNKNOWN resolving to BLOCK is overridden to ALLOW; state stays UNKNOWN.
	gateLatch(t, st, ctx, f, gateTicketKey, false, -time.Second)
	unknownBlock := gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock})
	unknownBlock.UnknownBehavior = domain.GateUnknownBlock
	rev = gatePut(t, st, ctx, f, int64p(rev), unknownBlock) // revokes the warn-case override
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "unknown case", now.Add(time.Hour), gateActorToken); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateUnknown || actionOf(dec) != "ALLOW" || dec.UnoverriddenAction == nil || *dec.UnoverriddenAction != domain.GateActionBlock {
		t.Errorf("UNKNOWN/BLOCK under an override: state=%s action=%s unoverridden=%v", dec.State, actionOf(dec), dec.UnoverriddenAction)
	}

	// A policy DELETE revokes the override and the next decision is NOT_CONFIGURED — never overridden.
	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, rev, gateActorToken); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateNotConfigured || dec.Action != nil || dec.Override != nil || dec.OverrideID != nil {
		t.Errorf("after delete: state=%s action=%v override=%+v", dec.State, dec.Action, dec.Override)
	}
}

// ── Presence (D7; invariants 2, 5) ───────────────────────────────────────────────────────

func TestGateDecisionPresenceRawJSON(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)

	always := []string{"decision_id", "evaluated_at", "reasons", "schema_version", "service_id", "service_name", "service_slug", "state"}

	// NOT_CONFIGURED: the always-present fields and nothing else; a recorded row with action NULL.
	dec := gateDecide(t, st, ctx, f)
	if dec.State != domain.GateStateNotConfigured || dec.Action != nil {
		t.Fatalf("no policy: state=%s action=%v", dec.State, dec.Action)
	}
	if got := gateJSONKeys(t, dec); strings.Join(got, ",") != strings.Join(always, ",") {
		t.Errorf("NOT_CONFIGURED keys = %v, want %v", got, always)
	}
	if len(dec.Reasons) != 1 || dec.Reasons[0].Code != "not_configured" || dec.Reasons[0].Docs != domain.GateDocsURL {
		t.Errorf("NOT_CONFIGURED reasons = %+v", dec.Reasons)
	}
	var state string
	var action, policyRev *string
	if err := st.pool.QueryRow(ctx, `SELECT state, action, policy_revision::text FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&state, &action, &policyRev); err != nil {
		t.Fatalf("the NOT_CONFIGURED decision was not recorded: %v", err)
	}
	if state != "NOT_CONFIGURED" || action != nil || policyRev != nil {
		t.Errorf("row: state=%s action=%v policy_revision=%v", state, action, policyRev)
	}
	got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil || gateCanon(t, got) != gateCanon(t, dec) {
		t.Errorf("by-id read of NOT_CONFIGURED differs: %v\n%s\n%s", err, gateCanon(t, got), gateCanon(t, dec))
	}

	// window_target_missing: policy fields present, no target/objective/burn_leases.
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	if _, err := st.pool.Exec(ctx, `DELETE FROM sla_targets WHERE id = $1`, f.targetID); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateUnknown, domain.GateActionWarn)
	wantReasons(t, dec, "budget_exhausted=window_target_missing", "budget_consumed=window_target_missing",
		"page_burn_firing=window_target_missing", "ticket_burn_firing=window_target_missing")
	keys := gateJSONKeys(t, dec)
	for _, k := range []string{"policy_revision", "window", "unknown_behavior", "max_seal_lag_seconds", "action", "sealed_through", "seal_lag", "fact_revisions"} {
		if !contains(keys, k) {
			t.Errorf("window_target_missing: %s absent: %v", k, keys)
		}
	}
	for _, k := range []string{"target_id", "objective", "objective_updated_at", "burn_leases", "override", "override_id", "unoverridden_action"} {
		if contains(keys, k) {
			t.Errorf("window_target_missing: %s present: %v", k, keys)
		}
	}
	got, err = st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil || gateCanon(t, got) != gateCanon(t, dec) {
		t.Errorf("by-id read differs: %v", err)
	}
}
