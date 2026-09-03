package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-026 / NFR-021 against a real database. The property is not "a row appears" — it is that the row
// appears IN THE MUTATING TRANSACTION, carries the principal, and does NOT appear for a machine write
// or for a retry that changed nothing.

func auditIncidentFixture(t *testing.T) (*Store, context.Context, string, string) {
	t.Helper()
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	return st, ctx, org.ID, proj.ID
}

func auditRows(t *testing.T, st *Store, ctx context.Context, orgID string) []struct{ Action, Target string } {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT action, target FROM audit_logs WHERE org_id = $1 AND action LIKE 'incident.%' ORDER BY created_at`, orgID)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []struct{ Action, Target string }
	for rows.Next() {
		var r struct{ Action, Target string }
		if err := rows.Scan(&r.Action, &r.Target); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestEveryPrincipalIncidentWriteIsAudited(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	// A session principal needs a user ROW, because `actor_user_id` is a uuid and a label alone names
	// nobody. The token shape is the one that legitimately has no id.
	user, err := st.CreateLocalUser(ctx, "ops@example.com", "Ops", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	actor := AuditActor{ActorUserID: user.ID, Label: "ops@example.com"}

	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened by hand", "ops@example.com", actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "found it", Author: "ops@example.com",
	}, actor); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "still on it", Author: "ops@example.com",
	}, actor); err != nil {
		t.Fatalf("note: %v", err)
	}
	if _, err := st.AcknowledgeIncidentByPrincipal(ctx, inc.ID, "ops@example.com", actor); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := st.UpsertPostmortemByPrincipal(ctx, inc.ID, "what happened", "ops@example.com", actor); err != nil {
		t.Fatalf("postmortem: %v", err)
	}

	rows := auditRows(t, st, ctx, orgID)
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action)
	}
	want := []string{"incident.create", "incident.status", "incident.note", "incident.acknowledge", "incident.postmortem"}
	if len(actions) != len(want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("actions = %v, want %v", actions, want)
		}
	}
	// D5's shapes, asserted rather than described. A status target names BOTH ends of the
	// transition, read from the row that was locked and never from the request body.
	if rows[1].Target != "incident "+inc.ID+" · investigating → identified" {
		t.Fatalf("status target = %q", rows[1].Target)
	}
	if rows[2].Target != "incident "+inc.ID+" · note" {
		t.Fatalf("note target = %q", rows[2].Target)
	}
	// A note's target carries no excerpt: the audit trail says what happened, never what was written.
	if got := rows[2].Target; got != "incident "+inc.ID+" · note" || len(got) > 80 {
		t.Fatalf("note target leaked content: %q", got)
	}
}

func TestAMachineWriteIsNotAudited(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	inc, err := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: projID, Title: "auto", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "monitor went down", "system")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.AddIncidentUpdateBySystem(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "recovered", Author: "system",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if rows := auditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("a machine write produced %d audit rows: %+v", len(rows), rows)
	}
}

// D8: an audit row means a change HAPPENED. A repeated acknowledgement is a no-op — and to earn that
// claim the WRITE underneath had to become one, which is D8a's correction: it used to rewrite
// `updated_at` on every retry.
func TestARepeatedAcknowledgementIsANoOpAndAuditsNothingTwice(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	actor := AuditActor{ActorUserID: "", ViaToken: true, Label: "token:deploy-bot"}
	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "bot", actor)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.AcknowledgeIncidentByPrincipal(ctx, inc.ID, "bot", actor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AcknowledgeIncidentByPrincipal(ctx, inc.ID, "bot", actor)
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("a repeated acknowledgement moved updated_at: %s → %s", first.UpdatedAt, second.UpdatedAt)
	}
	acks := 0
	for _, r := range auditRows(t, st, ctx, orgID) {
		if r.Action == "incident.acknowledge" {
			acks++
			// A token principal appends its label, because actor_user_id is NULL for a synthetic
			// token identity and the typed half would otherwise read "some token".
			if !strings.Contains(r.Target, "actor=token:deploy-bot") {
				t.Fatalf("a token write must name its label: %q", r.Target)
			}
		}
	}
	if acks != 1 {
		t.Fatalf("acknowledge audit rows = %d, want exactly 1", acks)
	}
}

// D7: an audit insert error aborts the mutation. Proven twice, because the two failures reach the
// helper at different points: an actor a caller got wrong (refused before any statement) and the
// INSERT itself failing (a planted trigger on `audit_logs`). Only the second one distinguishes a
// helper that returns its Exec error from one that swallows it.
func TestAnUnattributableWriteDoesNotHappen(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	// Two shapes that are not attributions: nothing at all, and a bare LABEL — which reads as a
	// session user who has no user row, and names nobody the trail can be followed back to.
	for _, actor := range []AuditActor{{}, {Label: "somebody"}} {
		if _, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
			ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
			Impact: domain.ImpactMajor, Source: domain.SourceManual,
		}, "opened", "ops", actor); err == nil {
			t.Fatalf("actor %+v was accepted", actor)
		}
	}
	_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", AuditActor{})
	if err == nil {
		t.Fatal("a zero-valued actor must be refused before any statement")
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM incidents WHERE project_id = $1`, projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the incident was written anyway: %d rows", n)
	}
	if rows := auditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("an anonymous audit row was written: %+v", rows)
	}
}

// D2's other half: no committed change without its row, and no row for a rolled-back change. The
// failure is PLANTED after the incident insert — a trigger on `incident_updates` — because that is
// where the opening update and the audit row both live, and a rollback has to take all three.
func TestARolledBackCreateLeavesNoIncidentAndNoAuditRow(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	if _, err := st.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION cerbix_test_refuse() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'planted failure'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER cerbix_test_refuse_updates BEFORE INSERT ON incident_updates
		    FOR EACH ROW EXECUTE FUNCTION cerbix_test_refuse();`); err != nil {
		t.Fatalf("plant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS cerbix_test_refuse_updates ON incident_updates`)
	})

	_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", AuditActor{ViaToken: true, Label: "token:test"})
	if err == nil {
		t.Fatal("the planted failure did not surface")
	}
	var incidents int
	if err := st.pool.QueryRow(ctx, `SELECT count(*)::int FROM incidents WHERE project_id = $1`, projID).Scan(&incidents); err != nil {
		t.Fatal(err)
	}
	if incidents != 0 {
		t.Fatalf("the incident survived the rollback: %d rows", incidents)
	}
	if rows := auditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("an audit row survived the rollback: %+v", rows)
	}
}

// NFR-021's first clause, asserted the only way it can be: plant a string in the body and search the
// WHOLE audit row for it, rather than reading the target builder and believing it.
func TestNoBodyEverReachesTheAuditTrail(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}
	const planted = "PLANTED-a1b2c3-not-for-the-trail"

	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opening "+planted, "ops", actor)
	if err != nil {
		t.Fatal(err)
	}
	// A STATUS change and, separately, the two shapes of a plain NOTE — the explicit keep-current and
	// the omitted status. The note branch is a different target builder, and a leak test that only
	// drives status changes never reaches it.
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "status " + planted, Author: "ops",
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentIdentified, Body: "note " + planted, Author: "ops",
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Body: "keep-current note " + planted, Author: "ops",
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "resolved " + planted, Author: "ops",
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPostmortemByPrincipal(ctx, inc.ID, "postmortem "+planted, "ops", actor); err != nil {
		t.Fatal(err)
	}

	rows, err := st.pool.Query(ctx,
		`SELECT coalesce(action,'') || ' ' || coalesce(target,'') FROM audit_logs WHERE org_id = $1`, orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var whole string
		if err := rows.Scan(&whole); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(whole, planted) {
			t.Fatalf("a body reached the audit trail: %q", whole)
		}
	}
	if n != 6 {
		t.Fatalf("audit rows = %d, want 6 — the search proves nothing if the writes did not happen", n)
	}
	// And the keep-current note is a NOTE, not a transition to the empty string.
	var notes int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM audit_logs WHERE org_id = $1 AND action = 'incident.note'`, orgID).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 2 {
		t.Fatalf("incident.note rows = %d, want 2 (the explicit keep-current and the omitted status)", notes)
	}
	var badTransition int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM audit_logs WHERE org_id = $1 AND target LIKE '%% → '`, orgID).Scan(&badTransition); err != nil {
		t.Fatal(err)
	}
	if badTransition != 0 {
		t.Fatalf("%d audit rows record a transition to nothing", badTransition)
	}
}

func TestAPostmortemAuditsCreatedThenUpdated(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}
	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPostmortemByPrincipal(ctx, inc.ID, "first", "ops", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPostmortemByPrincipal(ctx, inc.ID, "second", "ops", actor); err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, r := range auditRows(t, st, ctx, orgID) {
		if r.Action == string(IncidentAuditPostmortem) {
			targets = append(targets, r.Target)
		}
	}
	if len(targets) != 2 || !strings.Contains(targets[0], "postmortem created") || !strings.Contains(targets[1], "postmortem updated") {
		t.Fatalf("postmortem targets = %v, want created then updated", targets)
	}
}

// D2a: the postmortem writer resolves the incident FIRST, inside its own transaction, so an unknown
// incident is `ErrNotFound` from that read rather than a foreign-key error from the upsert, and the
// `created`/`updated` decision is taken while the row is held.
//
// What each half of this test can distinguish, stated plainly. The ErrNotFound half distinguishes the
// order: move the tenant read back after the upsert — the shape before D2a — and it fails. The waiting
// half does NOT distinguish the `FOR UPDATE` itself: `postmortems.incident_id` is a foreign key, so
// the insert takes a KEY SHARE lock on the same row and waits for a held FOR UPDATE either way. It is
// kept because it pins the serialization the decision depends on, not because it proves the keyword —
// a test whose name outruns what it can see is how a mechanism gets claimed and never reached.
func TestThePostmortemResolvesTheIncidentUnderItsOwnTransaction(t *testing.T) {
	st, ctx, _, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}

	if _, err := st.UpsertPostmortemByPrincipal(ctx, "00000000-0000-0000-0000-000000000000", "x", "ops", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown incident = %v, want ErrNotFound", err)
	}

	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the incident's row lock from outside and prove the postmortem waits for it. Without the
	// `FOR UPDATE` the upsert would run straight through and this would not time out.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // the test owns the transaction
	if _, err := tx.Exec(ctx, `SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	_, err = st.UpsertPostmortemByPrincipal(blocked, inc.ID, "body", "ops", actor)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the postmortem did not wait for the incident's row lock: %v", err)
	}
	_ = tx.Rollback(ctx)
	if _, err := st.UpsertPostmortemByPrincipal(ctx, inc.ID, "body", "ops", actor); err != nil {
		t.Fatalf("after the lock is released: %v", err)
	}
}

// incidentAuditCount is the whole-instance count of incident audit rows. The machine cases assert on
// it rather than on one organization's, because a machine writer that audited to the WRONG tenant
// would pass an org-scoped count while still writing the row FR-026 says it must not.
func incidentAuditCount(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM audit_logs WHERE action LIKE 'incident.%'`).Scan(&n); err != nil {
		t.Fatalf("count incident audit rows: %v", err)
	}
	return n
}

// Invariant 7, one case per writer of D1's table. A single "the machine does not audit" test would
// cover whichever writer it happened to call; the table exists because two of these live in different
// files with different idempotency keys, and one is a direct insert that no marker guard sees.
func TestNoMachineWriterAudits(t *testing.T) {
	t.Run("1+2 the reconciler's doors: auto-open and auto-resolve", func(t *testing.T) {
		st, ctx := outboxTestStore(t)
		org, _ := st.CreateOrganization(ctx, "acme", "Acme")
		proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
		mon, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://api.example.com/",
			IntervalSeconds: 30, Region: "core", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		inc, err := st.CreateIncidentBySystem(ctx, domain.Incident{
			ProjectID: proj.ID, MonitorID: mon.ID, Title: "api is down",
			Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
		}, "monitor api went down", "system")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddIncidentUpdateBySystem(ctx, domain.IncidentUpdate{
			IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "monitor api recovered", Author: "system",
		}); err != nil {
			t.Fatal(err)
		}
		// The work IS recorded — in the timeline, which is the document an operator reads for it.
		if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_updates WHERE incident_id = $1 AND author = 'system'`, inc.ID); n != 2 {
			t.Fatalf("system timeline entries = %d, want 2", n)
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the reconciler's doors wrote %d audit rows", n)
		}
	})

	t.Run("3+4 the service auto-incident and its auto-resolve", func(t *testing.T) {
		st, ctx := declStore(t)
		f := changeService(t, st, ctx)
		inc := openServiceIncidentAt(t, st, ctx, f, gateDBNow(t, st, ctx))

		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		did, err := resolveServiceIncidentTx(ctx, tx, f.serviceID, "the service recovered")
		if err != nil || !did {
			t.Fatalf("resolve = %v %v", did, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if inc == "" {
			t.Fatal("fixture: no incident")
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the service lifecycle wrote %d audit rows", n)
		}
	})

	t.Run("5 the ⚡ Context: note", func(t *testing.T) {
		st, ctx := outboxTestStore(t)
		org, _ := st.CreateOrganization(ctx, "acme", "Acme")
		proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
		inc, err := st.CreateIncidentBySystem(ctx, domain.Incident{
			ProjectID: proj.ID, Title: "auto", Status: domain.IncidentInvestigating,
			Impact: domain.ImpactMajor, Source: domain.SourceAuto,
		}, "opened", "system")
		if err != nil {
			t.Fatal(err)
		}
		added, err := st.AppendIncidentContext(ctx, inc.ID, domain.IncidentContextMarker+" 3 of 4 regions failing")
		if err != nil || !added {
			t.Fatalf("context note = %v %v", added, err)
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the context note wrote %d audit rows", n)
		}
	})

	t.Run("6a the ⏸ Suppressed: note, dependency flavour, on the pool", func(t *testing.T) {
		st, ctx := outboxTestStore(t)
		org, _ := st.CreateOrganization(ctx, "acme", "Acme")
		proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
		mon, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://api.example.com/",
			IntervalSeconds: 30, Region: "core", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateIncidentBySystem(ctx, domain.Incident{
			ProjectID: proj.ID, MonitorID: mon.ID, Title: "api is down",
			Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
		}, "opened", "system"); err != nil {
			t.Fatal(err)
		}
		added, err := st.AppendSuppressionNote(ctx, mon.ID, "database")
		if err != nil || !added {
			t.Fatalf("suppression note = %v %v", added, err)
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the dependency suppression note wrote %d audit rows", n)
		}
	})

	t.Run("6b the ⏸ Suppressed: note, delegation flavour, in a transaction", func(t *testing.T) {
		st, ctx := outboxTestStore(t)
		org, _ := st.CreateOrganization(ctx, "acme", "Acme")
		proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
		mon, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://api.example.com/",
			IntervalSeconds: 30, Region: "core", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateIncidentBySystem(ctx, domain.Incident{
			ProjectID: proj.ID, MonitorID: mon.ID, Title: "api is down",
			Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
		}, "opened", "system"); err != nil {
			t.Fatal(err)
		}
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		added, err := appendSuppressionNoteTx(ctx, tx, mon.ID, []string{"Checkout"})
		if err != nil || !added {
			t.Fatalf("delegation note = %v %v", added, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the delegation suppression note wrote %d audit rows", n)
		}
	})

	t.Run("7a the 🚀 Changes: note", func(t *testing.T) {
		st, ctx := declStore(t)
		f := changeService(t, st, ctx)
		opened := gateDBNow(t, st, ctx)
		plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "run-1",
			domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, opened.Add(-10*time.Minute))
		inc := openServiceIncidentAt(t, st, ctx, f, opened)
		res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
		if err != nil || !res.NoteAdded || len(res.Links) == 0 {
			t.Fatalf("correlation = %+v %v — the case proves nothing if nothing was written", res, err)
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("the changes note wrote %d audit rows", n)
		}
	})

	t.Run("7b CorrelateIncident's links and its 🕸 Impact: note", func(t *testing.T) {
		st, ctx := serviceSchemaStore(t)
		f := seedImpact(t, st, ctx, "database", "checkout")
		f.edge(t, st, ctx, "checkout", "database", 0)
		parent := f.open(t, st, ctx, "database")
		child := f.open(t, st, ctx, "checkout")
		if _, _, err := st.CorrelateIncident(ctx, parent.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CorrelateIncident(ctx, child.ID); err != nil {
			t.Fatal(err)
		}
		// Assert on what was WRITTEN, not on what was returned: the case proves nothing unless the
		// direct insert and its note actually happened.
		if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_service_impacts`); n == 0 {
			t.Fatal("no impact link was written")
		}
		if notes := impactNotes(t, st, ctx, child.ID); len(notes) == 0 {
			t.Fatal("no 🕸 Impact: note was written")
		}
		if n := incidentAuditCount(t, st, ctx); n != 0 {
			t.Fatalf("impact correlation wrote %d audit rows", n)
		}
	})
}

// D8b's race, against the real partial unique index. Two firings of one fingerprint arrive together;
// both miss the "already open" read; one wins the index and one must lose it BENIGNLY — as
// `ErrAlreadyOpen`, the same answer the sequential retry gets — instead of reaching the generic error
// path, where the receiver answered 500.
func TestAConcurrentDuplicateFiringLosesBenignly(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:alertmanager"}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
				ProjectID: projID, Title: "high latency", Status: domain.IncidentInvestigating,
				Impact: domain.ImpactCritical, Source: domain.SourceAPI, ExternalKey: "fp-1",
			}, "fired", "alertmanager", actor)
			errs <- err
		}()
	}
	close(start)
	var won, lost int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, ErrAlreadyOpen):
			lost++
		default:
			t.Fatalf("the loser of the race got %v, want ErrAlreadyOpen", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won=%d lost=%d, want exactly one of each", won, lost)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incidents WHERE external_key = 'fp-1'`); n != 1 {
		t.Fatalf("incidents for one fingerprint = %d, want 1", n)
	}
	// One mutation, one audit row: the loser wrote nothing at all.
	if rows := auditRows(t, st, ctx, orgID); len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1: %+v", len(rows), rows)
	}
}

// D8a's other half — the property the no-op guard must not break. An acknowledgement that WAITED
// behind a timeline update stamps forward, never behind the update it queued for.
func TestAnAcknowledgementThatWaitsStillStampsForward(t *testing.T) {
	st, ctx, _, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}
	inc, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the incident's row lock, start the acknowledgement behind it, then let the timeline write
	// commit. The acknowledgement's clock is read AFTER it gets the lock, so its stamp is later.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A row lock held past the end of this test would make the NEXT test's TruncateAll wait on it, so
	// the rollback is deferred as well as committed: a `t.Fatal` between here and the commit must not
	// leave the table locked for the rest of the package.
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, aerr := st.AcknowledgeIncidentByPrincipal(ctx, inc.ID, "ops", actor)
		done <- aerr
	}()
	time.Sleep(150 * time.Millisecond)
	var updatedAt time.Time
	if err := tx.QueryRow(ctx,
		`UPDATE incidents SET updated_at = statement_timestamp() WHERE id = $1 RETURNING updated_at`,
		inc.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	after, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.After(updatedAt) {
		t.Fatalf("the acknowledgement stamped %s, behind the update it waited for (%s)", after.UpdatedAt, updatedAt)
	}
}

// Invariant 5 and the tenancy half of §7: the organization is resolved from the project IN the insert,
// so an incident with no anchor and one whose service was deleted both land in the right trail — and
// another organization's reader never sees them.
func TestAnAuditRowLandsInTheIncidentsOwnOrganization(t *testing.T) {
	st, ctx := declStore(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}

	orgA, _ := st.CreateOrganization(ctx, "acme", "Acme")
	projA, _ := st.CreateProject(ctx, orgA.ID, "api", "API")
	orgB, _ := st.CreateOrganization(ctx, "globex", "Globex")
	projB, _ := st.CreateProject(ctx, orgB.ID, "web", "Web")

	// An incident with NO anchor.
	if _, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projA.ID, Title: "project-level", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor); err != nil {
		t.Fatal(err)
	}
	// An incident anchored to a SERVICE that is then deleted: the anchor is gone, the project is not,
	// and the trail is the organization's either way.
	svc, err := st.CreateService(ctx, domain.Service{ProjectID: projA.ID, Slug: "checkout", Name: "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	anchored, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projA.ID, ServiceID: svc.ID, Title: "service", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, svc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddIncidentUpdateByPrincipal(ctx, domain.IncidentUpdate{
		IncidentID: anchored.ID, Status: domain.IncidentIdentified, Body: "after the service went away", Author: "ops",
	}, actor); err != nil {
		t.Fatalf("a write to an orphaned incident: %v", err)
	}

	if n := len(auditRows(t, st, ctx, orgA.ID)); n != 3 {
		t.Fatalf("organization A's trail = %d rows, want 3", n)
	}
	if n := len(auditRows(t, st, ctx, orgB.ID)); n != 0 {
		t.Fatalf("organization B sees %d of A's rows", n)
	}
	_ = projB

	// D9: the rows appear in the organization listing that already exists, and in no instance listing.
	entries, err := st.ListAuditByOrg(ctx, orgA.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	incidentEntries := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "incident.") {
			incidentEntries++
		}
	}
	if incidentEntries != 3 {
		t.Fatalf("the organization listing shows %d incident rows, want 3", incidentEntries)
	}
	global, err := st.ListGlobalAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range global {
		if strings.HasPrefix(e.Action, "incident.") {
			t.Fatalf("an incident row reached the INSTANCE listing: %+v", e)
		}
	}
}

// Invariant 16: the trail outlives what it describes. Deleting the project takes the incident and
// leaves the organization's rows; only deleting the organization removes them.
func TestAuditRowsOutliveTheIncidentAndItsProject(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	actor := AuditActor{ViaToken: true, Label: "token:test"}
	if _, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projID); err != nil {
		t.Fatal(err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incidents`); n != 0 {
		t.Fatalf("the project's incidents survived it: %d", n)
	}
	if n := len(auditRows(t, st, ctx, orgID)); n != 1 {
		t.Fatalf("the trail did not outlive the project: %d rows", n)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if n := incidentAuditCount(t, st, ctx); n != 0 {
		t.Fatalf("deleting the organization left %d rows", n)
	}
}

// Invariant 6: the actor triple, one case per principal kind that exists. `actor_user_id` is NULL
// EXACTLY for the synthetic token identity — an OIDC client-credentials principal has a real user row
// and `via_token` true, so "NULL means token" would be wrong and "via_token means NULL" would be too.
func TestTheActorTripleMatchesThePrincipalKind(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	user, err := st.CreateLocalUser(ctx, "ops@example.com", "Ops", "x", false)
	if err != nil {
		t.Fatal(err)
	}

	kinds := []struct {
		name       string
		actor      AuditActor
		wantUserID bool
		wantToken  bool
	}{
		{"session user", AuditActor{ActorUserID: user.ID, Label: "ops@example.com"}, true, false},
		{"cerbix token", AuditActor{ViaToken: true, Label: "token:deploy-bot"}, false, true},
		{"oidc client credentials", AuditActor{ActorUserID: user.ID, ViaToken: true, Label: "ops@example.com"}, true, true},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, `DELETE FROM audit_logs WHERE org_id = $1`, orgID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
				ProjectID: projID, Title: k.name, Status: domain.IncidentInvestigating,
				Impact: domain.ImpactMajor, Source: domain.SourceManual,
			}, "opened", "ops", k.actor); err != nil {
				t.Fatal(err)
			}
			var gotUser *string
			var gotToken bool
			var target string
			if err := st.pool.QueryRow(ctx,
				`SELECT actor_user_id, via_token, target FROM audit_logs WHERE org_id = $1`, orgID).
				Scan(&gotUser, &gotToken, &target); err != nil {
				t.Fatal(err)
			}
			if (gotUser != nil) != k.wantUserID {
				t.Fatalf("actor_user_id present = %v, want %v", gotUser != nil, k.wantUserID)
			}
			if gotToken != k.wantToken {
				t.Fatalf("via_token = %v, want %v", gotToken, k.wantToken)
			}
			if k.wantToken != strings.Contains(target, "actor=") {
				t.Fatalf("target = %q for a %s", target, k.name)
			}
		})
	}

	// Deleting the user leaves the row with a NULL actor and its target intact (invariant 16).
	if _, err := st.pool.Exec(ctx, `DELETE FROM audit_logs WHERE org_id = $1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "by a user about to leave", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", AuditActor{ActorUserID: user.ID, Label: "ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	var gotUser *string
	var target string
	if err := st.pool.QueryRow(ctx,
		`SELECT actor_user_id, target FROM audit_logs WHERE org_id = $1`, orgID).Scan(&gotUser, &target); err != nil {
		t.Fatalf("the row did not outlive its actor: %v", err)
	}
	if gotUser != nil || !strings.Contains(target, "incident ") {
		t.Fatalf("after deleting the actor: user=%v target=%q", gotUser, target)
	}
}

func TestAnAuditInsertFailureAbortsTheMutation(t *testing.T) {
	st, ctx, orgID, projID := auditIncidentFixture(t)
	if _, err := st.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION cerbix_test_refuse_audit() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'planted audit failure'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER cerbix_test_refuse_audit_rows BEFORE INSERT ON audit_logs
		    FOR EACH ROW EXECUTE FUNCTION cerbix_test_refuse_audit();`); err != nil {
		t.Fatalf("plant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS cerbix_test_refuse_audit_rows ON audit_logs`)
	})

	_, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: projID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "ops", AuditActor{ViaToken: true, Label: "token:test"})
	if err == nil {
		t.Fatal("the mutation committed although its audit row could not be written")
	}
	if !strings.Contains(err.Error(), "audit incident.create") {
		t.Fatalf("the error does not name the audit insert: %v", err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incidents WHERE project_id = $1`, projID); n != 0 {
		t.Fatalf("the incident was written without its audit row: %d rows", n)
	}
	if rows := auditRows(t, st, ctx, orgID); len(rows) != 0 {
		t.Fatalf("audit rows exist: %+v", rows)
	}
}
