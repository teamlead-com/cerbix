package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
)

// iter-0133 — the phase-1 backlog sweep. Each test here fails on the pre-0133 code and was
// re-applied as a mutant to prove it.

// plantBeat inserts a raw heartbeat row directly: the ingest path quarantines future
// timestamps (correctly), and these tests need rows INSIDE a window whose span sits around
// or after now — the archived window's declared span is exactly the time under test.
func plantBeat(t *testing.T, st *Store, ctx context.Context, monitorID string, ts time.Time, up bool) {
	t.Helper()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
		 VALUES ($1,$2,$3,0,0,'',$2)`, monitorID, ts, up); err != nil {
		t.Fatalf("plant heartbeat: %v", err)
	}
}

// T1 — an archived FUTURE window must NEVER take effect, in either axis. Before this fix the
// archive left cancel_effective_at NULL for a window that had not started, so every
// effective-end reducer kept its declared end: when the window's time arrived, it excluded
// SLA time and suppressed alert delivery exactly as if the operator had never archived it.
func TestAnArchivedFutureWindowNeverTakesEffect(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	starts := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Minute)
	ends := starts.Add(30 * time.Minute)
	w, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: starts, EndsAt: ends, Reason: "cancelled before it began",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("create future window: %v", err)
	}
	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// The archive pinned the effect to the empty interval [starts_at, starts_at).
	var cancel *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT cancel_effective_at FROM maintenance_windows WHERE id=$1`, w.ID).Scan(&cancel); err != nil {
		t.Fatalf("reread window: %v", err)
	}
	if cancel == nil || !cancel.Equal(starts) {
		t.Fatalf("cancel_effective_at = %v, want the window's own start %s", cancel, starts)
	}

	// LEGACY axis: a DOWN heartbeat inside the archived window's declared span must COUNT.
	plantBeat(t, st, ctx, f.http, starts.Add(10*time.Minute), false)
	c, err := st.MonitorSLI(ctx, f.http, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	if c.Total != 1 || c.Up != 0 {
		t.Fatalf("SLI counted total=%d up=%d; the archived window still excludes the heartbeat", c.Total, c.Up)
	}

	// SERVICE axis: the spans reader — the evaluator's ONLY maintenance input — sees nothing.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	spans, err := maintenanceSpansFor(ctx, tx, f.projectID,
		[]reliability.Member{{MonitorID: f.http}}, starts, ends)
	if err != nil {
		t.Fatalf("spans: %v", err)
	}
	for _, sp := range spans {
		if sp.ID == w.ID {
			t.Fatalf("the archived future window still projects a span %s→%s", sp.From, sp.To)
		}
	}
}

// T1 — the LEGACY suppression paths honor the effective end, not the declared one. A window
// archived mid-flight stops excluding SLA time and stops suppressing alert delivery AT the
// cancel instant; the time it genuinely covered stands.
func TestLegacySuppressionStopsAtTheCancelInstant(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	starts := time.Now().UTC().Add(-time.Hour)
	ends := time.Now().UTC().Add(time.Hour)
	w, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: starts, EndsAt: ends, Reason: "cut short",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("create active window: %v", err)
	}

	// One DOWN heartbeat while the window genuinely governed…
	plantBeat(t, st, ctx, f.http, time.Now().UTC().Add(-30*time.Minute), false)

	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive active window: %v", err)
	}

	// …and one after the cut, still inside the DECLARED end.
	plantBeat(t, st, ctx, f.http, time.Now().UTC().Add(30*time.Minute), false)

	c, err := st.MonitorSLI(ctx, f.http, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("monitor sli: %v", err)
	}
	// Exactly ONE counts: the covered heartbeat stays excluded (that exclusion was real),
	// the post-cancel one is measured again.
	if c.Total != 1 {
		t.Fatalf("SLI total=%d, want 1: the declared end kept suppressing past the cancel", c.Total)
	}

	// Alert delivery mirrors it: the monitor is NOT in maintenance anymore.
	inMaint, err := st.MonitorInMaintenance(ctx, f.http)
	if err != nil {
		t.Fatalf("in maintenance: %v", err)
	}
	if inMaint {
		t.Fatal("an archived window still suppresses alert delivery")
	}
}

// T2 — maintenance invalidation goes through the COALESCING enqueue, and the ranges carry
// their window as origin. The raw INSERT it replaces stacked one more pending row per
// mutation per service — unbounded queue growth — and left origin_id NULL forever, so
// cancelRangesOfOrigin could never match anything (the origin_id overclaim).
func TestMaintenanceMutationsCoalesceAndCarryTheirOrigin(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	w1, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(20 * time.Minute), Reason: "first",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("first window: %v", err)
	}
	w2, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base.Add(10 * time.Minute), EndsAt: base.Add(40 * time.Minute), Reason: "second",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("second window: %v", err)
	}
	_ = w1

	var count int
	var rangeStart, rangeEnd time.Time
	var origin *string
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*), min(range_start), max(range_end), max(origin_id::text)
		   FROM service_repair_ranges
		  WHERE service_id = $1 AND reason = 'maintenance' AND state = 'pending'`,
		f.serviceID).Scan(&count, &rangeStart, &rangeEnd, &origin); err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d pending maintenance ranges after two mutations, want 1 (the union)", count)
	}
	if !rangeStart.Equal(domain.FloorToBucket(base)) || !rangeEnd.Equal(domain.CeilToBucket(base.Add(40*time.Minute))) {
		t.Fatalf("merged range [%s, %s) is not the union of both mutations", rangeStart, rangeEnd)
	}
	// A union required by TWO windows belongs to no single origin: stamping it with the
	// latest would make W2's cancellation able to discard the part W1 still requires,
	// violating §10.8's exact-union preservation. Ambiguous provenance is NULL provenance.
	if origin != nil {
		t.Fatalf("merged origin = %q, want NULL for a union of two windows (w1=%s w2=%s)", *origin, w1.ID, w2.ID)
	}

	// Cancel-by-origin cannot touch the ambiguous union…
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed below
	if err := cancelRangesOfOrigin(ctx, tx, f.serviceID, []string{w1.ID, w2.ID}); err != nil {
		t.Fatalf("cancel by origin: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var superseded int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges
		  WHERE service_id = $1 AND reason = 'maintenance' AND state = 'superseded'`,
		f.serviceID).Scan(&superseded); err != nil {
		t.Fatalf("read superseded: %v", err)
	}
	if superseded != 0 {
		t.Fatalf("cancel-by-origin superseded %d ranges of an ambiguous union, want 0", superseded)
	}

	// …and CAN cancel a single-origin range — the STORE-layer semantics; the production
	// canceller for maintenance origins is not wired yet, which iter-0133 §5 says outright.
	w3, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base.Add(2 * time.Hour), EndsAt: base.Add(3 * time.Hour), Reason: "third",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("third window: %v", err)
	}
	tx2, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx2.Rollback(ctx) //nolint:errcheck // committed below
	if err := cancelRangesOfOrigin(ctx, tx2, f.serviceID, []string{w3.ID}); err != nil {
		t.Fatalf("cancel single origin: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges
		  WHERE service_id = $1 AND reason = 'maintenance' AND state = 'superseded'`,
		f.serviceID).Scan(&superseded); err != nil {
		t.Fatalf("read superseded: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("cancel-by-origin superseded %d ranges, want exactly the single-origin one", superseded)
	}
}

// T3 — the fact table's monthly partitions are MAINTAINED, not merely seeded. Migration
// 00064 pre-created a window around its own run date; without an ongoing creator, every fact
// insert roughly six months after an install would land in the DEFAULT partition forever.
func TestServiceFactPartitionsAreMaintained(t *testing.T) {
	st, ctx := declStore(t)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 8, 0)
	name := "service_reliability_buckets_" + future.Format("200601")

	partitionExists := func() bool {
		var exists bool
		if err := st.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists); err != nil {
			t.Fatalf("check partition: %v", err)
		}
		return exists
	}
	// DDL survives TruncateAll, so a previous run's partition would fake the "before" state:
	// drop it and prove creation from scratch every run.
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset partition: %v", err)
	}
	if partitionExists() {
		t.Fatalf("partition %s exists before maintenance ran; the construction is wrong", name)
	}
	if err := st.EnsureServiceFactPartitions(ctx, 8); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !partitionExists() {
		t.Fatalf("partition %s missing after maintenance", name)
	}
	// Idempotent against itself and against the migration's own seeding.
	if err := st.EnsureServiceFactPartitions(ctx, 8); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
}

// F3 (round 2/2 P1) — a month whose rows already landed in DEFAULT is RECOVERED, not failed
// forever. Postgres refuses CREATE ... PARTITION OF when the default holds rows of the new
// range; after one missed cadence that refusal would repeat on every tick while every new
// fact kept piling into DEFAULT. The recovery moves the month's rows out and attaches.
func TestPartitionMaintenanceAdoptsAStrandedDefaultMonth(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 9, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset partition: %v", err)
	}

	// Strand a fact in DEFAULT: no partition exists for the month, so the parent routes it
	// there — exactly the state one missed cadence leaves behind.
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	bucket := future.Add(24 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("strand fact: %v", err)
	}
	var inDefault bool
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text = 'service_reliability_buckets_default'
		   FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&inDefault); err != nil {
		t.Fatalf("locate fact: %v", err)
	}
	if !inDefault {
		t.Fatal("construction is wrong: the fact did not land in DEFAULT")
	}

	if err := st.EnsureServiceFactPartitions(ctx, 9); err != nil {
		t.Fatalf("ensure with a stranded month: %v", err)
	}

	// The row is still visible through the parent AND now lives in the month partition.
	var home string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text
		   FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&home); err != nil {
		t.Fatalf("fact lost during adoption: %v", err)
	}
	if home != name {
		t.Fatalf("fact lives in %s, want %s", home, name)
	}
	// And the cadence stays idempotent afterwards.
	if err := st.EnsureServiceFactPartitions(ctx, 9); err != nil {
		t.Fatalf("ensure after adoption: %v", err)
	}
}

// T1 — the inventory tells the truth: List/Get carry archived_at and cancel_effective_at, so
// a client can tell an archived window from a live one. Before this, the SPA archived a
// window, reloaded, and watched it come back.
func TestListExposesTheArchiveState(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	starts := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	w, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: starts, EndsAt: starts.Add(time.Hour), Reason: "to archive",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	list, err := st.ListMaintenanceWindowsByProject(ctx, f.projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *domain.MaintenanceWindow
	for i := range list {
		if list[i].ID == w.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("the archived window left the list entirely; it is history, not a secret")
	}
	if found.ArchivedAt == nil || found.CancelEffectiveAt == nil {
		t.Fatalf("archived=%v cancel=%v: the list hides the archive state", found.ArchivedAt, found.CancelEffectiveAt)
	}
	got, err := st.GetMaintenanceWindow(ctx, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ArchivedAt == nil {
		t.Fatal("Get hides the archive state")
	}
}

// T4 — the operational snapshot the leader exports: queue depth by state, and a watermark lag
// that counts from era_start when nothing is sealed yet — a service that never sealed a
// bucket since its declaration IS lagging, and a zero there would blind the gauge.
func TestServiceReliabilityStatsSnapshot(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stat, err := st.ServiceReliabilityStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stat.RepairPending != 1 || stat.RepairRunning != 0 {
		t.Fatalf("pending=%d running=%d, want 1/0", stat.RepairPending, stat.RepairRunning)
	}
	if stat.WatermarkLagSeconds <= 0 {
		t.Fatalf("watermark lag %.3f, want > 0 for a service that sealed nothing yet", stat.WatermarkLagSeconds)
	}

	// A claim moves the row to running; the snapshot follows the state, not the history.
	if _, ok, err := st.claimRepairRangeOn(ctx, st.pool); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	stat, err = st.ServiceReliabilityStats(ctx)
	if err != nil {
		t.Fatalf("stats after claim: %v", err)
	}
	if stat.RepairPending != 0 || stat.RepairRunning != 1 {
		t.Fatalf("pending=%d running=%d after claim, want 0/1", stat.RepairPending, stat.RepairRunning)
	}
}

// T5 — §15.4 acceptance: a service-owner write cannot deadlock a routing delete. The owner
// path takes FOR KEY SHARE on the referenced routing rows BEFORE inserting the service, so
// both directions agree; this test parks both sides at their lock waits — observed in
// pg_stat_activity, the same determinism device as the barriered-backfill test — and then
// releases them into each other. What is asserted is the ABSENCE of a deadlock (40P01): one
// side wins the row, the other completes or fails with a clean owner error.
func TestServiceOwnerWritesCannotDeadlockWithRoutingDeletes(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	var policyID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO escalation_policies (project_id, name) VALUES ($1, 'lock-order-p') RETURNING id`,
		f.projectID).Scan(&policyID); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	barrier, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin barrier: %v", err)
	}
	defer barrier.Rollback(ctx) //nolint:errcheck // released below
	if _, err := barrier.Exec(ctx,
		`SELECT 1 FROM escalation_policies WHERE id = $1 FOR UPDATE`, policyID); err != nil {
		t.Fatalf("hold policy: %v", err)
	}

	type result struct {
		who string
		err error
	}
	done := make(chan result, 2)
	go func() {
		_, cerr := st.CreateService(ctx, domain.Service{
			ProjectID: f.projectID, Slug: "lock-order-svc", Name: "Lock order",
			EscalationPolicyID: policyID,
		})
		done <- result{"create", cerr}
	}()
	go func() {
		done <- result{"delete", st.DeleteEscalationPolicy(ctx, policyID)}
	}()

	// Both provably AT their waits before the barrier lifts.
	waited := false
	for i := 0; i < 400; i++ {
		var waiting int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock'
			    AND (query LIKE 'SELECT 1 FROM escalation_policies%' OR query LIKE 'DELETE FROM escalation_policies%')`).
			Scan(&waiting); err != nil {
			t.Fatalf("watch waiters: %v", err)
		}
		if waiting >= 2 {
			waited = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waited {
		t.Fatal("the two writers never reached their lock waits; the barrier did not bite")
	}
	if err := barrier.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			if strings.Contains(r.err.Error(), "40P01") || strings.Contains(r.err.Error(), "deadlock") {
				t.Fatalf("%s deadlocked: %v", r.who, r.err)
			}
			// A clean loss is a legal outcome: the delete won and the owner is gone.
			if r.who == "create" && !errors.Is(r.err, ErrOwnerNotInProject) {
				t.Fatalf("create failed with something other than the owner error: %v", r.err)
			}
		}
	}
}

// T5 — the same acceptance for the on-call schedule owner.
func TestServiceOwnerVersusScheduleDeleteCannotDeadlock(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	var scheduleID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at)
		 VALUES ($1, 'lock-order-s', 3600, now()) RETURNING id`,
		f.projectID).Scan(&scheduleID); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	barrier, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin barrier: %v", err)
	}
	defer barrier.Rollback(ctx) //nolint:errcheck // released below
	if _, err := barrier.Exec(ctx,
		`SELECT 1 FROM oncall_schedules WHERE id = $1 FOR UPDATE`, scheduleID); err != nil {
		t.Fatalf("hold schedule: %v", err)
	}

	type result struct {
		who string
		err error
	}
	done := make(chan result, 2)
	go func() {
		_, cerr := st.CreateService(ctx, domain.Service{
			ProjectID: f.projectID, Slug: "lock-order-svc2", Name: "Lock order 2",
			OncallScheduleID: scheduleID,
		})
		done <- result{"create", cerr}
	}()
	go func() {
		_, derr := st.pool.Exec(ctx, `DELETE FROM oncall_schedules WHERE id = $1`, scheduleID)
		done <- result{"delete", derr}
	}()

	waited := false
	for i := 0; i < 400; i++ {
		var waiting int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock'
			    AND (query LIKE 'SELECT 1 FROM oncall_schedules%' OR query LIKE 'DELETE FROM oncall_schedules%')`).
			Scan(&waiting); err != nil {
			t.Fatalf("watch waiters: %v", err)
		}
		if waiting >= 2 {
			waited = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waited {
		t.Fatal("the two writers never reached their lock waits")
	}
	if err := barrier.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			if strings.Contains(r.err.Error(), "40P01") || strings.Contains(r.err.Error(), "deadlock") {
				t.Fatalf("%s deadlocked: %v", r.who, r.err)
			}
			if r.who == "create" && !errors.Is(r.err, ErrOwnerNotInProject) {
				t.Fatalf("create failed with something other than the owner error: %v", r.err)
			}
		}
	}
}

// T5 — the MONITOR direction, documented rather than asserted safe (§15.4: pre-existing
// FR-012 backlog; FR-021 is required only not to widen it). Real UpdateMonitor racing a real
// DeleteEscalationPolicy, barriered at the policy row: the run must be BOUNDED — Postgres's
// deadlock detector resolves any cycle within about a second — and its outcome is logged as
// the record of today's behaviour.
func TestMonitorVersusPolicyDeleteIsBoundedAndDocumented(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	var policyID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO escalation_policies (project_id, name) VALUES ($1, 'doc-p') RETURNING id`,
		f.projectID).Scan(&policyID); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	mon, err := st.GetMonitor(ctx, f.http)
	if err != nil {
		t.Fatalf("read monitor: %v", err)
	}
	mon.EscalationPolicyID = policyID

	barrier, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin barrier: %v", err)
	}
	defer barrier.Rollback(ctx) //nolint:errcheck // released below
	if _, err := barrier.Exec(ctx,
		`SELECT 1 FROM escalation_policies WHERE id = $1 FOR UPDATE`, policyID); err != nil {
		t.Fatalf("hold policy: %v", err)
	}

	type result struct {
		who string
		err error
	}
	done := make(chan result, 2)
	go func() {
		_, uerr := st.UpdateMonitor(ctx, mon)
		done <- result{"update", uerr}
	}()
	go func() {
		done <- result{"delete", st.DeleteEscalationPolicy(ctx, policyID)}
	}()
	time.Sleep(150 * time.Millisecond) // both in flight against the barrier
	if err := barrier.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			t.Logf("documented outcome: %s → %v", r.who, r.err)
		case <-deadline:
			t.Fatal("the monitor/policy race did not resolve within 10s: an unbounded wait, not the documented hazard")
		}
	}
}

// T6 — the derived-slug collision suffix FITS the shape. A digit-only name lands on the
// "monitor-" + 55 path — exactly the 63-character limit — and the old "%s-%d" made the first
// collision candidate 65 characters: an invalid slug the loop never re-validated, so the
// INSERT died on the shape constraint as a 500.
func TestDerivedSlugCollisionSuffixFitsTheShape(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	name := "9" + strings.Repeat("8", 60) // digits only → base = "monitor-" + 55 digits = 63 chars
	first, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: name, Type: domain.MonitorHTTP, Target: "https://a.example",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !domain.ValidMonitorSlug(first.Slug) || len(first.Slug) != 63 {
		t.Fatalf("construction is wrong: first slug %q (len %d) must be the 63-char boundary", first.Slug, len(first.Slug))
	}
	second, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: name, Type: domain.MonitorHTTP, Target: "https://b.example",
	})
	if err != nil {
		t.Fatalf("the collision candidate overflowed the slug shape: %v", err)
	}
	if !domain.ValidMonitorSlug(second.Slug) {
		t.Fatalf("derived slug %q violates the shape it must satisfy", second.Slug)
	}
	if second.Slug == first.Slug {
		t.Fatal("collision not resolved")
	}
}

// T6 — the NO TRANSACTION migrations are crash-resumable: a crash between two of their
// statements leaves goose thinking the migration never ran, so every statement must walk
// over what the first attempt already built. Re-running the whole Up section against the
// migrated schema is exactly that resume, statement for statement.
func TestNoTransactionMigrationsAreCrashResumable(t *testing.T) {
	st, ctx := declStore(t)
	for _, name := range []string{
		"00043_heartbeats_hypertable.sql",
		"00064_service_reliability.sql",
		"00065_monitor_slug.sql",
	} {
		raw, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		// No args → pgx uses the simple protocol, which executes the multi-statement body
		// the same way a resumed migration would: sequentially, no wrapping transaction.
		if _, err := st.pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s cannot resume after a mid-flight crash: %v", name, err)
		}
	}
}

// T7 — the three service-domain acts an org admin will ask about are in the audit log, each
// written IN the transaction that performed the act.
func TestServiceActsAreAudited(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	auditCount := func(action string) int {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", action, err)
		}
		return n
	}

	// Declaration PUT (the UI path), CAS'd against the fixture's own revision.
	var current int64
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE(max(revision), 0) FROM service_definition_revisions WHERE service_id = $1`,
		f.serviceID).Scan(&current); err != nil {
		t.Fatalf("read revision: %v", err)
	}
	before := auditCount("service.declaration_put")
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, current, DeclarationOptions{CreatedBy: "op@example"}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if got := auditCount("service.declaration_put"); got != before+1 {
		t.Fatalf("declaration audit rows %d → %d, want +1", before, got)
	}

	// Window archive.
	starts := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Minute)
	w, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: starts, EndsAt: starts.Add(time.Hour), Reason: "audited",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("create window: %v", err)
	}
	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := auditCount("maintenance.window_archived"); got != 1 {
		t.Fatalf("archive audit rows = %d, want 1", got)
	}

	// Service delete.
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "audited-svc", Name: "Audited"})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	if err := st.DeleteService(ctx, f.projectID, svc.ID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	if got := auditCount("service.deleted"); got != 1 {
		t.Fatalf("delete audit rows = %d, want 1", got)
	}
}

// F1/U1 — an in-flight repair batch cannot commit a fact computed under a span the archive
// just cut, PROVEN AT THE FACT. The barrier is LATE by construction: spans are read per
// bucket immediately before that bucket's fact upsert, so holding the FACT ROW of a
// post-cancel bucket parks the batch AFTER it has read the OLD span for exactly the bucket
// whose exclusion the archive removes. The archive lands while it waits; without the
// generation bump the released batch commits the STALE exclusion — the assertion is the
// fact's value, not the range's state.
func TestArchiveFencesAnInFlightRepairBatch(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	starts := domain.FloorToBucket(time.Now().UTC().Add(-10 * time.Minute))
	ends := starts.Add(30 * time.Minute) // extends into the future: recompute is not horizon-bounded
	w, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: starts, EndsAt: ends, Reason: "to be cut mid-batch",
	}, "", monthRetention)
	if err != nil {
		t.Fatalf("create window: %v", err)
	}

	// Run the enqueued repair to completion FIRST: every bucket now has a fact row (the
	// future ones excluded under the window), so a fact row exists to hold hostage.
	first, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("first claim: %v ok=%v", err, ok)
	}
	if err := st.runRepairRangeOn(ctx, st.pool, first, time.Now().Add(30*time.Second), standaloneLifecycle); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// F is a bucket well AFTER the coming cancel instant (~now): excluded today, and the
	// exact bucket whose exclusion the archive is about to remove.
	fBucket := domain.FloorToBucket(time.Now().UTC().Add(10 * time.Minute))
	excludedAt := func(bucket time.Time) int64 {
		var us int64
		if err := st.pool.QueryRow(ctx,
			`SELECT excluded_us FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
			f.serviceID, bucket).Scan(&us); err != nil {
			t.Fatalf("read fact at %s: %v", bucket, err)
		}
		return us
	}
	if excludedAt(fBucket) == 0 {
		t.Fatal("construction is wrong: F is not excluded before the archive")
	}

	// A second recompute over the same span, parked at F's fact upsert — AFTER F's span read.
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, starts, ends, ReasonAdmin); err != nil {
		t.Fatalf("enqueue recompute: %v", err)
	}
	claimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	genBefore := claimed.Generation

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx,
		`SELECT 1 FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2 FOR UPDATE`,
		f.serviceID, fBucket); err != nil {
		t.Fatalf("hold F's fact row: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- st.runRepairRangeOn(ctx, st.pool, claimed, time.Now().Add(15*time.Second), standaloneLifecycle)
	}()
	waitForQueryLockWait(t, ctx, st, "INSERT INTO service_reliability_buckets")

	// The archive lands while the batch holds F's OLD (still-excluded) span reading.
	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("interrupted batch surfaced an error instead of a release: %v", err)
	}

	// Capture the fence's mechanics NOW (asserted last, so the mutant dies on the FACT).
	var state, lastError string
	var gen int64
	if err := st.pool.QueryRow(ctx,
		`SELECT state, last_error, maintenance_generation FROM service_repair_ranges WHERE id=$1`,
		claimed.ID).Scan(&state, &lastError, &gen); err != nil {
		t.Fatalf("reread range: %v", err)
	}

	// Drive the repair queue to quiescence: the fenced flow re-arms and reruns with the
	// truncated span; the unfenced (mutant) flow has already completed with the stale one
	// and leaves NOTHING to rerun — its stale fact is the final state.
	for {
		next, ok, err := st.claimRepairRangeOn(ctx, st.pool)
		if err != nil {
			t.Fatalf("drain claim: %v", err)
		}
		if !ok {
			break
		}
		if err := st.runRepairRangeOn(ctx, st.pool, next, time.Now().Add(30*time.Second), standaloneLifecycle); err != nil {
			t.Fatalf("drain run: %v", err)
		}
	}

	// THE assertion the round demanded — the right/wrong FACT at quiescence. Without the
	// generation bump the stale batch committed and nothing re-ran: F stays excluded here.
	if got := excludedAt(fBucket); got != 0 {
		t.Fatalf("bucket F carries %dus of exclusion computed under the pre-archive span", got)
	}
	if got := excludedAt(starts); got == 0 {
		t.Fatal("time the window genuinely covered lost its exclusion")
	}

	// And the mechanics that delivered it: the interrupted batch was superseded and
	// re-armed past the archive's generation, not completed.
	if state != "pending" || !strings.Contains(lastError, "superseded") {
		t.Fatalf("state=%q last_error=%q: the stale batch was not fenced", state, lastError)
	}
	if gen <= genBefore {
		t.Fatalf("generation %d not advanced past %d by the archive", gen, genBefore)
	}
}

// F4 (round 2/2 P1) — migration 00065's BACKFILL survives two pre-upgrade monitors sharing a
// long digit-only name. The runtime slug loop was fixed in T6; the migration's own loop still
// built base(63) || '-' || uuid4 = 68 chars, and the shape constraint added later in the same
// Up rejected the whole upgrade. The fixture recreates the pre-upgrade shape — slug NULL, no
// constraint — and executes the REAL Up section.
func TestMigration00065BackfillSurvivesLongDuplicateNames(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	name := "7" + strings.Repeat("6", 60)
	m1, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: name, Type: domain.MonitorHTTP, Target: "https://m1.example",
	})
	if err != nil {
		t.Fatalf("m1: %v", err)
	}
	m2, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: name, Type: domain.MonitorHTTP, Target: "https://m2.example",
	})
	if err != nil {
		t.Fatalf("m2: %v", err)
	}

	// Back to the pre-upgrade shape: no NOT NULL, no shape constraint, slugs unset.
	for _, q := range []string{
		`ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_slug_shape`,
		`ALTER TABLE monitors ALTER COLUMN slug DROP NOT NULL`,
		`UPDATE monitors SET slug = NULL WHERE id IN ($1, $2)`,
	} {
		if _, err := st.pool.Exec(ctx, q, m1.ID, m2.ID); err != nil {
			if !strings.Contains(q, "$1") {
				if _, err2 := st.pool.Exec(ctx, q); err2 != nil {
					t.Fatalf("pre-upgrade shape %q: %v", q, err2)
				}
				continue
			}
			t.Fatalf("pre-upgrade shape %q: %v", q, err)
		}
	}

	raw, err := migrationsFS.ReadFile("migrations/00065_monitor_slug.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	if _, err := st.pool.Exec(ctx, up); err != nil {
		t.Fatalf("the real Up path bricks on the duplicate long name: %v", err)
	}

	for _, id := range []string{m1.ID, m2.ID} {
		var slug string
		if err := st.pool.QueryRow(ctx, `SELECT slug FROM monitors WHERE id=$1`, id).Scan(&slug); err != nil {
			t.Fatalf("read slug: %v", err)
		}
		if !domain.ValidMonitorSlug(slug) {
			t.Fatalf("backfilled slug %q violates the shape", slug)
		}
	}
}

// F6 (round 2/2 P1) — the maintenance interval is half-open at BOTH live gates. Inside one
// transaction now() is the transaction timestamp, so the boundary can be pinned EXACTLY:
// a window whose effective end equals now() is over, not still suppressing.
func TestMaintenanceEndsExactlyAtItsEffectiveEnd(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only construction

	// ends_at = now() of THIS transaction: the exact boundary, deterministic.
	if _, err := tx.Exec(ctx,
		`INSERT INTO maintenance_windows (project_id, monitor_id, starts_at, ends_at, reason)
		 VALUES ($1, $2, now() - interval '1 hour', now(), 'boundary')`,
		f.projectID, f.http); err != nil {
		t.Fatalf("insert boundary window: %v", err)
	}
	inMaint, err := monitorInMaintenanceOn(ctx, tx, f.http)
	if err != nil {
		t.Fatalf("in maintenance: %v", err)
	}
	if inMaint {
		t.Fatal("a window whose effective end IS now() still suppresses: the interval closed at the end")
	}

	// One microsecond of remaining effect flips it — the boundary, not the whole window.
	if _, err := tx.Exec(ctx,
		`UPDATE maintenance_windows SET ends_at = now() + interval '1 microsecond'
		  WHERE project_id = $1 AND reason = 'boundary'`, f.projectID); err != nil {
		t.Fatalf("nudge end: %v", err)
	}
	inMaint, err = monitorInMaintenanceOn(ctx, tx, f.http)
	if err != nil {
		t.Fatalf("in maintenance after nudge: %v", err)
	}
	if !inMaint {
		t.Fatal("a window with remaining effect does not suppress; the boundary fix overshot")
	}
}

// F5 (round 2/2 P1) — the sampler's lag is clamped at zero: a freshly adopted service's
// era_start is CEILED to the next bucket, a legitimately future instant, and a negative lag
// would wreck the max() the gauge exists for.
func TestWatermarkLagClampsAtZeroForAFreshAdoption(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_materialization
		    SET sealed_through = NULL, era_start = now() + interval '1 minute'
		  WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("construct fresh adoption: %v", err)
	}
	stat, err := st.ServiceReliabilityStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stat.WatermarkLagSeconds < 0 {
		t.Fatalf("lag %.3f is negative: a future era_start leaked through the clamp", stat.WatermarkLagSeconds)
	}
	if stat.WatermarkLagSeconds != 0 {
		t.Fatalf("lag %.3f, want exactly 0 for the single fresh service", stat.WatermarkLagSeconds)
	}
}

// serviceEventsRecorder captures the commit-independent §21 events for assertions.
type serviceEventsRecorder struct {
	outcomes   map[string]int64 // "outcome/reason" → count
	rejections int64
}

func (r *serviceEventsRecorder) RecordServiceRepairOutcome(outcome, reason string) {
	if r.outcomes == nil {
		r.outcomes = map[string]int64{}
	}
	r.outcomes[outcome+"/"+reason]++
}
func (r *serviceEventsRecorder) RecordServiceUnrecomputableRejection() { r.rejections++ }

// U3/V — the §21 events are honest about their transactions. Commit-independent events
// (lifecycle outcomes with reason attribution, mutation rejections) go through the sink;
// in-transaction events (epoch fan-out, late arrivals) bump the persisted aggregate WITH the
// owning transaction, so a rollback takes the delta with it.
func TestServiceEventCountersFireAtTheirSites(t *testing.T) {
	st, ctx := declStore(t)
	rec := &serviceEventsRecorder{}
	st.WithServiceEvents(rec)

	f, base := sealedService(t, st, ctx)
	eventTotal := func(kind string) int64 {
		var v int64
		if err := st.pool.QueryRow(ctx,
			`SELECT COALESCE((SELECT value FROM service_metric_events WHERE kind=$1), 0)`, kind).Scan(&v); err != nil {
			t.Fatalf("read event %s: %v", kind, err)
		}
		return v
	}
	if eventTotal("epoch_fanout") == 0 {
		t.Fatal("declaring a service created an epoch and no persisted fan-out event")
	}

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(2*domain.CanonicalBucket), ReasonAdmin); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := st.runRepairRangeOn(ctx, st.pool, claimed, time.Now().Add(30*time.Second), standaloneLifecycle); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.outcomes["complete/admin"] == 0 {
		t.Fatalf("a completed admin range recorded no reason-attributed outcome: %v", rec.outcomes)
	}

	// A heartbeat behind the seal is a late-arrival event — through the HISTORICAL path,
	// which is where late data actually enters (the ordered ingest ignores out-of-order).
	if _, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: base.Add(20 * time.Second), Up: true},
	}); err != nil {
		t.Fatalf("historical: %v", err)
	}
	if eventTotal("late_arrivals") == 0 {
		t.Fatal("a heartbeat behind the seal recorded no persisted late-arrival event")
	}

	// The rejection counter counts the MUTATION refusal (ErrUnrecomputableRange), not a
	// parked repair row: a retroactive preview under a raw floor that already ate the range.
	if _, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), time.Minute, "op"); !errors.Is(err, ErrUnrecomputableRange) {
		t.Fatalf("preview under a spent raw floor: %v", err)
	}
	if rec.rejections != 1 {
		t.Fatalf("rejections = %d, want 1 for one refused mutation", rec.rejections)
	}

	// ROLLBACK-NEGATIVE: an aggregate delta dies with its transaction — the event counter
	// cannot report an epoch or a late arrival that did not durably happen.
	before := eventTotal("epoch_fanout")
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := bumpMetricEventTx(ctx, tx, metricEventEpochFanout, 1); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := eventTotal("epoch_fanout"); got != before {
		t.Fatalf("epoch fan-out %d → %d across a rollback, want unchanged", before, got)
	}
}

// helper: the pre-attach staging phase, executed manually so tests can stop "mid-crash"
// exactly between the long copy and the fenced cutover.
func stageMonthCopy(t *testing.T, st *Store, ctx context.Context, name string, month time.Time) {
	t.Helper()
	next := month.AddDate(0, 1, 0)
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (LIKE service_reliability_buckets INCLUDING ALL)`, name),
		fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s_month_chk CHECK (bucket_start >= '%s' AND bucket_start < '%s')`,
			name, name, month.Format(pgTimestamp), next.Format(pgTimestamp)),
		fmt.Sprintf(`INSERT INTO %s (%s)
			SELECT %s FROM service_reliability_buckets_default
			 WHERE bucket_start >= '%s' AND bucket_start < '%s'
			ON CONFLICT (service_id, bucket_start) DO UPDATE SET %s`,
			name, factColumns, factColumns, month.Format(pgTimestamp), next.Format(pgTimestamp), factUpsertSet),
	} {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("stage copy: %v", err)
		}
	}
}

// W (iter-0135, P0) — the parent copy stays AUTHORITATIVE through every adoption phase, and
// an interrupted adoption RESUMES instead of vanishing behind CREATE IF NOT EXISTS. The
// previous design DELETEd batches into an unattached staging table: parent reads saw holes
// mid-adoption, and after a crash the next cadence's CREATE ... IF NOT EXISTS saw the
// standalone table, printed "already exists, skipping", returned success, and attached
// nothing — Ensure healthy forever, facts invisible forever.
func TestAdoptionKeepsFactsVisibleAndResumes(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 10, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset partition: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	insertFact := func(bucket time.Time) {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO service_reliability_buckets
			   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
			 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
			f.serviceID, f.projectID, epochID, bucket); err != nil {
			t.Fatalf("insert fact: %v", err)
		}
	}
	parentCount := func() int {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM service_reliability_buckets
			  WHERE service_id=$1 AND bucket_start >= $2 AND bucket_start < $3`,
			f.serviceID, future, future.AddDate(0, 1, 0)).Scan(&n); err != nil {
			t.Fatalf("parent count: %v", err)
		}
		return n
	}
	insertFact(future.Add(24 * time.Hour))
	insertFact(future.Add(48 * time.Hour))

	// "Crash" after the long copy, before any cutover: staging exists, standalone.
	stageMonthCopy(t, st, ctx, name, future)

	// READ-DURING-COPY: every fact is still visible through the parent — nothing was
	// deleted into the unattached staging.
	if got := parentCount(); got != 2 {
		t.Fatalf("parent sees %d facts mid-adoption, want 2: the copy phase hid sealed history", got)
	}
	// New traffic keeps flowing into DEFAULT while the staging sits interrupted.
	insertFact(future.Add(72 * time.Hour))

	// RESUME: Ensure detects the standalone staging (pg_inherits, not CREATE's lying
	// success), converges the copy — including the row that arrived after the "crash" —
	// and attaches.
	if err := st.EnsureServiceFactPartitions(ctx, 10); err != nil {
		t.Fatalf("resume ensure: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if stateNow != factPartitionAttached {
		t.Fatalf("the interrupted adoption did not resume to ATTACHED (state=%d)", stateNow)
	}
	if got := parentCount(); got != 3 {
		t.Fatalf("parent sees %d facts after adoption, want 3 (none lost, none duplicated)", got)
	}
	var inDefault int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets_default
		  WHERE bucket_start >= $1 AND bucket_start < $2`,
		future, future.AddDate(0, 1, 0)).Scan(&inDefault); err != nil {
		t.Fatalf("default count: %v", err)
	}
	if inDefault != 0 {
		t.Fatalf("%d rows left behind in DEFAULT after adoption", inDefault)
	}
}

// W (iter-0135, P0) — a same-key write AFTER the copy converges at the fence instead of
// aborting on a staging PK conflict or attaching stale content: the copy is an UPSERT and
// the fence re-sweeps by computed_at.
func TestAdoptionReconcilesConcurrentUpdatesAtTheFence(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 11, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset partition: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	bucket := future.Add(24 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	// A holder parks the FENCE at its parent lock (locking a partitioned parent locks the
	// partitions too, so the fence waits behind this row lock) — and while the fence waits,
	// a recompute upserts the SAME key with new content and COMMITS. Everything the long
	// phase copied is stale the instant the fence acquires; only the fence's own delta sweep
	// can reconcile it.
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below
	if _, err := holder.Exec(ctx,
		`SELECT 1 FROM service_reliability_buckets_default WHERE service_id=$1 AND bucket_start=$2 FOR UPDATE`,
		f.serviceID, bucket); err != nil {
		t.Fatalf("hold row: %v", err)
	}
	adoptDone := make(chan error, 1)
	go func() { adoptDone <- st.adoptDefaultServiceFactMonth(ctx, name, future) }()
	waitForQueryLockWait(t, ctx, st, "LOCK TABLE service_reliability_buckets")
	if _, err := holder.Exec(ctx,
		`UPDATE service_reliability_buckets_default
		    SET good_us = 60000000, unknown_us = 0, computed_at = now()
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, bucket); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	if err := <-adoptDone; err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var goodUs int64
	var home string
	if err := st.pool.QueryRow(ctx,
		`SELECT good_us, tableoid::regclass::text FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, bucket).Scan(&goodUs, &home); err != nil {
		t.Fatalf("read fact: %v", err)
	}
	if home != name {
		t.Fatalf("fact lives in %s, want %s", home, name)
	}
	if goodUs != 60000000 {
		t.Fatalf("attached fact carries stale content (good_us=%d): the fence did not reconcile", goodUs)
	}
}

// W (iter-0135, P0) — a fence that cannot cut inside its bound ABORTS BOUNDED and leaves the
// world safe: the parent stays authoritative (every fact readable), the staging stays
// standalone and resumable, and boundedness is never bought with invisibility. WHAT THIS
// EXERCISES, precisely: the held row's table-level lock parks the fence at its LOCK TABLE,
// and the 2s lock_timeout aborts it — a row lock cannot reach the DELETE, because locking a
// partitioned parent waits for every partition lock first. The 5s statement_timeout on an
// oversized DELETE is enforced by the same SET LOCAL but has no deterministic construction
// here; the claim for it is the bound's existence, not a demonstrated abort.
func TestAdoptionFenceIsBoundedAndAbortsSafely(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 12, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset partition: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	bucket := future.Add(24 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // released below
	if _, err := holder.Exec(ctx,
		`SELECT 1 FROM service_reliability_buckets_default WHERE service_id=$1 AND bucket_start=$2 FOR UPDATE`,
		f.serviceID, bucket); err != nil {
		t.Fatalf("hold row: %v", err)
	}

	started := time.Now()
	adoptErr := st.adoptDefaultServiceFactMonth(ctx, name, future)
	elapsed := time.Since(started)
	if adoptErr == nil {
		t.Fatal("the fence cut through a held row without its bound firing")
	}
	// Bounded by the fence's own statement_timeout (5s) plus scheduling headroom — never
	// the old shape's open-ended wait inside a maintenance tick.
	if elapsed > 8*time.Second {
		t.Fatalf("the blocked fence held the caller %s; the bound did not fire", elapsed)
	}
	// SAFE: the fact is still readable through the parent, and the staging is resumable.
	var visible int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, bucket).Scan(&visible); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	if visible != 1 {
		t.Fatal("the aborted fence made a sealed fact invisible")
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionStandalone {
		t.Fatalf("staging state=%d err=%v, want resumable STANDALONE", stateNow, err)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.EnsureServiceFactPartitions(ctx, 12); err != nil {
		t.Fatalf("retry ensure: %v", err)
	}
	var home string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, bucket).Scan(&home); err != nil {
		t.Fatalf("read fact: %v", err)
	}
	if home != name {
		t.Fatalf("fact lives in %s after retry, want %s", home, name)
	}
}

// V — the sampler's work is INDEX-SUPPORTED, pinned by plan: with sequential scans disabled,
// every state count resolves through its partial index and the lag minimum through 00074's
// expression index. Tiny test tables would otherwise let the planner mask a missing index.
func TestReliabilityStatsPlansAreIndexBounded(t *testing.T) {
	st, ctx := declStore(t)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	plan := func(q string) string {
		rows, err := tx.Query(ctx, "EXPLAIN "+q)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var out strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			out.WriteString(line)
			out.WriteString("\n")
		}
		return out.String()
	}
	// Each sampled state is bounded by ROWS EXAMINED: its own partial index, no residual
	// state filter over a shared index (which scans the OTHER state's rows to prove zero).
	for state, idx := range map[string]string{
		"pending": "service_repair_ranges_pending_idx",
		"running": "service_repair_ranges_running_idx",
		"error":   "service_repair_ranges_errored_idx",
	} {
		p := plan(`SELECT count(*) FROM (SELECT 1 FROM service_repair_ranges WHERE state = '` + state + `' LIMIT 1000) x`)
		if !strings.Contains(p, idx) {
			t.Fatalf("the %s count does not use %s:\n%s", state, idx, p)
		}
		if strings.Contains(p, "Filter: (state") {
			t.Fatalf("the %s count still filters state over a shared index:\n%s", state, p)
		}
	}
	lagPlan := plan(`SELECT min(COALESCE(sealed_through, era_start)) FROM service_materialization`)
	if !strings.Contains(lagPlan, "service_materialization_watermark_idx") {
		t.Fatalf("the lag minimum does not use the expression index:\n%s", lagPlan)
	}
}

// W (iter-0135) — a lifecycle outcome is recorded only when a ROW actually changed: a
// claimed range whose service was cascade-deleted commits a zero-row UPDATE, and counting it
// would report an outcome that never durably happened.
func TestALifecycleOutcomeNeedsARow(t *testing.T) {
	st, ctx := declStore(t)
	rec := &serviceEventsRecorder{}
	st.WithServiceEvents(rec)
	f := adoptedService(t, st, ctx)

	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonAdmin); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// The range vanishes under the claim — the cascade-delete race, made deterministic.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_repair_ranges WHERE id=$1`, claimed.ID); err != nil {
		t.Fatalf("delete range: %v", err)
	}
	claimed.Cursor = claimed.To // straight to the COMPLETE lifecycle write
	if err := st.runRepairRangeOn(ctx, st.pool, claimed, time.Now().Add(10*time.Second), standaloneLifecycle); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.outcomes) != 0 {
		t.Fatalf("a zero-row lifecycle write recorded outcomes: %v", rec.outcomes)
	}
}

// X (iter-0135 r2/2, P1-1) — the one-transaction deletion contract holds MID-ADOPTION: the
// staging carries the parent's foreign keys from its first pass, so deleting a service
// cascades through the staging exactly as through the parent; and rows orphaned by an OLDER
// interrupted pass (LIKE copies no foreign keys) are swept before the keys are added, so a
// ghost can neither survive as retained tenant data nor wedge the ATTACH validation forever.
func TestServiceDeletionCascadesThroughStaging(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 13, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	next := future.AddDate(0, 1, 0)
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, future.Add(24*time.Hour)); err != nil {
		t.Fatalf("strand fact: %v", err)
	}

	// An interrupted pass copied the row into a bare staging (LIKE — no foreign keys)…
	stageMonthCopy(t, st, ctx, name, future)
	// …then the CURRENT pass gives the staging its referential lifecycle.
	if err := st.ensureStagingLifecycle(ctx, name,
		future.Format(pgTimestamp), next.Format(pgTimestamp)); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}

	// Deleting the service is ONE transaction that removes its facts EVERYWHERE — the
	// staging included, via the cascade the lifecycle installed.
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	var inStaging int
	if err := st.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE service_id = $1`, name), f.serviceID).Scan(&inStaging); err != nil {
		t.Fatalf("staging count: %v", err)
	}
	if inStaging != 0 {
		t.Fatalf("%d tenant fact rows survived the service deletion inside the staging", inStaging)
	}
	// And the ATTACH converges: no ghost row fails the parent's FK validation.
	if err := st.EnsureServiceFactPartitions(ctx, 13); err != nil {
		t.Fatalf("ensure after deletion: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionAttached {
		t.Fatalf("state=%d err=%v, want ATTACHED", stateNow, err)
	}
}

// X (iter-0135 r2/2, P1-1 ghost path) — rows orphaned BEFORE the lifecycle existed are swept
// on resume, not retained and not allowed to wedge the retry.
func TestOrphanedStagingRowsAreSweptOnResume(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 14, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, future.Add(24*time.Hour)); err != nil {
		t.Fatalf("strand fact: %v", err)
	}
	// The OLD interrupted pass: bare staging, no lifecycle — then the service dies. The
	// parent cascade cannot reach the staging, so a ghost row remains.
	stageMonthCopy(t, st, ctx, name, future)
	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	var ghosts int
	if err := st.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, name)).Scan(&ghosts); err != nil {
		t.Fatalf("ghost count: %v", err)
	}
	if ghosts == 0 {
		t.Fatal("construction is wrong: no ghost row to sweep")
	}
	// Resume: the sweep removes the ghost, the lifecycle installs, the ATTACH converges.
	if err := st.EnsureServiceFactPartitions(ctx, 14); err != nil {
		t.Fatalf("resume: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionAttached {
		t.Fatalf("state=%d err=%v, want ATTACHED past the ghost", stateNow, err)
	}
	var hidden int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets WHERE service_id = $1`, f.serviceID).Scan(&hidden); err != nil {
		t.Fatalf("hidden count: %v", err)
	}
	if hidden != 0 {
		t.Fatalf("%d facts of a deleted service survived adoption", hidden)
	}
}

// X (iter-0135 r2/2, P1-2) — past work does not roll out of reach: a stranded DEFAULT month
// and a standalone staging OLDER than the current month are both discovered and adopted by
// the recovery probe, oldest first, one per pass.
func TestPastMonthsAreRecoveredAfterRollover(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	past := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -8, 0)
	name := "service_reliability_buckets_" + past.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	// A fact for a month EIGHT months back: no partition was ever seeded there, so it lands
	// in DEFAULT — and the main loop (current..ahead) will never look at it again.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, past.Add(24*time.Hour)); err != nil {
		t.Fatalf("strand past fact: %v", err)
	}
	var inDefault bool
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text = 'service_reliability_buckets_default'
		   FROM service_reliability_buckets WHERE service_id=$1 AND bucket_start=$2`,
		f.serviceID, past.Add(24*time.Hour)).Scan(&inDefault); err != nil {
		t.Fatalf("locate: %v", err)
	}
	if !inDefault {
		t.Fatal("construction is wrong: the past fact did not land in DEFAULT")
	}

	// A FUTURE standalone staging exists too — an adoption another path is mid-way through.
	// It must not SHADOW past recovery: the probe that found it first, dismissed it as "not
	// past" and returned, lost every past month for as long as the leftover existed.
	shadow := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 16, 0)
	shadowName := "service_reliability_buckets_" + shadow.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+shadowName); err != nil {
		t.Fatalf("reset shadow: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		fmt.Sprintf(`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, shadowName)); err != nil {
		t.Fatalf("shadow staging: %v", err)
	}
	defer st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+shadowName) //nolint:errcheck // hygiene

	// The ordinary pass — with the clock injected at TODAY — recovers the past month.
	if err := st.ensureServiceFactPartitionsAt(ctx, now, 2); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionAttached {
		t.Fatalf("state=%d err=%v: the past DEFAULT month was not recovered", stateNow, err)
	}

	// Rollover of an INTERRUPTED adoption: a standalone staging for another past month.
	past2 := past.AddDate(0, 1, 0)
	name2 := "service_reliability_buckets_" + past2.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name2); err != nil {
		t.Fatalf("reset2: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, past2.Add(24*time.Hour)); err != nil {
		t.Fatalf("strand past2 fact: %v", err)
	}
	stageMonthCopy(t, st, ctx, name2, past2) // interrupted before attach, then the month rolled over
	if err := st.ensureServiceFactPartitionsAt(ctx, now, 2); err != nil {
		t.Fatalf("ensure2: %v", err)
	}
	state2, err := st.factPartitionStateOf(ctx, name2)
	if err != nil || state2 != factPartitionAttached {
		t.Fatalf("state=%d err=%v: the rolled-over standalone staging was not resumed", state2, err)
	}
}

// X (iter-0135 r2/2, P1-3) — the month CHECK is ensured AND VALIDATED on every resume, not
// only at creation: a crash between CREATE and ALTER must not leave a staging whose ATTACH
// needs a full validation scan inside the fence's statement budget.
func TestMonthCheckIsEnsuredOnResume(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	_ = f

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 15, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// The crash window: CREATE landed, the ALTER never did.
	if _, err := st.pool.Exec(ctx,
		fmt.Sprintf(`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
		t.Fatalf("bare staging: %v", err)
	}
	if err := st.ensureStagingLifecycle(ctx, name,
		future.Format(pgTimestamp), future.AddDate(0, 1, 0).Format(pgTimestamp)); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	var validated bool
	if err := st.pool.QueryRow(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = $1 AND conrelid = to_regclass($2)`,
		name+"_month_chk", name).Scan(&validated); err != nil {
		t.Fatalf("the month CHECK is missing after resume: %v", err)
	}
	if !validated {
		t.Fatal("the month CHECK exists but is not validated; the fenced ATTACH would scan")
	}
	// Hygiene: this staging is never adopted here; leaving it standalone would exercise the
	// recovery probe's future-shadow path in unrelated tests.
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// X (iter-0135 r2/2, P1-4) — the adoption paths are bounded by ROWS EXAMINED via the month
// index: the keyset pick and the fenced DELETE both resolve through a bucket_start-leading
// access path on the DEFAULT child, never a full scan of unrelated history.
func TestAdoptionPlansAreMonthIndexBounded(t *testing.T) {
	st, ctx := declStore(t)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	plan := func(q string) string {
		rows, err := tx.Query(ctx, "EXPLAIN "+q)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var out strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			out.WriteString(line)
			out.WriteString("\n")
		}
		return out.String()
	}
	// The DEFAULT child's clone of the partitioned month index, resolved from the catalog:
	// asserting "no Seq Scan" alone is the rows-examined trap all over again — a PK scan
	// with a bucket_start FILTER examines every row and prints no Seq Scan.
	var monthIdx string
	if err := tx.QueryRow(ctx, `
		SELECT c.relname FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = 'service_reliability_buckets_default'::regclass
		   AND pg_get_indexdef(i.indexrelid) LIKE '%(bucket_start, service_id)%'
		 LIMIT 1`).Scan(&monthIdx); err != nil {
		t.Fatalf("the DEFAULT child carries no (bucket_start, service_id) index: %v", err)
	}
	pick := plan(`SELECT service_id FROM service_reliability_buckets_default
		 WHERE bucket_start >= '2030-01-01' AND bucket_start < '2030-02-01'
		   AND (bucket_start, service_id) > ('2030-01-01'::timestamptz, '00000000-0000-0000-0000-000000000000'::uuid)
		 ORDER BY bucket_start, service_id LIMIT 500`)
	if !strings.Contains(pick, monthIdx) {
		t.Fatalf("the copy pick does not use the month index %s:\n%s", monthIdx, pick)
	}
	del := plan(`DELETE FROM service_reliability_buckets_default
		 WHERE bucket_start >= '2030-01-01' AND bucket_start < '2030-02-01'`)
	if !strings.Contains(del, monthIdx) {
		t.Fatalf("the fenced DELETE does not use the month index %s:\n%s", monthIdx, del)
	}
}

// Y (iter-0136, P1-1) — partition ownership is EXPLICITLY UTC, independent of the session
// TimeZone. Under Asia/Baku, session-local date_trunc named December's stranded facts
// 202511: the wrong month got adopted and the real one stayed unreachable forever. The whole
// rollover scenario runs on a connection whose session zone is Baku — the reviewer's exact
// product repro shape.
func TestPastRecoveryIsSessionTimezoneProof(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	tzStore, err := Open(ctx, dsn+sep+"timezone=Asia/Baku")
	if err != nil {
		t.Fatalf("open tz store: %v", err)
	}
	defer tzStore.Close()

	now := time.Now().UTC()
	past := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -10, 0)
	name := "service_reliability_buckets_" + past.Format("200601")
	wrongName := "service_reliability_buckets_" + past.AddDate(0, -1, 0).Format("200601")
	for _, n := range []string{name, wrongName} {
		if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+n); err != nil {
			t.Fatalf("reset %s: %v", n, err)
		}
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	// The discriminating instant: the first hour of a UTC month, which the Baku session
	// zone (+04) truncates into the PREVIOUS month.
	bucket := past.Add(1 * time.Hour)
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, bucket); err != nil {
		t.Fatalf("strand past fact: %v", err)
	}

	if err := tzStore.ensureServiceFactPartitionsAt(ctx, now, 2); err != nil {
		t.Fatalf("ensure under Baku session: %v", err)
	}
	stateRight, err := tzStore.factPartitionStateOf(ctx, name)
	if err != nil || stateRight != factPartitionAttached {
		t.Fatalf("state(%s)=%d err=%v: the UTC month was not the one recovered", name, stateRight, err)
	}
	stateWrong, err := tzStore.factPartitionStateOf(ctx, wrongName)
	if err != nil {
		t.Fatalf("state wrong: %v", err)
	}
	if stateWrong != factPartitionAbsent {
		t.Fatalf("the session-zone month %s was created (state=%d): ownership is not UTC", wrongName, stateWrong)
	}
	var home string
	if err := st.pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, bucket).Scan(&home); err != nil {
		t.Fatalf("locate fact: %v", err)
	}
	if home != name {
		t.Fatalf("fact lives in %s, want the UTC month %s", home, name)
	}
}

// Y (iter-0136, P1-1) — migration 00064's seed computes its months and bounds explicitly in
// UTC: the boundary probe runs the seed's own expressions under a Baku session and asserts
// the UTC month, where the session-local shape yields the next month over.
func TestMigrationSeedExpressionsAreUTC(t *testing.T) {
	st, ctx := declStore(t)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = 'Asia/Baku'`); err != nil {
		t.Fatalf("set zone: %v", err)
	}
	// 2025-12-01 01:00+04 is 2025-11-30 21:00Z: UTC truncation says NOVEMBER; the session-
	// local shape said December.
	var seedMonth, sessionMonth string
	if err := tx.QueryRow(ctx, `
		SELECT to_char(date_trunc('month', ('2025-12-01 01:00:00+04'::timestamptz) AT TIME ZONE 'UTC')::date, 'YYYYMM'),
		       to_char(date_trunc('month', '2025-12-01 01:00:00+04'::timestamptz)::date, 'YYYYMM')`).
		Scan(&seedMonth, &sessionMonth); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if seedMonth != "202511" {
		t.Fatalf("the seed expression names month %s under Baku, want the UTC month 202511", seedMonth)
	}
	if sessionMonth == seedMonth {
		t.Fatal("construction is wrong: the instant does not discriminate the session zone")
	}
	// And the shipped migration file actually carries the UTC-projected expressions.
	raw, err := migrationsFS.ReadFile("migrations/00064_service_reliability.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if !strings.Contains(string(raw), "AT TIME ZONE 'UTC'") || !strings.Contains(string(raw), "00:00:00+00") {
		t.Fatal("migration 00064's seed lost its explicit-UTC expressions")
	}
}

// Y (iter-0136, P1-3) — the copy is PROGRESS-resumable: an interrupted pass leaves its
// progress IN the staging, the next pass resumes past it, and the already-copied prefix is
// not rewritten (its xmin is untouched) — the unconditional prefix re-upsert was a livelock
// for any month larger than one maintenance pass.
func TestAdoptionResumesPastTheCopiedPrefix(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 17, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	buckets := []time.Time{future.Add(24 * time.Hour), future.Add(48 * time.Hour), future.Add(72 * time.Hour)}
	for _, b := range buckets {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO service_reliability_buckets
			   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
			 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
			f.serviceID, f.projectID, epochID, b); err != nil {
			t.Fatalf("strand fact: %v", err)
		}
	}
	// "Pass 1 timed out mid-copy": the staging holds only the first bucket.
	next := future.AddDate(0, 1, 0)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
		t.Fatalf("bare staging: %v", err)
	}
	if err := st.ensureStagingLifecycle(ctx, name,
		future.Format(pgTimestamp), next.Format(pgTimestamp)); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM service_reliability_buckets_default
		  WHERE service_id = $1 AND bucket_start = $2`, name, factColumns, factColumns),
		f.serviceID, buckets[0]); err != nil {
		t.Fatalf("prefix copy: %v", err)
	}
	var prefixXmin string
	if err := st.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT xmin::text FROM %s WHERE service_id = $1 AND bucket_start = $2`, name),
		f.serviceID, buckets[0]).Scan(&prefixXmin); err != nil {
		t.Fatalf("prefix xmin: %v", err)
	}

	// Pass 2: resumes AFTER the prefix, finishes, attaches.
	if err := st.adoptDefaultServiceFactMonth(ctx, name, future); err != nil {
		t.Fatalf("resume adopt: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionAttached {
		t.Fatalf("state=%d err=%v, want ATTACHED", stateNow, err)
	}
	var xminNow string
	var total int
	if err := st.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT xmin::text, (SELECT count(*) FROM %s) FROM %s WHERE service_id = $1 AND bucket_start = $2`,
		name, name), f.serviceID, buckets[0]).Scan(&xminNow, &total); err != nil {
		t.Fatalf("post xmin: %v", err)
	}
	if total != len(buckets) {
		t.Fatalf("attached month holds %d rows, want %d", total, len(buckets))
	}
	if xminNow != prefixXmin {
		t.Fatalf("the copied prefix was REWRITTEN on resume (xmin %s → %s): the livelock shape", prefixXmin, xminNow)
	}
}

// Y (iter-0136, P1-2 structural) — the cutover runs through deadlineTx: ONE absolute budget
// Begin→COMMIT, never per-statement SET LOCAL timeouts, whose restart-per-statement was the
// exact re-minting this project has now hit twice.
func TestAdoptionCutoverUsesTheDeadlineMechanism(t *testing.T) {
	raw, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "begin adoption cutover")
	if i < 0 {
		t.Fatal("cutover anchor missing")
	}
	tail := src[i:]
	if !strings.Contains(tail, "newDeadlineTx(") {
		t.Error("the cutover does not run through deadlineTx")
	}
	if strings.Contains(tail[:600], "SET LOCAL statement_timeout") {
		t.Error("the cutover carries a per-statement SET LOCAL timeout again")
	}
}

// Z (iter-0136 r2/2 P1-1; artifact reworked in iter-0137) — an OVERSIZE month never burns
// doomed fences: the row gate refuses BEFORE the parent lock, every cadence, and the
// recovery path is the SHIPPED operator artifact — `cerbix adopt-fact-month`, whose store
// entry runs here against the refused month (the command line itself is end-to-end tested
// in internal/cli). No documented-but-untested psql procedure remains.
func TestOversizeMonthIsGatedAndOperatorCommandAdopts(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	prevGate := adoptionFenceRowGate
	adoptionFenceRowGate = 1 // two stranded rows = oversize
	defer func() { adoptionFenceRowGate = prevGate }()

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 18, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	next := future.AddDate(0, 1, 0)
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	for _, b := range []time.Time{future.Add(24 * time.Hour), future.Add(48 * time.Hour)} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO service_reliability_buckets
			   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
			 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
			f.serviceID, f.projectID, epochID, b); err != nil {
			t.Fatalf("strand: %v", err)
		}
	}

	// Cadence 1 and cadence 2: both refuse at the gate — the copy is warmed once, but no
	// parent lock is taken and no all-row DELETE repeats.
	for i := 0; i < 2; i++ {
		err := st.adoptDefaultServiceFactMonth(ctx, name, future)
		if err == nil {
			t.Fatal("an oversize month cut through the fence gate")
		}
		if !strings.Contains(err.Error(), "supported fence bound") {
			t.Fatalf("oversize refusal has the wrong shape: %v", err)
		}
	}
	// Facts remain fully visible via the parent throughout.
	var visible int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start >= $2 AND bucket_start < $3`,
		f.serviceID, future, next).Scan(&visible); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	if visible != 2 {
		t.Fatalf("gated month hid facts: %d visible, want 2", visible)
	}

	// The refusal names the artifact an operator will actually run.
	if err := st.adoptDefaultServiceFactMonth(ctx, name, future); err == nil ||
		!strings.Contains(err.Error(), "adopt-fact-month") {
		t.Fatalf("the oversize refusal does not point at the operator command: %v", err)
	}

	// The OPERATOR ARTIFACT: the same code path with the gate off and an operator budget —
	// including month normalization from a mid-month instant, because a human types 2027-08,
	// not a bucket boundary.
	if err := st.AdoptServiceFactMonthOperator(ctx, future.Add(37*time.Hour), 30*time.Second); err != nil {
		t.Fatalf("operator adoption: %v", err)
	}
	stateNow, err := st.factPartitionStateOf(ctx, name)
	if err != nil || stateNow != factPartitionAttached {
		t.Fatalf("state=%d err=%v after the operator command, want ATTACHED", stateNow, err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start >= $2 AND bucket_start < $3`,
		f.serviceID, future, next).Scan(&visible); err != nil {
		t.Fatalf("visibility after adoption: %v", err)
	}
	if visible != 2 {
		t.Fatalf("operator adoption lost facts: %d visible, want 2", visible)
	}
	var inDefault int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets_default
		  WHERE bucket_start >= $1 AND bucket_start < $2`, future, next).Scan(&inDefault); err != nil {
		t.Fatalf("default count: %v", err)
	}
	if inDefault != 0 {
		t.Fatalf("%d rows left in DEFAULT after the operator adoption", inDefault)
	}
	// Idempotent rerun: an attached month is a no-op success.
	if err := st.AdoptServiceFactMonthOperator(ctx, future, 30*time.Second); err != nil {
		t.Fatalf("operator rerun: %v", err)
	}
}

// Z (iter-0136 r2/2, P1-2) — a fence entered with a nearly-spent CALLER deadline refuses
// cleanly before Begin instead of telling the server it may run for seconds while the client
// net cancels in milliseconds.
func TestFenceRefusesAnExhaustedCallerTail(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 19, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO service_reliability_buckets
		   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
		 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
		f.serviceID, f.projectID, epochID, future.Add(24*time.Hour)); err != nil {
		t.Fatalf("strand: %v", err)
	}
	// Warm the staging fully so ONLY the fence remains, then hand the fence a caller whose
	// tail is already inside the scheduling tolerance.
	stageMonthCopy(t, st, ctx, name, future)
	if err := st.ensureStagingLifecycle(ctx, name,
		future.Format(pgTimestamp), future.AddDate(0, 1, 0).Format(pgTimestamp)); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	// The BUDGET is what this test is about, and it is asserted through the budget rather than
	// through the wall clock. The original shape gave the caller a 5ms context deadline and expected
	// the refusal — which is a bet that several statements finish inside 5ms. Under a loaded machine
	// they do not: the context dies first and `context deadline exceeded` surfaces from a query,
	// which is an honest answer to a spent tail and not the property under test. Measured at 2
	// failures in 12 runs with the CPUs busy, so a full -race suite loses it roughly once in fifteen
	// (iter-0158 §1).
	//
	// A tiny FENCE budget reaches the same refusal deterministically: the deadline is
	// `min(now + fenceBudget, callerDeadline - schedulingTolerance)`, so a nanosecond budget is spent
	// before the check while the caller's context stays generous and no statement is cut.
	err := st.adoptServiceFactMonth(ctx, name, future, adoptionPolicy{
		fenceBudget: time.Nanosecond, rowGate: adoptionFenceRowGate,
	})
	if err == nil {
		t.Fatal("a spent budget cut a fence")
	}
	if !errors.Is(err, errSliceBudget) {
		t.Fatalf("the exhausted budget surfaced as %v, want the budget refusal (never a net kill)", err)
	}

	// And the other half, now stated as what it can actually promise: a caller whose TAIL expires
	// mid-flight is refused WITHOUT DAMAGE. Which error surfaces depends on where the tail ran out —
	// the budget refusal if the check is reached, a context deadline if a statement was cut — and both
	// are honest. What must never happen is a partition left unusable, which the safety assertions
	// below check for whichever path this run took.
	shortCtx, cancel := context.WithDeadline(ctx, time.Now().Add(5*time.Millisecond))
	defer cancel()
	if terr := st.adoptDefaultServiceFactMonth(shortCtx, name, future); terr == nil {
		t.Fatal("a spent caller tail cut a fence")
	}
	// Everything is safe: parent authoritative, staging resumable.
	stateNow, serr := st.factPartitionStateOf(ctx, name)
	if serr != nil || stateNow != factPartitionStandalone {
		t.Fatalf("state=%d err=%v, want resumable STANDALONE", stateNow, serr)
	}
}

// Z (iter-0136 r2/2, P2) — a constraint SQUATTING on a reserved name with the wrong
// definition fails the adoption CLOSED: the sweep is not skipped, the real key's creation is
// not swallowed as a duplicate, and nothing pretends the deletion contract holds.
func TestStagingNameSquatterFailsClosed(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	_ = f

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 20, 0)
	name := "service_reliability_buckets_" + future.Format("200601")
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
		t.Fatalf("staging: %v", err)
	}
	// The squatter: a validated CHECK wearing the service-FK's reserved name.
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT %s_service_fk CHECK (bucket_size_us > 0)`, name, name)); err != nil {
		t.Fatalf("squatter: %v", err)
	}
	err := st.ensureStagingLifecycle(ctx, name,
		future.Format(pgTimestamp), future.AddDate(0, 1, 0).Format(pgTimestamp))
	if err == nil {
		t.Fatal("a name squatter with the wrong definition was accepted")
	}
	if !strings.Contains(err.Error(), "wrong definition") {
		t.Fatalf("squatter refusal has the wrong shape: %v", err)
	}
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// Z (iter-0136 r2/2, P2; harness reworked in iter-0137) — the durable evidence for the UTC
// seed is the REAL thing: a fresh database is created, migrated 00001→latest under an
// Asia/Baku session, and the seeded month partitions' actual bounds are inspected for UTC
// midnight — a marker grep would survive a semantic change; pg_get_expr of the live catalog
// cannot. Harness contract: the probe database name is UNIQUE PER RUN and created by this
// test (ownership by creation), cleanup drops only that exact name, nothing is ever
// pre-dropped, and inside the configured DB gate a privilege problem is a FAILURE — a skip
// here reported the required acceptance proof green without running it.
func TestFreshMigrationUnderNonUTCSessionSeedsUTCBounds(t *testing.T) {
	st, ctx := declStore(t)

	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL (%q): %v", dsn, err)
	}
	probeName := fmt.Sprintf("cerbix_test_utcprobe_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := st.pool.Exec(ctx, `CREATE DATABASE `+probeName); err != nil {
		t.Fatalf("create probe database %s (the migration-evidence gate requires CREATEDB): %v", probeName, err)
	}
	defer func() {
		if _, err := st.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+probeName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database %s: %v", probeName, err)
		}
	}()

	u.Path = "/" + probeName
	q := u.Query()
	q.Set("timezone", "Asia/Baku")
	u.RawQuery = q.Encode()
	probeDSN := u.String()

	if err := Migrate(ctx, probeDSN); err != nil {
		t.Fatalf("fresh migration under Baku: %v", err)
	}
	probe, err := Open(ctx, probeDSN)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()

	rows, err := probe.pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'service_reliability_buckets'::regclass
		   AND c.relname ~ '^service_reliability_buckets_[0-9]{6}$'`)
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var relname, bound string
		if err := rows.Scan(&relname, &bound); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// Rendered under the Baku session, a UTC-midnight bound displays as 04:00:00+04;
		// a session-local (broken) seed would display as midnight local.
		if !strings.Contains(bound, "04:00:00+04") {
			t.Fatalf("%s carries a non-UTC bound under a fresh Baku migration: %s", relname, bound)
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if checked == 0 {
		t.Fatal("the fresh migration seeded no month partitions to inspect")
	}
}

// Z (iter-0136 FINAL [152] P1-1.1, iter-0137) — the row gate is AUTHORITATIVE, not advisory:
// it is re-counted UNDER the parent's ACCESS EXCLUSIVE lock. The unlocked preflight alone was
// a TOCTOU — a writer committing between the count and the lock let an oversize month enter
// the all-row fence the gate claimed impossible. Interleaving is forced deterministically:
// the test holds an AEX on the parent (ONLY — partitions stay reachable, so the adoption's
// preflight and copy proceed), waits until the fence queues behind it in pg_locks, commits
// MORE rows past the gate from the lock-holding transaction, and releases.
func TestOversizeGateIsRecheckedUnderTheParentLock(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	prevGate := adoptionFenceRowGate
	adoptionFenceRowGate = 4
	defer func() { adoptionFenceRowGate = prevGate }()

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 21, 0)
	name := serviceFactPartitionName(future)
	next := future.AddDate(0, 1, 0)
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var epochID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_evaluation_epochs WHERE service_id = $1 LIMIT 1`,
		f.serviceID).Scan(&epochID); err != nil {
		t.Fatalf("find epoch: %v", err)
	}
	plant := func(exec interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, hours ...int) {
		t.Helper()
		for _, h := range hours {
			if _, err := exec.Exec(ctx,
				`INSERT INTO service_reliability_buckets
				   (service_id, project_id, epoch_id, bucket_start, bucket_size_us, unknown_us, health_unknown_us)
				 VALUES ($1, $2, $3, $4, 60000000, 60000000, 60000000)`,
				f.serviceID, f.projectID, epochID, future.Add(time.Duration(h)*time.Hour)); err != nil {
				t.Fatalf("plant: %v", err)
			}
		}
	}
	plant(st.pool, 24, 48, 72) // 3 rows: under the gate, so the unlocked preflight passes

	// The staging must exist BEFORE the parent is locked: CREATE (LIKE parent) needs an
	// AccessShare on the parent and would deadlock the interleaving we are constructing.
	stageMonthCopy(t, st, ctx, name, future)

	lockTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer lockTx.Rollback(ctx) //nolint:errcheck // committed below
	if _, err := lockTx.Exec(ctx,
		`LOCK TABLE ONLY service_reliability_buckets IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold parent lock: %v", err)
	}

	adoptErr := make(chan error, 1)
	go func() {
		gctx, gcancel := context.WithTimeout(ctx, 30*time.Second)
		defer gcancel()
		adoptErr <- st.adoptDefaultServiceFactMonth(gctx, name, future)
	}()

	// Wait for the fence to queue behind the held lock — that is AFTER its unlocked
	// preflight counted 3 <= 4 and passed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiters int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE relation = 'service_reliability_buckets'::regclass AND NOT granted`).Scan(&waiters); err != nil {
			t.Fatalf("poll waiters: %v", err)
		}
		if waiters > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fence never queued behind the held parent lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The writer the preflight could not see: two more rows, committed while the fence
	// waits, taking the month past the gate (5 > 4).
	plant(lockTx, 96, 120)
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("commit lock holder: %v", err)
	}

	err = <-adoptErr
	if err == nil {
		t.Fatal("an oversize month slipped past the gate through the preflight/lock window")
	}
	if !strings.Contains(err.Error(), "under the parent lock") {
		t.Fatalf("the refusal did not come from the under-lock re-check: %v", err)
	}
	stateNow, serr := st.factPartitionStateOf(ctx, name)
	if serr != nil || stateNow != factPartitionStandalone {
		t.Fatalf("state=%d err=%v after the refusal, want STANDALONE (rollback released the month untouched)", stateNow, serr)
	}
	var visible int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_reliability_buckets
		  WHERE service_id=$1 AND bucket_start >= $2 AND bucket_start < $3`,
		f.serviceID, future, next).Scan(&visible); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	if visible != 5 {
		t.Fatalf("parent sees %d facts after the refused fence, want all 5", visible)
	}
	// DDL hygiene: the standalone staging outlives TruncateAll and must not shadow anyone.
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// Z (iter-0136 FINAL [152] P1-2, iter-0137) — the staging lifecycle verifies foreign keys by
// their COMPLETE definition. The reviewer reproduced two shapes the previous predicate
// accepted: a name-carrying FK over the WRONG columns (deleting the actual service would not
// cascade its staging facts) and the epoch key declared DEFERRABLE INITIALLY IMMEDIATE. Both
// now fail closed, as does a correct-looking key that was added NOT VALID — it has proven
// nothing about the rows the skipped sweep would have checked.
func TestStagingFKVerificationDemandsExactDefinitions(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	_ = f

	now := time.Now().UTC()
	future := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 22, 0)
	name := serviceFactPartitionName(future)
	from, to := future.Format(pgTimestamp), future.AddDate(0, 1, 0).Format(pgTimestamp)

	cases := []struct {
		label string
		decoy string
	}{
		{"service FK over the wrong columns", fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s_service_fk
			   FOREIGN KEY (epoch_id) REFERENCES services (id) ON DELETE CASCADE`, name, name)},
		{"epoch FK initially immediate", fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s_epoch_fk
			   FOREIGN KEY (epoch_id, project_id) REFERENCES service_evaluation_epochs (id, project_id)
			   ON DELETE NO ACTION DEFERRABLE`, name, name)},
		{"correct shape but NOT VALID", fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s_service_fk
			   FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id)
			   ON DELETE CASCADE NOT VALID`, name, name)},
	}
	for _, tc := range cases {
		if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			t.Fatalf("%s: reset: %v", tc.label, err)
		}
		if _, err := st.pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
			t.Fatalf("%s: staging: %v", tc.label, err)
		}
		if _, err := st.pool.Exec(ctx, tc.decoy); err != nil {
			t.Fatalf("%s: decoy: %v", tc.label, err)
		}
		err := st.ensureStagingLifecycle(ctx, name, from, to)
		if err == nil {
			t.Fatalf("%s: accepted", tc.label)
		}
		if !strings.Contains(err.Error(), "wrong definition") {
			t.Fatalf("%s: refusal has the wrong shape: %v", tc.label, err)
		}
	}

	// Positive control: a clean staging passes, and a SECOND pass recognizes the two exact
	// keys (an over-strict predicate that fails everything would wedge every resume).
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (LIKE service_reliability_buckets INCLUDING ALL)`, name)); err != nil {
		t.Fatalf("staging: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := st.ensureStagingLifecycle(ctx, name, from, to); err != nil {
			t.Fatalf("pass %d over real keys: %v", i+1, err)
		}
	}
	if _, err := st.pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
