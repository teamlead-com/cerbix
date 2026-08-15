package store

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func epochCount(t *testing.T, st *Store, ctx context.Context, serviceID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_evaluation_epochs WHERE service_id=$1 AND state='effective'`,
		serviceID).Scan(&n); err != nil {
		t.Fatalf("count epochs: %v", err)
	}
	return n
}

func currentEpoch(t *testing.T, st *Store, ctx context.Context, serviceID string) (id, revisionID, hash string) {
	t.Helper()
	if err := st.pool.QueryRow(ctx,
		`SELECT id, revision_id, snapshot_hash FROM service_evaluation_epochs
		  WHERE service_id=$1 AND state='effective'
		  ORDER BY effective_at DESC, epoch_seq DESC LIMIT 1`, serviceID).Scan(&id, &revisionID, &hash); err != nil {
		t.Fatalf("current epoch: %v", err)
	}
	return
}

// declaredService seeds a service whose SLI is one HTTP monitor, and returns both.
func declaredService(t *testing.T, st *Store, ctx context.Context) (f declFixture, monitor domain.Monitor) {
	t.Helper()
	f = seedDeclaration(t, st, ctx)
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis}, SLI: []string{f.http},
	}, 0, DeclarationOptions{CreatedBy: "op"}); err != nil {
		t.Fatalf("declaration: %v", err)
	}
	m, err := st.GetMonitor(ctx, f.http)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	return f, m
}

// A target change bumps execution_revision and MUST produce an epoch: it is exactly what
// makes two availability numbers incomparable, and a narrower snapshot would have missed it.
func TestTargetChangeCreatesAnEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)
	beforeID, beforeRev, beforeHash := currentEpoch(t, st, ctx, f.serviceID)

	m.Target = "https://checkout.example.com/healthz-v2"
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update monitor: %v", err)
	}

	// The assertion is on IDENTITY, not on a count. When the monitor write lands in the
	// same minute as the declaration it shares that boundary, so the new epoch DISPLACES the
	// old one and the effective count stays at one — accumulating there would mean two
	// epochs governing one bucket.
	afterID, afterRev, afterHash := currentEpoch(t, st, ctx, f.serviceID)
	if afterID == beforeID {
		t.Fatal("a target change produced no new epoch")
	}
	if afterHash == beforeHash {
		t.Error("the snapshot hash did not move on a target change")
	}
	if afterRev != beforeRev {
		t.Error("an execution change must not change which DECLARATION governs; only the epoch is new")
	}
}

// A rename bumps execution_revision under the coarse D-0142 fence and changes nothing the
// evaluator reads. Creating an epoch for it would make the timeline unreadable — this is
// where the snapshot-hash no-op rule belongs.
func TestRenameCreatesNoEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)
	before := epochCount(t, st, ctx, f.serviceID)

	beforeID, _, beforeHash := currentEpoch(t, st, ctx, f.serviceID)

	m.Name = "checkout-http renamed"
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update monitor: %v", err)
	}
	afterID, _, afterHash := currentEpoch(t, st, ctx, f.serviceID)
	if afterID != beforeID || afterHash != beforeHash {
		t.Error("a rename created an epoch for a change the evaluator never reads")
	}
	if got := epochCount(t, st, ctx, f.serviceID); got != before {
		t.Errorf("effective epochs = %d, want %d", got, before)
	}
}

// Confirmation and failure thresholds change cadence and alerting, not measured state.
func TestConfirmationThresholdChangeCreatesNoEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)
	before := epochCount(t, st, ctx, f.serviceID)

	beforeID, _, beforeHash := currentEpoch(t, st, ctx, f.serviceID)

	m.FailureThreshold = 5
	m.ConfirmIntervalSeconds = 10
	m.RenotifySeconds = 900
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update monitor: %v", err)
	}
	afterID, _, afterHash := currentEpoch(t, st, ctx, f.serviceID)
	if afterID != beforeID || afterHash != beforeHash {
		t.Error("confirmation is not an evaluator input, yet it produced an epoch")
	}
	if got := epochCount(t, st, ctx, f.serviceID); got != before {
		t.Errorf("effective epochs = %d, want %d", got, before)
	}
}

// Interval and region are inputs the evaluator reads, so each moves the snapshot.
func TestIntervalAndRegionChangesCreateEpochs(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)

	beforeID, _, beforeHash := currentEpoch(t, st, ctx, f.serviceID)
	m.IntervalSeconds = 300
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("interval: %v", err)
	}
	afterID, _, afterHash := currentEpoch(t, st, ctx, f.serviceID)
	if afterID == beforeID || afterHash == beforeHash {
		t.Fatal("an interval change produced no new epoch: cadence decides how long a result is held")
	}

	m, err := st.GetMonitor(ctx, f.http)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	beforeID, beforeHash = afterID, afterHash
	m.Region = "geo1"
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("region: %v", err)
	}
	afterID, _, afterHash = currentEpoch(t, st, ctx, f.serviceID)
	if afterID == beforeID || afterHash == beforeHash {
		t.Fatal("a region change produced no new epoch: the vantage point is an evaluator input")
	}
}

// Only `sli` membership counts. Operational context decides what is SHOWN on a service; a
// change to a diagnostic monitor is not an evaluator input and must create nothing.
func TestDiagnosticMonitorChangeCreatesNoEpoch(t *testing.T) {
	st, ctx := declStore(t)
	f, _ := declaredService(t, st, ctx)
	before := epochCount(t, st, ctx, f.serviceID)

	redis, err := st.GetMonitor(ctx, f.redis)
	if err != nil {
		t.Fatalf("get redis: %v", err)
	}
	beforeID, _, beforeHash := currentEpoch(t, st, ctx, f.serviceID)

	redis.Target = "tcp://cache-2:6379"
	if _, err := st.UpdateMonitor(ctx, redis); err != nil {
		t.Fatalf("update redis: %v", err)
	}
	afterID, _, afterHash := currentEpoch(t, st, ctx, f.serviceID)
	if afterID != beforeID || afterHash != beforeHash {
		t.Error("a diagnostic-only monitor produced an epoch; operational context is not an evaluator input")
	}
	if got := epochCount(t, st, ctx, f.serviceID); got != before {
		t.Errorf("effective epochs = %d, want %d", got, before)
	}
}

// A monitor in no service's SLI writes nothing at all. This is what makes "zero services
// costs nothing" a property rather than a claim.
func TestMonitorInNoServiceWritesNothing(t *testing.T) {
	st, ctx := declStore(t)
	f := seedDeclaration(t, st, ctx) // service exists, but no declaration was written

	m, err := st.GetMonitor(ctx, f.http)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	m.Target = "https://elsewhere.example.com/"
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := epochCount(t, st, ctx, f.serviceID); got != 0 {
		t.Errorf("%d epochs for a service with no declaration", got)
	}
}

// The load-bearing case of §6.2: an execution-driven epoch must resolve the declaration in
// force AT ITS OWN boundary, including a revision that is pending and not yet in effect.
//
// A declaration write and a monitor write inside the same minute both target the next
// boundary. The monitor write's epoch displaces the declaration's epoch — and if it resolved
// "the revision active right now" it would point at the PREVIOUS revision, leaving that
// boundary governed by the new declaration with its only epoch resolving to the old one. The
// foreign key would hold and the meaning would not.
func TestExecutionEpochResolvesThePendingDeclarationAtItsOwnBoundary(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)

	// Revision 2, effective at the next boundary.
	rev2, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.http, f.redis, f.synthetic}, SLI: []string{f.http, f.synthetic},
	}, 1, DeclarationOptions{CreatedBy: "op"})
	if err != nil {
		t.Fatalf("revision 2: %v", err)
	}

	// A monitor write in the same minute, landing on the same boundary.
	m.Target = "https://checkout.example.com/healthz-v3"
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update monitor: %v", err)
	}

	epochID, revisionID, _ := currentEpoch(t, st, ctx, f.serviceID)
	var epochEffective, revEffective time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT effective_at FROM service_evaluation_epochs WHERE id=$1`, epochID).Scan(&epochEffective); err != nil {
		t.Fatalf("epoch effective_at: %v", err)
	}
	revEffective = rev2.EffectiveAt
	if !epochEffective.Equal(revEffective) {
		t.Skip("the declaration and the monitor write landed in different minutes; this case needs one boundary")
	}
	if revisionID != rev2.ID {
		t.Fatalf("the surviving epoch resolves to revision %s, want the pending revision %s that governs this boundary",
			revisionID, rev2.ID)
	}

	// And exactly one effective epoch on that boundary, as the partial unique index requires.
	var effective int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_evaluation_epochs
		  WHERE service_id=$1 AND effective_at=$2 AND state='effective'`,
		f.serviceID, epochEffective).Scan(&effective); err != nil {
		t.Fatalf("count: %v", err)
	}
	if effective != 1 {
		t.Errorf("%d effective epochs on one boundary", effective)
	}
}

// An execution change creates epochs for every referencing service and touches no
// declaration: the two axes stay independent, which is what lets an operator edit a monitor
// that a file-owned service references.
func TestExecutionChangeLeavesDeclarationsUntouched(t *testing.T) {
	st, ctx := declStore(t)
	f, m := declaredService(t, st, ctx)

	var beforeRevisions int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_definition_revisions WHERE service_id=$1`, f.serviceID).Scan(&beforeRevisions); err != nil {
		t.Fatalf("count revisions: %v", err)
	}

	m.TimeoutSeconds = 9
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}

	var afterRevisions int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_definition_revisions WHERE service_id=$1`, f.serviceID).Scan(&afterRevisions); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if afterRevisions != beforeRevisions {
		t.Errorf("an execution change authored %d declaration revisions; it must author none",
			afterRevisions-beforeRevisions)
	}

	// ...and the declaration must still GOVERN. Counting rows is not enough: an earlier
	// version of this path reused the declaration-path supersede helper, so a monitor edit
	// left the revision row in place while marking it as never having taken effect. The
	// count was unchanged and the service had silently lost its declaration — after which
	// the next execution change found nothing to resolve and created no epoch at all.
	var state string
	if err := st.pool.QueryRow(ctx,
		`SELECT state FROM service_definition_revisions WHERE service_id=$1 ORDER BY revision DESC LIMIT 1`,
		f.serviceID).Scan(&state); err != nil {
		t.Fatalf("read revision state: %v", err)
	}
	if state != string(domain.RevisionEffective) {
		t.Fatalf("an execution change left the declaration %q; a monitor edit must never mutate a declaration", state)
	}

	// The epoch it created must resolve to that same governing revision.
	_, epochRevision, _ := currentEpoch(t, st, ctx, f.serviceID)
	var governing string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM service_definition_revisions
		  WHERE service_id=$1 AND state='effective' ORDER BY effective_at DESC, revision DESC LIMIT 1`,
		f.serviceID).Scan(&governing); err != nil {
		t.Fatalf("read governing revision: %v", err)
	}
	if epochRevision != governing {
		t.Errorf("the new epoch resolves to %s, but %s governs", epochRevision, governing)
	}
}

// The declared linearization point is the MONITOR WRITE, and it has to hold for every caller
// of the shared write contract — not just the API handler.
//
// The bump sat one level up, on UpdateMonitor, so every file-provider apply advanced
// execution semantics with no epoch at all: target, condition, region and enabled changes
// arriving from a bundle moved what was being measured while the epoch axis said nothing
// happened.
func TestAFileProviderApplyOpensAnEpochJustLikeTheAPIDoes(t *testing.T) {
	st, ctx := declStore(t)
	f := adoptedService(t, st, ctx)

	before := epochCount(t, st, ctx, f.serviceID)

	m, err := st.GetMonitor(ctx, f.http)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	m.IntervalSeconds = m.IntervalSeconds * 2 // changes the staleness deadline => the snapshot

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := updateMonitorTx(ctx, tx, st, m); err != nil {
		t.Fatalf("shared write contract: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if after := epochCount(t, st, ctx, f.serviceID); after == before {
		t.Fatal("a write through the shared contract created no epoch; the linearization point held for one caller only")
	}
}
