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
	res, err := st.ApplyFileManagedBundle(ctx, "platform", dp, "acme-payments.yaml", grace, 0, true)
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

	// --- No-op: same bundle, same path → NO DB write at all, generation does NOT advance
	// (§7/§17: periodic resync of an unchanged bundle must not amplify writes). ---
	res = applyBundle(t, st, ctx, bundle, 0)
	if res.Changed || !res.NoChange || res.Generation != 1 || res.Counts[fileprovider.ActionNoop] != 2 {
		t.Fatalf("noop result = %+v (want NoChange, generation unchanged at 1)", res)
	}
	_, rev2, _, updated2, _ := monRow(t, st, ctx, "api")
	if rev2 != apiRev || !updated2.Equal(apiUpdated) {
		t.Fatalf("no-op must not touch the monitor: rev %d→%d updated %v→%v", apiRev, rev2, apiUpdated, updated2)
	}
	// The bundle row's generation must also be unchanged by the no-op.
	var genAfterNoop int64
	_ = st.pool.QueryRow(ctx, `SELECT generation FROM file_provider_bundles WHERE provider_id='platform'`).Scan(&genAfterNoop)
	if genAfterNoop != 1 {
		t.Fatalf("no-op must not advance the bundle generation, got %d", genAfterNoop)
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
	if _, terr := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, "format: 1\norganization: ghost\nproject: nope\nmonitors: {}\n"), "x.yaml", 0, 0, true); terr == nil {
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
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, twoMon("payments")), "p.yaml", 0, 2, true); err != nil {
		t.Fatalf("payments within quota should apply: %v", err)
	}
	// billing: 1 monitor would make 3 > cap 2 → rejected whole, LKG (no monitors created).
	oneMon := "format: 1\norganization: acme\nproject: billing\nmonitors:\n  a: {name: A, type: http, target: https://a}\n"
	_, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, oneMon), "b.yaml", 0, 2, true)
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
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, b), "p.yaml", 0, 0, true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='file_provider.apply'`, org.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 file_provider.apply audit row, got %d", n)
	}
	// A no-op re-apply writes NO new audit row (nothing changed).
	_, _ = st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, b), "p.yaml", 0, 0, true)
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

// TestApplyRestoreNoConfigChangeNoRevisionBump covers §10 / P1#4: a UID that reappears WITHIN
// grace while still enabled and with unchanged config must only clear the orphan mark — it must
// NOT bump execution_revision, reset the scheduler watermark, notify, or audit.
func TestApplyRestoreNoConfigChangeNoRevisionBump(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	one := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n"
	empty := "format: 1\norganization: acme\nproject: payments\nmonitors: {}\n"

	applyBundle(t, st, ctx, one, 0)
	_, rev0, en0, _, _ := monRow(t, st, ctx, "a")
	if !en0 {
		t.Fatal("A must start enabled")
	}
	// Mark absence with a LARGE grace so A is orphaned but NOT disabled (still enabled).
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, empty), "p.yaml", time.Hour, 0, true); err != nil {
		t.Fatalf("orphan-mark apply: %v", err)
	}
	if en, orph := uidState(t, st, ctx, "a"); !en || !orph {
		t.Fatalf("A should be orphaned-but-enabled within grace (en=%v orph=%v)", en, orph)
	}
	auditBefore := auditCount(t, st, ctx)
	// Restore with UNCHANGED config while still enabled → pure un-orphan.
	res, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, one), "p.yaml", 0, 0, true)
	if err != nil {
		t.Fatalf("restore apply: %v", err)
	}
	if res.Changed {
		t.Fatalf("un-orphan with no config change must NOT report an execution change: %+v", res)
	}
	_, rev1, en1, _, _ := monRow(t, st, ctx, "a")
	if rev1 != rev0 {
		t.Fatalf("un-orphan must NOT bump execution_revision: %d → %d", rev0, rev1)
	}
	if !en1 {
		t.Fatal("A must stay enabled after un-orphan")
	}
	if _, orph := uidState(t, st, ctx, "a"); orph {
		t.Fatal("un-orphan must clear orphaned_at")
	}
	// The un-orphan is an ownership-state transition → audited (D-0148) even though it does not
	// bump execution_revision.
	if auditCount(t, st, ctx) != auditBefore+1 {
		t.Fatalf("un-orphan (ownership transition) must be audited: %d → %d", auditBefore, auditCount(t, st, ctx))
	}
}

// TestApplyRestoreEnabledSemantics covers P1#1 (§10): the restore write decision compares the
// monitor's ACTUAL enabled state with the DESIRED one, not "actual != enabled".
//   - a declarative `enabled: false` that reappears unchanged → pure un-orphan, no revision bump;
//   - a desired `enabled: true` monitor that was grace-disabled → re-enable, with a revision bump.
func TestApplyRestoreEnabledSemantics(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	empty := "format: 1\norganization: acme\nproject: payments\nmonitors: {}\n"

	// --- Case A: declarative enabled:false, reappears unchanged → no revision bump ---
	offBundle := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s, enabled: false}\n"
	applyBundle(t, st, ctx, offBundle, 0)
	_, revA0, enA0, _, _ := monRow(t, st, ctx, "a")
	if enA0 {
		t.Fatal("A must start disabled (declarative enabled:false)")
	}
	// Orphan-mark with a large grace so it stays as-is (disabled), then restore unchanged.
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, empty), "p.yaml", time.Hour, 0, true); err != nil {
		t.Fatalf("orphan-mark A: %v", err)
	}
	res := applyBundle(t, st, ctx, offBundle, 0)
	if res.Changed {
		t.Fatalf("declarative enabled:false un-orphan must not be an execution change: %+v", res)
	}
	_, revA1, enA1, _, _ := monRow(t, st, ctx, "a")
	if revA1 != revA0 {
		t.Fatalf("declarative enabled:false restore must NOT bump revision: %d → %d", revA0, revA1)
	}
	if enA1 {
		t.Fatal("A must stay disabled (desired enabled:false)")
	}

	// --- Case B: desired enabled:true, grace-disabled → re-enable + bump ---
	onBundle := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  b: {name: B, type: http, target: https://b, interval: 30s, timeout: 5s}\n"
	applyBundle(t, st, ctx, onBundle, 0)
	_, revB0, enB0, _, _ := monRow(t, st, ctx, "b")
	if !enB0 {
		t.Fatal("B must start enabled")
	}
	// Two trusted empty applies with grace 0: first marks orphaned, second grace-disables B.
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, empty), "p.yaml", 0, 0, true); err != nil {
		t.Fatalf("orphan-mark B: %v", err)
	}
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, empty), "p.yaml", 0, 0, true); err != nil {
		t.Fatalf("grace-disable B: %v", err)
	}
	if en, _ := uidState(t, st, ctx, "b"); en {
		t.Fatal("B must be grace-disabled before the restore")
	}
	res = applyBundle(t, st, ctx, onBundle, 0)
	if !res.Changed {
		t.Fatalf("re-enabling a grace-disabled monitor must be an execution change: %+v", res)
	}
	_, revB1, enB1, _, _ := monRow(t, st, ctx, "b")
	if !enB1 {
		t.Fatal("B must be re-enabled by the restore")
	}
	if revB1 <= revB0 {
		t.Fatalf("re-enable restore must bump execution_revision: %d → %d", revB0, revB1)
	}
}

// TestApplyDependencyChangeAudited covers §9 / P1#7: a dependency-only mutation changes the
// declarative graph and MUST be audited (even though it does not bump execution_revision).
func TestApplyDependencyChangeAudited(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	withDep := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s, depends_on: [b]}\n" +
		"  b: {name: B, type: http, target: https://b, interval: 30s, timeout: 5s}\n"
	noDep := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n" +
		"  b: {name: B, type: http, target: https://b, interval: 30s, timeout: 5s}\n"

	applyBundle(t, st, ctx, withDep, 0)
	_, revBefore, _, _, _ := monRow(t, st, ctx, "a")
	auditBefore := auditCount(t, st, ctx)

	res := applyBundle(t, st, ctx, noDep, 0)
	if res.Counts[fileprovider.ActionDependencyUpdate] != 1 {
		t.Fatalf("expected a dependency-only change, got %+v", res)
	}
	_, revAfter, _, _, _ := monRow(t, st, ctx, "a")
	if revAfter != revBefore {
		t.Fatalf("dependency-only change must NOT bump execution_revision: %d → %d", revBefore, revAfter)
	}
	if auditCount(t, st, ctx) != auditBefore+1 {
		t.Fatal("a dependency-only change must be audited")
	}
}

// TestRecordBundleAttemptByPath covers P1#5 (§9.1/§13): an invalid file at a PREVIOUSLY KNOWN
// path marks that bundle row rejected (without a generation bump); an unknown path is a no-op.
func TestRecordBundleAttemptByPath(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	one := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n"
	applyBundle(t, st, ctx, one, 0) // bundle row source_path = "acme-payments.yaml", applied, gen 1

	if err := recordBundleAttemptByPath(ctx, st.pool, "platform", "acme-payments.yaml", "rejected", "invalid yaml"); err != nil {
		t.Fatalf("by-path attempt: %v", err)
	}
	s, le, _, gen, _ := bundleRow(t, st, ctx, "payments")
	if s != "rejected" || le != "invalid yaml" {
		t.Fatalf("known-path invalid must mark the bundle rejected, got status=%q err=%q", s, le)
	}
	if gen != 1 {
		t.Fatalf("a by-path rejection must NOT bump the generation, got %d", gen)
	}
	// Unknown path → no row created, no error.
	before := auditCount(t, st, ctx)
	if err := recordBundleAttemptByPath(ctx, st.pool, "platform", "never-seen.yaml", "rejected", "boom"); err != nil {
		t.Fatalf("unknown-path attempt must be a no-op: %v", err)
	}
	var rows int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM file_provider_bundles WHERE provider_id='platform'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("unknown path must not create a bundle row, got %d rows", rows)
	}
	_ = before
}

// bundleRow reads a provider's single bundle diagnostics row for a project slug.
func bundleRow(t *testing.T, st *Store, ctx context.Context, projSlug string) (status, lastErr, path string, gen int64, exists bool) {
	t.Helper()
	err := st.pool.QueryRow(ctx,
		`SELECT b.status, b.last_error, b.source_path, b.generation
		   FROM file_provider_bundles b JOIN projects p ON p.id=b.project_id
		  WHERE b.provider_id='platform' AND p.slug=$1`, projSlug).Scan(&status, &lastErr, &path, &gen)
	if noRows(err) {
		return "", "", "", 0, false
	}
	if err != nil {
		t.Fatalf("read bundle row %q: %v", projSlug, err)
	}
	return status, lastErr, path, gen, true
}

// TestApplyBundleDiagnosticsLifecycle covers P1#2 (§15): the bundle diagnostics row lives on an
// axis independent of monitor state — a stale rejected status is cleared by a clean no-op
// (without a generation bump), the first (even empty) bundle is recorded with generation 1, and
// a rename of an empty/all-orphaned bundle still refreshes source_path.
func TestApplyBundleDiagnosticsLifecycle(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	_, _ = st.CreateProject(ctx, org.ID, "payments", "Payments")
	_, _ = st.CreateProject(ctx, org.ID, "billing", "Billing")
	one := "format: 1\norganization: acme\nproject: payments\nmonitors:\n  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n"

	// --- Stale rejected status cleared by a clean NO-OP, no generation bump ---
	applyBundle(t, st, ctx, one, 0) // create → applied, gen 1
	if err := st.RecordBundleAttempt(ctx, "platform", "acme", "payments", "acme-payments.yaml", "rejected", "boom"); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if s, le, _, _, _ := bundleRow(t, st, ctx, "payments"); s != "rejected" || le != "boom" {
		t.Fatalf("precondition: expected a rejected row, got status=%q err=%q", s, le)
	}
	res := applyBundle(t, st, ctx, one, 0) // clean no-op
	if !res.NoChange {
		t.Fatalf("re-applying the same bundle must be a no-op: %+v", res)
	}
	s, le, _, gen, _ := bundleRow(t, st, ctx, "payments")
	if s != "applied" || le != "" {
		t.Fatalf("a clean no-op must clear the stale rejected status, got status=%q err=%q", s, le)
	}
	if gen != 1 {
		t.Fatalf("clearing a stale status must NOT bump the generation, got %d", gen)
	}

	// --- First EMPTY bundle recorded with generation 1 ---
	emptyBilling := "format: 1\norganization: acme\nproject: billing\nmonitors: {}\n"
	res = applyBundle2(t, st, ctx, emptyBilling, "billing.yaml", 0)
	if !res.NoChange {
		t.Fatalf("an empty bundle applies as a no-op: %+v", res)
	}
	if s, _, path, g, ok := bundleRow(t, st, ctx, "billing"); !ok || s != "applied" || g != 1 || path != "billing.yaml" {
		t.Fatalf("first empty bundle must be recorded applied@gen1 with its path, got ok=%v status=%q gen=%d path=%q", ok, s, g, path)
	}

	// --- Rename of an (empty) bundle refreshes source_path without a generation bump ---
	res = applyBundle2(t, st, ctx, emptyBilling, "billing-renamed.yaml", 0)
	if _, _, path, g, _ := bundleRow(t, st, ctx, "billing"); path != "billing-renamed.yaml" || g != 1 {
		t.Fatalf("rename of an empty bundle must refresh path without bumping generation, got path=%q gen=%d", path, g)
	}
	_ = res
}

// applyBundle2 is applyBundle with an explicit source path (for rename/diagnostics tests).
func applyBundle2(t *testing.T, st *Store, ctx context.Context, y, sourcePath string, grace time.Duration) ApplyResult {
	t.Helper()
	res, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, y), sourcePath, grace, 0, true)
	if err != nil {
		t.Fatalf("apply %q: %v", sourcePath, err)
	}
	return res
}

// auditCount returns the number of file_provider.apply audit rows.
func auditCount(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='file_provider.apply'`).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// uidState reads a managed monitor's enabled flag and whether it is orphaned (orphaned_at set).
func uidState(t *testing.T, st *Store, ctx context.Context, uid string) (enabled, orphaned bool) {
	t.Helper()
	var orphAt *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT m.enabled, mm.orphaned_at FROM managed_monitors mm JOIN monitors m ON m.id=mm.monitor_id
		  WHERE mm.source_uid=$1`, uid).Scan(&enabled, &orphAt); err != nil {
		t.Fatalf("read uid %q state: %v", uid, err)
	}
	return enabled, orphAt != nil
}

// TestApplyBundleAbsenceGuard is the P0 orphan-safety regression (spec §9.1): when the scan is
// ambiguous (allowAbsence=false), a UID that is absent from a bundle for a PRESENT project must
// NOT be orphaned or grace-disabled — its last-known-good is kept. Only a trusted scan
// (allowAbsence=true) may orphan it. This is the "UID inside a present project" case the review
// flagged, distinct from a whole-project disappearance.
func TestApplyBundleAbsenceGuard(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if _, err := st.CreateProject(ctx, org.ID, "payments", "Payments"); err != nil {
		t.Fatalf("project: %v", err)
	}
	both := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n" +
		"  b: {name: B, type: http, target: https://b, interval: 30s, timeout: 5s}\n"
	onlyA := "format: 1\norganization: acme\nproject: payments\nmonitors:\n" +
		"  a: {name: A, type: http, target: https://a, interval: 30s, timeout: 5s}\n"

	// Seed A+B with a TRUSTED apply.
	applyBundle(t, st, ctx, both, 0)
	if en, orph := uidState(t, st, ctx, "b"); !en || orph {
		t.Fatalf("post-seed B should be enabled and not orphaned (en=%v orph=%v)", en, orph)
	}

	// Ambiguous scan: apply {A} with allowAbsence=FALSE, grace=0. B is absent from a PRESENT
	// project, but the guard must keep it enabled AND unmarked (no orphaned_at).
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, onlyA), "p.yaml", 0, 0, false); err != nil {
		t.Fatalf("apply {A} allowAbsence=false: %v", err)
	}
	if en, orph := uidState(t, st, ctx, "b"); !en || orph {
		t.Fatalf("allowAbsence=false must NOT orphan/disable a present-project UID (en=%v orph=%v)", en, orph)
	}
	// A second ambiguous scan must still not disable B (grace-disable also suppressed).
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, onlyA), "p.yaml", 0, 0, false); err != nil {
		t.Fatalf("second apply {A} allowAbsence=false: %v", err)
	}
	if en, orph := uidState(t, st, ctx, "b"); !en || orph {
		t.Fatalf("repeated allowAbsence=false must keep B live (en=%v orph=%v)", en, orph)
	}

	// A TRUSTED scan marks the first absence (orphaned_at set) but does NOT disable in the same
	// pass — the mark and the grace-disable are separate reconciles by design (§10).
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, onlyA), "p.yaml", 0, 0, true); err != nil {
		t.Fatalf("first trusted apply {A}: %v", err)
	}
	if en, orph := uidState(t, st, ctx, "b"); !en || !orph {
		t.Fatalf("first trusted scan must orphan B but keep it enabled (en=%v orph=%v)", en, orph)
	}
	// The NEXT trusted scan (grace=0, B already orphaned at tx start) disables B — proving the
	// guard, not a missing plan entry, was protecting it earlier.
	if _, err := st.ApplyFileManagedBundle(ctx, "platform", mustDecodeStore(t, onlyA), "p.yaml", 0, 0, true); err != nil {
		t.Fatalf("second trusted apply {A}: %v", err)
	}
	if en, orph := uidState(t, st, ctx, "b"); en || !orph {
		t.Fatalf("second trusted scan (grace=0) must disable orphaned B (en=%v orph=%v)", en, orph)
	}
}
