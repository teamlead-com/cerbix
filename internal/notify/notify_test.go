package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
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

type fakeStore struct {
	channels []domain.NotificationChannel
	byID     map[string]domain.NotificationChannel
}

func (f *fakeStore) ListEnabledChannelsForMonitor(_ context.Context, _ string) ([]domain.NotificationChannel, error) {
	return f.channels, nil
}
func (f *fakeStore) ListEnabledChannelsByProject(_ context.Context, _ string) ([]domain.NotificationChannel, error) {
	return f.channels, nil
}

// byID answers the snapshot lookup for the channels that STILL EXIST, keyed by id. A test that wants
// a deleted recipient simply leaves it out, which is what `Requested` vs `Resolved` is about.
func (f *fakeStore) EnabledChannelsByIDs(_ context.Context, ids []string) ([]domain.NotificationChannel, error) {
	if f.byID == nil {
		return f.channels, nil
	}
	var out []domain.NotificationChannel
	for _, id := range ids {
		if ch, ok := f.byID[id]; ok {
			out = append(out, ch)
		}
	}
	return out, nil
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

// The three numbers `DeliverChannelsReporting` returns, against the REAL dispatcher.
//
// D-0179 credits a service's coverage on `Delivered`, and until this test existed nothing in this
// package called this method at all: the outbox tests drove a fake that set the field by hand, so
// deleting the production `out.Delivered++` left the whole suite green while the real notifier
// returned zero for every successful send. A credit that is only ever computed by a test double is
// not a credit.
//
// `Requested` is what the snapshot asked for, `Resolved` is what still exists, `Delivered` is what a
// send actually succeeded for — three facts, and the case that matters is the one where the middle
// and the last disagree.
func TestChannelDeliveryReportsAskedExistedAndSucceeded(t *testing.T) {
	okHits, failHits := 0, 0
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		okHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		failHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()

	hook := func(id, url string) domain.NotificationChannel {
		return domain.NotificationChannel{
			ID: id, Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": url},
		}
	}

	for _, tc := range []struct {
		name                                 string
		channels                             map[string]domain.NotificationChannel
		ask                                  []string
		wantReq, wantResolved, wantDelivered int
		wantErr                              bool
	}{
		{
			name:     "every send succeeds",
			channels: map[string]domain.NotificationChannel{"a": hook("a", ok.URL), "b": hook("b", ok.URL)},
			ask:      []string{"a", "b"},
			wantReq:  2, wantResolved: 2, wantDelivered: 2,
		},
		{
			// The P0 shape: the channel EXISTS and its send fails. `Resolved` says one; nobody was
			// told, and crediting coverage on that number silenced the members permanently.
			name:     "the only channel that exists returns 500",
			channels: map[string]domain.NotificationChannel{"a": hook("a", fail.URL)},
			ask:      []string{"a"},
			wantReq:  1, wantResolved: 1, wantDelivered: 0, wantErr: true,
		},
		{
			name:     "one of two succeeds",
			channels: map[string]domain.NotificationChannel{"a": hook("a", ok.URL), "b": hook("b", fail.URL)},
			ask:      []string{"a", "b"},
			wantReq:  2, wantResolved: 2, wantDelivered: 1, wantErr: true,
		},
		{
			// §16.4a: a snapshot recipient deleted since the announcement opened. Not an error —
			// nothing to retry — and not a delivery either.
			name:     "a snapshot recipient has been deleted",
			channels: map[string]domain.NotificationChannel{"a": hook("a", ok.URL)},
			ask:      []string{"a", "gone"},
			wantReq:  2, wantResolved: 1, wantDelivered: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New(&fakeStore{byID: tc.channels}, ok.Client())
			res, err := d.DeliverChannelsReporting(context.Background(), tc.ask, "the service is DOWN")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if res.Requested != tc.wantReq || res.Resolved != tc.wantResolved || res.Delivered != tc.wantDelivered {
				t.Fatalf("requested/resolved/delivered = %d/%d/%d, want %d/%d/%d — coverage is credited "+
					"on the LAST of those, and a channel row that exists is not a send that succeeded",
					res.Requested, res.Resolved, res.Delivered,
					tc.wantReq, tc.wantResolved, tc.wantDelivered)
			}
		})
	}
}

// The same, for the EMAIL branch, which has its own increment on its own path.
func TestChannelDeliveryCountsEmailSendsThatSucceeded(t *testing.T) {
	var fail bool
	orig := sendMailFunc
	sendMailFunc = func(_ string, _ smtp.Auth, _ string, _ []string, _ []byte) error {
		if fail {
			return errors.New("smtp refused")
		}
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })

	mail := domain.NotificationChannel{
		ID: "e", Type: domain.ChannelEmail, Enabled: true,
		Config: map[string]string{"smtp_host": "mail", "smtp_port": "1025", "from": "c@x", "to": "a@x"},
	}
	d := New(&fakeStore{byID: map[string]domain.NotificationChannel{"e": mail}}, nil)

	res, err := d.DeliverChannelsReporting(context.Background(), []string{"e"}, "down")
	if err != nil || res.Delivered != 1 {
		t.Fatalf("a successful mail reported delivered=%d err=%v, want 1/nil", res.Delivered, err)
	}

	fail = true
	res, err = d.DeliverChannelsReporting(context.Background(), []string{"e"}, "down")
	if err == nil {
		t.Fatal("a refused SMTP session returned no error")
	}
	if res.Resolved != 1 || res.Delivered != 0 {
		t.Fatalf("a refused mail reported resolved=%d delivered=%d, want 1/0 — the mailbox exists and "+
			"nobody was told", res.Resolved, res.Delivered)
	}
}
