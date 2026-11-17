package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 / NFR-035 §4.1 — the service's impact mapping rides the same UI write path as `page_on`:
// read resolved, audited as one segment, NULL and explicit defaults one policy, and it is NOT a
// definition edit (no revision, no epoch) and NOT a paging edit (no generation bump, no close).

var customMapping = domain.ImpactMapping{
	Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone,
}

func writeMapping(t *testing.T, st *Store, f alertFixture, m domain.ImpactMapping) (domain.ServiceAlertPolicy, error) {
	t.Helper()
	return st.UpdateServiceAlertPolicy(t.Context(), f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{ImpactByState: &m}, AlertActor{})
}

func mappingColumnIsNull(t *testing.T, st *Store, f alertFixture) bool {
	t.Helper()
	var isNull bool
	if err := st.pool.QueryRow(t.Context(),
		`SELECT impact_by_state IS NULL FROM services WHERE id = $1`, f.serviceID).Scan(&isNull); err != nil {
		t.Fatalf("read column: %v", err)
	}
	return isNull
}

func TestTheMappingReadResolvesNullToTheDefaults(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	if !mappingColumnIsNull(t, st, f) {
		t.Fatal("a fresh service does not have a NULL mapping")
	}
	got, err := st.ServiceAlertPolicy(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ImpactByState != domain.DefaultImpactMapping() {
		t.Fatalf("read mapping = %+v, want the defaults", got.ImpactByState)
	}
}

func TestAMappingWriteIsAuditedAsOneSegment(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	if _, err := writeMapping(t, st, f, customMapping); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "service=" + f.serviceID +
		" impact_by_state:degraded=minor,down=major,unknown=none→degraded=minor,down=critical,unknown=none"
	got := auditTargets(t, st, ctx, "service.alerting")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("audit targets = %q, want exactly [%q]", got, want)
	}
	read, err := st.ServiceAlertPolicy(ctx, f.projectID, f.serviceID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.ImpactByState != customMapping {
		t.Fatalf("read mapping = %+v, want %+v", read.ImpactByState, customMapping)
	}
	if mappingColumnIsNull(t, st, f) {
		t.Fatal("a non-default mapping was stored as NULL")
	}
}

func TestWritingTheDefaultsOverNullIsNoChange(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	updatedAt := func() (s string) {
		t.Helper()
		if err := st.pool.QueryRow(ctx,
			`SELECT updated_at::text FROM services WHERE id = $1`, f.serviceID).Scan(&s); err != nil {
			t.Fatalf("updated_at: %v", err)
		}
		return
	}
	before := updatedAt()

	if _, err := writeMapping(t, st, f, domain.DefaultImpactMapping()); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	if rows := auditTargets(t, st, ctx, "service.alerting"); len(rows) != 0 {
		t.Fatalf("explicit defaults over NULL wrote audit rows: %q", rows)
	}
	if updatedAt() != before {
		t.Fatal("explicit defaults over NULL stamped updated_at")
	}
	if !mappingColumnIsNull(t, st, f) {
		t.Fatal("explicit defaults over NULL stored an object")
	}

	if _, err := writeMapping(t, st, f, customMapping); err != nil {
		t.Fatalf("write custom: %v", err)
	}
	if _, err := writeMapping(t, st, f, domain.DefaultImpactMapping()); err != nil {
		t.Fatalf("write defaults back: %v", err)
	}
	if !mappingColumnIsNull(t, st, f) {
		t.Fatal("the defaults written over a custom mapping were not stored as NULL (R3)")
	}
	rows := auditTargets(t, st, ctx, "service.alerting")
	wantBack := "service=" + f.serviceID +
		" impact_by_state:degraded=minor,down=critical,unknown=none→degraded=minor,down=major,unknown=none"
	if len(rows) != 2 || rows[1] != wantBack {
		t.Fatalf("audit targets = %q, want the second to be %q", rows, wantBack)
	}
}

func TestAPartialMappingIsRefusedByTheStore(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	if _, err := writeMapping(t, st, f, domain.ImpactMapping{Down: domain.ImpactCritical}); err == nil {
		t.Fatal("a partial mapping was accepted")
	}
	if !mappingColumnIsNull(t, st, f) {
		t.Fatal("a refused mapping changed the column")
	}
	if rows := auditTargets(t, st, ctx, "service.alerting"); len(rows) != 0 {
		t.Fatalf("a refused mapping wrote audit rows: %q", rows)
	}
}

func TestAMappingWriteCreatesNoRevisionNoEpochAndNoGenerationBump(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	counts := func() (revisions, epochs, generation int64) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id = $1),
			       (SELECT count(*) FROM service_evaluation_epochs WHERE service_id = $1),
			       (SELECT alert_config_generation FROM services WHERE id = $1)`,
			f.serviceID).Scan(&revisions, &epochs, &generation); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return
	}
	r0, e0, g0 := counts()
	if _, err := writeMapping(t, st, f, customMapping); err != nil {
		t.Fatalf("write: %v", err)
	}
	r1, e1, g1 := counts()
	if r0 != r1 || e0 != e1 || g0 != g1 {
		t.Fatalf("a mapping write moved revisions %d→%d, epochs %d→%d, generation %d→%d",
			r0, r1, e0, e1, g0, g1)
	}
}

func TestAMappingWriteEndsNoAnnouncement(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	if got := evalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("the fixture did not open an episode (%+v)", got)
	}

	if _, err := writeMapping(t, st, f, customMapping); err != nil {
		t.Fatalf("write: %v", err)
	}
	var open, closed int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE closed_at IS NULL), count(*) FILTER (WHERE closed_at IS NOT NULL)
		  FROM service_alert_episodes WHERE service_id = $1`, f.serviceID).Scan(&open, &closed); err != nil {
		t.Fatalf("episodes: %v", err)
	}
	if open != 1 || closed != 0 {
		t.Fatalf("a mapping write left %d open and %d closed episodes, want 1 and 0", open, closed)
	}
}
