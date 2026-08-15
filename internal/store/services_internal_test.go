package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func declStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	return serviceSchemaStore(t)
}

type declFixture struct {
	projectID string
	serviceID string
	http      string
	synthetic string
	redis     string
}

func seedDeclaration(t *testing.T, st *Store, ctx context.Context) declFixture {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "payments", "Payments")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	mk := func(name string, typ domain.MonitorType, region string) string {
		m, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: name, Type: typ, Target: "https://" + name + ".example.com/",
			IntervalSeconds: 30, Region: region, Enabled: true,
		})
		if err != nil {
			t.Fatalf("monitor %s: %v", name, err)
		}
		return m.ID
	}
	f := declFixture{projectID: proj.ID}
	f.http = mk("checkout-http", domain.MonitorHTTP, "core")
	f.synthetic = mk("checkout-synthetic", domain.MonitorHTTP, "core")
	f.redis = mk("checkout-redis", domain.MonitorTCP, "core")

	svc, err := st.CreateService(ctx, domain.Service{
		ProjectID: proj.ID, Slug: "checkout", Name: "Checkout",
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	f.serviceID = svc.ID
	return f
}

// Every definition revision gets a matching epoch, unconditionally. A revision without one
// is not a latency problem — a fact references the epoch ALONE, so a revision that never
// got an epoch can never be referenced by anything the evaluator produces.
func TestEveryRevisionGetsAMatchingEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	rev, epoch, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.synthetic, f.redis},
		SLI:      []string{f.http, f.synthetic},
	}, 0, DeclarationOptions{CreatedBy: "seymur@teamlead.com"})
	if err != nil {
		t.Fatalf("put declaration: %v", err)
	}
	if rev.Revision != 1 {
		t.Errorf("revision = %d, want 1", rev.Revision)
	}
	if epoch.RevisionID != rev.ID {
		t.Errorf("the epoch resolves to %s, want the revision just written (%s)", epoch.RevisionID, rev.ID)
	}
	if epoch.EffectiveAt != rev.EffectiveAt {
		t.Errorf("epoch and revision must share the boundary: %s vs %s", epoch.EffectiveAt, rev.EffectiveAt)
	}
	if len(epoch.Members) != 2 {
		t.Fatalf("the snapshot covers the DECLARED SLI, got %d members", len(epoch.Members))
	}
	if epoch.SnapshotHash == "" {
		t.Error("the snapshot hash is what makes an execution-driven epoch a no-op; it must be set")
	}

	// A second declaration, and the invariant must still hold.
	rev2, epoch2, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.synthetic, f.redis},
		SLI:      []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "seymur@teamlead.com"})
	if err != nil {
		t.Fatalf("second declaration: %v", err)
	}
	if epoch2.RevisionID != rev2.ID {
		t.Error("the second epoch does not resolve to the second revision")
	}

	var revisions, epochs int
	if err := st.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id=$1),
		        (SELECT count(*) FROM service_evaluation_epochs   WHERE service_id=$1)`,
		f.serviceID).Scan(&revisions, &epochs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if revisions != epochs {
		t.Errorf("%d revisions but %d epochs — a revision with no epoch is an unsatisfiable reference", revisions, epochs)
	}
}

// effective_at is CEILED from the database clock, so an ordinary write governs from the
// next canonical boundary rather than mid-bucket.
func TestEffectiveAtIsCeiledToTheNextBoundary(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	rev, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !rev.EffectiveAt.Equal(rev.EffectiveAt.Truncate(time.Minute)) {
		t.Errorf("effective_at %s is not on a bucket boundary", rev.EffectiveAt)
	}
	if rev.EffectiveAt.Before(rev.CreatedAt) {
		t.Errorf("effective_at %s precedes created_at %s — an ordinary write is prospective", rev.EffectiveAt, rev.CreatedAt)
	}
	if want := domain.CeilToBucket(rev.CreatedAt); !rev.EffectiveAt.Equal(want) {
		t.Errorf("effective_at = %s, want ceil(created_at) = %s", rev.EffectiveAt, want)
	}
}

// Two writes inside the same minute target the same next boundary. Immutable rows plus a
// half-open interval leave no order between them, so the later write displaces the earlier
// one — which is retained for audit, governs nothing, and frees the boundary.
func TestSameBoundaryWritesLeaveExactlyOneWinner(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	first, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.synthetic}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, secondEpoch, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.synthetic}, SLI: []string{f.http, f.synthetic},
	}, 1, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.EffectiveAt.Equal(first.EffectiveAt) {
		t.Skip("the two writes landed in different minutes; this test needs them to share a boundary")
	}

	var state string
	if err := st.pool.QueryRow(ctx,
		`SELECT state FROM service_definition_revisions WHERE id=$1`, first.ID).Scan(&state); err != nil {
		t.Fatalf("read first: %v", err)
	}
	if state != string(domain.RevisionSuperseded) {
		t.Errorf("the displaced revision is %q, want %q — it must be retained, not deleted", state, domain.RevisionSuperseded)
	}

	var effective int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_definition_revisions
		  WHERE service_id=$1 AND effective_at=$2 AND state='effective'`,
		f.serviceID, first.EffectiveAt).Scan(&effective); err != nil {
		t.Fatalf("count effective: %v", err)
	}
	if effective != 1 {
		t.Errorf("%d effective revisions on one boundary; a fact would resolve to more than one", effective)
	}

	// The epoch axis resolves the same way, and the surviving epoch points at the surviving
	// revision.
	if secondEpoch.RevisionID != second.ID {
		t.Error("the winning epoch does not resolve to the winning revision")
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_evaluation_epochs
		  WHERE service_id=$1 AND effective_at=$2 AND state='effective'`,
		f.serviceID, first.EffectiveAt).Scan(&effective); err != nil {
		t.Fatalf("count effective epochs: %v", err)
	}
	if effective != 1 {
		t.Errorf("%d effective epochs on one boundary", effective)
	}
}

// A row that never took effect governs no bucket, so work scoped to it has no target.
// Leaving such a job behind is how a watermark stalls on something nobody can finish.
func TestSupersededRevisionCancelsItsRanges(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	first, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	var rangeID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO service_repair_ranges (service_id, project_id, range_start, range_end, reason)
		 VALUES ($1,$2,$3,$4,'declaration') RETURNING id`,
		f.serviceID, f.projectID, first.EffectiveAt, first.EffectiveAt.Add(time.Hour)).Scan(&rangeID); err != nil {
		t.Fatalf("range: %v", err)
	}

	second, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.synthetic}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.EffectiveAt.Equal(first.EffectiveAt) {
		t.Skip("the two writes landed in different minutes")
	}
	var state string
	if err := st.pool.QueryRow(ctx, `SELECT state FROM service_repair_ranges WHERE id=$1`, rangeID).Scan(&state); err != nil {
		t.Fatalf("read range: %v", err)
	}
	if state != "superseded" {
		t.Errorf("range state = %q, want superseded — a job outlived the row that asked for it", state)
	}
}

// Two operators editing an SLI have made two different statements about what availability
// means. Picking one silently is the worst of the three options.
func TestStaleRevisionIsRejected(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "first"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.redis}, SLI: []string{f.redis},
	}, 0, DeclarationOptions{CreatedBy: "second"})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("a write against a stale revision returned %v, want ErrRevisionConflict", err)
	}
}

// An SLI member outside the operational context would be a number with no visible source.
func TestSLIMustBeWithinTheOperationalContext(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	_, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http, f.redis},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if !errors.Is(err, ErrSLINotInContext) {
		t.Fatalf("got %v, want ErrSLINotInContext", err)
	}
}

// Adding a monitor to the operational context must not change the SLI. This is the sharpest
// test of whether Service is a reliability-domain object or a folder for monitors.
func TestAddingToContextDoesNotChangeTheSLI(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	_, epoch1, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, epoch2, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(epoch2.Members) != len(epoch1.Members) {
		t.Fatalf("adding a diagnostic changed the reliability inputs: %d -> %d members",
			len(epoch1.Members), len(epoch2.Members))
	}
	if epoch2.SnapshotHash != epoch1.SnapshotHash {
		t.Error("adding a diagnostic changed the evaluation snapshot; availability would have silently drifted")
	}

	// ...and yet the second revision STILL gets its own epoch. The snapshot-hash no-op rule
	// belongs to epochs driven by a monitor EXECUTION write; applying it to a declaration
	// write leaves a revision no fact can reference, which is the failure this pairing
	// exists to prevent. An identical hash and a distinct epoch are both required.
	if epoch2.ID == epoch1.ID || epoch2.ID == "" {
		t.Fatalf("the second declaration reused or skipped its epoch (%q vs %q)", epoch2.ID, epoch1.ID)
	}
	var revisions, epochs int
	if err := st.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM service_definition_revisions WHERE service_id=$1),
		        (SELECT count(*) FROM service_evaluation_epochs   WHERE service_id=$1)`,
		f.serviceID).Scan(&revisions, &epochs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if revisions != epochs {
		t.Errorf("%d revisions but %d epochs: the no-op rule was applied to a declaration write", revisions, epochs)
	}
}

// The current reference set is what carries the delete guard, and it must track the
// declaration rather than accumulate.
func TestMemberRefsTrackTheDeclaration(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The in-force SLI names checkout-http, so deleting it must fail at commit.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitors WHERE id=$1`, f.http); err == nil {
		t.Error("deleting a monitor named by the in-force SLI was allowed")
	}
	// checkout-redis is only operational context, but it is still a reference.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitors WHERE id=$1`, f.redis); err == nil {
		t.Error("deleting a monitor named by the in-force context was allowed")
	}

	// Drop redis from the declaration; the reference goes with it and the delete is free.
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitors WHERE id=$1`, f.redis); err != nil {
		t.Errorf("a monitor no declaration references could not be deleted: %v", err)
	}
}

// A service with no reliability inputs is a valid state: operational context, no SLO. It
// still gets a revision and a matching epoch, so the declaration history stays continuous
// and every later revision has a predecessor.
func TestEmptySLIIsValidAndStillGetsAnEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	rev, epoch, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: nil,
	}, 0, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("an empty sli[] was rejected: %v", err)
	}
	if len(epoch.Members) != 0 {
		t.Errorf("the snapshot has %d members for an empty sli[]", len(epoch.Members))
	}
	if epoch.RevisionID != rev.ID {
		t.Error("the empty declaration got no matching epoch")
	}
}

// Credential material must never reach a snapshot. The read goes through the decrypt-free
// scanner, so this is a schema guarantee rather than a filtering one.
func TestEpochSnapshotCarriesNoSecretMaterial(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	redisMon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "cache", Type: domain.MonitorRedis,
		Target: "redis://cache:6379", IntervalSeconds: 30, Region: "core", Enabled: true,
		Config: map[string]string{"password": "hunter2"},
	})
	if err != nil {
		t.Skipf("redis monitor with an inline credential is not accepted here: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{redisMon.ID}, SLI: []string{redisMon.ID},
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	var snapshot string
	if err := st.pool.QueryRow(ctx,
		`SELECT snapshot::text FROM service_evaluation_epochs WHERE service_id=$1`, f.serviceID).Scan(&snapshot); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(snapshot, "hunter2") {
		t.Fatal("credential plaintext reached the stored epoch snapshot")
	}
}

// CeilToBucket's equality case, stated in a unit test because it is where two
// implementations diverge — and because an earlier draft of the design called this
// operation "floor" while giving the ceiling's answer.
func TestCeilToBucketEqualityCase(t *testing.T) {
	onBoundary := time.Date(2026, 8, 16, 12, 1, 0, 0, time.UTC)
	if got := domain.CeilToBucket(onBoundary); !got.Equal(onBoundary) {
		t.Errorf("a write exactly on a boundary uses that boundary, got %s", got)
	}
	inside := time.Date(2026, 8, 16, 12, 0, 30, 0, time.UTC)
	if got := domain.CeilToBucket(inside); !got.Equal(onBoundary) {
		t.Errorf("12:00:30 governs from 12:01:00, got %s", got)
	}
	if got := domain.FloorToBucket(inside); !got.Equal(time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("floor of 12:00:30 is 12:00:00, got %s", got)
	}
}

// Only the FIRST revision may reach backwards. A later revision doing so would rewrite
// facts another declaration already produced, which is an audited administrative repair
// rather than an ordinary edit.
func TestOnlyTheFirstRevisionMayBeRetroactive(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx)

	rev1, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-90 * time.Minute)})
	if err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	if !rev1.EffectiveAt.Before(rev1.CreatedAt) {
		t.Errorf("a backfilled first revision must already be in force over the range it adopts: effective %s, created %s",
			rev1.EffectiveAt, rev1.CreatedAt)
	}
	if !rev1.EffectiveAt.Equal(rev1.EffectiveAt.Truncate(time.Minute)) {
		t.Errorf("a retroactive effective_at is FLOORED to a boundary, got %s", rev1.EffectiveAt)
	}

	_, _, err = st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 1, DeclarationOptions{CreatedBy: "op", BackfillFrom: time.Now().UTC().Add(-30 * time.Minute)})
	if !errors.Is(err, ErrRetroactiveNotFirstRevision) {
		t.Fatalf("a later retroactive revision returned %v, want ErrRetroactiveNotFirstRevision", err)
	}
}
