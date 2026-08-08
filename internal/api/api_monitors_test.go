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

// TestMonitorManagementProvenance covers spec §15: monitor responses carry a tenant-safe
// `management` block — file-managed monitors report source=file + provider/uid/path +
// read_only; ordinary monitors report source=ui.
func TestMonitorManagementProvenance(t *testing.T) {
	fs := seededStore()
	fs.managed = map[string]store.FileManagement{
		"mon1": {Provider: "platform", UID: "api", SourcePath: "acme-payments.yaml"},
	}
	h := newHandler(fs)

	rec := do(h, o1Admin, http.MethodGet, "/api/v1/monitors/mon1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get mon1 = %d", rec.Code)
	}
	var got struct {
		ID         string `json:"id"`
		Management struct {
			Source, Provider, UID, Path string
			ReadOnly                    bool `json:"read_only"`
		} `json:"management"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID != "mon1" {
		t.Fatalf("monitor fields must still promote to top level: %s", rec.Body.String())
	}
	if got.Management.Source != "file" || got.Management.Provider != "platform" || got.Management.UID != "api" || got.Management.Path != "acme-payments.yaml" || !got.Management.ReadOnly {
		t.Fatalf("file provenance block = %+v", got.Management)
	}

	// An ordinary (unmanaged) monitor reports source=ui.
	plain := newHandler(seededStore())
	rec = do(plain, o1Admin, http.MethodGet, "/api/v1/monitors/mon1", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Management.Source != "ui" || got.Management.ReadOnly {
		t.Fatalf("unmanaged monitor management = %+v, want source=ui read_only=false", got.Management)
	}
}

// TestFileProviderDiagnostics covers spec §15: global-admin sees every provider bundle;
// an org-admin sees only its organization's bundles; a non-admin is refused.
func TestFileProviderDiagnostics(t *testing.T) {
	fs := seededStore()
	fs.diagnostics = []fakeDiag{
		{orgID: "o1", diag: store.FileProviderDiagnostic{Provider: "platform", Organization: "acme", Project: "payments", SourcePath: "a.yaml", Generation: 3, Status: "applied"}},
		{orgID: "o2", diag: store.FileProviderDiagnostic{Provider: "platform", Organization: "beta", Project: "web", SourcePath: "b.yaml", Generation: 1, Status: "degraded"}},
	}
	h := newHandler(fs)

	// Global admin: all bundles.
	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/admin/file-providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin diagnostics = %d", rec.Code)
	}
	var all struct {
		Providers []store.FileProviderDiagnostic `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all.Providers) != 2 {
		t.Fatalf("global admin should see all bundles, got %d", len(all.Providers))
	}
	// Non-admin: 403.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/admin/file-providers", ""); rec.Code == http.StatusOK {
		t.Fatalf("non-admin got %d, want non-200", rec.Code)
	}
	// Org admin: only own org (o1).
	rec = do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/file-providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("org admin diagnostics = %d (%s)", rec.Code, rec.Body.String())
	}
	var org struct {
		Providers []store.FileProviderDiagnostic `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &org)
	if len(org.Providers) != 1 || org.Providers[0].Organization != "acme" {
		t.Fatalf("org admin should see only own-org bundles, got %+v", org.Providers)
	}
	// Foreign org → not found / forbidden (never another tenant's data).
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o2/file-providers", ""); rec.Code == http.StatusOK {
		t.Fatalf("org admin must not read another org's diagnostics, got %d", rec.Code)
	}
}
