package store

import (
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D15: `cerbix_changes_retained` is fed by CountServiceChanges — every row of
// service_changes still held, across services and projects, after the retention pass. Rows the
// pass removes leave the count; rows it keeps (a young terminal on an old started) stay in it.
func TestCountServiceChangesFollowsTheRetentionPass(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	if n, err := st.CountServiceChanges(ctx); err != nil || n != 0 {
		t.Fatalf("empty table: n=%d err=%v", n, err)
	}
	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-8*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseFailed, now.Add(-5*time.Minute)))
	// A row of ANOTHER project counts too: the gauge is the table's, not a tenant's.
	plantChangeRow(t, st, ctx, f.otherProjectID, f.otherServiceID, "gitlab", "job-9", domain.ChangeKindFlag, domain.ChangePhaseSucceeded, now.Add(-2*time.Minute))
	// An old group the pass will remove whole, and an old started whose terminal is young (kept).
	old := now.Add(-500 * 24 * time.Hour)
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "github-actions", "run-old", domain.ChangeKindDeploy, domain.ChangePhaseStarted, old)
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "github-actions", "run-old", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, old.Add(time.Minute))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "github-actions", "run-long", domain.ChangeKindDeploy, domain.ChangePhaseStarted, old)
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "github-actions", "run-long", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-time.Minute))

	if n, err := st.CountServiceChanges(ctx); err != nil || n != 8 {
		t.Fatalf("before the pass: n=%d err=%v, want 8", n, err)
	}
	groups, rows, err := st.PurgeChangeGroups(ctx, now.Add(-400*24*time.Hour), 250)
	if err != nil || groups != 1 || rows != 2 {
		t.Fatalf("purge = %d groups / %d rows (%v), want the one old group and its two rows", groups, rows, err)
	}
	if n, err := st.CountServiceChanges(ctx); err != nil || n != 6 {
		t.Fatalf("after the pass: n=%d err=%v, want 6 (run-long kept whole by its young terminal)", n, err)
	}
}
