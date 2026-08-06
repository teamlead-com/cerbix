package api_test

import (
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestWebhookToggle(t *testing.T) {
	fs := seededStore()
	fs.webhooks["w1"] = domain.Webhook{ID: "w1", OrgID: "o1", URL: "https://x", Enabled: true}
	h := newHandler(fs)

	if rec := do(h, o1Viewer, http.MethodPatch, "/api/v1/webhooks/w1", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer toggle = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/webhooks/w1", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing field = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/webhooks/w1", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200", rec.Code)
	}
	if fs.webhooks["w1"].Enabled {
		t.Fatal("webhook still enabled")
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/webhooks/nope", `{"enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d, want 404", rec.Code)
	}
}

func TestChannelToggle(t *testing.T) {
	fs := seededStore()
	fs.channels["c1"] = domain.NotificationChannel{ID: "c1", ProjectID: "p1", Type: "webhook", Name: "ops", Enabled: true}
	h := newHandler(fs)

	if rec := do(h, o1Viewer, http.MethodPatch, "/api/v1/notification-channels/c1", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer toggle = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/notification-channels/c1", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200", rec.Code)
	}
	if fs.channels["c1"].Enabled {
		t.Fatal("channel still enabled")
	}
}
