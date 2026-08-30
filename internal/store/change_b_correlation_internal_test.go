package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D7 under adversity (func-change-intelligence §7 *Correlation*, invariants 7, 8, 22;
// iter-0165 task 2, Agent B; reviewer [41]): a phase RECORDED after the open is never a candidate
// however it is dated; a phase that commits while the delivery is parked between its candidate
// SELECT and its link INSERT changes nothing about the link; the window is closed at both ends;
// and no impact row can carry the correlation into another project.

// Reviewer [41]: `change.max_past` lets a pipeline record a phase whose occurred_at lies a day
// back, so a delayed `opened` delivery would find a phase dated INSIDE the window that was
// recorded AFTER the incident opened. The candidate universe is the rows recorded by the open
// (recorded_at <= opened_at): a backdated after-open phase alone links nothing and writes no
// note; beside a phase recorded before the open, THAT phase is the anchor and the later one
// neither anchors nor moves the group's latest.
func TestChangeCorrelationIgnoresPhasesRecordedAfterTheOpenHoweverTheyAreDated(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	opened := gateDBNow(t, st, ctx)
	inc := openServiceIncidentAt(t, st, ctx, f, opened)

	// Recorded now (after the open), dated two minutes BEFORE it — inside the window.
	ghost := mustRecord(t, st, ctx, changeInput(f, "ghost", domain.ChangePhaseSucceeded, opened.Add(-2*time.Minute)))
	if !ghost.RecordedAt.After(opened) {
		t.Fatalf("fixture: ghost recorded_at %s is not after the open %s", ghost.RecordedAt, opened)
	}
	res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
	if err != nil || res.Skipped || len(res.Links) != 0 || res.NoteAdded {
		t.Fatalf("a backdated phase recorded after the open was correlated: %+v %v", res, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 0 || len(changeNotes(t, st, ctx, inc)) != 0 {
		t.Fatalf("%d links and %d notes for an incident with only after-open rows", n, len(changeNotes(t, st, ctx, inc)))
	}

	// A `started` recorded (and dated) twenty minutes before the open — the retention planter
	// writes recorded_at = occurred_at — then its terminal recorded now and dated a minute before
	// the open: the anchor is the started row, the lag its own, the ghost still absent.
	startedID := plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "run-1", domain.ChangeKindDeploy, domain.ChangePhaseStarted, opened.Add(-20*time.Minute))
	late := changeInput(f, "run-1", domain.ChangePhaseSucceeded, opened.Add(-time.Minute))
	late.Source = "ci"
	terminal := mustRecord(t, st, ctx, late)
	res, err = st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
	if err != nil || len(res.Links) != 1 || !res.NoteAdded {
		t.Fatalf("with one pre-open row: %+v %v", res, err)
	}
	l := res.Links[0]
	if l.ChangeID != startedID || l.LagSeconds != 1200 || !l.OccurredAt.Equal(opened.Add(-20*time.Minute)) || l.Role != domain.ChangeLinkRoleOwnService {
		t.Fatalf("link = %+v, want the started row %s at lag 1200", l, startedID)
	}
	if l.ChangeID == terminal.ID {
		t.Fatal("the after-open terminal became the anchor")
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1 AND change_id IN ($2, $3)`, inc, ghost.ID, terminal.ID); n != 0 {
		t.Fatalf("%d after-open rows are linked", n)
	}
	notes := changeNotes(t, st, ctx, inc)
	if len(notes) != 1 || notes[0] != "🚀 Changes: 1 preceded this incident — deploy v1 by ci, −20m." {
		t.Fatalf("note = %q", notes)
	}
	// The incident side reads the anchor with the group's live phases beside it.
	links, err := st.ListIncidentChanges(ctx, f.projectID, inc)
	if err != nil || len(links) != 1 || links[0].Change.ID != startedID || len(links[0].Phases) != 2 || links[0].Phases[1].ID != terminal.ID {
		t.Fatalf("incident side = %+v %v", links, err)
	}
}

// A phase that commits while the delivery is parked between its candidate SELECT and its link
// INSERT: the link anchors the row the SELECT saw, its occurred_at and lag are that row's, the
// foreign key holds, and the group's live phases show the newcomer beside the anchor. The
// delivery is parked at its INSERT by a FOR UPDATE on the anchor row (the FK check needs KEY
// SHARE on it).
func TestChangeCorrelationAnchorsTheRowItSelectedWhenAPhaseCommitsMidDelivery(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	started := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	opened := gateDBNow(t, st, ctx)
	inc := openServiceIncidentAt(t, st, ctx, f, opened)

	park, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer park.Rollback(ctx) //nolint:errcheck
	if _, err := park.Exec(ctx, `SELECT 1 FROM service_changes WHERE id = $1 FOR UPDATE`, started.ID); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res ChangeCorrelation
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
		done <- outcome{res, err}
	}()
	blocked := waitForBlockedBackends(t, st, ctx, 1)
	if !strings.Contains(blocked[0].query, "INSERT INTO incident_changes") {
		t.Fatalf("the delivery is parked at %+v, want its link INSERT", blocked[0])
	}
	// The newcomer commits now — after the SELECT, before the INSERT.
	terminal := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, opened.Add(-time.Second)))
	select {
	case o := <-done:
		t.Fatalf("the delivery returned while parked: %+v %v", o.res, o.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := park.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery never returned")
	}
	if o.err != nil || len(o.res.Links) != 1 || !o.res.NoteAdded {
		t.Fatalf("delivery = %+v %v", o.res, o.err)
	}
	l := o.res.Links[0]
	if l.ChangeID != started.ID || l.LagSeconds != 600 || !l.OccurredAt.Equal(started.OccurredAt) {
		t.Fatalf("link = %+v, want the started row %s at lag 600", l, started.ID)
	}
	if n := countSQL(t, st, ctx, `
		SELECT count(*) FROM incident_changes ic JOIN service_changes c ON c.id = ic.change_id AND c.project_id = ic.project_id
		 WHERE ic.incident_id = $1 AND ic.change_id = $2 AND ic.occurred_at = c.occurred_at`, inc, started.ID); n != 1 {
		t.Fatalf("the link does not join its anchor row with the copied instant (%d)", n)
	}
	links, err := st.ListIncidentChanges(ctx, f.projectID, inc)
	if err != nil || len(links) != 1 || links[0].Change.ID != started.ID || links[0].LagSeconds != 600 ||
		len(links[0].Phases) != 2 || links[0].Phases[1].ID != terminal.ID {
		t.Fatalf("incident side = %+v %v, want the anchor with both live phases", links, err)
	}
	if notes := changeNotes(t, st, ctx, inc); len(notes) != 1 || !strings.Contains(notes[0], "−10m") {
		t.Fatalf("note = %q", notes)
	}
}

// The window `[opened_at − window, opened_at]` is closed at both ends: a change exactly at the
// lower bound links with lag = window, one microsecond below it does not; a change exactly AT
// the open links with lag 0, one microsecond after it does not. The note names the zero lag.
func TestChangeCorrelationWindowIsClosedAtBothEndsAndAZeroLagIsALink(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	opened := now.Add(time.Minute) // ahead of the clock: every row here is recorded by the open
	mustRecord(t, st, ctx, changeInput(f, "edge-in", domain.ChangePhaseSucceeded, opened.Add(-60*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "edge-out", domain.ChangePhaseSucceeded, opened.Add(-60*time.Minute-time.Microsecond)))
	mustRecord(t, st, ctx, changeInput(f, "at-open", domain.ChangePhaseSucceeded, opened))
	mustRecord(t, st, ctx, changeInput(f, "just-after", domain.ChangePhaseSucceeded, opened.Add(time.Microsecond)))
	inc := openServiceIncidentAt(t, st, ctx, f, opened)

	res, err := st.LinkPrecedingChanges(ctx, inc, 60*time.Minute, 5)
	if err != nil || len(res.Links) != 2 {
		t.Fatalf("links = %+v %v, want edge-in and at-open", res, err)
	}
	if res.Links[0].ExternalID != "at-open" || res.Links[0].LagSeconds != 0 || res.Links[1].ExternalID != "edge-in" || res.Links[1].LagSeconds != 3600 {
		t.Fatalf("links = %+v, want at-open (0) then edge-in (3600)", res.Links)
	}
	want := "🚀 Changes: 2 preceded this incident — deploy v4.2.1 by github-actions, −0s; deploy v4.2.1 by github-actions, −1h."
	if notes := changeNotes(t, st, ctx, inc); len(notes) != 1 || notes[0] != want {
		t.Fatalf("note = %q, want %q", notes, want)
	}
}

// No impact row can carry the correlation into another project: an `incident_service_impacts`
// row that names a service of another project is refused by its composite foreign key under
// either project spelling, so a `probable_root` in another project cannot exist; and even the
// link table refuses a change of another project by direct SQL. The store path with a legitimate
// upstream links only rows of the incident's project.
func TestChangeCorrelationCannotReachAnotherProjectEvenThroughAnImpactRow(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	opened := now.Add(time.Minute)
	foreign := changeInput(f, "foreign-1", domain.ChangePhaseSucceeded, opened.Add(-5*time.Minute))
	foreign.ProjectID, foreign.ServiceID = f.otherProjectID, f.otherServiceID
	foreignRow := mustRecord(t, st, ctx, foreign)
	up := changeInput(f, "up-1", domain.ChangePhaseSucceeded, opened.Add(-7*time.Minute))
	up.ServiceID = f.upstreamID
	upRow := mustRecord(t, st, ctx, up)
	inc := openServiceIncidentAt(t, st, ctx, f, opened)

	for _, project := range []string{f.projectID, f.otherProjectID} {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO incident_service_impacts (incident_id, service_id, project_id, role, path)
			VALUES ($1, $2, $3, 'probable_root', ARRAY['foreign', 'checkout'])`, inc, f.otherServiceID, project)
		if !isForeignKeyViolation(err) {
			t.Fatalf("impact row naming another project's service under project %s: %v, want an FK violation", project, err)
		}
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO incident_service_impacts (incident_id, service_id, project_id, role, path)
		VALUES ($1, $2, $3, 'probable_root', ARRAY['upstream-db', 'checkout'])`, inc, f.upstreamID, f.projectID); err != nil {
		t.Fatal(err)
	}
	res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
	if err != nil || len(res.Links) != 1 || res.Links[0].ChangeID != upRow.ID || res.Links[0].Role != domain.ChangeLinkRoleUpstream {
		t.Fatalf("links = %+v %v, want exactly the upstream row", res, err)
	}
	for _, project := range []string{f.projectID, f.otherProjectID} {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)
			VALUES ($1, $2, $3, 'upstream', $4, 300)`, inc, foreignRow.ID, project, foreignRow.OccurredAt)
		if !isForeignKeyViolation(err) {
			t.Fatalf("link to another project's change under project %s: %v, want an FK violation", project, err)
		}
	}
	// Both read directions stay inside the project.
	if _, err := st.ListIncidentChanges(ctx, f.otherProjectID, inc); err == nil {
		t.Fatal("another project read the incident's links")
	}
	pages := listAllGroups(t, st, ctx, changeFixture{reportFixture: f.reportFixture}, opened.Add(-time.Hour), opened.Add(time.Hour), nil, nil, 50)
	for _, g := range pages[0] {
		if g.ExternalID == "foreign-1" {
			t.Fatal("another project's group appeared on this service's timeline")
		}
	}
	_ = context.Background
}
