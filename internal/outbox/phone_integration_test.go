package outbox

// FR-041a on PostgreSQL, end to end through the outbox worker: the real claim (with the event's
// created_at), the real dispatcher wired for phone over the real store — rule-1 verdicts, verified
// numbers, D33's reservation, D32's attachment, the Phone panel read — and the fake provider. Each test
// runs in a database of its own, created from CERBIX_TEST_DATABASE_DSN and dropped at cleanup.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

type phoneDB struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	conn     *pgx.Conn
	project  string
	actor    store.AuditActor
	provider *telephony.FakeProvider
	metrics  *countingPhoneMetrics
	w        *Worker
	slackURL string
	slackBad atomic.Int32 // the next n Slack posts answer 500
	slackOK  atomic.Int32
}

func newPhoneDB(t *testing.T) *phoneDB {
	t.Helper()
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the phone outbox integration tests")
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
	name := fmt.Sprintf("cerbix_test_outbox_phone_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	st.WithCipher(c)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	p := &phoneDB{t: t, ctx: ctx, st: st, conn: conn, project: proj.ID,
		actor:    store.AuditActor{ViaToken: true, Label: "token:phone-test"},
		provider: telephony.NewFakeProvider(), metrics: &countingPhoneMetrics{}}
	p.telephony(true, domain.TelephonyProviderTwilio)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if p.slackBad.Load() > 0 {
			p.slackBad.Add(-1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		p.slackOK.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p.slackURL = srv.URL

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tel := &rigTelephony{provider: p.provider}
	tel.ts = domain.TelephonySettings{Configured: true, Enabled: true, Provider: domain.TelephonyProviderTwilio}
	tel.ts.Normalize()
	d := notify.New(st, nil).WithPhone(st, tel, p.metrics, logger)
	p.w = New(st, nil, d, &fakeMetrics{}, logger).
		WithPhoneLinks(func() string { return "https://cerbix.example" }, func() string { return "" })
	p.w.phone.refInterval = 10 * time.Millisecond
	return p
}

func (p *phoneDB) exec(sql string, args ...any) {
	p.t.Helper()
	if _, err := p.conn.Exec(p.ctx, sql, args...); err != nil {
		p.t.Fatalf("%s: %v", sql, err)
	}
}

// telephony sets the typed columns D31 reads, as a settings save leaves them.
func (p *phoneDB) telephony(enabled bool, provider string) {
	p.t.Helper()
	p.exec(`INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING`)
	p.exec(`UPDATE instance_settings SET telephony_enabled = $1, telephony_provider = $2 WHERE id = true`, enabled, provider)
}

func (p *phoneDB) storm(perIncident int) {
	p.t.Helper()
	p.exec(`UPDATE instance_settings SET telephony = jsonb_set(telephony, '{storm}',
		jsonb_build_object('per_incident_per_hour', $1::int, 'per_number_per_hour', 0)) WHERE id = true`, perIncident)
}

// dutySMS is a verified, enabled `number` sms channel.
func (p *phoneDB) dutySMS(number string) string {
	p.t.Helper()
	ch, err := p.st.CreatePhoneChannel(p.ctx, domain.NotificationChannel{ProjectID: p.project, Type: domain.ChannelSMS,
		Name: "duty " + number, Recipient: domain.PhoneRecipientNumber, Enabled: true}, number, p.actor)
	if err != nil {
		p.t.Fatalf("create duty channel: %v", err)
	}
	_, code, err := p.st.SendChannelVerificationCode(p.ctx, ch.ID, "apitoken:phone-test")
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.st.VerifyChannelNumber(p.ctx, ch.ID, code, p.actor); err != nil {
		p.t.Fatal(err)
	}
	enabled := true
	if _, err := p.st.UpdatePhoneChannel(p.ctx, ch.ID, store.PhoneChannelPatch{Enabled: &enabled}, p.actor, nil); err != nil {
		p.t.Fatal(err)
	}
	return ch.ID
}

func (p *phoneDB) slack() string {
	p.t.Helper()
	ch, err := p.st.CreateNotificationChannel(p.ctx, domain.NotificationChannel{ProjectID: p.project,
		Type: domain.ChannelSlack, Name: "slack", Enabled: true, Config: map[string]string{"url": p.slackURL}})
	if err != nil {
		p.t.Fatal(err)
	}
	return ch.ID
}

func (p *phoneDB) monitor(name string, autoIncident bool, seq int64, channels ...string) domain.Monitor {
	p.t.Helper()
	m, err := p.st.CreateMonitorByPrincipal(p.ctx, domain.Monitor{ProjectID: p.project, Name: name, Type: domain.MonitorTCP,
		Target: "10.0.0.1:80", IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 1, RecoveryThreshold: 1,
		Enabled: true, AutoIncident: autoIncident}, p.actor)
	if err != nil {
		p.t.Fatalf("monitor: %v", err)
	}
	p.exec(`UPDATE monitors SET state_sequence = $2, status = 'down' WHERE id = $1`, m.ID, seq)
	for _, ch := range channels {
		if err := p.st.LinkMonitorChannel(p.ctx, m.ID, ch); err != nil {
			p.t.Fatal(err)
		}
	}
	m.StateSequence = seq
	return m
}

// enqueue writes an outbox row as the producers do, created `age` ago, and returns its id.
func (p *phoneDB) enqueue(topic string, payload any, age time.Duration) string {
	p.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	var id string
	if err := p.conn.QueryRow(p.ctx, `INSERT INTO outbox_events (topic, payload, status, fenced, created_at, updated_at, next_attempt_at)
		VALUES ($1, $2, CASE WHEN $3 THEN 'pending_fenced' ELSE 'pending' END, $3, now() - $4::interval, now(), now())
		RETURNING id::text`, topic, raw, domain.FencedTopic(topic), fmt.Sprintf("%d milliseconds", age.Milliseconds())).Scan(&id); err != nil {
		p.t.Fatalf("enqueue: %v", err)
	}
	return id
}

// deliver makes the event due and runs one drain.
func (p *phoneDB) deliver(eventID string) {
	p.t.Helper()
	p.exec(`UPDATE outbox_events SET next_attempt_at = now() WHERE id = $1 AND status IN ('pending', 'pending_fenced')`, eventID)
	p.w.drain(p.ctx)
}

func (p *phoneDB) status(eventID string) (status string, attempts int) {
	p.t.Helper()
	if err := p.conn.QueryRow(p.ctx, `SELECT status, attempts FROM outbox_events WHERE id = $1`, eventID).
		Scan(&status, &attempts); err != nil {
		p.t.Fatal(err)
	}
	return status, attempts
}

type phoneRow struct {
	id, reason, incident string
	notes                []string
	attempts             int
}

func (p *phoneDB) rows(eventID string) []phoneRow {
	p.t.Helper()
	rs, err := p.conn.Query(p.ctx, `SELECT id::text, reason, COALESCE(incident_id::text, ''), notes, attempts
		  FROM phone_deliveries WHERE outbox_event_id = $1 ORDER BY created_at, id`, eventID)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rs.Close()
	var out []phoneRow
	for rs.Next() {
		var r phoneRow
		if err := rs.Scan(&r.id, &r.reason, &r.incident, &r.notes, &r.attempts); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// countingPhoneMetrics is notify.PhoneMetrics as "kind|provider|reason" counts.
type countingPhoneMetrics struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *countingPhoneMetrics) RecordPhoneDelivery(kind, provider, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[kind+"|"+provider+"|"+reason]++
}
func (m *countingPhoneMetrics) RecordPhoneQuotaEvent(string, string, string) {}

func (m *countingPhoneMetrics) get(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[key]
}

func downEvent(m domain.Monitor) domain.MonitorTransition {
	return domain.MonitorTransition{MonitorID: m.ID, Prev: domain.StatusUp, Cur: domain.StatusDown, Seq: m.StateSequence}
}

func upEvent(m domain.Monitor) domain.MonitorTransition {
	return domain.MonitorTransition{MonitorID: m.ID, Prev: domain.StatusDown, Cur: domain.StatusUp, Seq: m.StateSequence}
}

// §11 "Outbox — retries" (P16, D19, D33): an event retried five times with one phone and one text
// channel makes one accepted provider call; a `sending` row met at 10 s and at 30 s is not called and
// fails the event, and at 70 s is closed as outcome_unknown; provider_unreachable is called again on
// the same row.
func TestPhoneDeliveryOnPostgresRetries(t *testing.T) {
	p := newPhoneDB(t)
	duty := p.dutySMS("+447700900123")
	m := p.monitor("api", false, 3, duty, p.slack())

	// A webhook-style failure on the text channel, four times: the outbox retries the WHOLE event.
	p.slackBad.Store(4)
	e1 := p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0)
	for i := 1; i <= 5; i++ {
		p.deliver(e1)
		status, attempts := p.status(e1)
		if want := map[bool]string{true: "delivered", false: "pending"}[i == 5]; status != want || attempts != i {
			t.Fatalf("attempt %d: status %s attempts %d; want %s", i, status, attempts, want)
		}
	}
	if n := len(p.provider.Calls()); n != 1 {
		t.Fatalf("provider calls after five attempts = %d; want one accepted call", n)
	}
	if rows := p.rows(e1); len(rows) != 1 || rows[0].reason != domain.PhoneReasonSent || rows[0].attempts != 1 {
		t.Fatalf("rows %+v; want one sent row on its first attempt", rows)
	}

	// A crashed attempt left a `sending` row (reserved, provider maybe called, answer never stored).
	e2 := p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0)
	res, err := p.st.ReservePhoneDelivery(p.ctx, domain.PhoneReserveInput{ProjectID: p.project, ChannelID: duty,
		OutboxEventID: e2, Kind: domain.ChannelSMS, AlertKind: domain.PhoneAlertMonitorDown, MonitorID: m.ID,
		MonitorSeq: m.StateSequence, Number: "+447700900123"})
	if err != nil || res.Status != domain.PhoneReservationProceed {
		t.Fatalf("reserve: %+v, %v", res, err)
	}
	calls := len(p.provider.Calls())
	for _, age := range []int{10, 30} {
		p.exec(`UPDATE phone_deliveries SET updated_at = now() - make_interval(secs => $2) WHERE id = $1`, res.ID, age)
		p.deliver(e2)
		if status, _ := p.status(e2); status != "pending" {
			t.Fatalf("a %d s old sending row: event %s; want it failed for a retry", age, status)
		}
		if rows := p.rows(e2); len(rows) != 1 || rows[0].reason != domain.PhoneReasonSending {
			t.Fatalf("a %d s old sending row: rows %+v; want it left sending", age, rows)
		}
	}
	p.exec(`UPDATE phone_deliveries SET updated_at = now() - interval '70 seconds' WHERE id = $1`, res.ID)
	p.deliver(e2)
	if status, _ := p.status(e2); status != "delivered" {
		t.Fatalf("a 70 s old sending row: event %s; want delivered", status)
	}
	if rows := p.rows(e2); len(rows) != 1 || rows[0].reason != domain.PhoneReasonOutcomeUnknown {
		t.Fatalf("rows %+v; want the row closed as outcome_unknown", rows)
	}
	if n := len(p.provider.Calls()); n != calls {
		t.Fatalf("provider calls %d; want none for a row left sending", n-calls)
	}

	// provider_unreachable: the same row is armed again and called again.
	p.provider.ScriptSMS(telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503})
	e3 := p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0)
	p.deliver(e3)
	first := p.rows(e3)
	if status, _ := p.status(e3); status != "pending" || len(first) != 1 || first[0].reason != domain.PhoneReasonProviderUnreachable {
		t.Fatalf("unreachable: event %s rows %+v", status, first)
	}
	p.deliver(e3)
	again := p.rows(e3)
	if status, _ := p.status(e3); status != "delivered" || len(again) != 1 || again[0].id != first[0].id ||
		again[0].reason != domain.PhoneReasonSent || again[0].attempts != 2 {
		t.Fatalf("after the retry: event %s rows %+v; want the same row sent on its second attempt", status, again)
	}
}

// §11 "Trace" (P7): every not-sent outcome of part (a) has its row, its metric count and — the alert
// belonging to an incident — its line in that incident's Phone panel.
func TestPhoneDeliveryOnPostgresTrace(t *testing.T) {
	p := newPhoneDB(t)
	duty := p.dutySMS("+447700900123")
	m := p.monitor("api", true, 1)
	inc, err := p.st.CreateIncidentBySystem(p.ctx, domain.Incident{ProjectID: p.project, MonitorID: m.ID,
		Title: "api down", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}, "down", "system")
	if err != nil {
		t.Fatal(err)
	}
	// A user sms channel whose user has no verified number; a user voice channel whose user has one.
	user := func(email string) string {
		u, err := p.st.CreateLocalUser(p.ctx, email, "Person "+email, "x", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.st.CreateProjectMembership(p.ctx, p.project, u.ID, domain.RoleViewer); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	carol := user("carol@example.com")
	carolSMS, err := p.st.CreatePhoneChannel(p.ctx, domain.NotificationChannel{ProjectID: p.project, Type: domain.ChannelSMS,
		Name: "carol", Recipient: domain.PhoneRecipientUser, RecipientUserID: carol, Enabled: true}, "", p.actor)
	if err != nil {
		t.Fatal(err)
	}
	alice := user("alice@example.com")
	_, code, err := p.st.SetUserPhone(p.ctx, alice, "+905551110001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.st.VerifyUserPhone(p.ctx, alice, code); err != nil {
		t.Fatal(err)
	}
	aliceVoice, err := p.st.CreatePhoneChannel(p.ctx, domain.NotificationChannel{ProjectID: p.project, Type: domain.ChannelVoice,
		Name: "alice", Recipient: domain.PhoneRecipientUser, RecipientUserID: alice, Enabled: true}, "", p.actor)
	if err != nil {
		t.Fatal(err)
	}

	step := 0
	stepEvent := func(channels ...string) string {
		step++
		return p.enqueue(domain.TopicEscalationStep, domain.EscalationStepAlert{IncidentID: inc.ID, MonitorID: m.ID,
			MonitorName: m.Name, SubjectName: m.Name, Step: step, ChannelIDs: channels}, 0)
	}
	type want struct {
		event, channel, reason, metric string
	}
	var wants []want

	// not_deliverable (and a sent control, which fills the incident's storm slot used below).
	e := stepEvent(duty, carolSMS.ID)
	p.deliver(e)
	wants = append(wants, want{e, carolSMS.ID, domain.PhoneReasonNotDeliverable, "sms|fake|not_deliverable"})

	// storm_limited: one message per number per incident per hour, already spent by the control.
	p.storm(1)
	e = stepEvent(duty)
	p.deliver(e)
	wants = append(wants, want{e, duty, domain.PhoneReasonStormLimited, "sms|fake|storm_limited"})
	p.storm(0)

	// provider_rejected and provider_unreachable.
	p.provider.ScriptSMS(telephony.Result{Outcome: telephony.Rejected, HTTPStatus: 400, Code: "21211"})
	e = stepEvent(duty)
	p.deliver(e)
	wants = append(wants, want{e, duty, domain.PhoneReasonProviderRejected, "sms|fake|provider_rejected"})
	p.provider.ScriptSMS(telephony.Result{Outcome: telephony.Unreachable, HTTPStatus: 503})
	e = stepEvent(duty)
	p.deliver(e)
	wants = append(wants, want{e, duty, domain.PhoneReasonProviderUnreachable, "sms|fake|provider_unreachable"})

	// outcome_unknown: a crashed attempt's row, met by the retry after 60 s.
	e = stepEvent(duty)
	res, err := p.st.ReservePhoneDelivery(p.ctx, domain.PhoneReserveInput{ProjectID: p.project, ChannelID: duty,
		OutboxEventID: e, Kind: domain.ChannelSMS, AlertKind: domain.PhoneAlertEscalationStep, IncidentID: inc.ID,
		MonitorID: m.ID, Number: "+447700900123"})
	if err != nil {
		t.Fatal(err)
	}
	p.exec(`UPDATE phone_deliveries SET updated_at = now() - interval '70 seconds' WHERE id = $1`, res.ID)
	p.deliver(e)
	wants = append(wants, want{e, duty, domain.PhoneReasonOutcomeUnknown, "sms|fake|outcome_unknown"})

	// voice_unsupported_by_provider: the snapshot was taken under Twilio; the send is under generic_http.
	p.telephony(true, domain.TelephonyProviderGenericHTTP)
	e = stepEvent(aliceVoice.ID)
	p.deliver(e)
	wants = append(wants, want{e, aliceVoice.ID, domain.PhoneReasonVoiceUnsupportedByProvider, "voice|twilio|voice_unsupported_by_provider"})

	// ... and under Twilio whose `from` is a Messaging Service SID, which places no call (FR-041a review
	// P1-1, D3): the same reason, decided at send by the database.
	p.telephony(true, domain.TelephonyProviderTwilio)
	p.exec(`UPDATE instance_settings SET telephony = jsonb_set(telephony, '{twilio}',
		jsonb_build_object('from', 'MG0123456789abcdef0123456789abcdef')) WHERE id = true`)
	e = stepEvent(aliceVoice.ID)
	p.deliver(e)
	wants = append(wants, want{e, aliceVoice.ID, domain.PhoneReasonVoiceUnsupportedByProvider, "voice|twilio|voice_unsupported_by_provider"})

	// telephony_disabled.
	p.telephony(false, domain.TelephonyProviderTwilio)
	e = stepEvent(duty)
	p.deliver(e)
	wants = append(wants, want{e, duty, domain.PhoneReasonTelephonyDisabled, "sms|twilio|telephony_disabled"})

	panel, err := p.st.ListIncidentPhoneDeliveries(p.ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wants {
		var row *phoneRow
		for _, r := range p.rows(w.event) {
			if r.reason == w.reason {
				row = &r
			}
		}
		if row == nil {
			t.Errorf("%s: no row for event %s; rows %+v", w.reason, w.event, p.rows(w.event))
			continue
		}
		if row.incident != inc.ID {
			t.Errorf("%s: row incident %q; want %s", w.reason, row.incident, inc.ID)
		}
		if p.metrics.get(w.metric) < 1 {
			t.Errorf("%s: metric %s not counted (%v)", w.reason, w.metric, p.metrics.n)
		}
		inPanel := false
		for _, v := range panel {
			if v.ID == row.id && v.Reason == w.reason {
				inPanel = true
			}
		}
		if !inPanel {
			t.Errorf("%s: row %s is not in the incident's Phone panel", w.reason, row.id)
		}
	}
	// None of them is reported as sent (P6): the only sent row is the control.
	sent := 0
	for _, v := range panel {
		if v.Reason == domain.PhoneReasonSent {
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("%d sent rows in the panel; want the one control", sent)
	}
}

// §11 "Outbox — monitor race" on PostgreSQL (P21, D32, P14): a DOWN delivered before the reconciler
// opens the incident and one delivered after make the same send, the link present only in the second,
// `no_incident_ref` in the first — and the first row is attached to the incident when it opens. A
// recovery delivered before and after the reconciler resolves the incident links the same incident; a
// recovery whose only incident was resolved before it was enqueued links none, noted only with
// auto_incident on. The stored events' payload bytes are not touched.
func TestPhoneDeliveryOnPostgresMonitorRace(t *testing.T) {
	p := newPhoneDB(t)
	duty := p.dutySMS("+447700900123")
	m := p.monitor("api", true, 4, duty)
	payloadOf := func(id string) []byte {
		var raw []byte
		if err := p.conn.QueryRow(p.ctx, `SELECT payload FROM outbox_events WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	lastText := func() string {
		calls := p.provider.Calls()
		if len(calls) == 0 {
			t.Fatal("no SMS was sent")
		}
		return calls[len(calls)-1].Body
	}

	before := p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0)
	raw := payloadOf(before)
	p.deliver(before)
	textBefore := lastText()
	rows := p.rows(before)
	if len(rows) != 1 || rows[0].reason != domain.PhoneReasonSent || rows[0].incident != "" ||
		strings.Join(rows[0].notes, ",") != domain.PhoneNoteNoIncidentRef || strings.Contains(textBefore, "incident") {
		t.Fatalf("before the incident: text %q rows %+v", textBefore, rows)
	}
	if string(payloadOf(before)) != string(raw) {
		t.Fatal("the stored payload changed (P14)")
	}

	inc, err := p.st.CreateIncidentBySystem(p.ctx, domain.Incident{ProjectID: p.project, MonitorID: m.ID,
		Title: "api down", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}, "down", "system")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := p.st.IncidentRef(p.ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows := p.rows(before); rows[0].incident != inc.ID {
		t.Fatalf("the earlier row was not attached when the incident opened: %+v", rows)
	}

	after := p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0)
	p.deliver(after)
	textAfter := lastText()
	if want := textBefore + " - incident " + ref + " https://cerbix.example/i/" + ref; textAfter != want {
		t.Fatalf("after the incident opened: %q; want the same send with the link, %q", textAfter, want)
	}
	if rows := p.rows(after); len(rows) != 1 || rows[0].incident != inc.ID || len(rows[0].notes) != 0 {
		t.Fatalf("after: rows %+v", rows)
	}

	// Recovery before the reconciler resolves (open) and after (resolved after the event was enqueued).
	upOpen := p.enqueue(domain.TopicMonitorTransition, upEvent(m), 0)
	p.deliver(upOpen)
	textOpen := lastText()
	upResolved := p.enqueue(domain.TopicMonitorTransition, upEvent(m), 2*time.Second)
	p.exec(`UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, inc.ID)
	p.deliver(upResolved)
	textResolved := lastText()
	for _, text := range []string{textOpen, textResolved} {
		if !strings.Contains(text, "RECOVERED") || !strings.Contains(text, " - incident "+ref) {
			t.Fatalf("recovery %q; want the incident %s linked", text, ref)
		}
	}
	for _, e := range []string{upOpen, upResolved} {
		if rows := p.rows(e); len(rows) != 1 || rows[0].incident != inc.ID {
			t.Fatalf("recovery rows %+v; want the incident", rows)
		}
	}

	// Only an older outage, resolved before this recovery was enqueued: no link; noted with auto_incident.
	old := p.enqueue(domain.TopicMonitorTransition, upEvent(m), 0)
	p.exec(`UPDATE incidents SET resolved_at = now() - interval '1 hour' WHERE id = $1`, inc.ID)
	p.deliver(old)
	if text := lastText(); strings.Contains(text, "incident") {
		t.Fatalf("a recovery linked an older outage: %q", text)
	}
	if rows := p.rows(old); len(rows) != 1 || rows[0].incident != "" || strings.Join(rows[0].notes, ",") != domain.PhoneNoteNoIncidentRef {
		t.Fatalf("older outage, auto_incident on: rows %+v", rows)
	}
	p.exec(`UPDATE monitors SET auto_incident = false WHERE id = $1`, m.ID)
	quiet := p.enqueue(domain.TopicMonitorTransition, upEvent(m), 0)
	p.deliver(quiet)
	if rows := p.rows(quiet); len(rows) != 1 || len(rows[0].notes) != 0 {
		t.Fatalf("older outage, auto_incident off: rows %+v; want no note", rows)
	}
}

// Task 12 re-review N-1 on PostgreSQL: a Slack channel whose config cannot be decrypted fails the text
// half's read of the whole list. The phone channel beside it is still phoned — once, across retries
// (D19) — while the event fails for its text: a monitor's DOWN, and an escalation step.
func TestPhoneDeliveryOnPostgresUndecryptableTextChannel(t *testing.T) {
	p := newPhoneDB(t)
	duty := p.dutySMS("+447700900123")
	broken := p.slack()
	p.exec(`UPDATE notification_channels SET config = '{"url":"enc:v1:!!not-a-ciphertext"}' WHERE id = $1`, broken)
	m := p.monitor("api", false, 2, broken, duty)
	inc, err := p.st.CreateIncidentBySystem(p.ctx, domain.Incident{ProjectID: p.project, MonitorID: m.ID,
		Title: "api down", Status: domain.IncidentInvestigating, Source: domain.SourceAuto}, "down", "system")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		enqueue func() string
	}{
		{"monitor DOWN", func() string { return p.enqueue(domain.TopicMonitorTransition, downEvent(m), 0) }},
		{"escalation step", func() string {
			return p.enqueue(domain.TopicEscalationStep, domain.EscalationStepAlert{IncidentID: inc.ID,
				MonitorID: m.ID, MonitorName: m.Name, SubjectName: m.Name, Step: 1, ChannelIDs: []string{broken, duty}}, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := c.enqueue()
			before := len(p.provider.Calls())
			for attempt := 1; attempt <= 3; attempt++ {
				p.deliver(id)
				if status, attempts := p.status(id); status != "pending" || attempts != attempt {
					t.Fatalf("attempt %d: status %s attempts %d; want the event pending for its text", attempt, status, attempts)
				}
			}
			if n := len(p.provider.Calls()) - before; n != 1 {
				t.Fatalf("provider calls over three attempts = %d; want the one page", n)
			}
			if rows := p.rows(id); len(rows) != 1 || rows[0].reason != domain.PhoneReasonSent {
				t.Fatalf("rows %+v; want one sent row", rows)
			}
			if p.slackOK.Load() != 0 {
				t.Fatal("the undecryptable Slack channel was posted to")
			}
		})
	}
}
