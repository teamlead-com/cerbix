package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 store core (func-change-intelligence §7 — *Phases*, *Bounds and shape*, *Timeline*,
// *Correlation*, *Retention*; iter-0165 changeset 2). Every test here reaches the store-level
// scenario it is named after; the API and CLI rows of §7 belong to their own changesets.

// changeActor is the CI token every write here uses unless a test says otherwise (D5).
var changeActor = GateActor{ViaToken: true, Label: "token:ci"}

// changeFixture is an adopted service (with a materialization row and an epoch) plus a second
// service of the same project for the upstream cases, and a service of ANOTHER project for the
// tenancy cases.
type changeFixture struct {
	reportFixture
	upstreamID     string
	otherProjectID string
	otherServiceID string
}

func changeService(t *testing.T, st *Store, ctx context.Context) changeFixture {
	t.Helper()
	f := changeFixture{reportFixture: reportService(t, st, ctx)}
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1, 'upstream-db', 'Upstream DB') RETURNING id`,
		f.projectID).Scan(&f.upstreamID); err != nil {
		t.Fatalf("upstream service: %v", err)
	}
	// A fixture may be built more than once per test: the foreign org's slug must be unique.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	org, err := st.CreateOrganization(ctx, "other-org-"+suffix, "Other")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "other-"+suffix, "Other")
	if err != nil {
		t.Fatal(err)
	}
	f.otherProjectID = proj.ID
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name) VALUES ($1, 'foreign', 'Foreign') RETURNING id`,
		proj.ID).Scan(&f.otherServiceID); err != nil {
		t.Fatalf("foreign service: %v", err)
	}
	return f
}

// changeInput is a valid deploy phase for the fixture service with the §5a default bounds; a
// test overrides what it is about.
func changeInput(f changeFixture, ext string, phase domain.ChangePhase, at time.Time) RecordChangeInput {
	return RecordChangeInput{
		ProjectID: f.projectID, ServiceID: f.serviceID,
		Source: "github-actions", ExternalID: ext,
		Kind: domain.ChangeKindDeploy, Phase: phase, OccurredAt: at,
		Ref: "v4.2.1", URL: "https://ci.example.com/runs/" + ext,
		Actor: changeActor, MaxPast: 24 * time.Hour, MaxFuture: 5 * time.Minute,
	}
}

func mustRecord(t *testing.T, st *Store, ctx context.Context, in RecordChangeInput) domain.ChangePhaseRow {
	t.Helper()
	row, replayed, err := st.RecordChangePhase(ctx, in)
	if err != nil {
		t.Fatalf("record %s/%s %s: %v", in.Source, in.ExternalID, in.Phase, err)
	}
	if replayed {
		t.Fatalf("record %s/%s %s: unexpectedly a replay", in.Source, in.ExternalID, in.Phase)
	}
	return row
}

func recordCode(t *testing.T, st *Store, ctx context.Context, in RecordChangeInput) *domain.ChangeError {
	t.Helper()
	_, _, err := st.RecordChangePhase(ctx, in)
	var ce *domain.ChangeError
	if !errors.As(err, &ce) {
		t.Fatalf("record %s/%s %s: got %v, want a *domain.ChangeError", in.Source, in.ExternalID, in.Phase, err)
	}
	return ce
}

// plantChangeRow writes a phase row DIRECTLY — the way retention and long-range timeline tests
// must, since the write path refuses an occurred_at older than max_past (168 h at most).
func plantChangeRow(t *testing.T, st *Store, ctx context.Context, projectID, serviceID, source, ext string, kind domain.ChangeKind, phase domain.ChangePhase, at time.Time) string {
	t.Helper()
	id, err := newChangeID(at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, ref, url,
		                             occurred_at, actor_label, via_token, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'v1', '', $8, 'token:ci', true, $8)`,
		id, projectID, serviceID, source, ext, string(kind), string(phase), at.UTC()); err != nil {
		t.Fatalf("plant %s/%s %s: %v", source, ext, phase, err)
	}
	return id
}

func countSQL(t *testing.T, st *Store, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// openServiceIncidentAt opens the fixture service's auto-incident through the owner and pins its
// started_at to `at`, so lags are exact.
func openServiceIncidentAt(t *testing.T, st *Store, ctx context.Context, f changeFixture, at time.Time) string {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inc, _, err := st.OpenServiceIncidentTx(ctx, tx, f.serviceID, f.projectID, "checkout is down", 2, &f.revisionID)
	if err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE incidents SET started_at = $2 WHERE id = $1`, inc.ID, at.UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return inc.ID
}

func changeNotes(t *testing.T, st *Store, ctx context.Context, incidentID string) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx,
		`SELECT body FROM incident_updates WHERE incident_id = $1 AND author = 'system' AND body LIKE $2 ORDER BY created_at`,
		incidentID, domain.ChangesMarker+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func listAllGroups(t *testing.T, st *Store, ctx context.Context, f changeFixture, from, to time.Time, kinds []domain.ChangeKind, source *string, limit int) [][]domain.ChangeGroup {
	t.Helper()
	var pages [][]domain.ChangeGroup
	var cursor *ChangeCursor
	for i := 0; i < 50; i++ {
		groups, next, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, kinds, source, cursor, limit)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		pages = append(pages, groups)
		if next == nil {
			return pages
		}
		// The cursor survives its opaque encoding.
		decoded, err := DecodeChangeCursor(next.Encode())
		if err != nil || !decoded.LatestOccurredAt.Equal(next.LatestOccurredAt) || decoded.Source != next.Source || decoded.ExternalID != next.ExternalID {
			t.Fatalf("cursor does not round-trip: %+v → %+v (%v)", next, decoded, err)
		}
		cursor = &decoded
	}
	t.Fatal("the listing never ended")
	return nil
}

func groupIDs(pages [][]domain.ChangeGroup) []string {
	var out []string
	for _, p := range pages {
		for _, g := range p {
			out = append(out, g.Source+"/"+g.ExternalID)
		}
	}
	return out
}

// ── Phases (D3, D4, D5; invariants 3, 4, 5) ──────────────────────────────────────────────

// started then succeeded → two rows, one group, phases ordered, the group's latest is the
// terminal's instant; a terminal alone → one row, a group without a start.
func TestChangeStartedThenSucceededIsOneGroupAndATerminalAloneIsAccepted(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	started := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	done := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-8*time.Minute)))
	if started.ID == done.ID || started.ActorLabel != "token:ci" || !started.ViaToken || started.ActorUserID != nil {
		t.Fatalf("rows: started=%+v done=%+v", started, done)
	}
	alone := mustRecord(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseFailed, now.Add(-5*time.Minute)))
	if alone.Phase != domain.ChangePhaseFailed {
		t.Fatalf("alone = %+v", alone)
	}

	pages := listAllGroups(t, st, ctx, f, now.Add(-time.Hour), now.Add(time.Hour), nil, nil, 50)
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("groups = %v, want two", groupIDs(pages))
	}
	g2, g1 := pages[0][0], pages[0][1] // newest first: run-2 (−5m) then run-1 (−8m)
	if g2.ExternalID != "run-2" || len(g2.Phases) != 1 || g2.Phases[0].Phase != domain.ChangePhaseFailed {
		t.Fatalf("terminal-alone group = %+v", g2)
	}
	if g1.ExternalID != "run-1" || len(g1.Phases) != 2 || g1.Phases[0].Phase != domain.ChangePhaseStarted ||
		g1.Phases[1].Phase != domain.ChangePhaseSucceeded || !g1.LatestOccurredAt.Equal(done.OccurredAt) || g1.Kind != domain.ChangeKindDeploy {
		t.Fatalf("started+succeeded group = %+v", g1)
	}
	// The id is bound to the database instant it was recorded at (§5), like the gate's.
	if ms, ok := gateDecisionIDMillis(started.ID); !ok || ms != started.RecordedAt.UnixMilli() {
		t.Fatalf("id %s does not carry recorded_at's millisecond %d", started.ID, started.RecordedAt.UnixMilli())
	}
}

// The order table at the store: a second terminal is phase_order naming the first; started after
// a terminal is phase_order; a terminal predating started is occurred_at_before_start.
func TestChangePhaseOrderIsRefusedByNameAtTheStore(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-8*time.Minute)))
	ce := recordCode(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseFailed, now.Add(-7*time.Minute)))
	if ce.Code != domain.ChangeErrPhaseOrder || !strings.Contains(ce.Msg, "succeeded already recorded") {
		t.Fatalf("second terminal: %v", ce)
	}

	mustRecord(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseFailed, now.Add(-6*time.Minute)))
	ce = recordCode(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseStarted, now.Add(-7*time.Minute)))
	if ce.Code != domain.ChangeErrPhaseOrder || !strings.Contains(ce.Msg, "failed") {
		t.Fatalf("started after terminal: %v", ce)
	}

	mustRecord(t, st, ctx, changeInput(f, "run-3", domain.ChangePhaseStarted, now.Add(-5*time.Minute)))
	ce = recordCode(t, st, ctx, changeInput(f, "run-3", domain.ChangePhaseSucceeded, now.Add(-6*time.Minute)))
	if ce.Code != domain.ChangeErrOccurredBeforeStart || ce.Field != "occurred_at" {
		t.Fatalf("terminal before started: %v", ce)
	}
	// Nothing of the refused writes landed: run-1 has 2 rows, run-2 and run-3 one each.
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1`, f.serviceID); n != 4 {
		t.Fatalf("rows = %d, want 4", n)
	}
}

// An identical replay returns the ORIGINAL row — same id, same actor, same recorded_at — writes
// nothing and emits no audit row, whoever replays it; a differing replay is phase_exists naming
// the first differing field; a phase whose kind differs from the group is kind_mismatch.
func TestChangeIdenticalReplayReturnsTheOriginalAndDifferingReplayNamesTheField(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	in := changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-10*time.Minute))
	in.OccurredAt = in.OccurredAt.Add(123456 * time.Nanosecond) // sub-µs noise is dropped at the µs
	original := mustRecord(t, st, ctx, in)
	auditBefore := countSQL(t, st, ctx, `SELECT count(*) FROM audit_logs`)

	// Same body, another token, a moment later.
	replay := in
	replay.Actor = GateActor{ViaToken: true, Label: "token:ci-rotated"}
	row, replayed, err := st.RecordChangePhase(ctx, replay)
	if err != nil || !replayed {
		t.Fatalf("identical replay: replayed=%v err=%v", replayed, err)
	}
	if row.ID != original.ID || row.ActorLabel != "token:ci" || !row.RecordedAt.Equal(original.RecordedAt) {
		t.Fatalf("replay returned %+v, want the original %+v", row, original)
	}
	// A person replaying it is the same answer.
	replay.Actor = GateActor{ActorUserID: "", ViaToken: false, Label: "ops@example.com"}
	if row, replayed, err = st.RecordChangePhase(ctx, replay); err != nil || !replayed || row.ID != original.ID || row.ViaToken != true {
		t.Fatalf("human replay: %+v %v %v", row, replayed, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1`, f.serviceID); n != 1 {
		t.Fatalf("rows = %d after two replays, want 1", n)
	}
	if countSQL(t, st, ctx, `SELECT count(*) FROM audit_logs`) != auditBefore {
		t.Fatal("a replay wrote an audit row; recording is not an audit event (D5)")
	}

	// Differing replays, each naming the FIRST differing field in the documented order.
	for _, tc := range []struct {
		field string
		mut   func(*RecordChangeInput)
	}{
		{"kind", func(i *RecordChangeInput) { i.Kind = domain.ChangeKindRollback }},
		{"occurred_at", func(i *RecordChangeInput) { i.OccurredAt = i.OccurredAt.Add(time.Microsecond) }},
		{"ref", func(i *RecordChangeInput) { i.Ref = "v4.2.2" }},
		{"url", func(i *RecordChangeInput) { i.URL = "https://ci.example.com/runs/other" }},
		{"decision_id", func(i *RecordChangeInput) { d := "0199a0c0-0000-7000-8000-000000000000"; i.DecisionID = &d }},
	} {
		d := in
		tc.mut(&d)
		ce := recordCode(t, st, ctx, d)
		if ce.Code != domain.ChangeErrPhaseExists || ce.Field != tc.field {
			t.Errorf("differing %s: code=%s field=%s (%v)", tc.field, ce.Code, ce.Field, ce)
		}
	}
	// A ref differing only by a sub-µs occurred_at is still identical (µs is the resolution).
	same := in
	same.OccurredAt = in.OccurredAt.Add(400 * time.Nanosecond)
	if _, replayed, err := st.RecordChangePhase(ctx, same); err != nil || !replayed {
		t.Fatalf("sub-µs noise must not break identity: %v %v", replayed, err)
	}

	// kind_mismatch: a rollback phase cannot join a deploy group.
	mustRecord(t, st, ctx, changeInput(f, "run-2", domain.ChangePhaseStarted, now.Add(-4*time.Minute)))
	mis := changeInput(f, "run-2", domain.ChangePhaseSucceeded, now.Add(-3*time.Minute))
	mis.Kind = domain.ChangeKindRollback
	if ce := recordCode(t, st, ctx, mis); ce.Code != domain.ChangeErrKindMismatch {
		t.Fatalf("kind mismatch: %v", ce)
	}
}

// D4: two writers for one identity are serialized by the advisory lock — asserted with a PLANTED
// lock holder (the write waits for it) — and a race between succeeded and failed leaves exactly
// one terminal row.
func TestChangeConcurrentTerminalsAreSerializedByTheIdentityLock(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	// The planted holder: a transaction holding the identity's lock key. The write must wait.
	mustRecord(t, st, ctx, changeInput(f, "held", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Released on every path: a leaked holder would make pool.Close wait forever at cleanup.
	defer holder.Rollback(ctx) //nolint:errcheck // no-op after the explicit rollback below
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.serviceID+"/github-actions/held"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.RecordChangePhase(ctx, changeInput(f, "held", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute)))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the write did not wait for the lock holder (err=%v): the advisory lock is not the mechanism", err)
	case <-time.After(500 * time.Millisecond):
	}
	// An UNRELATED identity does not queue behind the holder.
	unrelated := make(chan error, 1)
	go func() {
		_, _, err := st.RecordChangePhase(ctx, changeInput(f, "other", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute)))
		unrelated <- err
	}()
	select {
	case err := <-unrelated:
		if err != nil {
			t.Fatalf("unrelated identity: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unrelated identity queued behind another identity's lock")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after the holder released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write never completed after the holder released")
	}

	// The race, five times: exactly one of succeeded/failed lands, the other is phase_order.
	for i := 0; i < 5; i++ {
		ext := fmt.Sprintf("race-%d", i)
		mustRecord(t, st, ctx, changeInput(f, ext, domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
		var wg sync.WaitGroup
		results := make([]error, 2)
		for j, phase := range []domain.ChangePhase{domain.ChangePhaseSucceeded, domain.ChangePhaseFailed} {
			wg.Add(1)
			go func(j int, phase domain.ChangePhase) {
				defer wg.Done()
				_, _, results[j] = st.RecordChangePhase(ctx, changeInput(f, ext, phase, now.Add(-9*time.Minute)))
			}(j, phase)
		}
		wg.Wait()
		ok, refused := 0, 0
		for _, err := range results {
			var ce *domain.ChangeError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ce) && ce.Code == domain.ChangeErrPhaseOrder:
				refused++
			default:
				t.Fatalf("race %s: unexpected %v", ext, err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("race %s: %d accepted, %d phase_order; want 1 and 1", ext, ok, refused)
		}
		if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1 AND external_id = $2`, f.serviceID, ext); n != 2 {
			t.Fatalf("race %s: %d rows, want started + one terminal", ext, n)
		}
	}
}

// D2 at the store: external_id is case-sensitive (two groups); non-canonical text — a leading
// space, a U+200B, a newline, a decomposed é — is refused BEFORE SQL by the domain validator and
// nothing is written; composed é is accepted and replays as identical; the DB CHECK refuses an
// ASCII control and accepts U+200B (the CHECK claims no more).
func TestChangeStoreRefusesNonCanonicalTextBeforeSQLAndTheCheckEnforcesOnlyASCII(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	mustRecord(t, st, ctx, changeInput(f, "Run-42", domain.ChangePhaseSucceeded, now.Add(-10*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "run-42", domain.ChangePhaseSucceeded, now.Add(-9*time.Minute)))
	if pages := listAllGroups(t, st, ctx, f, now.Add(-time.Hour), now.Add(time.Hour), nil, nil, 50); len(pages[0]) != 2 {
		t.Fatalf("Run-42 and run-42 are two identities; got %v", groupIDs(pages))
	}

	before := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`)
	for _, tc := range []struct {
		name  string
		mut   func(*RecordChangeInput)
		code  string
		field string
	}{
		{"leading space in ref", func(i *RecordChangeInput) { i.Ref = " v1" }, domain.ChangeErrRefInvalid, "ref"},
		{"U+200B in ref", func(i *RecordChangeInput) { i.Ref = "v1\u200b" }, domain.ChangeErrRefInvalid, "ref"},
		{"newline in ref", func(i *RecordChangeInput) { i.Ref = "v1\n" }, domain.ChangeErrRefInvalid, "ref"},
		{"decomposed é in ref", func(i *RecordChangeInput) { i.Ref = "cafe\u0301" }, domain.ChangeErrRefInvalid, "ref"},
		{"trailing space in external_id", func(i *RecordChangeInput) { i.ExternalID = "run-x " }, domain.ChangeErrExternalIDInvalid, "external_id"},
		{"U+2028 in url", func(i *RecordChangeInput) { i.URL = "https://x.example/\u2028" }, domain.ChangeErrURLInvalid, "url"},
	} {
		in := changeInput(f, "canon", domain.ChangePhaseSucceeded, now.Add(-8*time.Minute))
		tc.mut(&in)
		ce := recordCode(t, st, ctx, in)
		if ce.Code != tc.code || ce.Field != tc.field {
			t.Errorf("%s: %v", tc.name, ce)
		}
	}
	if countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`) != before {
		t.Fatal("a refused value reached the table")
	}
	// Composed é is canonical and replays as identical to itself.
	in := changeInput(f, "canon", domain.ChangePhaseSucceeded, now.Add(-8*time.Minute))
	in.Ref = "café"
	mustRecord(t, st, ctx, in)
	if _, replayed, err := st.RecordChangePhase(ctx, in); err != nil || !replayed {
		t.Fatalf("composed é replay: %v %v", replayed, err)
	}

	// The DB CHECK: `\x01` refused (23514), U+200B accepted — which is why the domain, not the
	// schema, is the Unicode authority.
	id1, _ := newChangeID(now)
	_, err := st.pool.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, ref, occurred_at, actor_label, via_token)
		VALUES ($1, $2, $3, 'direct', 'ctl', 'deploy', 'succeeded', E'v1\x01', $4, 'sql', true)`, id1, f.projectID, f.serviceID, now)
	if pgErrCode(err) != "23514" {
		t.Fatalf("direct SQL with \\x01 in ref: got %v, want the CHECK (23514)", err)
	}
	id2, _ := newChangeID(now)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, ref, occurred_at, actor_label, via_token)
		VALUES ($1, $2, $3, 'direct', 'zw', 'deploy', 'succeeded', 'v1'||U&'\200B', $4, 'sql', true)`, id2, f.projectID, f.serviceID, now); err != nil {
		t.Fatalf("direct SQL with U+200B must pass the CHECK (it claims only ASCII controls): %v", err)
	}
	// And the other CHECKs of §5 exist for the classes they name.
	for _, bad := range []struct{ col, val string }{
		{"source", "'Deploy_Bot'"}, {"external_id", "''"}, {"external_id", "repeat('x', 129)"},
		{"kind", "'config'"}, {"phase", "'done'"}, {"url", "'http://x'"}, {"ref", "repeat('x', 129)"},
	} {
		id, _ := newChangeID(now)
		cols := map[string]string{"source": "'direct'", "external_id": "'chk'", "kind": "'deploy'", "phase": "'started'", "ref": "''", "url": "''"}
		cols[bad.col] = bad.val
		_, err := st.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, ref, url, occurred_at, actor_label, via_token)
			VALUES ($1, $2, $3, %s, %s, %s, %s, %s, %s, $4, 'sql', true)`,
			cols["source"], cols["external_id"], cols["kind"], cols["phase"], cols["ref"], cols["url"]), id, f.projectID, f.serviceID, now)
		if pgErrCode(err) != "23514" {
			t.Errorf("CHECK on %s = %s: got %v, want 23514", bad.col, bad.val, err)
		}
	}
}

// ── Bounds and shape (D2, D11; invariants 2, 15) ─────────────────────────────────────────

func TestChangeBoundsSourceLengthURLClockAndDecision(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	in := changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	in.Source = "Deploy_Bot"
	if ce := recordCode(t, st, ctx, in); ce.Code != domain.ChangeErrSourceInvalid {
		t.Fatalf("source: %v", ce)
	}
	in = changeInput(f, strings.Repeat("x", 129), domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	if ce := recordCode(t, st, ctx, in); ce.Code != domain.ChangeErrExternalIDInvalid {
		t.Fatalf("129 chars: %v", ce)
	}
	in = changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	in.URL = "http://ci.example.com/runs/1"
	if ce := recordCode(t, st, ctx, in); ce.Code != domain.ChangeErrURLInvalid {
		t.Fatalf("http url: %v", ce)
	}
	in = changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	in.Kind, in.Phase = "config", domain.ChangePhaseSucceeded
	if ce := recordCode(t, st, ctx, in); ce.Code != domain.ChangeErrKindInvalid {
		t.Fatalf("kind: %v", ce)
	}

	// The clock bounds against the DATABASE clock: 25 h ago refused, 4 min ahead accepted,
	// 6 min ahead refused (max_past 24h, max_future 5m).
	if ce := recordCode(t, st, ctx, changeInput(f, "old", domain.ChangePhaseSucceeded, now.Add(-25*time.Hour))); ce.Code != domain.ChangeErrOccurredOutOfBounds {
		t.Fatalf("25h ago: %v", ce)
	}
	mustRecord(t, st, ctx, changeInput(f, "soon", domain.ChangePhaseSucceeded, now.Add(4*time.Minute)))
	if ce := recordCode(t, st, ctx, changeInput(f, "later", domain.ChangePhaseSucceeded, now.Add(6*time.Minute))); ce.Code != domain.ChangeErrOccurredOutOfBounds {
		t.Fatalf("6m ahead: %v", ce)
	}
	// The bounds are the caller's: a 1h max_past refuses 2h ago.
	tight := changeInput(f, "tight", domain.ChangePhaseSucceeded, now.Add(-2*time.Hour))
	tight.MaxPast = time.Hour
	if ce := recordCode(t, st, ctx, tight); ce.Code != domain.ChangeErrOccurredOutOfBounds {
		t.Fatalf("tight max_past: %v", ce)
	}

	// A foreign service is ErrNotFound, and nothing else is learned.
	foreign := changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	foreign.ServiceID = f.otherServiceID
	if _, _, err := st.RecordChangePhase(ctx, foreign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign service: %v", err)
	}

	// decision_id (D11): a ledger row of THIS service in THIS project — anything else is
	// decision_unknown; the timeline reads it back, and says aged out once the row is gone.
	gf := gateFixture{reportFixture: f.reportFixture}
	decision := gateInsertRow(t, st, ctx, gf, now.Add(-time.Minute), domain.GateStateBlock, nil)
	other := changeInput(f, "dec", domain.ChangePhaseSucceeded, now.Add(-30*time.Second))
	other.ServiceID = f.upstreamID
	other.DecisionID = &decision
	if ce := recordCode(t, st, ctx, other); ce.Code != domain.ChangeErrDecisionUnknown {
		t.Fatalf("another service's decision: %v", ce)
	}
	random := "0199a0c0-0000-7000-8000-000000000000"
	withRandom := changeInput(f, "dec", domain.ChangePhaseSucceeded, now.Add(-30*time.Second))
	withRandom.DecisionID = &random
	if ce := recordCode(t, st, ctx, withRandom); ce.Code != domain.ChangeErrDecisionUnknown {
		t.Fatalf("unknown decision: %v", ce)
	}
	malformed := "not-a-uuid"
	withMalformed := changeInput(f, "dec", domain.ChangePhaseSucceeded, now.Add(-30*time.Second))
	withMalformed.DecisionID = &malformed
	if ce := recordCode(t, st, ctx, withMalformed); ce.Code != domain.ChangeErrDecisionUnknown {
		t.Fatalf("malformed decision: %v", ce)
	}
	good := changeInput(f, "dec", domain.ChangePhaseSucceeded, now.Add(-30*time.Second))
	upper := strings.ToUpper(decision)
	good.DecisionID = &upper // case-insensitive uuid text, stored canonical
	row := mustRecord(t, st, ctx, good)
	if row.DecisionID == nil || *row.DecisionID != decision {
		t.Fatalf("decision stored as %v, want %s", row.DecisionID, decision)
	}
	pages := listAllGroups(t, st, ctx, f, now.Add(-time.Hour), now.Add(time.Hour), nil, nil, 50)
	var dec *domain.ChangeDecisionLink
	for _, g := range pages[0] {
		if g.ExternalID == "dec" {
			dec = g.Decision
		}
	}
	if dec == nil || dec.AgedOut || dec.State == nil || *dec.State != domain.GateStateBlock || dec.Action == nil || *dec.Action != domain.GateActionBlock {
		t.Fatalf("decision link = %+v, want BLOCK/BLOCK live", dec)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM service_gate_decisions WHERE id = $1`, decision); err != nil {
		t.Fatal(err)
	}
	pages = listAllGroups(t, st, ctx, f, now.Add(-time.Hour), now.Add(time.Hour), nil, nil, 50)
	for _, g := range pages[0] {
		if g.ExternalID == "dec" {
			dec = g.Decision
		}
	}
	if dec == nil || !dec.AgedOut || dec.ID != decision || dec.State != nil {
		t.Fatalf("aged-out decision link = %+v", dec)
	}
}

// ── Timeline (D6; invariant 6) ───────────────────────────────────────────────────────────

func TestChangeTimelineRangeLimitAndFilterBounds(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	code := func(err error) string {
		var ce *domain.ChangeError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return fmt.Sprint(err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-93*24*time.Hour), now, nil, nil, nil, 50); code(err) != domain.ChangeErrRangeTooWide {
		t.Fatalf("93 days: %v", err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-92*24*time.Hour), now, nil, nil, nil, 50); err != nil {
		t.Fatalf("92 days refused: %v", err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now, now, nil, nil, nil, 50); code(err) != domain.ChangeErrRangeInvalid {
		t.Fatalf("to == from: %v", err)
	}
	for _, l := range []int{0, 201} {
		if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-time.Hour), now, nil, nil, nil, l); code(err) != domain.ChangeErrLimitInvalid {
			t.Fatalf("limit %d: %v", l, err)
		}
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-time.Hour), now, []domain.ChangeKind{"config"}, nil, nil, 50); code(err) != domain.ChangeErrKindInvalid {
		t.Fatalf("bad kind: %v", err)
	}
	bad := "Deploy_Bot"
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-time.Hour), now, nil, &bad, nil, 50); code(err) != domain.ChangeErrSourceInvalid {
		t.Fatalf("bad source: %v", err)
	}
	if _, err := DecodeChangeCursor("not base64!"); code(err) != domain.ChangeErrCursorInvalid {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := DecodeChangeCursor(ChangeCursor{LatestOccurredAt: now, Source: "Bad_Source", ExternalID: "x"}.Encode()); code(err) != domain.ChangeErrCursorInvalid {
		t.Fatal("a cursor with a non-slug source must be refused")
	}
	// An external_id containing ':' survives the cursor.
	c := ChangeCursor{LatestOccurredAt: now.Truncate(time.Microsecond), Source: "ci", ExternalID: "job:42:b"}
	if d, err := DecodeChangeCursor(c.Encode()); err != nil || d != c {
		t.Fatalf("cursor round-trip: %+v %v", d, err)
	}
}

// Groups newest first by their LATEST phase; the cursor continues without duplicates across
// pages of 2; kind is a set (OR) and source one slug, applied before the limit; a group whose
// started precedes `from` but whose terminal is inside is returned with BOTH phases; a group
// whose latest is outside is absent.
func TestChangeTimelineGroupsNewestFirstWithCursorAndFilters(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	from, to := now.Add(-time.Hour), now.Add(time.Minute)

	// Five groups, latest instants strictly decreasing with the index; g0 started before `from`.
	mustRecord(t, st, ctx, changeInput(f, "g0", domain.ChangePhaseStarted, now.Add(-90*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "g0", domain.ChangePhaseSucceeded, now.Add(-2*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "g1", domain.ChangePhaseSucceeded, now.Add(-10*time.Minute)))
	flag := changeInput(f, "g2", domain.ChangePhaseSucceeded, now.Add(-20*time.Minute))
	flag.Kind, flag.Source = domain.ChangeKindFlag, "launchdarkly"
	mustRecord(t, st, ctx, flag)
	rollback := changeInput(f, "g3", domain.ChangePhaseSucceeded, now.Add(-30*time.Minute))
	rollback.Kind = domain.ChangeKindRollback
	mustRecord(t, st, ctx, rollback)
	mustRecord(t, st, ctx, changeInput(f, "g4", domain.ChangePhaseFailed, now.Add(-40*time.Minute)))
	// Outside the range: latest before from, and latest at/after to.
	mustRecord(t, st, ctx, changeInput(f, "old", domain.ChangePhaseSucceeded, now.Add(-2*time.Hour)))
	mustRecord(t, st, ctx, changeInput(f, "future", domain.ChangePhaseSucceeded, now.Add(2*time.Minute)))

	pages := listAllGroups(t, st, ctx, f, from, to, nil, nil, 2)
	if len(pages) != 3 || len(pages[0]) != 2 || len(pages[1]) != 2 || len(pages[2]) != 1 {
		t.Fatalf("pages = %v", groupIDs(pages))
	}
	want := []string{"github-actions/g0", "github-actions/g1", "launchdarkly/g2", "github-actions/g3", "github-actions/g4"}
	if got := groupIDs(pages); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	g0 := pages[0][0]
	if len(g0.Phases) != 2 || g0.Phases[0].Phase != domain.ChangePhaseStarted || !g0.Phases[0].OccurredAt.Before(from) {
		t.Fatalf("g0 must carry its started (before from) and its terminal: %+v", g0.Phases)
	}

	// kind OR, before the limit: deploy|flag with limit 2 → g0,g1 then g2,g4 — g3 (rollback) never.
	pages = listAllGroups(t, st, ctx, f, from, to, []domain.ChangeKind{domain.ChangeKindDeploy, domain.ChangeKindFlag}, nil, 2)
	if got := groupIDs(pages); strings.Join(got, ",") != "github-actions/g0,github-actions/g1,launchdarkly/g2,github-actions/g4" || len(pages[0]) != 2 {
		t.Fatalf("kind OR = %v", got)
	}
	src := "launchdarkly"
	pages = listAllGroups(t, st, ctx, f, from, to, nil, &src, 50)
	if got := groupIDs(pages); strings.Join(got, ",") != "launchdarkly/g2" {
		t.Fatalf("source filter = %v", got)
	}
	// An empty page is an empty, non-nil slice.
	groups, next, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-10*24*time.Hour), now.Add(-9*24*time.Hour), nil, nil, nil, 50)
	if err != nil || groups == nil || len(groups) != 0 || next != nil {
		t.Fatalf("empty range: %v %v %v", groups, next, err)
	}
}

// Two groups with the same latest_occurred_at: the identity is the tiebreaker and the cursor
// continues past both; a phase recorded mid-traversal moves its group ABOVE the cursor, so it is
// absent from this traversal and no page holds a duplicate.
func TestChangeTimelineTiebreakAndMidTraversalMove(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	from, to := now.Add(-time.Hour), now.Add(time.Minute)
	same := now.Add(-10 * time.Minute)

	b := changeInput(f, "b", domain.ChangePhaseSucceeded, same)
	mustRecord(t, st, ctx, b)
	a := changeInput(f, "a", domain.ChangePhaseSucceeded, same)
	mustRecord(t, st, ctx, a)
	z := changeInput(f, "z", domain.ChangePhaseSucceeded, same)
	z.Source = "argo"
	mustRecord(t, st, ctx, z)
	mustRecord(t, st, ctx, changeInput(f, "older", domain.ChangePhaseSucceeded, now.Add(-20*time.Minute)))

	pages := listAllGroups(t, st, ctx, f, from, to, nil, nil, 1)
	if got := groupIDs(pages); strings.Join(got, ",") != "argo/z,github-actions/a,github-actions/b,github-actions/older" || len(pages) != 4 {
		t.Fatalf("tiebreak order = %v (pages=%d)", got, len(pages))
	}

	// Mid-traversal: page 1 (limit 2) is z,a; then `older` gains a terminal newer than everything
	// — wait, it already has one; use `b`: it cannot gain a phase (terminal recorded). Take a
	// started-only group instead.
	mustRecord(t, st, ctx, changeInput(f, "moving", domain.ChangePhaseStarted, now.Add(-15*time.Minute)))
	first, cursor, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, nil, nil, nil, 2)
	if err != nil || len(first) != 2 || cursor == nil {
		t.Fatalf("page 1: %v %v %v", groupIDs([][]domain.ChangeGroup{first}), cursor, err)
	}
	// Order now: z,a (same, −10m) | b (−10m) | moving (−15m) | older (−20m). Page 1 = z, a.
	mustRecord(t, st, ctx, changeInput(f, "moving", domain.ChangePhaseSucceeded, now.Add(-time.Minute)))
	var rest []string
	for cursor != nil {
		var page []domain.ChangeGroup
		page, cursor, err = st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, nil, nil, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		rest = append(rest, groupIDs([][]domain.ChangeGroup{page})...)
	}
	all := append(groupIDs([][]domain.ChangeGroup{first}), rest...)
	seen := map[string]int{}
	for _, id := range all {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("group %s returned %d times", id, n)
		}
	}
	if seen["github-actions/moving"] != 0 {
		t.Fatalf("the moved group must be absent from this traversal: %v", all)
	}
	if strings.Join(rest, ",") != "github-actions/b,github-actions/older" {
		t.Fatalf("rest = %v", rest)
	}
	// A fresh traversal sees it first.
	fresh := listAllGroups(t, st, ctx, f, from, to, nil, nil, 50)
	if groupIDs(fresh)[0] != "github-actions/moving" {
		t.Fatalf("fresh order = %v", groupIDs(fresh))
	}
}

// A foreign project is ErrNotFound; deleting the service cascades its rows and links (D10) and
// the timeline is ErrNotFound; the incident's note remains as text.
func TestChangeTimelineForeignServiceIsNotFoundAndDeletionCascades(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	change := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-12*time.Minute)))
	inc := openServiceIncidentAt(t, st, ctx, f, change.OccurredAt.Add(12*time.Minute))
	if _, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.otherProjectID, f.serviceID, now.Add(-time.Hour), now, nil, nil, nil, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign project: %v", err)
	}
	if _, err := st.ListIncidentChanges(ctx, f.otherProjectID, inc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign project incident changes: %v", err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 1 {
		t.Fatalf("links before delete = %d", n)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-time.Hour), now, nil, nil, nil, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted service: %v", err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE service_id = $1`, f.serviceID); n != 0 {
		t.Fatalf("%d change rows survive the service", n)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 0 {
		t.Fatalf("%d links survive the service", n)
	}
	if notes := changeNotes(t, st, ctx, inc); len(notes) != 1 {
		t.Fatalf("the note must remain as text after the cascade: %v", notes)
	}
}

// ── Correlation (D7; invariants 7, 8, 9, 22) ─────────────────────────────────────────────

func TestChangeCorrelationLinksPrecedingChangesOnceWithTheNote(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)

	// The own change has BOTH phases before the open: the anchor must be the latest (succeeded,
	// −12m), never the started (−20m).
	ownStarted := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-20*time.Minute)))
	own := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-12*time.Minute)))
	opened := own.OccurredAt.Add(12 * time.Minute)
	// Upstream probable_root: a deploy 40 min before on the upstream service.
	up := changeInput(f, "up-7", domain.ChangePhaseSucceeded, opened.Add(-40*time.Minute))
	up.ServiceID, up.Source, up.Ref = f.upstreamID, "argo", "db-9"
	upRow := mustRecord(t, st, ctx, up)
	// 61 min before at a 60 min window: outside.
	edge := changeInput(f, "edge", domain.ChangePhaseSucceeded, opened.Add(-61*time.Minute))
	mustRecord(t, st, ctx, edge)
	// After the open: never linked.
	after := changeInput(f, "after", domain.ChangePhaseSucceeded, opened.Add(time.Minute))
	mustRecord(t, st, ctx, after)
	// A non-root impact (affected) on another service is NOT a candidate.
	inc := openServiceIncidentAt(t, st, ctx, f, opened)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO incident_service_impacts (incident_id, service_id, project_id, role, path)
		VALUES ($1, $2, $3, 'probable_root', ARRAY['upstream-db', 'checkout'])`, inc, f.upstreamID, f.projectID); err != nil {
		t.Fatal(err)
	}

	res, err := st.LinkPrecedingChanges(ctx, inc, 60*time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Links) != 2 || !res.NoteAdded || res.Skipped {
		t.Fatalf("result = %+v, want two links and the note", res)
	}
	if res.Links[0].ChangeID != own.ID || res.Links[0].Role != domain.ChangeLinkRoleOwnService || res.Links[0].LagSeconds != 720 {
		t.Fatalf("own link = %+v, want own_service lag 720 anchored on the succeeded row %s", res.Links[0], own.ID)
	}
	if res.Links[0].ChangeID == ownStarted.ID {
		t.Fatal("the anchor is the group's LATEST phase, not its started")
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1 AND change_id = $2`, inc, ownStarted.ID); n != 0 {
		t.Fatal("one link per GROUP: the started row must not be linked beside the terminal")
	}
	if res.Links[1].ChangeID != upRow.ID || res.Links[1].Role != domain.ChangeLinkRoleUpstream || res.Links[1].LagSeconds != 2400 {
		t.Fatalf("upstream link = %+v, want upstream lag 2400", res.Links[1])
	}
	notes := changeNotes(t, st, ctx, inc)
	want := "🚀 Changes: 2 preceded this incident — deploy v4.2.1 by github-actions, −12m; deploy db-9 by argo, −40m."
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes = %q, want one: %q", notes, want)
	}
	if strings.Contains(strings.ToLower(notes[0]), "caus") {
		t.Fatal("the note must say preceded, never caused")
	}

	// A second delivery: nothing written, nothing linked.
	again, err := st.LinkPrecedingChanges(ctx, inc, 60*time.Minute, 5)
	if err != nil || !again.Skipped || len(again.Links) != 0 || again.NoteAdded {
		t.Fatalf("redelivery = %+v %v", again, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 2 {
		t.Fatalf("links = %d after redelivery, want 2", n)
	}
	if len(changeNotes(t, st, ctx, inc)) != 1 {
		t.Fatal("the note was appended twice")
	}

	// Both read directions agree row for row (invariant 9).
	fromIncident, err := st.ListIncidentChanges(ctx, f.projectID, inc)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromIncident) != 2 || fromIncident[0].Change.ID != own.ID || fromIncident[0].Role != domain.ChangeLinkRoleOwnService ||
		fromIncident[0].LagSeconds != 720 || fromIncident[1].Change.ID != upRow.ID || fromIncident[1].Role != domain.ChangeLinkRoleUpstream {
		t.Fatalf("incident side = %+v", fromIncident)
	}
	pages := listAllGroups(t, st, ctx, f, opened.Add(-2*time.Hour), opened.Add(time.Hour), nil, nil, 50)
	linked := map[string]domain.ChangeIncidentLink{}
	for _, g := range pages[0] {
		for _, l := range g.Incidents {
			linked[g.ExternalID] = l
		}
	}
	if len(linked) != 1 || linked["run-1"].IncidentID != inc || linked["run-1"].LagSeconds != 720 ||
		linked["run-1"].Role != domain.ChangeLinkRoleOwnService || !linked["run-1"].OpenedAt.Equal(opened) || linked["run-1"].ChangeID != own.ID {
		t.Fatalf("change side = %+v", linked)
	}

}

// The note names at most noteMax entries and counts the rest (D7).
func TestChangeCorrelationNoteNamesAtMostMaxAndCountsTheRest(t *testing.T) {
	st, ctx := declStore(t)
	f2 := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	opened2 := now.Add(-time.Minute)
	for i := 0; i < 7; i++ {
		mustRecord(t, st, ctx, changeInput(f2, fmt.Sprintf("many-%d", i), domain.ChangePhaseSucceeded, opened2.Add(-time.Duration(i+1)*time.Minute)))
	}
	inc2 := openServiceIncidentAt(t, st, ctx, f2, opened2)
	if _, err := st.LinkPrecedingChanges(ctx, inc2, time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	if notes := changeNotes(t, st, ctx, inc2); len(notes) != 1 || !strings.HasPrefix(notes[0], "🚀 Changes: 7 preceded this incident — ") ||
		!strings.HasSuffix(notes[0], "; … and 4 more.") || strings.Count(notes[0], " by github-actions") != 3 {
		t.Fatalf("capped note = %q", notes)
	}
}

// The anchor is the latest phase KNOWN at the delivery: started 10 min before open, succeeded
// recorded AFTER → the link anchors the started row with its lag; the group's live phases show
// both; the note is unchanged.
func TestChangeCorrelationAnchorsTheLatestPhaseKnownAtOpen(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	started := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseStarted, now.Add(-10*time.Minute)))
	opened := started.OccurredAt.Add(10 * time.Minute)
	inc := openServiceIncidentAt(t, st, ctx, f, opened)
	res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
	if err != nil || len(res.Links) != 1 || res.Links[0].ChangeID != started.ID || res.Links[0].LagSeconds != 600 {
		t.Fatalf("link at open = %+v %v", res, err)
	}
	notesBefore := changeNotes(t, st, ctx, inc)

	mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, opened.Add(time.Minute)))
	links, err := st.ListIncidentChanges(ctx, f.projectID, inc)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %+v %v", links, err)
	}
	l := links[0]
	if l.Change.ID != started.ID || l.Change.Phase != domain.ChangePhaseStarted || l.LagSeconds != 600 || !l.OccurredAt.Equal(started.OccurredAt) {
		t.Fatalf("the anchor moved: %+v", l)
	}
	if len(l.Phases) != 2 || l.Phases[1].Phase != domain.ChangePhaseSucceeded {
		t.Fatalf("live phases = %+v, want started and succeeded", l.Phases)
	}
	if after := changeNotes(t, st, ctx, inc); len(after) != 1 || after[0] != notesBefore[0] {
		t.Fatalf("the note changed: %q → %q", notesBefore, after)
	}
	// Redelivery after the terminal: still nothing rewritten.
	if again, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5); err != nil || !again.Skipped {
		t.Fatalf("redelivery = %+v %v", again, err)
	}
}

// Links and note are ONE transaction: a planted failure between them leaves neither; the
// retry then writes both. A cross-project link is refused by the database (invariant 22).
// Unknown, monitor-anchored and project-level incidents are skipped.
func TestChangeCorrelationIsAtomicAndTenantSafe(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	own := mustRecord(t, st, ctx, changeInput(f, "run-1", domain.ChangePhaseSucceeded, now.Add(-12*time.Minute)))
	inc := openServiceIncidentAt(t, st, ctx, f, own.OccurredAt.Add(12*time.Minute))

	st.changeNoteFault = func() error { return errors.New("planted between links and note") }
	if _, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5); err == nil || !strings.Contains(err.Error(), "planted") {
		t.Fatalf("planted fault: %v", err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 0 {
		t.Fatalf("%d links survived a failed note: not one transaction", n)
	}
	if len(changeNotes(t, st, ctx, inc)) != 0 {
		t.Fatal("a note survived the failure")
	}
	st.changeNoteFault = nil
	res, err := st.LinkPrecedingChanges(ctx, inc, time.Hour, 5)
	if err != nil || len(res.Links) != 1 || !res.NoteAdded {
		t.Fatalf("retry = %+v %v", res, err)
	}

	// Cross-project by direct SQL: the composite keys refuse it in both directions.
	foreign := changeInput(f, "foreign-1", domain.ChangePhaseSucceeded, now.Add(-5*time.Minute))
	foreign.ProjectID, foreign.ServiceID = f.otherProjectID, f.otherServiceID
	foreignRow := mustRecord(t, st, ctx, foreign)
	for _, project := range []string{f.projectID, f.otherProjectID} {
		_, err := st.pool.Exec(ctx, `
			INSERT INTO incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)
			VALUES ($1, $2, $3, 'own_service', $4, 300)`, inc, foreignRow.ID, project, foreignRow.OccurredAt)
		if !isForeignKeyViolation(err) {
			t.Fatalf("cross-project link with project %s: got %v, want an FK violation", project, err)
		}
	}
	// A negative lag is refused too.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)
		VALUES ($1, $2, $3, 'upstream', $4, -1)`, inc, own.ID, f.projectID, own.OccurredAt); pgErrCode(err) != "23514" {
		t.Fatalf("negative lag: %v", err)
	}

	// Skips: unknown id, a project-level manual incident (no service), a malformed id.
	for _, id := range []string{"0199a0c0-0000-7000-8000-000000000000", "not-a-uuid"} {
		if res, err := st.LinkPrecedingChanges(ctx, id, time.Hour, 5); err != nil || !res.Skipped {
			t.Fatalf("unknown %q: %+v %v", id, res, err)
		}
	}
	var manual string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO incidents (project_id, title, status, impact, source) VALUES ($1, 'manual', 'investigating', 'minor', 'manual') RETURNING id`,
		f.projectID).Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if res, err := st.LinkPrecedingChanges(ctx, manual, time.Hour, 5); err != nil || !res.Skipped {
		t.Fatalf("manual incident: %+v %v", res, err)
	}

}

// An incident with no preceding change writes nothing — links or note (no empty note).
func TestChangeCorrelationWithNoPrecedingChangeWritesNothing(t *testing.T) {
	st, ctx := declStore(t)
	f3 := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	quiet := openServiceIncidentAt(t, st, ctx, f3, now)
	if res, err := st.LinkPrecedingChanges(ctx, quiet, time.Hour, 5); err != nil || res.Skipped || len(res.Links) != 0 || res.NoteAdded {
		t.Fatalf("quiet incident: %+v %v", res, err)
	}
	if len(changeNotes(t, st, ctx, quiet)) != 0 {
		t.Fatal("an empty correlation wrote a note")
	}
}

// ── Retention (D9; invariant 13) ─────────────────────────────────────────────────────────

// Groups older than the bound are deleted by KEY in batches: every phase of a selected group
// gone in one statement and none of an unselected one (a batch boundary of ONE group planted
// against a two-phase group), their links gone by cascade, a younger group untouched, and an
// old started with a young terminal KEPT — the group's age is its latest phase.
func TestChangeRetentionRemovesWholeGroupsByLatestPhaseInKeyOrder(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	day := 24 * time.Hour
	cutoff := now.Add(-400 * day)

	// G1: both phases old (latest −499 d). G5: old single (−480 d). G4: old single (−450 d).
	// G2: old started, young terminal (kept). G3: young (kept).
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g1", domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-500*day))
	g1Latest := plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g1", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-499*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g5", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-480*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g4", domain.ChangeKindDeploy, domain.ChangePhaseFailed, now.Add(-450*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g2", domain.ChangeKindDeploy, domain.ChangePhaseStarted, now.Add(-500*day))
	plantChangeRow(t, st, ctx, f.projectID, f.serviceID, "ci", "g2", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-1*day))
	plantChangeRow(t, st, ctx, f.projectID, f.upstreamID, "ci", "g3", domain.ChangeKindDeploy, domain.ChangePhaseSucceeded, now.Add(-10*day))
	// A link to G1's anchor: it must follow by cascade.
	inc := openServiceIncidentAt(t, st, ctx, f, now.Add(-499*day).Add(5*time.Minute))
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)
		VALUES ($1, $2, $3, 'own_service', $4, 300)`, inc, g1Latest, f.projectID, now.Add(-499*day)); err != nil {
		t.Fatal(err)
	}

	// Batch of ONE group key: G1 first (oldest latest), both of its rows.
	groups, rows, err := st.PurgeChangeGroups(ctx, cutoff, 1)
	if err != nil || groups != 1 || rows != 2 {
		t.Fatalf("batch 1 = %d groups %d rows %v, want 1 group / 2 rows (G1 whole)", groups, rows, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'g1'`); n != 0 {
		t.Fatalf("G1 rows left = %d: the group was split", n)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM incident_changes WHERE incident_id = $1`, inc); n != 0 {
		t.Fatal("G1's link survived the cascade")
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id IN ('g5', 'g4', 'g2', 'g3')`); n != 5 {
		t.Fatalf("unselected groups touched: %d rows, want 5", n)
	}
	// Next in key order: G5 (−480 d), then G4 (−450 d); then a short batch of 0.
	if groups, rows, err = st.PurgeChangeGroups(ctx, cutoff, 1); err != nil || groups != 1 || rows != 1 || countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'g5'`) != 0 {
		t.Fatalf("batch 2 = %d/%d %v, want G5", groups, rows, err)
	}
	if groups, rows, err = st.PurgeChangeGroups(ctx, cutoff, 10); err != nil || groups != 1 || rows != 1 || countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'g4'`) != 0 {
		t.Fatalf("batch 3 = %d/%d %v, want G4 alone (G2's latest is young)", groups, rows, err)
	}
	if groups, rows, err = st.PurgeChangeGroups(ctx, cutoff, 10); err != nil || groups != 0 || rows != 0 {
		t.Fatalf("batch 4 = %d/%d %v, want nothing", groups, rows, err)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'g2'`); n != 2 {
		t.Fatalf("G2 (old started, young terminal) rows = %d, want both kept", n)
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes WHERE external_id = 'g3'`); n != 1 {
		t.Fatal("the young group was touched")
	}
	if _, _, err := st.PurgeChangeGroups(ctx, cutoff, 0); err == nil {
		t.Fatal("a zero batch bound must be refused")
	}
}
