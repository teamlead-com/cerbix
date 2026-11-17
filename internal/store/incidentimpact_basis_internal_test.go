package store

import (
	"errors"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.6 / D15 — the basis has its own read, scoped to its project at the store boundary.
func TestTheBasisReadIsScopedToItsProject(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDown, m)

	got, err := st.IncidentImpactBasis(ctx, f.projectID, inc.ID)
	if err != nil || got == nil || got.Source != domain.ImpactBasisServiceMapping || got.Mapping == nil ||
		*got.Mapping != m || got.State != domain.ServiceAlertDown || got.Service == nil {
		t.Fatalf("basis = %+v, %v", got, err)
	}
	otherProj, err := st.CreateProject(ctx, f.orgID, "elsewhere", "Elsewhere")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	other := otherProj.ID
	if _, err = st.IncidentImpactBasis(ctx, other, inc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another project's read = %v, want ErrNotFound", err)
	}
	if _, err := st.IncidentImpactBasis(ctx, f.projectID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id = %v, want ErrNotFound", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET impact_basis = NULL WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("null the basis: %v", err)
	}
	if b, err := st.IncidentImpactBasis(ctx, f.projectID, inc.ID); b != nil || err != nil {
		t.Errorf("NULL basis = %+v, %v, want (nil, nil)", b, err)
	}
}
