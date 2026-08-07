package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
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

// TestNotificationChannelSecretsRedacted asserts that channel list responses (visible
// to viewers) never carry decrypted credentials: bot tokens, SMTP passwords, and
// secret-bearing webhook/Slack URLs are blanked, while non-secret config survives.
func TestNotificationChannelSecretsRedacted(t *testing.T) {
	h := newHandler(seededStore())

	// Telegram: the create response itself must already be redacted, and a viewer's
	// list must blank bot_token while keeping the non-secret chat_id.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/notification-channels",
		`{"type":"telegram","name":"tg","config":{"bot_token":"SECRET-BOT","chat_id":"123"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create telegram = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("create response leaked telegram bot_token: %s", rec.Body.String())
	}
	tg := viewerChannel(t, h, "telegram")
	if tg.Config["bot_token"] != "" {
		t.Fatalf("telegram bot_token not redacted: %q", tg.Config["bot_token"])
	}
	if tg.Config["chat_id"] != "123" {
		t.Fatalf("telegram chat_id must survive redaction: %q", tg.Config["chat_id"])
	}

	// Slack: the URL embeds the token, so it must be blanked too. (The fake reuses a
	// single channel id, so this overwrites the telegram one — list it fresh.)
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/notification-channels",
		`{"type":"slack","name":"sl","config":{"url":"https://hooks.slack.com/services/SECRET"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create slack = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("create response leaked slack url: %s", rec.Body.String())
	}
	sl := viewerChannel(t, h, "slack")
	if sl.Config["url"] != "" {
		t.Fatalf("slack url not redacted: %q", sl.Config["url"])
	}

	// PATCH (toggle) response must ALSO be redacted — the fix for the iter-0075 gap
	// where updateChannel returned the decrypted channel.
	patch := do(h, o1Admin, http.MethodPatch, "/api/v1/notification-channels/nc-new", `{"enabled":false}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch channel = %d (%s)", patch.Code, patch.Body.String())
	}
	if strings.Contains(patch.Body.String(), "SECRET") {
		t.Fatalf("PATCH response leaked the secret url: %s", patch.Body.String())
	}
}

// viewerChannel lists p1's channels as a viewer and returns the first of the given
// type, failing if the list leaked any secret substring.
func viewerChannel(t *testing.T, h http.Handler, typ string) struct {
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
} {
	t.Helper()
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/notification-channels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("channel list leaked a secret to a viewer: %s", rec.Body.String())
	}
	var chans []struct {
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range chans {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("no %s channel in list: %s", typ, rec.Body.String())
	return chans[0]
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
