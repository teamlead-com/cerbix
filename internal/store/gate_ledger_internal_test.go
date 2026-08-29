package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-024 store core (func-reliability-gate §5 identity + listing, §7 — *Ledger*, *Identity and
// reads*, *Attribution*).

// gateTodayDB is the database's UTC day start — the day the bootstrap partitions begin at.
func gateTodayDB(t *testing.T, st *Store, ctx context.Context) time.Time {
	t.Helper()
	var d time.Time
	if err := st.pool.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

// gateInsertRow writes a synthetic ledger row at a chosen instant through the store's own
// writer, so the id is bound to evaluated_at exactly as a live decision's is.
func gateInsertRow(t *testing.T, st *Store, ctx context.Context, f gateFixture, at time.Time, state domain.GateState, override *string) string {
	t.Helper()
	id, err := newGateDecisionID(at)
	if err != nil {
		t.Fatal(err)
	}
	sid := f.serviceID
	dec := domain.GateDecision{
		SchemaVersion: domain.GateDecisionSchemaV1, DecisionID: id, EvaluatedAt: at,
		ServiceID: &sid, ServiceSlug: "checkout", ServiceName: "Checkout", State: state,
		Reasons: []domain.GateReasonEntry{},
	}
	var policy *domain.GatePolicy
	if state != domain.GateStateNotConfigured {
		act := domain.GateActionAllow
		if state == domain.GateStateBlock {
			act = domain.GateActionBlock
		}
		rev := int64(1)
		dec.Action, dec.PolicyRevision = &act, &rev
		p := domain.GatePolicy{Window: gateWindow, SchemaVersion: 1, Revision: 1,
			Clauses:               map[domain.GateClause]domain.ClauseAssignment{domain.ClauseBudgetExhausted: domain.ClauseAssignBlock},
			BudgetConsumedPercent: 90, MaxSealLagSeconds: 900, UnknownBehavior: domain.GateUnknownWarn}
		policy = &p
		w := gateWindow
		dec.Window = &w
		if override != nil {
			dec.OverrideID = override
			allow := domain.GateActionAllow
			dec.Action, dec.UnoverriddenAction = &allow, &act
			dec.Override = &domain.GateOverrideApplied{ID: *override, ActorLabel: "token:ci", Reason: "r", ExpiresAt: at.Add(time.Hour)}
		}
	} else {
		dec.Reasons = []domain.GateReasonEntry{{Code: "not_configured", Docs: domain.GateDocsURL}}
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertGateDecisionTx(ctx, tx, f.projectID, dec, policy); err != nil {
		t.Fatalf("insert row at %s: %v", at, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func gateListAll(t *testing.T, st *Store, ctx context.Context, projectID string, from, to time.Time, serviceID *string, limit int) [][]domain.GateDecisionSummary {
	t.Helper()
	var pages [][]domain.GateDecisionSummary
	var cursor *GateCursor
	for {
		items, next, err := st.ListGateDecisions(ctx, projectID, from, to, serviceID, cursor, limit)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		pages = append(pages, items)
		if next == nil {
			return pages
		}
		cursor = next
	}
}

func gateIDsOf(pages [][]domain.GateDecisionSummary) []string {
	var ids []string
	for _, p := range pages {
		for _, it := range p {
			ids = append(ids, it.DecisionID)
		}
	}
	return ids
}

// ── Identity (§5; invariant 12) ──────────────────────────────────────────────────────────

// The id's millisecond is evaluated_at's — the DATABASE instant handed in — whatever the
// application clock says: a row written 1 ms before midnight belongs to that day even when the
// application clock is 5 s into the next one (the id is built from the instant, never from
// time.Now()), and the database CHECKs the binding on every row written.
func TestGateDecisionIDIsBoundToEvaluatedAt(t *testing.T) {
	at := time.Date(2026, 8, 29, 23, 59, 59, 999_000_000, time.UTC) // 1 ms before midnight
	id, err := newGateDecisionID(at)
	if err != nil {
		t.Fatal(err)
	}
	ms, ok := gateDecisionIDMillis(id)
	if !ok || ms != at.UnixMilli() {
		t.Fatalf("id %s carries %d, want %d", id, ms, at.UnixMilli())
	}
	if id[14] != '7' {
		t.Errorf("version nibble = %c, want 7", id[14])
	}
	if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("variant nibble = %c, want 8|9|a|b", v)
	}
	from, to, ok := gateDecisionDay(id)
	if !ok || !from.Equal(time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day of %s = [%s, %s)", id, from, to)
	}
	// Two ids for one instant differ in their random bits.
	id2, _ := newGateDecisionID(at)
	if id2 == id {
		t.Error("two ids for one millisecond collided")
	}
	for _, bad := range []string{"", "nope", strings.ReplaceAll(id, "-", ""), id[:35] + "g", id + "0"} {
		if _, ok := gateDecisionIDMillis(bad); ok {
			t.Errorf("%q parsed as an id", bad)
		}
	}

	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)
	var bound bool
	if err := st.pool.QueryRow(ctx, `
		SELECT gate_uuid_ms(id) = floor(extract(epoch FROM evaluated_at) * 1000)
		  FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&bound); err != nil || !bound {
		t.Errorf("the row's id is not bound to its evaluated_at: %v %v", bound, err)
	}
	if ms, _ := gateDecisionIDMillis(dec.DecisionID); ms != dec.EvaluatedAt.UnixMilli() {
		t.Errorf("id ms %d, evaluated_at ms %d", ms, dec.EvaluatedAt.UnixMilli())
	}
}

// ── Ledger (D10; invariant 12) ───────────────────────────────────────────────────────────

func TestGateLedgerRowSurvivesRenameAndDeletion(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)
	if dec.ServiceSlug != "checkout" || dec.ServiceName != "Checkout" {
		t.Fatalf("snapshot slug/name = %s/%s", dec.ServiceSlug, dec.ServiceName)
	}

	// Rename: the row keeps the old slug and name.
	if _, err := st.pool.Exec(ctx, `UPDATE services SET slug = 'payments-checkout', name = 'Payments Checkout' WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil || got.ServiceSlug != "checkout" || got.ServiceName != "Checkout" {
		t.Errorf("after rename: %s/%s %v", got.ServiceSlug, got.ServiceName, err)
	}
	// Delete: the row survives with service_id NULL and is still readable; the listing carries
	// service_id present-and-null.
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	got, err = st.GetGateDecision(ctx, f.projectID, dec.DecisionID)
	if err != nil {
		t.Fatalf("after delete the row must be readable: %v", err)
	}
	if got.ServiceID != nil || got.ServiceSlug != "checkout" || got.State != domain.GateStateAllow || got.PolicyRevision == nil {
		t.Errorf("after delete: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"service_id":null`) {
		t.Errorf("service_id must be present-and-null after deletion: %s", raw)
	}
	pages := gateListAll(t, st, ctx, f.projectID, dec.EvaluatedAt.Add(-time.Minute), dec.EvaluatedAt.Add(time.Minute), nil, 10)
	if ids := gateIDsOf(pages); len(ids) != 1 || ids[0] != dec.DecisionID {
		t.Errorf("listing after delete = %v", ids)
	}
	// Tenant and identity: a foreign project, a malformed id and an unknown id are not found.
	if _, err := st.GetGateDecision(ctx, "00000000-0000-0000-0000-000000000001", dec.DecisionID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign project = %v", err)
	}
	if _, err := st.GetGateDecision(ctx, f.projectID, "not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id = %v", err)
	}
	other, _ := newGateDecisionID(dec.EvaluatedAt)
	if _, err := st.GetGateDecision(ctx, f.projectID, other); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id = %v", err)
	}
}

// planChildren walks an EXPLAIN (FORMAT JSON) plan and returns every scanned relation with the
// index it used and any post-scan filter.
type planScan struct{ relation, index, filter, indexCond string }

func planChildren(t *testing.T, plan json.RawMessage) []planScan {
	t.Helper()
	var root []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(plan, &root); err != nil {
		t.Fatalf("explain json: %v\n%s", err, plan)
	}
	var out []planScan
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if rel, ok := n["Relation Name"].(string); ok {
			s := planScan{relation: rel}
			s.index, _ = n["Index Name"].(string)
			s.filter, _ = n["Filter"].(string)
			s.indexCond, _ = n["Index Cond"].(string)
			out = append(out, s)
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				walk(k.(map[string]any))
			}
		}
	}
	walk(root[0].Plan)
	return out
}

func gateExplain(t *testing.T, st *Store, ctx context.Context, sql string, args ...any) []planScan {
	t.Helper()
	var plan json.RawMessage
	if err := st.pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return planChildren(t, plan)
}

// The by-id read prunes to ONE child for a real id, and to ZERO children for a day that was
// never created — 404 both ways, with no application clock anywhere.
func TestGateLedgerByIdPrunesToOnePartition(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	dec := gateDecide(t, st, ctx, f)

	from, to, _ := gateDecisionDay(dec.DecisionID)
	scans := gateExplain(t, st, ctx, gateDecisionByIDSQL, dec.DecisionID, from, to, f.projectID)
	if len(scans) != 1 || !strings.HasPrefix(scans[0].relation, "service_gate_decisions_p") {
		t.Errorf("by-id plan scans %+v, want exactly one child", scans)
	}
	// A day 30 days ago: no partition was ever created — zero children, not found.
	old := gateTodayDB(t, st, ctx).Add(-30 * 24 * time.Hour).Add(12 * time.Hour)
	oldID, _ := newGateDecisionID(old)
	ofrom, oto, _ := gateDecisionDay(oldID)
	if scans := gateExplain(t, st, ctx, gateDecisionByIDSQL, oldID, ofrom, oto, f.projectID); len(scans) != 0 {
		t.Errorf("never-created day scans %+v, want none", scans)
	}
	if _, err := st.GetGateDecision(ctx, f.projectID, oldID); !errors.Is(err, ErrNotFound) {
		t.Errorf("never-created day = %v, want ErrNotFound", err)
	}
}

// ── Listing (§5 listing contract) ────────────────────────────────────────────────────────

func TestGateLedgerListingContract(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	today := gateTodayDB(t, st, ctx)
	t0 := today.Add(time.Hour)
	// Three fixed rows a minute apart; two more on a second service for the filter.
	ids := []string{
		gateInsertRow(t, st, ctx, f, t0, domain.GateStateAllow, nil),
		gateInsertRow(t, st, ctx, f, t0.Add(time.Minute), domain.GateStateBlock, nil),
		gateInsertRow(t, st, ctx, f, t0.Add(2*time.Minute), domain.GateStateNotConfigured, nil),
	}
	other := f
	if err := st.pool.QueryRow(ctx, `INSERT INTO services (project_id, slug, name) VALUES ($1, 'other', 'Other') RETURNING id`, f.projectID).Scan(&other.serviceID); err != nil {
		t.Fatal(err)
	}
	otherIDs := []string{
		gateInsertRow(t, st, ctx, other, t0.Add(30*time.Second), domain.GateStateWarn, nil),
		gateInsertRow(t, st, ctx, other, t0.Add(90*time.Second), domain.GateStateWarn, nil),
	}

	// Refusals.
	if _, _, err := st.ListGateDecisions(ctx, f.projectID, t0, t0, nil, nil, 10); validationField(t, err) != "range" {
		t.Errorf("to == from named %v", err)
	}
	if _, _, err := st.ListGateDecisions(ctx, f.projectID, t0.Add(time.Minute), t0, nil, nil, 10); validationField(t, err) != "range" {
		t.Errorf("to < from named %v", err)
	}
	if _, _, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), nil, nil, 0); validationField(t, err) != "limit" {
		t.Errorf("limit 0 named %v", err)
	}
	if _, _, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), nil, nil, GateListLimitMax+1); validationField(t, err) != "limit" {
		t.Errorf("limit 201 named %v", err)
	}

	// [from, to): the row at exactly `to` excluded, the row at exactly `from` included; DESC order.
	items, next, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(2*time.Minute), &f.serviceID, nil, 50)
	if err != nil || next != nil || len(items) != 2 || items[0].DecisionID != ids[1] || items[1].DecisionID != ids[0] {
		t.Errorf("[from,to) page = %v next=%v err=%v", gateIDsOf([][]domain.GateDecisionSummary{items}), next, err)
	}

	// limit = 2 over three rows: 2 + 1, no gap, the cursor from the LAST RETURNED row (the
	// mutation that encodes the probe row would skip ids[0]).
	page1, next, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), &f.serviceID, nil, 2)
	if err != nil || len(page1) != 2 || next == nil {
		t.Fatalf("page 1 = %d items next=%v err=%v", len(page1), next, err)
	}
	if next.ID != page1[1].DecisionID || !next.EvaluatedAt.Equal(page1[1].EvaluatedAt) {
		t.Errorf("cursor %+v is not the last returned row %s", next, page1[1].DecisionID)
	}
	// The cursor is opaque and round-trips strictly.
	decoded, err := DecodeGateCursor(next.Encode())
	if err != nil || decoded.ID != next.ID || !decoded.EvaluatedAt.Equal(next.EvaluatedAt) {
		t.Errorf("cursor round-trip: %+v %v", decoded, err)
	}
	for _, bad := range []string{"", "!!!", "bm90LWEtY3Vyc29y", "MTIzNDU2", "MTIzOm5vcGU", "eDoxMjM0NTY3OC0xMjM0LTEyMzQtMTIzNC0xMjM0NTY3ODkwYWI"} {
		if _, err := DecodeGateCursor(bad); !errors.Is(err, ErrGateCursorInvalid) {
			t.Errorf("cursor %q decoded: %v", bad, err)
		}
	}
	page2, next2, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), &f.serviceID, &decoded, 2)
	if err != nil || len(page2) != 1 || next2 != nil || page2[0].DecisionID != ids[0] {
		t.Errorf("page 2 = %v next=%v err=%v", gateIDsOf([][]domain.GateDecisionSummary{page2}), next2, err)
	}
	all := append(gateIDsOf([][]domain.GateDecisionSummary{page1}), gateIDsOf([][]domain.GateDecisionSummary{page2})...)
	if strings.Join(all, ",") != ids[2]+","+ids[1]+","+ids[0] {
		t.Errorf("traversal = %v, want newest first with no gap and no repeat", all)
	}
	// Exactly limit rows: no next page.
	if items, next, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), &f.serviceID, nil, 3); err != nil || len(items) != 3 || next != nil {
		t.Errorf("exact fit: %d items next=%v err=%v", len(items), next, err)
	}

	// No service filter: both services interleaved by time; a foreign service: an empty page.
	if got := gateIDsOf(gateListAll(t, st, ctx, f.projectID, t0, t0.Add(time.Hour), nil, 2)); strings.Join(got, ",") != strings.Join([]string{ids[2], otherIDs[1], ids[1], otherIDs[0], ids[0]}, ",") {
		t.Errorf("project-wide traversal = %v", got)
	}
	foreign := "00000000-0000-0000-0000-000000000001"
	if items, next, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), &foreign, nil, 10); err != nil || len(items) != 0 || next != nil || items == nil {
		t.Errorf("foreign service = %v %v %v (want an empty, non-nil page)", items, next, err)
	}
	if items, _, err := st.ListGateDecisions(ctx, foreign, t0, t0.Add(time.Hour), nil, nil, 10); err != nil || len(items) != 0 {
		t.Errorf("foreign project = %v %v", items, err)
	}

	// Item presence follows the by-id contract: NOT_CONFIGURED has no action/policy_revision.
	for _, it := range page1 {
		keys := gateJSONKeys(t, it)
		if it.State == domain.GateStateNotConfigured {
			if contains(keys, "action") || contains(keys, "policy_revision") || contains(keys, "override_id") {
				t.Errorf("NOT_CONFIGURED item keys %v", keys)
			}
		} else if !contains(keys, "action") || !contains(keys, "policy_revision") {
			t.Errorf("configured item keys %v", keys)
		}
	}
}

// With a writer inserting throughout the traversal: every row committed before page 1 exactly
// once, no returned key repeated. Rows committed during the traversal may appear or not.
func TestGateLedgerListingUnderAConcurrentWriter(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	today := gateTodayDB(t, st, ctx)
	t0 := today.Add(2 * time.Hour)
	pre := map[string]bool{}
	for i := 0; i < 40; i++ {
		pre[gateInsertRow(t, st, ctx, f, t0.Add(time.Duration(i)*time.Second), domain.GateStateAllow, nil)] = true
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Inserts land INSIDE the traversed range, between existing keys, and after it.
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			at := t0.Add(time.Duration(i%40)*time.Second + 500*time.Millisecond)
			if i%3 == 0 {
				at = t0.Add(time.Minute + time.Duration(i)*time.Millisecond)
			}
			gateInsertRow(t, st, ctx, f, at, domain.GateStateAllow, nil)
		}
	}()
	var seen []string
	var cursor *GateCursor
	for {
		items, next, err := st.ListGateDecisions(ctx, f.projectID, t0, t0.Add(time.Hour), nil, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			seen = append(seen, it.DecisionID)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	close(stop)
	wg.Wait()
	counts := map[string]int{}
	for _, id := range seen {
		counts[id]++
		if counts[id] > 1 {
			t.Errorf("key %s returned twice", id)
		}
	}
	for id := range pre {
		if counts[id] != 1 {
			t.Errorf("pre-traversal row %s returned %d times", id, counts[id])
		}
	}
}

// A decision transaction that took its statement_timestamp() before page 1 and commits after
// it is NOT asserted to appear: the traversal has no duplicate and the row is readable by id
// afterwards — the live-keyset limitation made explicit.
func TestGateLedgerListingWithALongDecisionHeldOpen(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	for i := 0; i < 3; i++ {
		gateDecide(t, st, ctx, f)
	}
	release, held := make(chan struct{}), make(chan struct{})
	gateDecisionHook = func(_ context.Context, _ int, phase string, _ pgx.Tx) error {
		if phase == gatePhaseReadsDone {
			close(held)
			<-release
		}
		return nil
	}
	t.Cleanup(func() { gateDecisionHook = nil })
	var late domain.GateDecision
	var lateErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		late, lateErr = st.DecideGate(ctx, f.projectID, f.serviceID, 30*time.Second)
	}()
	<-held
	from, to := gateTodayDB(t, st, ctx), gateTodayDB(t, st, ctx).Add(24*time.Hour)
	page1, next, err := st.ListGateDecisions(ctx, f.projectID, from, to, nil, nil, 2)
	if err != nil || len(page1) != 2 || next == nil {
		t.Fatalf("page 1: %d %v %v", len(page1), next, err)
	}
	close(release)
	<-done
	if lateErr != nil {
		t.Fatalf("the held decision failed: %v", lateErr)
	}
	seen := map[string]int{}
	for _, it := range page1 {
		seen[it.DecisionID]++
	}
	cursor := next
	for cursor != nil {
		items, n, err := st.ListGateDecisions(ctx, f.projectID, from, to, nil, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			seen[it.DecisionID]++
		}
		cursor = n
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("key %s returned %d times", id, n)
		}
	}
	if _, err := st.GetGateDecision(ctx, f.projectID, late.DecisionID); err != nil {
		t.Errorf("the late decision is not readable by id: %v", err)
	}
}

// gateBulkRows plants `n` ALLOW rows for a project on one child, 20 ms apart from `base`,
// with ids built in SQL the way the writer builds them (48-bit ms, version 7, variant 10), so a
// populated, representative child can be ANALYZEd without 10 000 round trips.
func gateBulkRows(t *testing.T, st *Store, ctx context.Context, projectID string, serviceID *string, base time.Time, n int) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_gate_decisions
		    (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
		     policy_revision, window_name, policy_snapshot, evaluated_at)
		SELECT (lpad(to_hex(floor(extract(epoch FROM gs) * 1000)::bigint), 12, '0')
		        || '7' || substr(h, 1, 3)
		        || substr('89ab', 1 + (('x' || substr(h, 4, 1))::bit(4)::int % 4), 1)
		        || substr(h, 5, 3) || substr(h, 8, 12))::uuid,
		       $1, $2, 'bulk', 'Bulk', 'ALLOW', 'ALLOW', '[]', '{}', 1, '24h', '{"revision":1}', gs
		  FROM (SELECT gs, md5(random()::text || gs::text) AS h
		          FROM generate_series($3::timestamptz, $3::timestamptz + ($4 - 1) * interval '20 milliseconds', interval '20 milliseconds') gs) x`,
		projectID, serviceID, base, n); err != nil {
		t.Fatalf("bulk rows: %v", err)
	}
}

// Pruning and index use: a 31-day page is one statement over ≤ 32 children; on ANALYZEd,
// populated, REPRESENTATIVE children (several projects, so project_id is selective) each scan
// runs on the matching index with no post-filter; a range inside one day shows exactly one child.
func TestGateLedgerListingPlan(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	today := gateTodayDB(t, st, ctx)
	sparse := f
	if err := st.pool.QueryRow(ctx, `INSERT INTO services (project_id, slug, name) VALUES ($1, 'sparse', 'Sparse') RETURNING id`, f.projectID).Scan(&sparse.serviceID); err != nil {
		t.Fatal(err)
	}
	var orgID string
	if err := st.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, f.projectID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	others := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		p, err := st.CreateProject(ctx, orgID, fmt.Sprintf("other-%d", i), fmt.Sprintf("Other %d", i))
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, p.ID)
	}
	// Two children: 3 000 rows for the fixture project (2 990 busy + 10 sparse) and 3 000 for
	// each of three other projects per child.
	for day := 0; day < 2; day++ {
		base := today.Add(time.Duration(day)*24*time.Hour + 3*time.Hour)
		gateBulkRows(t, st, ctx, f.projectID, &f.serviceID, base, 2990)
		for _, o := range others {
			gateBulkRows(t, st, ctx, o, nil, base.Add(2*time.Hour), 3000)
		}
		for i := 0; i < 5; i++ {
			gateInsertRow(t, st, ctx, sparse, base.Add(time.Duration(i)*time.Minute), domain.GateStateAllow, nil)
		}
	}
	if _, err := st.pool.Exec(ctx, `ANALYZE service_gate_decisions`); err != nil {
		t.Fatal(err)
	}
	populated := func(rel string) bool {
		return strings.HasSuffix(rel, "p"+today.Format("20060102")) || strings.HasSuffix(rel, "p"+today.Add(24*time.Hour).Format("20060102"))
	}
	listSQL := func(withService bool) (string, []any) {
		sql := `SELECT` + gateSummaryColumns + ` FROM service_gate_decisions WHERE project_id = $1 AND evaluated_at >= $2 AND evaluated_at < $3`
		args := []any{f.projectID, today.Add(-29 * 24 * time.Hour), today.Add(2 * 24 * time.Hour)}
		if withService {
			args = append(args, sparse.serviceID)
			sql += " AND service_id = $4"
		}
		sql += fmt.Sprintf(" ORDER BY evaluated_at DESC, id DESC LIMIT $%d", len(args)+1)
		return sql, append(args, 51)
	}
	// 31 days → ≤ 32 children (here: the attached bootstrap days inside the range).
	sql, args := listSQL(false)
	scans := gateExplain(t, st, ctx, sql, args...)
	if len(scans) == 0 || len(scans) > 32 {
		t.Fatalf("31-day page scans %d children", len(scans))
	}
	seenPopulated := 0
	for _, s := range scans {
		if !populated(s.relation) {
			continue
		}
		seenPopulated++
		if !strings.Contains(s.index, "project_id_evaluated_at_id") || strings.Contains(s.filter, "service_id") || strings.Contains(s.filter, "project_id") {
			t.Errorf("populated child %s: index=%q filter=%q, want the (project_id, evaluated_at, id) path with no post-filter", s.relation, s.index, s.filter)
		}
	}
	if seenPopulated != 2 {
		t.Errorf("the plan touched %d populated children, want both", seenPopulated)
	}
	sql, args = listSQL(true)
	for _, s := range gateExplain(t, st, ctx, sql, args...) {
		if !populated(s.relation) {
			continue
		}
		if !strings.Contains(s.index, "project_id_service_id") || strings.Contains(s.filter, "service_id") {
			t.Errorf("sparse-service child %s: index=%q filter=%q, want the (project_id, service_id, …) path with no service_id filter", s.relation, s.index, s.filter)
		}
	}
	// A range inside one day: exactly one child.
	one := gateExplain(t, st, ctx, `SELECT`+gateSummaryColumns+` FROM service_gate_decisions WHERE project_id = $1 AND evaluated_at >= $2 AND evaluated_at < $3 ORDER BY evaluated_at DESC, id DESC LIMIT 51`,
		f.projectID, today.Add(time.Hour), today.Add(5*time.Hour))
	if len(one) != 1 {
		t.Errorf("one-day range scans %+v, want exactly one child", one)
	}
	// Semantics on the populated fixture: the sparse service's ten rows, newest first, in 3 pages,
	// each page one statement over the same plan.
	pages := gateListAll(t, st, ctx, f.projectID, today.Add(-29*24*time.Hour), today.Add(2*24*time.Hour), &sparse.serviceID, 4)
	got := gateIDsOf(pages)
	if len(pages) != 3 || len(got) != 10 {
		t.Errorf("sparse traversal: %d pages, %d rows", len(pages), len(got))
	}
	for i := 1; i < len(got); i++ {
		// UUIDv7 text sorts by time within a day, and the two days are ordered too.
		if got[i] >= got[i-1] {
			t.Errorf("sparse traversal not newest-first at %d: %s then %s", i, got[i-1], got[i])
		}
	}
}

// Raw-JSON presence over four rows — NOT_CONFIGURED, non-overridden, overridden,
// deleted-service — matches the by-id response's field set (the summary is a strict
// projection; every field it carries is present exactly when the by-id response carries it).
func TestGateLedgerListingPresenceMatchesByID(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateLatch(t, st, ctx, f, gatePageKey, true, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 90*time.Second)

	notConfigured := gateDecide(t, st, ctx, f)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	plain := gateDecide(t, st, ctx, f)
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "r", gateDBNow(t, st, ctx).Add(time.Hour), gateActorToken); err != nil {
		t.Fatal(err)
	}
	overridden := gateDecide(t, st, ctx, f)
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatal(err)
	}
	from, to := notConfigured.EvaluatedAt.Add(-time.Second), overridden.EvaluatedAt.Add(time.Second)
	items, next, err := st.ListGateDecisions(ctx, f.projectID, from, to, nil, nil, 10)
	if err != nil || next != nil || len(items) != 3 {
		t.Fatalf("listing: %d %v %v", len(items), next, err)
	}
	for _, it := range items {
		byID, err := st.GetGateDecision(ctx, f.projectID, it.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		itemKeys, fullKeys := gateJSONKeys(t, it), gateJSONKeys(t, byID)
		for _, k := range itemKeys {
			if !contains(fullKeys, k) {
				t.Errorf("%s item carries %s which the by-id response lacks", it.State, k)
			}
		}
		for _, k := range []string{"action", "policy_revision", "override_id"} {
			if contains(itemKeys, k) != contains(fullKeys, k) {
				t.Errorf("%s: presence of %s differs between item (%v) and by-id (%v)", it.State, k, contains(itemKeys, k), contains(fullKeys, k))
			}
		}
		raw, _ := json.Marshal(it)
		if !strings.Contains(string(raw), `"service_id":null`) || !strings.Contains(string(raw), `"service_slug":"checkout"`) {
			t.Errorf("deleted-service item must carry service_id null and the slug: %s", raw)
		}
		switch it.State {
		case domain.GateStateNotConfigured:
			if contains(itemKeys, "action") || contains(itemKeys, "policy_revision") || contains(itemKeys, "override_id") {
				t.Errorf("NOT_CONFIGURED item keys %v", itemKeys)
			}
		case domain.GateStateBlock:
			if !contains(itemKeys, "action") || !contains(itemKeys, "policy_revision") {
				t.Errorf("BLOCK item keys %v", itemKeys)
			}
			if it.DecisionID == overridden.DecisionID && (!contains(itemKeys, "override_id") || actionOf(byID) != "ALLOW") {
				t.Errorf("overridden item keys %v action %s", itemKeys, actionOf(byID))
			}
			if it.DecisionID == plain.DecisionID && contains(itemKeys, "override_id") {
				t.Errorf("non-overridden item keys %v", itemKeys)
			}
		}
	}
}
