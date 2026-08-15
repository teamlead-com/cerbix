package store

import (
	"context"
	"errors"
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

	p, err := st.PreviewMaintenanceMutation(ctx, f.projectID, f.http, base, base.Add(2*time.Minute), rawFloor, "op")
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
