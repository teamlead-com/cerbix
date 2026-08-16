package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sealedService adopts a service, materializes and seals a range, and returns the base.
func sealedService(t *testing.T, st *Store, ctx context.Context) (declFixture, time.Time) {
	t.Helper()
	f := adoptedService(t, st, ctx)
	base := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Minute)
	materializeFrom(t, st, ctx, f, base)
	for i := 0; i < 10; i++ {
		beat(t, st, ctx, f.http, base.Add(time.Duration(i)*time.Minute+10*time.Second), true)
	}
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, f.serviceID, base, base.Add(10*time.Minute)); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return f, base
}

func maintenanceGeneration(t *testing.T, st *Store, ctx context.Context, projectID string) int64 {
	t.Helper()
	var g int64
	if err := st.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT generation FROM project_maintenance_generation WHERE project_id=$1), 0)`,
		projectID).Scan(&g); err != nil {
		t.Fatalf("generation: %v", err)
	}
	return g
}

// A window over time nothing has sealed is an ordinary prospective mutation: nothing sealed
// means nothing to repair, so demanding a preview would be ceremony.
func TestWindowOverUnsealedTimeNeedsNoPreview(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)
	from := time.Now().UTC().Add(time.Hour)

	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http, StartsAt: from, EndsAt: from.Add(time.Hour), Reason: "planned",
	}, "", time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("a future window was refused: %v", err)
	}
}

// A window intersecting SEALED time is a retroactive repair by definition, and it is refused
// without a confirmed preview. Silently accepting it and changing a sealed number is the
// operation this whole contract exists to prevent.
func TestWindowOverSealedTimeRequiresAPreview(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	_, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(5 * time.Minute), Reason: "last night",
	}, "", time.Now().UTC().Add(-30*24*time.Hour))
	if !errors.Is(err, ErrRetroactiveNeedsPreview) {
		t.Fatalf("got %v, want ErrRetroactiveNeedsPreview", err)
	}
}

// With a preview, the same window applies — and it bumps the generation, enqueues a repair
// and rewinds the watermark, all in one transaction. A pending job on its own would leave the
// API serving the old number for as long as the queue is deep.
func TestConfirmedRetroactiveWindowInvalidatesImmediately(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	beforeGeneration := maintenanceGeneration(t, st, ctx, f.projectID)
	beforeThrough := sealedThrough(t, st, ctx, f.serviceID)
	if beforeThrough == nil {
		t.Fatal("the fixture did not seal anything")
	}

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(5*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 || p.Services[0].ServiceID != f.serviceID {
		t.Fatalf("preview affected %+v, want the one declaring service", p.Services)
	}

	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(5 * time.Minute), Reason: "last night",
	}, p.ID, rawFloor); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if got := maintenanceGeneration(t, st, ctx, f.projectID); got <= beforeGeneration {
		t.Errorf("generation = %d, want it bumped past %d", got, beforeGeneration)
	}
	var ranges int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges WHERE service_id=$1 AND reason='maintenance'`,
		f.serviceID).Scan(&ranges); err != nil {
		t.Fatalf("count ranges: %v", err)
	}
	if ranges != 1 {
		t.Errorf("%d maintenance repair ranges, want 1", ranges)
	}
	after := sealedThrough(t, st, ctx, f.serviceID)
	if after == nil || !after.Equal(base) {
		t.Fatalf("sealed_through = %v, want the rewind to %s — the API would keep serving the old number", after, base)
	}
	var retractedTo *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT retracted_to FROM service_materialization WHERE service_id=$1`, f.serviceID).Scan(&retractedTo); err != nil {
		t.Fatalf("retraction: %v", err)
	}
	if retractedTo == nil {
		t.Error("the watermark moved backwards without recording the retraction")
	}
}

// A token is spent on confirm. Replaying it must not apply a second mutation.
func TestPreviewTokenIsSpentOnce(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(5*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w := domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(5 * time.Minute), Reason: "once",
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, w, p.ID, rawFloor); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, w, p.ID, rawFloor)
	if !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("replaying a spent token returned %v, want ErrPreviewStale", err)
	}
}

// A declaration change between preview and confirm makes the token stale: the set was
// previewed at one revision and would be mutated at another.
func TestPreviewGoesStaleWhenADeclarationMoves(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(5*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "someone else"}); err != nil {
		t.Fatalf("concurrent declaration: %v", err)
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(5 * time.Minute), Reason: "stale",
	}, p.ID, rawFloor)
	if !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("got %v, want ErrPreviewStale", err)
	}
}

// A service that enters the affected set between preview and confirm also makes the token
// stale. Re-reading the generations of services already known proves those rows did not move
// and proves nothing about the SET.
func TestPreviewGoesStaleWhenAServiceJoinsTheAffectedSet(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(5*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// A SECOND service starts declaring the same monitor as a reliability input.
	other, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "search", Name: "Search"})
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, other.ID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("second declaration: %v", err)
	}

	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(5 * time.Minute), Reason: "set moved",
	}, p.ID, rawFloor)
	if !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("got %v, want ErrPreviewStale — a service joined the set and would be mutated unpreviewed", err)
	}
}

// Beyond raw retention the sealed part cannot be recomputed, so the mutation fails CLOSED
// and names the earliest instant that would work. Silently accepting it and changing nothing
// is the worst of the three outcomes, because it looks like a completed command.
func TestRetroactiveMutationBeyondRawFailsClosed(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	// A floor AFTER the range: the raw heartbeats behind those buckets are notionally gone.
	rawFloor := base.Add(5 * time.Minute)

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(2*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "too old",
	}, p.ID, rawFloor)
	if !errors.Is(err, ErrUnrecomputableRange) {
		t.Fatalf("got %v, want ErrUnrecomputableRange", err)
	}
	var windows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM maintenance_windows WHERE project_id=$1`, f.projectID).Scan(&windows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if windows != 0 {
		t.Errorf("%d windows were created despite the fence; the mutation was partially applied", windows)
	}
}

// Archive is always permitted, needs no raw data, and never rewrites sealed past. That is
// what makes cleaning up old windows possible at all.
func TestArchiveIsAlwaysPermittedAndLeavesSealedFactsAlone(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	// A window that already applied to sealed time.
	w, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "old",
	})
	if err != nil {
		t.Fatalf("seed window: %v", err)
	}
	beforeThrough := sealedThrough(t, st, ctx, f.serviceID)

	if err := st.ArchiveMaintenanceWindow(ctx, f.projectID, w.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var archived *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT archived_at FROM maintenance_windows WHERE id=$1`, w.ID).Scan(&archived); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if archived == nil {
		t.Fatal("archive did not stamp the row")
	}
	after := sealedThrough(t, st, ctx, f.serviceID)
	if (beforeThrough == nil) != (after == nil) || (after != nil && !after.Equal(*beforeThrough)) {
		t.Errorf("archive moved the watermark %v -> %v; it must not touch sealed past", beforeThrough, after)
	}
	var ranges int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges WHERE service_id=$1`, f.serviceID).Scan(&ranges); err != nil {
		t.Fatalf("count: %v", err)
	}
	if ranges != 0 {
		t.Errorf("archive enqueued %d repairs; it claims nothing about the past", ranges)
	}
}

// Annul is the privileged act that DOES claim the past was wrong, so it needs the preview
// and the fence — and it is the only thing that removes a span from the evaluator's input.
func TestAnnulRequiresAPreviewAndRepairsTheRange(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	w, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "mistake",
	})
	if err != nil {
		t.Fatalf("seed window: %v", err)
	}

	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, "", rawFloor); !errors.Is(err, ErrRetroactiveNeedsPreview) {
		t.Fatalf("annul without a preview returned %v, want ErrRetroactiveNeedsPreview", err)
	}

	// The token has to be issued FOR AN ANNUL. A create-kind preview authorizing an annul is
	// the binding hole this contract closes.
	// An annul names the WINDOW it removes: two windows over the same monitor and range are
	// different mutations with different consequences.
	p, err := st.PreviewMutationOf(ctx, f.projectID, f.http, w.ID, MutationAnnul, base, base.Add(2*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, p.ID, rawFloor); err != nil {
		t.Fatalf("annul: %v", err)
	}
	var windows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM maintenance_windows WHERE id=$1`, w.ID).Scan(&windows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if windows != 0 {
		t.Error("annul left the window in the evaluator's input")
	}
	var ranges int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_repair_ranges WHERE service_id=$1 AND reason='maintenance'`,
		f.serviceID).Scan(&ranges); err != nil {
		t.Fatalf("count ranges: %v", err)
	}
	if ranges == 0 {
		t.Error("annul enqueued no repair; the past it claims was wrong would never be recomputed")
	}
}

// The regression the shipped suite was missing entirely: a confirmed retroactive mutation
// must CHANGE THE NUMBERS.
//
// Every existing test stopped at "a repair range was enqueued", and that was satisfied by
// code in which the repair walked the range, hit `state='sealed'`, returned, and left every
// stale fact exactly where it was. The watermark then advanced over them again. Asserting on
// the queue instead of the result is how a repair path that repairs nothing passes review.
func TestConfirmedAnnulActuallyRewritesTheSealedFacts(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// A window covering the first two buckets, applied while they were already sealed GOOD.
	w, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "planned",
	})
	if err != nil {
		t.Fatalf("seed window: %v", err)
	}
	// Recompute under the window so the sealed facts carry the exclusion…
	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(2*time.Minute), ReasonMaintenance); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drainRepair(t, st, ctx)

	excluded, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact for the first bucket")
	}
	if excluded.excluded == 0 {
		t.Fatalf("the sealed bucket was never recomputed under the window: %+v — repair is a no-op over sealed facts", excluded)
	}
	if excluded.state != "sealed" {
		t.Errorf("a recomputed bucket became %q; correcting a final number must not un-seal it", excluded.state)
	}

	// …then annul the window, which says it never applied, and require the facts to come back.
	// The token has to be issued FOR AN ANNUL. A create-kind preview authorizing an annul is
	// the binding hole this contract closes.
	// An annul names the WINDOW it removes: two windows over the same monitor and range are
	// different mutations with different consequences.
	p, err := st.PreviewMutationOf(ctx, f.projectID, f.http, w.ID, MutationAnnul, base, base.Add(2*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, p.ID, rawFloor); err != nil {
		t.Fatalf("annul: %v", err)
	}
	drainRepair(t, st, ctx)

	restored, _ := readFact(t, st, ctx, f.serviceID, base)
	if restored.excluded != 0 {
		t.Errorf("after annul the bucket still excludes %d us; the annul repaired nothing", restored.excluded)
	}
	if restored.good == 0 {
		t.Error("after annul the bucket records no good time; the recompute did not restore the observation")
	}

	// And the restatement of a sealed number is on the record, with both sides.
	var audits int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action='service.bucket_recomputed' AND target LIKE '%'||$1||'%'`,
		f.serviceID).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits == 0 {
		t.Error("a sealed number changed with no audit row; a silent restatement is indistinguishable from rot")
	}
}

// drainRepair runs the leader's service slice until the queue empties, exactly as the
// scheduler sub-tick does.
func drainRepair(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	ls, ok, err := st.TryBecomeLeaderSession(ctx, time.Now().UnixNano())
	if err != nil || !ok {
		t.Fatalf("leader session: %v ok=%v", err, ok)
	}
	defer ls.Release()
	for i := 0; i < 50; i++ {
		worked, err := ls.RunServiceRepairSlice(ctx, time.Now().Add(2*time.Second))
		if err != nil {
			t.Fatalf("repair slice: %v", err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("repair queue never drained")
}

// A preview token authorizes ONE mutation. The shipped confirm checked that the world had not
// moved and never checked what it was being asked to do, so a token issued for a small window
// on one monitor authorized a different, larger change — the exact retroactive rewrite the
// preview exists to gate.
func TestAPreviewTokenOnlyAuthorizesTheMutationItWasIssuedFor(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	small := func() (string, error) {
		p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
			base, base.Add(2*time.Minute), rawFloor, "op")
		return p.ID, err
	}

	// (a) A different RANGE.
	id, err := small()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(9 * time.Minute), Reason: "widened",
	}, id, rawFloor)
	if !errors.Is(err, ErrPreviewStale) {
		t.Errorf("a token for a 2-minute window authorized a 9-minute one: %v", err)
	}

	// (b) A different MONITOR. Both mutations must resolve to the SAME affected set, or a set
	// difference would reject it for the wrong reason and prove nothing about the monitor
	// binding. Widening this service's SLI to both monitors gives exactly that: each window
	// then affects {this service} and nothing else.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http, f.redis},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("widen sli: %v", err)
	}
	id, err = small()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.redis,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "wrong monitor",
	}, id, rawFloor)
	if !errors.Is(err, ErrPreviewStale) {
		t.Errorf("a token for one monitor authorized a window on another with the same affected set: %v", err)
	}

	// (c) A different KIND. A preview of "create this window" must not confirm an annul.
	w, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "seed",
	})
	if err != nil {
		t.Fatalf("seed window: %v", err)
	}
	id, err = small()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, w.ID, id, rawFloor); !errors.Is(err, ErrPreviewStale) {
		t.Errorf("a create-kind token authorized an annul: %v", err)
	}

	// …and the matching mutation still works, so the binding is a gate and not a wall.
	id, err = small()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "exact",
	}, id, rawFloor); err != nil {
		t.Errorf("the mutation the token was issued for was refused: %v", err)
	}
}

// Deleting a service is a change to the affected SET, and the comparison alone cannot see it:
// the row is missing from the current set for the same reason a cascade had removed it from
// the stored one. The shipped schema cascaded, so the two agreed and the confirm PASSED.
func TestDeletingAnAffectedServiceInvalidatesThePreview(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// A SECOND service on the same monitor, so the affected set has two members. With only
	// one, deleting it leaves nothing for the mutation to touch and no preview is required —
	// correct behaviour, and it would hide the defect rather than expose it.
	second, err := st.CreateService(ctx, domain.Service{
		ProjectID: f.projectID, Slug: "second", Name: "Second",
	})
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, second.ID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: base}); err != nil {
		t.Fatalf("declare second: %v", err)
	}
	if _, err := st.MaterializeServiceRange(ctx, f.projectID, second.ID, base, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("materialize second: %v", err)
	}

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) < 2 {
		t.Fatalf("the preview saw %d affected services, want 2", len(p.Services))
	}

	if err := st.DeleteService(ctx, f.projectID, second.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The snapshot survives the deletion — it records what was true when the preview ran.
	var kept int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM maintenance_preview_services WHERE preview_id=$1 AND service_id=$2`,
		p.ID, second.ID).Scan(&kept); err != nil {
		t.Fatalf("count snapshot: %v", err)
	}
	if kept != 1 {
		t.Error("the deletion edited the preview's snapshot; a record of what was true cannot be rewritten by later events")
	}

	_, err = st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "after the delete",
	}, p.ID, rawFloor)
	if !errors.Is(err, ErrPreviewStale) && !errors.Is(err, ErrRetroactiveNeedsPreview) {
		t.Errorf("a preview survived the deletion of a service it covered: %v", err)
	}
}

// A preview must show what WOULD change, not only what is. The shipped version summed the
// current good/bad and called that a preview: the operator was asked to authorize a change to
// sealed numbers and shown the numbers they already had.
func TestPreviewProjectsBothSidesOfTheMutation(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(3*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Services) != 1 {
		t.Fatalf("%d affected services, want 1", len(p.Services))
	}
	svc := p.Services[0]
	if !svc.Projected {
		t.Fatal("the after-state was not projected for a three-bucket range")
	}
	if svc.Before.Good == 0 {
		t.Fatalf("before shows no good time: %+v", svc.Before)
	}
	// A maintenance window over sealed GOOD time moves it into EXCLUDED. Availability before
	// and after must therefore differ — that difference is the whole content of a preview.
	if svc.After.Excluded <= svc.Before.Excluded {
		t.Errorf("after excludes %d us and before %d — the projection did not apply the window",
			svc.After.Excluded, svc.Before.Excluded)
	}
	if svc.After.Good >= svc.Before.Good {
		t.Errorf("good time did not fall: before %d, after %d", svc.Before.Good, svc.After.Good)
	}

	// And the projection matches what the confirm actually produces: the preview runs the
	// same reducer, so the two cannot drift.
	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(3 * time.Minute), Reason: "projected",
	}, p.ID, rawFloor); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	drainRepair(t, st, ctx)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	actual, err := currentAggregate(ctx, tx, f.serviceID, base, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("read actual: %v", err)
	}
	if actual.Excluded != svc.After.Excluded || actual.Good != svc.After.Good {
		t.Errorf("the preview promised %+v and the confirm produced %+v", svc.After, actual)
	}
}

// Annul is identified by its WINDOW. With two windows over the same monitor and range,
// annulling one may change nothing while annulling the other changes the number, so a token
// issued for one must not confirm the other.
func TestAnAnnulTokenIsBoundToTheWindowItNames(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	first, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "first",
	})
	if err != nil {
		t.Fatalf("first window: %v", err)
	}
	second, err := st.CreateMaintenanceWindow(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "second",
	})
	if err != nil {
		t.Fatalf("second window: %v", err)
	}

	p, err := st.PreviewMutationOf(ctx, f.projectID, f.http, first.ID, MutationAnnul,
		base, base.Add(2*time.Minute), rawFloor, "op")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, second.ID, p.ID, rawFloor); !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("a token issued for one window authorized annulling another: %v", err)
	}
	if err := st.AnnulMaintenanceWindow(ctx, f.projectID, first.ID, p.ID, rawFloor); err != nil {
		t.Fatalf("the window the token named was refused: %v", err)
	}
}

// The mutation itself is audited, with who authorized it and under which token — questions
// the per-bucket recompute rows cannot answer, because they record what changed rather than
// who decided it should.
func TestARetroactiveMutationIsAuditedWithItsActorAndToken(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)
	rawFloor := time.Now().UTC().Add(-30 * 24 * time.Hour)

	p, err := st.PreviewMutation(ctx, f.projectID, f.http, MutationCreate,
		base, base.Add(2*time.Minute), rawFloor, "seymur@teamlead.com")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := st.CreateMaintenanceWindowChecked(ctx, domain.MaintenanceWindow{
		ProjectID: f.projectID, MonitorID: f.http,
		StartsAt: base, EndsAt: base.Add(2 * time.Minute), Reason: "audited",
	}, p.ID, rawFloor); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	var target string
	if err := st.pool.QueryRow(ctx,
		`SELECT target FROM audit_logs WHERE action='service.maintenance_mutated' ORDER BY created_at DESC LIMIT 1`).
		Scan(&target); err != nil {
		t.Fatalf("no mutation audit row: %v", err)
	}
	for _, want := range []string{"mutation=create", "by=seymur@teamlead.com", "preview=" + p.ID, f.http} {
		if !strings.Contains(target, want) {
			t.Errorf("audit target %q is missing %q", target, want)
		}
	}
}

// A restatement that moves HEALTH without moving availability is still a restatement. Auditing
// only good/bad reported that nothing had happened.
func TestARecomputeThatMovesOnlyHealthIsStillAudited(t *testing.T) {
	st, ctx := declStore(t)
	f, base := sealedService(t, st, ctx)

	// Rewrite the sealed bucket so only the HEALTH axis differs from what a recompute will
	// produce; availability stays exactly where it is.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_reliability_buckets
		    SET healthy_us = 0, degraded_us = healthy_us + degraded_us
		  WHERE service_id=$1 AND bucket_start=$2`, f.serviceID, base); err != nil {
		t.Fatalf("skew the health axis: %v", err)
	}
	before, ok := readFact(t, st, ctx, f.serviceID, base)
	if !ok {
		t.Fatal("no fact to recompute")
	}

	if err := st.EnqueueRepairRange(ctx, f.projectID, f.serviceID, base, base.Add(time.Minute), ReasonAdmin); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drainRepair(t, st, ctx)

	after, _ := readFact(t, st, ctx, f.serviceID, base)
	if after.good != before.good || after.bad != before.bad {
		t.Fatalf("availability moved too (%d/%d -> %d/%d); this test would then prove nothing",
			before.good, before.bad, after.good, after.bad)
	}
	if after.healthy == before.healthy {
		t.Fatalf("the recompute did not restore the health axis: %+v", after)
	}

	var audits int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action='service.bucket_recomputed' AND target LIKE '%'||$1||'%'`,
		f.serviceID).Scan(&audits); err != nil {
		t.Fatalf("count: %v", err)
	}
	if audits == 0 {
		t.Error("health moved on a sealed bucket with no audit row; comparing only availability calls that 'nothing happened'")
	}
}
