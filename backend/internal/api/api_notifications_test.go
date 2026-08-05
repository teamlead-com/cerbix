package api_test

import (
	"net/http"
	"testing"
)

func TestNotificationChannelAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// List: viewer may read.
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/notification-channels", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer list channels = %d, want 200", rec.Code)
	}
	// Create: viewer forbidden, admin ok.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/projects/p1/notification-channels", `{"type":"webhook","name":"x","config":{"url":"https://y"}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/notification-channels", `{"type":"slack","name":"ops","config":{"url":"https://y"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201", rec.Code)
	}
	// Bad type / missing config → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/notification-channels", `{"type":"sms","name":"x","config":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/notification-channels", `{"type":"telegram","name":"t","config":{"bot_token":"b"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("telegram missing chat_id = %d, want 400", rec.Code)
	}
	// Delete authz.
	if rec := do(h, p1Viewer, http.MethodDelete, "/api/v1/notification-channels/nc1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/notification-channels/nc1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
}

func TestMonitorChannelLinking(t *testing.T) {
	h := newHandler(seededStore())
	// Viewer cannot link.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/monitors/mon1/notifications", `{"channel_id":"nc1"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer link = %d, want 403", rec.Code)
	}
	// Editor links a same-project channel.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/mon1/notifications", `{"channel_id":"nc1"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("admin link = %d, want 204", rec.Code)
	}
	// A channel from another project is rejected.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/mon1/notifications", `{"channel_id":"nc3"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-project channel = %d, want 400", rec.Code)
	}
	// List reflects the link.
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/monitors/mon1/notifications", "")
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), "nc1") {
		t.Fatalf("list linked channels = %d body=%s", rec.Code, rec.Body.String())
	}
	// Unlink.
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/monitors/mon1/notifications/nc1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unlink = %d, want 204", rec.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
