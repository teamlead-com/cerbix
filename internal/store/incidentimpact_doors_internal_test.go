package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.2 — each creating door states the basis it used; a monitor incident takes its project's
// default; a door refuses the kind of incident that is not its own.

func incidentCount(t *testing.T, st *Store, ctx context.Context, projectID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	return n
}

func openMonitorIncident(t *testing.T, st *Store, ctx context.Context, projectID, monitorID string) (domain.Incident, error) {
	t.Helper()
	return st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: projectID, MonitorID: monitorID, Title: "api is down",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}, "Automatically opened", "system")
}

func TestEachCreatingDoorRecordsItsBasis(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	manual := openManualFixture(t, st, ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceAPI,
	})
	if b := readBasis(t, st, ctx, manual.ID); b.Source != domain.ImpactBasisManual {
		t.Errorf("manual basis = %+v", b)
	}

	am, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "alert", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactCritical, Source: domain.SourceAPI, ExternalKey: "fp-1",
	}, domain.ImpactBasisAlertmanager, "firing", "token", fixtureActor)
	if err != nil {
		t.Fatalf("alertmanager create: %v", err)
	}
	if b := readBasis(t, st, ctx, am.ID); b.Source != domain.ImpactBasisAlertmanager {
		t.Errorf("alertmanager basis = %+v", b)
	}

	sys, err := openMonitorIncident(t, st, ctx, f.projectID, f.monitorID)
	if err != nil {
		t.Fatalf("system create: %v", err)
	}
	if sys.Impact != domain.ImpactMajor {
		t.Errorf("system impact = %q, want major", sys.Impact)
	}
	if b := readBasis(t, st, ctx, sys.ID); b.Source != domain.ImpactBasisProjectDefault {
		t.Errorf("system basis = %+v", b)
	}
	// The raw column holds exactly the source key, nothing else (I14).
	var raw string
	if err := st.pool.QueryRow(ctx, `SELECT impact_basis::text FROM incidents WHERE id = $1`, sys.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"source": "project_default"}` {
		t.Errorf("raw basis = %s", raw)
	}
}

func TestThePrincipalDoorRefusesAMissingBasisSource(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	for _, basis := range []domain.ImpactBasisSource{"", domain.ImpactBasisProjectDefault, domain.ImpactBasisServiceMapping} {
		_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
			ProjectID: f.projectID, Title: "x", Status: domain.IncidentInvestigating,
			Impact: domain.ImpactMinor, Source: domain.SourceManual,
		}, basis, "opened", "fixture", fixtureActor)
		if !errors.Is(err, ErrWrongIncidentDoor) {
			t.Errorf("basis %q: err = %v, want ErrWrongIncidentDoor", basis, err)
		}
	}
	if n := incidentCount(t, st, ctx, f.projectID); n != 0 {
		t.Errorf("%d incidents written, want 0", n)
	}
}

func TestTheSystemDoorTakesOnlyAMonitorAutoIncidentWithNoImpact(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	base := domain.Incident{
		ProjectID: f.projectID, MonitorID: f.monitorID, Title: "x",
		Status: domain.IncidentInvestigating, Source: domain.SourceAuto,
	}
	withImpact := base
	withImpact.Impact = domain.ImpactMajor
	noMonitor := base
	noMonitor.MonitorID = ""
	serviceAnchor := base
	serviceAnchor.ServiceID = f.serviceID // MonitorID stays set: only the service clause can refuse it
	manualSource := base
	manualSource.Source = domain.SourceManual
	for name, inc := range map[string]domain.Incident{
		"a supplied impact": withImpact, "no monitor": noMonitor,
		"a service anchor": serviceAnchor, "a manual source": manualSource,
	} {
		if _, err := st.CreateIncidentBySystem(ctx, inc, "opened", "system"); !errors.Is(err, ErrWrongIncidentDoor) {
			t.Errorf("%s: err = %v, want ErrWrongIncidentDoor", name, err)
		}
	}
	if n := incidentCount(t, st, ctx, f.projectID); n != 0 {
		t.Errorf("%d incidents written, want 0", n)
	}
}

func TestAnEmptyImpactIsRefusedOnManualAndAlertmanagerIncidents(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	for name, c := range map[string]struct {
		basis domain.ImpactBasisSource
		inc   domain.Incident
	}{
		"manual":       {domain.ImpactBasisManual, domain.Incident{Source: domain.SourceManual}},
		"alertmanager": {domain.ImpactBasisAlertmanager, domain.Incident{Source: domain.SourceAPI, ExternalKey: "fp-2"}},
		// Valid under R1 (an empty impact is allowed for a monitor auto incident): only the door's
		// own check refuses it. Without it the incident would open at the project default with a
		// manual basis, which breaks I14.
		"auto monitor incident": {domain.ImpactBasisManual, domain.Incident{Source: domain.SourceAuto, MonitorID: f.monitorID}},
	} {
		c.inc.ProjectID, c.inc.Title, c.inc.Status = f.projectID, "x", domain.IncidentInvestigating
		if _, err := st.CreateIncidentByPrincipal(ctx, c.inc, c.basis, "opened", "fixture", fixtureActor); !errors.Is(err, ErrWrongIncidentDoor) {
			t.Errorf("%s: err = %v, want ErrWrongIncidentDoor", name, err)
		}
	}
	if n := incidentCount(t, st, ctx, f.projectID); n != 0 {
		t.Errorf("%d incidents written, want 0", n)
	}
}

func TestACreateForAnUnknownProjectIsNotFound(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: "00000000-0000-0000-0000-000000000000", Title: "x", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, domain.ImpactBasisManual, "opened", "fixture", fixtureActor)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if n := incidentCount(t, st, ctx, f.projectID); n != 0 {
		t.Errorf("%d incidents written, want 0", n)
	}
}

func resolveIncident(t *testing.T, st *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func TestAMonitorIncidentTakesItsProjectDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	for _, want := range []domain.IncidentImpact{domain.ImpactMajor, domain.ImpactCritical, domain.ImpactNone} {
		if want != domain.ImpactMajor {
			if _, err := st.pool.Exec(ctx, `UPDATE projects SET default_incident_impact = $2 WHERE id = $1`,
				f.projectID, string(want)); err != nil {
				t.Fatalf("set default: %v", err)
			}
		}
		inc, err := openMonitorIncident(t, st, ctx, f.projectID, f.monitorID)
		if err != nil {
			t.Fatalf("%s: create: %v", want, err)
		}
		if inc.Impact != want {
			t.Errorf("impact = %q, want %q", inc.Impact, want)
		}
		resolveIncident(t, st, ctx, inc.ID)
	}
}

func TestTheProjectDefaultIsReadInsideTheCreatingTransaction(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `UPDATE projects SET default_incident_impact = 'critical' WHERE id = $1`, f.projectID); err != nil {
		t.Fatal(err)
	}
	// A plain UPDATE takes FOR NO KEY UPDATE, which does not block the insert's foreign-key check.
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := writer.Exec(ctx, `UPDATE projects SET default_incident_impact = 'minor' WHERE id = $1`, f.projectID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		inc domain.Incident
		err error
	}
	done := make(chan result, 1)
	go func() {
		inc, err := openMonitorIncident(t, st, ctx, f.projectID, f.monitorID)
		done <- result{inc, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("create under an open writer: %v", r.err)
		}
		if r.inc.Impact != domain.ImpactCritical {
			t.Errorf("impact = %q, want the committed critical, never the uncommitted minor", r.inc.Impact)
		}
		resolveIncident(t, st, ctx, r.inc.ID)
	case <-time.After(10 * time.Second):
		t.Fatal("the create blocked behind an open writer")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	inc, err := openMonitorIncident(t, st, ctx, f.projectID, f.monitorID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Impact != domain.ImpactMinor {
		t.Errorf("after commit impact = %q, want minor", inc.Impact)
	}
}
