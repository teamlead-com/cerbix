package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPushTokenHiddenFromViewer proves the push token (a bearer capability for the
// public push endpoint) is returned only to a caller who can WRITE the monitor, not
// to a read-only viewer — closing the viewer-can-forge-heartbeats leak.
func TestPushTokenHiddenFromViewer(t *testing.T) {
	h := newHandler(seededStore()) // monp is a push monitor in p1 with PushToken "tok-push"

	pushTokenFrom := func(body []byte) string {
		var m struct {
			PushToken string `json:"push_token"`
		}
		_ = json.Unmarshal(body, &m)
		return m.PushToken
	}

	// GET single monitor: writer sees the token, viewer does not.
	adminGet := do(h, o1Admin, http.MethodGet, "/api/v1/monitors/monp", "")
	if adminGet.Code != http.StatusOK {
		t.Fatalf("admin get = %d", adminGet.Code)
	}
	if pushTokenFrom(adminGet.Body.Bytes()) != "tok-push" {
		t.Fatalf("editor/admin must still receive the push token, got %q", pushTokenFrom(adminGet.Body.Bytes()))
	}
	viewerGet := do(h, p1Viewer, http.MethodGet, "/api/v1/monitors/monp", "")
	if viewerGet.Code != http.StatusOK {
		t.Fatalf("viewer get = %d", viewerGet.Code)
	}
	if strings.Contains(viewerGet.Body.String(), "tok-push") {
		t.Fatalf("viewer must NOT receive the push token: %s", viewerGet.Body.String())
	}
	if pushTokenFrom(viewerGet.Body.Bytes()) != "" {
		t.Fatalf("viewer push_token must be blank, got %q", pushTokenFrom(viewerGet.Body.Bytes()))
	}

	// List monitors: same rule per row.
	viewerList := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/monitors", "")
	if viewerList.Code != http.StatusOK || strings.Contains(viewerList.Body.String(), "tok-push") {
		t.Fatalf("viewer list leaked push token (code=%d): %s", viewerList.Code, viewerList.Body.String())
	}
	adminList := do(h, o1Admin, http.MethodGet, "/api/v1/projects/p1/monitors", "")
	if !strings.Contains(adminList.Body.String(), "tok-push") {
		t.Fatalf("admin list should retain the push token: %s", adminList.Body.String())
	}
}
