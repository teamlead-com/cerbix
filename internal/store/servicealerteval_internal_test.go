package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.3 — the live evaluator end to end: what it announces, what it refuses to announce, and
// what it says when an announcement ends. These run against the REAL projection path, so a change
// that made the pager disagree with the public page would fail here rather than in production.

const evalCadence = 30 * time.Second

// alertFixture is an owning service whose single SLI member's health the test drives directly.
type alertFixture struct {
	projectID, serviceID, monitorID string
}

func alertingService(t *testing.T, st *Store, ctx context.Context) alertFixture {
	t.Helper()
	f := armedService(t, st, ctx) // ownership, effective declaration, a route
	// The evaluator writes its own state; start from nothing so the first pass is a genuine bootstrap.
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state`); err != nil {
		t.Fatalf("clear state: %v", err)
	}
	return alertFixture{projectID: f.projectID, serviceID: f.serviceID, monitorID: f.monitorID}
}

// setMemberHealth drives the SLI member's observed state by writing a heartbeat the evaluator will
// read, which is what makes these tests exercise the real evaluation rather than a stub.
func setMemberHealth(t *testing.T, st *Store, ctx context.Context, f alertFixture, up bool) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code)
		VALUES ($1, now(), $2, 10, 200)
		ON CONFLICT (monitor_id, ts) DO UPDATE SET up = EXCLUDED.up`, f.monitorID, up); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func evalOnce(t *testing.T, st *Store, ctx context.Context) ServiceAlertEvaluation {
	t.Helper()
	got, err := st.evaluateServiceAlertsOn(ctx, st.pool, evalCadence)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return got
}

func alertEvents(t *testing.T, st *Store, ctx context.Context) []domain.ServiceAlert {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT payload FROM outbox_events WHERE topic = 'service_alert' ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []domain.ServiceAlert
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		var a domain.ServiceAlert
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// The bootstrap: a service that starts life healthy announces NOTHING, and its state row is written
// so that delegation can arm.
func TestEvaluatorBootstrapAnnouncesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, true)

	for i := 0; i < 3; i++ {
		got := evalOnce(t, st, ctx)
		if got.Onsets != 0 || got.Closes != 0 {
			t.Fatalf("pass %d: a healthy service announced something (%+v)", i, got)
		}
	}
	if events := alertEvents(t, st, ctx); len(events) != 0 {
		t.Fatalf("a service that was healthy throughout emitted %d events", len(events))
	}
	// The state row exists and is fresh, which is what lets the delegation query arm.
	var fresh bool
	if err := st.pool.QueryRow(ctx, `
		SELECT now() < lease_until AND last_error IS NULL FROM service_alert_state
		 WHERE service_id = $1`, f.serviceID).Scan(&fresh); err != nil {
		t.Fatalf("state: %v", err)
	}
	if !fresh {
		t.Fatal("the evaluator wrote no fresh state, so delegation could never arm")
	}
}

// Confirmation, then the onset — and the page says how long it waited.
func TestEvaluatorConfirmsBeforeAnnouncing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)

	// The member fails. `confirm_evaluations` is 2, so the first pass must stay silent.
	setMemberHealth(t, st, ctx, f, false)
	if got := evalOnce(t, st, ctx); got.Onsets != 0 {
		t.Fatalf("an unconfirmed DOWN announced immediately (%+v)", got)
	}
	if got := evalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("a confirmed DOWN did not announce (%+v)", got)
	}
	events := alertEvents(t, st, ctx)
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly one onset", len(events))
	}
	e := events[0]
	if !e.Firing || e.State != domain.ServiceAlertDown || e.Signal != domain.ServiceSignalHealth {
		t.Fatalf("onset = %+v", e)
	}
	if e.ConfirmedOver < 2 {
		t.Fatalf("the page does not say it waited: confirmed over %d", e.ConfirmedOver)
	}
	if len(e.Recipients) == 0 {
		t.Fatal("the onset resolved no recipients, so nobody would be paged")
	}
	if e.EpisodeID == "" {
		t.Fatal("no episode was opened, so the close would have no recipients to reach")
	}

	// Staying down announces nothing further.
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	if len(alertEvents(t, st, ctx)) != 1 {
		t.Fatal("a service that stayed down announced again")
	}
}

// The close reaches the ONSET's recipients even when the schedule has rotated, and it says WHY it
// ended rather than claiming a recovery it cannot evidence.
func TestEvaluatorClosesToTheOnsetRecipients(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// A schedule with one participant is the route at onset time.
	chans := liveChannels(t, st, ctx, f.projectID, "first", "second")
	var schedule string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO oncall_schedules (project_id, name, shift_seconds, anchor_at, participants)
		VALUES ($1,'primary',604800,now(),jsonb_build_array($2::text)) RETURNING id`,
		f.projectID, chans[0]).Scan(&schedule); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET oncall_schedule_id = $2 WHERE id = $1`, f.serviceID, schedule); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	onset := alertEvents(t, st, ctx)
	if len(onset) != 1 || !onset[0].Firing {
		t.Fatalf("expected one onset, got %+v", onset)
	}
	if len(onset[0].Recipients) != 1 || onset[0].Recipients[0] != chans[0] {
		t.Fatalf("onset recipients = %v, want the schedule's on-call", onset[0].Recipients)
	}

	// The rotation changes who is on call BEFORE the service recovers.
	if _, err := st.pool.Exec(ctx,
		`UPDATE oncall_schedules SET participants = jsonb_build_array($2::text) WHERE id = $1`,
		schedule, chans[1]); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)

	events := alertEvents(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("events = %d, want the onset and its close", len(events))
	}
	closeEvent := events[1]
	if closeEvent.Firing {
		t.Fatalf("the second event is not a close: %+v", closeEvent)
	}
	if closeEvent.CloseReason != domain.CloseRecovered {
		t.Fatalf("close reason = %q, want recovered", closeEvent.CloseReason)
	}
	if len(closeEvent.Recipients) != 1 || closeEvent.Recipients[0] != chans[0] {
		t.Fatalf("close recipients = %v, want the ONSET's snapshot — a rotation must not page a "+
			"stranger and leave the original recipient hanging", closeEvent.Recipients)
	}
	if closeEvent.Seq <= onset[0].Seq {
		t.Fatalf("close seq %d is not after the onset's %d", closeEvent.Seq, onset[0].Seq)
	}
}

// Entering a declared maintenance window ENDS the announcement — and says so, rather than claiming
// the service recovered.
func TestEvaluatorClosesWithoutClaimingRecovery(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	if got := len(alertEvents(t, st, ctx)); got != 1 {
		t.Fatalf("expected the onset, got %d events", got)
	}

	// A project-wide window covering now.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO maintenance_windows (project_id, monitor_id, reason, starts_at, ends_at)
		VALUES ($1, NULL, 'planned', now() - interval '1 minute', now() + interval '1 hour')`,
		f.projectID); err != nil {
		t.Fatalf("window: %v", err)
	}
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)

	events := alertEvents(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("events = %d, want the onset and its close", len(events))
	}
	if events[1].Firing {
		t.Fatal("entering maintenance did not close the announcement")
	}
	if events[1].CloseReason != domain.CloseEnteredMaintenance {
		t.Fatalf("close reason = %q, want entered_maintenance — a declared window is not a recovery",
			events[1].CloseReason)
	}
	// The rendered message must not read as "fixed".
	if msg := events[1].Message(); !strings.Contains(msg, "not a recovery") {
		t.Fatalf("the close message claims a recovery it cannot evidence: %q", msg)
	}
}

// A service nobody can measure is UNKNOWN, and unknown does not page unless it was declared.
func TestEvaluatorDoesNotPageUnknownByDefault(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	// No heartbeats at all: nothing decidable, so the service is unknown rather than down.
	for i := 0; i < 3; i++ {
		evalOnce(t, st, ctx)
	}
	if events := alertEvents(t, st, ctx); len(events) != 0 {
		t.Fatalf("an unmeasurable service paged as if it were down: %+v", events)
	}
	var observed string
	if err := st.pool.QueryRow(ctx,
		`SELECT observed_state FROM service_alert_state WHERE service_id = $1`, f.serviceID).
		Scan(&observed); err != nil {
		t.Fatalf("state: %v", err)
	}
	if observed != string(domain.ServiceAlertUnknown) {
		t.Fatalf("observed = %q, want unknown", observed)
	}

	// Declared, it pages — as UNKNOWN, not as down.
	if _, err := st.pool.Exec(ctx,
		`UPDATE services SET page_on_unknown = true WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("declare: %v", err)
	}
	evalOnce(t, st, ctx)
	evalOnce(t, st, ctx)
	events := alertEvents(t, st, ctx)
	if len(events) != 1 {
		t.Fatalf("events = %d, want one after declaring page_on_unknown", len(events))
	}
	if events[0].State != domain.ServiceAlertUnknown {
		t.Fatalf("state = %q, want unknown — blindness must not be announced as an outage",
			events[0].State)
	}
}

// The slice is bounded and fair: with more alerting services than the cap, one pass evaluates the
// cap and the NEXT pass reaches the rest, because the order is by how long each has waited.
func TestEvaluatorSliceIsBoundedAndFair(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_alert_state`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// Enough owning services to exceed the cap — spread over several projects, because a project
	// caps its own service count and this test is about the EVALUATOR's bound, not that one.
	for i := 0; i < ServiceAlertSliceCap+5; i++ {
		projectID := f.projectID
		if i >= 40 {
			proj, err := st.CreateProject(ctx, f.orgID, fmt.Sprintf("extra-%d", i/40), "Extra")
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Fatalf("project: %v", err)
			}
			if err == nil {
				projectID = proj.ID
			} else {
				if err := st.pool.QueryRow(ctx,
					`SELECT id FROM projects WHERE org_id = $1 AND slug = $2`,
					f.orgID, fmt.Sprintf("extra-%d", i/40)).Scan(&projectID); err != nil {
					t.Fatalf("resolve project: %v", err)
				}
			}
		}
		svc, err := st.CreateService(ctx, domain.Service{
			ProjectID: projectID, Slug: fmt.Sprintf("svc-%03d", i), Name: fmt.Sprintf("Svc %d", i),
		})
		if err != nil {
			t.Fatalf("service %d: %v", i, err)
		}
		if _, err := st.pool.Exec(ctx,
			`UPDATE services SET owns_paging = true WHERE id = $1`, svc.ID); err != nil {
			t.Fatalf("own %d: %v", i, err)
		}
	}
	first := evalOnce(t, st, ctx)
	if first.Evaluated != ServiceAlertSliceCap {
		t.Fatalf("first slice evaluated %d, want the cap %d", first.Evaluated, ServiceAlertSliceCap)
	}
	second := evalOnce(t, st, ctx)
	if second.Evaluated == 0 {
		t.Fatal("the second pass evaluated nothing: services beyond the cap would starve")
	}
	var unevaluated int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM services s
		 WHERE s.owns_paging
		   AND NOT EXISTS (SELECT 1 FROM service_alert_state st WHERE st.service_id = s.id)`).
		Scan(&unevaluated); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unevaluated != 0 {
		t.Fatalf("%d owning services were never reached after two passes", unevaluated)
	}
}

// FR-022 REWRITES what FR-021 invariant 86 asserted, and the rewrite rather than a deletion is the point.
//
// Invariant 86 said a service alert opens, resolves or annotates NO incident, with the single exception of
// the §16.1 suppression note on a MONITOR's incident. That was true and tested until FR-022 (D-0170) made
// service incidents the feature. The invariant is now marked SUPERSEDED at its number in §16.8 and its
// discharge row points here — the same treatment phase 5 gave the phase-2 burn-rejection test when it
// inverted (`TestServiceScopedBurnTargetIsSupported`). Deleting this test would have removed the record
// that the rule ever changed.
//
// What survives of 86 is the half FR-022 does NOT touch: a service alert still does nothing to a MONITOR's
// incident. That half is asserted here too, because it is the one an implementer is most likely to break
// while adding the other.
func TestAServiceAlertOpensAndResolvesOnlyItsOwnIncident(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// A MONITOR's incident, opened before anything else happens. Nothing the evaluator does may touch it.
	monitorIncident, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.monitorID, Title: "checkout-http down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened by hand", "tester")
	if err != nil {
		t.Fatalf("monitor incident: %v", err)
	}

	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)

	// A confirmed outage opens ONE incident, anchored to the SERVICE, in the same pass that announces.
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	got := evalOnce(t, st, ctx)
	if got.Onsets != 1 || got.IncidentsOpened != 1 {
		t.Fatalf("onset pass = %+v, want one onset and one incident opened", got)
	}
	inc, err := st.FindOpenAutoIncidentByService(ctx, st.pool, f.serviceID)
	if err != nil {
		t.Fatalf("no open service incident after an announced onset: %v — the incident and the announcement "+
			"are one transaction (FR-022 invariant 7)", err)
	}
	if inc.ServiceID != f.serviceID || inc.MonitorID != "" {
		t.Fatalf("anchors = service %q / monitor %q, want the service alone", inc.ServiceID, inc.MonitorID)
	}
	var opening string
	if err := st.pool.QueryRow(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 ORDER BY created_at LIMIT 1`, inc.ID).Scan(&opening); err != nil {
		t.Fatalf("read opening note: %v", err)
	}
	if !strings.Contains(opening, "confirmed over") {
		t.Errorf("opening note = %q, want it to state what confirmed the open — an operator who finds an "+
			"incident nobody typed needs the machine's reason", opening)
	}

	// The recovery RESOLVES it, in the same pass that closes the announcement (invariant 15). Without this
	// the operator reads "investigating" on a recovered service and the next outage cannot open one.
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	closed := evalOnce(t, st, ctx)
	if closed.Closes != 1 || closed.IncidentsResolved != 1 {
		t.Fatalf("close pass = %+v, want one close and one incident resolved", closed)
	}
	resolved, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if resolved.Status != domain.IncidentResolved || resolved.ResolvedAt == nil {
		t.Fatalf("incident after recovery = %s (resolved_at %v), want resolved", resolved.Status, resolved.ResolvedAt)
	}
	// ...and it SAYS SO on the timeline (§7's matrix line, which asks for the update and not only the
	// status). An incident that flips to resolved with nothing written is a machine's conclusion with no
	// stated reason, in the one place a human reads afterwards to find out what happened.
	var closing string
	if err := st.pool.QueryRow(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 ORDER BY created_at DESC LIMIT 1`,
		inc.ID).Scan(&closing); err != nil {
		t.Fatalf("read closing note: %v", err)
	}
	if !strings.Contains(closing, "Resolved automatically") {
		t.Errorf("last timeline entry = %q, want the machine to state that IT resolved this and why", closing)
	}

	// ...and a SECOND failure opens a SECOND incident (invariant 16) — which is only possible because the
	// first was resolved, so the per-service unique index no longer blocks it.
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	again := evalOnce(t, st, ctx)
	if again.IncidentsOpened != 1 {
		t.Fatalf("second outage = %+v, want a new incident: a service that recovers and fails again must be "+
			"visible in the timeline", again)
	}
	second, err := st.FindOpenAutoIncidentByService(ctx, st.pool, f.serviceID)
	if err != nil || second.ID == inc.ID {
		t.Fatalf("second incident = %s (err %v), want a new one", second.ID, err)
	}

	// The surviving half of invariant 86: the MONITOR's incident is untouched throughout — same status, same
	// timeline length. This is the half an implementer is most likely to break while adding the other.
	mon, err := st.GetIncident(ctx, monitorIncident.ID)
	if err != nil {
		t.Fatalf("read monitor incident: %v", err)
	}
	if mon.Status != domain.IncidentInvestigating || mon.ResolvedAt != nil {
		t.Errorf("the monitor's incident changed to %s — a service alert must do nothing to it (NFR-017)", mon.Status)
	}
	var monUpdates int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_updates WHERE incident_id = $1`, monitorIncident.ID).Scan(&monUpdates); err != nil {
		t.Fatalf("count monitor updates: %v", err)
	}
	if monUpdates != 1 {
		t.Errorf("the monitor's incident gained %d updates, want only its own opening one", monUpdates)
	}
}

// liveChannels creates two REAL notification channels and returns their ids. Schedule participants
// are channel ids (`api/tenant_scope.go`), and a fixture that invents strings only ever satisfied the
// old "the array is non-empty" arming rule — under the current one such a participant resolves to
// nothing and correctly falls back to the project's channels.
func liveChannels(t *testing.T, st *Store, ctx context.Context, projectID string, names ...string) []string {
	t.Helper()
	out := make([]string, 0, len(names))
	for _, n := range names {
		var id string
		if err := st.pool.QueryRow(ctx, `
			INSERT INTO notification_channels (project_id, type, name, config, enabled)
			VALUES ($1,'webhook',$2,'{"url":"https://hook.example/x"}',true)
			RETURNING id::text`, projectID, n).Scan(&id); err != nil {
			t.Fatalf("channel %s: %v", n, err)
		}
		out = append(out, id)
	}
	return out
}

// D-0187 — the outage that nobody heard about gets told, once there is somebody to tell.
//
// D-0179 closed the swallow: an announcement that reached nobody stops arming coverage, so the
// members keep paging for themselves. It stopped there deliberately, and the gap it left is this —
// the incident is open, the outage is real, and the service's own alert was never received by anyone,
// forever, because the latch says firing and the evaluator therefore sees no edge.
func TestAnUndeliveredOnsetIsAnnouncedAgainOnceThereIsSomebodyToTell(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)

	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	if got := evalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("the confirmed DOWN did not announce (%+v)", got)
	}
	first := alertEvents(t, st, ctx)
	if len(first) != 1 {
		t.Fatalf("%d events, want the one onset", len(first))
	}

	// Still down, still announced: nothing further, which is the behaviour the re-announcement must
	// not disturb.
	setMemberHealth(t, st, ctx, f, false)
	if got := evalOnce(t, st, ctx); got.Onsets != 0 {
		t.Fatalf("a service that stayed down announced again (%+v)", got)
	}

	// The worker attempts it and reaches nobody, terminally. This is `MarkServiceAlertUndeliverable`
	// through its own door, so the test exercises the same write the worker makes.
	if err := st.MarkServiceAlertUndeliverable(ctx, domain.ServiceAlert{
		ServiceID: f.serviceID, ProjectID: f.projectID,
		Signal: domain.ServiceSignalHealth, Firing: true, Seq: first[0].Seq,
	}); err != nil {
		t.Fatalf("condemn: %v", err)
	}

	// With no route there is still nobody to tell, so the re-announcement is WITHHELD rather than
	// enqueued into the same emptiness — D-0176's rule, and it must keep applying here.
	exec(t, st, ctx, `UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID)
	got := evalOnce(t, st, ctx)
	if got.Onsets != 0 {
		t.Fatalf("a re-announcement went out with no route (%+v): that is the emptiness D-0176 "+
			"refuses to announce into", got)
	}
	if got.Withheld[WithheldUnroutable] != 1 {
		t.Fatalf("the withheld re-announcement was not counted: %+v", got.Withheld)
	}
	if len(alertEvents(t, st, ctx)) != 1 {
		t.Fatal("a second event exists despite there being nobody to receive it")
	}

	// The operator fixes the channel. NOW it is announced again.
	exec(t, st, ctx, `UPDATE notification_channels SET enabled = true WHERE project_id = $1`, f.projectID)
	if got := evalOnce(t, st, ctx); got.Onsets != 1 {
		t.Fatalf("the outage was not re-announced after the route came back (%+v): the incident is "+
			"open, the service is down, and nobody has ever been told", got)
	}
	events := alertEvents(t, st, ctx)
	if len(events) != 2 {
		t.Fatalf("%d events, want the original and the re-announcement", len(events))
	}
	again := events[1]
	if !again.Firing || again.State != domain.ServiceAlertDown {
		t.Fatalf("the re-announcement is not an onset for the current state: %+v", again)
	}
	if again.Seq <= first[0].Seq {
		t.Fatalf("the re-announcement carries sequence %d, not past the condemned %d — a receiver "+
			"ordering on it would treat the new page as superseded", again.Seq, first[0].Seq)
	}
	if len(again.Recipients) == 0 {
		t.Fatal("the re-announcement resolved no recipients")
	}
	if again.EpisodeID == first[0].EpisodeID {
		t.Fatal("the re-announcement re-used the episode nobody heard: its recipient snapshot names " +
			"people who could not be reached, and the close would go to them")
	}

	// The superseded episode says WHY it ended, and it is not a policy change.
	var reason string
	if err := st.pool.QueryRow(ctx,
		`SELECT close_reason FROM service_alert_episodes WHERE id = $1`,
		first[0].EpisodeID).Scan(&reason); err != nil {
		t.Fatalf("read superseded episode: %v", err)
	}
	if reason != string(domain.CloseUndelivered) {
		t.Fatalf("the episode nobody heard was closed as %q, want %q — nothing about the policy "+
			"changed, and a false cause in a record an operator reads is worse than none",
			reason, domain.CloseUndelivered)
	}

	// And it does not loop: the re-announcement has not been condemned, so the next pass is quiet.
	if got := evalOnce(t, st, ctx); got.Onsets != 0 {
		t.Fatalf("the evaluator announced a third time (%+v): a re-announcement that repeats every "+
			"cadence is a pager loop, not a fix", got)
	}
}
