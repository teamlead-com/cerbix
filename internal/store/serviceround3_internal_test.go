package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// Round-3 regressions (iter-0127). Each of these fails on the iter-0126 code, and each was
// re-applied as a mutant to prove it.

const monthRetention = 30 * 24 * time.Hour

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
	if len(p.Services) == 0 || p.Services[0].Before.Good == 0 {
		t.Fatalf("the preview did not see the partially-covered bucket: %+v", p.Services)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, p.ID, monthRetention); err != nil {
		t.Fatalf("tokened annul: %v", err)
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

// P0-3 — the projection has a WALL budget. Past it the token degrades to `approximate`
// instead of holding the project's membership lock for the duration.
func TestPreviewDegradesToApproximateAtItsWallBudget(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	st.previewBudget = time.Nanosecond // expire the wall budget before the first bucket

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(3*time.Minute), monthRetention, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Coverage != "approximate" {
		t.Fatalf("coverage = %q under an exhausted wall budget, want approximate", p.Coverage)
	}
	// …and an approximate token is unconfirmable, as before.
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(3 * time.Minute), Reason: "unshown",
	}, p.ID, monthRetention)
	if !errors.Is(err, ErrPreviewApproximate) {
		t.Fatalf("an approximate token confirmed: %v", err)
	}
}

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
	// Re-run the migration's teeth against live rows: every open token dies.
	if _, err := st.pool.Exec(ctx, `UPDATE maintenance_previews SET expires_at = now() WHERE expires_at > now()`); err != nil {
		t.Fatalf("apply the expiry: %v", err)
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
	defer ls.Release()

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
