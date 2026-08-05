package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Incident lifecycle events are now enqueued to the transactional outbox by the
// store (CreateIncident / AddIncidentUpdate), not emitted from the API layer, so
// that behavior is covered by the store integration test rather than here.

func TestWebhookManagementAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/webhooks", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer list webhooks = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/webhooks", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin list webhooks = %d, want 200", rec.Code)
	}
	// Secrets are stripped from the listing.
	rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/webhooks", "")
	var listed []domain.Webhook
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	for _, w := range listed {
		if w.Secret != "" {
			t.Fatal("listing must not expose secrets")
		}
	}
	// Create generates a secret and returns it once.
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/webhooks", `{"url":"https://hook.example/new"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook = %d, want 201", rec.Code)
	}
	var created domain.Webhook
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Secret == "" {
		t.Fatal("create response should carry the generated secret")
	}
	// Non-http URL rejected.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/webhooks", `{"url":"ftp://x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d, want 400", rec.Code)
	}
	// Delete authz.
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/webhooks/wh1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/webhooks/wh1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
}
