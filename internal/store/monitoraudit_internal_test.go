package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-026 §10 (D-0233): a monitor write says who wrote it. Same fixture and the same reading as the
// incident tests, over the `monitor.%` actions.

func monitorAuditRows(t *testing.T, st *Store, ctx context.Context, orgID string) []struct{ Action, Target string } {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT action, target FROM audit_logs WHERE org_id = $1 AND action LIKE 'monitor.%' ORDER BY created_at`, orgID)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []struct{ Action, Target string }
	for rows.Next() {
		var r struct{ Action, Target string }
		if err := rows.Scan(&r.Action, &r.Target); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// tokenActor is the principal shape that needs no users row: a Cerbix API token.
var tokenActor = AuditActor{ViaToken: true, Label: "token:ci-deployer"}

func auditedHTTPMonitor(projID string) domain.Monitor {
	return domain.Monitor{
		ProjectID: projID, Name: "audited-http", Type: domain.MonitorHTTP, Target: "https://example.com/health",
		IntervalSeconds: 60, TimeoutSeconds: 5, Region: "core", Enabled: true,
		Config: map[string]string{"password": "not-for-the-trail"},
	}
}

// Invariant 17 (one row per door, in the transaction) and D13's shapes, for the three doors in order.
func TestAMonitorWriteLeavesOneAuditRowInItsTransaction(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)

	created, err := st.CreateMonitorByPrincipal(ctx, auditedHTTPMonitor(projID), tokenActor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rows := monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 1 || rows[0].Action != string(MonitorAuditCreate) {
		t.Fatalf("after create: %+v, want exactly one monitor.create", rows)
	}
	if want := MonitorCreateTarget(tokenActor, created); rows[0].Target != want {
		t.Fatalf("create target = %q, want %q", rows[0].Target, want)
	}
	for _, part := range []string{"monitor " + created.ID, created.Slug, "http", "region=core", "actor=token:ci-deployer"} {
		if !strings.Contains(rows[0].Target, part) {
			t.Fatalf("create target %q lacks %q", rows[0].Target, part)
		}
	}

	// Pause it: the update names the flip, taken from the row it locked, not from the caller's claim.
	paused := created
	paused.Enabled = false
	if _, err := st.UpdateMonitorByPrincipal(ctx, paused, tokenActor); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows = monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 2 || rows[1].Action != string(MonitorAuditUpdate) {
		t.Fatalf("after update: %+v, want a second row, monitor.update", rows)
	}
	if !strings.Contains(rows[1].Target, "enabled true→false") {
		t.Fatalf("update target %q does not name the pause", rows[1].Target)
	}
	// An update that does not touch `enabled` names no flip.
	renamed := paused
	renamed.Name = "audited-http-renamed"
	if _, err := st.UpdateMonitorByPrincipal(ctx, renamed, tokenActor); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	rows = monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 3 || strings.Contains(rows[2].Target, "enabled") {
		t.Fatalf("after a rename: %+v, want a third row without an enabled clause", rows)
	}

	if err := st.DeleteMonitorByPrincipal(ctx, renamed.ID, tokenActor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows = monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 4 || rows[3].Action != string(MonitorAuditDelete) || !strings.HasSuffix(strings.SplitN(rows[3].Target, " · actor=", 2)[0], "· deleted") {
		t.Fatalf("after delete: %+v, want monitor.delete ending in '· deleted'", rows)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM monitors WHERE id = $1`, created.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the monitor survived its audited delete")
	}
}

// D13 / invariant 20: the target names the document and never its contents — and for a canary with an
// acknowledged `cleanup.kind: none`, it carries FR-029 invariant 13's clause.
func TestAMonitorAuditTargetNamesTheDocumentAndNotItsContents(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)

	// A credentialed type: the inline password must not reach the trail.
	if _, err := st.CreateMonitorByPrincipal(ctx, auditedHTTPMonitor(projID), tokenActor); err != nil {
		t.Fatalf("create http: %v", err)
	}
	// A canary whose document acknowledges no cleanup, with distinctive values in every position a
	// leak could come from.
	canary := domain.Monitor{
		ProjectID: projID, Name: "audited-canary", Type: domain.MonitorAsyncCanary,
		IntervalSeconds: 300, TimeoutSeconds: 300, Region: "core", Enabled: true,
		Config: map[string]string{domain.CanaryWorkflowKey: `{"kind":"async_transaction_v1",` +
			`"submit":{"kind":"http_json","method":"POST","url":"https://leak-url.example/upload","body":{"tenant":"leak-body"}},` +
			`"cleanup":{"kind":"none","acknowledged":true}}`},
	}
	createdCanary, err := st.CreateMonitorByPrincipal(ctx, canary, tokenActor)
	if err != nil {
		t.Fatalf("create canary: %v", err)
	}
	rows := monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want two creates", rows)
	}
	for _, r := range rows {
		for _, leaked := range []string{"not-for-the-trail", "leak-url", "leak-body", "upload", "tenant"} {
			if strings.Contains(r.Target, leaked) {
				t.Fatalf("target %q carries document content %q", r.Target, leaked)
			}
		}
	}
	if !strings.Contains(rows[0].Target, "cleanup=") {
		// the http monitor carries no clause
	} else {
		t.Fatalf("an http monitor's target carries a canary clause: %q", rows[0].Target)
	}
	if !strings.Contains(rows[1].Target, "cleanup=none acknowledged") {
		t.Fatalf("the acknowledged cleanup is not in the trail: %q", rows[1].Target)
	}
	// The clause follows the DOCUMENT: an update that keeps it keeps the clause.
	if _, err := st.UpdateMonitorByPrincipal(ctx, createdCanary, tokenActor); err != nil {
		t.Fatalf("update canary: %v", err)
	}
	rows = monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 3 || !strings.Contains(rows[2].Target, "cleanup=none acknowledged") {
		t.Fatalf("the update lost the clause: %+v", rows)
	}
}

// Invariant 18: a zero-valued actor, or a bare label, is refused before any statement — no monitor,
// no anonymous row.
func TestAZeroValuedActorRefusesAMonitorWriteBeforeAnyStatement(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	for _, actor := range []AuditActor{{}, {Label: "somebody"}} {
		if _, err := st.CreateMonitorByPrincipal(ctx, auditedHTTPMonitor(projID), actor); err == nil {
			t.Fatalf("actor %+v was accepted", actor)
		}
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM monitors WHERE project_id = $1`, projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the monitor was written anyway: %d rows", n)
	}
	if rows := monitorAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("an anonymous audit row was written: %+v", rows)
	}
	// The same doors refuse the actor for update and delete, on a monitor that exists.
	seeded, err := st.CreateMonitor(ctx, auditedHTTPMonitor(projID)) // the fixture hook, no principal
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateMonitorByPrincipal(ctx, seeded, AuditActor{}); err == nil {
		t.Fatal("update accepted a zero-valued actor")
	}
	if err := st.DeleteMonitorByPrincipal(ctx, seeded.ID, AuditActor{}); err == nil {
		t.Fatal("delete accepted a zero-valued actor")
	}
	if rows := monitorAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("a refused write left a row: %+v", rows)
	}
}

// Invariant 19 / D1: the file provider is a machine writer. Its apply creates, updates and removes
// monitors through its own writer and leaves NO `monitor.%` row — the bundle is the record.
func TestTheFileProviderWritesNoMonitorAuditRow(t *testing.T) {
	st, ctx := outboxTestStore(t)
	// The bundle names an existing organization and project; the provider creates neither.
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(ctx, org.ID, "payments", "Payments"); err != nil {
		t.Fatal(err)
	}
	applyBundle(t, st, ctx, `
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
`, time.Hour)
	// A semantic update and then a removal, through the same machine path.
	applyBundle(t, st, ctx, `
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
`, time.Hour)
	applyBundle(t, st, ctx, `
format: 1
organization: acme
project: payments
monitors: {}
`, 0)
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM audit_logs WHERE action LIKE 'monitor.%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the file provider wrote %d monitor audit row(s); a machine write is recorded by its bundle, not here", n)
	}
}

// Reviewer P1 [112]: the delete target and its tenant come from the row held FOR UPDATE, never from a
// caller's copy. Two organizations, so a wrong attribution would be visible as a row in the wrong trail;
// then a stale read, so a region changed after the caller last looked is named as it IS at delete time.
func TestTheDeleteAuditNamesTheLockedRowNotTheCallersCopy(t *testing.T) {
	st, ctx, orgA, projA := auditIncidentFixture(t)
	orgB, _ := st.CreateOrganization(ctx, "other", "Other")
	projB, _ := st.CreateProject(ctx, orgB.ID, "other-api", "Other API")

	a := auditedHTTPMonitor(projA)
	a.Name = "monitor-a"
	createdA, err := st.CreateMonitor(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	b := auditedHTTPMonitor(projB.ID)
	b.Name = "monitor-b"
	b.Region = "eu"
	createdB, err := st.CreateMonitor(ctx, b)
	if err != nil {
		t.Fatal(err)
	}

	// A concurrent edit moves A to another region after "the caller" read it.
	stale := createdA
	moved := createdA
	moved.Region = "apac"
	if _, err := st.UpdateMonitor(ctx, moved); err != nil {
		t.Fatal(err)
	}
	_ = stale // the door takes an id: there is no stale struct to hand it, and that is the fix

	if err := st.DeleteMonitorByPrincipal(ctx, createdA.ID, tokenActor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rowsA := monitorAuditRows(t, st, ctx, orgA)
	if len(rowsA) != 1 || rowsA[0].Action != string(MonitorAuditDelete) {
		t.Fatalf("org A rows = %+v, want exactly the delete", rowsA)
	}
	for _, part := range []string{"monitor " + createdA.ID, createdA.Slug, "region=apac", "· deleted"} {
		if !strings.Contains(rowsA[0].Target, part) {
			t.Fatalf("delete target %q lacks %q — it must name the row as it was at delete time", rowsA[0].Target, part)
		}
	}
	if strings.Contains(rowsA[0].Target, "region=core") {
		t.Fatalf("delete target %q names the region the caller last read, not the locked row's", rowsA[0].Target)
	}
	if rowsB := monitorAuditRows(t, st, ctx, orgB.ID); len(rowsB) != 0 {
		t.Fatalf("a delete in org A left a row in org B: %+v", rowsB)
	}
	var left int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM monitors WHERE id = $1`, createdB.ID).Scan(&left); err != nil || left != 1 {
		t.Fatalf("monitor B: left=%d err=%v", left, err)
	}
}

// The update target reads the STORED slug from the returned row: a caller that omits the slug (legal —
// an omitted slug means "keep it") still produces a target that names it.
func TestTheUpdateAuditNamesTheStoredSlug(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	created, err := st.CreateMonitor(ctx, auditedHTTPMonitor(projID))
	if err != nil {
		t.Fatal(err)
	}
	edit := created
	edit.Slug = "" // the caller does not repeat the slug
	edit.Name = "renamed"
	if _, err := st.UpdateMonitorByPrincipal(ctx, edit, tokenActor); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows := monitorAuditRows(t, st, ctx, orgID)
	if len(rows) != 1 || !strings.Contains(rows[0].Target, created.Slug) {
		t.Fatalf("update target %+v does not name the stored slug %q", rows, created.Slug)
	}
}
