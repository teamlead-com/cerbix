package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestMessage(t *testing.T) {
	m := domain.Monitor{Name: "API"}
	if got := Message(m, false); !strings.Contains(got, "DOWN") || !strings.Contains(got, "API") {
		t.Fatalf("down message = %q", got)
	}
	if got := Message(m, true); !strings.Contains(got, "recovered") {
		t.Fatalf("up message = %q", got)
	}
}

func TestRenderPerType(t *testing.T) {
	mon := domain.Monitor{ID: "m1", Name: "API", ProjectID: "p1"}

	// Slack → {text}.
	_, body, ct, err := render(domain.NotificationChannel{Type: domain.ChannelSlack, Config: map[string]string{"url": "http://s"}}, mon, false)
	if err != nil || ct != "application/json" {
		t.Fatalf("slack render: %v", err)
	}
	var slack map[string]string
	_ = json.Unmarshal(body, &slack)
	if !strings.Contains(slack["text"], "DOWN") {
		t.Fatalf("slack body = %s", body)
	}

	// Telegram → api URL + chat_id.
	url, body, _, err := render(domain.NotificationChannel{Type: domain.ChannelTelegram, Config: map[string]string{"bot_token": "BOT", "chat_id": "42"}}, mon, true)
	if err != nil || !strings.Contains(url, "/botBOT/sendMessage") {
		t.Fatalf("telegram url = %q err %v", url, err)
	}
	var tg map[string]string
	_ = json.Unmarshal(body, &tg)
	if tg["chat_id"] != "42" {
		t.Fatalf("telegram body = %s", body)
	}

	// Webhook → structured JSON.
	_, body, _, err = render(domain.NotificationChannel{Type: domain.ChannelWebhook, Config: map[string]string{"url": "http://w"}}, mon, false)
	if err != nil || !strings.Contains(string(body), "monitor.transition") || !strings.Contains(string(body), "m1") {
		t.Fatalf("webhook body = %s err %v", body, err)
	}
}

type fakeStore struct{ channels []domain.NotificationChannel }

func (f *fakeStore) ListEnabledChannelsForMonitor(_ context.Context, _ string) ([]domain.NotificationChannel, error) {
	return f.channels, nil
}
func (f *fakeStore) ListEnabledChannelsByProject(_ context.Context, _ string) ([]domain.NotificationChannel, error) {
	return f.channels, nil
}
func (f *fakeStore) EnabledChannelsByIDs(_ context.Context, _ []string) ([]domain.NotificationChannel, error) {
	return f.channels, nil
}

func TestEmailDelivery(t *testing.T) {
	type sent struct {
		addr, from string
		to         []string
		msg        string
	}
	captured := make(chan sent, 1)
	orig := sendMailFunc
	sendMailFunc = func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
		captured <- sent{addr: addr, from: from, to: to, msg: string(msg)}
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })

	store := &fakeStore{channels: []domain.NotificationChannel{{
		ID: "ec", Type: domain.ChannelEmail, Enabled: true,
		Config: map[string]string{"smtp_host": "mail", "smtp_port": "1025", "from": "cerbix@x", "to": "a@x, b@x"},
	}}}
	d := New(store, nil)

	if err := d.Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	s := <-captured
	if s.addr != "mail:1025" || s.from != "cerbix@x" {
		t.Fatalf("smtp addr/from = %q / %q", s.addr, s.from)
	}
	if len(s.to) != 2 || s.to[0] != "a@x" || s.to[1] != "b@x" {
		t.Fatalf("recipients = %v, want [a@x b@x]", s.to)
	}
	if !strings.Contains(s.msg, "Subject: ") || !strings.Contains(s.msg, "DOWN") {
		t.Fatalf("message = %q", s.msg)
	}
}

func TestDeliveryPosts(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &fakeStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Config: map[string]string{"url": srv.URL}, Enabled: true},
	}}
	d := New(store, srv.Client())

	if err := d.Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	b := <-got
	if !strings.Contains(string(b), "DOWN") {
		t.Fatalf("delivered body = %s", b)
	}
}

// TestDeliverReportsFailure confirms a non-2xx channel surfaces an error so the
// outbox retries.
func TestDeliverReportsFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	store := &fakeStore{channels: []domain.NotificationChannel{
		{ID: "nc1", Type: domain.ChannelWebhook, Config: map[string]string{"url": bad.URL}, Enabled: true},
	}}
	if err := New(store, bad.Client()).Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false); err == nil {
		t.Fatal("non-2xx channel should surface an error")
	}
}
