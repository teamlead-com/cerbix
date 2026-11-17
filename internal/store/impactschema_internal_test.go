package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

// FR-042 / NFR-035 §5 — migration 00119.

func TestIncidentImpactMigrationUpAndDown(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	db, _, cleanup := probeDatabaseAt(t, st, ctx, "impact", 118)
	defer cleanup()

	// The old binary's shape: only pre-00119 columns are named.
	var orgID, projID, svcID, incID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('acme','Acme') RETURNING id::text`).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id::text`, orgID).Scan(&projID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1,'checkout','Checkout') RETURNING id::text`, projID).Scan(&svcID); err != nil {
		t.Fatalf("service: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO incidents (project_id, service_id, title, status, impact, source)
		 VALUES ($1,$2,'old','investigating','major','auto') RETURNING id::text`, projID, svcID).Scan(&incID); err != nil {
		t.Fatalf("incident: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 119); err != nil {
		t.Fatalf("up to 119: %v", err)
	}
	var def string
	var mapNull, basisNull bool
	if err := db.QueryRowContext(ctx, `SELECT default_incident_impact FROM projects WHERE id = $1`, projID).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if def != "major" {
		t.Fatalf("default_incident_impact = %q, want major", def)
	}
	if err := db.QueryRowContext(ctx, `SELECT impact_by_state IS NULL FROM services WHERE id = $1`, svcID).Scan(&mapNull); err != nil || !mapNull {
		t.Fatalf("services.impact_by_state not NULL: %v %v", mapNull, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT impact_basis IS NULL FROM incidents WHERE id = $1`, incID).Scan(&basisNull); err != nil || !basisNull {
		t.Fatalf("incidents.impact_basis not NULL: %v %v", basisNull, err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 118); err != nil {
		t.Fatalf("down to 118: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'services' AND column_name = 'impact_by_state')
		   OR (table_name = 'projects' AND column_name = 'default_incident_impact')
		   OR (table_name = 'incidents' AND column_name = 'impact_basis')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d impact columns survived Down", n)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 119); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func TestImpactColumnsRefuseShapesNobodySpecified(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	projectID, _, serviceID := seedService(t, st, ctx)
	var incID string
	if err := st.pool.QueryRow(ctx, `INSERT INTO incidents (project_id, title, status, impact, source)
		VALUES ($1,'i','investigating','major','manual') RETURNING id::text`, projectID).Scan(&incID); err != nil {
		t.Fatalf("incident: %v", err)
	}

	try := func(q string, arg any) error {
		_, err := st.pool.Exec(ctx, q, arg)
		return err
	}
	svcUpdate := `UPDATE services SET impact_by_state = $1::jsonb WHERE id = '` + serviceID + `'`
	incUpdate := `UPDATE incidents SET impact_basis = $1::jsonb WHERE id = '` + incID + `'`
	projUpdate := `UPDATE projects SET default_incident_impact = $1 WHERE id = '` + projectID + `'`

	refuse := func(label, q, v string) {
		t.Helper()
		err := try(q, v)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Errorf("%s %s: err = %v, want SQLSTATE 23514", label, v, err)
		}
	}
	accept := func(label, q string, v any) {
		t.Helper()
		if err := try(q, v); err != nil {
			t.Errorf("%s %v refused: %v", label, v, err)
		}
	}

	for _, v := range []string{
		`{}`,
		`{"degraded":"minor","down":"major"}`,
		`{"degraded":"minor","down":"major","unknown":"none","extra":"none"}`,
		`{"degraded":"minor","down":"severe","unknown":"none"}`,
		`{"degraded":null,"down":"major","unknown":"none"}`,
		`[]`,
		`"major"`,
	} {
		refuse("services.impact_by_state", svcUpdate, v)
	}
	refuse("projects.default_incident_impact", projUpdate, "severe")

	const m = `{"degraded":"minor","down":"major","unknown":"none"}`
	const svc = `{"name":"x","slug":"y"}`
	for _, v := range []string{
		`{"source":"weird"}`,
		`{"source":"manual","state":"down"}`,
		`{"source":null}`,
		`{}`,
		`{"source":"service_mapping","mapping":` + m + `,"state":"down"}`,
		`{"source":"service_mapping","mapping":` + m + `,"state":"down","service":{"name":"x","slug":"y","id":"z"}}`,
		`{"source":"service_mapping","mapping":` + m + `,"state":"healthy","service":` + svc + `}`,
		`{"source":"service_mapping","mapping":{"degraded":"minor","down":"severe","unknown":"none"},"state":"down","service":` + svc + `}`,
		`{"source":"service_mapping","mapping":` + m + `,"state":"down","service":{"name":1,"slug":"y"}}`,
	} {
		refuse("incidents.impact_basis", incUpdate, v)
	}

	for _, v := range []string{
		m,
		`{"degraded":"none","down":"critical","unknown":"minor"}`,
	} {
		accept("services.impact_by_state", svcUpdate, v)
	}
	accept("services.impact_by_state", svcUpdate, nil)
	accept("projects.default_incident_impact", projUpdate, "critical")
	for _, v := range []string{
		`{"source":"manual"}`,
		`{"source":"project_default"}`,
		`{"source":"alertmanager"}`,
		`{"source":"service_mapping","mapping":` + m + `,"state":"unknown","service":` + svc + `}`,
	} {
		accept("incidents.impact_basis", incUpdate, v)
	}
	accept("incidents.impact_basis", incUpdate, nil)
}
