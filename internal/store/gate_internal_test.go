package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-024 store core (func-reliability-gate §7 — *Policy writes*, *Override*, *Attribution*).
// Fixtures plant SEALED facts the way the report tests do (plantRange/setWatermark) and seed
// burn latches the way the arming tests do, so the gate is exercised against the owners'
// own storage shapes, never against a second way of writing them.

func gateStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run reliability gate store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// The ledger is cascaded with its project by TruncateAll; the test host may also carry
	// rows from another project-less run, so the ledger is emptied explicitly too.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_gate_decisions`); err != nil {
		t.Fatalf("empty ledger: %v", err)
	}
	return st, ctx
}

// gateActorToken is the machine principal every mutation here uses unless a test says
// otherwise: the typed half is NULL + true, the label names the token (D9).
var gateActorToken = GateActor{ViaToken: true, Label: "token:ci"}

// gateWindow is the fixture's SLO window. 24h keeps a full-continuity plant at 1 440 buckets.
const gateWindow = "24h"

// Two rules on the fixture target: one page, one ticket. Canonical keys below.
const (
	gatePageRule   = `{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page"}`
	gateTicketRule = `{"long_window_seconds":21600,"short_window_seconds":1800,"threshold":6,"severity":"ticket"}`
	gatePageKey    = "page/3600/300/14.4"
	gateTicketKey  = "ticket/21600/1800/6"
)

// gateFixture is an adopted service with a service-scoped 24h target carrying a page and a
// ticket rule, a materialization watermark and a fully sealed 24h window.
type gateFixture struct {
	reportFixture
	targetID string
	// sealed is the watermark; the sealed window is [sealed − 24h, sealed).
	sealed time.Time
}

// gateDBNow is the database's clock — the one every gate comparison uses.
func gateDBNow(t *testing.T, st *Store, ctx context.Context) time.Time {
	t.Helper()
	now, err := dbNow(ctx, st.pool)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// gateService builds the fixture with the watermark `lag` behind the database clock and every
// bucket carrying the given µs split (good + bad = one minute). Objective 50 makes the
// arithmetic exact: bad/measured = 0.25 → BurnedPercent = 50 exactly, 0.5 → 100.
func gateService(t *testing.T, st *Store, ctx context.Context, lag time.Duration, goodUs, badUs int64) gateFixture {
	t.Helper()
	f := gateFixture{reportFixture: reportService(t, st, ctx)}
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO sla_targets (service_id, window_name, objective, burn_alert_enabled, burn_rules)
		VALUES ($1, $2, 50, true, $3::jsonb)
		RETURNING id`, f.serviceID, gateWindow, "["+gatePageRule+","+gateTicketRule+"]").Scan(&f.targetID); err != nil {
		t.Fatalf("target: %v", err)
	}
	f.sealed = gateDBNow(t, st, ctx).Add(-lag)
	gateReplant(t, st, ctx, &f, lag, goodUs, badUs)
	return f
}

// gateReplant moves the watermark to `lag` behind the database clock and re-plants the whole
// window with the split.
func gateReplant(t *testing.T, st *Store, ctx context.Context, f *gateFixture, lag time.Duration, goodUs, badUs int64) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear facts: %v", err)
	}
	f.sealed = gateDBNow(t, st, ctx).Add(-lag)
	from := f.sealed.Add(-24 * time.Hour)
	plantRange(t, st, ctx, f.reportFixture, f.epochID, from, f.sealed, goodUs, badUs, 0, 0, "sealed")
	setWatermark(t, st, ctx, f.reportFixture, from.Add(-time.Hour), f.sealed)
}

// gateUnseal removes the watermark: the service has sealed nothing.
func gateUnseal(t *testing.T, st *Store, ctx context.Context, f gateFixture) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_reliability_buckets WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear facts: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_materialization SET sealed_through = NULL WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("unseal: %v", err)
	}
}

// gateLatch seeds one rule's latch on the fixture target: level, verdict and a lease that ends
// `lease` after the database clock (negative = expired).
func gateLatch(t *testing.T, st *Store, ctx context.Context, f gateFixture, ruleKey string, firing bool, lease time.Duration) {
	t.Helper()
	verdict := "clear"
	if firing {
		verdict = "fire"
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict, emitted_seq,
		   target_generation, config_generation, evaluated_at, lease_until)
		SELECT s.id, s.project_id, t.id, $3, $4, $5, CASE WHEN $4 THEN 1 ELSE 0 END,
		       t.alert_generation, s.alert_config_generation, now(), now() + $6::interval
		  FROM services s JOIN sla_targets t ON t.service_id = s.id
		 WHERE s.id = $1 AND t.id = $2
		ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO UPDATE SET
		   firing = EXCLUDED.firing, last_verdict = EXCLUDED.last_verdict, emitted_seq = EXCLUDED.emitted_seq,
		   evaluated_at = EXCLUDED.evaluated_at, lease_until = EXCLUDED.lease_until, last_error = NULL`,
		f.serviceID, f.targetID, ruleKey, firing, verdict, pgInterval(lease)); err != nil {
		t.Fatalf("latch %s: %v", ruleKey, err)
	}
}

// gateFreshLatches seeds both rules quiet and fresh — the ALLOW baseline.
func gateFreshLatches(t *testing.T, st *Store, ctx context.Context, f gateFixture) {
	t.Helper()
	gateLatch(t, st, ctx, f, gatePageKey, false, 90*time.Second)
	gateLatch(t, st, ctx, f, gateTicketKey, false, 90*time.Second)
}

func pgInterval(d time.Duration) string {
	return strings.TrimSuffix(d.String(), "0s")
}

// gateOpenIncident opens the service's auto-incident through the owner.
func gateOpenIncident(t *testing.T, st *Store, ctx context.Context, f gateFixture) string {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inc, _, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout is down", 2, &f.revisionID)
	if err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return inc.ID
}

// gateDoc is the shipped template (D11) as a policy document for the fixture window, with
// the given assignments overriding the defaults.
func gateDoc(assign map[domain.GateClause]domain.ClauseAssignment) domain.GatePolicyDocument {
	defaults := map[domain.GateClause]domain.ClauseAssignment{
		domain.ClauseBudgetExhausted:     domain.ClauseAssignBlock,
		domain.ClauseBudgetConsumed:      domain.ClauseAssignWarn,
		domain.ClausePageBurnFiring:      domain.ClauseAssignBlock,
		domain.ClauseTicketBurnFiring:    domain.ClauseAssignWarn,
		domain.ClauseServiceIncidentOpen: domain.ClauseAssignWarn,
	}
	for c, a := range assign {
		defaults[c] = a
	}
	doc := domain.GatePolicyDocument{
		SchemaVersion: domain.GatePolicySchemaV1, Window: gateWindow,
		BudgetConsumedPercent: 90, MaxSealLagSeconds: 900, UnknownBehavior: domain.GateUnknownWarn,
	}
	for _, c := range domain.GateClausesV1 {
		doc.Clauses = append(doc.Clauses, domain.GateClauseEntry{Clause: c, Assignment: defaults[c]})
	}
	return doc
}

func gatePut(t *testing.T, st *Store, ctx context.Context, f gateFixture, expected *int64, doc domain.GatePolicyDocument) int64 {
	t.Helper()
	rev, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, expected, doc, gateActorToken)
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	return rev
}

func gateAuditCount(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action LIKE 'gate.%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func int64p(v int64) *int64 { return &v }

func policyErrField(t *testing.T, err error) string {
	t.Helper()
	var pe *domain.GatePolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a *domain.GatePolicyError", err)
	}
	return pe.Field
}

func validationField(t *testing.T, err error) string {
	t.Helper()
	var ve *GateValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v, want a *GateValidationError", err)
	}
	return ve.Field
}

// ── Policy writes (D11, D13a, D14; invariant 11) ─────────────────────────────────────────

func TestGatePolicyWriteRefusesByName(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)

	put := func(doc domain.GatePolicyDocument) error {
		_, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, nil, doc, gateActorToken)
		return err
	}
	missing := gateDoc(nil)
	missing.Clauses = missing.Clauses[1:]
	if got := policyErrField(t, put(missing)); got != "clauses.budget_exhausted" {
		t.Errorf("missing clause named %q, want clauses.budget_exhausted", got)
	}
	unknown := gateDoc(nil)
	unknown.Clauses = append(unknown.Clauses, domain.GateClauseEntry{Clause: "coverage_not_armed", Assignment: "block"})
	if got := policyErrField(t, put(unknown)); got != "clauses.coverage_not_armed" {
		t.Errorf("unknown clause named %q", got)
	}
	dup := gateDoc(nil)
	dup.Clauses = append(dup.Clauses, domain.GateClauseEntry{Clause: domain.ClauseBudgetConsumed, Assignment: "warn"})
	if got := policyErrField(t, put(dup)); got != "clauses.budget_consumed" {
		t.Errorf("duplicate clause named %q", got)
	}
	noUnknown := gateDoc(nil)
	noUnknown.UnknownBehavior = ""
	if got := policyErrField(t, put(noUnknown)); got != "unknown_behavior" {
		t.Errorf("missing unknown_behavior named %q", got)
	}
	noTarget := gateDoc(nil)
	noTarget.Window = "7d" // a real SLA window the service has no target for
	if got := policyErrField(t, put(noTarget)); got != "window" {
		t.Errorf("window without target named %q", got)
	}
	notAWindow := gateDoc(nil)
	notAWindow.Window = "13d"
	if got := policyErrField(t, put(notAWindow)); got != "window" {
		t.Errorf("unknown window named %q", got)
	}
	if n := gateAuditCount(t, st, ctx); n != 0 {
		t.Errorf("refused writes left %d audit rows", n)
	}
	if _, err := st.GetGatePolicy(ctx, f.projectID, f.serviceID); !errors.Is(err, ErrGatePolicyNotConfigured) {
		t.Errorf("after refusals GetGatePolicy = %v, want ErrGatePolicyNotConfigured", err)
	}
	// Tenant: a foreign project answers ErrNotFound on read and write, not "not configured".
	if _, err := st.GetGatePolicy(ctx, "00000000-0000-0000-0000-000000000001", f.serviceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign read = %v, want ErrNotFound", err)
	}
	if _, _, err := st.PutGatePolicy(ctx, "00000000-0000-0000-0000-000000000001", f.serviceID, nil, gateDoc(nil), gateActorToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign write = %v, want ErrNotFound", err)
	}
	if _, err := st.GetGatePolicy(ctx, f.projectID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed read = %v, want ErrNotFound", err)
	}
}

func TestGatePolicyRoundTripsCanonicallyAndNoOpsOnIdenticalRewrite(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)

	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	if rev != 1 {
		t.Fatalf("first revision = %d, want 1", rev)
	}
	p, err := st.GetGatePolicy(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window != gateWindow || p.BudgetConsumedPercent != 90 || p.MaxSealLagSeconds != 900 || p.UnknownBehavior != domain.GateUnknownWarn ||
		p.UpdatedBy != "token:ci" || p.Revision != 1 || !p.Live() {
		t.Errorf("stored policy = %+v", p)
	}
	doc := p.Document()
	if len(doc.Clauses) != len(domain.GateClausesV1) {
		t.Fatalf("round-trip lost clauses: %+v", doc.Clauses)
	}
	for i, c := range domain.GateClausesV1 {
		if doc.Clauses[i].Clause != c {
			t.Errorf("clause %d = %s, want the canonical order %s", i, doc.Clauses[i].Clause, c)
		}
	}
	audits := gateAuditCount(t, st, ctx)
	if audits != 1 {
		t.Fatalf("one write, %d audit rows", audits)
	}

	// Identical rewrite in a DIFFERENT clause order: same revision, changed == false, no audit.
	shuffled := gateDoc(nil)
	shuffled.Clauses[0], shuffled.Clauses[4] = shuffled.Clauses[4], shuffled.Clauses[0]
	rev2, changed, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(1), shuffled, gateActorToken)
	if err != nil || changed || rev2 != 1 {
		t.Fatalf("identical rewrite: rev=%d changed=%v err=%v; want 1,false,nil", rev2, changed, err)
	}
	if n := gateAuditCount(t, st, ctx); n != audits {
		t.Errorf("identical rewrite wrote an audit row (%d → %d)", audits, n)
	}
	// A real change bumps to 2 and audits.
	rev3, changed, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(1),
		gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock}), gateActorToken)
	if err != nil || !changed || rev3 != 2 {
		t.Fatalf("real change: rev=%d changed=%v err=%v; want 2,true,nil", rev3, changed, err)
	}
	if n := gateAuditCount(t, st, ctx); n != audits+1 {
		t.Errorf("real change audit rows %d, want %d", n, audits+1)
	}
	var target string
	if err := st.pool.QueryRow(ctx, `SELECT target FROM audit_logs WHERE action = 'gate.policy.write' ORDER BY created_at DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target, "before=") || !strings.Contains(target, "after=") || !strings.Contains(target, `"service_incident_open":"block"`) {
		t.Errorf("audit target lacks before/after: %s", target)
	}
}

// D13a: the CAS runs BEFORE the no-op comparison — an identical body with a stale
// expected_revision is a conflict, never a silent success; and a stale write changes NOTHING.
func TestGatePolicyCASBeforeNoOpAndStaleWriteChangesNothing(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))
	rev := gatePut(t, st, ctx, f, int64p(1), gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseTicketBurnFiring: domain.ClauseAssignIgnore}))
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}
	ovID, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, 2, "release 1.2", gateDBNow(t, st, ctx).Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetGatePolicy(ctx, f.projectID, f.serviceID)
	audits := gateAuditCount(t, st, ctx)

	// Identical to what is stored, but expected 1: conflict.
	same := before.Document()
	if _, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(1), same, gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("identical body + stale expected = %v, want ErrGateRevisionConflict", err)
	}
	// A different body with a stale expected: conflict, nothing changed.
	if _, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(1), gateDoc(nil), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("stale expected = %v, want ErrGateRevisionConflict", err)
	}
	// nil expected against a live policy: conflict.
	if _, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, nil, gateDoc(nil), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("nil expected over a live policy = %v, want ErrGateRevisionConflict", err)
	}
	after, err := st.GetGatePolicy(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.UpdatedAt != before.UpdatedAt || after.Clauses[domain.ClauseTicketBurnFiring] != domain.ClauseAssignIgnore {
		t.Errorf("a refused write changed the policy: %+v → %+v", before, after)
	}
	if n := gateAuditCount(t, st, ctx); n != audits {
		t.Errorf("a refused write audited (%d → %d)", audits, n)
	}
	ov, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, ovID)
	if err != nil || ov.Status != domain.GateOverrideActive {
		t.Errorf("a refused write touched the override: %+v %v", ov, err)
	}
}

// D13a's race: A reads N; the policy is deleted (N+1); B recreates (N+2); A's PUT with N and
// A's override with policy_revision N are both conflicts and B's state is untouched.
func TestGatePolicyDeleteRecreateRaceNeverReusesARevision(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	n := gatePut(t, st, ctx, f, nil, gateDoc(nil)) // A reads N = 1

	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, n, gateActorToken); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetGatePolicy(ctx, f.projectID, f.serviceID); !errors.Is(err, ErrGatePolicyNotConfigured) {
		t.Fatalf("after delete = %v, want ErrGatePolicyNotConfigured", err)
	}
	var tombRev int64
	var deleted *time.Time
	if err := st.pool.QueryRow(ctx, `SELECT revision, deleted_at FROM service_gate_policies WHERE service_id = $1`, f.serviceID).Scan(&tombRev, &deleted); err != nil {
		t.Fatalf("the row must survive as a tombstone: %v", err)
	}
	if tombRev != n+1 || deleted == nil {
		t.Fatalf("tombstone revision=%d deleted_at=%v, want %d and set", tombRev, deleted, n+1)
	}
	// A second delete: not configured; a delete with the wrong revision on a live row: conflict.
	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, n+1, gateActorToken); !errors.Is(err, ErrGatePolicyNotConfigured) {
		t.Errorf("delete of a tombstone = %v, want ErrGatePolicyNotConfigured", err)
	}
	// Recreate: expected nil matches the tombstone; a non-nil expected never does.
	if _, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(n+1), gateDoc(nil), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("non-nil expected against a tombstone = %v, want ErrGateRevisionConflict", err)
	}
	bDoc := gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseServiceIncidentOpen: domain.ClauseAssignBlock})
	recreated := gatePut(t, st, ctx, f, nil, bDoc)
	if recreated != n+2 {
		t.Fatalf("recreate revision = %d, want %d (a generation, never reused)", recreated, n+2)
	}
	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, n, gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("delete with a stale revision = %v, want ErrGateRevisionConflict", err)
	}

	// A's stale writes.
	if _, _, err := st.PutGatePolicy(ctx, f.projectID, f.serviceID, int64p(n), gateDoc(nil), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("A's PUT with N = %v, want ErrGateRevisionConflict", err)
	}
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, n, "stale", gateDBNow(t, st, ctx).Add(time.Hour), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("A's override with N = %v, want ErrGateRevisionConflict", err)
	}
	p, err := st.GetGatePolicy(ctx, f.projectID, f.serviceID)
	if err != nil || p.Revision != n+2 || p.Clauses[domain.ClauseServiceIncidentOpen] != domain.ClauseAssignBlock {
		t.Errorf("B's policy was touched: %+v %v", p, err)
	}
	if _, err := st.ActiveGateOverride(ctx, f.projectID, f.serviceID); !errors.Is(err, ErrGateOverrideNone) {
		t.Errorf("an override slipped in: %v", err)
	}
}

// ── Override lifecycle (D9, D13a; invariants 8, 9, 17) ───────────────────────────────────

func TestGateOverrideBoundsAndSlot(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)

	// Bounds, each naming its field — on the DATABASE clock.
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "x", now.Add(7*24*time.Hour+time.Second), gateActorToken); validationField(t, err) != "expires_at" {
		t.Errorf("7d+1s named %v", err)
	}
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "x", now.Add(-time.Second), gateActorToken); validationField(t, err) != "expires_at" {
		t.Errorf("past expiry named %v", err)
	}
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "", now.Add(time.Hour), gateActorToken); validationField(t, err) != "reason" {
		t.Errorf("empty reason named %v", err)
	}
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, strings.Repeat("ё", 501), now.Add(time.Hour), gateActorToken); validationField(t, err) != "reason" {
		t.Errorf("501-char reason named %v", err)
	}
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev+1, "x", now.Add(time.Hour), gateActorToken); !errors.Is(err, ErrGateRevisionConflict) {
		t.Errorf("wrong policy_revision = %v, want ErrGateRevisionConflict", err)
	}
	// Exactly 7 days is allowed (≤).
	id1, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, strings.Repeat("ё", 500), now.Add(7*24*time.Hour-time.Second), gateActorToken)
	if err != nil {
		t.Fatalf("7d − 1s: %v", err)
	}
	// One slot.
	if _, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "second", now.Add(time.Hour), gateActorToken); !errors.Is(err, ErrGateOverrideActive) {
		t.Errorf("second active = %v, want ErrGateOverrideActive", err)
	}
	active, err := st.ActiveGateOverride(ctx, f.projectID, f.serviceID)
	if err != nil || active.ID != id1 || active.Status != domain.GateOverrideActive || active.ActorLabel != "token:ci" || !active.ViaToken || active.ActorUserID != nil {
		t.Fatalf("active = %+v %v", active, err)
	}
	// Expiry releases the slot: the expired row is closed as `expired` by the next create,
	// attribution NULL, status `expired` both before and after that closure.
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_overrides SET expires_at = now() - interval '1 second' WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, id1); err != nil || got.Status != domain.GateOverrideExpired || got.RevokedAt != nil {
		t.Fatalf("expired-but-open row: %+v %v", got, err)
	}
	if _, err := st.ActiveGateOverride(ctx, f.projectID, f.serviceID); !errors.Is(err, ErrGateOverrideNone) {
		t.Errorf("active read over an expired row = %v, want ErrGateOverrideNone", err)
	}
	id2, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "after expiry", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
	got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, id1)
	if err != nil || got.Status != domain.GateOverrideExpired || got.RevokedAt == nil || got.RevokedReason != domain.GateRevokedExpired ||
		got.RevokedByLabel != nil || got.RevokedViaToken != nil || got.RevokedByUserID != nil {
		t.Errorf("closed expired row: %+v %v", got, err)
	}
	// History: newest first, both rows, each with its status.
	hist, err := st.ListGateOverrides(ctx, f.projectID, f.serviceID)
	if err != nil || len(hist) != 2 || hist[0].ID != id2 || hist[1].ID != id1 || hist[0].Status != domain.GateOverrideActive {
		t.Errorf("history = %+v %v", hist, err)
	}
	// Tenant: foreign service → ErrNotFound on every read.
	foreign := "00000000-0000-0000-0000-000000000001"
	if _, err := st.ActiveGateOverride(ctx, foreign, f.serviceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign active = %v", err)
	}
	if _, err := st.ListGateOverrides(ctx, foreign, f.serviceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign list = %v", err)
	}
	if _, err := st.GetGateOverride(ctx, foreign, f.serviceID, id2); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign by-id = %v", err)
	}
	if _, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed by-id = %v", err)
	}
}

func TestGateOverrideConcurrentCreatesExactlyOneWins(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	expires := gateDBNow(t, st, ctx).Add(time.Hour)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "race", expires, gateActorToken)
		}(i)
	}
	wg.Wait()
	wins, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrGateOverrideActive):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || refused != n-1 {
		t.Errorf("wins=%d refused=%d, want 1 and %d", wins, refused, n-1)
	}
	var open int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_overrides WHERE service_id = $1 AND revoked_at IS NULL`, f.serviceID).Scan(&open); err != nil || open != 1 {
		t.Errorf("open rows = %d %v, want 1", open, err)
	}
}

func TestGateOverrideRevokeIsByIdAndOnlyActive(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)
	a, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "A", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	// A expires; B is created; a UI still holding A revokes it → 409, B untouched.
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_overrides SET expires_at = now() - interval '1 second' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "B", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, a, gateActorToken); !errors.Is(err, ErrGateOverrideNotActive) {
		t.Errorf("revoke of the expired A = %v, want ErrGateOverrideNotActive", err)
	}
	if got, _ := st.GetGateOverride(ctx, f.projectID, f.serviceID, b); got.Status != domain.GateOverrideActive {
		t.Errorf("B was touched: %+v", got)
	}
	// Foreign and unknown ids: not found.
	if err := st.RevokeGateOverride(ctx, "00000000-0000-0000-0000-000000000001", f.serviceID, b, gateActorToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign revoke = %v", err)
	}
	if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, "00000000-0000-0000-0000-000000000009", gateActorToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown revoke = %v", err)
	}
	// A token revoker: the complete triple, typed-token contract.
	revoker := GateActor{ViaToken: true, Label: "token:release-bot"}
	if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, b, revoker); err != nil {
		t.Fatalf("revoke B: %v", err)
	}
	got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, b)
	if err != nil || got.Status != domain.GateOverrideRevoked || got.RevokedReason != domain.GateRevokedManual ||
		got.RevokedByLabel == nil || *got.RevokedByLabel != "token:release-bot" || got.RevokedViaToken == nil || !*got.RevokedViaToken || got.RevokedByUserID != nil {
		t.Errorf("revoked B: %+v %v", got, err)
	}
	// Revoking again is the same 409, never a silent success.
	if err := st.RevokeGateOverride(ctx, f.projectID, f.serviceID, b, revoker); !errors.Is(err, ErrGateOverrideNotActive) {
		t.Errorf("second revoke = %v, want ErrGateOverrideNotActive", err)
	}
	var audits int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('gate.override.create','gate.override.revoke')`).Scan(&audits); err != nil || audits != 3 {
		t.Errorf("override audit rows = %d %v, want 3 (two creates, one revoke)", audits, err)
	}
}

// A policy edit closes the active override as policy_changed; a delete as policy_deleted;
// both read as `inert` with attribution NULL (D9, D13a).
func TestGatePolicyEditAndDeleteRevokeTheActiveOverride(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil))
	now := gateDBNow(t, st, ctx)

	a, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev, "A", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatal(err)
	}
	rev2 := gatePut(t, st, ctx, f, int64p(rev), gateDoc(map[domain.GateClause]domain.ClauseAssignment{domain.ClauseTicketBurnFiring: domain.ClauseAssignBlock}))
	got, err := st.GetGateOverride(ctx, f.projectID, f.serviceID, a)
	if err != nil || got.Status != domain.GateOverrideInert || got.RevokedReason != domain.GateRevokedPolicyChanged || got.RevokedAt == nil || got.RevokedByLabel != nil {
		t.Errorf("after edit: %+v %v", got, err)
	}
	b, err := st.CreateGateOverride(ctx, f.projectID, f.serviceID, rev2, "B", now.Add(time.Hour), gateActorToken)
	if err != nil {
		t.Fatalf("B after the edit released the slot: %v", err)
	}
	if err := st.DeleteGatePolicy(ctx, f.projectID, f.serviceID, rev2, gateActorToken); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetGateOverride(ctx, f.projectID, f.serviceID, b)
	if err != nil || got.Status != domain.GateOverrideInert || got.RevokedReason != domain.GateRevokedPolicyDeleted || got.RevokedByLabel != nil {
		t.Errorf("after delete: %+v %v", got, err)
	}
	if _, err := st.ActiveGateOverride(ctx, f.projectID, f.serviceID); !errors.Is(err, ErrGateOverrideNone) {
		t.Errorf("active after delete = %v", err)
	}
}
