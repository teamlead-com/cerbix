package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-022's schema half: the anchor is exclusive, tenant-safe, and survives the deletion of what it points
// at. All three are asserted with DIRECT SQL where the point is what ANY writer can do — the store not
// offering a way to set two anchors proves only what this code does today.
func TestAnIncidentHasAtMostOneAnchor(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// (1) at most one anchor: both at once is unrepresentable (FR-022 invariant 1).
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO incidents (project_id, monitor_id, service_id, title, source)
		 VALUES ($1, $2, $3, 'both anchors', 'manual')`, f.projectID, f.monitorID, f.serviceID); err == nil {
		t.Error("an incident naming BOTH a monitor and a service was accepted — the discriminator every read " +
			"path branches on must be unambiguous at the schema (incidents_one_anchor_chk)")
	}
	// ...and NEITHER stays legal, because a manual project-level incident has always had neither.
	var projectLevel string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO incidents (project_id, title, source) VALUES ($1, 'project level', 'manual') RETURNING id::text`,
		f.projectID).Scan(&projectLevel); err != nil {
		t.Fatalf("a project-level incident must keep working: %v", err)
	}

	// (2) the anchor cannot cross tenants (FR-022 invariant 2), even by direct SQL.
	otherOrg, _ := st.CreateOrganization(ctx, "globex", "Globex")
	otherProj, _ := st.CreateProject(ctx, otherOrg.ID, "other", "Other")
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO incidents (project_id, service_id, title, source) VALUES ($1, $2, 'cross tenant', 'manual')`,
		otherProj.ID, f.serviceID); err == nil {
		t.Error("a service anchor crossed a project boundary — the FK is composite exactly so that a " +
			"same-project guarantee does not live in application code (FR-021 invariant 48's rule)")
	}
}

// Deleting the service CLEARS the anchor and not the tenant key (FR-022 invariant 3). This is the trap
// iter-0125 hit with a bare ON DELETE SET NULL: Postgres applies it to EVERY referencing column, including
// the NOT NULL project_id, and the delete then fails outright — or worse, the incident loses its tenant.
func TestDeletingAServiceClearsTheAnchorAndKeepsTheIncident(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	inc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: f.serviceID, Title: "checkout degraded",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened", "tester")
	if err != nil {
		t.Fatalf("create service incident: %v", err)
	}
	if inc.ServiceID != f.serviceID || inc.MonitorID != "" {
		t.Fatalf("stored anchors = service %q / monitor %q", inc.ServiceID, inc.MonitorID)
	}

	if err := st.DeleteService(ctx, f.projectID, f.serviceID); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	after, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("the incident did not survive its service's deletion: %v — a timeline is a record of what "+
			"happened, and deleting the subject does not unhappen it", err)
	}
	if after.ServiceID != "" {
		t.Errorf("anchor after delete = %q, want cleared", after.ServiceID)
	}
	if after.ProjectID != f.projectID {
		t.Errorf("project after delete = %q, want %q — a column-list SET NULL clears the ANCHOR; a bare one "+
			"would take the tenant key with it", after.ProjectID, f.projectID)
	}
	// And the TIMELINE stands (§7's matrix line). The row surviving is not the claim — the claim is that
	// what people wrote and what the machine wrote about this outage are still readable after its subject
	// is gone, which is the whole reason the incident is kept rather than cascaded away.
	var updates int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_updates WHERE incident_id = $1`, inc.ID).Scan(&updates); err != nil {
		t.Fatalf("count timeline: %v", err)
	}
	if updates == 0 {
		t.Errorf("the incident survived its service but its TIMELINE did not — an incident with no timeline " +
			"is a row, not a record")
	}
}

// One OPEN auto-incident per service (FR-022 invariant 4), and the opening is one transaction with whatever
// the caller does alongside it (invariant 7). A flapping service must not accumulate incidents, and under
// concurrent evaluators only the database can promise that.
func TestOpeningAServiceIncidentIsIdempotentAndSnapshotsItsMembers(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	open := func() (domain.Incident, bool) {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		inc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout degraded", 2, governingRevision(t, st, ctx, f.serviceID))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return inc, created
	}

	first, created := open()
	if !created {
		t.Fatal("the first open reported created=false")
	}
	second, createdAgain := open()
	if createdAgain {
		t.Fatal("a second open created ANOTHER incident — a flapping service would accumulate them " +
			"(incidents_service_open_auto_idx)")
	}
	if second.ID != first.ID {
		t.Fatalf("the second open returned %s, want the already-open %s: a caller told 'not created' must be "+
			"able to annotate the incident that IS open", second.ID, first.ID)
	}
	// The property, asserted behaviourally and not only through the code path: after two opens the service
	// has ONE open auto-incident. (Dropping the index makes the ON CONFLICT above fail loudly instead of
	// letting rows accumulate — correct behaviour, but it means that mutation proves the code DEPENDS on the
	// index rather than proving this count, which is why the count is asserted here too.)
	var openCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incidents WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'`,
		f.serviceID).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("%d open auto-incidents for one service — a flapping service must not accumulate them", openCount)
	}

	// The opening note states what confirmed it — an operator who finds an incident nobody typed needs the
	// machine's reason, and the number is the one that governs paging.
	var body string
	if err := st.pool.QueryRow(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 ORDER BY created_at LIMIT 1`, first.ID).Scan(&body); err != nil {
		t.Fatalf("read opening note: %v", err)
	}
	if body == "" || !strings.Contains(body, "confirmed over 2") {
		t.Errorf("opening note = %q, want it to state what confirmed the open", body)
	}

	// The member snapshot exists and names the member, and it KEEPS naming it after the monitor is gone.
	members, ok, err := st.IncidentMemberSnapshot(ctx, first.ID)
	if err != nil || !ok {
		t.Fatalf("snapshot: %v (present=%v)", err, ok)
	}
	// One ROW per monitor, roles aggregated: the declaration stores a row per (monitor, role) and this
	// member is both context and SLI, which a postmortem must show once rather than twice.
	if len(members) != 1 || members[0].MonitorID != f.monitorID {
		t.Fatalf("snapshot = %+v, want the one declared member once", members)
	}
	if len(members[0].Roles) < 1 || members[0].Name == "" {
		t.Fatalf("snapshot member = %+v, want its name and at least one role", members[0])
	}
	if err := st.DeleteMonitor(ctx, f.monitorID); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	after, ok, err := st.IncidentMemberSnapshot(ctx, first.ID)
	if err != nil || !ok || len(after) != 1 || after[0].MonitorID != f.monitorID {
		t.Fatalf("snapshot after the member was deleted = %+v (present=%v, err=%v) — a postmortem is read "+
			"after the world moved, which is the whole reason it is a snapshot", after, ok, err)
	}

	// A monitor or project-level incident has NO snapshot, and that is a different answer from an empty one.
	other, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, Title: "manual", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMinor, Source: domain.SourceManual,
	}, "", "tester")
	if err != nil {
		t.Fatalf("create manual incident: %v", err)
	}
	if _, present, err := st.IncidentMemberSnapshot(ctx, other.ID); err != nil || present {
		t.Errorf("a project-level incident reported a snapshot (present=%v, err=%v)", present, err)
	}
}

// The machine does not touch what a person owns (spec D1b). Two shapes, and both matter because the
// resolve is a blind UPDATE by service: a HUMAN-opened service incident must survive the evaluator's
// close, and a human's resolution of a machine's incident must not be re-annotated.
//
// Written after a mutation showed the gap: widening the resolve's WHERE from `source = 'auto'` to any
// open incident left every test green, which means "a machine must not overwrite a person's conclusion"
// was resting on nothing.
func TestTheMachineLeavesAHumanIncidentAlone(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	human, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: f.serviceID, Title: "checkout — investigating by hand",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opened by a person", "ada@example.com")
	if err != nil {
		t.Fatalf("human incident: %v", err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	resolved, err := st.ResolveServiceIncidentTx(ctx, tx, f.serviceID, "Resolved automatically.")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if resolved {
		t.Error("the machine resolved a HUMAN's incident — an auto-resolve must name `source = 'auto'`, or " +
			"every operator-opened incident closes itself the moment the service recovers")
	}

	after, err := st.GetIncident(ctx, human.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.Status != domain.IncidentInvestigating || after.ResolvedAt != nil {
		t.Fatalf("the human's incident became %s (resolved_at %v) — a machine must not draw a conclusion a "+
			"person is still working on", after.Status, after.ResolvedAt)
	}
	var updates int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_updates WHERE incident_id = $1`, human.ID).Scan(&updates); err != nil {
		t.Fatalf("count updates: %v", err)
	}
	if updates != 1 {
		t.Errorf("the human's incident gained %d updates, want only its own — a machine note on a person's "+
			"timeline is a second author nobody asked for", updates)
	}
}

// FR-022 invariant 5, the ARMED half: the three gates that decide whether a service PAGES are the
// same three that decide whether it opens an incident. A service that does not own paging is not
// covering anybody, so its members page for themselves — and nothing may open an incident on their
// behalf, because an incident nobody was told about is worse than no incident at all.
func TestADisarmedServiceOpensNoIncidentAndItsMembersKeepPaging(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	if _, err := st.pool.Exec(ctx, `UPDATE services SET owns_paging = false WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("disarm: %v", err)
	}

	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	got := evalOnce(t, st, ctx)

	if got.Onsets != 0 || got.IncidentsOpened != 0 {
		t.Fatalf("a DISARMED service announced %+v — it covers nobody, so it speaks for nobody", got)
	}
	var incidents int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incidents WHERE service_id = $1`, f.serviceID).Scan(&incidents); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if incidents != 0 {
		t.Fatalf("%d incident(s) opened for a service that does not page — an incident opened without an "+
			"announcement is one nobody was told about (FR-022 invariant 5)", incidents)
	}
	// The other side of the same coin, and the reason this is safe: the member is NOT delegated, so
	// its own alert is delivered. Silence on both sides would be the actual outage of the alerting.
	v, err := st.ActiveDelegation(ctx, f.monitorID, f.projectID, DelegationLive)
	if err != nil {
		t.Fatalf("delegation: %v", err)
	}
	if v.Suppress() {
		t.Fatalf("the member's own alert is suppressed by a service that is not paging (owners %+v): "+
			"nobody would be told anything", v.Owners)
	}
}

// FR-022 invariant 12: the ⏸ note keeps exactly ONE home — the MONITOR's incident — so a single
// outage is never annotated twice. Both incidents are open here, which is the normal state during a
// service outage: the members are down and so is the service.
func TestTheSuppressionNoteKeepsOneHomeWhenBothIncidentsAreOpen(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	monitorInc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, MonitorID: f.monitorID, Title: "checkout-http is down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "monitor reported down", "system")
	if err != nil {
		t.Fatalf("monitor incident: %v", err)
	}
	serviceInc, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: f.serviceID, Title: "Checkout — service down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceAuto,
	}, "opened automatically", "system")
	if err != nil {
		t.Fatalf("service incident: %v", err)
	}
	var eventID string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO outbox_events (topic, payload) VALUES ('monitor_transition','{}') RETURNING id`).
		Scan(&eventID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := st.RecordSuppression(ctx, eventID, f.monitorID, f.projectID,
		domain.TopicMonitorTransition, []DelegationOwner{
			{ServiceID: f.serviceID, Slug: "checkout", Name: "Checkout"},
		}); err != nil {
		t.Fatalf("record suppression: %v", err)
	}

	count := func(incidentID string) int {
		t.Helper()
		var n int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM incident_updates
			 WHERE incident_id = $1 AND author = 'system' AND body LIKE $2`,
			incidentID, domain.SuppressionMarker+"%").Scan(&n); err != nil {
			t.Fatalf("count notes: %v", err)
		}
		return n
	}
	if got := count(monitorInc.ID); got != 1 {
		t.Fatalf("the MONITOR's incident carries %d ⏸ notes, want exactly 1 — it is the one whose "+
			"delivery was withheld", got)
	}
	if got := count(serviceInc.ID); got != 0 {
		t.Fatalf("the SERVICE's incident carries %d ⏸ notes: one outage would be explained twice, and "+
			"the second explanation is on the incident that did the suppressing (FR-022 invariant 12)", got)
	}
}

// FR-022 invariant 10: an open service incident changes NO component status. The §15.0 precedence
// table decides what a page renders from the SERVICE's evaluated health, and an incident is rendered
// AS an incident — a second, independent statement. If an incident could also move the status, the
// table would stop being total (FR-021 invariants 66–68) and two sources would disagree about the
// same component.
func TestAnOpenServiceIncidentMovesNoComponentStatus(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)
	setMemberHealth(t, st, ctx, f, true)

	refs := []ServiceRef{{ProjectID: f.projectID, ServiceID: f.serviceID}}
	before, err := st.ServicePageProjections(ctx, refs, true)
	if err != nil {
		t.Fatalf("projection before: %v", err)
	}
	b := before[f.serviceID]
	if b.SLI == "" {
		t.Fatalf("the fixture produced no projection at all: %+v", b)
	}

	if _, err := st.CreateIncident(ctx, domain.Incident{
		ProjectID: f.projectID, ServiceID: f.serviceID, Title: "Checkout — service down",
		Status: domain.IncidentInvestigating, Impact: domain.ImpactCritical, Source: domain.SourceAuto,
	}, "opened automatically", "system"); err != nil {
		t.Fatalf("service incident: %v", err)
	}

	after, err := st.ServicePageProjections(ctx, refs, true)
	if err != nil {
		t.Fatalf("projection after: %v", err)
	}
	a := after[f.serviceID]
	switch {
	case a.SLI != b.SLI:
		t.Fatalf("component status moved from %q to %q because an incident opened — the precedence "+
			"table reads the service's HEALTH, and an incident is rendered as an incident", b.SLI, a.SLI)
	case a.Reason != b.Reason || a.UptimeWithheld != b.UptimeWithheld:
		t.Fatalf("the projection's reasons changed with an incident open: %+v vs %+v", b, a)
	case (a.Uptime == nil) != (b.Uptime == nil):
		t.Fatalf("the quoted number appeared or vanished with an incident open: %v vs %v", b.Uptime, a.Uptime)
	case a.Excluded != b.Excluded:
		t.Fatalf("maintenance exclusion changed with an incident open: %v vs %v", b.Excluded, a.Excluded)
	}
}

// FR-022 + FR-012: a service incident owes the SAME lifecycle events as any other incident.
//
// `incident_event` is what reaches incident webhooks and the confirmed subscribers of every status
// page surfacing the project. The service path enqueued only its correlation attempt, so a service
// outage was visible in the database and the UI while the people who explicitly asked to be told
// heard nothing — and the monitor's auto-path, which goes through `CreateIncident`, always sent it.
// Two automatic paths disagreeing about whether an outage is announceable is the defect; the
// service's own `service_alert` does not cover it, because that pages the service's recipients, a
// different audience from the page's subscribers.
func TestServiceIncidentAnnouncesItsLifecycleLikeAnyOther(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	inTx := func(fn func(tx pgx.Tx)) {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		fn(tx)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	events := func() []domain.IncidentEvent {
		t.Helper()
		rows, err := st.pool.Query(ctx,
			`SELECT payload FROM outbox_events WHERE topic = $1 ORDER BY created_at, id`,
			domain.TopicIncidentEvent)
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		defer rows.Close()
		var out []domain.IncidentEvent
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("scan event: %v", err)
			}
			var ev domain.IncidentEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatalf("decode event: %v", err)
			}
			out = append(out, ev)
		}
		return out
	}

	var opened domain.Incident
	inTx(func(tx pgx.Tx) {
		inc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout down", 3, governingRevision(t, st, ctx, f.serviceID))
		if err != nil || !created {
			t.Fatalf("open: %v (created=%v)", err, created)
		}
		opened = inc
	})
	evs := events()
	if len(evs) != 1 || evs[0].Type != domain.EventIncidentOpened || evs[0].Incident.ID != opened.ID {
		t.Fatalf("after the open, events = %+v; want exactly one incident.opened for %s", evs, opened.ID)
	}

	// The evaluator that LOSES the open race announces nothing: one opening, one announcement.
	inTx(func(tx pgx.Tx) {
		if _, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout down", 3, governingRevision(t, st, ctx, f.serviceID)); err != nil || created {
			t.Fatalf("second open: %v (created=%v, want false)", err, created)
		}
	})
	if got := len(events()); got != 1 {
		t.Fatalf("a losing racer announced too: %d events, want 1", got)
	}

	inTx(func(tx pgx.Tx) {
		resolved, err := st.ResolveServiceIncidentTx(ctx, tx, f.serviceID, "recovered")
		if err != nil || !resolved {
			t.Fatalf("resolve: %v (resolved=%v)", err, resolved)
		}
	})
	evs = events()
	if len(evs) != 2 {
		t.Fatalf("after the resolve, %d events, want 2", len(evs))
	}
	closing := evs[1]
	switch {
	case closing.Type != domain.EventIncidentResolved:
		t.Fatalf("closing event type = %q, want incident.resolved", closing.Type)
	case closing.Incident.ID != opened.ID:
		t.Fatalf("the close names incident %s, want %s", closing.Incident.ID, opened.ID)
	case closing.Incident.Status != domain.IncidentResolved:
		t.Fatalf("the close carries status %q — it must describe the row as it now IS", closing.Incident.Status)
	case closing.Update == nil || closing.Update.Body != "recovered":
		t.Fatalf("the close carries update %+v, want the timeline entry it wrote", closing.Update)
	}

	// A second resolve has nothing to end, and must not announce an ending twice.
	inTx(func(tx pgx.Tx) {
		if resolved, err := st.ResolveServiceIncidentTx(ctx, tx, f.serviceID, "again"); err != nil || resolved {
			t.Fatalf("second resolve: %v (resolved=%v, want false)", err, resolved)
		}
	})
	if got := len(events()); got != 2 {
		t.Fatalf("a no-op resolve announced: %d events, want 2", got)
	}
}

// FR-022 D1b + FR-021 §16.4a — the LIVE occurrence ends as ONE thing.
//
// Closing the health episode without resolving the incident stranded the service: the only other
// caller of the resolve is the evaluator's recovery, and the evaluator's slice is `WHERE
// s.owns_paging`, so after ownership is switched off nothing would ever look at the service again.
// The incident then sits open forever, and — because at most one auto-incident may be open per
// service — it also refuses the NEXT outage its own incident once ownership comes back.
func TestDisowningAServiceEndsItsIncidentAndNotJustItsAlert(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	inc := openLiveOccurrence(t, st, ctx, f)

	// The operator turns paging ownership off while the outage is live.
	off := false
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{OwnsPaging: &off}, AlertActor{}); err != nil {
		t.Fatalf("disown: %v", err)
	}

	var status string
	var resolvedAt *time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT status, resolved_at FROM incidents WHERE id = $1`, inc.ID).Scan(&status, &resolvedAt); err != nil {
		t.Fatalf("reread incident: %v", err)
	}
	if status != string(domain.IncidentResolved) || resolvedAt == nil {
		t.Fatalf("incident is %q/resolved_at=%v after disowning; it would never be resolved by anything "+
			"again, and it blocks the next outage from opening one", status, resolvedAt)
	}
	// The timeline says WHY, and refuses to be read as a recovery.
	var body string
	if err := st.pool.QueryRow(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 AND status = 'resolved'
		  ORDER BY created_at DESC LIMIT 1`, inc.ID).Scan(&body); err != nil {
		t.Fatalf("read closing note: %v", err)
	}
	if !strings.Contains(body, "paging ownership") || !strings.Contains(body, "not a statement that the service recovered") {
		t.Fatalf("closing note = %q; it must name the reason and deny a recovery", body)
	}
	// And the ending was announced, like any other incident ending.
	if got := incidentEventTypes(t, st, ctx); len(got) != 2 || got[1] != string(domain.EventIncidentResolved) {
		t.Fatalf("lifecycle events = %v, want an opened followed by a resolved", got)
	}
	// The service can now own paging again and the NEXT outage opens its own incident.
	on := true
	if _, err := st.UpdateServiceAlertPolicy(ctx, f.projectID, f.serviceID,
		ServiceAlertPolicyPatch{OwnsPaging: &on}, AlertActor{}); err != nil {
		t.Fatalf("re-own: %v", err)
	}
	next := openLiveOccurrence(t, st, ctx, f)
	if next.ID == inc.ID {
		t.Fatal("the second outage reused the first incident — the survivor was still occupying the index")
	}
}

// A BURN close is not the outage record: disabling a burn target says nothing about whether the
// service is down, so the incident must survive it untouched.
func TestDisablingBurnAlertsLeavesTheOutageIncidentAlone(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openLiveOccurrence(t, st, ctx, f)

	// There must be an open BURN episode for this test to mean anything: with nothing to close, the
	// close loop never runs and the assertion below would hold for the wrong reason.
	openBurnOccurrence(t, st, ctx, f)

	if err := st.SetServiceBurnAlerting(ctx, f.projectID, f.serviceID, "30d", false, nil, AlertActor{}); err != nil {
		t.Fatalf("disable burn: %v", err)
	}
	var closed int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM service_alert_episodes WHERE signal = 'burn' AND closed_at IS NOT NULL`).
		Scan(&closed); err != nil {
		t.Fatalf("count closed burn episodes: %v", err)
	}
	if closed != 1 {
		t.Fatalf("%d burn episodes closed, want 1 — the fixture did not exercise the close path", closed)
	}

	var status string
	if err := st.pool.QueryRow(ctx,
		`SELECT status FROM incidents WHERE id = $1`, inc.ID).Scan(&status); err != nil {
		t.Fatalf("reread incident: %v", err)
	}
	if status != string(domain.IncidentInvestigating) {
		t.Fatalf("incident is %q after a BURN close; a budget alert is not the outage record", status)
	}
}

// openLiveOccurrence puts the service in the state a live outage leaves behind: an open health
// episode and the auto-incident the evaluator opened for it.
func openLiveOccurrence(t *testing.T, st *Store, ctx context.Context, f armFixture) domain.Incident {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	inc, created, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout down", 2, governingRevision(t, st, ctx, f.serviceID))
	if err != nil || !created {
		t.Fatalf("open incident: %v (created=%v)", err, created)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_alert_episodes
		  (service_id, project_id, service_name, signal, state, recipients, emitted_seq)
		SELECT s.id, s.project_id, s.name, 'health', 'down', '["chan-1"]'::jsonb, 1
		  FROM services s WHERE s.id = $1`, f.serviceID); err != nil {
		t.Fatalf("open episode: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE service_alert_state SET live_firing = true, emitted_state = 'down', emitted_seq = 1
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("latch live: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inc
}

func incidentEventTypes(t *testing.T, st *Store, ctx context.Context) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT payload->>'event' FROM outbox_events WHERE topic = $1 ORDER BY created_at, id`,
		domain.TopicIncidentEvent)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan type: %v", err)
		}
		out = append(out, typ)
	}
	return out
}

// openBurnOccurrence adds an OPEN burn episode with its latch, so a burn lifecycle close has
// something to close. The three target columns travel together: 00082's CHECK ties rule_key,
// target_snapshot_id and target_window to the burn signal.
func openBurnOccurrence(t *testing.T, st *Store, ctx context.Context, f armFixture) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_alert_episodes
		  (service_id, project_id, service_name, signal, rule_key, target_snapshot_id, target_window,
		   state, recipients, emitted_seq)
		SELECT s.id, s.project_id, s.name, 'burn', 'page/3600/300/14', $2, '30d',
		       'page/3600/300/14', '["chan-1"]'::jsonb, 1
		  FROM services s WHERE s.id = $1`, f.serviceID, f.targetID); err != nil {
		t.Fatalf("open burn episode: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict,
		   target_generation, config_generation, emitted_seq, evaluated_at, lease_until)
		SELECT s.id, s.project_id, t.id, 'page/3600/300/14', true, 'fire', t.alert_generation,
		       s.alert_config_generation, 1, now(), now() + interval '90 seconds'
		  FROM services s JOIN sla_targets t ON t.id = $2
		 WHERE s.id = $1
		ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO NOTHING`,
		f.serviceID, f.targetID); err != nil {
		t.Fatalf("latch burn: %v", err)
	}
}

// governingRevision resolves the declaration in force NOW, the way the epoch owner and the evaluator
// resolve it: the latest boundary that has PASSED. Tests call it so they hand the opener the same id a
// real evaluator would, rather than whatever happens to be newest.
func governingRevision(t *testing.T, st *Store, ctx context.Context, serviceID string) *string {
	t.Helper()
	var id *string
	if err := st.pool.QueryRow(ctx, `
		SELECT r.id::text FROM service_definition_revisions r
		 WHERE r.service_id = $1 AND r.state = 'effective' AND r.effective_at <= now()
		 ORDER BY r.effective_at DESC, r.revision DESC LIMIT 1`, serviceID).Scan(&id); err != nil {
		if noRows(err) {
			return nil
		}
		t.Fatalf("resolve governing revision: %v", err)
	}
	return id
}

// FR-022 invariant 13 / spec D6 — the postmortem names the declaration the OUTAGE was measured
// against, not one that becomes effective later.
//
// The window is ordinary, not exotic. `service_definition_revisions.state` defaults to 'effective' the
// moment a revision is authored (00064), and its `effective_at` sits on the next bucket boundary — so
// every ordinary edit opens a gap in which "the latest effective revision" and "the revision governing
// now" are different rows. An incident opened inside that gap used to snapshot the members of a
// declaration that had not started governing anything.
func TestTheIncidentSnapshotNamesTheGoverningDeclaration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// A second monitor, and a NEXT revision that adds it, effective a minute from now.
	other, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: f.projectID, Name: "checkout-dns", Type: domain.MonitorHTTP,
		Target: "https://dns.example.com/", IntervalSeconds: 30, Region: "core", Enabled: true,
	})
	if err != nil {
		t.Fatalf("second monitor: %v", err)
	}
	var futureRev string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO service_definition_revisions (service_id, project_id, revision, effective_at)
		SELECT s.id, s.project_id, 99, now() + interval '1 minute' FROM services s WHERE s.id = $1
		RETURNING id::text`, f.serviceID).Scan(&futureRev); err != nil {
		t.Fatalf("future revision: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_definition_members (revision_id, project_id, monitor_id, monitor_name, role)
		VALUES ($1::uuid, $2, $3, 'checkout-dns', 'sli')`, futureRev, f.projectID, other.ID); err != nil {
		t.Fatalf("future members: %v", err)
	}
	// It is 'effective' ALREADY — that is the trap, spelled out rather than assumed.
	var state string
	if err := st.pool.QueryRow(ctx,
		`SELECT state FROM service_definition_revisions WHERE id = $1::uuid`, futureRev).Scan(&state); err != nil {
		t.Fatalf("read future state: %v", err)
	}
	if state != "effective" {
		t.Fatalf("the future revision is %q; this test no longer exercises the window it was written for", state)
	}

	inc := openLiveOccurrence(t, st, ctx, f)

	names := snapshotNames(t, st, ctx, inc.ID)
	for _, n := range names {
		if n == "checkout-dns" {
			t.Fatalf("the snapshot named %v — it took a declaration that governs only from the next "+
				"boundary, while the outage was computed from the previous one", names)
		}
	}
	if len(names) == 0 {
		t.Fatal("the snapshot is empty; the governing declaration does have members")
	}

	// And a service with NO governing declaration snapshots nothing rather than reaching for the
	// newest one — the fallback is the defect one level down.
	if err := snapshotServiceMembersTx(ctx, mustTx(t, st, ctx), "00000000-0000-0000-0000-000000000000",
		f.projectID, f.serviceID, nil); err != nil {
		t.Fatalf("nil revision must be a no-op, got %v", err)
	}
}

func snapshotNames(t *testing.T, st *Store, ctx context.Context, incidentID string) []string {
	t.Helper()
	var raw []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT members FROM incident_member_snapshots WHERE incident_id = $1`, incidentID).Scan(&raw); err != nil {
		if noRows(err) {
			return nil
		}
		t.Fatalf("read snapshot: %v", err)
	}
	var members []domain.IncidentMember
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Name)
	}
	return out
}

func mustTx(t *testing.T, st *Store, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return tx
}

// A revision that does not belong to this service is REFUSED, not recorded as "no members". The
// difference matters because the snapshot is what a postmortem reads: `members: []` is a claim that
// the declaration named nobody, and storing it for a revision we simply had no business reading
// would put a false statement in the incident record.
func TestSnapshotRefusesARevisionThatIsNotTheServices(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// A second service in the same project, with its own declaration.
	other, err := st.CreateService(ctx, domain.Service{ProjectID: f.projectID, Slug: "cart", Name: "Cart"})
	if err != nil {
		t.Fatalf("second service: %v", err)
	}
	var foreign string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO service_definition_revisions (service_id, project_id, revision, effective_at)
		VALUES ($1, $2, 1, now() - interval '1 hour') RETURNING id::text`,
		other.ID, f.projectID).Scan(&foreign); err != nil {
		t.Fatalf("foreign revision: %v", err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only probe
	err = snapshotServiceMembersTx(ctx, tx, "00000000-0000-0000-0000-000000000000",
		f.projectID, f.serviceID, &foreign)
	if err == nil {
		t.Fatal("another service's revision was accepted: the snapshot would record its members, or " +
			"an empty list, as the truth about this incident")
	}
	if !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("refusal = %v, want it to name the ownership failure", err)
	}
}

// FR-022 invariant 5 in FULL: "only while that signal's coverage is ARMED". `owns_paging` is one of
// five conditions and the only one the evaluator was checking — the slice filters on it. Two others
// can be false at exactly the moment a service decides to speak.
//
// The route case is the sharper one. With ownership on, a pageable DOWN and no enabled channel, the
// announcement reaches nobody, delegation is DIS-ARMED through its routable clause, and the members
// are correctly paging for themselves. Opening an incident there is what this file's own comment
// calls worse than opening none — and latching `live_firing` while doing it is worse still: restoring
// the channel later produces no edge, so the outage is never paged at all.
func TestAnUnroutableServiceOpensNothingAndDoesNotSwallowTheLaterOnset(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// Take the route away, leaving every other arming condition true.
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("disable channels: %v", err)
	}

	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	got := evalOnce(t, st, ctx)

	if got.Onsets != 0 || got.IncidentsOpened != 0 {
		t.Fatalf("an UNROUTABLE service announced %+v — nobody could be told, and its members are "+
			"already paging for themselves", got)
	}
	if got.Withheld[WithheldUnroutable] != 1 {
		t.Fatalf("the withheld onset was counted as %+v, want one under %q: a broken route and an "+
			"absent declaration are different problems with different owners",
			got.Withheld, WithheldUnroutable)
	}
	var incidents int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incidents WHERE service_id = $1`, f.serviceID).Scan(&incidents); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if incidents != 0 {
		t.Fatalf("%d incident(s) opened for an announcement nobody received", incidents)
	}
	// The latch did NOT advance, which is the half that decides whether the outage is ever paged.
	var firing bool
	var emitted *string
	if err := st.pool.QueryRow(ctx,
		`SELECT live_firing, emitted_state FROM service_alert_state WHERE service_id = $1`,
		f.serviceID).Scan(&firing, &emitted); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if firing || emitted != nil {
		t.Fatalf("the service latched (firing=%v emitted=%v) while announcing nothing: restoring the "+
			"route would produce no edge and the outage would never be paged", firing, emitted)
	}

	// Give it a route back. The SAME outage now announces and opens its incident.
	if _, err := st.pool.Exec(ctx,
		`UPDATE notification_channels SET enabled = true WHERE project_id = $1`, f.projectID); err != nil {
		t.Fatalf("re-enable channels: %v", err)
	}
	got = evalOnce(t, st, ctx)
	if got.Onsets != 1 || got.IncidentsOpened != 1 {
		t.Fatalf("after the route came back the evaluation reported %+v, want one onset and one "+
			"incident for the outage that was waiting", got)
	}
}

// The same fail-closed rule for the OTHER missing arming condition: before any declaration governs,
// `revision_id` is NULL, the verdict was computed from nothing, and delegation is dis-armed.
func TestAServiceWithNoGoverningDeclarationOpensNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := alertingService(t, st, ctx)

	// Push every revision into the future: authored, effective later, governing nothing right now.
	if _, err := st.pool.Exec(ctx,
		`UPDATE service_definition_revisions SET effective_at = now() + interval '1 hour'
		  WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("defer the declaration: %v", err)
	}

	setMemberHealth(t, st, ctx, f, true)
	evalOnce(t, st, ctx)
	setMemberHealth(t, st, ctx, f, false)
	evalOnce(t, st, ctx)
	got := evalOnce(t, st, ctx)

	var incidents int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM incidents WHERE service_id = $1`, f.serviceID).Scan(&incidents); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if got.IncidentsOpened != 0 || incidents != 0 {
		t.Fatalf("a service with no governing declaration opened %d incident(s) (%+v): the verdict "+
			"was computed from no declaration at all", incidents, got)
	}
	if got.Withheld[WithheldNoGoverningRevision] != 1 {
		t.Fatalf("withheld = %+v, want one under %q — reporting this as a broken route would send "+
			"somebody to fix a paging configuration that is fine",
			got.Withheld, WithheldNoGoverningRevision)
	}
}

// The clock the lifecycle close stamps the incident with is the caller's post-lock instant, not the
// transaction's start.
//
// D-0177's clock work fixed the manual paths — `AddIncidentUpdate`, `AcknowledgeIncident` — and this
// one kept reading `now()`. A writer that waited on a row lock then stamped a resolution EARLIER than
// the action that caused it, so the timeline claimed the incident ended before the close that ended
// it, and `ListIncidentUpdates` (which orders by `created_at`) rendered them in that order.
func TestAServiceIncidentResolvesAfterTheWaitItActuallyDid(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	// Open the incident and COMMIT it, so a second connection can hold it.
	setup, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if _, _, err := st.OpenServiceIncidentTx(ctx, setup, f.serviceID, f.projectID, "down", 3, nil); err != nil {
		_ = setup.Rollback(ctx)
		t.Fatalf("open: %v", err)
	}
	if err := setup.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// A BLOCKER holds the incident row, the way a manual writer does.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	var held string
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM incidents WHERE service_id = $1 FOR UPDATE`, f.serviceID).Scan(&held); err != nil {
		_ = blocker.Rollback(ctx)
		t.Fatalf("hold incident: %v", err)
	}

	// The resolver's transaction starts NOW — so its transaction clock is now — and then waits.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin resolver: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Touch the transaction so its start clock is materialised before the wait begins.
	var txStart time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txStart); err != nil {
		t.Fatalf("read transaction clock: %v", err)
	}

	// The resolver's own backend, so the wait below watches THIS transaction and not whatever else
	// happens to be running. `pg_locks` filtered only by "some ungranted tuple lock" would accept a
	// stranger's waiter on a shared database and hand this test a floor it never earned.
	var resolverPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&resolverPID); err != nil {
		t.Fatalf("resolver pid: %v", err)
	}
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, rerr := st.ResolveServiceIncidentTx(ctx, tx, f.serviceID, "recovered")
		done <- rerr
	}()

	// Wait until the resolver is genuinely BLOCKED on the row, not merely slow to start. Anything
	// less makes this a sleep, and a sleep proves nothing about a lock.
	waitUntilBlockedBy(t, st, ctx, resolverPID, blockerPID)

	// The floor: every instant from here on is after the wait. Read on a third connection so the
	// resolver's own transaction cannot supply it.
	var floor time.Time
	if err := st.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&floor); err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if rerr := <-done; rerr != nil {
		t.Fatalf("resolve: %v", rerr)
	}

	var resolvedAt, updatedAt, noteAt, outboxCreated, outboxUpdated, outboxDue time.Time
	if err := tx.QueryRow(ctx, `
		SELECT i.resolved_at, i.updated_at, u.created_at,
		       o.created_at, o.updated_at, o.next_attempt_at
		  FROM incidents i
		  JOIN incident_updates u ON u.incident_id = i.id AND u.status = 'resolved'
		  JOIN outbox_events o ON o.topic = 'incident_event'
		                      AND o.payload -> 'incident' ->> 'id' = i.id::text
		                      AND o.payload ->> 'event' = 'incident.resolved'
		 WHERE i.service_id = $1`, f.serviceID).Scan(
		&resolvedAt, &updatedAt, &noteAt, &outboxCreated, &outboxUpdated, &outboxDue); err != nil {
		t.Fatalf("read: %v", err)
	}
	for name, got := range map[string]time.Time{
		"resolved_at": resolvedAt, "updated_at": updatedAt, "the timeline note": noteAt,
		"outbox created_at": outboxCreated, "outbox updated_at": outboxUpdated,
		"outbox next_attempt_at": outboxDue,
	} {
		if got.UTC().Before(floor.UTC()) {
			t.Fatalf("%s = %s is BEFORE the lock was released (%s): the resolver stamped an instant "+
				"from before the wait it actually did, so the record says the incident ended before "+
				"the close that ended it", name, got.UTC(), floor.UTC())
		}
	}
}

// waitUntilBlockedBy returns once `waiter` is blocked SPECIFICALLY by `holder`. `pg_blocking_pids`
// answers exactly that question, which is the whole reason to use it: an earlier version asked
// whether ANY ungranted tuple lock existed anywhere in the cluster, so on a shared or concurrent
// database an unrelated waiter would satisfy it and the floor read afterwards would be one this test
// never earned.
//
// Polling rather than sleeping, for the same class of reason: a timer passes whether or not the
// resolver ever queued.
func waitUntilBlockedBy(t *testing.T, st *Store, ctx context.Context, waiter, holder int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := st.pool.QueryRow(ctx,
			`SELECT $2::int = ANY(pg_blocking_pids($1::int))`, waiter, holder).Scan(&blocked); err != nil {
			t.Fatalf("poll blocking pids: %v", err)
		}
		if blocked {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never blocked on backend %d: without a real wait this test proves nothing "+
		"about a post-lock clock", waiter, holder)
}
