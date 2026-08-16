package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// These tests assert the constraints migration 00064 exists FOR, not that its tables were
// created. A schema test that only checks table names passes while every guard is missing.

func serviceSchemaStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run service reliability schema tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st, ctx
}

// seedService creates an org/project/monitor/service and returns the ids.
func seedService(t *testing.T, st *Store, ctx context.Context) (projectID, monitorID, serviceID string) {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "checkout-http", Type: domain.MonitorHTTP,
		Target: "https://checkout.example.com/healthz", IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	var svcID string
	err = st.pool.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1,'checkout','Checkout') RETURNING id`,
		proj.ID).Scan(&svcID)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return proj.ID, mon.ID, svcID
}

func newRevision(t *testing.T, st *Store, ctx context.Context, projectID, serviceID string, rev int64, effectiveAt time.Time) string {
	t.Helper()
	var id string
	err := st.pool.QueryRow(ctx,
		`INSERT INTO service_definition_revisions (service_id, project_id, revision, effective_at)
		 VALUES ($1,$2,$3,$4) RETURNING id`, serviceID, projectID, rev, effectiveAt).Scan(&id)
	if err != nil {
		t.Fatalf("revision %d: %v", rev, err)
	}
	return id
}

func newEpoch(t *testing.T, st *Store, ctx context.Context, projectID, serviceID, revisionID string, seq int64, effectiveAt time.Time) string {
	t.Helper()
	var id string
	err := st.pool.QueryRow(ctx,
		`INSERT INTO service_evaluation_epochs (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		 VALUES ($1,$2,$3,$4,$5,'h') RETURNING id`, serviceID, projectID, seq, revisionID, effectiveAt).Scan(&id)
	if err != nil {
		t.Fatalf("epoch %d: %v", seq, err)
	}
	return id
}

func insertBucket(st *Store, ctx context.Context, projectID, serviceID, epochID string, start time.Time, good, bad, unknown, excluded, healthy, degraded, down, healthUnknown int64) error {
	_, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us,
		    good_us, bad_us, unknown_us, excluded_us,
		    healthy_us, degraded_us, down_us, health_unknown_us)
		 VALUES ($1,$2,$3,$4,60000000,$5,$6,$7,$8,$9,$10,$11,$12)`,
		serviceID, projectID, epochID, start, good, bad, unknown, excluded, healthy, degraded, down, healthUnknown)
	return err
}

// The two CHECK constraints are the point of the fact table: both axes must account for
// the whole bucket. A reducer that loses time produces a number nobody can reconcile, and
// the database is the last place that can still say so.
func TestFactConservationIsEnforcedOnBothAxes(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	rev := newRevision(t, st, ctx, proj, svc, 1, time.Now().UTC().Truncate(time.Minute))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, time.Now().UTC().Truncate(time.Minute))
	start := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	// A conserving row is accepted.
	if err := insertBucket(st, ctx, proj, svc, epoch, start, 40e6, 20e6, 0, 0, 40e6, 0, 20e6, 0); err != nil {
		t.Fatalf("a conserving row was rejected: %v", err)
	}

	// Availability axis one microsecond short.
	err := insertBucket(st, ctx, proj, svc, epoch, start.Add(time.Minute), 40e6, 20e6-1, 0, 0, 40e6, 0, 20e6, 0)
	if err == nil || !strings.Contains(err.Error(), "availability_conserves") {
		t.Errorf("availability axis losing 1µs was accepted (err=%v)", err)
	}

	// Health axis short while availability conserves — the axis a single check would miss.
	err = insertBucket(st, ctx, proj, svc, epoch, start.Add(2*time.Minute), 40e6, 20e6, 0, 0, 40e6, 0, 20e6-1, 0)
	if err == nil || !strings.Contains(err.Error(), "health_conserves") {
		t.Errorf("health axis losing 1µs was accepted (err=%v)", err)
	}
}

// Exactly one EFFECTIVE row per (service, effective_at) on both axes. This is what makes
// the same-boundary race decidable: two writes seconds apart target the same next boundary,
// and without this index both would be effective and a fact would resolve to two.
func TestOneEffectiveRowPerBoundary(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	at := time.Date(2026, 8, 16, 12, 1, 0, 0, time.UTC)

	r1 := newRevision(t, st, ctx, proj, svc, 1, at)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_definition_revisions (service_id, project_id, revision, effective_at)
		 VALUES ($1,$2,2,$3)`, svc, proj, at); err == nil {
		t.Error("a second EFFECTIVE revision on the same boundary was accepted")
	}

	// Marking the first superseded_before_effect frees the boundary — that is exactly how
	// the later write wins while the earlier row is retained for audit.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_definition_revisions SET state='superseded_before_effect' WHERE id=$1`, r1); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	r2 := newRevision(t, st, ctx, proj, svc, 2, at)

	newEpoch(t, st, ctx, proj, svc, r2, 1, at)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_evaluation_epochs (service_id, project_id, epoch_seq, revision_id, effective_at, snapshot_hash)
		 VALUES ($1,$2,2,$3,$4,'h')`, svc, proj, r2, at); err == nil {
		t.Error("a second EFFECTIVE epoch on the same boundary was accepted")
	}
}

// The live guard fires at COMMIT: deleting a monitor the in-force SLI names fails, which is
// what the API maps to 409. The deferred FK is what lets the check happen at commit rather
// than mid-statement.
func TestDeletingAnInUseMonitorFailsAtCommit(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, mon, svc := seedService(t, st, ctx)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_member_refs (service_id, project_id, monitor_id, role)
		 VALUES ($1,$2,$3,'sli')`, svc, proj, mon); err != nil {
		t.Fatalf("member ref: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitors WHERE id=$1`, mon); err == nil {
		t.Fatal("deleting a monitor referenced by an in-force SLI was allowed")
	}
}

// ...while HISTORY survives that same deletion. A revision from three months ago must stay
// readable after the monitor it named is gone, or the timeline loses the boundary that
// explains why the number changed. One FK cannot serve both, which is why there are two
// tables.
func TestHistoricalMembershipSurvivesMonitorDeletion(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, mon, svc := seedService(t, st, ctx)
	rev := newRevision(t, st, ctx, proj, svc, 1, time.Now().UTC().Truncate(time.Minute))
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_definition_members (revision_id, project_id, monitor_id, monitor_name, role)
		 VALUES ($1,$2,$3,'checkout-http','sli')`, rev, proj, mon); err != nil {
		t.Fatalf("historical member: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitors WHERE id=$1`, mon); err != nil {
		t.Fatalf("history blocked a monitor deletion it should not guard: %v", err)
	}
	var name string
	if err := st.pool.QueryRow(ctx,
		`SELECT monitor_name FROM service_definition_members WHERE revision_id=$1`, rev).Scan(&name); err != nil {
		t.Fatalf("historical row vanished with the monitor: %v", err)
	}
	if name != "checkout-http" {
		t.Errorf("the name snapshot is what keeps the row legible, got %q", name)
	}
}

// A fact whose bucket falls outside every pre-created month must still land. Losing one
// would be a silent hole in a watermark that is defined by contiguity — the DEFAULT
// partition is what makes that impossible.
func TestDefaultPartitionAcceptsAnUnplannedBucket(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	rev := newRevision(t, st, ctx, proj, svc, 1, time.Now().UTC().Truncate(time.Minute))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, time.Now().UTC().Truncate(time.Minute))

	far := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := insertBucket(st, ctx, proj, svc, epoch, far, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("a bucket outside every pre-created month was rejected: %v", err)
	}
	var partition string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM service_reliability_buckets WHERE bucket_start=$1`, far).Scan(&partition); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasSuffix(partition, "_default") {
		t.Errorf("expected the DEFAULT partition, got %s", partition)
	}
}

// A sealed fact must carry the instant it was sealed: "sealed" with no stamp would make the
// audit trail unreadable at exactly the moment someone disputes the number.
func TestSealedFactMustCarryItsStamp(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, _, svc := seedService(t, st, ctx)
	rev := newRevision(t, st, ctx, proj, svc, 1, time.Now().UTC().Truncate(time.Minute))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, time.Now().UTC().Truncate(time.Minute))
	start := time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)
	if err := insertBucket(st, ctx, proj, svc, epoch, start, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_reliability_buckets SET state='sealed' WHERE service_id=$1 AND bucket_start=$2`,
		svc, start); err == nil {
		t.Error("a fact was marked sealed with no sealed_at")
	}
}

// Deleting a service removes its facts, ingest rows, late arrivals and ranges in one
// cascade: leaving any behind would strand rows nothing can ever resolve.
func TestServiceDeletionCascadesTheWholeDomain(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, mon, svc := seedService(t, st, ctx)
	rev := newRevision(t, st, ctx, proj, svc, 1, time.Now().UTC().Truncate(time.Minute))
	epoch := newEpoch(t, st, ctx, proj, svc, rev, 1, time.Now().UTC().Truncate(time.Minute))
	start := time.Date(2026, 8, 16, 14, 0, 0, 0, time.UTC)
	if err := insertBucket(st, ctx, proj, svc, epoch, start, 60e6, 0, 0, 0, 60e6, 0, 0, 0); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO service_bucket_ingest (service_id, project_id, bucket_start) VALUES ($1,$2,$3)`,
		`INSERT INTO service_late_arrivals (service_id, project_id, bucket_start, monitor_id) VALUES ($1,$2,$3,'` + mon + `')`,
		`INSERT INTO service_materialization (service_id, project_id, materialization_start, era_start) VALUES ($1,$2,$3,$3)`,
	} {
		if _, err := st.pool.Exec(ctx, q, svc, proj, start); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_repair_ranges (service_id, project_id, range_start, range_end, reason)
		 VALUES ($1,$2,$3,$4,'backfill')`, svc, proj, start, start.Add(time.Hour)); err != nil {
		t.Fatalf("range: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id=$1`, svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	for _, table := range []string{
		"service_reliability_buckets", "service_bucket_ingest", "service_late_arrivals",
		"service_materialization", "service_repair_ranges", "service_definition_revisions",
		"service_evaluation_epochs",
	} {
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE service_id=$1`, svc).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s kept %d rows after the service was deleted", table, n)
		}
	}
}

// Maintenance gains its two temporal columns and keeps its existing rows: archive and
// cancel are additive, and the ordinary delete path is untouched by this migration.
func TestMaintenanceGainsArchiveAndCancelColumns(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	proj, mon, _ := seedService(t, st, ctx)
	from := time.Now().UTC().Add(-time.Hour)
	mw, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: proj, MonitorID: mon, StartsAt: from, EndsAt: from.Add(2 * time.Hour), Reason: "db failover",
	})
	if err != nil {
		t.Fatalf("create window: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE maintenance_windows SET archived_at = now(), cancel_effective_at = statement_timestamp() WHERE id=$1`,
		mw.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var archived, cancelled *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT archived_at, cancel_effective_at FROM maintenance_windows WHERE id=$1`, mw.ID).Scan(&archived, &cancelled); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if archived == nil || cancelled == nil {
		t.Fatal("archive/cancel stamps did not persist")
	}
}
