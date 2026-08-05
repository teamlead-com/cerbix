package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.example.com/monitoring/cerbix/internal/authz"
	"git.example.com/monitoring/cerbix/internal/domain"
)

// Service-account principals (as the auth middleware would build from a token).
var (
	tokenEditor   = authz.Principal{UserID: "apitoken:t", ViaToken: true, Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleEditor}}}
	tokenViewer   = authz.Principal{UserID: "apitoken:v", ViaToken: true, Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}}}
	tokenOtherOrg = authz.Principal{UserID: "apitoken:o", ViaToken: true, Memberships: []domain.Membership{{OrgID: "o2", Role: domain.RoleEditor}}}
)

func TestApiTokenManagementAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// List: viewer forbidden, admin ok.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/tokens", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer list tokens = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/tokens", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin list tokens = %d, want 200", rec.Code)
	}
	// Outsider hidden.
	if rec := do(h, outsider, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"x","role":"editor"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider create token = %d, want 404", rec.Code)
	}
	// Viewer forbidden.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"x","role":"editor"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create token = %d, want 403", rec.Code)
	}
	// Admin creates an editor token; the secret is returned once.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"ci","role":"editor"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create token = %d, want 201", rec.Code)
	}
	var out struct {
		Token    string          `json:"token"`
		ApiToken domain.ApiToken `json:"api_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" || out.ApiToken.ID == "" {
		t.Fatalf("expected a plaintext token and metadata, got %+v", out)
	}
	// Bad role for org scope → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/tokens", `{"name":"x","role":"project_admin"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("org-scope project_admin = %d, want 400", rec.Code)
	}
}

func TestApiTokenDeleteAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/tokens/at1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete token = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/tokens/at1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete token = %d, want 204", rec.Code)
	}
}

func TestIncidentViaTokenSourceAndScope(t *testing.T) {
	h := newHandler(seededStore())
	// An editor token posts an incident → 201 with source "api".
	rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"seen by CI","impact":"minor"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("token create incident = %d, want 201", rec.Code)
	}
	var inc domain.Incident
	_ = json.Unmarshal(rec.Body.Bytes(), &inc)
	if inc.Source != domain.SourceAPI {
		t.Fatalf("incident source = %q, want api", inc.Source)
	}
	// A viewer token cannot write.
	if rec := do(h, tokenViewer, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer token create = %d, want 403", rec.Code)
	}
	// A token scoped to another org cannot even see the project.
	if rec := do(h, tokenOtherOrg, http.MethodPost, "/api/v1/projects/p1/incidents", `{"title":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org token create = %d, want 404", rec.Code)
	}
}

func TestAlertmanagerWebhook(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	fire := `{"alerts":[{"status":"firing","fingerprint":"fp-1","labels":{"alertname":"HighLatency","severity":"critical"},"annotations":{"summary":"p99 latency high","description":"p99 > 2s for 5m"}}]}`

	// A firing alert opens one incident (source api, impact from severity, external key set).
	rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", fire)
	if rec.Code != http.StatusOK {
		t.Fatalf("firing = %d, want 200", rec.Code)
	}
	var res struct{ Opened, Resolved, Ignored int }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Opened != 1 {
		t.Fatalf("opened = %d, want 1 (%s)", res.Opened, rec.Body.String())
	}
	var opened domain.Incident
	for _, inc := range fs.incidents {
		if inc.ExternalKey == "fp-1" {
			opened = inc
		}
	}
	if opened.ID == "" || opened.Source != domain.SourceAPI || opened.Impact != domain.ImpactCritical || opened.Title != "p99 latency high" {
		t.Fatalf("opened incident = %+v", opened)
	}

	// A second identical firing is idempotent — no new incident.
	rec = do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", fire)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Opened != 0 || res.Ignored != 1 {
		t.Fatalf("re-firing = %+v, want opened 0 / ignored 1", res)
	}

	// A resolved alert closes the incident that fingerprint opened.
	rec = do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
		`{"alerts":[{"status":"resolved","fingerprint":"fp-1","labels":{"alertname":"HighLatency"}}]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Resolved != 1 {
		t.Fatalf("resolved = %+v, want resolved 1", res)
	}
	if fs.incidents[opened.ID].Status != domain.IncidentResolved {
		t.Fatalf("incident status = %q, want resolved", fs.incidents[opened.ID].Status)
	}

	// Resolving an unknown fingerprint is ignored, not an error.
	rec = do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
		`{"alerts":[{"status":"resolved","fingerprint":"nope"}]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Resolved != 0 || res.Ignored != 1 {
		t.Fatalf("unknown resolve = %d %+v", rec.Code, res)
	}

	// A viewer token cannot post alerts.
	if rec := do(h, tokenViewer, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", fire); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer token alert = %d, want 403", rec.Code)
	}
}
