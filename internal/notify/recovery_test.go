package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// recordingEndpoint captures every JSON body POSTed to it, by path.
type recordingEndpoint struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func newRecordingEndpoint(t *testing.T) (*recordingEndpoint, *httptest.Server) {
	t.Helper()
	r := &recordingEndpoint{bodies: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("non-JSON body on %s: %q", req.URL.Path, raw)
		}
		r.mu.Lock()
		r.bodies[req.URL.Path] = append(r.bodies[req.URL.Path], m)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func (r *recordingEndpoint) one(t *testing.T, path string) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies[path]) != 1 {
		t.Fatalf("%s received %d bodies, want 1: %v", path, len(r.bodies[path]), r.bodies)
	}
	return r.bodies[path][0]
}

// deliverToEveryChannel sends one transition to a webhook, a Slack, a Telegram and an email channel
// and returns what each received.
func deliverToEveryChannel(t *testing.T, mon domain.Monitor, up bool, recovery *domain.RecoverySnapshot) (
	webhook, slack, telegram map[string]any, subject, body string,
) {
	t.Helper()
	rec, srv := newRecordingEndpoint(t)
	origAPI := telegramAPI
	telegramAPI = srv.URL + "/tg"
	t.Cleanup(func() { telegramAPI = origAPI })
	sent := captureChannelMail(t)
	d := New(&fakeStore{channels: []domain.NotificationChannel{
		{ID: "w", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": srv.URL + "/hook"}},
		{ID: "s", Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": srv.URL + "/slack"}},
		{ID: "t", Type: domain.ChannelTelegram, Enabled: true, Config: map[string]string{"bot_token": "BOT", "chat_id": "42"}},
		emailChannel("cerbix@example.com", "ops@example.com"),
	}}, nil)
	if err := d.Deliver(context.Background(), mon, up, recovery); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !sent.called {
		t.Fatal("no mail was sent")
	}
	m, err := mail.ReadMessage(bytes.NewReader(sent.msg))
	if err != nil {
		t.Fatalf("parse mail: %v\n%s", err, sent.msg)
	}
	subject, err = new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(m.Body)
	return rec.one(t, "/hook"), rec.one(t, "/slack"), rec.one(t, "/tg/botBOT/sendMessage"), subject,
		strings.TrimSuffix(b.String(), "\r\n")
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// webhookKeys is the monitor.transition contract as it was before FR-043: §4.6 adds no key.
var webhookKeys = []string{"event", "monitor", "monitor_id", "project_id", "text", "up"}

// TestRecoveryNotificationCarriesTheSentence (FR-043 §4.6): a confirmed run of N > 1 appends the
// snapshot's sentence after ONE space to the notification text on every channel — Slack, Telegram,
// the webhook's `text` and the email body. The webhook keeps `"event":"monitor.transition"` and
// `"up":true` with no key added, and the email Subject stays today's string: the transition still
// goes through Deliver, not DeliverText, which would change all three.
func TestRecoveryNotificationCarriesTheSentence(t *testing.T) {
	mon := domain.Monitor{ID: "m1", Name: "API", ProjectID: "p1", RecoveryThreshold: 9}
	firstOK := time.Date(2026, 11, 16, 18, 4, 5, 750_000_000, time.FixedZone("UTC+4", 4*3600))
	snap := &domain.RecoverySnapshot{Run: 3, Threshold: 3, FirstOK: firstOK}
	today := "🟢 API recovered — it is back UP."
	want := today + " Recovered: 3/3 consecutive checks passed; first OK at 2026-11-16T14:04:05Z"

	webhook, slack, telegram, subject, body := deliverToEveryChannel(t, mon, true, snap)

	if webhook["text"] != want {
		t.Errorf("webhook text = %q, want %q", webhook["text"], want)
	}
	if webhook["event"] != "monitor.transition" || webhook["up"] != true {
		t.Errorf("webhook event/up = %v/%v, want monitor.transition/true", webhook["event"], webhook["up"])
	}
	if got := keysOf(webhook); !reflect.DeepEqual(got, webhookKeys) {
		t.Errorf("webhook keys = %v, want exactly %v", got, webhookKeys)
	}
	if slack["text"] != want {
		t.Errorf("slack text = %q, want %q", slack["text"], want)
	}
	if telegram["text"] != want || telegram["chat_id"] != "42" {
		t.Errorf("telegram body = %v, want text %q", telegram, want)
	}
	if subject != today {
		t.Errorf("email subject = %q, want today's %q", subject, today)
	}
	if body != want {
		t.Errorf("email body = %q, want %q", body, want)
	}
}

// TestRecoveryNotificationWithoutARunIsToday (I1, §4.6): no snapshot — every event enqueued before
// the upgrade — and a run of one render every text byte for byte as before, the email subject and
// body included. A DOWN is never touched.
func TestRecoveryNotificationWithoutARunIsToday(t *testing.T) {
	mon := domain.Monitor{ID: "m1", Name: "API", ProjectID: "p1", RecoveryThreshold: 1}
	firstOK := time.Date(2026, 11, 16, 14, 4, 5, 0, time.UTC)
	cases := []struct {
		name     string
		up       bool
		recovery *domain.RecoverySnapshot
		want     string
	}{
		{"legacy up, no snapshot", true, nil, "🟢 API recovered — it is back UP."},
		{"up after a run of one", true, &domain.RecoverySnapshot{Run: 1, Threshold: 1, FirstOK: firstOK}, "🟢 API recovered — it is back UP."},
		{"down", false, nil, "🔴 API is DOWN."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			webhook, slack, telegram, subject, body := deliverToEveryChannel(t, mon, c.up, c.recovery)
			for name, got := range map[string]any{"webhook": webhook["text"], "slack": slack["text"],
				"telegram": telegram["text"], "email subject": subject, "email body": body} {
				if got != c.want {
					t.Errorf("%s = %q, want %q", name, got, c.want)
				}
			}
			if webhook["up"] != c.up || webhook["event"] != "monitor.transition" {
				t.Errorf("webhook event/up = %v/%v", webhook["event"], webhook["up"])
			}
			if got := keysOf(webhook); !reflect.DeepEqual(got, webhookKeys) {
				t.Errorf("webhook keys = %v, want exactly %v", got, webhookKeys)
			}
		})
	}
}
