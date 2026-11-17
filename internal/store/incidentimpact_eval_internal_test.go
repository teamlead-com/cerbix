package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.3 — the rise as the LIVE evaluator runs it: on every confirmed pageable state, before
// the withheld/notify branching (D13), counted in the pass's summary (D20).

// openDirect opens the service's auto-incident at `state` under `m` in its own committed transaction.
// The rise reads only the incident, so the projection does not have to produce `state`.
func openDirect(t *testing.T, st *Store, ctx context.Context, f alertFixture,
	state domain.ServiceAlertState, m domain.ImpactMapping) domain.Incident {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	inc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID,
		"checkout "+string(state), state, m, 2, nil)
	if err != nil || !created {
		t.Fatalf("open at %s: created=%v err=%v", state, created, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inc
}

// raisedEvents counts the incident's `incident.updated` events that carry `impact_raised`.
func raisedEvents(t *testing.T, st *Store, ctx context.Context, incidentID string) int {
	t.Helper()
	n := 0
	for _, e := range riseEvents(t, st, ctx, incidentID) {
		if e.raised != "" {
			n++
		}
	}
	return n
}

func incidentImpact(t *testing.T, st *Store, ctx context.Context, incidentID string) domain.IncidentImpact {
	t.Helper()
	var impact domain.IncidentImpact
	if err := st.pool.QueryRow(ctx, `SELECT impact FROM incidents WHERE id = $1`, incidentID).Scan(&impact); err != nil {
		t.Fatalf("read impact: %v", err)
	}
	return impact
}

// timelineLen counts the incident's timeline entries of every kind.
func timelineLen(t *testing.T, st *Store, ctx context.Context, incidentID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incident_updates WHERE incident_id = $1`,
		incidentID).Scan(&n); err != nil {
		t.Fatalf("count timeline: %v", err)
	}
	return n
}

// I2, I6 — the unconfirmed pass raises nothing, the confirming pass raises once, and a
// re-evaluation of the same state writes nothing.
func TestTheEvaluatorRaisesOnAConfirmedPageableState(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	inc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	setMemberHealth(t, st, ctx, f, false)

	if got := evalOnce(t, st, ctx); got.IncidentsRaised != 0 {
		t.Fatalf("pass 1 (streak 1 of 2): raised %d, want 0 — an unconfirmed state rose", got.IncidentsRaised)
	}
	if got := incidentImpact(t, st, ctx, inc.ID); got != domain.ImpactMinor {
		t.Fatalf("after pass 1 impact %q, want minor", got)
	}

	notesBefore := timelineLen(t, st, ctx, inc.ID)
	got := evalOnce(t, st, ctx)
	if got.IncidentsRaised != 1 {
		t.Fatalf("pass 2 (confirmed): raised %d, want 1 (%+v)", got.IncidentsRaised, got)
	}
	if got := incidentImpact(t, st, ctx, inc.ID); got != domain.ImpactMajor {
		t.Fatalf("after pass 2 impact %q, want major", got)
	}
	if n := len(riseNotes(t, st, ctx, inc.ID)); n != 1 {
		t.Fatalf("after pass 2 rise notes = %d, want 1", n)
	}
	// The rise and the onset share this evaluation: the onset finds the incident open and adds
	// no second opening note, and the timeline gained exactly the one rise note.
	if got.Onsets != 1 || got.IncidentsOpened != 0 {
		t.Fatalf("pass 2: onsets=%d opened=%d, want 1 and 0 (%+v)", got.Onsets, got.IncidentsOpened, got)
	}
	if n := timelineLen(t, st, ctx, inc.ID); n != notesBefore+1 {
		t.Fatalf("pass 2 timeline entries = %d, want %d (exactly the one rise note)", n, notesBefore+1)
	}

	if got := evalOnce(t, st, ctx); got.IncidentsRaised != 0 {
		t.Fatalf("pass 3 (same state): raised %d, want 0", got.IncidentsRaised)
	}
	if n := len(riseNotes(t, st, ctx, inc.ID)); n != 1 {
		t.Errorf("after pass 3 rise notes = %d, want 1 (I6: a re-evaluation writes no note)", n)
	}
	if n := raisedEvents(t, st, ctx, inc.ID); n != 1 {
		t.Errorf("after pass 3 rise events = %d, want 1 (I6: a re-evaluation writes no event)", n)
	}
}

// D13 — the rise follows the CONFIRMED state: an onset withheld for want of a route still raises.
func TestARiseFollowsTheConfirmedStateWhileTheOnsetIsWithheld(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	inc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("disable channels: %v", err)
	}
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)

	got := evalOnce(t, st, ctx)
	if got.Withheld[WithheldUnroutable] != 1 || got.Onsets != 0 {
		t.Fatalf("the onset was not withheld as unroutable: %+v", got)
	}
	if got.IncidentsRaised != 1 {
		t.Fatalf("raised %d, want 1: a withheld onset still raises (D13)", got.IncidentsRaised)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactMajor {
		t.Fatalf("impact %q, want major", impact)
	}
}

// FR-022 invariant 5 unchanged — a withheld onset with no open incident opens and raises nothing.
func TestAWithheldOnsetWithNoIncidentRaisesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("disable channels: %v", err)
	}
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	got := evalOnce(t, st, ctx)
	if got.IncidentsRaised != 0 {
		t.Fatalf("raised %d with no incident open", got.IncidentsRaised)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM incidents`).Scan(&n); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if n != 0 {
		t.Fatalf("incidents = %d, want 0", n)
	}
}

// I4 — a mapping edited while the incident is open changes nothing now, and a later rise uses the
// frozen mapping, not the live one.
func TestARisingIncidentUsesItsFrozenMapping(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	frozen := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactMajor, Unknown: domain.ImpactNone}
	inc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, frozen)

	live := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{ImpactByState: &live}, AlertActor{}); err != nil {
		t.Fatalf("edit mapping: %v", err)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactMinor {
		t.Fatalf("the edit changed the open incident's impact to %q", impact)
	}
	if b := readBasis(t, st, ctx, inc.ID); b.Mapping == nil || *b.Mapping != frozen {
		t.Fatalf("the edit changed the open incident's basis mapping: %+v", b)
	}

	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactMajor {
		t.Fatalf("impact %q, want major (the frozen mapping), not critical (the live one)", impact)
	}
}

// I7 — the service's own incident rises; a manual and an Alertmanager incident in the same project
// keep their impact, their updated_at and their timeline.
func TestOnlyTheServiceIncidentRises(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	svc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	manual := openManualFixture(t, st, ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	})
	am, err := st.CreateIncidentByPrincipal(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "alert", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceAPI, ExternalKey: "fp-1",
	}, domain.ImpactBasisAlertmanager, "firing", "token", fixtureActor)
	if err != nil {
		t.Fatalf("alertmanager create: %v", err)
	}
	before := map[string]incidentState{
		"manual":       readIncidentState(t, st, ctx, manual.ID),
		"alertmanager": readIncidentState(t, st, ctx, am.ID),
	}

	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)

	if impact := incidentImpact(t, st, ctx, svc.ID); impact != domain.ImpactMajor {
		t.Fatalf("service incident impact %q, want major", impact)
	}
	for name, id := range map[string]string{"manual": manual.ID, "alertmanager": am.ID} {
		after := readIncidentState(t, st, ctx, id)
		b := before[name]
		if after.impact != domain.ImpactMinor {
			t.Errorf("%s impact %q, want minor (never raised)", name, after.impact)
		}
		if !after.updatedAt.Equal(b.updatedAt) {
			t.Errorf("%s updated_at moved: %s → %s", name, b.updatedAt.UTC(), after.updatedAt.UTC())
		}
		if after.notes != 1 {
			t.Errorf("%s timeline entries = %d, want 1", name, after.notes)
		}
	}
}

// I12, §7 clock case (b) — the same interleave as the direct clock test, through the evaluator's
// REPEATABLE READ snapshot: the lock that waited for a committed change fails the pass with 40001
// and writes nothing; the next pass raises over a snapshot that holds the writer's change.
func TestARiseThatLosesTheLockFailsThePassAndLandsOnTheNext(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	inc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, m)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx) // streak 1 of 2: nothing to raise yet

	// A BLOCKER holds the incident row and changes its status, the way an acknowledge does.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID); err != nil {
		t.Fatalf("hold incident: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`UPDATE incidents SET status = 'identified', updated_at = statement_timestamp() WHERE id = $1`,
		inc.ID); err != nil {
		t.Fatalf("blocker status change: %v", err)
	}
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}

	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	evalPID := int(conn.Conn().PgConn().PID())

	done := make(chan error, 1)
	go func() {
		_, eerr := st.evaluateServiceAlertsOn(ctx, conn, evalCadence)
		done <- eerr
	}()
	// Genuinely BLOCKED on the blocker — a sleep would prove nothing about a lock.
	waitUntilBlockedBy(t, st, ctx, evalPID, blockerPID)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}

	evalErr := <-done
	var pe *pgconn.PgError
	if !errors.As(evalErr, &pe) || pe.Code != "40001" {
		t.Fatalf("the pass returned %v, want SQLSTATE 40001 (a REPEATABLE READ lock that waited for a "+
			"committed change)", evalErr)
	}
	// The rise runs before the onset, so it is the rise's lock that lost — not the onset's
	// `ON CONFLICT`, which would wait on the same row if the rise were not there.
	if !strings.Contains(evalErr.Error(), "lock service incident for impact rise") {
		t.Fatalf("the 40001 did not come from the rise step's lock: %v", evalErr)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactMinor {
		t.Fatalf("the failed pass left impact %q, want minor: it wrote nothing", impact)
	}
	if n := len(riseNotes(t, st, ctx, inc.ID)); n != 0 {
		t.Fatalf("the failed pass left %d rise notes", n)
	}
	if n := raisedEvents(t, st, ctx, inc.ID); n != 0 {
		t.Fatalf("the failed pass left %d rise events", n)
	}

	// The next tick raises, and its note carries the status the writer left.
	if got := evalOnce(t, st, ctx); got.IncidentsRaised != 1 {
		t.Fatalf("the next pass raised %d, want 1 (%+v)", got.IncidentsRaised, got)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactCritical {
		t.Fatalf("impact %q, want critical", impact)
	}
	notes := riseNotes(t, st, ctx, inc.ID)
	if len(notes) != 1 {
		t.Fatalf("rise notes = %d, want 1", len(notes))
	}
	if notes[0].status != domain.IncidentIdentified {
		t.Errorf("note status %q, want identified (the status the writer left)", notes[0].status)
	}
	if n := raisedEvents(t, st, ctx, inc.ID); n != 1 {
		t.Errorf("rise events = %d, want 1", n)
	}
}

// I12, §7 clock case (c) — on an incident the evaluator has just raised, the next evaluation of the
// same state completes WITHOUT waiting on a transaction that holds the incident row: an evaluation
// with nothing to raise takes no lock (§4.3 step 0). Under `lock_timeout` a lock attempt would fail
// with 55P03 instead of returning nil.
func TestAnEvaluationWithNothingToRaiseTakesNoLockOnTheIncident(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	inc := openDirect(t, st, ctx, f, domain.ServiceAlertDegraded, m)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	if got := evalOnce(t, st, ctx); got.IncidentsRaised != 1 {
		t.Fatalf("the confirming pass raised %d, want 1 (%+v)", got.IncidentsRaised, got)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactCritical {
		t.Fatalf("impact %q, want critical before the lock is held", impact)
	}

	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT id FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID); err != nil {
		t.Fatalf("hold incident: %v", err)
	}

	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET lock_timeout`) }()

	got, err := st.evaluateServiceAlertsOn(ctx, conn, evalCadence)
	if err != nil {
		t.Fatalf("an evaluation with nothing to raise failed behind a held incident row (a lock "+
			"attempt?): %v", err)
	}
	if got.IncidentsRaised != 0 {
		t.Fatalf("raised %d on a re-evaluation of the same state", got.IncidentsRaised)
	}
	if impact := incidentImpact(t, st, ctx, inc.ID); impact != domain.ImpactCritical {
		t.Fatalf("impact %q, want critical (the raised impact stands)", impact)
	}
	if n := len(riseNotes(t, st, ctx, inc.ID)); n != 1 {
		t.Fatalf("rise notes = %d, want 1", n)
	}
}

// I2, D13 — the rise needs a confirmed state the policy PAGES. A service paging only DOWN that
// confirms DEGRADED does not rise, even though the mapping would put degraded above down.
func TestAConfirmedStateThePolicyDoesNotPageDoesNotRaise(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// A second SLI member under quorum(1, 2): one member up and one down is DEGRADED, both down is DOWN.
	second, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: 1, ProjectID: f.projectID, Name: "checkout-api", Type: domain.MonitorHTTP,
		Target: "https://api.checkout.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("second monitor: %v", err)
	}
	if _, _, err := st.PutServiceDeclaration(ctx, f.projectID, f.serviceID, domain.ServiceDeclaration{
		Monitors: []string{f.monitorID, second.ID}, SLI: []string{f.monitorID, second.ID},
		Policies: domain.ServicePolicies{
			Aggregation: domain.AggregationPolicy{Mode: domain.AggQuorum, DegradedMin: 1, HealthyMin: 2},
		},
	}, 1, DeclarationOptions{CreatedBy: "test"}); err != nil {
		t.Fatalf("declaration: %v", err)
	}
	for _, q := range []string{
		`UPDATE service_definition_revisions SET effective_at = now() - interval '30 minutes' WHERE service_id = $1 AND effective_at > now()`,
		`UPDATE service_evaluation_epochs SET effective_at = now() - interval '30 minutes' WHERE service_id = $1 AND effective_at > now()`,
	} {
		if _, err := st.pool.Exec(ctx, q, f.serviceID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state`); err != nil {
		t.Fatalf("clear state: %v", err)
	}
	// Pages DOWN only; degraded is mapped ABOVE down so a rise on it would be visible.
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET page_on = '{down}', page_on_unknown = false WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("policy: %v", err)
	}
	mapping := domain.ImpactMapping{Degraded: domain.ImpactCritical, Down: domain.ImpactMajor, Unknown: domain.ImpactNone}
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{ImpactByState: &mapping}, AlertActor{}); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	beatBoth := func(a, b bool) {
		setMemberHealth(t, st, ctx, f, a)
		setMemberHealth(t, st, ctx, alertFixture{projectID: f.projectID, serviceID: f.serviceID, monitorID: second.ID}, b)
	}

	// DOWN, confirmed: the evaluator itself opens the auto-incident at major.
	beatBoth(false, false)
	evalOnce(t, st, ctx)
	if got := evalOnce(t, st, ctx); got.Onsets != 1 || got.IncidentsOpened != 1 {
		t.Fatalf("down was not announced and opened: %+v", got)
	}
	var incID string
	if err := st.pool.QueryRow(ctx, `SELECT id FROM incidents WHERE project_id = $1`, f.projectID).Scan(&incID); err != nil {
		t.Fatalf("find incident: %v", err)
	}
	if got := incidentImpact(t, st, ctx, incID); got != domain.ImpactMajor {
		t.Fatalf("opened at %q, want major", got)
	}
	events := len(riseEvents(t, st, ctx, incID))

	// DEGRADED, confirmed over several passes: not a state this policy pages, so nothing rises.
	beatBoth(true, false)
	for i := 1; i <= 4; i++ {
		got := evalOnce(t, st, ctx)
		if got.IncidentsRaised != 0 {
			t.Fatalf("pass %d at degraded: raised %d, want 0 — an unpaged state rose (%+v)", i, got.IncidentsRaised, got)
		}
	}
	var observed string
	if err := st.pool.QueryRow(ctx, `SELECT candidate_state FROM service_alert_state WHERE service_id = $1`,
		f.serviceID).Scan(&observed); err != nil || observed != string(domain.ServiceAlertDegraded) {
		t.Fatalf("the service never reached degraded (candidate %q, %v): the test proves nothing", observed, err)
	}
	if got := incidentImpact(t, st, ctx, incID); got != domain.ImpactMajor {
		t.Fatalf("impact %q, want major (unchanged)", got)
	}
	if n := len(riseNotes(t, st, ctx, incID)); n != 0 {
		t.Fatalf("rise notes = %d, want 0", n)
	}
	if n := len(riseEvents(t, st, ctx, incID)); n != events || raisedEvents(t, st, ctx, incID) != 0 {
		t.Fatalf("rise events changed (%d -> %d)", events, n)
	}
}
