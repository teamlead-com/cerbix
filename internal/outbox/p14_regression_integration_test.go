package outbox

// P14 (FR-041 / NFR-034) on a real database: what a project WITHOUT phone channels produces through an
// outage with an escalation ladder, frozen as a golden before any FR-041a code existed. Every later
// change must leave these bytes as they are; a difference is a P14 violation to fix in the code, never
// in the golden. `go test ./internal/outbox -run TestP14 -update-p14` rewrites the golden and exists
// for the capture on the unmodified tree only.
//
// What is compared: every outbox_events row (topic, status, payload bytes), every HTTP body each
// channel endpoint received, the escalation steps each pass enqueued, and the JSON of each incident as
// GetIncident returns it. UUIDs become `<idN>` in first-seen order across the whole text and RFC3339
// instants become `<t>`; nothing else is rewritten.
//
// Isolation is the recovery rig's (recovery_integration_test.go): a database of its own, created from
// CERBIX_TEST_DATABASE_DSN and dropped by exact name at cleanup. Storage mode does not reach any of
// these bytes, so the golden holds in hypertable and declarative-partition mode alike.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/ingest"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/store"
)

var updateP14 = flag.Bool("update-p14", false, "rewrite testdata/p14_no_phone_project.golden (the capture on the unmodified tree only)")

const p14GoldenPath = "testdata/p14_no_phone_project.golden"

// bodyRecorder is a channel endpoint that keeps every request body it receives, byte for byte, in
// arrival order.
type bodyRecorder struct {
	name   string
	url    string
	mu     sync.Mutex
	bodies [][]byte
}

func newBodyRecorder(t *testing.T, name string) *bodyRecorder {
	t.Helper()
	b := &bodyRecorder{name: name}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, raw)
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	b.url = srv.URL + "/" + name
	return b
}

func (b *bodyRecorder) received() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]byte(nil), b.bodies...)
}

// p14Pass is one escalation pass at a controlled instant and the steps it enqueued.
type p14Pass struct {
	label string
	pass  store.EscalationPass
	steps []domain.EscalationStepAlert
}

type p14Rig struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	conn     *pgx.Conn
	rc       *ingest.Reconciler
	w        *Worker
	ts       time.Time
	ladder   domain.Monitor
	flat     domain.Monitor
	recs     []*bodyRecorder
	chanName map[string]string // channel id → name
	passes   []p14Pass
	seenStep int // escalation_step rows already attributed to a pass
	// lastEscalatedAt is the ladder incident's last escalation in the controlled frame, in seconds
	// after its start; nil before the first step fired.
	lastEscalatedAt *int
}

// p14Wiring varies how the rig is wired, never the scenario: the zero value is the wiring the golden
// was captured with; TestP14NoPhoneProjectGoldenProductionWiring passes production's phone wiring.
type p14Wiring struct {
	// prepare runs after the store is opened and before any row of the scenario is written.
	prepare func(ctx context.Context, st *store.Store)
	// worker builds the outbox worker over the rig's store.
	worker func(st *store.Store, logger *slog.Logger) *Worker
}

func newP14Rig(t *testing.T) *p14Rig {
	t.Helper()
	return newP14RigWired(t, p14Wiring{})
}

func newP14RigWired(t *testing.T, wiring p14Wiring) *p14Rig {
	t.Helper()
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the P14 golden regression")
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
	name := fmt.Sprintf("cerbix_test_outbox_p14_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	if wiring.prepare != nil {
		wiring.prepare(ctx, st)
	}

	org, err := st.CreateOrganization(ctx, "p14", "P14")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "p14", "P14")
	if err != nil {
		t.Fatal(err)
	}
	r := &p14Rig{t: t, ctx: ctx, st: st, conn: conn, chanName: map[string]string{}}

	// Three channels, none of them a phone: a webhook, a Slack, and a DISABLED Slack whose endpoint
	// must never hear anything although it sits on the ladder's second step.
	channel := func(name string, typ domain.ChannelType, enabled bool) domain.NotificationChannel {
		rec := newBodyRecorder(t, name)
		r.recs = append(r.recs, rec)
		ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
			ProjectID: proj.ID, Type: typ, Name: name, Enabled: enabled, Config: map[string]string{"url": rec.url},
		})
		if err != nil {
			t.Fatalf("create channel %s: %v", name, err)
		}
		r.chanName[ch.ID] = name
		return ch
	}
	hook := channel("hook", domain.ChannelWebhook, true)
	slack := channel("slack", domain.ChannelSlack, true)
	slackOff := channel("slack-off", domain.ChannelSlack, false)

	policy, err := st.CreateEscalationPolicy(ctx, domain.EscalationPolicy{
		ProjectID: proj.ID, Name: "p14-ladder", RepeatLast: true,
		Steps: []domain.EscalationStep{
			{AfterSeconds: 0, Targets: []domain.EscalationTarget{{Type: domain.EscalationTargetChannel, ID: hook.ID}}},
			{AfterSeconds: 300, Targets: []domain.EscalationTarget{
				{Type: domain.EscalationTargetChannel, ID: slack.ID},
				{Type: domain.EscalationTargetChannel, ID: slackOff.ID},
			}},
		},
	})
	if err != nil {
		t.Fatalf("create escalation policy: %v", err)
	}

	actor := store.AuditActor{ViaToken: true, Label: "token:p14-test"}
	monitor := func(m domain.Monitor) domain.Monitor {
		m.ProjectID, m.Type, m.IntervalSeconds, m.TimeoutSeconds = proj.ID, domain.MonitorTCP, 60, 5
		m.FailureThreshold, m.RecoveryThreshold, m.Enabled, m.AutoIncident = 2, 1, true, true
		created, err := st.CreateMonitorByPrincipal(ctx, m, actor)
		if err != nil {
			t.Fatalf("create monitor %s: %v", m.Name, err)
		}
		return created
	}
	r.ladder = monitor(domain.Monitor{
		Name: "ladder-api", Target: "10.0.0.1:80", EscalationPolicyID: policy.ID, RenotifySeconds: 600,
	})
	r.flat = monitor(domain.Monitor{Name: "flat-api", Target: "10.0.0.2:80"})
	for _, id := range []string{hook.ID, slack.ID} {
		if err := st.LinkMonitorChannel(ctx, r.flat.ID, id); err != nil {
			t.Fatalf("link flat-api: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.rc = ingest.NewReconciler(st, nil, nil, logger)
	// The real dispatcher over the real store: channel listing, the enabled filter and the bodies are
	// all production's.
	r.w = New(st, &fakeWebhook{}, notify.New(st, nil), &fakeMetrics{}, logger)
	if wiring.worker != nil {
		r.w = wiring.worker(st, logger)
	}
	// Results a few minutes in the past, one second apart: inside the default skew, always fresh.
	r.ts = time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Millisecond)
	return r
}

// check records one scheduled result for m and, on a status change, reconciles exactly as ingest's
// Consumer.handle does.
func (r *p14Rig) check(m domain.Monitor, up bool) store.ResultOutcome {
	r.t.Helper()
	r.ts = r.ts.Add(time.Second)
	cur, err := r.st.GetMonitor(r.ctx, m.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	hb := domain.Heartbeat{MonitorID: m.ID, Ts: r.ts, Up: up, ExecutionRevision: cur.ExecutionRevision, LatencyMS: 12}
	if !up {
		hb.Msg = "connection refused"
	}
	o, err := r.st.RecordScheduledResult(r.ctx, hb)
	if err != nil {
		r.t.Fatalf("record result for %s: %v", m.Name, err)
	}
	if !o.Applied {
		r.t.Fatalf("result for %s not applied: %+v", m.Name, o)
	}
	if o.Prev != o.Cur {
		r.rc.Reconcile(r.ctx, hb, o.Prev, o.Cur, o.Suppressed, o.Recovery)
	}
	return o
}

func (r *p14Rig) goDown(m domain.Monitor) {
	r.t.Helper()
	r.check(m, false)
	if o := r.check(m, false); o.Cur != domain.StatusDown || o.Prev == domain.StatusDown {
		r.t.Fatalf("%s second failure: %+v, want the down verdict", m.Name, o)
	}
	if _, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, m.ID); err != nil {
		r.t.Fatalf("%s: no open auto-incident after the down verdict: %v", m.Name, err)
	}
}

func (r *p14Rig) recover(m domain.Monitor) {
	r.t.Helper()
	if o := r.check(m, true); o.Prev != domain.StatusDown || o.Cur != domain.StatusUp {
		r.t.Fatalf("%s recovery: %+v, want down→up", m.Name, o)
	}
}

// deliver drains the outbox until a drain settles nothing more: a fenced event becomes claimable only
// after the event it waits on was delivered by an earlier claim.
func (r *p14Rig) deliver() {
	r.t.Helper()
	prev := -1
	for i := 0; i < 10; i++ {
		r.w.drain(r.ctx)
		var pending int
		if err := r.conn.QueryRow(r.ctx,
			`SELECT count(*) FROM outbox_events WHERE status IN ('pending','pending_fenced') AND next_attempt_at <= now()`,
		).Scan(&pending); err != nil {
			r.t.Fatal(err)
		}
		if pending == 0 || pending == prev {
			return
		}
		prev = pending
	}
}

// passAt runs one escalation pass at `offset` seconds after the ladder incident's start. The pass
// reads the database's now(), which a test cannot set, so the incident is moved instead: its start
// becomes now() − offset, and its last escalation keeps its place in that frame. The pass runs in a
// later transaction, so it sees the offset plus a few milliseconds and never less — every due time
// here is reached, none is crossed early.
func (r *p14Rig) passAt(label string, offset int) {
	r.t.Helper()
	inc, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, r.ladder.ID)
	if err != nil {
		r.t.Fatalf("%s: the ladder incident is not open: %v", label, err)
	}
	var last any
	if r.lastEscalatedAt != nil {
		last = *r.lastEscalatedAt
	}
	if _, err := r.conn.Exec(r.ctx,
		`UPDATE incidents
		    SET started_at = now() - make_interval(secs => $2::int),
		        last_escalated_at = CASE WHEN $3::int IS NULL THEN NULL
		                                 ELSE now() - make_interval(secs => $2::int) + make_interval(secs => $3::int) END
		  WHERE id = $1`, inc.ID, offset, last); err != nil {
		r.t.Fatalf("%s: move the ladder incident: %v", label, err)
	}
	pass, err := r.st.AdvanceEscalations(r.ctx)
	if err != nil {
		r.t.Fatalf("%s: advance escalations: %v", label, err)
	}
	if pass.Total() > 0 {
		o := offset
		r.lastEscalatedAt = &o
	}
	rows, err := r.conn.Query(r.ctx,
		`SELECT payload::text FROM outbox_events WHERE topic = 'escalation_step' ORDER BY created_at`)
	if err != nil {
		r.t.Fatal(err)
	}
	var all []domain.EscalationStepAlert
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			r.t.Fatal(err)
		}
		var a domain.EscalationStepAlert
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			r.t.Fatal(err)
		}
		all = append(all, a)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	rows.Close()
	fresh := append([]domain.EscalationStepAlert(nil), all[r.seenStep:]...)
	r.seenStep = len(all)
	sort.SliceStable(fresh, func(i, j int) bool {
		if fresh[i].Step != fresh[j].Step {
			return fresh[i].Step < fresh[j].Step
		}
		return !fresh[i].Repeat && fresh[j].Repeat
	})
	r.passes = append(r.passes, p14Pass{label: label, pass: pass, steps: fresh})
	r.deliver()
}

// p14Normaliser rewrites UUIDs to `<idN>` in first-seen order and RFC3339 instants to `<t>`.
type p14Normaliser struct{ ids map[string]string }

var (
	p14UUID    = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	p14Instant = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}(:?\d{2})?)`)
)

func (n *p14Normaliser) apply(s string) string {
	s = p14Instant.ReplaceAllString(s, "<t>")
	return p14UUID.ReplaceAllStringFunc(s, func(id string) string {
		id = strings.ToLower(id)
		if p, ok := n.ids[id]; ok {
			return p
		}
		p := fmt.Sprintf("<id%d>", len(n.ids)+1)
		n.ids[id] = p
		return p
	})
}

// p14Masked is the order key for rows that tie on created_at: the text with every id and instant
// blanked, so the order never depends on a random UUID.
func p14Masked(s string) string {
	return p14UUID.ReplaceAllString(p14Instant.ReplaceAllString(s, "<t>"), "<id>")
}

func (r *p14Rig) golden() string {
	r.t.Helper()
	n := &p14Normaliser{ids: map[string]string{}}
	var b strings.Builder
	b.WriteString("# P14 (FR-041 / NFR-034): a project without phone channels through an outage with a ladder.\n")
	b.WriteString("# Captured on the tree before FR-041a. Never regenerate it; a difference is a P14 violation.\n")

	// 1. Every outbox row: (created_at, then masked text) — rows written by one transaction share
	// created_at, and their ids are random.
	type outboxRow struct {
		at                     time.Time
		topic, status, payload string
	}
	rows, err := r.conn.Query(r.ctx, `SELECT created_at, topic, status, payload::text FROM outbox_events ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	var obs []outboxRow
	for rows.Next() {
		var o outboxRow
		if err := rows.Scan(&o.at, &o.topic, &o.status, &o.payload); err != nil {
			r.t.Fatal(err)
		}
		obs = append(obs, o)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	rows.Close()
	sort.SliceStable(obs, func(i, j int) bool {
		if !obs[i].at.Equal(obs[j].at) {
			return obs[i].at.Before(obs[j].at)
		}
		return p14Masked(obs[i].topic+" "+obs[i].payload) < p14Masked(obs[j].topic+" "+obs[j].payload)
	})
	fmt.Fprintf(&b, "\n== outbox_events (%d) ==\n", len(obs))
	for _, o := range obs {
		fmt.Fprintf(&b, "%s %s %s\n", o.topic, o.status, n.apply(o.payload))
	}

	// 2. Every body each channel endpoint received, in arrival order.
	for _, rec := range r.recs {
		got := rec.received()
		fmt.Fprintf(&b, "\n== recorder %s (%d) ==\n", rec.name, len(got))
		for _, body := range got {
			fmt.Fprintf(&b, "%s\n", n.apply(string(body)))
		}
	}

	// 3. The escalation steps each pass enqueued.
	b.WriteString("\n== escalation passes ==\n")
	for _, p := range r.passes {
		fmt.Fprintf(&b, "%s monitor_steps=%d service_steps=%d\n", p.label, p.pass.MonitorSteps, p.pass.ServiceSteps)
		for _, s := range p.steps {
			names := make([]string, 0, len(s.ChannelIDs))
			for _, id := range s.ChannelIDs {
				name, ok := r.chanName[id]
				if !ok {
					name = n.apply(id)
				}
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Fprintf(&b, "  %s step=%d repeat=%t channels=[%s]\n", s.Subject(), s.Step, s.Repeat, strings.Join(names, " "))
		}
	}

	// 4. Each incident as GetIncident returns it.
	b.WriteString("\n== incidents ==\n")
	for _, m := range []domain.Monitor{r.ladder, r.flat} {
		var id string
		if err := r.conn.QueryRow(r.ctx,
			`SELECT id::text FROM incidents WHERE monitor_id = $1 ORDER BY started_at`, m.ID).Scan(&id); err != nil {
			r.t.Fatalf("incident of %s: %v", m.Name, err)
		}
		inc, err := r.st.GetIncident(r.ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		raw, err := json.Marshal(inc)
		if err != nil {
			r.t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s\n", m.Name, n.apply(string(raw)))
	}
	return b.String()
}

// TestP14NoPhoneProjectGolden (P14, spec §11 "Regression"): one project with a webhook, a Slack and a
// disabled Slack channel; `ladder-api` pages through a two-step ladder (0 s → hook; 300 s → slack and
// the disabled slack-off; repeat_last every 600 s), `flat-api` has no ladder and is linked to hook and
// slack. Both go down, the ladder is driven at open, +301 s, +901 s and +1501 s, both recover, and
// every outbox row, channel body, escalation step and incident is compared with the golden.
func TestP14NoPhoneProjectGolden(t *testing.T) {
	r := newP14Rig(t)
	got := r.runScenario()
	if *updateP14 {
		if err := os.MkdirAll(filepath.Dir(p14GoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p14GoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", p14GoldenPath)
		return
	}
	r.compareGolden(got)
}

// runScenario drives the outage the golden records and returns the normalised text.
func (r *p14Rig) runScenario() string {
	t := r.t
	t.Helper()
	r.goDown(r.ladder)
	r.goDown(r.flat)
	r.deliver()

	r.passAt("open", 0)
	r.passAt("+301s", 301)
	r.passAt("+901s", 901)
	r.passAt("+1501s", 1501)

	r.recover(r.ladder)
	r.recover(r.flat)
	r.deliver()
	for _, m := range []domain.Monitor{r.ladder, r.flat} {
		if _, err := r.st.FindOpenAutoIncidentByMonitor(r.ctx, m.ID); err == nil {
			t.Fatalf("%s: the auto-incident is still open after the recovery", m.Name)
		}
	}

	// The two facts the golden exists to keep, asserted on their own as well: a disabled Slack on a
	// step is still enqueued, and dropped only at send.
	for _, rec := range r.recs {
		if rec.name == "slack-off" && len(rec.received()) != 0 {
			t.Fatalf("the disabled channel received %d bodies, want none", len(rec.received()))
		}
	}
	var stepOne bool
	for _, p := range r.passes {
		for _, s := range p.steps {
			if s.Step == 1 && !s.Repeat {
				for _, id := range s.ChannelIDs {
					stepOne = stepOne || r.chanName[id] == "slack-off"
				}
			}
		}
	}
	if !stepOne {
		t.Fatal("the 300 s step was not enqueued with the disabled slack-off among its channels")
	}
	return r.golden()
}

// compareGolden fails unless got is the golden byte for byte.
func (r *p14Rig) compareGolden(got string) {
	t := r.t
	t.Helper()
	want, err := os.ReadFile(p14GoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		t.Fatalf("P14 golden differs (- golden, + now); fix the code, never the golden:\n%s", p14Diff(string(want), got))
	}
}

// p14Diff is a line diff in unified style (no hunks): ` ` common, `-` only in a, `+` only in b.
func p14Diff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			fmt.Fprintf(&out, "  %s\n", x[i])
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			fmt.Fprintf(&out, "+ %s\n", y[j])
			j++
		default:
			fmt.Fprintf(&out, "- %s\n", x[i])
			i++
		}
	}
	return out.String()
}
