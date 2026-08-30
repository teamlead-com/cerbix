package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Agent B — adversarial pass over changeset 2 (func-reliability-gate §7 *Presence*, *Ledger*,
// *Identity and reads*; D7, D10, §5 listing contract).

// §7 *Identity and reads*: "a synthetic 5 KiB evidence fails the decision at the CHECK". The
// evidence is assembled from owners, so the driver is the owner: a target carrying 40 burn
// rules (planted by SQL past the domain's cap of 4) yields 40 `burn_leases[]` entries and an
// evidence past 4 KiB. The decision must FAIL — SQLSTATE 23514 on the payload CHECK, not a
// truncated row, not ErrGateLedgerUnwritable (the other 23514) — and write nothing.
func TestGateBEvidenceOverTheCheckFailsTheDecisionAndNeverTruncates(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	rules := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		rules = append(rules, fmt.Sprintf(`{"long_window_seconds":%d,"short_window_seconds":300,"threshold":14.4,"severity":"page"}`, 3600+60*i))
	}
	if _, err := st.pool.Exec(ctx, `UPDATE sla_targets SET burn_rules = $2::jsonb WHERE id = $1`, f.targetID, "["+strings.Join(rules, ",")+"]"); err != nil {
		t.Fatalf("plant 40 rules: %v", err)
	}
	_, err := st.DecideGate(ctx, f.projectID, f.serviceID, gateBudget)
	if err == nil {
		t.Fatal("a decision whose evidence exceeds 4 KiB was recorded; the writer must never truncate and the CHECK must refuse it")
	}
	if pgErrCode(err) != "23514" || !strings.Contains(err.Error(), "service_gate_decisions_payload_chk") {
		t.Errorf("err = %v, want SQLSTATE 23514 on service_gate_decisions_payload_chk", err)
	}
	if errors.Is(err, ErrGateLedgerUnwritable) || errors.Is(err, ErrGateBudgetExceeded) || errors.Is(err, ErrGateSnapshotConflict) {
		t.Errorf("a payload CHECK failure was classified as another error: %v", err)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decisions WHERE service_id = $1`, f.serviceID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("ledger rows after the refused decision = %d %v, want 0", rows, err)
	}
}

// The canonical evidence of a realistic ALLOW row — every D7 field present, about 1 KiB — is
// what the database holds: the jsonb re-rendered and re-canonicalized equals the writer's bytes,
// and the key set is the fixture's key set, nothing added, nothing dropped.
func TestGateBCanonicalEvidenceEqualsTheStoredRendering(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)

	written := gateCanon(t, dec.GateDecisionEvidence)
	var storedText string
	var storedLen int
	if err := st.pool.QueryRow(ctx, `SELECT evidence::text, octet_length(evidence::text) FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&storedText, &storedLen); err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal([]byte(storedText), &generic); err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != written {
		t.Errorf("stored evidence differs from the writer's canonical bytes:\n%s\n%s", stored, written)
	}
	if storedLen < 512 || storedLen > 2048 {
		t.Errorf("a full-evidence ALLOW row is %d bytes; expected the ~1 KiB class", storedLen)
	}
	want := gateJSONKeys(t, dec.GateDecisionEvidence)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(storedText), &m); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stored keys %v ≠ fixture keys %v", got, want)
	}
	for _, k := range []string{"burn_leases", "coverage_lease_until", "coverage_state", "fact_revisions", "facts_fresh_until",
		"governing_revision", "max_seal_lag_seconds", "objective", "objective_updated_at", "seal_lag", "target_id", "unknown_behavior"} {
		if !contains(got, k) {
			t.Errorf("full-evidence row lacks %s", k)
		}
	}
	for _, k := range []string{"override", "unoverridden_action"} {
		if contains(got, k) {
			t.Errorf("an un-overridden row carries %s", k)
		}
	}
}

// The WORST valid row fits the byte CHECKs: the domain's maximum of 4 burn rules all firing, an
// open incident, an exhausted budget (every clause matched → reasons at its longest), an override
// whose reason is 500 four-byte runes (2 000 bytes, the longest a valid reason renders to),
// coverage armed, a governing revision. If a valid input could exceed the CHECK the gate would be
// undecidable for that service — this pins the margin.
func TestGateBWorstCaseValidRowFitsTheByteChecks(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute/2, minute/2) // BurnedPercent = 100
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	rules := []string{
		gatePageRule, gateTicketRule,
		`{"long_window_seconds":7200,"short_window_seconds":600,"threshold":7.2,"severity":"page"}`,
		`{"long_window_seconds":86400,"short_window_seconds":3600,"threshold":3,"severity":"ticket"}`,
	}
	if err := domain.ValidateBurnRules(parseRules(t, rules)); err != nil {
		t.Fatalf("the fixture's four rules must be VALID: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE sla_targets SET burn_rules = $2::jsonb WHERE id = $1`, f.targetID, "["+strings.Join(rules, ",")+"]"); err != nil {
		t.Fatal(err)
	}
	for _, r := range parseRules(t, rules) {
		gateLatch(t, st, ctx, f, r.Key(), true, 90*time.Second)
	}
	gateOpenIncident(t, st, ctx, f)
	all := map[domain.GateClause]domain.ClauseAssignment{}
	for _, c := range domain.GateClausesV1 {
		all[c] = domain.ClauseAssignBlock
	}
	rev := gatePut(t, st, ctx, f, nil, gateDoc(all))
	reason := strings.Repeat("😀", domain.GateOverrideReasonMaxLen)
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, reason, gateDBNow(t, st, ctx).Add(7*24*time.Hour-time.Minute), GateActor{ViaToken: true, Label: "token:" + strings.Repeat("n", 64)}); err != nil {
		t.Fatalf("a 500-rune reason is valid: %v", err)
	}
	dec, err := st.DecideGate(ctx, f.projectID, f.serviceID, gateBudget)
	if err != nil {
		t.Fatalf("the worst VALID row must be decidable: %v", err)
	}
	if dec.State != domain.GateStateBlock || dec.Override == nil || len(dec.Reasons) < 5 {
		t.Fatalf("fixture did not produce the worst case: state=%s override=%v reasons=%d", dec.State, dec.Override != nil, len(dec.Reasons))
	}
	var evidenceLen, reasonsLen, snapshotLen int
	if err := st.pool.QueryRow(ctx, `
		SELECT octet_length(evidence::text), octet_length(reasons::text), octet_length(policy_snapshot::text)
		  FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&evidenceLen, &reasonsLen, &snapshotLen); err != nil {
		t.Fatal(err)
	}
	t.Logf("worst valid row: evidence=%d/4096 reasons=%d/1024 snapshot=%d/4096 bytes", evidenceLen, reasonsLen, snapshotLen)
	if evidenceLen > 4096 || reasonsLen > 1024 || snapshotLen > 4096 {
		t.Errorf("the worst valid row does not fit the CHECKs: evidence=%d reasons=%d snapshot=%d", evidenceLen, reasonsLen, snapshotLen)
	}
}

func parseRules(t *testing.T, rules []string) []domain.BurnRule {
	t.Helper()
	var out []domain.BurnRule
	if err := json.Unmarshal([]byte("["+strings.Join(rules, ",")+"]"), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// D7 presence for the two evidence-only absences A's tests never assert: `coverage_lease_until`
// and `coverage_state` are absent with reason `never_evaluated` until the service has been
// evaluated; `governing_revision` is absent with reason `no_governing_revision` when no
// declaration governs at evaluated_at. Neither reason enters the algebra: the state stays ALLOW.
// Raw JSON from json.Marshal, key ABSENCE asserted.
func TestGateBPresenceNeverEvaluatedAndNoGoverningRevisionRawJSON(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	reasonCodesOnly := func(dec domain.GateDecision) map[string]string {
		out := map[string]string{}
		for _, r := range dec.Reasons {
			if r.Clause == "" {
				out[r.Code] = r.Source
			}
		}
		return out
	}

	// (1) Never evaluated: no coverage row at all.
	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	keys := gateJSONKeys(t, dec)
	for _, k := range []string{"coverage_lease_until", "coverage_state"} {
		if contains(keys, k) {
			t.Errorf("never_evaluated: %s present in %v", k, keys)
		}
	}
	if src, ok := reasonCodesOnly(dec)["never_evaluated"]; !ok || src != gateSourceCoverage {
		t.Errorf("never_evaluated reason missing or mis-sourced: %+v", dec.Reasons)
	}
	if !contains(keys, "governing_revision") {
		t.Errorf("governing_revision absent while a declaration governs: %v", keys)
	}
	if _, ok := reasonCodesOnly(dec)["no_governing_revision"]; ok {
		t.Errorf("no_governing_revision reported while a declaration governs: %+v", dec.Reasons)
	}
	// The ledger reads it back identically.
	if got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID); err != nil || gateCanon(t, got) != gateCanon(t, dec) {
		t.Errorf("by-id read differs: %v", err)
	}

	// (2) Evaluated: both coverage fields present, the reason gone.
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	dec = gateDecide(t, st, ctx, f)
	keys = gateJSONKeys(t, dec)
	for _, k := range []string{"coverage_lease_until", "coverage_state"} {
		if !contains(keys, k) {
			t.Errorf("evaluated: %s absent in %v", k, keys)
		}
	}
	if _, ok := reasonCodesOnly(dec)["never_evaluated"]; ok {
		t.Errorf("never_evaluated reported after an evaluation: %+v", dec.Reasons)
	}

	// (3) No declaration governs at evaluated_at: the revision becomes effective in the future.
	if _, err := st.pool.Exec(ctx, `UPDATE service_definition_revisions SET effective_at = now() + interval '1 hour' WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	dec = gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	keys = gateJSONKeys(t, dec)
	if contains(keys, "governing_revision") {
		t.Errorf("no_governing_revision: governing_revision present in %v", keys)
	}
	if src, ok := reasonCodesOnly(dec)["no_governing_revision"]; !ok || src != gateSourceRevisions {
		t.Errorf("no_governing_revision reason missing or mis-sourced: %+v", dec.Reasons)
	}
	// fact_revisions is about the SEALED facts, not the governing declaration: still present.
	if !contains(keys, "fact_revisions") || dec.FactRevisions == nil || dec.FactRevisions.Count != 1 {
		t.Errorf("fact_revisions must survive the absence of a governing revision: %v %+v", keys, dec.FactRevisions)
	}
	if got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID); err != nil || gateCanon(t, got) != gateCanon(t, dec) {
		t.Errorf("by-id read differs: %v", err)
	}
}

// §5 listing contract: the keyset is a ROW comparison `(evaluated_at, id) < ($ts, $id)`, so rows
// sharing one evaluated_at are separated by id. A's rows are minutes apart and cannot see the
// tiebreak; here three rows share ONE instant and `limit = 1` must yield three pages, every row
// once, in id DESC order — the mutation `evaluated_at < $ts` returns one row and stops.
func TestGateBListingCursorTiebreakOnSameInstantRows(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	t0 := gateTodayDB(t, st, ctx).Add(3 * time.Hour)
	ids := []string{
		gateInsertRow(t, st, ctx, f, t0, domain.GateStateAllow, nil),
		gateInsertRow(t, st, ctx, f, t0, domain.GateStateAllow, nil),
		gateInsertRow(t, st, ctx, f, t0, domain.GateStateAllow, nil),
	}
	// A neighbour a second later, to prove the same-instant group is ordered below it as a block.
	newer := gateInsertRow(t, st, ctx, f, t0.Add(time.Second), domain.GateStateAllow, nil)

	pages := gateListAll(t, st, ctx, f.projectID, t0, t0.Add(time.Minute), &f.serviceID, nil, 1)
	got := gateIDsOf(pages)
	if len(pages) != 4 || len(got) != 4 {
		t.Fatalf("limit=1 over four rows: %d pages, %d rows — %v", len(pages), len(got), got)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	want := append([]string{newer}, ids...)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("traversal = %v, want %v (evaluated_at DESC, id DESC; same instant broken by id)", got, want)
	}
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
		if seen[id] > 1 {
			t.Errorf("key %s returned twice", id)
		}
	}
	// A cursor EQUAL to a row (its own key) never returns that row again.
	cur := &GateCursor{EvaluatedAt: t0, ID: ids[0]}
	items, _, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Minute), &f.serviceID, nil, cur, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.DecisionID == ids[0] {
			t.Errorf("the cursor row %s was returned again", ids[0])
		}
		if it.DecisionID > ids[0] {
			t.Errorf("row %s (id above the cursor at the same instant) returned below the cursor", it.DecisionID)
		}
	}
	if len(items) != 2 {
		t.Errorf("below cursor (%s at t0): %d rows, want the two smaller ids", ids[0], len(items))
	}
}
