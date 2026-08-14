package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

func postgresRefMonitor(projectID, name, ref string) domain.Monitor {
	return domain.Monitor{
		ProjectID: projectID, Name: name, Type: domain.MonitorPostgres,
		Target: "db.internal:5432", IntervalSeconds: 60, TimeoutSeconds: 5,
		Enabled: true,
		Config: map[string]string{
			"username": "cerbix", "database": "app", "password_ref": ref,
		},
	}
}

func TestMonitorSecretRefsAtomicAndTenantScoped(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectA := secretsFixture(t, st, ctx, "ref-org-a", "app")
	_, projectB := secretsFixture(t, st, ctx, "ref-org-b", "app")
	first, err := st.CreateProjectSecret(ctx, testSecretActor, projectA, "db-password", "first-value")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateProjectSecret(ctx, testSecretActor, projectA, "db-password-next", "second-value")
	if err != nil {
		t.Fatal(err)
	}
	// A same-named secret in another tenant must never satisfy project A's lookup.
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectB, "foreign-only", "foreign-value"); err != nil {
		t.Fatal(err)
	}

	mon, err := st.CreateMonitor(ctx, postgresRefMonitor(projectA, "db", first.Name))
	if err != nil {
		t.Fatalf("create referenced monitor: %v", err)
	}
	var gotSecret, gotProject, gotSetting string
	if err := st.pool.QueryRow(ctx,
		`SELECT secret_id::text, project_id::text, setting_key
		   FROM monitor_secret_refs WHERE monitor_id = $1`, mon.ID).
		Scan(&gotSecret, &gotProject, &gotSetting); err != nil {
		t.Fatalf("read normalized ref: %v", err)
	}
	if gotSecret != first.ID || gotProject != projectA || gotSetting != "password_ref" {
		t.Fatalf("ref = secret=%s project=%s setting=%s", gotSecret, gotProject, gotSetting)
	}

	missing := postgresRefMonitor(projectA, "missing", "foreign-only")
	if _, err := st.CreateMonitor(ctx, missing); err == nil {
		t.Fatal("cross-project same-name lookup must fail")
	} else {
		var notFound SecretRefNotFoundError
		if !errors.As(err, &notFound) || notFound.Name != "foreign-only" {
			t.Fatalf("want typed tenant-scoped missing ref, got %v", err)
		}
	}
	var leaked int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE project_id=$1 AND name='missing'`, projectA).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("failed create must leave no monitor, count=%d err=%v", leaked, err)
	}

	mon.Config["password_ref"] = second.Name
	updated, err := st.UpdateMonitor(ctx, mon)
	if err != nil {
		t.Fatalf("repoint monitor: %v", err)
	}
	if updated.ExecutionRevision != mon.ExecutionRevision+1 {
		t.Fatalf("revision = %d, want %d", updated.ExecutionRevision, mon.ExecutionRevision+1)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT secret_id::text FROM monitor_secret_refs WHERE monitor_id=$1 AND setting_key='password_ref'`, mon.ID).
		Scan(&gotSecret); err != nil || gotSecret != second.ID {
		t.Fatalf("updated ref = %s err=%v, want %s", gotSecret, err, second.ID)
	}
}

func TestFileApplySecretRefMissingPreservesLKG(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projectID := secretsFixture(t, st, ctx, "mac-ref-org", "payments")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-value"); err != nil {
		t.Fatal(err)
	}
	scope := config.ProviderScopeConfig{Type: config.ProviderScopeInstance}
	good := []byte(`
format: 1
organization: mac-ref-org
project: payments
monitors:
  db:
    name: Primary DB
    type: postgres
    target: db.internal:5432
    interval: 30s
    timeout: 5s
    settings:
      username: cerbix
      database: app
      password_ref: db-password
`)
	desired, err := fileprovider.Decode(good, scope)
	if err != nil {
		t.Fatalf("decode good bundle: %v", err)
	}
	if _, err := st.ApplyFileManagedBundle(ctx, "secret-provider", desired, "db.yaml", time.Minute, 100, true); err != nil {
		t.Fatalf("apply good bundle: %v", err)
	}
	id, rev, _, _, _ := monRow(t, st, ctx, "db")
	var refs int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM monitor_secret_refs WHERE monitor_id=$1`, id).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("file apply refs=%d err=%v", refs, err)
	}

	missing := []byte(strings.ReplaceAll(string(good), "password_ref: db-password", "password_ref: missing-password"))
	desiredMissing, err := fileprovider.Decode(missing, scope)
	if err != nil {
		t.Fatalf("decode missing-ref bundle is structurally valid: %v", err)
	}
	_, err = st.ApplyFileManagedBundle(ctx, "secret-provider", desiredMissing, "db.yaml", time.Minute, 100, true)
	var be *fileprovider.BundleError
	if !errors.As(err, &be) || be.Reason != fileprovider.ReasonSecretRefNotFound || be.Org != "mac-ref-org" || be.Project != "payments" {
		t.Fatalf("want bindable secret_ref_not_found, got %v", err)
	}
	_, afterRev, _, _, _ := monRow(t, st, ctx, "db")
	if afterRev != rev {
		t.Fatalf("failed apply mutated LKG revision %d -> %d", rev, afterRev)
	}
}

func TestFileApplySecretRefFeatureOffIsBindableAndAtomic(t *testing.T) {
	st, ctx := secretsTestStore(t)
	st.WithSecretsEnabled(false)
	_, projectID := secretsFixture(t, st, ctx, "ref-off-org", "ref-off-project")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projectID, "db-password", "inventory-value"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	desired, err := fileprovider.Decode([]byte(`
format: 1
organization: ref-off-org
project: ref-off-project
monitors:
  database:
    name: Database
    type: postgres
    target: db.internal:5432
    interval: 1m
    timeout: 5s
    settings:
      username: cerbix
      database: app
      password_ref: db-password
`), config.ProviderScopeConfig{Type: config.ProviderScopeInstance})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = st.ApplyFileManagedBundle(ctx, "secret-provider", desired, "db.yaml", time.Minute, 100, true)
	var bundleErr *fileprovider.BundleError
	if !errors.As(err, &bundleErr) || bundleErr.Reason != fileprovider.ReasonFeatureDisabled || bundleErr.Org != "ref-off-org" || bundleErr.Project != "ref-off-project" {
		t.Fatalf("apply error = %v, want bindable feature_disabled", err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM monitors WHERE project_id=$1`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("monitor count = %d, want zero after rejected atomic apply", count)
	}
}
