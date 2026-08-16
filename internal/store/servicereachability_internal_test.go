package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// PRODUCT-PATH reachability (func-service-reliability §10.5).
//
// Every other test in this subsystem calls MaterializeServiceRange, or inserts the
// service_materialization row itself, and they all passed while the feature produced nothing
// in production: no code path outside a test ever created that row or asked for a bucket to
// be computed. The whole subsystem was correct and unreachable.
//
// These tests are therefore forbidden from calling any materialization entry point directly.
// They may use ONLY what the product uses: the declaration write the HTTP handler calls, the
// result-recording the ingest pipeline calls, and the leader slice the scheduler calls.

// leaderSliceFor drives the service queue exactly as the scheduler's sub-tick does — on a
// real lock-owning session, not through the pool.
func leaderSliceFor(t *testing.T, st *Store, ctx context.Context, rounds int) {
	t.Helper()
	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("leader session: %v", err)
	}
	if !ok {
		t.Fatal("could not take leadership in a test that owns the database")
	}
	defer ls.Release()
	for i := 0; i < rounds; i++ {
		worked, err := ls.RunServiceSlice(ctx, time.Now().Add(2*time.Second))
		if err != nil {
			t.Fatalf("service slice %d: %v", i, err)
		}
		if !worked {
			return
		}
	}
}

// The one that would have caught the whole defect: declare a service through the ordinary
// write path, record ordinary heartbeats, run the ordinary leader slice, and require FACTS
// and a MOVING WATERMARK. Nothing here touches materialization by name.
func TestDeclaringAServiceMakesItMaterializeWithoutAnyoneAskingTwice(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	// Adopt a short stretch of history so the buckets under test are already past their
	// accounting grace when the driver reaches them.
	from := time.Now().UTC().Add(-30 * time.Minute)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: from}); err != nil {
		t.Fatalf("declare: %v", err)
	}

	// A declaration ALONE must put the service on the driver's list. If this row is missing,
	// nothing downstream can ever run.
	var start time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT materialization_start FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&start); err != nil {
		t.Fatalf("the declaration did not start materialization: %v", err)
	}

	// Ordinary heartbeats over the adopted stretch, through the ingest path a prober uses.
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	for i := 0; i < 10; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+15*time.Second), true)
	}

	leaderSliceFor(t, st, ctx, 40)

	// Facts exist for the observed buckets…
	for i := 0; i < 10; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		fact, ok := readFact(t, st, ctx, f.serviceID, at)
		if !ok {
			t.Fatalf("no fact for bucket %s — the scheduler slice produced nothing", at)
		}
		if fact.state != "sealed" {
			t.Errorf("bucket %s is %q, want sealed (it is well past the grace)", at, fact.state)
		}
		if fact.good == 0 {
			t.Errorf("bucket %s recorded no good time from an UP observation", at)
		}
	}

	// …and the watermark moved past them. This is the assertion the shipped E2E inverted:
	// it accepted "not materialized yet" as a legitimate resting state when it was in fact
	// the only state the system could reach.
	through := sealedThrough(t, st, ctx, f.serviceID)
	if through == nil {
		t.Fatal("sealed_through is still NULL after the leader ran; the watermark never advances in production")
	}
	if !through.After(base.Add(9 * time.Minute)) {
		t.Errorf("sealed_through = %s, want past the last observed bucket %s", *through, base.Add(9*time.Minute))
	}
}

// The driver's cursor is PROGRESS and the watermark is EVIDENCE. A service that declared no
// reliability inputs for a stretch produces no facts there, and the two must disagree: the
// watermark stops at the gap, the cursor walks on. Conflating them either invents a sealed
// window over unmeasured time or wedges the driver on the same bucket forever.
func TestProgressWalksPastAGapTheWatermarkRefusesToCross(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	from := time.Now().UTC().Add(-30 * time.Minute)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: from}); err != nil {
		t.Fatalf("declare: %v", err)
	}

	// Observations only in a LATER window, so the earlier adopted stretch has no evidence.
	base := domain.FloorToBucket(time.Now().UTC().Add(-10 * time.Minute))
	for i := 0; i < 5; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}

	leaderSliceFor(t, st, ctx, 60)

	var cursor time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT materialized_through FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&cursor); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if !cursor.After(base) {
		t.Errorf("materialized_through = %s, want past %s — the driver wedged on the empty stretch", cursor, base)
	}
}

// A service with no reliability inputs is never put on the driver's list at all. It produces
// no facts by design, so queuing it would mean a driver walking forever over nothing.
func TestAServiceWithNoInputsIsNotOnTheDriversList(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: nil,
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	var rows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_materialization WHERE service_id=$1`, f.serviceID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Error("a service with an empty SLI was queued for materialization")
	}

	// …and declaring inputs later DOES put it on the list.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_materialization WHERE service_id=$1`, f.serviceID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Error("declaring reliability inputs did not start materialization")
	}
}

// materialization_start never moves. A later revision cannot make history begin somewhere
// else, and letting it would silently redefine what a complete window covers.
func TestMaterializationStartIsFixedByTheFirstDeclarationThatHasInputs(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	from := time.Now().UTC().Add(-2 * time.Hour)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: from}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	var first time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT materialization_start FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&first); err != nil {
		t.Fatalf("read start: %v", err)
	}

	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http, f.redis},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	var second time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT materialization_start FROM service_materialization WHERE service_id=$1`,
		f.serviceID).Scan(&second); err != nil {
		t.Fatalf("reread start: %v", err)
	}
	if !second.Equal(first) {
		t.Errorf("materialization_start moved %s -> %s on a later revision", first, second)
	}
}

// §10.4 covers agent historical backfill by name, and the shipped code went straight to the
// pool with raw inserts — so a backfilled result was invisible to the seal handshake: no
// dirty mark, no repair, and a sealed window that silently disagreed with the raw table it
// was computed from.
func TestHistoricalBackfillGoesThroughTheSealHandshake(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	ts := time.Now().UTC().Add(-25 * time.Minute)
	bucket := bucketOf(t, st, ctx, ts)
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != 0 {
		t.Fatalf("bucket already marked: %d", got)
	}

	inserted, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: ts, Up: true},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if inserted != 1 {
		t.Fatalf("inserted %d, want 1", inserted)
	}
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != 1 {
		t.Fatalf("ingest generation = %d after a historical insert, want 1", got)
	}

	// …and a row absorbed by ON CONFLICT DO NOTHING changed nothing, so it must NOT mark the
	// bucket again. The handshake is gated on actual insertion, not on having been asked.
	if _, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: ts, Up: true},
	}); err != nil {
		t.Fatalf("re-backfill: %v", err)
	}
	if got := ingestGeneration(t, st, ctx, f.serviceID, bucket); got != 1 {
		t.Errorf("ingest generation = %d after a duplicate that inserted nothing, want 1", got)
	}
}

// Evidence arriving BEHIND the watermark makes a sealed number wrong. The mark alone cannot
// fix it — ordinary materialization refuses to touch a sealed bucket and the driver has
// already walked past — so the correction has to be queued with the heartbeat that caused it.
func TestLateDataBehindTheWatermarkQueuesItsOwnRepair(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	// Seal a stretch through the product path.
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	for i := 0; i < 5; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}
	leaderSliceFor(t, st, ctx, 60)
	through := sealedThrough(t, st, ctx, f.serviceID)
	if through == nil {
		t.Fatal("nothing sealed; the rest of this test would prove nothing")
	}

	// A result for a bucket well inside the sealed window arrives now.
	late := base.Add(2*time.Minute + 30*time.Second)
	if !late.Before(*through) {
		t.Fatalf("test setup: %s is not behind the watermark %s", late, *through)
	}
	if _, _, err := st.RecordHistoricalResults(ctx, []domain.Heartbeat{
		{MonitorID: f.http, Ts: late, Up: false},
	}); err != nil {
		t.Fatalf("late backfill: %v", err)
	}

	var queued int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges
		  WHERE service_id=$1 AND reason='late_data' AND state='pending'`,
		f.serviceID).Scan(&queued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued == 0 {
		t.Fatal("late data behind the watermark queued no repair; the sealed number stays wrong forever")
	}

	// And running the queue actually changes it.
	before, _ := readFact(t, st, ctx, f.serviceID, domain.FloorToBucket(late))
	drainRepair(t, st, ctx)
	after, _ := readFact(t, st, ctx, f.serviceID, domain.FloorToBucket(late))
	if after.bad == before.bad {
		t.Errorf("the sealed bucket still reports bad_us=%d after a DOWN result arrived late", after.bad)
	}
}

// The fan-out cap must be refused where it is EXCEEDED, not enforced later by a kill switch.
//
// It lived only inside noteHeartbeatForServices, which runs in the HEARTBEAT's transaction.
// So the 26th declaration of a monitor was accepted happily and then broke ingest for that
// monitor: a service-configuration change took down core monitoring, and the error surfaced
// nowhere near the write that caused it.
func TestExceedingTheFanOutCapIsRefusedAtTheDeclarationNotAtIngest(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	// Fill the cap with services that all declare the same monitor as their SLI.
	for i := 0; i < DefaultMaxServicesPerMonitor; i++ {
		svc, err := st.CreateService(ctx, domain.Service{
			ProjectID: f.projectID, Slug: fmt.Sprintf("fanout-%d", i), Name: "Fan-out",
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, svc.ID, domain.ServiceDeclaration{
			Monitors: []string{f.http}, SLI: []string{f.http},
		}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
			t.Fatalf("declare %d: %v", i, err)
		}
	}

	// One more must be REFUSED…
	over, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "fanout-over", Name: "Over"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, err = st.PutServiceDeclaration(ctx, f.projectID, over.ID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if !errors.Is(err, ErrMonitorInTooManyServices) {
		t.Fatalf("got %v, want ErrMonitorInTooManyServices", err)
	}

	// …and, the part that actually mattered: the monitor still ingests.
	out := beat(t, st, ctx, f.http, time.Now().UTC().Add(-30*time.Second), true)
	if !out.Inserted {
		t.Fatal("a heartbeat was lost after the cap was reached; a service edit must never break core monitoring")
	}
}

// A declaration cannot name an unbounded evaluation context: the epoch snapshots every member
// and the reducer's breakpoint set grows with them.
func TestAnOversizedDeclarationIsRefused(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	huge := make([]string, DefaultMaxMembersPerRevision+1)
	for i := range huge {
		huge[i] = f.http
	}
	// Distinct ids are what the cap counts, so build them from real monitors where possible
	// and fall back to the same one — dedup happens before the check, so use unique strings.
	for i := range huge {
		huge[i] = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}
	_, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: huge, SLI: nil,
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if !errors.Is(err, ErrTooManyMembers) {
		t.Fatalf("got %v, want ErrTooManyMembers", err)
	}
}

// Deleting a monitor a service declares must REMOVE ITS REACH, as a declaration in its own
// right — not fail, and not silently drop the reference.
//
// The deferred FK on service_member_refs fired at COMMIT and rejected the delete outright,
// including the ordinary case of an operator removing a UI monitor from a UI service. §19.30
// requires the §15.1 matrix, and none of it existed.
func TestDeletingADeclaredMonitorRetiresItAsASystemRevision(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	before, _, _, revBefore, err := currentDeclarationTx2(t, st, ctx, f.serviceID)
	if err != nil {
		t.Fatalf("read declaration: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("fixture declares %d monitors, want 2", len(before))
	}

	// The redis monitor is in the CONTEXT but not the SLI; deleting it must still work and
	// must still be recorded.
	if err := st.DeleteMonitor(ctx, f.redis); err != nil {
		t.Fatalf("deleting a declared monitor was refused: %v", err)
	}

	monitors, sli, _, revAfter, err := currentDeclarationTx2(t, st, ctx, f.serviceID)
	if err != nil {
		t.Fatalf("reread declaration: %v", err)
	}
	if revAfter != revBefore+1 {
		t.Errorf("revision %d -> %d; the change to what the service measures was not declared", revBefore, revAfter)
	}
	for _, id := range append(append([]string{}, monitors...), sli...) {
		if id == f.redis {
			t.Fatal("the deleted monitor is still declared")
		}
	}
	if len(monitors) != 1 || len(sli) != 1 {
		t.Errorf("monitors=%v sli=%v — the rewrite changed more than the one member it had to", monitors, sli)
	}

	// The consequence is on the record, and says it was a consequence.
	var author string
	if err := st.pool.QueryRow(ctx,
		`SELECT created_by FROM service_definition_revisions
		  WHERE service_id=$1 ORDER BY revision DESC LIMIT 1`, f.serviceID).Scan(&author); err != nil {
		t.Fatalf("read author: %v", err)
	}
	if author != "system:monitor-deleted" {
		t.Errorf("author = %q, want the system attribution", author)
	}
	var audits int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action='service.member_retired' AND target LIKE '%'||$1||'%'`,
		f.redis).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits == 0 {
		t.Error("a service's declared inputs shrank with no audit row")
	}
}

// A monitor no service declares deletes exactly as before: zero services costs nothing.
func TestDeletingAnUndeclaredMonitorIsUnaffected(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	if err := st.DeleteMonitor(ctx, f.synthetic); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var revisions int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_definition_revisions WHERE service_id=$1`, f.serviceID).Scan(&revisions); err != nil {
		t.Fatalf("count: %v", err)
	}
	if revisions != 1 {
		t.Errorf("%d revisions after deleting a monitor no service declares, want 1", revisions)
	}
}

// currentDeclarationTx2 reads the declaration in force through the pool, for assertions.
func currentDeclarationTx2(t *testing.T, st *Store, ctx context.Context, serviceID string) (
	monitors, sli []string, policies domain.ServicePolicies, revision int64, err error,
) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return nil, nil, policies, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	return currentDeclarationTx(ctx, tx, serviceID)
}
