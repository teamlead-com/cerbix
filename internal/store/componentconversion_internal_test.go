package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

var convActor = GraphActor{Label: "ops@example.com"}

// The preview and the confirmation share ONE validator, and consent is bound to BOTH tokens.
func TestComponentConversionCASOnBothTokens(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Payments API", MonitorID: f.monitorID,
	})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	target := ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID}
	plan, err := st.PreviewComponentConversion(ctx, f.orgID, c.ID, target)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if plan.Proposed.Source != domain.ComponentSourceService || plan.Proposed.ServiceID != f.serviceID {
		t.Fatalf("proposed = %+v, want the service source bound", plan.Proposed)
	}
	// The proposed row keeps the monitor binding: that is what makes the revert possible.
	if plan.Proposed.MonitorID != f.monitorID || plan.RevertsTo != domain.ComponentSourceMonitor {
		t.Fatalf("proposed = %+v, revertsTo=%q — want the monitor kept dormant", plan.Proposed, plan.RevertsTo)
	}
	if plan.NoOp {
		t.Fatal("a real source change was reported as a no-op")
	}

	// (a) a stale COMPONENT revision is refused.
	if _, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, target,
		plan.Revision+1, plan.PageGeneration, convActor); !errors.Is(err, ErrComponentConversionStale) {
		t.Fatalf("stale revision = %v, want ErrComponentConversionStale", err)
	}
	// (b) a NEIGHBOUR's edit invalidates the consent too: the preview showed a page summary.
	if _, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "neighbour"}); err != nil {
		t.Fatalf("neighbour: %v", err)
	}
	if _, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, target,
		plan.Revision, plan.PageGeneration, convActor); !errors.Is(err, ErrComponentConversionStale) {
		t.Fatalf("neighbour edit did not invalidate the preview: %v", err)
	}

	// (c) re-previewing and confirming succeeds, and BOTH counters advance.
	plan2, err := st.PreviewComponentConversion(ctx, f.orgID, c.ID, target)
	if err != nil {
		t.Fatalf("re-preview: %v", err)
	}
	got, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, target,
		plan2.Revision, plan2.PageGeneration, convActor)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got.Source != domain.ComponentSourceService || got.ServiceID != f.serviceID || got.MonitorID != f.monitorID {
		t.Fatalf("converted = %+v, want service active with the monitor dormant", got)
	}
	if got.Revision <= plan2.Revision {
		t.Fatalf("component revision did not advance: %d → %d", plan2.Revision, got.Revision)
	}
	var action, auditTarget string
	if err := st.pool.QueryRow(ctx, `
		SELECT action, target FROM audit_logs
		 WHERE action = 'statuspage.component.converted' ORDER BY created_at DESC LIMIT 1`).
		Scan(&action, &auditTarget); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	if !strings.Contains(auditTarget, "from=monitor") || !strings.Contains(auditTarget, "to=service") {
		t.Fatalf("audit target = %q, want both ends named", auditTarget)
	}
}

// The revert restores what was replaced WITHOUT the operator naming it again, and a manual
// status survives a round trip through another source.
func TestComponentConversionIsReversible(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Third-party CDN", ManualStatus: domain.CompDegraded,
	})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	toService := ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID}
	plan, err := st.PreviewComponentConversion(ctx, f.orgID, c.ID, toService)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if plan.RevertsTo != domain.ComponentSourceManual {
		t.Fatalf("revertsTo = %q, want manual (the stated status is what it would restore)", plan.RevertsTo)
	}
	conv, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, toService,
		plan.Revision, plan.PageGeneration, convActor)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if conv.ManualStatus != domain.CompDegraded {
		t.Fatalf("manual status was lost on conversion: %q", conv.ManualStatus)
	}
	// An unrelated edit must NOT wipe the dormant manual status — the edit form of a
	// service-backed component does not show it, so it submits an empty value.
	edited, err := st.UpdateComponent(ctx, f.orgID, domain.Component{
		ID: conv.ID, StatusPageID: conv.StatusPageID, Name: "Third-party CDN (EU)",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if edited.ManualStatus != domain.CompDegraded {
		t.Fatalf("an edit wiped the dormant manual status: %q", edited.ManualStatus)
	}

	// The revert names NO id and NO status: the dormant values are the target.
	back := ComponentConversionTarget{Source: domain.ComponentSourceManual}
	plan2, err := st.PreviewComponentConversion(ctx, f.orgID, c.ID, back)
	if err != nil {
		t.Fatalf("revert preview: %v", err)
	}
	rev, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, back,
		plan2.Revision, plan2.PageGeneration, convActor)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if rev.Source != domain.ComponentSourceManual || rev.ManualStatus != domain.CompDegraded {
		t.Fatalf("revert = %+v, want manual/degraded restored", rev)
	}
	if rev.ServiceID != f.serviceID {
		t.Fatalf("revert dropped the service binding %q — a re-convert would need it named again", rev.ServiceID)
	}
}

// Confirming twice is safe, and the second call is not recorded as an act.
func TestComponentConversionNoOpIsIdempotent(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Checkout", ServiceID: f.serviceID,
	})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	target := ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: f.serviceID}
	plan, err := st.PreviewComponentConversion(ctx, f.orgID, c.ID, target)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !plan.NoOp {
		t.Fatal("converting to the current source was not reported as a no-op")
	}
	got, err := st.ConfirmComponentConversion(ctx, f.orgID, c.ID, target,
		plan.Revision, plan.PageGeneration, convActor)
	if err != nil {
		t.Fatalf("no-op confirm: %v", err)
	}
	if got.Revision != plan.Revision {
		t.Fatalf("a no-op advanced the revision: %d → %d", plan.Revision, got.Revision)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = 'statuspage.component.converted'`).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 0 {
		t.Fatalf("a no-op wrote %d audit rows", n)
	}
}

// A target from another tenant is refused by the validator, not by the FK.
func TestComponentConversionRefusesForeignTarget(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "x"})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	proj2, _ := st.CreateProject(ctx, org2.ID, "p2", "P2")
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: proj2.ID, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien service: %v", err)
	}
	_, err = st.PreviewComponentConversion(ctx, f.orgID, c.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: alien.ID})
	if !errors.Is(err, ErrComponentConversionTarget) {
		t.Fatalf("foreign target = %v, want ErrComponentConversionTarget", err)
	}
	// And a same-org service from a project the page is not scoped to is refused with a reason,
	// rather than reaching the deferred trigger.
	other, err := st.CreateService(ctx, domain.Service{ProjectID: f.otherProj, Slug: "s", Name: "s"})
	if err != nil {
		t.Fatalf("other service: %v", err)
	}
	_, err = st.PreviewComponentConversion(ctx, f.orgID, c.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceService, ServiceID: other.ID})
	if !errors.Is(err, ErrComponentConversionTarget) || !strings.Contains(err.Error(), "scoped") {
		t.Fatalf("out-of-scope target = %v, want a named page-scope refusal", err)
	}
	// `no_data` cannot be stated by an operator, on any path.
	_, err = st.PreviewComponentConversion(ctx, f.orgID, c.ID,
		ComponentConversionTarget{Source: domain.ComponentSourceManual, ManualStatus: domain.CompNoData})
	if !errors.Is(err, ErrComponentConversionTarget) {
		t.Fatalf("manual no_data = %v, want a refusal", err)
	}
}

// Retiring is ONE transaction that ends both the lifecycle and the execution, and it deletes
// nothing (§15.5, invariant 74).
func TestRetireMonitorEndsLifecycleAndExecution(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	// A COMPOSITE: §15.5 scopes the lifecycle to composites, and the earlier version of this test
	// used the fixture's HTTP monitor — pinning surface the phase never had ([314] P1-6).
	comp, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "Edge", Type: domain.MonitorComposite,
		IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true,
		Config: map[string]string{"children": f.monitorID, "mode": "all"},
	})
	if err != nil {
		t.Fatalf("composite: %v", err)
	}
	before, err := st.GetMonitor(ctx, comp.ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}

	// The successor link alone changes NOTHING about execution — that is why it is separate.
	linked, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, f.serviceID, convActor)
	if err != nil {
		t.Fatalf("set successor: %v", err)
	}
	if linked.SupersededByServiceID != f.serviceID {
		t.Fatalf("successor = %q, want the service", linked.SupersededByServiceID)
	}
	if !linked.Enabled || linked.Retired() {
		t.Fatalf("the link changed execution: enabled=%v retired=%v", linked.Enabled, linked.Retired())
	}
	if linked.ExecutionRevision != before.ExecutionRevision {
		t.Fatalf("the link bumped the config fence %d → %d, forcing a needless re-probe",
			before.ExecutionRevision, linked.ExecutionRevision)
	}

	retired, err := st.RetireMonitor(ctx, f.projectID, comp.ID, convActor)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if !retired.Retired() {
		t.Fatal("retired_at was not set")
	}
	if retired.Enabled {
		t.Fatal("a retired monitor is still enabled — it would keep probing and paging")
	}
	if retired.Status != domain.StatusPending || retired.ConsecutiveFailures != 0 {
		t.Fatalf("live state not reset: status=%q failures=%d", retired.Status, retired.ConsecutiveFailures)
	}
	if retired.StateSequence <= before.StateSequence || retired.ExecutionRevision <= before.ExecutionRevision {
		t.Fatalf("fences did not advance: seq %d→%d, rev %d→%d",
			before.StateSequence, retired.StateSequence, before.ExecutionRevision, retired.ExecutionRevision)
	}
	// Nothing was deleted: the monitor is still there, still readable, link intact.
	again, err := st.GetMonitor(ctx, comp.ID)
	if err != nil {
		t.Fatalf("a retired monitor must stay readable: %v", err)
	}
	if again.SupersededByServiceID != f.serviceID {
		t.Fatalf("retire dropped the successor link: %q", again.SupersededByServiceID)
	}

	if _, err := st.RetireMonitor(ctx, f.projectID, comp.ID, convActor); !errors.Is(err, ErrMonitorAlreadyRetired) {
		t.Fatalf("second retire = %v, want ErrMonitorAlreadyRetired", err)
	}

	back, err := st.ReactivateMonitor(ctx, f.projectID, comp.ID, convActor)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if back.Retired() || !back.Enabled {
		t.Fatalf("reactivate = retired:%v enabled:%v", back.Retired(), back.Enabled)
	}
	if back.Status != domain.StatusPending {
		t.Fatalf("reactivate resumed status %q — a pre-retire observation is not evidence about now",
			back.Status)
	}
	if _, err := st.ReactivateMonitor(ctx, f.projectID, comp.ID, convActor); !errors.Is(err, ErrMonitorNotRetired) {
		t.Fatalf("second reactivate = %v, want ErrMonitorNotRetired", err)
	}

	// A successor must be a service of the SAME project.
	if _, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, f.pageProj, convActor); !errors.Is(err, ErrSuccessorNotAService) {
		t.Fatalf("bogus successor = %v, want ErrSuccessorNotAService", err)
	}
	// Both acts are audited.
	for _, action := range []string{"monitor.successor.set", "monitor.retired", "monitor.reactivated"} {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", action, err)
		}
		if n == 0 {
			t.Errorf("%s was not audited", action)
		}
	}
}

// Deleting a service SET NULLs the annotation rather than corrupting the monitor (§15.5).
func TestSuccessorLinkClearedOnServiceDelete(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	comp, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "Edge", Type: domain.MonitorComposite,
		IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true,
		Config: map[string]string{"children": f.monitorID, "mode": "all"},
	})
	if err != nil {
		t.Fatalf("composite: %v", err)
	}
	if _, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, f.serviceID, convActor); err != nil {
		t.Fatalf("set successor: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	m, err := st.GetMonitor(ctx, comp.ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if m.SupersededByServiceID != "" {
		t.Fatalf("successor link survived the service deletion: %q", m.SupersededByServiceID)
	}
	if !m.Enabled {
		t.Fatal("losing an annotation disabled the monitor")
	}
}
