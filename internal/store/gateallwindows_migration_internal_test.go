package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestGateAllWindowsMigrationPreservesV1PolicyRows(t *testing.T) {
	admin, ctx := gateStore(t)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	databaseName := fmt.Sprintf("cerbix_test_gate_v2_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+databaseName); err != nil {
		t.Fatalf("create migration probe: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+databaseName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop migration probe: %v", err)
		}
	})
	parsed.Path = "/" + databaseName
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open migration probe: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 108); err != nil {
		t.Fatalf("migrate probe to 108: %v", err)
	}
	var serviceID, projectID string
	if err := db.QueryRowContext(ctx, `
		WITH org AS (
		    INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id
		), project AS (
		    INSERT INTO projects (org_id, slug, name) SELECT id, 'api', 'API' FROM org RETURNING id
		), service AS (
		    INSERT INTO services (project_id, slug, name) SELECT id, 'checkout', 'Checkout' FROM project
		    RETURNING id, project_id
		), service_policy AS (
		    INSERT INTO service_gate_policies
		      (service_id, project_id, window_name, schema_version, clauses,
		       budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision, updated_by)
		    SELECT id, project_id, '24h', 1,
		           '{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"}'::jsonb,
		           90, 900, 'warn', 7, 'token:ci'
		      FROM service RETURNING service_id, project_id
		)
		INSERT INTO project_gate_policies
		  (project_id, window_name, schema_version, clauses, budget_consumed_percent,
		   max_seal_lag_seconds, unknown_behavior, revision, updated_at, updated_by)
		SELECT project_id, '24h', 1,
		       '{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"}'::jsonb,
		       90, 900, 'warn', 11, statement_timestamp(), 'token:ci'
		  FROM service_policy
		RETURNING (SELECT service_id::text FROM service_policy), project_id::text`).Scan(&serviceID, &projectID); err != nil {
		t.Fatalf("seed v1 policies: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 109); err != nil {
		t.Fatalf("migrate probe to 109: %v", err)
	}
	for name, query := range map[string]string{
		"service": `SELECT revision, schema_version, window_name, window_mode FROM service_gate_policies WHERE service_id = $1`,
		"project": `SELECT revision, schema_version, window_name, window_mode FROM project_gate_policies WHERE project_id = $1`,
	} {
		id, wantRevision := serviceID, int64(7)
		if name == "project" {
			id, wantRevision = projectID, 11
		}
		var revision int64
		var schemaVersion int
		var window, mode string
		if err := db.QueryRowContext(ctx, query, id).Scan(&revision, &schemaVersion, &window, &mode); err != nil {
			t.Fatalf("read migrated %s policy: %v", name, err)
		}
		if revision != wantRevision || schemaVersion != domain.GatePolicySchemaV1 || window != "24h" || mode != string(domain.GateWindowModeOne) {
			t.Errorf("migrated %s policy = revision %d schema %d window %q mode %q", name, revision, schemaVersion, window, mode)
		}
	}
}

func TestProjectGatePolicyMigrationBackfillsLegacyDecisionSource(t *testing.T) {
	admin, ctx := gateStore(t)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	databaseName := fmt.Sprintf("cerbix_test_gate_source_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+databaseName); err != nil {
		t.Fatalf("create migration probe: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+databaseName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop migration probe: %v", err)
		}
	})
	parsed.Path = "/" + databaseName
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open migration probe: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 107); err != nil {
		t.Fatalf("migrate probe to 107: %v", err)
	}

	var projectID, liveServiceID, deletedServiceID string
	if err := db.QueryRowContext(ctx, `
		WITH org AS (
		    INSERT INTO organizations (slug, name) VALUES ('source-acme', 'Source Acme') RETURNING id
		), project AS (
		    INSERT INTO projects (org_id, slug, name) SELECT id, 'source-api', 'Source API' FROM org RETURNING id
		), live_service AS (
		    INSERT INTO services (project_id, slug, name) SELECT id, 'live', 'Live' FROM project RETURNING id, project_id
		), deleted_service AS (
		    INSERT INTO services (project_id, slug, name) SELECT id, 'deleted', 'Deleted' FROM project RETURNING id
		)
		SELECT live_service.project_id::text, live_service.id::text, deleted_service.id::text
		  FROM live_service, deleted_service`).Scan(&projectID, &liveServiceID, &deletedServiceID); err != nil {
		t.Fatalf("seed migration services: %v", err)
	}

	insertLegacyDecision := func(serviceID, slug string, at time.Time) string {
		t.Helper()
		id, err := newGateDecisionID(at)
		if err != nil {
			t.Fatalf("decision id: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO service_gate_decisions
			  (id, project_id, service_id, service_slug, service_name, state, action, reasons, evidence,
			   policy_revision, window_name, policy_snapshot, evaluated_at)
			VALUES ($1, $2, $3, $4, $4, 'ALLOW', 'ALLOW', '[]', '{}', 1, '30d',
			        '{"schema_version":1,"window":"30d"}', $5)`,
			id, projectID, serviceID, slug, at); err != nil {
			t.Fatalf("insert legacy decision %s: %v", slug, err)
		}
		return id
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	liveDecisionID := insertLegacyDecision(liveServiceID, "live", at)
	deletedDecisionID := insertLegacyDecision(deletedServiceID, "deleted", at.Add(time.Millisecond))
	if _, err := db.ExecContext(ctx, `DELETE FROM services WHERE id = $1`, deletedServiceID); err != nil {
		t.Fatalf("delete legacy service: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 108); err != nil {
		t.Fatalf("migrate probe to 108: %v", err)
	}
	for _, tc := range []struct {
		name, decisionID string
		wantService      *string
		wantOwner        *string
	}{
		{name: "live service", decisionID: liveDecisionID, wantService: &liveServiceID, wantOwner: &liveServiceID},
		{name: "deleted service", decisionID: deletedDecisionID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var serviceID, ownerID sql.NullString
			var source string
			if err := db.QueryRowContext(ctx, `
				SELECT service_id::text, policy_source, policy_owner_id::text
				  FROM service_gate_decisions WHERE id = $1`, tc.decisionID).Scan(&serviceID, &source, &ownerID); err != nil {
				t.Fatalf("read migrated decision: %v", err)
			}
			if source != string(domain.GatePolicySourceService) {
				t.Errorf("policy source = %q, want service", source)
			}
			if tc.wantService == nil && serviceID.Valid {
				t.Errorf("service id = %q, want null after deletion", serviceID.String)
			}
			if tc.wantService != nil && (!serviceID.Valid || serviceID.String != *tc.wantService) {
				t.Errorf("service id = %v, want %q", serviceID, *tc.wantService)
			}
			if tc.wantOwner == nil && ownerID.Valid {
				t.Errorf("owner id = %q, want absent because the deleted UUID is unrecoverable", ownerID.String)
			}
			if tc.wantOwner != nil && (!ownerID.Valid || ownerID.String != *tc.wantOwner) {
				t.Errorf("owner id = %v, want %q", ownerID, *tc.wantOwner)
			}
		})
	}
}
