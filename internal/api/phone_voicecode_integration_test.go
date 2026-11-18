package api_test

// FR-041a revision 9, §11 row "Verification by a call", on PostgreSQL: the handlers over the REAL
// store and the REAL provider built from the stored settings (telephony.Service), with an HTTP
// transport that answers every request itself — no network. What only the real store can show: a
// code by SMS and a code by a call spend ONE hourly and ONE daily allowance (the limits count codes),
// the code a call spoke verifies, a refused call stores no number and no code, and a code — either
// way — writes no phone_deliveries row, which is also what the storm guard counts (§6.2), so it is
// outside it. Each test runs in a database of its own, created from CERBIX_TEST_DATABASE_DSN and
// dropped at cleanup.

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// providerRequest is one request the provider made: which resource, to whom, and what it carried —
// the SMS text or the call's TwiML.
type providerRequest struct {
	resource string // "Messages.json" | "Calls.json" | the gateway's path
	to       string
	content  string
}

// recordingTransport answers every provider request 201 with a sid, and records it.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []providerRequest
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	form, _ := url.ParseQuery(string(body))
	req := providerRequest{resource: r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], to: form.Get("To")}
	req.content = form.Get("Body")
	if tw := form.Get("Twiml"); tw != "" {
		req.content = tw
	}
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, req)
	rt.mu.Unlock()
	return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"sid":"SM0123456789abcdef0123456789abcdef"}`)), Request: r}, nil
}

func (rt *recordingTransport) requests() []providerRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]providerRequest(nil), rt.reqs...)
}

// storeSettings is telephony.SettingsSource over the store: the group as it is stored now, as
// settings.Service answers CurrentTelephony in production.
type storeSettings struct{ st *store.Store }

func (s storeSettings) Telephony() domain.TelephonySettings {
	ts, _ := s.st.GetTelephonySettings(context.Background())
	return ts
}

func (s storeSettings) CurrentTelephony(ctx context.Context) (domain.TelephonySettings, error) {
	return s.st.GetTelephonySettings(ctx)
}

type voiceCodeDB struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	conn  *pgx.Conn
	tr    *recordingTransport
	h     http.Handler
	admin authz.Principal
}

func newVoiceCodeDB(t *testing.T) *voiceCodeDB {
	t.Helper()
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the verification-by-a-call integration tests")
	}
	u, err := url.Parse(gate)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	adminConn, err := pgx.Connect(ctx, gate)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("cerbix_test_api_voicecode_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminConn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database %s (requires CREATEDB): %v", name, err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := adminConn.Exec(dctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = adminConn.Close(dctx)
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
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	d := &voiceCodeDB{t: t, ctx: ctx, st: st, conn: conn, tr: &recordingTransport{}}
	svc := telephony.NewService(storeSettings{st}, &http.Client{Transport: d.tr})
	d.h = api.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTelephony(svc).Router()
	ga, err := st.CreateLocalUser(ctx, "ga@example.com", "Global Admin", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	d.admin = authz.Principal{UserID: ga.ID, IsGlobalAdmin: true}
	d.saveTelephony("+15005550006") // a number: twilio can place calls
	return d
}

// saveTelephony stores an enabled twilio group whose `from` is from, or generic_http for "".
func (d *voiceCodeDB) saveTelephony(from string) {
	d.t.Helper()
	ts := domain.TelephonySettings{Enabled: true, Provider: domain.TelephonyProviderTwilio,
		Twilio: domain.TelephonyTwilio{AccountSID: "AC0123456789abcdef0123456789abcdef", AuthToken: "tok", From: from}}
	if from == "" {
		ts = domain.TelephonySettings{Enabled: true, Provider: domain.TelephonyProviderGenericHTTP,
			GenericHTTP: domain.TelephonyGenericHTTP{Method: "POST", URL: "https://sms.example/send",
				ContentType: "application/x-www-form-urlencoded", BodyTemplate: "To={{to}}&Body={{text}}"}}
	}
	ts.Normalize()
	if err := d.st.SaveTelephonySettings(d.ctx, ts, domain.TelephonySaveOptions{ConfirmUnroutable: true}); err != nil {
		d.t.Fatalf("save telephony settings: %v", err)
	}
}

func (d *voiceCodeDB) person(email string) authz.Principal {
	d.t.Helper()
	u, err := d.st.CreateLocalUser(d.ctx, email, email, "x", false)
	if err != nil {
		d.t.Fatal(err)
	}
	return authz.Principal{UserID: u.ID}
}

func (d *voiceCodeDB) count(sql string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.conn.QueryRow(d.ctx, sql, args...).Scan(&n); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (d *voiceCodeDB) send(p authz.Principal, method, path, body string, want int) {
	d.t.Helper()
	if rec := do(d.h, p, method, path, body); rec.Code != want {
		d.t.Fatalf("%s %s %s = %d %s, want %d", method, path, body, rec.Code, rec.Body.String(), want)
	}
}

var spokenDigits = regexp.MustCompile(`>(\d), (\d), (\d), (\d), (\d), (\d)\.</Say>`)

// spokenCode is the code the last request — a call — spoke.
func (d *voiceCodeDB) spokenCode() string {
	d.t.Helper()
	reqs := d.tr.requests()
	if len(reqs) == 0 || reqs[len(reqs)-1].resource != "Calls.json" {
		d.t.Fatalf("the last provider request is not a call: %+v", reqs)
	}
	m := spokenDigits.FindStringSubmatch(reqs[len(reqs)-1].content)
	if m == nil {
		d.t.Fatalf("no spoken code in %s", reqs[len(reqs)-1].content)
	}
	return strings.Join(m[1:], "")
}

func (d *voiceCodeDB) resources() []string {
	var out []string
	for _, r := range d.tr.requests() {
		out = append(out, r.resource)
	}
	return out
}

// §11: a code by SMS and a code by a call spend one hourly allowance (3 per requester) and one daily
// allowance (10 per number) — the limits count codes, not messages — and none of them writes a
// phone_deliveries row (so none is counted by the storm guard, which counts those rows).
func TestCodeBySMSAndByCallSpendOneAllowanceOnPostgres(t *testing.T) {
	d := newVoiceCodeDB(t)

	// The hourly allowance: SMS, call, SMS — the fourth code, by a call, is refused.
	ann := d.person("ann@example.com")
	d.send(ann, http.MethodPut, mePhonePath, `{"number":"+905321116869"}`, http.StatusOK)
	d.send(ann, http.MethodPost, meVerifySendPath, `{"via":"voice"}`, http.StatusNoContent)
	d.send(ann, http.MethodPost, meVerifySendPath, `{"via":"sms"}`, http.StatusNoContent)
	assertReason(t, "the fourth code of the hour, by a call", do(d.h, ann, http.MethodPost, meVerifySendPath, `{"via":"voice"}`),
		http.StatusTooManyRequests, "send_limited")
	if got := strings.Join(d.resources(), ","); got != "Messages.json,Calls.json,Messages.json" {
		t.Fatalf("provider requests = %s; want SMS, call, SMS and nothing for the refused fourth", got)
	}
	if n := d.count(`SELECT count(*) FROM phone_verifications WHERE requested_by = $1`, ann.UserID); n != 3 {
		t.Fatalf("ann's code rows = %d, want 3: one per code, whichever way it went", n)
	}

	// The daily allowance: 8 codes of the day to bob's number (older than the hour, so they spend
	// no hourly allowance), then a call and an SMS make ten — the next, by a call, is refused although
	// bob has spent only two of his hour.
	bob := d.person("bob@example.com")
	d.send(bob, http.MethodPut, mePhonePath, `{"number":"+905321117000","via":"voice"}`, http.StatusOK)
	if _, err := d.conn.Exec(d.ctx, `INSERT INTO phone_verifications
		    (subject_kind, subject_id, requested_by, number_hash, code_hash, created_at, expires_at)
		SELECT 'user', user_id, 'earlier-requester', number_hash, 'spent', now() - interval '2 hours', now() - interval '110 minutes'
		  FROM user_phones, generate_series(1, 8) WHERE user_id = $1`, bob.UserID); err != nil {
		t.Fatal(err)
	}
	d.send(bob, http.MethodPost, meVerifySendPath, `{"via":"sms"}`, http.StatusNoContent)
	before := len(d.tr.requests())
	assertReason(t, "the eleventh code of the day, by a call", do(d.h, bob, http.MethodPost, meVerifySendPath, `{"via":"voice"}`),
		http.StatusTooManyRequests, "send_limited")
	if n := d.count(`SELECT count(*) FROM phone_verifications WHERE requested_by = $1 AND created_at > now() - interval '1 hour'`, bob.UserID); n != 2 {
		t.Fatalf("bob's codes of the hour = %d; want 2, so the refusal was the number's daily limit", n)
	}
	if n := len(d.tr.requests()); n != before {
		t.Fatalf("the refused code made %d provider requests", n-before)
	}

	if n := d.count(`SELECT count(*) FROM phone_deliveries`); n != 0 {
		t.Fatalf("phone_deliveries rows = %d after codes by SMS and by call; want none (a code is not an alert)", n)
	}
}

// §11: the code a call spoke verifies as an SMS code does — a person's own number and a `number`
// channel's — and the call is the only request: one call, no SMS.
func TestCodeByCallVerifiesOnPostgres(t *testing.T) {
	d := newVoiceCodeDB(t)
	cat := d.person("cat@example.com")
	d.send(cat, http.MethodPut, mePhonePath, `{"number":"+905321116869","via":"voice"}`, http.StatusOK)
	code := d.spokenCode()
	if reqs := d.tr.requests(); len(reqs) != 1 || reqs[0].to != "+905321116869" {
		t.Fatalf("provider requests = %+v; want the one call to the number", reqs)
	}
	rec := do(d.h, cat, http.MethodPost, mePhoneVerifyPath, `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verified":true`) {
		t.Fatalf("verify the spoken code = %d %s", rec.Code, rec.Body.String())
	}

	org, err := d.st.CreateOrganization(d.ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := d.st.CreateProject(d.ctx, org.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := d.st.CreatePhoneChannel(d.ctx, domain.NotificationChannel{ProjectID: proj.ID, Type: domain.ChannelVoice,
		Name: "duty", Recipient: domain.PhoneRecipientNumber}, "+905321117000", store.AuditActor{ActorUserID: d.admin.UserID, Label: "ga@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	d.send(d.admin, http.MethodPost, "/api/v1/notification-channels/"+ch.ID+"/verify/send", `{"via":"voice"}`, http.StatusNoContent)
	code = d.spokenCode()
	if reqs := d.tr.requests(); len(reqs) != 2 || reqs[1].to != "+905321117000" {
		t.Fatalf("provider requests = %+v; want the channel's one call to its number", reqs)
	}
	rec = do(d.h, d.admin, http.MethodPost, "/api/v1/notification-channels/"+ch.ID+"/verify", `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"number_verified":true`) {
		t.Fatalf("verify the channel's spoken code = %d %s", rec.Code, rec.Body.String())
	}
	if n := d.count(`SELECT count(*) FROM phone_deliveries`); n != 0 {
		t.Fatalf("phone_deliveries rows = %d; want none", n)
	}
}

// §11: while the provider cannot place calls — generic_http, or twilio whose `from` is a Messaging
// Service SID — `via: voice` is refused on each of the three sends with no number stored, no code
// stored and no request made.
func TestCodeByCallRefusedStoresNothingOnPostgres(t *testing.T) {
	d := newVoiceCodeDB(t)
	dan := d.person("dan@example.com")
	d.send(dan, http.MethodPut, mePhonePath, `{"number":"+905321116869"}`, http.StatusOK) // by SMS: stored, 1 code
	org, err := d.st.CreateOrganization(d.ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := d.st.CreateProject(d.ctx, org.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := d.st.CreatePhoneChannel(d.ctx, domain.NotificationChannel{ProjectID: proj.ID, Type: domain.ChannelSMS,
		Name: "duty", Recipient: domain.PhoneRecipientNumber}, "+905321117000", store.AuditActor{ActorUserID: d.admin.UserID, Label: "ga@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	eve := d.person("eve@example.com")
	codes := d.count(`SELECT count(*) FROM phone_verifications`)
	requests := len(d.tr.requests())

	for name, from := range map[string]string{"generic_http": "", "twilio with a Messaging Service SID": "MG0123456789abcdef0123456789abcdef"} {
		d.saveTelephony(from)
		assertReason(t, name+": PUT", do(d.h, eve, http.MethodPut, mePhonePath, `{"number":"+905321118000","via":"voice"}`),
			http.StatusBadRequest, "voice_unsupported_by_provider")
		assertReason(t, name+": verify/send", do(d.h, dan, http.MethodPost, meVerifySendPath, `{"via":"voice"}`),
			http.StatusBadRequest, "voice_unsupported_by_provider")
		assertReason(t, name+": channel verify/send", do(d.h, d.admin, http.MethodPost, "/api/v1/notification-channels/"+ch.ID+"/verify/send", `{"via":"voice"}`),
			http.StatusBadRequest, "voice_unsupported_by_provider")
		if n := d.count(`SELECT count(*) FROM user_phones WHERE user_id = $1`, eve.UserID); n != 0 {
			t.Fatalf("%s: the refused PUT stored the number", name)
		}
		if n := d.count(`SELECT count(*) FROM phone_verifications`); n != codes {
			t.Fatalf("%s: code rows %d, had %d; the refusals stored a code", name, n, codes)
		}
		if n := len(d.tr.requests()); n != requests {
			t.Fatalf("%s: the refusals made %d provider requests", name, n-requests)
		}
	}
}

// §4.2: a verification code is outside the storm guard — it goes, by SMS and by a call, while the
// number's storm slots (storm.per_number_per_hour alert rows of the hour) are full, for a person's
// own number and for a `number` channel's.
func TestCodeGoesWhileTheNumbersStormSlotsAreFullOnPostgres(t *testing.T) {
	d := newVoiceCodeDB(t)
	org, err := d.st.CreateOrganization(d.ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := d.st.CreateProject(d.ctx, org.ID, "prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := d.st.CreatePhoneChannel(d.ctx, domain.NotificationChannel{ProjectID: proj.ID, Type: domain.ChannelSMS,
		Name: "duty", Recipient: domain.PhoneRecipientNumber}, "+905321117000", store.AuditActor{ActorUserID: d.admin.UserID, Label: "ga@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	fill := func(subjectKind string) {
		t.Helper()
		// 20 sent alert rows of the hour on the number the subject's code was asked for: the default
		// per-number bound, so one more ALERT would be storm_limited.
		if _, err := d.conn.Exec(d.ctx, `INSERT INTO phone_deliveries
			    (project_id, channel_id, number_hash, kind, alert_kind, reason)
			SELECT $1, $2, v.number_hash, 'sms', 'monitor_down', 'sent'
			  FROM (SELECT number_hash FROM phone_verifications WHERE subject_kind = $3 LIMIT 1) v, generate_series(1, 20)`,
			proj.ID, ch.ID, subjectKind); err != nil {
			t.Fatal(err)
		}
	}
	chSend := "/api/v1/notification-channels/" + ch.ID + "/verify/send"

	ann := d.person("ann@example.com")
	d.send(ann, http.MethodPut, mePhonePath, `{"number":"+905321116869"}`, http.StatusOK)
	d.send(d.admin, http.MethodPost, chSend, "", http.StatusNoContent)
	fill("user")
	fill("channel")
	if n := d.count(`SELECT count(*) FROM phone_deliveries`); n != 40 {
		t.Fatalf("seeded storm rows = %d, want 40", n)
	}
	before := len(d.tr.requests())
	d.send(ann, http.MethodPost, meVerifySendPath, `{"via":"voice"}`, http.StatusNoContent)
	d.send(ann, http.MethodPost, meVerifySendPath, `{"via":"sms"}`, http.StatusNoContent)
	d.send(d.admin, http.MethodPost, chSend, `{"via":"voice"}`, http.StatusNoContent)
	d.send(d.admin, http.MethodPost, chSend, `{"via":"sms"}`, http.StatusNoContent)
	if n := len(d.tr.requests()) - before; n != 4 {
		t.Fatalf("a full storm let %d of 4 codes through", n)
	}
}
