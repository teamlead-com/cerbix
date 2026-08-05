package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestDeadOutboxAdminOnly(t *testing.T) {
	fs := seededStore()
	fs.deadOutbox["e1"] = domain.OutboxEventView{ID: "e1", Topic: domain.TopicIncidentEvent, Status: "dead", Attempts: 10}
	fs.deadOutbox["e2"] = domain.OutboxEventView{ID: "e2", Topic: domain.TopicMonitorTransition, Status: "dead", Attempts: 10}
	h := newHandler(fs)

	// A non-global-admin is forbidden on every endpoint.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/outbox/dead"},
		{http.MethodPost, "/api/v1/admin/outbox/dead/e1/replay"},
		{http.MethodPost, "/api/v1/admin/outbox/dead/replay-all"},
	} {
		if rec := do(h, o1Admin, tc.method, tc.path, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as org-admin = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// Global admin lists the dead events.
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/admin/outbox/dead", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list dead = %d, want 200", rec.Code)
	}
	var listed []domain.OutboxEventView
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 2 {
		t.Fatalf("listed %d dead events, want 2", len(listed))
	}
}

func TestDeadOutboxReplay(t *testing.T) {
	fs := seededStore()
	fs.deadOutbox["e1"] = domain.OutboxEventView{ID: "e1", Topic: domain.TopicIncidentEvent, Status: "dead"}
	h := newHandler(fs)

	// Replaying an unknown id → 404.
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/admin/outbox/dead/nope/replay", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("replay unknown = %d, want 404", rec.Code)
	}
	// Replaying a real dead event → 204 and it's requeued (gone from dead).
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/admin/outbox/dead/e1/replay", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("replay = %d, want 204", rec.Code)
	}
	if _, ok := fs.deadOutbox["e1"]; ok {
		t.Fatal("event should have been requeued out of the dead set")
	}

	// Replay-all returns the count and empties the dead set.
	fs.deadOutbox["a"] = domain.OutboxEventView{ID: "a", Status: "dead"}
	fs.deadOutbox["b"] = domain.OutboxEventView{ID: "b", Status: "dead"}
	rec := do(h, globalAdmin, http.MethodPost, "/api/v1/admin/outbox/dead/replay-all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay-all = %d, want 200", rec.Code)
	}
	var got struct {
		Replayed int `json:"replayed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Replayed != 2 || len(fs.deadOutbox) != 0 {
		t.Fatalf("replay-all count = %d, remaining = %d, want 2/0", got.Replayed, len(fs.deadOutbox))
	}
}
