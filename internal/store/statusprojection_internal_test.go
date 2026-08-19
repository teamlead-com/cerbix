package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type projFixture struct {
	orgID     string
	projectID string
	otherProj string
	pageOrg   string // org-level page (project_id NULL)
	pageProj  string // project-scoped page
	monitorID string
	serviceID string
}

func seedProjection(t *testing.T, st *Store, ctx context.Context) projFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	other, err := st.CreateProject(ctx, org.ID, "search", "Search")
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	f := projFixture{orgID: org.ID, projectID: proj.ID, otherProj: other.ID}

	orgPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, Slug: "acme-status", Title: "Acme", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("org page: %v", err)
	}
	f.pageOrg = orgPage.ID
	projPage, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org.ID, ProjectID: proj.ID, Slug: "payments-status", Title: "Payments",
		Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("project page: %v", err)
	}
	f.pageProj = projPage.ID

	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "payments-http", Type: domain.MonitorHTTP,
		Target: "https://payments.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	f.monitorID = mon.ID
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: proj.ID, Slug: "checkout", Name: "Checkout"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	f.serviceID = svc.ID
	return f
}

// The source is a DISCRIMINATOR and the backfill is total (invariants 62–63): every shipped row
// gets a source, a monitor binding implies 'monitor', and everything else — including a row with
// neither binding nor status — is manual.
func TestComponentSourceDiscriminatorAndBackfill(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	mc, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Payments API", MonitorID: f.monitorID,
	})
	if err != nil {
		t.Fatalf("monitor component: %v", err)
	}
	if mc.Source != domain.ComponentSourceMonitor || mc.SourceProject != f.projectID || mc.OrgID != f.orgID {
		t.Fatalf("monitor component = %+v, want source monitor / project / org resolved server-side", mc)
	}
	sc, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Checkout", ServiceID: f.serviceID,
	})
	if err != nil {
		t.Fatalf("service component: %v", err)
	}
	if sc.Source != domain.ComponentSourceService || sc.SourceProject != f.projectID {
		t.Fatalf("service component = %+v, want source service", sc)
	}
	// Neither binding nor status: a manual component whose operator has not spoken yet.
	man, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "Third-party CDN"})
	if err != nil {
		t.Fatalf("manual component: %v", err)
	}
	if man.Source != domain.ComponentSourceManual || man.SourceProject != "" {
		t.Fatalf("manual component = %+v, want source manual with no project", man)
	}
}

// Invariant 62 at the schema: a component cannot be planted in another org, and a
// project-scoped page cannot hold a foreign project's component — the latter through the
// deferred constraint trigger, since a CHECK cannot read status_pages.
func TestComponentTenancyBySchema(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	// (a) cross-ORG: another org's page id with this org's org_id.
	org2, _ := st.CreateOrganization(ctx, "other", "Other")
	page2, err := st.CreateStatusPage(ctx, domain.StatusPage{
		OrgID: org2.ID, Slug: "other-status", Title: "Other", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("other page: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO components (status_page_id, org_id, source, name) VALUES ($1, $2, 'manual', 'x')`,
		page2.ID, f.orgID); err == nil {
		t.Fatal("a component was planted on another org's page")
	}

	// (b) cross-PROJECT service on a project-scoped page — the trigger's job.
	alien, err := st.CreateService(ctx, domain.Service{ProjectID: f.otherProj, Slug: "alien", Name: "alien"})
	if err != nil {
		t.Fatalf("alien service: %v", err)
	}
	_, err = st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Alien", ServiceID: alien.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "component_project_outside_page_scope") {
		t.Fatalf("foreign component on a project-scoped page = %v, want the page-scope guard", err)
	}

	// (c) the SAME component is legitimate on an ORG-LEVEL page: it holds several projects.
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageOrg, Name: "Alien", ServiceID: alien.ID,
	}); err != nil {
		t.Fatalf("org-level page must admit another project's service: %v", err)
	}

	// (d) narrowing a page's scope afterwards is refused by the other side of the trigger.
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET project_id = $2 WHERE id = $1`, f.pageOrg, f.projectID); err == nil {
		t.Fatal("a page was narrowed to a project while holding another project's component")
	}
}

// `no_data` is computed, never typed (invariant 64).
func TestManualStatusCannotBeNoData(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO components (status_page_id, org_id, source, name, manual_status)
		VALUES ($1, $2, 'manual', 'x', 'no_data')`, f.pageProj, f.orgID); err == nil {
		t.Fatal("manual_status='no_data' was accepted; it must be refused by CHECK")
	}
}

// A service bound as the ACTIVE source cannot be deleted (invariant 71): RESTRICT, because
// SET NULL would be an automatic conversion and CASCADE would delete a public row.
func TestServiceDeleteRefusedWhileBackingAComponent(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	if _, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Checkout", ServiceID: f.serviceID,
	}); err != nil {
		t.Fatalf("component: %v", err)
	}
	err := st.DeleteService(ctx, f.projectID, f.serviceID)
	if err == nil {
		t.Fatal("the service was deleted while a status-page component rendered it")
	}
	// The refusal must be typed AND name what to fix, not surface a raw constraint error.
	if !errors.Is(err, ErrServiceRendered) {
		t.Fatalf("delete error = %v, want ErrServiceRendered", err)
	}
	if !strings.Contains(err.Error(), "Checkout") || !strings.Contains(err.Error(), "Payments") {
		t.Fatalf("delete error = %v, want the component and page named", err)
	}
}

// The page ceiling only ever shrinks, so an oversized page cannot grow (invariant 71b).
func TestPageComponentCeiling(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	// Lower the ceiling to the current count to model an already-full page.
	if _, err := st.pool.Exec(ctx,
		`UPDATE status_pages SET component_ceiling = 1 WHERE id = $1`, f.pageProj); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}
	if _, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "first"}); err != nil {
		t.Fatalf("first component: %v", err)
	}
	_, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "second"})
	if !errors.Is(err, ErrPageComponentCeiling) {
		t.Fatalf("second component = %v, want ErrPageComponentCeiling", err)
	}
}

// Every component mutation bumps the page's structural CAS counter — neighbour edits included,
// because the conversion preview shows the page SUMMARY (invariant 70).
func TestPageGenerationBumpsOnAnyComponentMutation(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	gen := func() int64 {
		t.Helper()
		var g int64
		if err := st.pool.QueryRow(ctx,
			`SELECT component_generation FROM status_pages WHERE id = $1`, f.pageProj).Scan(&g); err != nil {
			t.Fatalf("read generation: %v", err)
		}
		return g
	}
	before := gen()
	if _, err := st.CreateComponent(ctx, domain.Component{StatusPageID: f.pageProj, Name: "one"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	after := gen()
	if after <= before {
		t.Fatalf("generation did not advance on create: %d → %d", before, after)
	}
}

// The §15.0 precedence is total, and the two rules it encodes hold: maintenance outranks
// absence, and a live-healthy service before its first seal is operational (the strip, not the
// status, is what `SealedInWindow` governs) — invariant 66.
func TestPublicComponentStatusPrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   ServiceStatusProjection
		want domain.ComponentStatus
	}{
		{"excluded outranks a healthy sli", ServiceStatusProjection{SLI: "healthy", Excluded: true}, domain.CompMaintenance},
		{"excluded outranks absence", ServiceStatusProjection{SLI: "unknown", Excluded: true}, domain.CompMaintenance},
		{"down", ServiceStatusProjection{SLI: "down"}, domain.CompMajorOutage},
		{"degraded", ServiceStatusProjection{SLI: "degraded"}, domain.CompDegraded},
		{"healthy", ServiceStatusProjection{SLI: "healthy"}, domain.CompOperational},
		{"unknown", ServiceStatusProjection{SLI: "unknown"}, domain.CompNoData},
		{"no sli declared", ServiceStatusProjection{SLI: "unknown", Reason: "no_sli_declared"}, domain.CompNoData},
		{
			"healthy before the first seal is OPERATIONAL, not no_data",
			ServiceStatusProjection{SLI: "healthy", SealedInWindow: false},
			domain.CompOperational,
		},
	}
	for _, c := range cases {
		if got := PublicComponentStatus(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The projection is honest about a service with no declaration: unknown with its reason, and no
// sealed history — and it reads the watermark rather than the wall clock.
func TestServiceStatusProjectionUndeclaredService(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	m, err := st.ServicePageProjections(ctx,
		[]ServiceRef{{ProjectID: f.projectID, ServiceID: f.serviceID}}, false)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	p, ok := m[f.serviceID]
	if !ok {
		t.Fatal("the service is missing from its own project's projection")
	}
	if p.SLI != "unknown" || p.Reason != "no_sli_declared" || p.SealedInWindow {
		t.Fatalf("projection = %+v, want unknown/no_sli_declared with no sealed window", p)
	}
	sp := ServiceStatusProjection{ServiceID: p.ServiceID, SLI: p.SLI, Excluded: p.Excluded,
		Reason: p.Reason, SealedThrough: p.SealedThrough, SealedInWindow: p.SealedInWindow}
	if PublicComponentStatus(sp) != domain.CompNoData {
		t.Fatalf("an undeclared service must project no_data, got %q", PublicComponentStatus(sp))
	}
	// A foreign project's read returns nothing rather than another tenant's row.
	other, err := st.ServicePageProjections(ctx,
		[]ServiceRef{{ProjectID: f.otherProj, ServiceID: f.serviceID}}, false)
	if err != nil {
		t.Fatalf("foreign projection: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("a foreign project read %d projections", len(other))
	}
	_ = time.Now
}

// A DORMANT service binding does not pin the service: deleting it clears the revert option and
// invalidates any preview built on it, rather than blocking on a binding no customer can see.
func TestServiceDeleteClearsDormantBinding(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	c, err := st.CreateComponent(ctx, domain.Component{
		StatusPageID: f.pageProj, Name: "Checkout", ServiceID: f.serviceID,
	})
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	// Convert it away directly, leaving service_id dormant — what a revert would restore.
	if _, err := st.pool.Exec(ctx, `
		UPDATE components SET source = 'monitor', monitor_id = $2 WHERE id = $1`,
		c.ID, f.monitorID); err != nil {
		t.Fatalf("convert: %v", err)
	}
	var genBefore, revBefore int64
	if err := st.pool.QueryRow(ctx, `
		SELECT sp.component_generation, c.revision
		  FROM components c JOIN status_pages sp ON sp.id = c.status_page_id
		 WHERE c.id = $1`, c.ID).Scan(&genBefore, &revBefore); err != nil {
		t.Fatalf("read before: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete with only a dormant binding must succeed: %v", err)
	}
	var svcID *string
	var genAfter, revAfter int64
	if err := st.pool.QueryRow(ctx, `
		SELECT c.service_id, sp.component_generation, c.revision
		  FROM components c JOIN status_pages sp ON sp.id = c.status_page_id
		 WHERE c.id = $1`, c.ID).Scan(&svcID, &genAfter, &revAfter); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if svcID != nil {
		t.Fatalf("dormant binding survived the deletion: %v", *svcID)
	}
	if genAfter <= genBefore || revAfter <= revBefore {
		t.Fatalf("sweep did not invalidate consent: gen %d→%d, rev %d→%d",
			genBefore, genAfter, revBefore, revAfter)
	}
}

// The 90-day strip is SEALED facts only, ends at the WATERMARK, keeps the NEWEST day under the
// cap, and skips days with no decidable time (invariant 69). Each of those is a way the strip
// could quietly lie, so each has an assertion here.
func TestServiceDailyAvailabilityWindowAndCap(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	rev := newRevision(t, st, ctx, proj, svc, 1, base.AddDate(0, 0, -200))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, base.AddDate(0, 0, -200))

	// The watermark sits at 12:00, deliberately NOT on a midnight boundary: a 90-day window then
	// straddles 91 calendar days, which is what makes an ascending LIMIT 90 drop the newest one.
	watermark := base.Add(12 * time.Hour)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_materialization (service_id, project_id, sealed_through, materialization_start, era_start)
		VALUES ($1,$2,$3,$4,$4)
		ON CONFLICT (service_id) DO UPDATE SET sealed_through = EXCLUDED.sealed_through`,
		svc, proj, watermark, base.AddDate(0, 0, -200)); err != nil {
		t.Fatalf("materialization: %v", err)
	}

	// 100 days of one sealed, fully-decidable bucket each, ending at the watermark's own day.
	for i := 0; i < 100; i++ {
		if i == 5 {
			continue // day -5 is reserved for the fully-excluded case below
		}
		day := base.AddDate(0, 0, -i).Add(6 * time.Hour)
		if err := insertBucket(st, ctx, proj, svc, epoch, day, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
			t.Fatalf("bucket %d: %v", i, err)
		}
		if _, err := st.pool.Exec(ctx,
			`UPDATE service_reliability_buckets SET state='sealed', sealed_at=now()
			  WHERE service_id=$1 AND bucket_start=$2`, svc, day); err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
	}
	// A day INSIDE the window whose time is entirely excluded by maintenance: nothing decidable,
	// so nothing to publish — it must be ABSENT, not 0%.
	blank := base.AddDate(0, 0, -5).Add(9 * time.Hour)
	// Fully excluded on BOTH axes: excluded_us alone accounts for the bucket, which is exactly
	// what the two conservation CHECKs require of a maintenance-covered minute.
	if err := insertBucket(st, ctx, proj, svc, epoch, blank, 0, 0, 0, 60e6, 0, 0, 0, 0); err != nil {
		t.Fatalf("excluded bucket: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_reliability_buckets SET state='sealed', sealed_at=now()
		  WHERE service_id=$1 AND bucket_start=$2`, svc, blank); err != nil {
		t.Fatalf("seal excluded: %v", err)
	}
	// The STRADDLE: the window opens at 12:00 on its first day, so a bucket LATER that day is
	// inside it. With one bucket on the opening day and one on the watermark's own day, the window
	// spans 91 calendar days — which is the only shape in which a `LIMIT 90` can drop a day, and
	// the reason an ascending limit would drop the NEWEST one.
	straddle := base.AddDate(0, 0, -90).Add(18 * time.Hour)
	if err := insertBucket(st, ctx, proj, svc, epoch, straddle, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("straddle bucket: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_reliability_buckets SET state='sealed', sealed_at=now()
		  WHERE service_id=$1 AND bucket_start=$2`, svc, straddle); err != nil {
		t.Fatalf("seal straddle: %v", err)
	}

	// A PROVISIONAL bucket at the very edge: sealed facts only, so it must not appear.
	provisional := base.AddDate(0, 0, -1).Add(18 * time.Hour)
	if err := insertBucket(st, ctx, proj, svc, epoch, provisional, 0, 60e6, 0, 0, 0, 0, 60e6, 0); err != nil {
		t.Fatalf("provisional bucket: %v", err)
	}

	// Through the PAGE path, which is the only implementation now: a second per-service strip
	// builder would be exactly the duplicated rule this phase kept having to remove.
	page, err := st.ServicePageProjections(ctx, []ServiceRef{{ProjectID: proj, ServiceID: svc}}, true)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	days := page[svc].Daily
	if len(days) == 0 {
		t.Fatal("no days returned")
	}
	if len(days) > 90 {
		t.Fatalf("returned %d days, above the 90-point cap", len(days))
	}
	// Ascending, and the NEWEST sealed day inside the window is present — the day the ascending
	// LIMIT used to drop.
	for i := 1; i < len(days); i++ {
		if !days[i].Day.After(days[i-1].Day) {
			t.Fatalf("days are not ascending at %d: %v then %v", i, days[i-1].Day, days[i].Day)
		}
	}
	// The watermark's OWN day carries a sealed bucket at 06:00, so it is the newest day the strip
	// can honestly show. An ascending `LIMIT 90` over the 91 groups above drops exactly this one.
	newest := days[len(days)-1].Day.UTC().Format("2006-01-02")
	if newest != base.Format("2006-01-02") {
		t.Fatalf("newest day = %s, want %s — the cap dropped the most recent day",
			newest, base.Format("2006-01-02"))
	}
	// The window ends AT the watermark: nothing after it, and nothing older than 90 days before.
	oldest := days[0].Day.UTC()
	if oldest.Before(watermark.AddDate(0, 0, -90).Truncate(24 * time.Hour)) {
		t.Fatalf("oldest day %v predates the 90-day window starting %v", oldest, watermark.AddDate(0, 0, -90))
	}
	// The all-excluded day is absent, and no returned day is a fabricated 0%.
	blankLabel := blank.Format("2006-01-02")
	for _, d := range days {
		if d.Day.UTC().Format("2006-01-02") == blankLabel {
			t.Fatalf("a day with zero decidable time was published as %.2f%%", d.Uptime)
		}
		if d.DecidableFraction <= 0 {
			t.Fatalf("day %v published with no decidable time", d.Day)
		}
	}
	// A foreign project reads nothing.
	other, err := st.CreateProject(ctx, orgOf(t, st, ctx, proj), "elsewhere", "Elsewhere")
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	foreign, err := st.ServicePageProjections(ctx,
		[]ServiceRef{{ProjectID: other.ID, ServiceID: svc}}, true)
	if err != nil {
		t.Fatalf("foreign daily: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("a foreign project read %d projections of another tenant's history", len(foreign))
	}
}

// orgOf resolves a project's org so a test can create a sibling project.
func orgOf(t *testing.T, st *Store, ctx context.Context, projectID string) string {
	t.Helper()
	var org string
	if err := st.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, projectID).Scan(&org); err != nil {
		t.Fatalf("resolve org: %v", err)
	}
	return org
}
