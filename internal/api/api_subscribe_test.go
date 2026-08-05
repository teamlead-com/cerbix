package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func newPublicHandlerWithMailer(fs *fakeStore, m api.Mailer) http.Handler {
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8)
	if m != nil {
		h.WithMailer(m)
	}
	return h.PublicRouter()
}

// token of the single subscriber the fake created (there is exactly one).
func onlyToken(fs *fakeStore) string {
	for tok := range fs.subscribers {
		return tok
	}
	return ""
}

// queuedConfirms decodes the subscriber-confirmation emails queued to the outbox.
func queuedConfirms(fs *fakeStore) []domain.SubscriberConfirm {
	var out []domain.SubscriberConfirm
	for _, e := range fs.outboxEvents {
		if e.Topic != domain.TopicSubscriberConfirm {
			continue
		}
		var sc domain.SubscriberConfirm
		if err := json.Unmarshal(e.Payload, &sc); err == nil {
			out = append(out, sc)
		}
	}
	return out
}

func TestSubscribeConfirmUnsubscribe(t *testing.T) {
	fs := seededStore()
	mail := &fakeMailer{}
	h := newPublicHandlerWithMailer(fs, mail)

	// Subscribe to the public page → 202 + a confirmation email *queued* (not sent
	// inline; the outbox worker delivers it, so a slow SMTP can't block the request).
	rec := do(h, outsider, http.MethodPost, "/api/v1/public/status-pages/acme-status/subscribers", `{"email":"a@x.com"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("subscribe = %d, want 202", rec.Code)
	}
	if len(mail.sent) != 0 {
		t.Fatalf("email must be queued, not sent inline: %+v", mail.sent)
	}
	if q := queuedConfirms(fs); len(q) != 1 || q[0].To != "a@x.com" || !strings.Contains(q[0].Body, "confirm=") {
		t.Fatalf("queued confirmation = %+v", q)
	}
	tok := onlyToken(fs)
	if tok == "" {
		t.Fatal("no subscriber created")
	}

	// Invalid email → 400, no email sent.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/status-pages/acme-status/subscribers", `{"email":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad email = %d, want 400", rec.Code)
	}

	// Internal page hidden from subscribe.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/status-pages/internal-status/subscribers", `{"email":"a@x.com"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("subscribe internal = %d, want 404", rec.Code)
	}

	// Confirm with the token → 200; unknown token → 404.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/subscriptions/"+tok+"/confirm", ""); rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", rec.Code)
	}
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/subscriptions/nope/confirm", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("confirm unknown = %d, want 404", rec.Code)
	}

	// Re-subscribing a confirmed email reports already_subscribed (no new email queued).
	before := len(queuedConfirms(fs))
	rec = do(h, outsider, http.MethodPost, "/api/v1/public/status-pages/acme-status/subscribers", `{"email":"a@x.com"}`)
	var body struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body.Status != "already_subscribed" {
		t.Fatalf("re-subscribe = %d %q, want 200 already_subscribed", rec.Code, body.Status)
	}
	if len(queuedConfirms(fs)) != before {
		t.Fatal("confirmed re-subscribe should not queue another email")
	}

	// Unsubscribe removes it; second time is 404. Re-subscribe rotated the token
	// (ON CONFLICT re-issues it), so use the current one.
	tok = onlyToken(fs)
	if rec := do(h, outsider, http.MethodDelete, "/api/v1/public/subscriptions/"+tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe = %d, want 204", rec.Code)
	}
	if rec := do(h, outsider, http.MethodDelete, "/api/v1/public/subscriptions/"+tok, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unsubscribe again = %d, want 404", rec.Code)
	}
}

func TestSubscribeDisabledWithoutMailer(t *testing.T) {
	h := newPublicHandlerWithMailer(seededStore(), nil)
	if rec := do(h, outsider, http.MethodPost, "/api/v1/public/status-pages/acme-status/subscribers", `{"email":"a@x.com"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("subscribe without mailer = %d, want 503", rec.Code)
	}
}
