package store

import (
	"errors"
	"strings"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// TestMonitorDependencies covers the graph invariants and the suppression
// lookup: replace-set round-trip, foreign/self/cycle rejection (incl.
// transitive), DownAncestors by status and by open auto-incident, delete
// cascade, and the idempotent suppression note.
func TestMonitorDependencies(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	other, _ := st.CreateProject(ctx, org.ID, "web", "Web")
	mk := func(projID, name string) domain.Monitor {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: projID, Name: name, Type: domain.MonitorTCP, Target: "10.0.0.1:80",
			IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return m
	}
	pg := mk(proj.ID, "postgres-main")
	apiSvc := mk(proj.ID, "api-svc")
	checkout := mk(proj.ID, "checkout")
	foreign := mk(other.ID, "outsider")

	// checkout → api-svc → postgres-main
	if err := st.ReplaceMonitorDependencies(ctx, apiSvc.ID, proj.ID, []string{pg.ID}); err != nil {
		t.Fatalf("set api deps: %v", err)
	}
	if err := st.ReplaceMonitorDependencies(ctx, checkout.ID, proj.ID, []string{apiSvc.ID}); err != nil {
		t.Fatalf("set checkout deps: %v", err)
	}
	// depends_on comes back with the monitor.
	got, _ := st.GetMonitor(ctx, apiSvc.ID)
	if len(got.DependsOn) != 1 || got.DependsOn[0] != pg.ID {
		t.Fatalf("depends_on round-trip: %v", got.DependsOn)
	}

	// Rejections: foreign project, self, direct cycle, transitive cycle.
	if err := st.ReplaceMonitorDependencies(ctx, apiSvc.ID, proj.ID, []string{foreign.ID}); !errors.Is(err, ErrDependencyForeign) {
		t.Fatalf("foreign parent: %v", err)
	}
	if err := st.ReplaceMonitorDependencies(ctx, apiSvc.ID, proj.ID, []string{apiSvc.ID}); !errors.Is(err, ErrDependencyForeign) {
		t.Fatalf("self parent: %v", err)
	}
	if err := st.ReplaceMonitorDependencies(ctx, pg.ID, proj.ID, []string{apiSvc.ID}); !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("direct cycle: %v", err)
	}
	if err := st.ReplaceMonitorDependencies(ctx, pg.ID, proj.ID, []string{checkout.ID}); !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("transitive cycle: %v", err)
	}

	// DownAncestors: nothing down yet.
	if anc, err := st.DownAncestors(ctx, checkout.ID); err != nil || len(anc) != 0 {
		t.Fatalf("healthy graph: %v %v", anc, err)
	}
	// postgres-main goes down → transitive ancestor of checkout.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET status='down' WHERE id=$1`, pg.ID); err != nil {
		t.Fatalf("mark down: %v", err)
	}
	anc, err := st.DownAncestors(ctx, checkout.ID)
	if err != nil || len(anc) != 1 || anc[0].Name != "postgres-main" {
		t.Fatalf("transitive down ancestor: %+v err=%v", anc, err)
	}
	// The root itself has no down ancestors.
	if anc, _ := st.DownAncestors(ctx, pg.ID); len(anc) != 0 {
		t.Fatalf("root must have no down ancestors: %v", anc)
	}

	// Open auto-incident counts as down even before the status flips back.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET status='up' WHERE id=$1`, pg.ID); err != nil {
		t.Fatal(err)
	}
	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: pg.ID, Title: "postgres-main is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto", "system")
	if err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if anc, _ := st.DownAncestors(ctx, apiSvc.ID); len(anc) != 1 {
		t.Fatalf("open auto-incident must count as down: %v", anc)
	}

	// Suppression note on the child's open auto-incident — once.
	childInc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: proj.ID, MonitorID: apiSvc.ID, Title: "api-svc is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "auto", "system")
	if err != nil {
		t.Fatalf("child incident: %v", err)
	}
	if added, err := st.AppendSuppressionNote(ctx, apiSvc.ID, "postgres-main"); err != nil || !added {
		t.Fatalf("first note: added=%v err=%v", added, err)
	}
	if added, err := st.AppendSuppressionNote(ctx, apiSvc.ID, "postgres-main"); err != nil || added {
		t.Fatalf("second note must be a no-op: added=%v err=%v", added, err)
	}
	ups, _ := st.ListIncidentUpdates(ctx, childInc.ID)
	notes := 0
	for _, u := range ups {
		if strings.HasPrefix(u.Body, domain.SuppressionMarker) {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("suppression notes = %d, want 1", notes)
	}
	_ = inc

	// Deleting a parent cascades its edges away.
	if err := st.DeleteMonitor(ctx, pg.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	got, _ = st.GetMonitor(ctx, apiSvc.ID)
	if len(got.DependsOn) != 0 {
		t.Fatalf("edges must cascade on parent delete: %v", got.DependsOn)
	}
}
