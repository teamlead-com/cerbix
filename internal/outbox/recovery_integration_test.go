package outbox

// FR-043 on a real database: the whole path a recovery takes — RecordScheduledResult, then
// Reconciler.Reconcile exactly as ingest's Consumer.handle calls it, then this worker delivering
// through the real notify.Dispatcher to a webhook channel. §7 asks for it in so many words: the
// confirming transaction clears recovery_started_at before either consumer reads the monitor, so a
// unit test with a hand-built monitor cannot catch a sentence rendered from the row.
//
// Isolation: internal/store truncates the shared test database while `go test ./...` runs packages
// in parallel, so each test here migrates a database of its OWN, created from
// CERBIX_TEST_DATABASE_DSN and dropped by exact name at cleanup — the pattern of
// internal/appmux/harness_test.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/ingest"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/store"
)

const (
	recoveredToday = "Monitor recovered — automatically resolved."
	upToday        = "🟢 rt-api recovered — it is back UP."
	downToday      = "🔴 rt-api is DOWN."
)

// recoveryRig is one monitor (threshold 3 unless stated) over a private database, with the ingest reconciler,
// the outbox worker and a webhook channel recording what it is sent.
type recoveryRig struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	conn  *pgx.Conn
	rc    *ingest.Reconciler
	w     *Worker
	hooks *hookRecorder
	actor store.AuditActor
	mon   domain.Monitor
	ts    time.Time
}

func newRecoveryRig(t *testing.T) *recoveryRig { t.Helper(); return newRecoveryRigAt(t, 3) }

func newRecoveryRigAt(t *testing.T, threshold int) *recoveryRig {
	t.Helper()
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the FR-043 ingest + outbox PostgreSQL tests")
	}
	u, err := url.Parse(gate)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, gate)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("cerbix_test_outbox_rt_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database %s (requires CREATEDB): %v", name, err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := admin.Exec(dctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = admin.Close(dctx)
	})
	u.Path = "/" + name
	dsn := u.String()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	org, err := st.CreateOrganization(ctx, "rt", "RT")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "rt", "RT")
	if err != nil {
		t.Fatal(err)
	}
	actor := store.AuditActor{ViaToken: true, Label: "token:rt-test"}
	// The production create door; the fixture sets the threshold explicitly (§4.5: nothing defaults).
	mon, err := st.CreateMonitorByPrincipal(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "rt-api", Type: domain.MonitorTCP, Target: "10.0.0.1:80",
		IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 2, RecoveryThreshold: threshold,
		Enabled: true, AutoIncident: true,
	}, actor)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	hooks, srv := newHookRecorder(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	nd := notify.New(channelStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": srv.URL + "/hook"}},
	}}, nil)
	return &recoveryRig{
		t: t, ctx: ctx, st: st, conn: conn, hooks: hooks, actor: actor, mon: mon,
		rc: ingest.NewReconciler(st, nil, nil, logger),
		w:  New(st, &fakeWebhook{}, nd, &fakeMetrics{}, logger),
		// Results a few minutes in the past, one second apart: inside the default skew, always fresh.
		ts: time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Millisecond),
	}
}

// check records one scheduled result at the monitor's current revision and, on a status change,
// reconciles exactly as ingest's Consumer.handle does.
func (r *recoveryRig) check(up bool) store.ResultOutcome {
	r.t.Helper()
	r.ts = r.ts.Add(time.Second)
	cur, err := r.st.GetMonitor(r.ctx, r.mon.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	hb := domain.Heartbeat{MonitorID: r.mon.ID, Ts: r.ts, Up: up, ExecutionRevision: cur.ExecutionRevision, LatencyMS: 12}
	if !up {
		hb.Msg = "connection refused"
	}
	o, err := r.st.RecordScheduledResult(r.ctx, hb)
	if err != nil {
		r.t.Fatalf("record result: %v", err)
	}
	if !o.Applied {
		r.t.Fatalf("result not applied: %+v", o)
	}
	if o.Applied && o.Prev != o.Cur {
		r.rc.Reconcile(r.ctx, hb, o.Prev, o.Cur, o.Suppressed, o.Recovery)
	}
	return o
}

func (r *recoveryRig) monitor() domain.Monitor {
	r.t.Helper()
	m, err := r.st.GetMonitor(r.ctx, r.mon.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

// goDown fails the monitor failure_threshold times; it must end DOWN with one open auto-incident,
// whose DOWN notification is delivered here so the recovery that follows is counted on its own.
func (r *recoveryRig) goDown() domain.Incident {
	r.t.Helper()
	r.check(false)
	if o := r.check(false); o.Cur != domain.StatusDown || o.Prev == domain.StatusDown {
		r.t.Fatalf("second failure: %+v, want the down verdict", o)
	}
	inc, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, r.mon.ID)
	if err != nil {
		r.t.Fatalf("no open auto-incident after the down verdict: %v", err)
	}
	before := len(r.hooks.texts())
	r.w.drain(r.ctx)
	if got := r.hooks.texts()[before:]; len(got) != 1 || got[0] != downToday {
		r.t.Fatalf("down notification = %q, want [%q]", got, downToday)
	}
	return inc
}

// upTransitions counts the monitor's up MonitorTransition events in the outbox, delivered or not.
func (r *recoveryRig) upTransitions() int {
	r.t.Helper()
	var n int
	if err := r.conn.QueryRow(r.ctx,
		`SELECT count(*) FROM outbox_events
		  WHERE topic = 'monitor_transition' AND payload->>'monitor_id' = $1 AND payload->>'cur' = 'up'`,
		r.mon.ID).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *recoveryRig) autoIncidents() []domain.Incident {
	r.t.Helper()
	all, err := r.st.ListIncidentsByProject(r.ctx, r.mon.ProjectID)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []domain.Incident
	for _, inc := range all {
		if inc.MonitorID == r.mon.ID && inc.Source == domain.SourceAuto {
			out = append(out, inc)
		}
	}
	return out
}

func (r *recoveryRig) resolvedUpdates(incidentID string) []domain.IncidentUpdate {
	r.t.Helper()
	ups, err := r.st.ListIncidentUpdates(r.ctx, incidentID)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []domain.IncidentUpdate
	for _, u := range ups {
		if u.Status == domain.IncidentResolved {
			out = append(out, u)
		}
	}
	return out
}

// startRun records the run's first success: the monitor must stay DOWN with the run at 1 and a
// first-OK stamped, which is returned — the instant the sentence must name after the columns are
// cleared.
func (r *recoveryRig) startRun() time.Time {
	r.t.Helper()
	if o := r.check(true); o.Cur != domain.StatusDown || o.Prev != domain.StatusDown || o.Recovery != nil {
		r.t.Fatalf("first success of a threshold-3 run: %+v, want still down, no snapshot", o)
	}
	m := r.monitor()
	if m.ConsecutiveSuccesses != 1 || m.RecoveryStartedAt == nil {
		r.t.Fatalf("after the first success: consecutive_successes=%d recovery_started_at=%v, want 1 and stamped",
			m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
	return *m.RecoveryStartedAt
}

func sentence(n, m int, firstOK time.Time) string {
	return fmt.Sprintf("Recovered: %d/%d consecutive checks passed; first OK at %s", n, m, firstOK.UTC().Format(time.RFC3339))
}

// confirmRun records successes two and three: the third confirms the recovery and its outcome
// carries the snapshot 3/3 with the first-OK read mid-run; the columns are cleared by that commit.
func (r *recoveryRig) confirmRun(firstOK time.Time) {
	r.t.Helper()
	if o := r.check(true); o.Cur != domain.StatusDown {
		r.t.Fatalf("second success: %+v, want still down", o)
	}
	o := r.check(true)
	if o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		r.t.Fatalf("third success: %+v, want the confirmed down→up", o)
	}
	if o.Recovery == nil || o.Recovery.Run != 3 || o.Recovery.Threshold != 3 || !o.Recovery.FirstOK.Equal(firstOK) {
		r.t.Fatalf("snapshot = %+v, want 3/3 first OK %v", o.Recovery, firstOK)
	}
	if m := r.monitor(); m.Status != domain.StatusUp || m.ConsecutiveSuccesses != 0 || m.RecoveryStartedAt != nil {
		r.t.Fatalf("after the confirming commit: status=%s consecutive_successes=%d recovery_started_at=%v, want up, 0, NULL",
			m.Status, m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
}

// TestRecoveryThroughIngestAndOutbox (FR-043 §7 "Ingest + outbox, real database"; I4, I9, I14): a
// threshold-3 monitor goes down and then succeeds three times. Exactly one up MonitorTransition, one
// resolved auto-incident, one recovery notification — and the sentence 3/3 with the first-OK read
// mid-run in BOTH the resolving update and the notification, character for character, read after
// the confirming commit cleared the columns. The second outage repeats it with the threshold raised
// through the store's update door after the recovery commit and before delivery: the text still
// says 3/3, because it is rendered from the event, not the row.
//
// I9 rides the first outage: a person writes an update between success 1 and success 3, and the
// incident's resolved_at is the resolving transaction's clock — equal to the resolving update's
// own instant, not earlier than started_at and not earlier than that update.
func TestRecoveryThroughIngestAndOutbox(t *testing.T) {
	r := newRecoveryRig(t)

	// ── outage 1 ──
	inc := r.goDown()
	firstOK := r.startRun()
	if !firstOK.After(inc.StartedAt) && !firstOK.Equal(inc.StartedAt) {
		t.Fatalf("first OK %v precedes the incident's started_at %v on one consumer (I17)", firstOK, inc.StartedAt)
	}
	manual, err := r.st.AddIncidentUpdateByPrincipal(r.ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Body: "Pool is warming; watching it.", Author: "ops",
	}, r.actor)
	if err != nil {
		t.Fatalf("a person's update during the run: %v (the incident must still be open)", err)
	}
	r.confirmRun(firstOK)

	want := sentence(3, 3, firstOK)
	incs := r.autoIncidents()
	if len(incs) != 1 || incs[0].ID != inc.ID || incs[0].Status != domain.IncidentResolved || incs[0].ResolvedAt == nil {
		t.Fatalf("auto-incidents = %+v, want the one, resolved", incs)
	}
	res := r.resolvedUpdates(inc.ID)
	if len(res) != 1 {
		t.Fatalf("resolving updates = %+v, want exactly one", res)
	}
	if res[0].Body != recoveredToday+" "+want {
		t.Fatalf("resolving update = %q, want %q", res[0].Body, recoveredToday+" "+want)
	}
	resolvedAt := *incs[0].ResolvedAt
	if !resolvedAt.Equal(res[0].CreatedAt) {
		t.Fatalf("resolved_at %v is not the resolving transaction's clock %v (I9)", resolvedAt, res[0].CreatedAt)
	}
	if resolvedAt.Before(incs[0].StartedAt) || resolvedAt.Before(manual.CreatedAt) {
		t.Fatalf("resolved_at %v precedes started_at %v or the update written during the run %v (I9)",
			resolvedAt, incs[0].StartedAt, manual.CreatedAt)
	}
	if n := r.upTransitions(); n != 1 {
		t.Fatalf("up transitions in the outbox = %d, want 1 (I4)", n)
	}
	before := len(r.hooks.texts())
	r.w.drain(r.ctx)
	got := r.hooks.texts()[before:]
	if len(got) != 1 || got[0] != upToday+" "+want {
		t.Fatalf("recovery notifications = %q, want [%q]", got, upToday+" "+want)
	}
	// I14: the two surfaces carry the same string, character for character.
	if a, b := strings.TrimPrefix(res[0].Body, recoveredToday+" "), strings.TrimPrefix(got[0], upToday+" "); a != b {
		t.Fatalf("incident sentence %q != notification sentence %q (I14)", a, b)
	}

	// ── outage 2: the threshold is edited between the recovery commit and the delivery ──
	inc2 := r.goDown()
	if inc2.ID == inc.ID {
		t.Fatal("the second outage reused the resolved incident")
	}
	firstOK2 := r.startRun()
	r.confirmRun(firstOK2)
	edited := r.monitor()
	edited.RecoveryThreshold = 5
	if _, err := r.st.UpdateMonitorByPrincipal(r.ctx, edited, r.actor, false); err != nil {
		t.Fatalf("raise the threshold before delivery: %v", err)
	}
	if m := r.monitor(); m.RecoveryThreshold != 5 {
		t.Fatalf("threshold after the edit = %d, want 5", m.RecoveryThreshold)
	}
	want2 := sentence(3, 3, firstOK2)
	if res2 := r.resolvedUpdates(inc2.ID); len(res2) != 1 || res2[0].Body != recoveredToday+" "+want2 {
		t.Fatalf("second resolving updates = %+v, want one reading %q", res2, want2)
	}
	if n := r.upTransitions(); n != 2 {
		t.Fatalf("up transitions in the outbox = %d, want 2 after two recoveries (I4)", n)
	}
	before = len(r.hooks.texts())
	r.w.drain(r.ctx)
	if got := r.hooks.texts()[before:]; len(got) != 1 || got[0] != upToday+" "+want2 {
		t.Fatalf("recovery notification after the edit = %q, want [%q] (I14)", got, upToday+" "+want2)
	}
}

// TestBrokenRecoveryRunDeliversNothing (I4): two successes and a failure on a threshold-3 monitor —
// the run breaks. No MonitorTransition is enqueued, the auto-incident stays open, nothing is
// delivered, and the monitor is still DOWN with the run reset.
func TestBrokenRecoveryRunDeliversNothing(t *testing.T) {
	r := newRecoveryRig(t)
	inc := r.goDown()
	r.startRun()
	if o := r.check(true); o.Cur != domain.StatusDown {
		t.Fatalf("second success: %+v, want still down", o)
	}
	if o := r.check(false); o.Prev != domain.StatusDown || o.Cur != domain.StatusDown {
		t.Fatalf("the breaking failure: %+v, want down→down", o)
	}
	if m := r.monitor(); m.Status != domain.StatusDown || m.ConsecutiveSuccesses != 0 || m.RecoveryStartedAt != nil {
		t.Fatalf("after the break: status=%s run=%d first_ok=%v, want down, 0, NULL", m.Status, m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
	if n := r.upTransitions(); n != 0 {
		t.Fatalf("up transitions = %d, want 0", n)
	}
	open, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, r.mon.ID)
	if err != nil || open.ID != inc.ID {
		t.Fatalf("open auto-incident = %+v (%v), want %s still open", open, err, inc.ID)
	}
	before := len(r.hooks.texts())
	r.w.drain(r.ctx)
	if got := r.hooks.texts()[before:]; len(got) != 0 {
		t.Fatalf("a broken run delivered %q, want nothing", got)
	}
}

// TestManualResolveDuringARecoveryRun (D11): a person resolves the auto-incident in the middle of a
// run. When the run breaks, no new incident opens — the monitor never left DOWN. When a later run
// completes, there is one up transition and the person's resolution is the only one: the
// reconciler finds nothing open and resolves nothing a second time. The recovery is still notified.
func TestManualResolveDuringARecoveryRun(t *testing.T) {
	r := newRecoveryRig(t)
	inc := r.goDown()
	r.startRun()
	if _, err := r.st.AddIncidentUpdateByPrincipal(r.ctx, domain.IncidentUpdate{
		IncidentID: inc.ID, Status: domain.IncidentResolved, Body: "Handled by hand.", Author: "ops",
	}, r.actor); err != nil {
		t.Fatalf("manual resolve during the run: %v (the incident must still be open)", err)
	}

	// The run breaks.
	if o := r.check(false); o.Prev != domain.StatusDown || o.Cur != domain.StatusDown {
		t.Fatalf("the breaking failure: %+v, want down→down", o)
	}
	if incs := r.autoIncidents(); len(incs) != 1 || incs[0].Status != domain.IncidentResolved {
		t.Fatalf("auto-incidents after the break = %+v, want only the manually resolved one", incs)
	}
	if _, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, r.mon.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an auto-incident is open after the break: %v", err)
	}

	// A later run completes.
	firstOK := r.startRun()
	r.confirmRun(firstOK)
	if n := r.upTransitions(); n != 1 {
		t.Fatalf("up transitions = %d, want 1", n)
	}
	incs := r.autoIncidents()
	if len(incs) != 1 {
		t.Fatalf("auto-incidents = %+v, want one", incs)
	}
	if res := r.resolvedUpdates(inc.ID); len(res) != 1 || res[0].Author != "ops" {
		t.Fatalf("resolving updates = %+v, want only the person's", res)
	}
	before := len(r.hooks.texts())
	r.w.drain(r.ctx)
	want := upToday + " " + sentence(3, 3, firstOK)
	if got := r.hooks.texts()[before:]; len(got) != 1 || got[0] != want {
		t.Fatalf("recovery notifications = %q, want [%q]", got, want)
	}
}

// TestThresholdOneRecoveryIsToday (I1) on the same path: at threshold 1 the first success is the
// recovery, as before the feature, and the resolving update and the notification are today's text
// byte for byte — no sentence for a run of one.
func TestThresholdOneRecoveryIsToday(t *testing.T) {
	r := newRecoveryRigAt(t, 1)
	inc := r.goDown()
	o := r.check(true)
	if o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		t.Fatalf("first success at threshold 1: %+v, want down→up", o)
	}
	if m := r.monitor(); m.ConsecutiveSuccesses != 0 || m.RecoveryStartedAt != nil {
		t.Fatalf("after the recovery: run=%d first_ok=%v, want 0, NULL", m.ConsecutiveSuccesses, m.RecoveryStartedAt)
	}
	if res := r.resolvedUpdates(inc.ID); len(res) != 1 || res[0].Body != recoveredToday {
		t.Fatalf("resolving updates = %+v, want one reading %q", res, recoveredToday)
	}
	before := len(r.hooks.texts())
	r.w.drain(r.ctx)
	if got := r.hooks.texts()[before:]; len(got) != 1 || got[0] != upToday {
		t.Fatalf("recovery notifications = %q, want [%q]", got, upToday)
	}
}
