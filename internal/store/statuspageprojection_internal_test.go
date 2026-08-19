package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// [314] P1-3 — the projection is PAGE-scoped: one snapshot and a constant statement count for a set
// of (project, service) pairs that may span many projects. A per-project batch, which is what the
// first submission shipped, is not one snapshot at all for an org-level page.
func TestServicePageProjectionsAreOneBatchAcrossProjects(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	// A second project with its own service, so the page genuinely spans projects.
	other, err := st.CreateService(ctx, domain.Service{
		ProjectID: f.otherProj, Slug: "search", Name: "Search",
	})
	if err != nil {
		t.Fatalf("other service: %v", err)
	}

	refs := []ServiceRef{
		{ProjectID: f.projectID, ServiceID: f.serviceID},
		{ProjectID: f.otherProj, ServiceID: other.ID},
	}
	got, err := st.ServicePageProjections(ctx, refs, true)
	if err != nil {
		t.Fatalf("projections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d projections for 2 pairs across 2 projects", len(got))
	}
	for _, r := range refs {
		p, ok := got[r.ServiceID]
		if !ok {
			t.Fatalf("service %s missing", r.ServiceID)
		}
		if p.ProjectID != r.ProjectID {
			t.Fatalf("service %s came back under project %s, want %s", r.ServiceID, p.ProjectID, r.ProjectID)
		}
		// Undeclared: unknown with its reason, no number, and the withheld reason names why.
		if p.SLI != "unknown" || p.Reason != "no_sli_declared" {
			t.Fatalf("%s = %+v, want unknown/no_sli_declared", r.ServiceID, p)
		}
		if p.Uptime != nil {
			t.Fatalf("%s quoted %v with nothing declared", r.ServiceID, *p.Uptime)
		}
		if p.UptimeWithheld == "" {
			t.Fatalf("%s withheld its number with NO reason", r.ServiceID)
		}
	}

	// A CROSSED pair — the right service under the wrong project — resolves to nothing. The pair
	// filter is the tenancy boundary, so a crafted component cannot make another tenant's service
	// appear on a page.
	crossed, err := st.ServicePageProjections(ctx,
		[]ServiceRef{{ProjectID: f.otherProj, ServiceID: f.serviceID}}, false)
	if err != nil {
		t.Fatalf("crossed: %v", err)
	}
	if len(crossed) != 0 {
		t.Fatalf("a crossed (project, service) pair resolved %d projections", len(crossed))
	}
}

// The statement count is what the batching claim rests on, so it is measured rather than argued:
// 12 services across 6 projects must cost the same statements as 2 across 1.
func TestServicePageProjectionsStatementCountIsConstant(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")

	// The statement count is what a batching claim rests on, so it is MEASURED. Every behavioural
	// assertion in this file passes just as well against a per-component loop; only this fails.
	measure := func(refs []ServiceRef) int {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		tracer := &countingTracer{}
		cfg.ConnConfig.Tracer = tracer
		cfg.MaxConns = 2
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer pool.Close()
		traced := *st
		traced.pool = pool
		start := tracer.n
		if _, err := traced.ServicePageProjections(ctx, refs, true); err != nil {
			t.Fatalf("projections: %v", err)
		}
		return tracer.n - start
	}

	// DECLARED services with live members, and a maintenance window in each project. A fixture of
	// bare services never reaches the observation/maintenance branch at all — which is why the
	// first version of this test measured a constant that proved nothing ([318] P1-1).
	seedProject := func(projectID, tag string, n int) []ServiceRef {
		out := make([]ServiceRef, 0, n)
		for j := 0; j < n; j++ {
			slug := tag + "-svc" + string(rune('a'+j))
			out = append(out, ServiceRef{
				ProjectID: projectID,
				ServiceID: pageDeclaredService(t, st, ctx, projectID, slug, slug+"-mon"),
			})
		}
		now := time.Now().UTC()
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO maintenance_windows (project_id, monitor_id, reason, starts_at, ends_at)
			VALUES ($1, NULL, 'window', $2, $3)`,
			projectID, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
			t.Fatalf("maintenance window in %s: %v", projectID, err)
		}
		return out
	}

	refs := seedProject(f.projectID, "base", 1)
	small := measure(refs)

	// 10 more DECLARED services over 5 more projects, each with its own maintenance window: the
	// page now spans SIX projects, which is exactly what a per-project loop multiplies.
	for i := 0; i < 5; i++ {
		proj, err := st.CreateProject(ctx, f.orgID, "extra"+string(rune('a'+i)), "Extra")
		if err != nil {
			t.Fatalf("project %d: %v", i, err)
		}
		refs = append(refs, seedProject(proj.ID, "x"+string(rune('a'+i)), 2)...)
	}
	big := measure(refs)
	if big != small {
		t.Fatalf("statements: %d for 1 service in 1 project, %d for %d services in 6 projects — "+
			"the unauthenticated render cost grows with the page", small, big, len(refs))
	}
}

// [314] P1-3 — a service WITH sealed facts carries its number, and the number comes from the SAME
// §11.2/§11.3 owner the authenticated report uses. A page that quoted what the report withholds
// would be two claims about one set of facts.
func TestServicePageProjectionAgreesWithTheReport(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	rev := newRevision(t, st, ctx, proj, svc, 1, base.AddDate(0, 0, -120))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, base.AddDate(0, 0, -120))
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_materialization (service_id, project_id, sealed_through, materialization_start, era_start)
		VALUES ($1,$2,$3,$4,$4)
		ON CONFLICT (service_id) DO UPDATE SET sealed_through = EXCLUDED.sealed_through`,
		svc, proj, base, base.AddDate(0, 0, -120)); err != nil {
		t.Fatalf("materialization: %v", err)
	}
	// A single sealed minute is nowhere near 90 days of continuity, so BOTH the report and the page
	// must withhold — and for the same stated reason.
	if err := insertBucket(st, ctx, proj, svc, epoch, base.AddDate(0, 0, -1), 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_reliability_buckets SET state='sealed', sealed_at=now() WHERE service_id=$1`, svc); err != nil {
		t.Fatalf("seal: %v", err)
	}

	page, err := st.ServicePageProjections(ctx, []ServiceRef{{ProjectID: proj, ServiceID: svc}}, true)
	if err != nil {
		t.Fatalf("page projection: %v", err)
	}
	p := page[svc]
	if p.Uptime != nil {
		t.Fatalf("the page quoted %.4f%% from one sealed minute of a 90-day window", *p.Uptime)
	}
	if p.UptimeWithheld == "" {
		t.Fatal("the page withheld its number with no reason")
	}
	if !p.SealedInWindow {
		t.Fatal("a sealed fact inside the window was not seen, so no strip would be drawn")
	}
	if len(p.Daily) != 1 {
		t.Fatalf("strip = %d days, want the one sealed day", len(p.Daily))
	}
	// The authenticated report over the same window agrees about withholding, and its reason is the
	// one the page reports.
	win, ok := slaWindow90()
	if !ok {
		t.Skip("no 90d window registered")
	}
	rep, err := st.ServiceReliabilityReport(ctx, proj, svc, win)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Availability != nil {
		t.Fatalf("the report quoted %.4f%% where the page withheld", *rep.Availability)
	}
	reportReason := rep.AggregateWithheld
	if reportReason == "" {
		reportReason = string(rep.Reason)
	}
	if p.UptimeWithheld != reportReason {
		t.Fatalf("page withheld for %q, report for %q — two claims about one set of facts",
			p.UptimeWithheld, reportReason)
	}
}

// slaWindow90 resolves the 90-day window the public strip uses.
func slaWindow90() (sla.Window, bool) { return sla.WindowByName("90d") }

// The strip's days are UTC days, under ANY server session time zone.
//
// `date_trunc('day', timestamptz)` truncates in the SESSION zone, so a server running in, say,
// Asia/Baku would shift every boundary while the page still called them UTC days — a defect no
// UTC-session test can see. This uses the harness the migration-evidence test established
// (iter-0136/0137): a probe database UNIQUE PER RUN, created here, migrated under a Baku session,
// and dropped with FORCE afterwards. Nothing global is left behind, which is what makes this
// provable rather than merely asserted.
func TestDailyStripDaysAreUTCUnderANonUTCSession(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL (%q): %v", dsn, err)
	}
	probeName := fmt.Sprintf("cerbix_test_tzprobe_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+probeName); err != nil {
		t.Fatalf("create probe database %s (this gate requires CREATEDB): %v", probeName, err)
	}
	defer func() {
		// A fresh and GENEROUS context. This is CLEANUP, not a product deadline: `DROP DATABASE`
		// queues behind the catalog work of every other package creating and migrating databases
		// during a full `-race ./...`, and a 30-second budget failed the test for a reason that
		// says nothing about the code under test. The drop itself is still mandatory — a leaked
		// probe database would be a real defect — so this waits rather than skipping.
		dropCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, err := st.pool.Exec(dropCtx, `DROP DATABASE IF EXISTS `+probeName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database %s: %v", probeName, err)
		}
	}()

	u.Path = "/" + probeName
	q := u.Query()
	q.Set("timezone", "Asia/Baku") // UTC+4: a 22:00 UTC bucket falls on the NEXT Baku day
	u.RawQuery = q.Encode()
	probeDSN := u.String()
	if err := Migrate(ctx, probeDSN); err != nil {
		t.Fatalf("migrate probe: %v", err)
	}
	probe, err := Open(ctx, probeDSN)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()

	proj, _, svc := seedService(t, probe, ctx)
	// 22:00 UTC on the 15th is 02:00 on the 16th in Baku. A session-zone truncation would label
	// this bucket the 16th; the strip must call it the 15th.
	bucket := time.Date(2026, 8, 15, 22, 0, 0, 0, time.UTC)
	rev := newRevision(t, probe, ctx, proj, svc, 1, bucket.AddDate(0, 0, -30))
	epoch := newEpoch(t, probe, ctx, proj, svc, rev, 1, bucket.AddDate(0, 0, -30))
	if _, err := probe.pool.Exec(ctx, `
		INSERT INTO service_materialization (service_id, project_id, sealed_through, materialization_start, era_start)
		VALUES ($1,$2,$3,$4,$4)`,
		svc, proj, bucket.Add(2*time.Hour), bucket.AddDate(0, 0, -30)); err != nil {
		t.Fatalf("materialization: %v", err)
	}
	if err := insertBucket(probe, ctx, proj, svc, epoch, bucket, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	if _, err := probe.pool.Exec(ctx,
		`UPDATE service_reliability_buckets SET state='sealed', sealed_at=now() WHERE service_id=$1`, svc); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// The session really is non-UTC — otherwise this test proves nothing.
	var zone string
	if err := probe.pool.QueryRow(ctx, `SHOW TimeZone`).Scan(&zone); err != nil {
		t.Fatalf("read session zone: %v", err)
	}
	if zone == "UTC" {
		t.Fatalf("the probe session is UTC (%q): the harness cannot detect a session-zone truncation", zone)
	}

	page, err := probe.ServicePageProjections(ctx, []ServiceRef{{ProjectID: proj, ServiceID: svc}}, true)
	if err != nil {
		t.Fatalf("page projections: %v", err)
	}
	days := page[svc].Daily
	if len(days) != 1 {
		t.Fatalf("strip = %d days, want the one sealed day", len(days))
	}
	if got := days[0].Day.UTC().Format("2006-01-02"); got != "2026-08-15" {
		t.Fatalf("day = %s under a %s session, want the UTC day 2026-08-15", got, zone)
	}
}

// [318] P0-1 — an org-level page mixes projects, and a project-wide maintenance window covers
// EVERY member the evaluator is handed (`reliability.MaintenanceSpan.covers` treats an empty
// MonitorID as project-wide). Merging the page's spans into one list therefore made project A's
// window mark project B's service as under maintenance — a false PUBLIC status on a customer's
// page. Each service is evaluated against ITS OWN project's spans only.
func TestProjectWideMaintenanceDoesNotCrossProjects(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := seedProjection(t, st, ctx)

	// Two projects, each with a declared service whose SLI is a live monitor, so both reach the
	// evaluator with real members — the branch a member-less fixture never exercises.
	svcA := pageDeclaredService(t, st, ctx, f.projectID, "alpha", "mon-alpha")
	svcB := pageDeclaredService(t, st, ctx, f.otherProj, "beta", "mon-beta")

	// A PROJECT-WIDE window in project A only, in force right now.
	now := time.Now().UTC()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO maintenance_windows (project_id, monitor_id, reason, starts_at, ends_at)
		VALUES ($1, NULL, 'A-wide', $2, $3)`,
		f.projectID, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("maintenance window: %v", err)
	}

	got, err := st.ServicePageProjections(ctx, []ServiceRef{
		{ProjectID: f.projectID, ServiceID: svcA},
		{ProjectID: f.otherProj, ServiceID: svcB},
	}, false)
	if err != nil {
		t.Fatalf("page projections: %v", err)
	}
	a, b := got[svcA], got[svcB]
	if !a.Excluded {
		t.Fatalf("project A's own service is not excluded by A's project-wide window: %+v", a)
	}
	if b.Excluded {
		t.Fatalf("project B's service was excluded by project A's maintenance window: %+v — "+
			"a project-wide span leaked across the org page", b)
	}
	if PublicComponentStatus(ServiceStatusProjection{SLI: b.SLI, Excluded: b.Excluded}) == domain.CompMaintenance {
		t.Fatal("project B's component would publish 'Under maintenance' because of another project")
	}
}

// pageDeclaredService creates a service with ONE live monitor declared as both context and SLI, so
// the projection reaches the evaluator with real members.
func pageDeclaredService(t *testing.T, st *Store, ctx context.Context, projectID, slug, monitorName string) string {
	t.Helper()
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projectID, Name: monitorName, Type: domain.MonitorHTTP,
		Target: "https://" + slug + ".example.com/", IntervalSeconds: 60, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor %s: %v", monitorName, err)
	}
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: projectID, Slug: slug, Name: slug})
	if err != nil {
		t.Fatalf("service %s: %v", slug, err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, projectID, svc.ID, domain.ServiceDeclaration{
		Monitors: []string{mon.ID}, SLI: []string{mon.ID},
	}, 0, DeclarationOptions{CreatedBy: "test"}); err != nil {
		t.Fatalf("declare %s: %v", slug, err)
	}
	// A declaration takes effect at the NEXT canonical bucket boundary, which is correct and is
	// not what this test is about. Backdating the epoch is a fixture concern: it puts the service
	// in the state the maintenance scoping question presupposes — declared and already effective.
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_evaluation_epochs SET effective_at = now() - interval '1 hour'
		 WHERE service_id = $1`, svc.ID); err != nil {
		t.Fatalf("backdate epoch for %s: %v", slug, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE service_definition_revisions SET effective_at = now() - interval '1 hour'
		 WHERE service_id = $1`, svc.ID); err != nil {
		t.Fatalf("backdate revision for %s: %v", slug, err)
	}
	return svc.ID
}
