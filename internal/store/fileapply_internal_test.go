package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// applyBundle decodes an instance-scoped bundle and applies it for provider "platform".
func applyBundle(t *testing.T, st *Store, ctx context.Context, y string, grace time.Duration) ApplyResult {
	t.Helper()
	dp, err := fileprovider.Decode([]byte(y), config.ProviderScopeConfig{Type: config.ProviderScopeInstance})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, err := st.ApplyFileManagedBundle(ctx, "platform", dp, "acme-payments.yaml", grace, 0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

func monRow(t *testing.T, st *Store, ctx context.Context, uid string) (id string, rev int64, enabled bool, updatedAt time.Time, pushHash *string) {
	t.Helper()
	err := st.pool.QueryRow(ctx,
		`SELECT m.id::text, m.execution_revision, m.enabled, m.updated_at, m.push_token_hash
		   FROM managed_monitors mm JOIN monitors m ON m.id = mm.monitor_id
		  WHERE mm.source_uid = $1`, uid).Scan(&id, &rev, &enabled, &updatedAt, &pushHash)
	if err != nil {
		t.Fatalf("read monitor %q: %v", uid, err)
	}
	return
}

// TestApplyFileManagedBundle covers the atomic reconcile lifecycle (spec §18.2): create,
// canonical no-op (no revision bump / no updated_at change, generation advances), semantic
// update (revision bump), dependency-only update (no bump), orphan → grace-disable (no
// delete), and restore (same DB id + push token, re-enabled), plus tenant-not-found.
func TestApplyFileManagedBundle(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if _, err := st.CreateProject(ctx, org.ID, "payments", "Payments"); err != nil {
		t.Fatalf("project: %v", err)
	}

	bundle := `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    interval: 30s
    timeout: 5s
    depends_on: [cron]
  cron:
    name: Nightly job
    type: push
    interval: 3600s
`

	// --- Create ---
	res := applyBundle(t, st, ctx, bundle, 0)
	if !res.Changed || res.Generation != 1 || res.Counts[fileprovider.ActionCreate] != 2 {
		t.Fatalf("create result = %+v", res)
	}
	apiID, apiRev, apiEnabled, apiUpdated, _ := monRow(t, st, ctx, "api")
	cronID, _, _, _, cronPush := monRow(t, st, ctx, "cron")
	if !apiEnabled || cronPush == nil {
		t.Fatalf("post-create: apiEnabled=%v cronPush=%v (push token must be server-generated)", apiEnabled, cronPush)
	}
	cronToken := *cronPush
	// Dependency edge api→cron exists.
	var deps int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM monitor_dependencies WHERE monitor_id=$1 AND depends_on_id=$2`, apiID, cronID).Scan(&deps)
	if deps != 1 {
		t.Fatalf("dependency edge missing: %d", deps)
	}

	// --- No-op: same bundle → no revision bump, no updated_at change, generation advances ---
	res = applyBundle(t, st, ctx, bundle, 0)
	if res.Changed || res.Generation != 2 || res.Counts[fileprovider.ActionNoop] != 2 {
		t.Fatalf("noop result = %+v", res)
	}
	_, rev2, _, updated2, _ := monRow(t, st, ctx, "api")
	if rev2 != apiRev || !updated2.Equal(apiUpdated) {
		t.Fatalf("no-op must not touch the monitor: rev %d→%d updated %v→%v", apiRev, rev2, apiUpdated, updated2)
	}

	// --- Semantic update: change interval → revision bump ---
	updated := `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    interval: 60s
    timeout: 5s
    depends_on: [cron]
  cron:
    name: Nightly job
    type: push
    interval: 3600s
`
	res = applyBundle(t, st, ctx, updated, 0)
	if !res.Changed || res.Counts[fileprovider.ActionUpdate] != 1 || res.Counts[fileprovider.ActionNoop] != 1 {
		t.Fatalf("update result = %+v", res)
	}
	_, rev3, _, _, _ := monRow(t, st, ctx, "api")
	if rev3 <= apiRev {
		t.Fatalf("semantic update must bump execution_revision: %d → %d", apiRev, rev3)
	}

	// --- Dependency-only update: drop api→cron → NO revision bump ---
	nodep := `
format: 1
organization: acme
project: payments
monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    interval: 60s
    timeout: 5s
  cron:
    name: Nightly job
    type: push
    interval: 3600s
`
	res = applyBundle(t, st, ctx, nodep, 0)
	if res.Counts[fileprovider.ActionDependencyUpdate] != 1 {
		t.Fatalf("dependency-only result = %+v", res)
	}
	_, rev4, _, _, _ := monRow(t, st, ctx, "api")
	if rev4 != rev3 {
		t.Fatalf("dependency-only change must NOT bump revision: %d → %d", rev3, rev4)
	}
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM monitor_dependencies WHERE monitor_id=$1`, apiID).Scan(&deps)
	if deps != 0 {
		t.Fatalf("dependency edge should be removed, got %d", deps)
	}

	// --- Orphan (first absence marks) then grace(0) disable on the next scan ---
	empty := "format: 1\norganization: acme\nproject: payments\nmonitors: {}\n"
	res = applyBundle(t, st, ctx, empty, 0)
	if res.Counts[fileprovider.ActionOrphan] != 2 {
		t.Fatalf("first absence result = %+v", res)
	}
	if _, _, en, _, _ := monRow(t, st, ctx, "api"); !en {
		t.Fatal("first absence must NOT disable yet (grace)")
	}
	res = applyBundle(t, st, ctx, empty, 0)
	if !res.Changed {
		t.Fatalf("grace-elapsed absence should disable (Changed) = %+v", res)
	}
	if _, _, en, _, _ := monRow(t, st, ctx, "api"); en {
		t.Fatal("grace-elapsed absence must disable the monitor")
	}
	// Monitor rows still exist (no hard delete).
	var live int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE id = ANY($1)`, []string{apiID, cronID}).Scan(&live)
	if live != 2 {
		t.Fatalf("orphan must never hard-delete: %d monitors remain", live)
	}

	// --- Restore: same UID reappears → same DB id + same push token, re-enabled ---
	res = applyBundle(t, st, ctx, bundle, 0)
	if res.Counts[fileprovider.ActionRestore] != 2 {
		t.Fatalf("restore result = %+v", res)
	}
	rApiID, _, rEn, _, _ := monRow(t, st, ctx, "api")
	rCronID, _, _, _, rCronPush := monRow(t, st, ctx, "cron")
	if rApiID != apiID || rCronID != cronID {
		t.Fatalf("restore must reuse DB ids: api %s→%s cron %s→%s", apiID, rApiID, cronID, rCronID)
	}
	if !rEn {
		t.Fatal("restore must re-enable")
	}
	if rCronPush == nil || *rCronPush != cronToken {
		t.Fatalf("restore must preserve the push token")
	}

	// --- Tenant not found: no mutation, typed error ---
	if _, terr := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, "format: 1\norganization: ghost\nproject: nope\nmonitors: {}\n"), "x.yaml", 0, 0); terr == nil {
		t.Fatal("missing tenant must reject")
	}
}

func mustDecodeStore(t *testing.T, y string) *fileprovider.DesiredProject {
	t.Helper()
	dp, err := fileprovider.Decode([]byte(y), config.ProviderScopeConfig{Type: config.ProviderScopeInstance})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return dp
}

// TestApplyBundleMaxManagedMonitors covers the max_managed_monitors quota (spec §4/§17): a
// bundle whose creates would push the provider past its cap is rejected whole (LKG kept), and
// the quota is counted under the provider-wide serialization lock.
func TestApplyBundleMaxManagedMonitors(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	_, _ = st.CreateProject(ctx, org.ID, "billing", "Billing")

	twoMon := func(project string) string {
		return "format: 1\norganization: acme\nproject: " + project + "\nmonitors:\n" +
			"  a: {name: A, type: http, target: https://a}\n  b: {name: B, type: http, target: https://b}\n"
	}
	// payments: 2 monitors, cap 2 → fits.
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, twoMon("payments")), "p.yaml", 0, 2); err != nil {
		t.Fatalf("payments within quota should apply: %v", err)
	}
	// billing: 1 monitor would make 3 > cap 2 → rejected whole, LKG (no monitors created).
	oneMon := "format: 1\norganization: acme\nproject: billing\nmonitors:\n  a: {name: A, type: http, target: https://a}\n"
	_, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, oneMon), "b.yaml", 0, 2)
	var be *fileprovider.BundleError
	if err == nil || !errors.As(err, &be) || be.Reason != fileprovider.ReasonQuotaExceeded {
		t.Fatalf("over-quota bundle must reject with max_managed_monitors, got %v", err)
	}
	var billing int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM managed_monitors mm JOIN projects p ON p.id=mm.project_id WHERE p.slug='billing'`).Scan(&billing)
	if billing != 0 {
		t.Fatalf("rejected over-quota bundle must create nothing, got %d", billing)
	}
}

// TestApplyWritesAudit covers spec §9 step 10: a changed apply appends a tenant audit record
// in the SAME transaction as the monitor writes.
func TestApplyWritesAudit(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	b := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  a: {name: A, type: http, target: https://a}\n"
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, b), "p.yaml", 0, 0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='file_provider.apply'`, org.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 file_provider.apply audit row, got %d", n)
	}
	// A no-op re-apply writes NO new audit row (nothing changed).
	_, _ = st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, b), "p.yaml", 0, 0)
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='file_provider.apply'`, org.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("no-op apply must not audit: got %d rows", n)
	}
}

// TestRecordBundleAttempt covers spec §15: a rejected/errored attempt is persisted on the
// bundle's diagnostics row (status/last_error/attempted_at) without advancing generation; an
// unresolvable tenant is a no-op.
func TestRecordBundleAttempt(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")

	if err := st.RecordBundleAttempt(ctx, "platform", "acme", "payments", "p.yaml", "rejected", "max_managed_monitors"); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	diags, err := st.FileProviderDiagnostics(ctx, "")
	if err != nil || len(diags) != 1 {
		t.Fatalf("diagnostics = %d err=%v, want 1", len(diags), err)
	}
	d := diags[0]
	if d.Status != "rejected" || d.LastError != "max_managed_monitors" || d.Generation != 0 {
		t.Fatalf("persisted attempt = %+v", d)
	}
	// Unresolvable tenant → no-op, no error, no row.
	if err := st.RecordBundleAttempt(ctx, "platform", "ghost", "nope", "x.yaml", "error", "boom"); err != nil {
		t.Fatalf("unresolvable tenant must be a no-op: %v", err)
	}
	if diags, _ := st.FileProviderDiagnostics(ctx, ""); len(diags) != 1 {
		t.Fatalf("unresolvable tenant must not create a row, got %d", len(diags))
	}
}
