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

// A file-owned service is READ-ONLY through the UI path. Letting the declaration through
// would be worse than a 409: the very next reconcile restates it from the file, so the
// operator's edit to what availability MEANS would vanish with nothing to show for it.
func TestUIDeclarationOnAFileOwnedServiceIsRefused(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	var svcID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&svcID); err != nil {
		t.Fatalf("service row: %v", err)
	}
	var monID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM monitors WHERE project_id=$1 AND slug='checkout-http'`, projID).Scan(&monID); err != nil {
		t.Fatalf("monitor row: %v", err)
	}

	_, _, err := st.PutServiceDeclaration(ctx, projID, svcID, domain.ServiceDeclaration{
		Monitors: []string{monID}, SLI: []string{monID},
	}, 1, DeclarationOptions{CreatedBy: "operator"})
	if !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("got %v, want ErrServiceManagedByFile", err)
	}

	// ...and the file's own declaration is the one still in force.
	var revisions int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_definition_revisions WHERE service_id=$1`, svcID).Scan(&revisions); err != nil {
		t.Fatalf("count: %v", err)
	}
	if revisions != 1 {
		t.Errorf("%d revisions, want the file's one", revisions)
	}
}

// Deleting a file-owned service through the UI is refused for the same reason, and the
// refusal survives ORPHANING: absence from a bundle never releases ownership, so an orphaned
// service is still the file's to delete.
func TestDeletingAFileOwnedServiceIsRefusedEvenWhenOrphaned(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	var svcID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&svcID); err != nil {
		t.Fatalf("service row: %v", err)
	}
	if err := st.DeleteService(ctx, projID, svcID); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("got %v, want ErrServiceManagedByFile", err)
	}

	withoutServices := strings.Split(svcBundle, "services:")[0] + "services: {}\n"
	applyServiceBundle(t, st, ctx, withoutServices)
	if err := st.DeleteService(ctx, projID, svcID); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("orphaned: got %v, want ErrServiceManagedByFile — orphaning is not a release", err)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM services WHERE id=$1`, svcID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Error("the refused delete removed the service anyway")
	}
}

// A UI-owned service is deletable, so the guard above is about OWNERSHIP and not about
// services in general.
func TestDeletingAUIOwnedServiceSucceeds(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "cart", Name: "Cart"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteService(ctx, projID, svc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteService(ctx, projID, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

// A slug collision is a conflict the caller can act on, not an internal error.
func TestDuplicateServiceSlugIsAConflict(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	if _, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "cart", Name: "Cart"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "cart", Name: "Cart again"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}

// The slug is the URL segment and the bundle's reference key. The store refuses a malformed
// one even though both of today's callers check first — it is the choke point every future
// caller has to come through.
func TestStoreRefusesAMalformedServiceSlug(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	for _, slug := range []string{"", "Checkout", "check out", "check/out", "1checkout", strings.Repeat("a", 64)} {
		if _, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: slug, Name: "n"}); err == nil {
			t.Errorf("slug %q was accepted", slug)
		}
	}
	if _, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "check-out-2", Name: "n"}); err != nil {
		t.Errorf("a well-formed slug was refused: %v", err)
	}
}

// The list query answers in ONE round trip and reports the two counts separately, the file
// provenance, and the watermark. The obvious alternative — list, then fetch each detail — is
// an N+1 on the first screen of the feature.
func TestListServiceSummariesIsOneQueryAndKeepsBothCounts(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)
	if _, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "aaa-undeclared", Name: "Undeclared"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rows, err := st.ListServiceSummaries(ctx, projID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].Service.Slug != "aaa-undeclared" {
		t.Fatalf("rows are not slug-ordered: %q first", rows[0].Service.Slug)
	}

	// An undeclared service is a row with revision 0 — a state, not a gap, and certainly not
	// an omission from the list.
	if rows[0].Revision != 0 || rows[0].ContextMembers != 0 || rows[0].SLIMembers != 0 {
		t.Errorf("undeclared row = %+v, want revision 0 and no members", rows[0])
	}
	if rows[0].ManagedBy != "" {
		t.Errorf("a UI-created service reported provider %q", rows[0].ManagedBy)
	}
	if rows[0].EffectiveAt != nil {
		t.Errorf("undeclared row has effective_at %v", rows[0].EffectiveAt)
	}

	// The bundle declares two monitors of which one is an SLI. Both counts must survive, or
	// the list hides the distinction the declaration model rests on.
	d := rows[1]
	if d.Service.Slug != "checkout" {
		t.Fatalf("second row = %q", d.Service.Slug)
	}
	if d.ContextMembers != 2 || d.SLIMembers != 1 {
		t.Errorf("counts = %d context / %d sli, want 2 / 1", d.ContextMembers, d.SLIMembers)
	}
	if d.Revision != 1 || d.EpochSeq != 1 {
		t.Errorf("revision=%d epoch=%d, want 1/1", d.Revision, d.EpochSeq)
	}
	if d.ManagedBy != "payments-bundle" {
		t.Errorf("managed_by = %q, want the owning provider", d.ManagedBy)
	}
	if d.SealedThrough != nil {
		t.Errorf("sealed_through = %v before anything was materialized", d.SealedThrough)
	}
	if d.EffectiveAt == nil {
		t.Error("a declared service has no effective_at")
	}

	// A second revision must not ACCUMULATE members — the row reads the revision in force,
	// not every member row the service has ever had.
	changed := strings.Replace(svcBundle, "sli: [checkout-http]", "sli: [checkout-http, checkout-db]", 1)
	applyServiceBundle(t, st, ctx, changed)
	rows2, err := st.ListServiceSummaries(ctx, projID)
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	if rows2[1].ContextMembers != 2 || rows2[1].SLIMembers != 2 {
		t.Errorf("counts = %d / %d after the second revision, want 2 / 2 (not the sum of both revisions)",
			rows2[1].ContextMembers, rows2[1].SLIMembers)
	}
	if rows2[1].Revision != 2 {
		t.Errorf("revision = %d, want 2", rows2[1].Revision)
	}
}

// Routing decides who gets paged. The FKs referenced the routing tables by ID ALONE, so the
// database had no opinion about tenancy and the create path passed whatever id it was handed:
// an editor in one project could attach another project's escalation policy, and operational
// response would then point across a tenant boundary.
func TestAServiceCannotBorrowAnotherProjectsOwner(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, projID := seedTenant(t, st, ctx)
	other, err := st.CreateProject(ctx, org, "other", "Other")
	if err != nil {
		t.Fatalf("second project: %v", err)
	}
	// The row is seeded directly: this test is about FK tenancy, and a valid ladder of
	// steps and targets would be fixture noise around the thing under test.
	foreignID := seedPolicy(t, st, ctx, other.ID, "их политика")

	_, err = st.CreateService(ctx, domain.Service{
		ProjectID: projID, Slug: "borrowed", Name: "Borrowed",
		EscalationPolicyID: foreignID,
	})
	if !errors.Is(err, ErrOwnerNotInProject) {
		t.Fatalf("got %v, want ErrOwnerNotInProject", err)
	}

	// …and the same policy inside the right project is accepted, so this is a tenancy check
	// and not a ban on owners.
	mineID := seedPolicy(t, st, ctx, projID, "наша политика")
	if _, err := st.CreateService(ctx, domain.Service{
		ProjectID: projID, Slug: "owned", Name: "Owned", EscalationPolicyID: mineID,
	}); err != nil {
		t.Fatalf("a same-project owner was refused: %v", err)
	}
}

// seedPolicy inserts a bare escalation-policy row for tenancy fixtures.
func seedPolicy(t *testing.T, st *Store, ctx context.Context, projectID, name string) string {
	t.Helper()
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO escalation_policies (project_id, name, steps) VALUES ($1,$2,'[]'::jsonb) RETURNING id::text`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	return id
}

// The bundle parses the owner, validates it and folds it into the canonical hash — and the
// apply persisted only name and description. So a declaration of who is responsible applied
// "successfully", changed nothing, and its hash asserted it was in force.
func TestBundleOwnerIsActuallyPersisted(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	policyID := seedPolicy(t, st, ctx, projID, "payments-oncall")

	owned := strings.Replace(svcBundle, "  checkout:\n    name: Checkout",
		"  checkout:\n    name: Checkout\n    owner:\n      escalation_policy: payments-oncall", 1)
	applyServiceBundle(t, st, ctx, owned)

	var got *string
	if err := st.pool.QueryRow(ctx,
		`SELECT escalation_policy_id::text FROM services WHERE project_id=$1 AND slug='checkout'`,
		projID).Scan(&got); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if got == nil || *got != policyID {
		t.Fatalf("escalation_policy_id = %v, want the declared %s", got, policyID)
	}
}

// A name the project does not have is REFUSED rather than nulled: silently having no owner is
// exactly the outcome the declaration was written to prevent.
func TestBundleRefusesAnUnknownOwner(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	seedTenant(t, st, ctx)
	ghost := strings.Replace(svcBundle, "  checkout:\n    name: Checkout",
		"  checkout:\n    name: Checkout\n    owner:\n      escalation_policy: nobody", 1)
	if err := applyServiceBundleErr(t, st, ctx, ghost); !errors.Is(err, ErrServiceOwnerUnknown) {
		t.Fatalf("got %v, want ErrServiceOwnerUnknown", err)
	}
}

// §15.1, the UI→file cell: a UI delete may not rewrite a file-owned declaration.
//
// iter-0125's implementation checked no ownership and authored a system revision for EVERY
// referencing service — a UI action forcing a change to a resource the UI does not own. Its
// test built only a UI-owned service, so the matrix was never exercised.
func TestDeletingAMonitorDeclaredByAFileOwnedServiceIsRefused(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	// A UI-owned monitor, declared into the FILE-owned service by a UI declaration.
	ui, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "ui-extra", Type: domain.MonitorTCP,
		Target: "localhost:1", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("ui monitor: %v", err)
	}
	var svcID, httpID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id=$1 AND slug='checkout'`, projID).Scan(&svcID); err != nil {
		t.Fatalf("service: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM monitors WHERE project_id=$1 AND slug='checkout-http'`, projID).Scan(&httpID); err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_member_refs (service_id, project_id, monitor_id, role) VALUES ($1,$2,$3,'context')`,
		svcID, projID, ui.ID); err != nil {
		t.Fatalf("reference the ui monitor from the file-owned service: %v", err)
	}

	if err := st.DeleteMonitor(ctx, ui.ID); !errors.Is(err, ErrServiceManagedByFile) {
		t.Fatalf("got %v, want ErrServiceManagedByFile — a UI delete rewrote a file-owned declaration", err)
	}
	// …and the monitor survives: the refusal is not a partial delete.
	if _, err := st.GetMonitor(ctx, ui.ID); err != nil {
		t.Errorf("the monitor was removed by a refused delete: %v", err)
	}
}

// §10.9: an elapsed or archived window is retained for at least the fact horizon, and ONLY an
// annul removes its span. Deleting a monitor used to be rejected outright, which hid that
// `maintenance_windows.monitor_id` cascaded — making the delete SUCCEED is what exposed it.
func TestDeletingAMonitorKeepsItsMaintenanceProvenance(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "doomed", Type: domain.MonitorTCP,
		Target: "localhost:1", IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	past := time.Now().UTC().Add(-2 * time.Hour)
	w, err := st.createMaintenanceWindowUnchecked(ctx, domain.MaintenanceWindow{
		ProjectID: projID, MonitorID: mon.ID,
		StartsAt: past, EndsAt: past.Add(time.Hour), Reason: "elapsed",
	})
	if err != nil {
		t.Fatalf("window: %v", err)
	}

	if err := st.DeleteMonitor(ctx, mon.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var kept int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM maintenance_windows WHERE id=$1`, w.ID).Scan(&kept); err != nil {
		t.Fatalf("count: %v", err)
	}
	if kept != 1 {
		t.Fatal("the window vanished with the monitor; the next recompute would silently rewrite sealed history without the exclusion")
	}
	// It keeps its SCOPED identity: a window that lost its monitor is not a project-wide one.
	var scoped *string
	if err := st.pool.QueryRow(ctx, `SELECT monitor_id::text FROM maintenance_windows WHERE id=$1`, w.ID).Scan(&scoped); err != nil {
		t.Fatalf("read scope: %v", err)
	}
	if scoped == nil || *scoped != mon.ID {
		t.Errorf("monitor_id = %v, want the retained %s — NULL already means the whole project", scoped, mon.ID)
	}
}

// The composite owner FK must clear the OWNER on delete, not the tenant key. iter-0125 wrote
// a bare ON DELETE SET NULL, which Postgres applies to every referencing column — including
// the NOT NULL project_id — so a referenced policy could not be deleted at all.
func TestDeletingAReferencedOwnerClearsTheReferenceAndNotTheTenant(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)

	for _, tc := range []struct {
		what   string
		seed   func() string
		assign func(string) domain.Service
		del    string
		column string
	}{
		{
			what: "escalation policy",
			seed: func() string { return seedPolicy(t, st, ctx, projID, "pol") },
			assign: func(id string) domain.Service {
				return domain.Service{ProjectID: projID, Slug: "svc-esc", Name: "E", EscalationPolicyID: id}
			},
			del: "escalation_policies", column: "escalation_policy_id",
		},
		{
			what: "on-call schedule",
			seed: func() string { return seedSchedule(t, st, ctx, projID, "sched") },
			assign: func(id string) domain.Service {
				return domain.Service{ProjectID: projID, Slug: "svc-oncall", Name: "O", OncallScheduleID: id}
			},
			del: "oncall_schedules", column: "oncall_schedule_id",
		},
	} {
		ownerID := tc.seed()
		svc, err := st.CreateService(ctx, tc.assign(ownerID))
		if err != nil {
			t.Fatalf("%s: create service: %v", tc.what, err)
		}
		if _, err := st.pool.Exec(ctx, `DELETE FROM `+tc.del+` WHERE id=$1`, ownerID); err != nil {
			t.Fatalf("%s: deleting a referenced owner failed: %v", tc.what, err)
		}
		var project string
		var owner *string
		if err := st.pool.QueryRow(ctx,
			`SELECT project_id::text, `+tc.column+`::text FROM services WHERE id=$1`, svc.ID).Scan(&project, &owner); err != nil {
			t.Fatalf("%s: read back: %v", tc.what, err)
		}
		if owner != nil {
			t.Errorf("%s: the reference survived the delete", tc.what)
		}
		if project != projID {
			t.Errorf("%s: project_id became %q — the tenant key was cleared instead of the owner", tc.what, project)
		}
	}
}

func seedSchedule(t *testing.T, st *Store, ctx context.Context, projectID, name string) string {
	t.Helper()
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at)
		 VALUES ($1,$2,86400,now()) RETURNING id::text`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	return id
}

// The per-project cap has ONE owner, used by both writers. iter-0125 checked it on the UI path
// only, so a bundle created services without bound while the AC claimed otherwise.
func TestTheProjectServiceCapBindsTheBundleToo(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	st.WithServiceLimits(ServiceLimits{ServicesPerProject: 1})

	applyServiceBundle(t, st, ctx, svcBundle) // one service — at the cap

	two := strings.Replace(svcBundle, "services:\n", "services:\n  cart:\n    name: Cart\n    monitors: [checkout-db]\n    sli: [checkout-db]\n", 1)
	if err := applyServiceBundleErr(t, st, ctx, two); !errors.Is(err, ErrTooManyServices) {
		t.Fatalf("got %v, want ErrTooManyServices — a bundle crossed the per-project cap", err)
	}
	if _, err := st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "ui-one", Name: "UI"}); !errors.Is(err, ErrTooManyServices) {
		t.Errorf("the UI path disagrees with the bundle about the same cap: %v", err)
	}
}

// The store's cap is DEFENSE, not policy: operator input is validated fail-fast in
// internal/config (see config.TestServiceCapsAreRejectedNotReinterpreted), so what reaches
// here is legal. The store still refuses to run past the hard maxima and fills zeros for
// programmatic callers that set nothing.
func TestStoreCapIsDefenseNotReinterpretation(t *testing.T) {
	got := capServiceLimits(ServiceLimits{ServicesPerProject: 9999, MembersPerRevision: 9999, ServicesPerMonitor: 9999})
	if got.ServicesPerProject != HardMaxServicesPerProject ||
		got.MembersPerRevision != HardMaxMembersPerRevision ||
		got.ServicesPerMonitor != HardMaxServicesPerMonitor {
		t.Fatalf("capped to %+v, want the hard maxima", got)
	}
	def := capServiceLimits(ServiceLimits{})
	if def.ServicesPerProject != DefaultMaxServicesPerProject ||
		def.MembersPerRevision != DefaultMaxMembersPerRevision ||
		def.ServicesPerMonitor != DefaultMaxServicesPerMonitor {
		t.Fatalf("defaults resolved to %+v", def)
	}
}

// FR-021 §16.6a — a paging edit from a FILE changes who is paged, and nothing else.
//
// "None of these fields bumps a definition revision or an evaluation epoch" is the whole reason the
// declaration hash excludes them: the update branch of the apply creates a revision AND its epoch
// unconditionally, so an `owns_paging` toggle riding that hash would re-segment reliability history
// for an alerting edit. The apply reconciles paging on every branch against the row instead.
func TestFileAlertingEditWritesNoRevisionAndNoEpoch(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	applyServiceBundle(t, st, ctx, svcBundle)

	var serviceID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id = $1`, projID).Scan(&serviceID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	countRevisions := func() (revisions, epochs int64, generation int64) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id = $1),
			       (SELECT count(*) FROM service_evaluation_epochs WHERE service_id = $1),
			       (SELECT alert_config_generation FROM services WHERE id = $1)`,
			serviceID).Scan(&revisions, &epochs, &generation); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return
	}
	revBefore, epochBefore, genBefore := countRevisions()

	// The SAME declaration, now also declaring paging.
	withAlerting := svcBundle + `    alerting:
      owns_paging: true
      page_on: [down, degraded]
      page_on_unknown: true
      confirm_evaluations: 4
`
	applyServiceBundle(t, st, ctx, withAlerting)

	revAfter, epochAfter, genAfter := countRevisions()
	if revAfter != revBefore {
		t.Fatalf("a paging edit created %d definition revision(s): it re-segments reliability "+
			"history for a change to who gets woken", revAfter-revBefore)
	}
	if epochAfter != epochBefore {
		t.Fatalf("a paging edit created %d evaluation epoch(s)", epochAfter-epochBefore)
	}
	// It DOES dis-arm, which is the safe direction and the trigger's job.
	if genAfter <= genBefore {
		t.Fatalf("alert_config_generation stayed at %d: the edit did not dis-arm delegation", genAfter)
	}

	var policy domain.ServiceAlertPolicy
	var pageOn []string
	if err := st.pool.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations
		  FROM services WHERE id = $1`, serviceID).
		Scan(&policy.OwnsPaging, &pageOn, &policy.PageOnUnknown, &policy.ConfirmEvaluations); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if !policy.OwnsPaging || !policy.PageOnUnknown || policy.ConfirmEvaluations != 4 ||
		len(pageOn) != 2 {
		t.Fatalf("the declaration did not reach the row: %+v page_on=%v", policy, pageOn)
	}

	// And re-applying the identical bundle changes nothing at all — the reconcile is a read when
	// the row already says what the file says, so a file provider does not fight the evaluators
	// for row locks on every scan.
	res := applyServiceBundle(t, st, ctx, withAlerting)
	if res.Services.Updated != 0 {
		t.Fatalf("re-applying an unchanged bundle reported %d service updates", res.Services.Updated)
	}
	if _, _, gen := countRevisions(); gen != genAfter {
		t.Fatalf("a re-apply bumped alert_config_generation %d→%d, dis-arming for no change",
			genAfter, gen)
	}
}

// §15.2 — the file IS the desired state, so the same file must converge to the same database
// whatever it used to say.
//
// The case that makes it concrete: a bundle declares `owns_paging: true`, and the block is later
// deleted from the file. If absence meant "leave it", the service would keep owning paging forever
// — and being file-managed, the UI could not correct it, because those edits are refused with a 409.
// A fresh installation applying the same final file would meanwhile get the default. One file, two
// databases.
func TestRemovingTheAlertingBlockRestoresTheDeclaredDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)

	owning := svcBundle + `    alerting:
      owns_paging: true
      page_on: [down, degraded]
      confirm_evaluations: 5
`
	applyServiceBundle(t, st, ctx, owning)

	var serviceID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id = $1`, projID).Scan(&serviceID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	policy := func() domain.ServiceAlertPolicy {
		t.Helper()
		p, err := st.ServiceAlertPolicy(ctx, projID, serviceID)
		if err != nil {
			t.Fatalf("read policy: %v", err)
		}
		return p
	}
	if got := policy(); !got.OwnsPaging || got.ConfirmEvaluations != 5 {
		t.Fatalf("the declaration did not apply: %+v", got)
	}

	// The block is deleted from the file.
	applyServiceBundle(t, st, ctx, svcBundle)

	got := policy()
	want := domain.DefaultServiceAlertPolicy().Canonical()
	if got.OwnsPaging {
		t.Fatal("the service still owns paging after the declaration was deleted from the file: " +
			"the same file now means two different things depending on history, and a file-managed " +
			"service cannot be corrected through the UI")
	}
	if got.ConfirmEvaluations != want.ConfirmEvaluations || len(got.PageOn) != len(want.PageOn) {
		t.Fatalf("removing the block converged to %+v, want the default %+v", got, want)
	}
}
