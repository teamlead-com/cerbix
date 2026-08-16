package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// Round-3 regressions (iter-0127). Each of these fails on the iter-0126 code, and each was
// re-applied as a mutant to prove it.

const monthRetention = 30 * 24 * time.Hour

// cadenceLimit is the NORMATIVE §10.10 assertion: the slice budget plus
// max_scheduling_tolerance, and NOTHING else — the pass threshold, verbatim. The environment
// is not in it: an uncapped runtime sample added to the bound can grow under contention until
// it green-lights the very regression under test, which is the fixed-150ms failure mode with
// extra steps. What the environment costs is measured and LOGGED beside the contract by
// measuredOverhead, as a diagnostic for reading a failure, never as slack for passing.
func cadenceLimit(budget time.Duration) time.Duration {
	return budget + schedulingTolerance
}

// measuredOverhead samples what THIS environment spends around one bounded round trip — the
// same BEGIN / SET LOCAL pair / statement / COMMIT shape the slices use. DIAGNOSTIC ONLY: it
// is logged next to the contract so a failure can be read (was that the code or a loaded
// pool?), and it is never added to a pass threshold. The floor is one scheduler quantum, so
// a suspiciously quiet sample still reads as a real number.
func measuredOverhead(t *testing.T, ctx context.Context, st *Store) time.Duration {
	t.Helper()
	worst := 5 * time.Millisecond
	for i := 0; i < 5; i++ {
		started := time.Now()
		if err := boundedLifecycleExec(ctx, st.pool, time.Now().Add(lifecycleWriteBound), `SELECT 1`); err != nil {
			t.Fatalf("overhead probe: %v", err)
		}
		if d := time.Since(started); d > worst {
			worst = d
		}
	}
	return worst
}

// P0-1 — a retroactive window ENTIRELY INSIDE one bucket must still hit the preview gate.
//
// `bucket_start >= from AND bucket_start < to` counted zero buckets for [10:00:30, 10:00:45),
// so the mutation needed no token — and the invalidation then FLOORED to 10:00 and rewrote
// exactly the sealed bucket the gate exists to guard.
func TestASubMinuteRetroactiveWindowStillNeedsAPreview(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	sub := domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base.Add(30 * time.Second), EndsAt: base.Add(45 * time.Second), Reason: "sub-minute",
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, sub, "", monthRetention); !errors.Is(err, ErrRetroactiveNeedsPreview) {
		t.Fatalf("a sub-minute window over a sealed bucket needed no preview: %v", err)
	}

	// The annul side has the same boundary. Seed a sub-minute window, then annul it.
	w, err := st.createMaintenanceWindowUnchecked(ctx, sub)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, "", monthRetention); !errors.Is(err, ErrRetroactiveNeedsPreview) {
		t.Fatalf("a sub-minute annul over a sealed bucket needed no preview: %v", err)
	}

	// …and the tokened path works end to end, so the gate is a gate and not a wall: the
	// preview sees the bucket (interval overlap on the BEFORE side too) and the confirm runs.
	p, err := st.PreviewMutationOf(ctx, f.projectID, f.http, w.ID, MutationAnnul,
		sub.StartsAt, sub.EndsAt, monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("%d services, want 1", len(p.Services))
	}
	svc := p.Services[0]
	// BOTH sides conserve to the FIFTEEN seconds that were asked about — not to the sixty of
	// the bucket the window happens to live inside. This is the payload's own contract, and
	// the previous implementation violated it for every sub-bucket request.
	span := int64(15 * time.Second / time.Microsecond)
	for _, side := range []struct {
		name string
		a    ServiceAggregate
	}{{"before", svc.Before}, {"after", svc.After}} {
		if got := side.a.Good + side.a.Bad + side.a.Unknown + side.a.Excluded; got != span {
			t.Errorf("%s availability sums to %dus, want the requested %dus", side.name, got, span)
		}
		if got := side.a.Healthy + side.a.Degraded + side.a.Down + side.a.HealthUnknown + side.a.Excluded; got != span {
			t.Errorf("%s health sums to %dus, want the requested %dus", side.name, got, span)
		}
	}
	// The window is IN FORCE on the before side (it exists) and gone on the after side —
	// annulling those fifteen seconds returns them from excluded to good.
	if svc.Before.Excluded != span {
		t.Errorf("before.excluded = %d, want the whole sub-range %d", svc.Before.Excluded, span)
	}
	if svc.After.Good != span {
		t.Errorf("after.good = %d, want the whole sub-range %d", svc.After.Good, span)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, p.ID, monthRetention); err != nil {
		t.Fatalf("tokened annul: %v", err)
	}
}

// A range reaching past everything governed still conserves on both sides: time no epoch
// governs is UNKNOWN, not silently dropped — dropping it is how a gap summed to zero while
// the very same payload promised sums equal to the range.
func TestPreviewConservesAcrossUngovernedTime(t *testing.T) {
	st, ctx := declStore(t)
	f, _ := sealedService(t, st, ctx)

	// The range STRADDLES the adopted era's start: the affected set is interval-aware, so a
	// range wholly outside the service's validity would (correctly) affect nothing and there
	// would be no conservation to check. Two of these three minutes precede the era and are
	// governed by no epoch; the third is inside it.
	var eraStart time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT MIN(effective_at) FROM service_definition_revisions WHERE service_id=$1`,
		f.serviceID).Scan(&eraStart); err != nil {
		t.Fatalf("read era start: %v", err)
	}
	from := eraStart.Add(-2 * time.Minute)
	to := from.Add(3 * time.Minute)

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate, from, to, 0, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("%d services, want 1", len(p.Services))
	}
	span := to.Sub(from).Microseconds()
	svc := p.Services[0]
	for _, side := range []struct {
		name string
		a    ServiceAggregate
	}{{"before", svc.Before}, {"after", svc.After}} {
		if got := side.a.Good + side.a.Bad + side.a.Unknown + side.a.Excluded; got != span {
			t.Errorf("%s availability sums to %d over ungoverned time, want %d", side.name, got, span)
		}
	}
	// The two ungoverned minutes read UNKNOWN — not dropped, not invented.
	if svc.Before.Unknown < int64(2*time.Minute/time.Microsecond) {
		t.Errorf("ungoverned time reads as %+v, want at least the two pre-era minutes UNKNOWN", svc.Before)
	}
}

// P0-2 — the projection must carry the HEALTH axis, because a mutation can move health
// without touching availability at all. The construction: a bucket whose member is GOOD but
// DEGRADED (quorum met for availability, not for health), and a window over it. Availability
// good→excluded moves, but the diagnostic content is on the health side — a projection
// missing that axis has nothing to say about what the operator is really restating.
func TestPreviewCarriesTheHealthAxis(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("%d services", len(p.Services))
	}
	svc := p.Services[0]
	// The fixture's sealed buckets are healthy; the window excludes them on BOTH axes. If the
	// health fields stay zero on either side, the axis is not being carried at all.
	if svc.Before.Healthy == 0 {
		t.Fatalf("before.healthy = 0: the projection does not carry the health axis (%+v)", svc.Before)
	}
	if svc.After.Healthy >= svc.Before.Healthy {
		t.Errorf("healthy did not fall under the window: before %d, after %d", svc.Before.Healthy, svc.After.Healthy)
	}
	// Conservation holds on both sides of both axes — the projection is real reducer output,
	// not a copied column.
	span := int64(2 * time.Minute / time.Microsecond)
	for _, side := range []struct {
		name string
		a    ServiceAggregate
	}{{"before", svc.Before}, {"after", svc.After}} {
		if got := side.a.Good + side.a.Bad + side.a.Unknown + side.a.Excluded; got != span {
			t.Errorf("%s availability sums to %d, want %d", side.name, got, span)
		}
		if got := side.a.Healthy + side.a.Degraded + side.a.Down + side.a.HealthUnknown + side.a.Excluded; got != span {
			t.Errorf("%s health sums to %d, want %d", side.name, got, span)
		}
	}
}

// The former TestPreviewDegradesToApproximateAtItsWallBudget was superseded in round 5: with
// the budget covering the WHOLE operation, a nanosecond budget cannot even take the
// membership lock, which is a bounded honest error — and the approximate-under-exhaustion
// property it existed for is proven end to end by TestPreviewPersistsItsTokenInsideTheReserve.

// P0-4 — a token that predates the projection is not confirmable. Simulated exactly as the
// upgrade left it: a stored preview whose service rows claim projected=true with after_* at
// their defaults, but whose expiry migration 00073 applies is what kills it.
func TestPreUpgradePreviewTokensAreDead(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// Execute the migration's OWN statements, read from the embedded file — not a copy that
	// can drift. A previous version of this test hand-wrote the UPDATE, which proved the
	// test's idea of the migration rather than the migration.
	for _, stmt := range migrationStatements(t, "00073_preview_health_axis.sql", "UPDATE maintenance_preview") {
		if _, err := st.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("apply migration statement %q: %v", stmt, err)
		}
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "grandfathered",
	}, p.ID, monthRetention)
	if !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("an expired pre-upgrade token confirmed: %v", err)
	}
}

// P0-5 — a forward slice must leave the session EXACTLY as it found it, on the real
// lock-owning leader connection. A session-level SET survived the slice: the leadership
// Check, the advisory unlock and the next pool user all inherited this slice's 250ms.
func TestForwardSliceLeavesTheLeaderSessionUnpoisoned(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	from := time.Now().UTC().Add(-30 * time.Minute)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: from}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: %v ok=%v", err, ok)
	}
	released := false
	defer func() {
		if !released {
			ls.Release()
		}
	}()

	// Run REAL slices on the pinned connection, with a deadline tight enough that the
	// timeouts are set to a small value.
	for i := 0; i < 10; i++ {
		worked, err := ls.RunServiceSlice(ctx, time.Now().Add(250*time.Millisecond))
		if err != nil {
			t.Fatalf("slice %d: %v", i, err)
		}
		if !worked {
			break
		}
	}

	// The session's timeouts are the defaults again. If a slice's SET leaked, everything
	// that runs on this connection from now on — the leadership Check included — inherits a
	// 250ms bound it never asked for.
	var stmtTimeout, lockTimeout string
	if err := ls.conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("show statement_timeout: %v", err)
	}
	if err := ls.conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&lockTimeout); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if stmtTimeout != "0" || lockTimeout != "0" {
		t.Fatalf("the slice poisoned the leader session: statement_timeout=%s lock_timeout=%s, want 0/0",
			stmtTimeout, lockTimeout)
	}

	// And the leadership machinery still works on that same session.
	held, err := ls.Check(ctx)
	if err != nil || !held {
		t.Fatalf("leadership check after slices: held=%v err=%v", held, err)
	}

	// Now the ERROR path — the branch the original hazard lived on. A blocked bucket forces
	// the slice's lock_timeout to fire and the transaction to roll back; the session must
	// STILL come out clean, and releasing it must actually free the advisory lock.
	// New evidence FIRST — the blocker below stalls ingest too, and a beat recorded under it
	// would hang, fail the test, and leave the table lock stranded for the whole package.
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(5+i)*time.Minute+10*time.Second), true)
	}
	blocker, berr := st.pool.Begin(ctx)
	if berr != nil {
		t.Fatalf("begin blocker: %v", berr)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below; defer covers a Fatalf
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_bucket_ingest IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock ingest: %v", err)
	}
	if _, err := ls.RunServiceSlice(ctx, time.Now().Add(300*time.Millisecond)); err == nil {
		t.Log("the blocked slice returned no error; only session hygiene is under test here")
	}
	_ = blocker.Rollback(ctx)
	if err := ls.conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("show after error path: %v", err)
	}
	if stmtTimeout != "0" {
		t.Fatalf("the ERROR path poisoned the leader session: statement_timeout=%s", stmtTimeout)
	}
	if held, err := ls.Check(ctx); err != nil || !held {
		t.Fatalf("leadership check after the error path: held=%v err=%v", held, err)
	}

	// Release frees the lock for a successor — the unlock ran on a healthy session.
	key := ls.key
	ls.Release()
	released = true
	successor, ok2, err := st.TryBecomeLeaderSession(ctx, key)
	if err != nil || !ok2 {
		t.Fatalf("the released lock could not be re-acquired: ok=%v err=%v", ok2, err)
	}
	successor.Release()
}

// P0-6 — starting a new era advances the DRIVER'S CURSOR with it, atomically, so a service
// reviving after a long unwalked silence does not burn one empty bucket per slice across the
// whole gap before reaching the present.
func TestStartingAnEraAdvancesTheCursorWithIt(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	// Adopt far back and NEVER run the driver, standing in for a scheduler that was down
	// through the silence: the cursor stays at the beginning.
	origin := time.Now().UTC().Add(-2 * time.Hour)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: origin}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: nil,
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("empty declaration: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 2, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("revival: %v", err)
	}

	var era, cursor, start time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT era_start, materialized_through, materialization_start
		   FROM service_materialization WHERE service_id=$1`, f.serviceID).Scan(&era, &cursor, &start); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !cursor.Equal(era) {
		t.Fatalf("materialized_through = %s, era_start = %s — the driver would walk the whole silence bucket by bucket", cursor, era)
	}
	if !start.Equal(domain.FloorToBucket(origin)) {
		t.Errorf("materialization_start moved to %s; it must keep recording the original %s", start, domain.FloorToBucket(origin))
	}
}

// P0-8 — a recompute whose evidence was destroyed leaves the sealed facts ALONE and parks
// the range as unrecomputable. Deleting a monitor cascades its heartbeats; rerunning the
// reducer then finds nothing and would write UNKNOWN over measured GOOD.
func TestRecomputeOverDestroyedEvidenceFailsClosed(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	before, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok || before.good == 0 {
		t.Fatalf("fixture did not seal a good bucket: %+v", before)
	}

	// Destroy the evidence through the product path.
	if err := st.DeleteMonitor(ctx, f.http); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	// An unrelated admin recompute over the sealed stretch.
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(3*time.Minute), ReasonAdmin); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ls, lok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !lok {
		t.Fatalf("leader: %v ok=%v", err, lok)
	}
	defer ls.Release()
	for i := 0; i < 10; i++ {
		worked, err := ls.RunServiceRepairSlice(ctx, time.Now().Add(2*time.Second))
		if !worked {
			break
		}
		if err != nil && !errors.Is(err, ErrEvidenceGone) {
			t.Fatalf("slice: %v", err)
		}
	}

	after, _ := readFact(t, st, ctx, f.serviceID, base)
	if after.good != before.good || after.state != "sealed" {
		t.Fatalf("the sealed fact was rewritten from a partial record: %+v -> %+v", before, after)
	}
	var state, lastErr string
	if err := st.pool.QueryRow(ctx,
		`SELECT state, last_error FROM service_repair_ranges
		  WHERE service_id=$1 ORDER BY updated_at DESC LIMIT 1`, f.serviceID).Scan(&state, &lastErr); err != nil {
		t.Fatalf("read range: %v", err)
	}
	if state != "error" {
		t.Fatalf("range state = %q, want the terminal error state (retrying cannot resurrect deleted heartbeats)", state)
	}
	if !strings.Contains(lastErr, "evidence") {
		t.Errorf("last_error %q does not say why", lastErr)
	}
}

// P1-6 (cap): the two writers CONCURRENTLY racing at the last slot admit exactly one.
func TestTheLastServiceSlotAdmitsExactlyOneWriter(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	_, projID := seedTenant(t, st, ctx)
	st.WithServiceLimits(ServiceLimits{ServicesPerProject: 1})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() { // the UI writer
		defer wg.Done()
		<-start
		_, errs[0] = st.CreateService(ctx, domain.Service{ProjectID: projID, Slug: "ui-one", Name: "UI"})
	}()
	dp, derr := fileprovider.Decode([]byte(svcBundle), config.ProviderScopeConfig{Type: "instance"})
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	go func() { // the bundle writer
		defer wg.Done()
		<-start
		_, errs[1] = st.ApplyFileManagedBundle(ctx, "payments-bundle", dp, "/bundles/payments.yaml", time.Hour, 100, true)
	}()
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrTooManyServices) {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d writers took the last slot, want exactly 1 (errs: %v)", succeeded, errs)
	}
}

// P1-11 — overlapping historical backfills over the SAME rows in OPPOSITE input orders must
// not deadlock: both raw inserts and service marks take their keys in one global order.
func TestOverlappingBackfillsInOppositeOrdersDoNotDeadlock(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	// BOTH monitors in the SLI, so each heartbeat marks the same service keys and the two
	// batches contend over an identical lock set.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http, f.redis},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("declare: %v", err)
	}

	// Every round inserts FRESH rows. A first draft reused one PK set across rounds, and
	// from round two on `ON CONFLICT DO NOTHING` matched existing rows and took no row locks
	// at all — twenty-four of its twenty-five rounds could not have deadlocked no matter what
	// order anything ran in, which is how the unsorted mutant survived it.
	for round := 0; round < 12; round++ {
		base := domain.FloorToBucket(time.Now().UTC().Add(-90 * time.Minute)).Add(time.Duration(round) * 5 * time.Minute)
		forward := make([]domain.Heartbeat, 0, 400)
		for i := 0; i < 200; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			forward = append(forward, domain.Heartbeat{MonitorID: f.http, Ts: ts, Up: true})
			forward = append(forward, domain.Heartbeat{MonitorID: f.redis, Ts: ts, Up: true})
		}
		backward := make([]domain.Heartbeat, len(forward))
		for i := range forward {
			backward[i] = forward[len(forward)-1-i]
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, _, errs[0] = st.RecordHistoricalResults(ctx, forward) }()
		go func() { defer wg.Done(); <-start; _, _, errs[1] = st.RecordHistoricalResults(ctx, backward) }()
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d: %v", round, i, err)
			}
		}
	}
}

// P0-2 (round 4) — the wall budget bounds the WHOLE preview, deterministically proven with a
// real lock wait: a blocker holds heartbeats hostage, the projection's statement bound fires
// inside its savepoint, and the operation still delivers the promised approximate token —
// within a bounded wall time, with no service allowed to run after the budget died.
func TestABlockedPreviewDegradesInsteadOfHoldingTheProject(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	// A SECOND affected service, so the test also proves post-exhaustion services are
	// SKIPPED rather than each burning a statement bound of their own — the accumulation
	// D-0159 forbids.
	second, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "second", Name: "Second"})
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, second.ID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: base}); err != nil {
		t.Fatalf("declare second: %v", err)
	}

	// The blocker: even plain SELECTs on heartbeats now wait, which is exactly the shape of
	// a projection statement stuck behind someone else's lock.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE heartbeats IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock heartbeats: %v", err)
	}

	st.previewBudget = 400 * time.Millisecond
	overhead := measuredOverhead(t, ctx, st)
	started := time.Now()
	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), monthRetention, "op")
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if err != nil {
		t.Fatalf("a blocked preview failed instead of degrading: %v", err)
	}
	if p.Coverage != "approximate" {
		t.Fatalf("coverage = %q, want approximate", p.Coverage)
	}
	for _, svc := range p.Services {
		if svc.Projected {
			t.Errorf("service %s claims a projection computed while its statements were blocked", svc.ServiceID)
		}
		if svc.Reason != ReasonWallBudget {
			t.Errorf("service %s reason = %q, want %q", svc.ServiceID, svc.Reason, ReasonWallBudget)
		}
	}
	// The normative §10.10 bound itself, plus this run's measured environmental overhead.
	t.Logf("contract %s, measured overhead %s", cadenceLimit(st.previewBudget), overhead)
	if limit := cadenceLimit(st.previewBudget); elapsed > limit {
		t.Fatalf("the preview took %s against the %s contract (budget %s)", elapsed, limit, st.previewBudget)
	}
	// …and the project's membership lock is free again: an ordinary write goes straight
	// through instead of queueing behind a preview that should have let go.
	if _, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "after-preview", Name: "After"}); err != nil {
		t.Fatalf("the membership lock did not come back: %v", err)
	}
}

// P1-6a (round 4) — a change that moves HEALTH and leaves availability EXACTLY where it was.
//
// Construction: two SLI members under quorum(degraded_min=1, healthy_min=2), one UP and one
// DOWN — the bucket is GOOD (quorum met) but DEGRADED (healthy_min missed). A window over the
// DOWN member excludes it, the survivor is up, healthy_min clamps to the eligible one, and
// the bucket turns HEALTHY while good/bad do not move a microsecond. Cross-wired code that
// copies availability into the health fields cannot pass: before.Healthy must be ZERO while
// before.Good is the whole range.
func TestPreviewShowsAHealthOnlyChange(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	from := time.Now().UTC().Add(-40 * time.Minute)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http, f.redis},
		Policies: domain.ServicePolicies{
			Aggregation: domain.AggregationPolicy{Mode: domain.AggQuorum, DegradedMin: 1, HealthyMin: 2},
		},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: from}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	base := domain.FloorToBucket(time.Now().UTC().Add(-30 * time.Minute))
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
		beat(t, st, ctx, f.redis, base.Add(time.Duration(i)*time.Minute+12*time.Second), false)
	}
	leaderSliceFor(t, st, ctx, 80)

	// The range starts at the SECOND bucket: sample-and-hold has both members' observations
	// in force from there, so the whole range is decided and the axes are cleanly split.
	p, err := st.PreviewMutation(ctx, f.projectID, f.redis, MutationCreate,
		base.Add(time.Minute), base.Add(3*time.Minute), monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("%d services, want 1", len(p.Services))
	}
	svc := p.Services[0]
	span := int64(2 * time.Minute / time.Microsecond)

	// GOOD but not HEALTHY: the two axes DISAGREE on the before side. This is the assertion
	// that kills cross-wiring — code copying availability into the health fields would show
	// healthy == good, and here healthy is zero while good is nearly the whole range.
	if svc.Before.Good == 0 || svc.Before.Healthy != 0 {
		t.Fatalf("before = %+v, want good time with ZERO healthy time (quorum met, healthy_min missed)", svc.Before)
	}
	if svc.Before.Degraded != svc.Before.Good {
		t.Fatalf("before = %+v: every good microsecond here is degraded, and the two counts differ", svc.Before)
	}

	// The mutation moves health and ONLY health: availability is identical to the
	// microsecond, and every degraded microsecond turned healthy.
	if svc.After.Good != svc.Before.Good || svc.After.Bad != svc.Before.Bad ||
		svc.After.Unknown != svc.Before.Unknown || svc.After.Excluded != svc.Before.Excluded {
		t.Fatalf("availability moved: %+v -> %+v; this window must not touch it", svc.Before, svc.After)
	}
	if svc.After.Healthy != svc.Before.Degraded || svc.After.Degraded != 0 {
		t.Errorf("health did not flip degraded->healthy: %+v -> %+v", svc.Before, svc.After)
	}

	// And both sides still conserve to the requested range.
	for _, side := range []struct {
		name string
		a    ServiceAggregate
	}{{"before", svc.Before}, {"after", svc.After}} {
		if got := side.a.Good + side.a.Bad + side.a.Unknown + side.a.Excluded; got != span {
			t.Errorf("%s availability sums to %d, want %d", side.name, got, span)
		}
		if got := side.a.Healthy + side.a.Degraded + side.a.Down + side.a.HealthUnknown + side.a.Excluded; got != span {
			t.Errorf("%s health sums to %d, want %d", side.name, got, span)
		}
	}
}

// migrationStatements extracts the statements containing `match` from an embedded migration's
// Up section, so a test exercises the migration's own SQL rather than a copy that can drift.
func migrationStatements(t *testing.T, file, match string) []string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatalf("read migration %s: %v", file, err)
	}
	up := string(raw)
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	// Comments go FIRST, statements second: a semicolon inside a comment sentence would
	// otherwise split the comment and leave its tail masquerading as SQL.
	var kept []string
	for _, line := range strings.Split(up, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		clean := strings.TrimSpace(stmt)
		if clean != "" && strings.Contains(clean, match) {
			out = append(out, clean)
		}
	}
	if len(out) == 0 {
		t.Fatalf("migration %s has no statement matching %q — the teeth this test relies on are gone", file, match)
	}
	return out
}

// P1-6d (round 4) — the opposite-order deadlock, DETERMINISTICALLY interleaved. A barrier
// transaction holds the middle heartbeat key; both backfills are launched and provably reach
// their lock waits (observed in pg_stat_activity) while each already holds its half of the
// key space; the barrier then releases. With one global insert order the two waits are on the
// SAME key and no cycle can exist; with input order the forward writer holds the early keys,
// the backward writer the late ones, and the release completes a cycle Postgres kills.
func TestBarrieredOppositeOrderBackfillsCannotDeadlock(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("declare: %v", err)
	}

	base := domain.FloorToBucket(time.Now().UTC().Add(-60 * time.Minute))
	const n = 41
	forward := make([]domain.Heartbeat, 0, n)
	for i := 0; i < n; i++ {
		forward = append(forward, domain.Heartbeat{MonitorID: f.http, Ts: base.Add(time.Duration(i) * time.Second), Up: true})
	}
	backward := make([]domain.Heartbeat, n)
	for i := range forward {
		backward[i] = forward[n-1-i]
	}
	middle := forward[n/2]

	// The barrier: insert and HOLD the middle key, so both writers must stop exactly there.
	barrier, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin barrier: %v", err)
	}
	defer barrier.Rollback(ctx) //nolint:errcheck // released below
	if _, err := barrier.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
		 VALUES ($1,$2,true,0,0,'',$2)`, middle.MonitorID, middle.Ts); err != nil {
		t.Fatalf("hold the middle key: %v", err)
	}

	done := make(chan error, 2)
	go func() { _, _, err := st.RecordHistoricalResults(ctx, forward); done <- err }()
	go func() { _, _, err := st.RecordHistoricalResults(ctx, backward); done <- err }()

	// Both writers are provably AT their lock waits before the barrier lifts — this is what
	// makes the interleaving deterministic rather than a race the scheduler may serialize.
	waited := false
	for i := 0; i < 200; i++ {
		var waiting int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO heartbeats%'`).Scan(&waiting); err != nil {
			t.Fatalf("watch waiters: %v", err)
		}
		if waiting >= 2 {
			waited = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !waited {
		t.Fatal("the two backfills never reached their lock waits; the barrier did not bite")
	}
	if err := barrier.Rollback(ctx); err != nil {
		t.Fatalf("release barrier: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("writer %d: %v (a deadlock here means the keys were taken in input order)", i, err)
		}
	}
}

// Round-5 (iter-0129) regressions.

// P1-4 — the affected set answers "declared the monitor INSIDE the range", not "ever". A
// service whose only SLI membership ended before the range, or began after it, is not
// affected; one whose membership begins inside it is.
func TestAffectedSetIsIntervalAware(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	// Revision 1: monitor in the SLI. Revision 2 (a minute later, backdated via direct
	// column update because effective_at is prospective by design): monitor REMOVED. The
	// service's membership therefore ENDED at rev2's boundary.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-3 * time.Hour)}); err != nil {
		t.Fatalf("rev1: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.redis}, SLI: []string{f.redis},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("rev2: %v", err)
	}
	// Backdate rev2 so the membership visibly ended two hours ago.
	cut := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_definition_revisions SET effective_at=$2 WHERE service_id=$1 AND revision=2`,
		f.serviceID, cut); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	// A range wholly AFTER the membership ended: not affected.
	after, err := servicesAffectedByWindow(ctx, tx, f.projectID, f.http,
		cut.Add(30*time.Minute), cut.Add(40*time.Minute))
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a range after the membership ended still affects the service; the set is not interval-aware")
	}

	// A range straddling the boundary: affected (the membership held for part of it).
	straddle, err := servicesAffectedByWindow(ctx, tx, f.projectID, f.http,
		cut.Add(-10*time.Minute), cut.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("straddle: %v", err)
	}
	if len(straddle) != 1 {
		t.Errorf("a range overlapping the membership does not affect the service (got %d)", len(straddle))
	}

	// The replacement monitor is affected only after ITS membership began.
	before, err := servicesAffectedByWindow(ctx, tx, f.projectID, f.redis,
		cut.Add(-40*time.Minute), cut.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("a range before the new membership began still affects the service")
	}
}

// P0-1 (round 5) — the preview's budget covers the WHOLE operation. Blocked on the very first
// thing it needs — the project's membership advisory lock — it returns within the budget plus
// tolerance, not within some floor invented for the pre-phase.
func TestAPreviewBlockedOnTheMembershipLockIsBounded(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	st.previewBudget = 500 * time.Millisecond
	overhead := measuredOverhead(t, ctx, st)

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if err := lockServiceMembership(ctx, blocker, f.projectID); err != nil {
		t.Fatalf("hold membership: %v", err)
	}

	started := time.Now()
	_, err = st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), monthRetention, "op")
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)

	// Without the lock there is no consistent affected set, so no token of any kind can be
	// minted: a bounded ERROR is the honest outcome. What is forbidden is the wait itself
	// exceeding the budget the caller was promised.
	if err == nil {
		t.Fatal("a preview minted a token without the membership lock")
	}
	// The SERVER must speak first. A refusal that arrives as context.DeadlineExceeded means
	// the client net fired before the server bound — the wait was "bounded" by killing the
	// connection, which is the exact mechanism the server-first rule exists to prevent.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the client net fired before the server bound: %v", err)
	}
	t.Logf("contract %s, measured overhead %s", cadenceLimit(st.previewBudget), overhead)
	if limit := cadenceLimit(st.previewBudget); elapsed > limit {
		t.Fatalf("blocked on the membership lock for %s against the %s contract", elapsed, limit)
	}
}

// P0-1 (round 5) — the budget reserves time for PERSISTING the token, and the persistence
// bound is re-issued after the projection savepoints: SET LOCAL issued inside a savepoint
// SURVIVES its RELEASE, so without the re-issue a successfully degraded projection inherited
// a near-zero bound and died as a 500 at the token insert.
func TestPreviewPersistsItsTokenInsideTheReserve(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	// A budget small enough that the projection exhausts it, leaving ONLY the reserve.
	st.previewBudget = previewPersistReserve + 50*time.Millisecond
	overhead := measuredOverhead(t, ctx, st)

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE heartbeats IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock heartbeats: %v", err)
	}

	started := time.Now()
	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), monthRetention, "op")
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if err != nil {
		t.Fatalf("an exhausted budget must still persist the approximate answer, got: %v", err)
	}
	if p.Coverage != "approximate" {
		t.Fatalf("coverage = %q, want approximate", p.Coverage)
	}
	// The whole operation — lock, resolve, blocked projection, persist, commit — inside the
	// normative contract.
	t.Logf("contract %s, measured overhead %s", cadenceLimit(st.previewBudget), overhead)
	if limit := cadenceLimit(st.previewBudget); elapsed > limit {
		t.Fatalf("the preview took %s against the %s contract", elapsed, limit)
	}
	// The token is REAL: committed, readable, and honestly unconfirmable.
	var coverage string
	if qerr := st.pool.QueryRow(ctx,
		`SELECT coverage FROM maintenance_previews WHERE id=$1`, p.ID).Scan(&coverage); qerr != nil {
		t.Fatalf("the approximate token was not persisted: %v", qerr)
	}
	if _, cerr := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "unshown",
	}, p.ID, monthRetention); !errors.Is(cerr, ErrPreviewApproximate) {
		t.Fatalf("an approximate token confirmed: %v", cerr)
	}
}

// P0-2 (round 5) — the forward slice's cadence: blocked or not, BEGIN→commit stays inside
// the slice deadline plus the scheduling tolerance, because the server bounds are derived
// from the remainder and always fire before the client's net.
func TestForwardSliceCadenceHoldsWhileBlocked(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-30 * time.Minute)}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_bucket_ingest IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock ingest: %v", err)
	}

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader: %v ok=%v", err, ok)
	}
	defer ls.Release()

	overhead := measuredOverhead(t, ctx, st)
	started := time.Now()
	_, serr := ls.RunServiceSlice(ctx, started.Add(250*time.Millisecond))
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if serr == nil {
		t.Log("the blocked slice returned no error; the cadence is what is under test")
	}
	t.Logf("contract %s, measured overhead %s", cadenceLimit(250*time.Millisecond), overhead)
	if limit := cadenceLimit(250 * time.Millisecond); elapsed > limit {
		t.Fatalf("BEGIN→return took %s against the %s contract", elapsed, limit)
	}
	// …and the leader's session survived the server-side refusal.
	var stmtTimeout string
	if err := ls.conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("the leader connection did not survive the blocked slice: %v", err)
	}
	if stmtTimeout != "0" {
		t.Fatalf("the slice left statement_timeout=%s on the leader session", stmtTimeout)
	}
	if held, err := ls.Check(ctx); err != nil || !held {
		t.Fatalf("leadership lost to one blocked bucket: held=%v err=%v", held, err)
	}
}

// P1-6 (round 5) — the projection bound counts TOUCHED buckets. An unaligned range touches
// one more bucket than its duration suggests, and integer division admitted budget+1.
func TestProjectionBoundCountsTouchedBuckets(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	// Wall budget large enough that range_too_long is the only reason in play for the
	// unaligned case, and small enough that the aligned case degrades fast instead of
	// walking four thousand buckets.
	st.previewBudget = previewPersistReserve + 100*time.Millisecond

	span := time.Duration(previewProjectionBudget) * domain.CanonicalBucket

	// Unaligned: duration == budget buckets, but floor(from)..ceil(to) touches budget+1.
	unaligned := base.Add(30 * time.Second)
	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		unaligned, unaligned.Add(span), monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) == 0 || p.Services[0].Reason != ReasonRangeTooLong {
		t.Fatalf("an unaligned range touching budget+1 buckets was admitted: %+v", p.Services)
	}

	// Aligned at exactly the budget: admitted (whatever reason it later degrades with, it
	// must NOT be range_too_long).
	p2, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(span), monthRetention, "op")
	if err != nil {
		t.Fatalf("aligned preview: %v", err)
	}
	if len(p2.Services) > 0 && p2.Services[0].Reason == ReasonRangeTooLong {
		t.Fatalf("a range of exactly the budget was refused as too long")
	}
}

// Round-6 (iter-0130) regressions — the lifecycle phases the previous round left outside the
// mechanism.

// P0-1 (round 6) — the PERSISTENCE of a forward slice is bounded too. A block on
// service_materialization lands exactly on the cursor write that runs in the reserve, after
// the work loop: previously that phase ran on the raw transaction with one fixed SET —
// restarted per statement — and the client net could win the race and take the leader's
// connection with it.
func TestForwardPersistenceIsBoundedAndTheSessionSurvives(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-30 * time.Minute)}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	for i := 0; i < 3; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_materialization IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock materialization: %v", err)
	}

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader: %v ok=%v", err, ok)
	}
	defer ls.Release()

	overhead := measuredOverhead(t, ctx, st)
	started := time.Now()
	_, serr := ls.RunServiceSlice(ctx, started.Add(250*time.Millisecond))
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if serr == nil {
		t.Log("the blocked slice returned no error; the cadence and the session are what matter")
	}
	if errors.Is(serr, context.DeadlineExceeded) {
		t.Fatalf("the client net beat the server bound in the persistence phase: %v", serr)
	}
	t.Logf("contract %s, measured overhead %s", cadenceLimit(250*time.Millisecond), overhead)
	if limit := cadenceLimit(250 * time.Millisecond); elapsed > limit {
		t.Fatalf("persistence blocked for %s against the %s contract", elapsed, limit)
	}
	var stmtTimeout string
	if err := ls.conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("the leader connection did not survive the blocked persistence: %v", err)
	}
	if stmtTimeout != "0" {
		t.Fatalf("persistence left statement_timeout=%s on the leader session", stmtTimeout)
	}
	if held, err := ls.Check(ctx); err != nil || !held {
		t.Fatalf("leadership lost to blocked persistence: held=%v err=%v", held, err)
	}
}

// P0-2 (round 6) — a blocked CLAIM is bounded. SKIP LOCKED dodges row waits and nothing
// else: a table lock parked the claim on the leader connection, on the root context, before
// any envelope existed.
func TestABlockedClaimIsBoundedOnTheLeaderSession(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_repair_ranges IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock queue: %v", err)
	}

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader: %v ok=%v", err, ok)
	}
	defer ls.Release()

	overhead := measuredOverhead(t, ctx, st)
	started := time.Now()
	_, serr := ls.RunServiceSlice(ctx, started.Add(250*time.Millisecond))
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if serr == nil {
		t.Log("the blocked claim surfaced no error; bounded time and a live session are the contract")
	}
	t.Logf("contract %s, measured overhead %s", cadenceLimit(250*time.Millisecond), overhead)
	if limit := cadenceLimit(250 * time.Millisecond); elapsed > limit {
		t.Fatalf("the claim blocked for %s against the %s contract", elapsed, limit)
	}
	if held, err := ls.Check(ctx); err != nil || !held {
		t.Fatalf("leadership lost to a blocked claim: held=%v err=%v", held, err)
	}
}

// P0-2 (round 6) — a blocked POST-BUDGET RELEASE is bounded, and missing it is SAFE: the
// range stays `running` under its lease, the lease lapses, and any claimer resumes at the
// durable cursor. Unbounded, this exact write held the leader connection indefinitely —
// after the budget had already expired, when no slice clock was left to protect anything.
func TestABlockedReleaseFallsBackToTheLease(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(30*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Claim first — the queue must be free for that — THEN block the table, so the batch's
	// budget expires and the release write is what hits the lock.
	claimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_repair_ranges IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock queue: %v", err)
	}

	// Run the claimed range with an already-tiny budget: the batch stops immediately and the
	// RELEASE is the next thing that touches the locked table. Under leaderLifecycle the
	// release takes its time FROM the slice — what little is left of these 10ms — and a
	// refused or server-refused write both land back here bounded, with the lease owning
	// recovery. This is the round-6 P0 shape: the old release minted a fresh 100ms here.
	overhead := measuredOverhead(t, ctx, st)
	started := time.Now()
	rerr := st.runRepairRangeOn(ctx, st.pool, claimed, time.Now().Add(10*time.Millisecond), leaderLifecycle)
	elapsed := time.Since(started)
	if rerr == nil {
		t.Fatal("the blocked release reported success; the write cannot have happened")
	}
	t.Logf("contract %s, measured overhead %s", cadenceLimit(10*time.Millisecond), overhead)
	if limit := cadenceLimit(10 * time.Millisecond); elapsed > limit {
		t.Fatalf("the release blocked for %s against the %s slice contract", elapsed, limit)
	}
	_ = blocker.Rollback(ctx)

	// The range is still RUNNING under its lease — nothing was lost, only postponed.
	state, cursor := rangeState(t, st, ctx, claimed.ID)
	if state != "running" {
		t.Fatalf("state = %q after a missed release, want running (the lease owns recovery)", state)
	}
	if !cursor.Equal(claimed.Cursor) {
		t.Fatalf("cursor moved to %s without a committed batch", cursor)
	}

	// Lapse the lease; the next claim resumes exactly where the durable cursor stood.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_repair_ranges SET lease_expires_at = now() - interval '1 second' WHERE id=$1`,
		claimed.ID); err != nil {
		t.Fatalf("lapse lease: %v", err)
	}
	reclaimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("reclaim after the missed release: %v ok=%v", err, ok)
	}
	if reclaimed.ID != claimed.ID || !reclaimed.Cursor.Equal(claimed.Cursor) {
		t.Fatalf("resumed %s at %s, want %s at %s", reclaimed.ID, reclaimed.Cursor, claimed.ID, claimed.Cursor)
	}
}

// The lifecycle cap is a CEILING for a generous caller, never a grant: a COMPLETE under a
// ten-second caller deadline still cannot hold the queue table past the cap.
func TestALifecycleWriteIsCappedUnderAGenerousDeadline(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	// Nothing left to walk: the COMPLETE is the first statement to touch the locked queue.
	claimed.Cursor = claimed.To

	overhead := measuredOverhead(t, ctx, st)
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := blocker.Exec(ctx, `LOCK TABLE service_repair_ranges IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock queue: %v", err)
	}

	started := time.Now()
	rerr := st.runRepairRangeOn(ctx, st.pool, claimed, time.Now().Add(10*time.Second), leaderLifecycle)
	elapsed := time.Since(started)
	_ = blocker.Rollback(ctx)
	if rerr == nil {
		t.Fatal("the blocked complete reported success; the write cannot have happened")
	}
	t.Logf("cap contract %s, measured overhead %s", cadenceLimit(lifecycleWriteBound), overhead)
	if limit := cadenceLimit(lifecycleWriteBound); elapsed > limit {
		t.Fatalf("the complete held the caller for %s against the %s cap", elapsed, limit)
	}
}

// waitForQueryLockWait polls pg_stat_activity until a statement with the given prefix is
// provably parked at a lock wait — the same determinism device the barriered-deadlock test
// uses: the interleaving is observed, not hoped for.
func waitForQueryLockWait(t *testing.T, ctx context.Context, st *Store, prefix string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		var waiting int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock' AND query LIKE $1`, prefix+"%").Scan(&waiting); err != nil {
			t.Fatalf("watch waiters: %v", err)
		}
		if waiting >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no statement %q ever reached a lock wait", prefix)
}

// The round-6 P0, asserted at the spec's own number on the WHOLE slice: work consumes the
// budget, the closing lifecycle write hits a table lock, and RunServiceSlice still returns
// inside max_dispatch_delay + max_scheduling_tolerance — because the lifecycle write takes
// its time FROM the slice. The iter-0130 shape minted a fresh 100ms after the budget was
// spent, and this exact construction measured it at ~350ms for a 250ms slice.
func TestAFullSliceBlockedAtTheLifecycleWriteHoldsCadence(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(30*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	overhead := measuredOverhead(t, ctx, st)

	// First blocker parks the WORK at its first bucket, creating the observable moment at
	// which the queue table can be locked while the range is already claimed and running.
	workBlocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin work blocker: %v", err)
	}
	defer workBlocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := workBlocker.Exec(ctx, `LOCK TABLE service_bucket_ingest IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock ingest: %v", err)
	}

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader: %v ok=%v", err, ok)
	}
	defer ls.Release()

	type run struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan run, 1)
	go func() {
		started := time.Now()
		_, serr := ls.RunServiceSlice(ctx, started.Add(250*time.Millisecond))
		done <- run{time.Since(started), serr}
	}()

	// The slice is provably AT its work-phase lock wait; the claim is long committed, so the
	// queue is free to lock — and the next statement to touch it is the lifecycle write.
	waitForQueryLockWait(t, ctx, st, "INSERT INTO service_bucket_ingest")
	queueBlocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin queue blocker: %v", err)
	}
	defer queueBlocker.Rollback(ctx) //nolint:errcheck // released below
	if _, err := queueBlocker.Exec(ctx, `LOCK TABLE service_repair_ranges IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock queue: %v", err)
	}
	// Release the work: the batch finishes its buckets and slams into the blocked queue with
	// the budget nearly spent — persistence first, then the closing lifecycle write.
	if err := workBlocker.Rollback(ctx); err != nil {
		t.Fatalf("release work: %v", err)
	}

	res := <-done
	_ = queueBlocker.Rollback(ctx)
	if res.err == nil {
		t.Log("the blocked slice surfaced no error; the cadence and the session are the contract")
	}
	if errors.Is(res.err, context.DeadlineExceeded) {
		t.Fatalf("the client net beat the server bound at the lifecycle write: %v", res.err)
	}
	t.Logf("contract %s, measured overhead %s, elapsed %s", cadenceLimit(250*time.Millisecond), overhead, res.elapsed)
	if limit := cadenceLimit(250 * time.Millisecond); res.elapsed > limit {
		t.Fatalf("the slice took %s against the %s §10.10 contract", res.elapsed, limit)
	}

	// The leader survived: default bounds on the session, leadership still held.
	var stmtTimeout string
	if err := ls.conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("the leader connection did not survive: %v", err)
	}
	if stmtTimeout != "0" {
		t.Fatalf("the slice left statement_timeout=%s on the leader session", stmtTimeout)
	}
	if held, err := ls.Check(ctx); err != nil || !held {
		t.Fatalf("leadership lost: held=%v err=%v", held, err)
	}

	// Nothing was persisted, so the recovery contract is EXACT: still running under the
	// lease with the cursor at the range start, and a lapsed lease hands the range back at
	// that same cursor.
	var rangeID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_repair_ranges WHERE service_id=$1`, f.serviceID).Scan(&rangeID); err != nil {
		t.Fatalf("find the range: %v", err)
	}
	state, cursor := rangeState(t, st, ctx, rangeID)
	if state != "running" {
		t.Fatalf("state = %q after the refused lifecycle write, want running (the lease owns recovery)", state)
	}
	if !cursor.Equal(base) {
		t.Fatalf("cursor moved to %s without a committed batch, want %s", cursor, base)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_repair_ranges SET lease_expires_at = now() - interval '1 second' WHERE id=$1`,
		rangeID); err != nil {
		t.Fatalf("lapse lease: %v", err)
	}
	reclaimed, ok, err := st.claimRepairRangeOn(ctx, st.pool)
	if err != nil || !ok {
		t.Fatalf("reclaim after the lapsed lease: %v ok=%v", err, ok)
	}
	if reclaimed.ID != rangeID || !reclaimed.Cursor.Equal(base) {
		t.Fatalf("resumed %s at %s, want %s at %s", reclaimed.ID, reclaimed.Cursor, rangeID, base)
	}
}

// delayedBeginConn stalls BEGIN — the one statement no server bound can speak for, because
// the SET LOCALs come after it. honorCtx models a stall the client net can interrupt (a dead
// peer); without it the stall resolves by itself and hands the code an already-expired
// context, which is exactly the shape that used to be granted a fresh 100ms.
type delayedBeginConn struct {
	dbConn
	delay    time.Duration
	honorCtx bool
}

func (d delayedBeginConn) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.honorCtx {
		timer := time.NewTimer(d.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		time.Sleep(d.delay)
	}
	return d.dbConn.Begin(ctx)
}

// The iter-0130 P0 (final round): the claim measured `remaining`, ran BEGIN on the root
// context, and minted time.Now().Add(remaining) AFTER it returned — a stalled BEGIN was
// unbounded, and the time it consumed was granted a second time. Now one absolute claimBy is
// fixed before BEGIN: the net (claimBy + tolerance) bounds the stall itself, and a BEGIN
// that returns past claimBy finds every subsequent statement refused — no claim after expiry.
func TestAClaimCannotOutliveItsBound(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var rangeID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_repair_ranges WHERE service_id=$1`, f.serviceID).Scan(&rangeID); err != nil {
		t.Fatalf("find range: %v", err)
	}
	assertUnclaimed := func(when string) {
		t.Helper()
		var state string
		var lease *time.Time
		if err := st.pool.QueryRow(ctx,
			`SELECT state, lease_expires_at FROM service_repair_ranges WHERE id=$1`, rangeID).
			Scan(&state, &lease); err != nil {
			t.Fatalf("reread range: %v", err)
		}
		if state != "pending" || lease != nil {
			t.Fatalf("%s: state=%q lease=%v — the range was claimed past its bound", when, state, lease)
		}
	}

	// (a) A stall the net can interrupt: BEGIN parks for 400ms against the 100ms cap. BEGIN's
	// net sits at claimBy ITSELF — no server bound exists there to speak first — so the cut
	// lands at the budget and the flat budget + tolerance assertion holds for this phase by
	// the same arithmetic as every server-bounded one.
	stalled := delayedBeginConn{dbConn: st.pool, delay: 400 * time.Millisecond, honorCtx: true}
	started := time.Now()
	_, ok, err := st.claimRepairRangeBounded(ctx, stalled, time.Now().Add(10*time.Second))
	elapsed := time.Since(started)
	if ok {
		t.Fatal("a claim came back from a BEGIN the net had to kill")
	}
	if err == nil {
		t.Fatal("a stalled BEGIN surfaced neither a claim nor an error")
	}
	if limit := cadenceLimit(lifecycleWriteBound); elapsed > limit {
		t.Fatalf("the stalled BEGIN held the caller %s against the flat %s contract", elapsed, limit)
	}
	assertUnclaimed("after the interrupted stall")

	// (b) A stall that resolves by itself, PAST the bound: BEGIN sleeps 150ms against the
	// 100ms cap and then proceeds. Expiry must mean refusal — the old shape granted the
	// statements after BEGIN a fresh 100ms and claimed the range at ~190ms on the leader
	// connection.
	slow := delayedBeginConn{dbConn: st.pool, delay: 150 * time.Millisecond, honorCtx: false}
	if _, ok, err := st.claimRepairRangeBounded(ctx, slow, time.Now().Add(10*time.Second)); ok {
		t.Fatalf("a claim succeeded AFTER its bound expired (err=%v)", err)
	}
	assertUnclaimed("after the self-resolving stall")
}

// The iter-0130 P1 (final round): the undersized-tail guard lived inside the repair slice
// and returned "nothing to do" — and false there means "the repair queue is empty", so a
// short tail ran the FORWARD pass with a repair backlog still pending. Repair-first means an
// undersized tail runs NEITHER phase: no claim to park under a lease, no forward advance
// jumping the queue.
func TestAnUndersizedTailRunsNeitherRepairNorForward(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-30 * time.Minute)}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	base := domain.FloorToBucket(time.Now().UTC().Add(-20 * time.Minute))
	beat(t, st, ctx, f.http, base.Add(10*time.Second), true)
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute), ReasonBackfill); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	forwardCursor := func() *time.Time {
		var c *time.Time
		if err := st.pool.QueryRow(ctx,
			`SELECT materialized_through FROM service_materialization WHERE service_id=$1`,
			f.serviceID).Scan(&c); err != nil {
			t.Fatalf("read forward cursor: %v", err)
		}
		return c
	}
	before := forwardCursor()

	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader: %v ok=%v", err, ok)
	}
	defer ls.Release()

	// 100ms: under the 140ms tail floor, but ABOVE forward's 60ms commit reserve — so the
	// iter-0130 guard placement does not merely error out of the forward pass, it ADVANCES
	// the cursor and commits.
	worked, serr := ls.RunServiceSlice(ctx, time.Now().Add(100*time.Millisecond))

	// The cursor is checked FIRST: the kill for the guard-inside-repair mutant is the
	// MOVEMENT itself — forward jumped a pending repair backlog — not a side effect of the
	// phase having been entered.
	after := forwardCursor()
	switch {
	case before == nil && after != nil:
		t.Fatalf("forward advanced to %s on an undersized tail with a repair backlog pending", *after)
	case before != nil && (after == nil || !after.Equal(*before)):
		t.Fatalf("forward cursor moved %v → %v on an undersized tail", before, after)
	}
	// The repair range was not touched: still pending, no lease minted.
	var state string
	var lease *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT state, lease_expires_at FROM service_repair_ranges WHERE service_id=$1`,
		f.serviceID).Scan(&state, &lease); err != nil {
		t.Fatalf("reread range: %v", err)
	}
	if state != "pending" || lease != nil {
		t.Fatalf("the tail claimed the range: state=%q lease=%v", state, lease)
	}
	// And the tail reported exactly nothing to the scheduler.
	if serr != nil || worked {
		t.Fatalf("an undersized tail did work: worked=%v err=%v", worked, serr)
	}
}

// The §10.10 accumulation acceptance, verbatim: TWO consecutive statements, each finishing
// near its own sub-deadline, under ONE absolute slice — and the total, BEGIN through
// rollback, still inside budget + max_scheduling_tolerance. This is the case a set-once
// timeout cannot pass: statement_timeout is a duration RESTARTED per statement, so a bound
// issued once lets every following statement consume the original allowance again. The
// mechanism under test is deadlineTx re-deriving the bound from the REMAINDER: the first
// sleep fits its bound and spends most of the budget; the second is CUT by what is left —
// by the SERVER, before the client net says anything.
func TestConsecutiveStatementsCannotAccumulate(t *testing.T) {
	st, ctx := declStore(t)
	const budget = 250 * time.Millisecond

	// A pinned connection, so the session that ran the slice is the session inspected after.
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	started := time.Now()
	deadline := started.Add(budget)
	// The production net shape: one scheduling tolerance BEHIND the server bounds.
	cctx, cancel := context.WithDeadline(ctx, deadline.Add(schedulingTolerance))
	defer cancel()

	rawTx, err := conn.Begin(cctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck // rolled back below
	tx := newDeadlineTx(rawTx, deadline, 0)

	// Statement 1: sleeps 200ms under a ~250ms bound — inside its own sub-deadline, and the
	// budget is now nearly spent.
	if _, err := tx.Exec(cctx, `SELECT pg_sleep(0.2)`); err != nil {
		t.Fatalf("the first statement was within its bound and failed: %v", err)
	}
	// Statement 2: the same sleep again. A set-once bound would let it run its full 200ms —
	// per-statement restart — for a ~400ms total. The remainder-derived bound cuts it at
	// what is LEFT (~50ms), and the refusal must be the server's.
	_, serr := tx.Exec(cctx, `SELECT pg_sleep(0.2)`)
	if serr == nil {
		t.Fatal("the second statement ran to completion past the slice budget")
	}
	if errors.Is(serr, context.DeadlineExceeded) {
		t.Fatalf("the client net beat the server bound: %v", serr)
	}
	if !isStatementBudgetError(serr) && !errors.Is(serr, errSliceBudget) {
		t.Fatalf("the second statement died of something other than the budget: %v", serr)
	}
	if err := rawTx.Rollback(ctx); err != nil {
		t.Fatalf("rollback after the budget refusal: %v", err)
	}
	elapsed := time.Since(started)
	if limit := cadenceLimit(budget); elapsed > limit {
		t.Fatalf("BEGIN→rollback took %s against the flat %s contract", elapsed, limit)
	}

	// The SET LOCALs died with the transaction: the session carries defaults.
	var stmtTimeout string
	if err := conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmtTimeout); err != nil {
		t.Fatalf("the session did not survive: %v", err)
	}
	if stmtTimeout != "0" {
		t.Fatalf("the slice left statement_timeout=%s on the session", stmtTimeout)
	}

	// And the COMMIT side of the same acceptance: a transaction that stops INSIDE its budget
	// commits through the adapter, bounded, with time left. Its slice is ITS OWN — a fresh
	// deadline and a fresh net derived from it, the production envelope — because the first
	// slice is entitled to consume its whole contract, and a second transaction inheriting
	// the first's client net would start life with near-zero (or negative) time on a loaded
	// runner and fail without anything being wrong.
	deadline2 := time.Now().Add(budget)
	cctx2, cancel2 := context.WithDeadline(ctx, deadline2.Add(schedulingTolerance))
	defer cancel2()
	rawTx2, err := conn.Begin(cctx2)
	if err != nil {
		t.Fatalf("begin second tx: %v", err)
	}
	defer rawTx2.Rollback(ctx) //nolint:errcheck // no-op after commit
	tx2 := newDeadlineTx(rawTx2, deadline2, 0)
	if _, err := tx2.Exec(cctx2, `SELECT 1`); err != nil {
		t.Fatalf("bounded statement: %v", err)
	}
	if err := tx2.Commit(cctx2); err != nil {
		t.Fatalf("bounded commit: %v", err)
	}
}

// A STRUCTURAL tripwire, stated as exactly that. Two persistence hybrids — a raw-transaction
// persist with one fixed SET, and a commit outside the adapter — diverge from the mechanism
// only in a sub-50ms race between a fixed bound's tail and the client net, which no
// deterministic lock construction can pin without flaking. Their behavioural kills are
// therefore NOT claimed; what is enforced instead is the structure the mechanism depends on:
// every slice path persists through persistPhase() and commits through the adapter, never
// the raw transaction. A source scan is a weak proof and an excellent alarm.
func TestSlicePathsPersistThroughTheEnvelope(t *testing.T) {
	for _, file := range []string{"servicematerialize.go", "servicerepair.go", "servicemaintenance.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := string(raw)
		if strings.Contains(src, "rawTx.Commit(") {
			t.Errorf("%s commits the raw transaction directly; the commit has left the mechanism", file)
		}
		if !strings.Contains(src, ".persistPhase()") {
			t.Errorf("%s does not persist through the envelope", file)
		}
	}
}
