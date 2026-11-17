package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.3 — the rise step, called directly. The evaluator's wiring is tested where it lives.

// raiseOnce runs the rise step in its own transaction and commits it.
func raiseOnce(t *testing.T, st *Store, ctx context.Context, serviceID string, state domain.ServiceAlertState) bool {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	raised, err := raiseServiceIncidentImpactTx(ctx, tx, serviceID, state)
	if err != nil {
		t.Fatalf("raise %s: %v", state, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return raised
}

// openAt opens the service's auto-incident at a state under a mapping, committed.
func openAt(t *testing.T, st *Store, ctx context.Context, f armFixture,
	state domain.ServiceAlertState, m domain.ImpactMapping) domain.Incident {
	t.Helper()
	inc, created, err := openMappedServiceIncident(t, st, ctx, f.serviceID, f.projectID, state, m, 2)
	if err != nil || !created {
		t.Fatalf("open at %s: created=%v err=%v", state, created, err)
	}
	return inc
}

type riseNote struct {
	body, author string
	status       domain.IncidentStatus
	createdAt    time.Time
}

// riseNotes returns the incident's `⬆ Impact raised:` notes in order.
func riseNotes(t *testing.T, st *Store, ctx context.Context, incidentID string) []riseNote {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT body, author, status, created_at FROM incident_updates
		 WHERE incident_id = $1 AND body LIKE $2 || '%'
		 ORDER BY created_at, id`, incidentID, domain.ImpactRaisedMarker)
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	defer rows.Close()
	var out []riseNote
	for rows.Next() {
		var n riseNote
		if err := rows.Scan(&n.body, &n.author, &n.status, &n.createdAt); err != nil {
			t.Fatalf("scan note: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notes: %v", err)
	}
	return out
}

type riseEvent struct {
	seq     int64
	raised  string // payload->'impact_raised' as compact JSON text
	payload string
}

// riseEvents returns the incident's `incident.updated` outbox rows, by seq.
func riseEvents(t *testing.T, st *Store, ctx context.Context, incidentID string) []riseEvent {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT (payload->>'seq')::bigint, COALESCE((payload->'impact_raised')::text, ''), payload::text
		  FROM outbox_events
		 WHERE topic = 'incident_event'
		   AND payload->'incident'->>'id' = $1
		   AND payload->>'event' = 'incident.updated'
		 ORDER BY (payload->>'seq')::bigint`, incidentID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []riseEvent
	for rows.Next() {
		var e riseEvent
		if err := rows.Scan(&e.seq, &e.raised, &e.payload); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return out
}

// sameJSON compares two JSON texts by value.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON %s: %v", want, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	return string(gb) == string(wb)
}

// incidentState is what a rise may and may not touch.
type incidentState struct {
	impact    domain.IncidentImpact
	status    domain.IncidentStatus
	updatedAt time.Time
	basis     string
	notes     int
	outbox    int
}

func readIncidentState(t *testing.T, st *Store, ctx context.Context, incidentID string) incidentState {
	t.Helper()
	var s incidentState
	if err := st.pool.QueryRow(ctx, `
		SELECT impact, status, updated_at, COALESCE(impact_basis::text, ''),
		       (SELECT count(*) FROM incident_updates WHERE incident_id = $1),
		       (SELECT count(*) FROM outbox_events)
		  FROM incidents WHERE id = $1`, incidentID).Scan(
		&s.impact, &s.status, &s.updatedAt, &s.basis, &s.notes, &s.outbox); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	return s
}

// I2, I6 — degraded (minor) then down: one rise to major, one note, one event.
func TestARiseFromDegradedToDownWritesOneNoteAndOneEvent(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if inc.Impact != domain.ImpactMinor {
		t.Fatalf("opened at %q, want minor", inc.Impact)
	}

	if !raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("degraded → down did not raise")
	}
	after := readIncidentState(t, st, ctx, inc.ID)
	if after.impact != domain.ImpactMajor {
		t.Errorf("impact %q, want major", after.impact)
	}
	b := readBasis(t, st, ctx, inc.ID)
	if b.State != domain.ServiceAlertDown || b.Source != domain.ImpactBasisServiceMapping ||
		b.Mapping == nil || *b.Mapping != domain.DefaultImpactMapping() ||
		b.Service == nil || b.Service.Name != "Checkout" || b.Service.Slug != "checkout" {
		t.Errorf("basis after the rise %+v: only state may change", b)
	}

	notes := riseNotes(t, st, ctx, inc.ID)
	if len(notes) != 1 {
		t.Fatalf("%d rise notes, want 1", len(notes))
	}
	if want := "⬆ Impact raised: minor → major — the service is down."; notes[0].body != want {
		t.Errorf("note %q, want %q", notes[0].body, want)
	}
	if notes[0].author != "system" || notes[0].status != after.status {
		t.Errorf("note author %q status %q; want system and the incident's %q",
			notes[0].author, notes[0].status, after.status)
	}

	events := riseEvents(t, st, ctx, inc.ID)
	if len(events) != 1 {
		t.Fatalf("%d incident.updated events, want 1", len(events))
	}
	if !sameJSON(t, events[0].raised, `{"from":"minor","to":"major"}`) {
		t.Errorf("impact_raised %s", events[0].raised)
	}
	if strings.Contains(events[0].payload, "impact_basis") {
		t.Errorf("the event carries impact_basis (I11): %s", events[0].payload)
	}
}

// D2 — a fall changes nothing: the peak stands.
func TestAFallChangesNothing(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDown, domain.DefaultImpactMapping())
	before := readIncidentState(t, st, ctx, inc.ID)

	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDegraded) {
		t.Fatal("down → degraded raised")
	}
	if after := readIncidentState(t, st, ctx, inc.ID); after != before {
		t.Errorf("a fall wrote something:\n before %+v\n  after %+v", before, after)
	}
}

func TestARiseFromUnknownToDown(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertUnknown, domain.DefaultImpactMapping())
	if inc.Impact != domain.ImpactNone {
		t.Fatalf("opened at %q, want none", inc.Impact)
	}
	if !raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("unknown → down did not raise")
	}
	if got := readIncidentState(t, st, ctx, inc.ID).impact; got != domain.ImpactMajor {
		t.Errorf("impact %q, want major", got)
	}
	notes := riseNotes(t, st, ctx, inc.ID)
	if len(notes) != 1 || notes[0].body != "⬆ Impact raised: none → major — the service is down." {
		t.Errorf("notes %+v", notes)
	}
	events := riseEvents(t, st, ctx, inc.ID)
	if len(events) != 1 || !sameJSON(t, events[0].raised, `{"from":"none","to":"major"}`) {
		t.Errorf("events %+v", events)
	}
}

// I6 — an incident may rise more than once; each rise has its own note and event. A marker guard
// (one note per incident, like `⚡ Context:`) would fail this (R9).
func TestAnIncidentCanRiseTwice(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertUnknown, domain.DefaultImpactMapping())

	if !raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDegraded) {
		t.Fatal("unknown → degraded did not raise")
	}
	if !raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("degraded → down did not raise")
	}
	if got := readIncidentState(t, st, ctx, inc.ID).impact; got != domain.ImpactMajor {
		t.Errorf("impact %q, want major", got)
	}
	notes := riseNotes(t, st, ctx, inc.ID)
	wantNotes := []string{
		"⬆ Impact raised: none → minor — the service is degraded.",
		"⬆ Impact raised: minor → major — the service is down.",
	}
	if len(notes) != len(wantNotes) {
		t.Fatalf("%d rise notes, want %d: %+v", len(notes), len(wantNotes), notes)
	}
	for i, w := range wantNotes {
		if notes[i].body != w {
			t.Errorf("note %d %q, want %q", i, notes[i].body, w)
		}
	}
	events := riseEvents(t, st, ctx, inc.ID)
	if len(events) != 2 {
		t.Fatalf("%d incident.updated events, want 2", len(events))
	}
	if events[0].seq >= events[1].seq {
		t.Errorf("seqs %d, %d do not increase", events[0].seq, events[1].seq)
	}
	if !sameJSON(t, events[0].raised, `{"from":"none","to":"minor"}`) ||
		!sameJSON(t, events[1].raised, `{"from":"minor","to":"major"}`) {
		t.Errorf("impact_raised pairs %s, %s", events[0].raised, events[1].raised)
	}
}

// I6 — a re-evaluation of the same state writes nothing.
func TestTheSameStateTwiceRaisesOnce(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if !raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("first step did not raise")
	}
	before := readIncidentState(t, st, ctx, inc.ID)
	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("second step with the same state raised")
	}
	if after := readIncidentState(t, st, ctx, inc.ID); after != before {
		t.Errorf("the repeat wrote something:\n before %+v\n  after %+v", before, after)
	}
}

// D12 — an incident with no basis never rises.
func TestAnIncidentWithoutABasisNeverRises(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if _, err := st.pool.Exec(ctx, `UPDATE incidents SET impact_basis = NULL WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("clear basis: %v", err)
	}
	before := readIncidentState(t, st, ctx, inc.ID)
	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("an incident with no basis rose")
	}
	if after := readIncidentState(t, st, ctx, inc.ID); after != before {
		t.Errorf("wrote something:\n before %+v\n  after %+v", before, after)
	}
}

func TestOnlyAServiceMappingBasisRises(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if _, err := st.pool.Exec(ctx,
		`UPDATE incidents SET impact_basis = '{"source":"manual"}'::jsonb WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("set manual basis: %v", err)
	}
	before := readIncidentState(t, st, ctx, inc.ID)
	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("a manual basis rose")
	}
	if after := readIncidentState(t, st, ctx, inc.ID); after != before {
		t.Errorf("wrote something:\n before %+v\n  after %+v", before, after)
	}
}

func TestNoOpenIncidentMeansNoRise(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)

	countAll := func() (int, int) {
		var notes, outbox int
		if err := st.pool.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM incident_updates), (SELECT count(*) FROM outbox_events)`).
			Scan(&notes, &outbox); err != nil {
			t.Fatalf("count: %v", err)
		}
		return notes, outbox
	}

	// No incident at all.
	n0, o0 := countAll()
	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("raised with no incident")
	}
	if n1, o1 := countAll(); n1 != n0 || o1 != o0 {
		t.Errorf("no incident: notes %d→%d, outbox %d→%d", n0, n1, o0, o1)
	}

	// A resolved one.
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	if _, err := st.pool.Exec(ctx,
		`UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	before := readIncidentState(t, st, ctx, inc.ID)
	if raiseOnce(t, st, ctx, f.serviceID, domain.ServiceAlertDown) {
		t.Fatal("a resolved incident rose")
	}
	if after := readIncidentState(t, st, ctx, inc.ID); after != before {
		t.Errorf("wrote something:\n before %+v\n  after %+v", before, after)
	}
}

// I12, §7 clock case (a) — the rise waits behind a manual writer that COMMITS a status change, then
// stamps the incident, its note and its outbox row with ONE instant read after that wait, and the
// note carries the status the writer left. Shape of TestAServiceIncidentResolvesAfterTheWaitItActuallyDid.
//
// Why this would fail with the clock read before the lock: step 0 and the caller's transaction
// both run before the wait, so any instant taken there — `now()`, or `statement_timestamp()` of a
// statement issued before `FOR UPDATE` — is earlier than the floor read while the step is blocked.
func TestARiseStampsOneInstantReadAfterTheLock(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, m)

	// A BLOCKER holds the incident row and changes its status, the way an acknowledge does.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	var held string
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM incidents WHERE service_id = $1 FOR UPDATE`, f.serviceID).Scan(&held); err != nil {
		t.Fatalf("hold incident: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`UPDATE incidents SET status = 'identified', updated_at = statement_timestamp() WHERE id = $1`,
		held); err != nil {
		t.Fatalf("blocker status change: %v", err)
	}

	// The raiser: READ COMMITTED, its transaction clock materialised before the wait.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin raiser: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var txStart time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txStart); err != nil {
		t.Fatalf("read transaction clock: %v", err)
	}
	var raiserPID, blockerPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&raiserPID); err != nil {
		t.Fatalf("raiser pid: %v", err)
	}
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}

	type result struct {
		raised bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		raised, rerr := raiseServiceIncidentImpactTx(ctx, tx, f.serviceID, domain.ServiceAlertDown)
		done <- result{raised, rerr}
	}()

	// Genuinely BLOCKED on the blocker — a sleep would prove nothing about a lock.
	waitUntilBlockedBy(t, st, ctx, raiserPID, blockerPID)

	// The floor, on a third connection: every instant from here on is after the wait.
	var floor time.Time
	if err := st.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&floor); err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("raise: %v", r.err)
	}
	if !r.raised {
		t.Fatal("the step did not raise")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit raiser: %v", err)
	}
	if !txStart.Before(floor) {
		t.Fatalf("transaction clock %s is not before the floor %s: the test cannot tell a pre-lock clock",
			txStart.UTC(), floor.UTC())
	}

	var impact domain.IncidentImpact
	var status, noteStatus domain.IncidentStatus
	var updatedAt, noteAt, outboxCreated, outboxUpdated, outboxDue time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT i.impact, i.status, i.updated_at, u.status, u.created_at,
		       o.created_at, o.updated_at, o.next_attempt_at
		  FROM incidents i
		  JOIN incident_updates u ON u.incident_id = i.id AND u.body LIKE $2 || '%'
		  JOIN outbox_events o ON o.topic = 'incident_event'
		                      AND o.payload -> 'incident' ->> 'id' = i.id::text
		                      AND o.payload ->> 'event' = 'incident.updated'
		 WHERE i.id = $1`, inc.ID, domain.ImpactRaisedMarker).Scan(
		&impact, &status, &updatedAt, &noteStatus, &noteAt,
		&outboxCreated, &outboxUpdated, &outboxDue); err != nil {
		t.Fatalf("read: %v", err)
	}
	stamps := map[string]time.Time{
		"incidents.updated_at": updatedAt, "the note's created_at": noteAt,
		"outbox created_at": outboxCreated, "outbox updated_at": outboxUpdated,
		"outbox next_attempt_at": outboxDue,
	}
	for name, got := range stamps {
		if got.UTC().Before(floor.UTC()) {
			t.Errorf("%s = %s is BEFORE the lock was released (%s): the rise stamped an instant from "+
				"before the wait it actually did", name, got.UTC(), floor.UTC())
		}
		if !got.Equal(updatedAt) {
			t.Errorf("%s = %s differs from incidents.updated_at %s: a rise is ONE instant",
				name, got.UTC(), updatedAt.UTC())
		}
	}
	if noteStatus != domain.IncidentIdentified {
		t.Errorf("note status %q, want identified (the status the writer left)", noteStatus)
	}
	if status != domain.IncidentIdentified {
		t.Errorf("incident status %q, want identified: a rise does not move the lifecycle", status)
	}
	if impact != domain.ImpactCritical {
		t.Errorf("impact %q, want critical", impact)
	}
}

// §4.3 step 1 — the ranks are compared AGAIN on what the lock returns. Step 0 reads without a lock,
// so a writer can raise the row between the look and the lock; under READ COMMITTED the locking read
// returns that writer's committed version, and the step must then stop. Without the re-compare it
// would write `major` over `critical` — a fall, with its own note and event.
func TestARiseComparesTheRanksAgainUnderTheLock(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	inc := openAt(t, st, ctx, f, domain.ServiceAlertDegraded, domain.DefaultImpactMapping())
	before := readIncidentState(t, st, ctx, inc.ID)

	// The BLOCKER holds the row and raises it past what the step is about to write.
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM incidents WHERE id = $1 FOR UPDATE`, inc.ID); err != nil {
		t.Fatalf("hold incident: %v", err)
	}
	if _, err := blocker.Exec(ctx, `UPDATE incidents SET impact = 'critical' WHERE id = $1`, inc.ID); err != nil {
		t.Fatalf("blocker raise: %v", err)
	}

	tx, err := st.pool.Begin(ctx) // READ COMMITTED
	if err != nil {
		t.Fatalf("begin raiser: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raiserPID, blockerPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&raiserPID); err != nil {
		t.Fatalf("raiser pid: %v", err)
	}
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}

	type result struct {
		raised bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		// Step 0 sees minor < major (the blocker's change is uncommitted) and goes for the lock.
		raised, rerr := raiseServiceIncidentImpactTx(ctx, tx, f.serviceID, domain.ServiceAlertDown)
		done <- result{raised, rerr}
	}()
	waitUntilBlockedBy(t, st, ctx, raiserPID, blockerPID)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("raise: %v", r.err)
	}
	if r.raised {
		t.Fatal("the step raised over a row the lock returned already at critical")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit raiser: %v", err)
	}

	after := readIncidentState(t, st, ctx, inc.ID)
	if after.impact != domain.ImpactCritical {
		t.Errorf("impact %q, want critical (the writer's, not lowered)", after.impact)
	}
	if after.notes != before.notes {
		t.Errorf("notes %d → %d: the step wrote a note", before.notes, after.notes)
	}
	if after.outbox != before.outbox {
		t.Errorf("outbox rows %d → %d: the step wrote an event", before.outbox, after.outbox)
	}
	if !after.updatedAt.Equal(before.updatedAt) {
		t.Errorf("updated_at moved %s → %s: the step stamped the incident", before.updatedAt.UTC(), after.updatedAt.UTC())
	}
}
