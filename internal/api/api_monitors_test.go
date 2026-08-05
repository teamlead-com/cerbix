package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestUpdateMonitorAuthzAndPartial(t *testing.T) {
	h := newHandler(seededStore())

	// Viewer cannot edit.
	if rec := do(h, p1Viewer, http.MethodPatch, "/api/v1/monitors/mon1", `{"name":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d, want 403", rec.Code)
	}
	// Editor renames + disables; type stays http, other fields unchanged.
	rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/mon1", `{"name":"renamed","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200", rec.Code)
	}
	var m domain.Monitor
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Name != "renamed" || m.Enabled || m.Type != domain.MonitorHTTP || m.Target != "https://x" {
		t.Fatalf("partial update wrong: %+v", m)
	}
	// A partial conditions-only update leaves the name from the previous edit.
	rec = do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/mon1", `{"conditions":["[STATUS] == 204"]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if len(m.Conditions) != 1 || m.Conditions[0] != "[STATUS] == 204" || m.Name != "renamed" {
		t.Fatalf("conditions-only update wrong: %+v", m)
	}
	// Invalid (interval 0 on an http monitor) → 400.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/mon1", `{"interval_seconds":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid interval = %d, want 400", rec.Code)
	}
	// Unknown monitor → 404.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/nope", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown monitor = %d, want 404", rec.Code)
	}
}
