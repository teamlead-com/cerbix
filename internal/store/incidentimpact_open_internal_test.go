package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.2 — a service auto-incident opens at mapping[state] and records the mapping it was
// classified under (D8).

// openMappedServiceIncident opens a service auto-incident in its own committed transaction at the given
// state and mapping. Later tasks reuse it.
func openMappedServiceIncident(t *testing.T, st *Store, ctx context.Context, serviceID, projectID string,
	state domain.ServiceAlertState, mapping domain.ImpactMapping, confirmedOver int) (domain.Incident, bool, error) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	inc, created, err := st.OpenServiceIncidentTx(ctx, tx, serviceID, projectID, "checkout "+string(state),
		state, mapping, confirmedOver, governingRevision(t, st, ctx, serviceID))
	if err != nil {
		return inc, created, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inc, created, nil
}

func readBasis(t *testing.T, st *Store, ctx context.Context, incidentID string) domain.ImpactBasis {
	t.Helper()
	var raw []byte
	if err := st.pool.QueryRow(ctx, `SELECT impact_basis FROM incidents WHERE id = $1`, incidentID).Scan(&raw); err != nil {
		t.Fatalf("read basis: %v", err)
	}
	var b domain.ImpactBasis
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode basis %s: %v", raw, err)
	}
	return b
}

func openingNote(t *testing.T, st *Store, ctx context.Context, incidentID string) string {
	t.Helper()
	var body string
	if err := st.pool.QueryRow(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 ORDER BY created_at, id LIMIT 1`,
		incidentID).Scan(&body); err != nil {
		t.Fatalf("read opening note: %v", err)
	}
	return body
}

func TestAServiceIncidentOpensAtItsMappedImpact(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	custom := domain.ImpactMapping{Degraded: domain.ImpactCritical, Down: domain.ImpactCritical, Unknown: domain.ImpactMajor}
	for _, m := range []domain.ImpactMapping{domain.DefaultImpactMapping(), custom} {
		for _, state := range []domain.ServiceAlertState{
			domain.ServiceAlertDegraded, domain.ServiceAlertDown, domain.ServiceAlertUnknown,
		} {
			want, _ := m.For(state)
			inc, created, err := openMappedServiceIncident(t, st, ctx, f.serviceID, f.projectID, state, m, 3)
			if err != nil || !created {
				t.Fatalf("%s/%s: open: created=%v err=%v", m, state, created, err)
			}
			if inc.Impact != want {
				t.Errorf("%s/%s: impact %q, want %q", m, state, inc.Impact, want)
			}
			b := readBasis(t, st, ctx, inc.ID)
			if b.Source != domain.ImpactBasisServiceMapping || b.State != state ||
				b.Mapping == nil || *b.Mapping != m || b.Service == nil ||
				b.Service.Name != "Checkout" || b.Service.Slug != "checkout" {
				t.Errorf("%s/%s: basis %+v", m, state, b)
			}
			if got, want := openingNote(t, st, ctx, inc.ID), domain.ServiceIncidentOpeningNote(state, 3); got != want {
				t.Errorf("%s/%s: opening note %q, want %q", m, state, got, want)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, inc.ID); err != nil {
				t.Fatalf("resolve: %v", err)
			}
		}
	}
}

func TestTheBasisSnapshotOutlivesTheService(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc, _, err := openMappedServiceIncident(t, st, ctx, f.serviceID, f.projectID, domain.ServiceAlertDown, domain.DefaultImpactMapping(), 2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	var serviceNull bool
	var name, slug string
	if err := st.pool.QueryRow(ctx,
		`SELECT service_id IS NULL, impact_basis->'service'->>'name', impact_basis->'service'->>'slug'
		   FROM incidents WHERE id = $1`, inc.ID).Scan(&serviceNull, &name, &slug); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !serviceNull || name != "Checkout" || slug != "checkout" {
		t.Errorf("service_id null=%v name=%q slug=%q; the snapshot must outlive the service", serviceNull, name, slug)
	}
}

func TestOpenServiceIncidentRefusesAStateThatCannotOpenOne(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	cases := []struct {
		name  string
		state domain.ServiceAlertState
		m     domain.ImpactMapping
	}{
		{"healthy", domain.ServiceAlertHealthy, domain.DefaultImpactMapping()},
		{"excluded", domain.ServiceAlertExcluded, domain.DefaultImpactMapping()},
		{"partial mapping", domain.ServiceAlertDown, domain.ImpactMapping{Down: domain.ImpactMajor}},
	}
	for _, c := range cases {
		if _, _, err := openMappedServiceIncident(t, st, ctx, f.serviceID, f.projectID, c.state, c.m, 2); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incidents`).Scan(&n); err != nil || n != 0 {
		t.Errorf("incident rows = %d (err %v), want 0", n, err)
	}
}

func TestTheEvaluatorOpensAtTheMappedImpact(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{ImpactByState: &m}, AlertActor{}); err != nil {
		t.Fatalf("set mapping: %v", err)
	}
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)

	var id string
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(id::text), '') FROM incidents WHERE service_id = $1`,
		f.serviceID).Scan(&n, &id); err != nil || n != 1 {
		t.Fatalf("incidents = %d (err %v), want 1", n, err)
	}
	var impact string
	if err := st.pool.QueryRow(ctx, `SELECT impact FROM incidents WHERE id = $1`, id).Scan(&impact); err != nil {
		t.Fatal(err)
	}
	if impact != "critical" {
		t.Errorf("impact %q, want critical", impact)
	}
	b := readBasis(t, st, ctx, id)
	if b.State != domain.ServiceAlertDown || b.Mapping == nil || *b.Mapping != m {
		t.Errorf("basis %+v", b)
	}
	if got, want := openingNote(t, st, ctx, id), "Opened automatically: service DOWN confirmed over 2 evaluation(s)."; got != want {
		t.Errorf("note %q, want %q", got, want)
	}
}
