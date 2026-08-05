package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestEscalationPolicyAPI(t *testing.T) {
	h := newHandler(seededStore())

	// Create (editor+) with a valid two-step ladder.
	body := `{"name":"oncall","repeat_last":true,"steps":[
		{"after_seconds":0,"targets":[{"type":"channel","id":"nc1"}]},
		{"after_seconds":600,"targets":[{"type":"channel","id":"nc1"}]}]}`
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/escalation-policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create policy = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created domain.EscalationPolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" || len(created.Steps) != 2 || !created.RepeatLast {
		t.Fatalf("created policy = %+v", created)
	}

	// Invalid (no steps) → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/escalation-policies", `{"name":"x","steps":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty steps = %d, want 400", rec.Code)
	}
	// Viewer may not create (project write).
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/projects/p1/escalation-policies", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403", rec.Code)
	}
	// List returns it.
	rec = do(h, o1Admin, http.MethodGet, "/api/v1/projects/p1/escalation-policies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list []domain.EscalationPolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	// Delete.
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/escalation-policies/"+created.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
}

func TestOnCallScheduleAPI(t *testing.T) {
	h := newHandler(seededStore())
	body := `{"name":"primary","shift_seconds":604800,"anchor_at":"2026-01-05T00:00:00Z","participants":["nc1"]}`
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/oncall-schedules", body); rec.Code != http.StatusCreated {
		t.Fatalf("create schedule = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	// Invalid (shift 0) → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/oncall-schedules",
		`{"name":"x","shift_seconds":0,"participants":["nc1"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad shift = %d, want 400", rec.Code)
	}
}

func TestOnCallOverrideAPI(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	// Seed a schedule directly.
	fs.oncall = map[string]domain.OnCallSchedule{"sc1": {ID: "sc1", ProjectID: "p1", Name: "rot", ShiftSeconds: 604800, Participants: []string{"nc1"}}}

	// Add an override (editor+).
	body := `{"channel_id":"nc1","starts_at":"2026-02-01T00:00:00Z","ends_at":"2026-02-08T00:00:00Z"}`
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/oncall-schedules/sc1/overrides", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add override = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var ov domain.OnCallOverride
	_ = json.Unmarshal(rec.Body.Bytes(), &ov)

	// Invalid (ends<=starts) → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/oncall-schedules/sc1/overrides",
		`{"channel_id":"nc1","starts_at":"2026-02-08T00:00:00Z","ends_at":"2026-02-01T00:00:00Z"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad override = %d, want 400", rec.Code)
	}
	// Viewer may not add.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/oncall-schedules/sc1/overrides", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer add = %d, want 403", rec.Code)
	}
	// List + current on-call resolve.
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/oncall-schedules/sc1/overrides", ""); rec.Code != http.StatusOK {
		t.Fatalf("list overrides = %d", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/oncall-schedules/sc1/current", ""); rec.Code != http.StatusOK {
		t.Fatalf("current on-call = %d (%s)", rec.Code, rec.Body.String())
	}
	// Delete.
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/oncall-overrides/"+ov.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete override = %d, want 204", rec.Code)
	}
}

func TestAcknowledgeIncidentAPI(t *testing.T) {
	h := newHandler(seededStore())
	// Ack the seeded open incident inc1 (editor+).
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/acknowledge", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var inc domain.Incident
	_ = json.Unmarshal(rec.Body.Bytes(), &inc)
	if inc.AcknowledgedAt == nil {
		t.Fatalf("incident not acknowledged: %+v", inc)
	}
	// Viewer may not acknowledge.
	if rec := do(h, p1Viewer, http.MethodPost, "/api/v1/incidents/inc1/acknowledge", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer ack = %d, want 403", rec.Code)
	}
	// Unknown incident → 404.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/nope/acknowledge", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown ack = %d, want 404", rec.Code)
	}
}
