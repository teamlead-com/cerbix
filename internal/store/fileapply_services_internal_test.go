package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

const svcBundle = `format: 2
organization: acme
project: payments
monitors:
  checkout-http:
    name: Checkout HTTP
    type: http
    target: https://checkout.example.com/healthz
    interval: 30s
  checkout-db:
    name: Checkout DB
    type: tcp
    target: db.internal:5432
    interval: 60s
services:
  checkout:
    name: Checkout
    monitors: [checkout-http, checkout-db]
    sli: [checkout-http]
`

func applyServiceBundle(t *testing.T, st *Store, ctx context.Context, yaml string) ApplyResult {
	t.Helper()
	dp, err := fileprovider.Decode([]byte(yaml), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, err := st.ApplyFileManagedBundle(ctx, "payments-bundle", dp, "/bundles/payments.yaml", time.Hour, 100, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

func applyServiceBundleErr(t *testing.T, st *Store, ctx context.Context, yaml string) error {
	t.Helper()
	dp, err := fileprovider.Decode([]byte(yaml), config.ProviderScopeConfig{Type: "instance"})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = st.ApplyFileManagedBundle(ctx, "payments-bundle", dp, "/bundles/payments.yaml", time.Hour, 100, true)
	if err == nil {
		t.Fatal("apply succeeded, want a rejection")
	}
	return err
}

func seedTenant(t *testing.T, st *Store, ctx context.Context) (orgID, projID string) {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return org.ID, proj.ID
}

// A format-2 bundle creates the service AND its declaration in the same apply, with the
// monitors it names resolved from their slugs.
func TestBundleCreatesServiceWithItsDeclaration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)

	res := applyServiceBundle(t, st, ctx, svcBundle)
	if res.Services.Created != 1 {
		t.Fatalf("service counts = %+v, want one created", res.Services)
	}

	var svcID, name string
	if err := st.pool.QueryRow(ctx,
		`SELECT id, name FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&svcID, &name); err != nil {
		t.Fatalf("service row: %v", err)
	}
	if name != "Checkout" {
		t.Errorf("name = %q", name)
	}
	var revisions, epochs, sliMembers int
	if err := st.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id=$1),
		        (SELECT count(*) FROM service_evaluation_epochs   WHERE service_id=$1),
		        (SELECT count(*) FROM service_member_refs WHERE service_id=$1 AND role='sli')`,
		svcID).Scan(&revisions, &epochs, &sliMembers); err != nil {
		t.Fatalf("counts: %v", err)
	}
	if revisions != 1 || epochs != 1 {
		t.Errorf("%d revisions and %d epochs, want one of each", revisions, epochs)
	}
	if sliMembers != 1 {
		t.Errorf("%d sli refs, want 1", sliMembers)
	}
}

// Reapplying the same file is a NO-OP: an unchanged canonical hash must not create a
// definition revision, or every reconcile tick would restate what availability means.
func TestReapplyingTheSameBundleIsANoOp(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	seedTenant(t, st, ctx)

	applyServiceBundle(t, st, ctx, svcBundle)
	var before int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_definition_revisions`).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}

	res := applyServiceBundle(t, st, ctx, svcBundle)
	if res.Services.NoOp != 1 || res.Services.Updated != 0 {
		t.Errorf("second apply = %+v, want one no-op", res.Services)
	}
	var after int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_definition_revisions`).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("%d revisions after a no-op apply, want %d", after, before)
	}
}

// A change to the SLI writes a new revision and a matching epoch — and touches no monitor.
// Both halves matter: a service-only change must not bump `execution_revision`, and it must
// still create the epoch, or the new revision is a reference nothing can satisfy.
func TestServiceOnlyChangeWritesARevisionAndTouchesNoMonitor(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	type mon struct {
		id  string
		rev int64
	}
	before := map[string]int64{}
	rows, err := st.pool.Query(ctx, `SELECT id, execution_revision FROM monitors WHERE project_id=$1`, projID)
	if err != nil {
		t.Fatalf("read monitors: %v", err)
	}
	for rows.Next() {
		var m mon
		if err := rows.Scan(&m.id, &m.rev); err != nil {
			t.Fatalf("scan: %v", err)
		}
		before[m.id] = m.rev
	}
	rows.Close()

	changed := strings.Replace(svcBundle, "sli: [checkout-http]", "sli: [checkout-http, checkout-db]", 1)
	res := applyServiceBundle(t, st, ctx, changed)
	if res.Services.Updated != 1 {
		t.Fatalf("service counts = %+v, want one update", res.Services)
	}
	if res.Changed {
		t.Error("a service-only change reported an execution-config change; it must not wake the scheduler for a dispatch it does not affect")
	}

	rows, err = st.pool.Query(ctx, `SELECT id, execution_revision FROM monitors WHERE project_id=$1`, projID)
	if err != nil {
		t.Fatalf("reread monitors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m mon
		if err := rows.Scan(&m.id, &m.rev); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if m.rev != before[m.id] {
			t.Errorf("monitor %s execution_revision moved %d -> %d on a service-only change", m.id, before[m.id], m.rev)
		}
	}

	var revisions, epochs int
	if err := st.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM service_definition_revisions),
		        (SELECT count(*) FROM service_evaluation_epochs)`).Scan(&revisions, &epochs); err != nil {
		t.Fatalf("counts: %v", err)
	}
	if revisions != 2 {
		t.Errorf("%d revisions, want 2", revisions)
	}
	if epochs != revisions {
		t.Errorf("%d revisions but %d epochs: a revision with no epoch is an unsatisfiable reference", revisions, epochs)
	}
}

// Adoption never happens by name. A bundle declaring a slug a UI-owned service already holds
// is rejected — and for a service the project-unique slug means there is no second row to
// create either, which is a stronger consequence than the monitor case.
func TestBundleRefusesAUIOwnedServiceSlug(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)

	if _, err := st.CreateService(ctx, domain.Service{
		ProjectID: projID, Slug: "checkout", Name: "Checkout (created in the UI)",
	}); err != nil {
		t.Fatalf("ui service: %v", err)
	}

	err := applyServiceBundleErr(t, st, ctx, svcBundle)
	if !errors.Is(err, ErrServiceSlugOwnedByUI) {
		t.Fatalf("got %v, want ErrServiceSlugOwnedByUI", err)
	}
	// ...and the UI-owned service is untouched: a rejected bundle keeps last-known-good.
	var name string
	if err := st.pool.QueryRow(ctx,
		`SELECT name FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Checkout (created in the UI)" {
		t.Errorf("the UI-owned service was modified: name = %q", name)
	}
}

// A declaration that names a monitor slug which does not resolve could never be evaluated,
// so it is refused rather than stored as a dangling reference.
func TestBundleRefusesAnUnknownMonitorSlug(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	seedTenant(t, st, ctx)

	y := strings.Replace(svcBundle, "monitors: [checkout-http, checkout-db]", "monitors: [checkout-http, checkout-db, ghost]", 1)
	err := applyServiceBundleErr(t, st, ctx, y)
	if !errors.Is(err, ErrServiceMemberUnknown) {
		t.Fatalf("got %v, want ErrServiceMemberUnknown", err)
	}
}

// Absence from a bundle ORPHANS an owned service; it never deletes one. A service carries
// facts, incidents and possibly a public projection, and removing that because a file was
// edited is not a reconcile.
func TestAbsenceOrphansRatherThanDeletes(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	withoutServices := strings.Split(svcBundle, "services:")[0] + "services: {}\n"
	res := applyServiceBundle(t, st, ctx, withoutServices)
	if res.Services.Orphaned != 1 {
		t.Fatalf("service counts = %+v, want one orphaned", res.Services)
	}
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatal("an absent service was deleted rather than orphaned")
	}
	var orphanedAt *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT orphaned_at FROM managed_services WHERE source_uid='checkout'`).Scan(&orphanedAt); err != nil {
		t.Fatalf("ownership row: %v", err)
	}
	if orphanedAt == nil {
		t.Error("the service was not marked orphaned")
	}

	// Bringing it back restores rather than duplicating.
	res = applyServiceBundle(t, st, ctx, svcBundle)
	if res.Services.Restored != 1 {
		t.Errorf("service counts = %+v, want one restored", res.Services)
	}
}

// A format-1 bundle leaves services alone entirely. That format cannot express services, so
// its silence about them is not a statement of intent — and treating it as one would mean
// downgrading a file's `format:` line silently orphans every service it used to declare.
func TestFormat1BundleDoesNotOrphanServices(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	v1 := strings.Replace(strings.Split(svcBundle, "services:")[0], "format: 2", "format: 1", 1)
	res := applyServiceBundle(t, st, ctx, v1)
	if res.Services.Orphaned != 0 {
		t.Fatalf("a format-1 bundle orphaned %d services; its silence is not intent", res.Services.Orphaned)
	}
	var orphanedAt *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT orphaned_at FROM managed_services WHERE source_uid='checkout'`).Scan(&orphanedAt); err != nil {
		t.Fatalf("ownership row: %v", err)
	}
	if orphanedAt != nil {
		t.Error("the service was marked orphaned by a bundle that cannot express services")
	}
}
