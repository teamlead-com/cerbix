package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func TestPublicRenderEnriched(t *testing.T) {
	fs := seededStore()
	now := time.Now()
	resolved := now.Add(-24 * time.Hour)
	// A resolved incident (with timeline + postmortem) and an upcoming maintenance
	// in the component's project (p1).
	fs.incidents["incR"] = domain.Incident{ID: "incR", ProjectID: "p1", Title: "past blip", Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual, ResolvedAt: &resolved}
	fs.incUpdates["incR"] = []domain.IncidentUpdate{
		{ID: "iuR1", IncidentID: "incR", Status: domain.IncidentInvestigating, Body: "looking into it"},
		{ID: "iuR2", IncidentID: "incR", Status: domain.IncidentResolved, Body: "fixed"},
	}
	fs.postmortems["incR"] = domain.Postmortem{ID: "pmR", IncidentID: "incR", Body: "## Summary\nroot cause was X", Author: "op"}
	fs.maintenance["mw1"] = domain.MaintenanceWindow{ID: "mw1", ProjectID: "p1", Reason: "db upgrade", StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
	h := newPublicHandler(fs)

	rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d, want 200", rec.Code)
	}
	var render struct {
		Components []struct {
			Daily []struct {
				Day string `json:"day"`
			} `json:"daily"`
		} `json:"components"`
		ActiveIncidents []struct {
			ID      string `json:"id"`
			Updates []struct {
				ID string `json:"id"`
			} `json:"updates"`
		} `json:"active_incidents"`
		RecentIncidents []struct {
			ID      string `json:"id"`
			Updates []struct {
				ID string `json:"id"`
			} `json:"updates"`
			Postmortem *struct {
				Body string `json:"body"`
			} `json:"postmortem"`
		} `json:"recent_incidents"`
		Maintenance []struct {
			ID string `json:"id"`
		} `json:"maintenance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &render); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Monitor-backed component carries a per-day strip.
	if len(render.Components) != 1 || len(render.Components[0].Daily) == 0 {
		t.Fatalf("component daily = %+v, want non-empty", render.Components)
	}
	// inc1 is active; incR is resolved history; mw1 is upcoming maintenance.
	if len(render.ActiveIncidents) != 1 {
		t.Fatalf("active incidents = %d, want 1", len(render.ActiveIncidents))
	}
	// The active incident carries its timeline (inc1 is seeded with one update).
	if len(render.ActiveIncidents[0].Updates) != 1 {
		t.Fatalf("active incident updates = %d, want 1", len(render.ActiveIncidents[0].Updates))
	}
	if len(render.RecentIncidents) != 1 || render.RecentIncidents[0].ID != "incR" {
		t.Fatalf("recent incidents = %+v, want [incR]", render.RecentIncidents)
	}
	// The past incident carries its timeline and published postmortem.
	if got := render.RecentIncidents[0]; len(got.Updates) != 2 {
		t.Fatalf("recent incident updates = %d, want 2", len(got.Updates))
	}
	if pm := render.RecentIncidents[0].Postmortem; pm == nil || pm.Body == "" {
		t.Fatalf("recent incident should carry its postmortem, got %+v", pm)
	}
	if len(render.Maintenance) != 1 || render.Maintenance[0].ID != "mw1" {
		t.Fatalf("maintenance = %+v, want [mw1]", render.Maintenance)
	}
}

func TestListStatusPages(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/status-pages", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status pages = %d, want 200", rec.Code)
	}
	var pages []domain.StatusPage
	_ = json.Unmarshal(rec.Body.Bytes(), &pages)
	if len(pages) != 3 {
		t.Fatalf("o1 should have 3 status pages, got %d", len(pages))
	}
	// Outsider cannot list.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/organizations/o1/status-pages", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider list = %d, want 404", rec.Code)
	}
}

func TestCreateStatusPageAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, outsider, http.MethodPost, "/api/v1/organizations/o1/status-pages", `{"slug":"s","title":"S"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider create = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/organizations/o1/status-pages", `{"slug":"s","title":"S"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/status-pages", `{"slug":"s","title":"S"}`); rec.Code != http.StatusCreated {
		t.Fatalf("org admin create = %d, want 201", rec.Code)
	}
	// Unlisted page gets a generated token.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/status-pages", `{"slug":"u","title":"U","visibility":"unlisted"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create unlisted = %d, want 201", rec.Code)
	}
	var sp domain.StatusPage
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	if sp.UnlistedToken == "" {
		t.Fatal("unlisted page should have a generated token")
	}
	// Bad visibility → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/status-pages", `{"slug":"x","title":"X","visibility":"secret"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad visibility = %d, want 400", rec.Code)
	}
}

func TestComponentAuthzAndMonitorOrgCheck(t *testing.T) {
	h := newHandler(seededStore())
	// Viewer cannot add a component.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"Web"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer add component = %d, want 403", rec.Code)
	}
	// Admin adds a manual component.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"Web","manual_status":"operational"}`); rec.Code != http.StatusCreated {
		t.Fatalf("admin add component = %d, want 201", rec.Code)
	}
	// Monitor in the same org → 201.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"API","monitor_id":"mon1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("component with in-org monitor = %d, want 201", rec.Code)
	}
	// Monitor mon3 is in o2 → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"X","monitor_id":"mon3"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("component with cross-org monitor = %d, want 400", rec.Code)
	}
}

func TestDeleteComponentAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/components/c1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/components/c1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
}

func TestUpdateStatusPageAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// Outsider hidden, viewer forbidden.
	if rec := do(h, outsider, http.MethodPatch, "/api/v1/status-pages/sp1", `{"title":"X"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider update = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodPatch, "/api/v1/status-pages/sp1", `{"title":"X"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer update = %d, want 403", rec.Code)
	}
	// Admin renames.
	rec := do(h, o1Admin, http.MethodPatch, "/api/v1/status-pages/sp1", `{"title":"Renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin update = %d, want 200", rec.Code)
	}
	var sp domain.StatusPage
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	if sp.Title != "Renamed" {
		t.Fatalf("title = %q, want Renamed", sp.Title)
	}
	// Switching to unlisted mints a token.
	rec = do(h, o1Admin, http.MethodPatch, "/api/v1/status-pages/sp1", `{"visibility":"unlisted"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	if sp.Visibility != domain.VisibilityUnlisted || sp.UnlistedToken == "" {
		t.Fatalf("switch to unlisted = %+v, want token minted", sp)
	}
}

func TestDeleteStatusPageAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, outsider, http.MethodDelete, "/api/v1/status-pages/sp1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider delete = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/status-pages/sp1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/status-pages/sp1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
	// Gone afterwards.
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/status-pages/sp1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

func TestAuthedRenderIsolation(t *testing.T) {
	h := newHandler(seededStore())
	// Member renders the internal page.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/status-pages/sp2/render", ""); rec.Code != http.StatusOK {
		t.Fatalf("member render internal = %d, want 200", rec.Code)
	}
	// Outsider cannot.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/status-pages/sp2/render", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider render = %d, want 404", rec.Code)
	}
}

func TestPublicRenderVisibilityGate(t *testing.T) {
	h := newPublicHandler(seededStore())
	// Public page: served, with the monitor-backed component and a summary.
	rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d, want 200", rec.Code)
	}
	var render struct {
		Summary    string `json:"summary"`
		Components []struct {
			Name   string  `json:"name"`
			Status string  `json:"status"`
			Uptime float64 `json:"uptime_90d"`
		} `json:"components"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &render)
	if len(render.Components) != 1 || render.Components[0].Name != "API" {
		t.Fatalf("expected 1 component 'API', got %+v", render.Components)
	}
	if render.Summary == "" {
		t.Fatal("summary should be set")
	}

	// Internal page: hidden from the public endpoint.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/internal-status", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("public render internal = %d, want 404", rec.Code)
	}

	// Unlisted: 404 without token, 200 with the matching token.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted without token = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status?token=wrong", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted wrong token = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/secret-status?token=tok123", ""); rec.Code != http.StatusOK {
		t.Fatalf("unlisted correct token = %d, want 200", rec.Code)
	}
}
