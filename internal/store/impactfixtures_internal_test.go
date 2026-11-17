package store

import (
	"context"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.2 — fixtures for the creating doors. The system door takes one kind of incident (a
// monitor-anchored auto incident with no impact), so a test that needs any other incident opens it
// through the door that kind really comes through.

// fixtureActor is the principal a fixture-created manual incident is attributed to.
var fixtureActor = AuditActor{ViaToken: true, Label: "token:fixture"}

// fixtureMonitor creates an enabled HTTP monitor in the project and returns its id.
func fixtureMonitor(t *testing.T, st *Store, ctx context.Context, projectID string) string {
	t.Helper()
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: 1,
		ProjectID:         projectID, Name: "fixture-http", Type: domain.MonitorHTTP,
		Target: "https://fixture.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("fixture monitor: %v", err)
	}
	return mon.ID
}

// openManualFixture opens an incident through the principal door with a manual basis.
func openManualFixture(t *testing.T, st *Store, ctx context.Context, inc domain.Incident) domain.Incident {
	t.Helper()
	created, err := st.CreateIncidentByPrincipal(ctx, inc, domain.ImpactBasisManual, "opened", "fixture", fixtureActor)
	if err != nil {
		t.Fatalf("open manual fixture: %v", err)
	}
	return created
}

// openServiceFixture opens a service auto-incident whose down impact is `impact`, in its own
// committed transaction.
func openServiceFixture(t *testing.T, st *Store, ctx context.Context, projectID, serviceID, title string,
	impact domain.IncidentImpact) domain.Incident {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	inc, _, err := st.OpenServiceIncidentTx(ctx, tx, serviceID, projectID, title, domain.ServiceAlertDown,
		domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: impact, Unknown: domain.ImpactNone}, 1, nil)
	if err != nil {
		t.Fatalf("open service fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inc
}
