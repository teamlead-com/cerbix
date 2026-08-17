package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func newChild(t *testing.T, st *Store, ctx context.Context, projectID, name, region string) domain.Monitor {
	t.Helper()
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: name, Type: domain.MonitorHTTP,
		Target: "https://" + name + ".example.com/", IntervalSeconds: 60, Region: region, Enabled: true,
	})
	if err != nil {
		t.Fatalf("child %s: %v", name, err)
	}
	return m
}

func newComposite(t *testing.T, st *Store, ctx context.Context, projectID, name, mode, quorum string, children []string) domain.Monitor {
	t.Helper()
	cfg := map[string]string{"children": strings.Join(children, ","), "mode": mode}
	if quorum != "" {
		cfg["quorum"] = quorum
	}
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: name, Type: domain.MonitorComposite,
		IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true, Config: cfg,
	})
	if err != nil {
		t.Fatalf("composite %s: %v", name, err)
	}
	return m
}

// The conversion creates the service, declares the children as BOTH context and SLI, links both
// ends through the one stored column, and leaves the composite running (§15.5, invariant 74).
func TestConvertCompositeToService(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	a := newChild(t, st, ctx, f.projectID, "edge-a", domain.DefaultRegion)
	b := newChild(t, st, ctx, f.projectID, "edge-b", domain.DefaultRegion)
	comp := newComposite(t, st, ctx, f.projectID, "Edge", "all", "", []string{a.ID, b.ID})

	got, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{a.ID, b.ID}, convActor)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.AlreadyConverted {
		t.Fatal("a first conversion reported itself as already converted")
	}
	if got.Service.Name != "Edge" || got.Service.ID == "" {
		t.Fatalf("service = %+v", got.Service)
	}
	if got.Monitor.SupersededByServiceID != got.Service.ID {
		t.Fatalf("link = %q, want the new service", got.Monitor.SupersededByServiceID)
	}
	// The composite keeps running: conversion is not retirement.
	if !got.Monitor.Enabled || got.Monitor.Retired() {
		t.Fatalf("conversion changed execution: enabled=%v retired=%v",
			got.Monitor.Enabled, got.Monitor.Retired())
	}
	// The declaration names both children in both layers.
	detail, err := st.ServiceDetail(ctx, f.projectID, got.Service.ID)
	if err != nil {
		t.Fatalf("service detail: %v", err)
	}
	if len(detail.Declaration.SLI) != 2 || len(detail.Declaration.Monitors) != 2 {
		t.Fatalf("declaration = %+v, want both children in context and SLI", detail.Declaration)
	}
	if detail.Declaration.Policies.Aggregation.Mode != domain.AggAll {
		t.Fatalf("aggregation = %q, want all", detail.Declaration.Policies.Aggregation.Mode)
	}
	// The other end of the link reads the SAME column.
	back, err := st.ListMonitorsSupersededBy(ctx, f.projectID, got.Service.ID)
	if err != nil {
		t.Fatalf("list superseded: %v", err)
	}
	if len(back) != 1 || back[0].ID != comp.ID {
		t.Fatalf("superseded list = %+v, want just the composite", back)
	}

	// Re-confirming is an idempotent no-op returning the existing service.
	again, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{a.ID, b.ID}, convActor)
	if err != nil {
		t.Fatalf("re-convert: %v", err)
	}
	if !again.AlreadyConverted || again.Service.ID != got.Service.ID {
		t.Fatalf("re-convert = %+v, want the existing service reported as already converted", again)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM services WHERE project_id = $1 AND slug = $2`,
		f.projectID, got.Service.Slug).Scan(&n); err != nil {
		t.Fatalf("count services: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-convert created a twin: %d services", n)
	}
}

// The quorum translation inverts a DOWN-vote count into a minimum GOOD count. Getting this
// backwards would silently redefine availability, so the arithmetic is pinned.
func TestCompositeQuorumTranslation(t *testing.T) {
	single := map[string]int{domain.DefaultRegion: 5}
	cases := []struct {
		mode, quorum string
		members      int
		regions      map[string]int
		wantMode     domain.AggMode
		wantDegraded int
		wantErr      error
	}{
		{mode: "all", members: 3, regions: single, wantMode: domain.AggAll},
		{mode: "any", members: 3, regions: single, wantMode: domain.AggAny},
		// 5 children, down when >= 2 are not good ⇒ serving needs >= 4 good.
		{mode: "quorum", quorum: "2", members: 5, regions: single, wantMode: domain.AggQuorum, wantDegraded: 4},
		// down when >= 1 not good is the same rule as `all` expressed as a quorum ⇒ needs all 5.
		{mode: "quorum", quorum: "1", members: 5, regions: single, wantMode: domain.AggQuorum, wantDegraded: 5},
		// down only when ALL are bad is `any` expressed as a quorum ⇒ needs just 1 good.
		{mode: "quorum", quorum: "5", members: 5, regions: single, wantMode: domain.AggQuorum, wantDegraded: 1},
		{
			mode: "quorum", quorum: "2", members: 4,
			regions: map[string]int{domain.DefaultRegion: 2, "eu": 2},
			wantErr: ErrCompositeQuorumNotTranslatable,
		},
		{
			mode: "quorum", quorum: "9", members: 3, regions: single,
			wantErr: ErrCompositeQuorumNotTranslatable,
		},
	}
	for _, c := range cases {
		cfg := map[string]string{"mode": c.mode}
		if c.quorum != "" {
			cfg["quorum"] = c.quorum
		}
		got, err := compositePolicies(domain.Monitor{Config: cfg}, c.members, c.regions)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("mode=%s quorum=%s: err = %v, want %v", c.mode, c.quorum, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("mode=%s quorum=%s: %v", c.mode, c.quorum, err)
			continue
		}
		if got.Aggregation.Mode != c.wantMode {
			t.Errorf("mode=%s: aggregation = %q, want %q", c.mode, got.Aggregation.Mode, c.wantMode)
		}
		if c.wantDegraded != 0 && got.Aggregation.DegradedMin != c.wantDegraded {
			t.Errorf("mode=%s quorum=%s: degraded_min = %d, want %d",
				c.mode, c.quorum, got.Aggregation.DegradedMin, c.wantDegraded)
		}
	}
}

// A slug collision is a named 409, never a suffixed twin.
func TestConvertCompositeSlugCollision(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	a := newChild(t, st, ctx, f.projectID, "api-a", domain.DefaultRegion)
	comp := newComposite(t, st, ctx, f.projectID, "Checkout", "all", "", []string{a.ID})
	// The fixture's service already holds slug "checkout", which the composite's own slug
	// derives to.
	_, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{a.ID}, convActor)
	if !errors.Is(err, ErrServiceSlugTaken) {
		t.Fatalf("collision = %v, want ErrServiceSlugTaken", err)
	}
	if !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("collision error = %v, want the existing slug named", err)
	}
}

// A non-composite, and a composite whose children are all gone, are refused with a reason.
func TestConvertCompositeRefusals(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	if _, err := st.ConvertCompositeToService(ctx, f.projectID, f.monitorID, []string{f.monitorID}, convActor); !errors.Is(err, ErrCompositeNotComposite) {
		t.Fatalf("http monitor = %v, want ErrCompositeNotComposite", err)
	}
	child := newChild(t, st, ctx, f.projectID, "doomed", domain.DefaultRegion)
	survivor := newChild(t, st, ctx, f.projectID, "survivor", domain.DefaultRegion)
	comp := newComposite(t, st, ctx, f.projectID, "Ghost", "all", "", []string{child.ID, survivor.ID})
	if err := st.DeleteMonitor(ctx, child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	// PARTIAL loss: one child gone, one alive. Converting on the survivor alone would silently
	// redefine `all` over 2 as `all` over 1 ([314] P1-5), so it is refused and names what is gone.
	_, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{survivor.ID}, convActor)
	if !errors.Is(err, ErrCompositeChildMissing) || !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("partially-lost composite = %v, want ErrCompositeChildMissing naming the id", err)
	}
}

// [314] P1-5 — the SLI is the operator's STATEMENT, and it is separate from the context. §15.5:
// "requires: explicit confirmation of sli[] — never silently all children".
func TestCompositeConversionRequiresExplicitSLI(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	a := newChild(t, st, ctx, f.projectID, "edge-a", domain.DefaultRegion)
	b := newChild(t, st, ctx, f.projectID, "edge-b", domain.DefaultRegion)
	c := newChild(t, st, ctx, f.projectID, "edge-c", domain.DefaultRegion)
	comp := newComposite(t, st, ctx, f.projectID, "Edge", "all", "", []string{a.ID, b.ID, c.ID})

	// No selection at all: refused, naming how many children are available to choose from.
	_, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, nil, convActor)
	if !errors.Is(err, ErrCompositeSLIRequired) || !strings.Contains(err.Error(), "3") {
		t.Fatalf("empty sli = %v, want ErrCompositeSLIRequired naming the live children", err)
	}
	// A member that is not a child of THIS composite.
	outsider := newChild(t, st, ctx, f.projectID, "outsider", domain.DefaultRegion)
	if _, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID,
		[]string{a.ID, outsider.ID}, convActor); !errors.Is(err, ErrCompositeSLINotAChild) {
		t.Fatalf("foreign sli member = %v, want ErrCompositeSLINotAChild", err)
	}

	// A PROPER SUBSET is honoured: two of three children measure availability, all three stay in
	// the operational context, and the policy is derived from the SELECTION.
	got, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{a.ID, b.ID}, convActor)
	if err != nil {
		t.Fatalf("subset conversion: %v", err)
	}
	detail, err := st.ServiceDetail(ctx, f.projectID, got.Service.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Declaration.Monitors) != 3 {
		t.Fatalf("context = %d monitors, want all 3 children", len(detail.Declaration.Monitors))
	}
	if len(detail.Declaration.SLI) != 2 {
		t.Fatalf("sli = %d members, want the 2 the operator chose", len(detail.Declaration.SLI))
	}
	for _, id := range detail.Declaration.SLI {
		if id == c.ID {
			t.Fatal("an unselected child reached the SLI")
		}
	}
	// The audit row records both counts, so "what was measured" is recoverable later.
	var target string
	if err := st.pool.QueryRow(ctx, `
		SELECT target FROM audit_logs WHERE action = 'monitor.converted_to_service'
		 ORDER BY created_at DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(target, "context=3") || !strings.Contains(target, "sli=2") {
		t.Fatalf("audit target = %q, want both counts", target)
	}
}

// [314] P1-6 — the lifecycle actions are COMPOSITE-only, and the file-ownership rule is the
// asymmetry [316] settled: the annotation is permitted (it is not a declared field and no reapply
// can restate it), while retire/reactivate and conversion refuse (they write declared state).
func TestCompositeLifecycleScopeAndFileOwnership(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	child := newChild(t, st, ctx, f.projectID, "child", domain.DefaultRegion)
	comp := newComposite(t, st, ctx, f.projectID, "Edge", "all", "", []string{child.ID})

	// Composite-only, on every action.
	if _, err := st.RetireMonitor(ctx, f.projectID, f.monitorID, convActor); !errors.Is(err, ErrNotAComposite) {
		t.Fatalf("retire on an http monitor = %v, want ErrNotAComposite", err)
	}
	if _, err := st.SetMonitorSuccessor(ctx, f.projectID, f.monitorID, f.serviceID, convActor); !errors.Is(err, ErrNotAComposite) {
		t.Fatalf("successor on an http monitor = %v, want ErrNotAComposite", err)
	}

	// Now claim file ownership of the composite.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid, source_path)
		VALUES ($1,'platform',$2,$3,'edge','/etc/cerbix/monitoring.d/edge.yaml')`,
		comp.ID, f.orgID, f.projectID); err != nil {
		t.Fatalf("claim ownership: %v", err)
	}
	before, err := st.GetMonitor(ctx, comp.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// The ANNOTATION is permitted: it is not a declared field, so no reapply can contradict it.
	linked, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, f.serviceID, convActor)
	if err != nil {
		t.Fatalf("annotating a file-managed composite must be permitted: %v", err)
	}
	if linked.SupersededByServiceID != f.serviceID {
		t.Fatalf("successor = %q", linked.SupersededByServiceID)
	}
	// And it moves NO execution state: not the config generation, not the liveness watermark, not
	// the transition sequence.
	if linked.ExecutionRevision != before.ExecutionRevision || linked.StateSequence != before.StateSequence {
		t.Fatalf("the annotation moved execution state: rev %d→%d, seq %d→%d",
			before.ExecutionRevision, linked.ExecutionRevision, before.StateSequence, linked.StateSequence)
	}
	if linked.Enabled != before.Enabled || linked.Status != before.Status {
		t.Fatalf("the annotation changed execution: enabled %v→%v, status %q→%q",
			before.Enabled, linked.Enabled, before.Status, linked.Status)
	}
	// Clearing it is equally permitted, and audited.
	if _, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, "", convActor); err != nil {
		t.Fatalf("clearing on a file-managed composite: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action IN ('monitor.successor.set','monitor.successor.cleared')`).
		Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d audit rows for two annotation acts", n)
	}

	// The DECLARED-state actions refuse.
	if _, err := st.RetireMonitor(ctx, f.projectID, comp.ID, convActor); !errors.Is(err, ErrManagedByFile) {
		t.Fatalf("retiring a file-managed composite = %v, want ErrManagedByFile", err)
	}
	if _, err := st.ConvertCompositeToService(ctx, f.projectID, comp.ID, []string{child.ID}, convActor); !errors.Is(err, ErrManagedByFile) {
		t.Fatalf("converting a file-managed composite = %v, want ErrManagedByFile", err)
	}
	// Deleting the successor service clears the annotation through the FK, not through a hook.
	if _, err := st.SetMonitorSuccessor(ctx, f.projectID, comp.ID, f.serviceID, convActor); err != nil {
		t.Fatalf("re-annotate: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	after, err := st.GetMonitor(ctx, comp.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.SupersededByServiceID != "" {
		t.Fatalf("the annotation survived its service: %q", after.SupersededByServiceID)
	}
}
