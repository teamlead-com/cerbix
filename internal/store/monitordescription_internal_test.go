package store

import (
	"testing"
	"time"
)

// FR-030 invariants 2, 3 and 5 against the real table.

func TestMonitorDescriptionRoundTripsAndReadsEmptyWhenNeverSet(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	plain, err := st.CreateMonitor(ctx, auditedHTTPMonitor(projID))
	if err != nil {
		t.Fatal(err)
	}
	// Invariant 3: a monitor that never had one reads back empty — the column default, not NULL.
	if got, _ := st.GetMonitor(ctx, plain.ID); got.Description != "" {
		t.Fatalf("a monitor created without a description reads %q", got.Description)
	}
	described := auditedHTTPMonitor(projID)
	described.Name = "described"
	described.Description = "Confirms the payment provider can reach our callback URL."
	created, err := st.CreateMonitor(ctx, described)
	if err != nil {
		t.Fatal(err)
	}
	if created.Description != described.Description {
		t.Fatalf("create returned %q", created.Description)
	}
	got, err := st.GetMonitor(ctx, created.ID)
	if err != nil || got.Description != described.Description {
		t.Fatalf("read back %q err=%v", got.Description, err)
	}
	// Update changes it; an empty update clears it.
	got.Description = "A shorter sentence."
	if upd, err := st.UpdateMonitor(ctx, got); err != nil || upd.Description != "A shorter sentence." {
		t.Fatalf("update: %q err=%v", upd.Description, err)
	}
	got.Description = ""
	if upd, err := st.UpdateMonitor(ctx, got); err != nil || upd.Description != "" {
		t.Fatalf("clear: %q err=%v", upd.Description, err)
	}
	if rows := monitorAuditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("fixture hooks audited: %+v", rows)
	}
}

// Invariant 5: the bundle owns the field. A file-managed monitor is read-only in the UI and the API
// (`ErrManagedByFile`), so the ONLY writer of its description is the bundle: a bundle with the key sets
// it, an identical re-apply is not a change, a bundle that changes only the description is a change (the
// canonical hash covers it), and a bundle that drops the key clears it.
func TestABundleOwnsTheDescriptionLikeEveryOtherField(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(ctx, org.ID, "payments", "Payments"); err != nil {
		t.Fatal(err)
	}
	const head = "\nformat: 1\norganization: acme\nproject: payments\nmonitors:\n  api:\n    name: Payments API\n    type: http\n    target: https://payments.internal/health\n    interval: 30s\n    timeout: 5s\n"
	withDesc := head + "    description: Confirms the provider reaches our callback URL.\n"
	res := applyBundle(t, st, ctx, withDesc, time.Hour)
	id, _, _, _, _ := monRow(t, st, ctx, "api")
	m, err := st.GetMonitor(ctx, id)
	if err != nil || m.Description != "Confirms the provider reaches our callback URL." {
		t.Fatalf("bundle did not set the description: %q err=%v (apply=%+v)", m.Description, err, res)
	}
	// Identical re-apply: no change at all.
	if again := applyBundle(t, st, ctx, withDesc, time.Hour); !again.NoChange {
		t.Fatalf("an identical bundle counted as a change: %+v", again)
	}
	// Only the description moves: that IS a change, because the hash covers the field.
	reworded := head + "    description: Reworded.\n"
	if res := applyBundle(t, st, ctx, reworded, time.Hour); res.NoChange {
		t.Fatalf("a description-only change read as no change: %+v", res)
	}
	if m, _ = st.GetMonitor(ctx, id); m.Description != "Reworded." {
		t.Fatalf("after the reworded bundle: %q", m.Description)
	}
	// The UI cannot write it: the monitor is the bundle's.
	m.Description = "set in the UI"
	if _, err := st.UpdateMonitor(ctx, m); err == nil {
		t.Fatal("a file-managed monitor accepted a UI write")
	}
	// The key removed: cleared, and a change.
	if res := applyBundle(t, st, ctx, head, time.Hour); res.NoChange {
		t.Fatalf("dropping the key was not a change: %+v", res)
	}
	if m, _ = st.GetMonitor(ctx, id); m.Description != "" {
		t.Fatalf("the bundle without the key left %q", m.Description)
	}
}
