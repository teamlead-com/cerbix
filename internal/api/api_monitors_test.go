package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
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

// TestFileManagedMonitorReadOnly proves the ownership contract at the transport boundary
// (spec §8): a file-managed monitor rejects declarative CRUD with 409 + tenant-safe
// provenance, while unmanaged monitors stay editable.
func TestFileManagedMonitorReadOnly(t *testing.T) {
	fs := seededStore()
	fs.managed = map[string]store.FileManagement{
		"mon1": {Provider: "platform", UID: "api", SourcePath: "acme-payments.yaml"},
	}
	h := newHandler(fs)

	rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/mon1", `{"name":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch file-managed = %d, want 409", rec.Code)
	}
	var body struct {
		Error      string `json:"error"`
		Management struct {
			Source, Provider, UID, Path string
			ReadOnly                    bool `json:"read_only"`
		} `json:"management"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != "managed_by_file" || body.Management.Provider != "platform" || body.Management.UID != "api" || !body.Management.ReadOnly {
		t.Fatalf("409 body = %s", rec.Body.String())
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/monitors/mon1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete file-managed = %d, want 409", rec.Code)
	}
	// The SAME monitor, when NOT file-managed, stays editable (coexistence, not a global lock).
	unmanaged := newHandler(seededStore())
	if rec := do(unmanaged, o1Admin, http.MethodPatch, "/api/v1/monitors/mon1", `{"name":"y"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch unmanaged = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}
