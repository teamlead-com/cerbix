package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestPublicRenderEnriched(t *testing.T) {
	fs := seededStore()
	now := time.Now()
	resolved := now.Add(-24 * time.Hour)
	// A resolved incident (with timeline + postmortem) and an upcoming maintenance
	// in the component's project (p1).
	fs.incidents["incR"] = domain.Incident{ID: "incR", ProjectID: "p1", Title: "past blip", Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual, ResolvedAt: &resolved}
	fs.incUpdates["incR"] = []domain.IncidentUpdate{
		{ID: "iuR1", IncidentID: "incR", Status: domain.IncidentInvestigating, Body: "looking into it", Author: "op-uuid"},
		{ID: "iuR2", IncidentID: "incR", Status: domain.IncidentResolved, Body: "fixed", Author: "op-uuid"},
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
				ID         string `json:"id"`
				IncidentID string `json:"incident_id"`
				Author     string `json:"author"`
			} `json:"updates"`
			Postmortem *struct {
				ID         string `json:"id"`
				IncidentID string `json:"incident_id"`
				Body       string `json:"body"`
				Author     string `json:"author"`
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
	// Public redaction: timeline updates + postmortem must NOT leak internal ids or the
	// author (a user UUID) — only body/status/timestamps are public.
	for _, u := range render.RecentIncidents[0].Updates {
		if u.ID != "" || u.IncidentID != "" || u.Author != "" {
			t.Fatalf("public update leaked internal fields: %+v", u)
		}
	}
	if pm := render.RecentIncidents[0].Postmortem; pm.ID != "" || pm.IncidentID != "" || pm.Author != "" {
		t.Fatalf("public postmortem leaked internal fields: %+v", pm)
	}
	if len(render.Maintenance) != 1 || render.Maintenance[0].ID != "mw1" {
		t.Fatalf("maintenance = %+v, want [mw1]", render.Maintenance)
	}
}

// TestPublicRenderRedactsInternalIDs proves the public (unauthenticated) status-page
// render strips internal identifiers and the ack actor from incidents/maintenance,
// while the authenticated preview keeps them.
func TestPublicRenderRedactsInternalIDs(t *testing.T) {
	fs := seededStore()
	// Give the seeded active incident (inc1, p1) sensitive internal fields.
	inc := fs.incidents["inc1"]
	inc.MonitorID = "mon1"
	inc.ExternalKey = "am-fp-SENTINEL"
	inc.AcknowledgedBy = "u1"
	inc.Source = domain.SourceAuto
	fs.incidents["inc1"] = inc

	// Public render: the external key must not appear anywhere, and the incident's
	// internal ids / ack actor must be blanked.
	pub := newPublicHandler(fs)
	rec := do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "am-fp-SENTINEL") {
		t.Fatalf("public render leaked external_key: %s", rec.Body.String())
	}
	var pubRender struct {
		ActiveIncidents []struct {
			ID             string `json:"id"`
			ProjectID      string `json:"project_id"`
			MonitorID      string `json:"monitor_id"`
			ExternalKey    string `json:"external_key"`
			AcknowledgedBy string `json:"acknowledged_by"`
		} `json:"active_incidents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pubRender); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pubRender.ActiveIncidents) != 1 {
		t.Fatalf("active incidents = %d, want 1", len(pubRender.ActiveIncidents))
	}
	got := pubRender.ActiveIncidents[0]
	if got.ProjectID != "" || got.MonitorID != "" || got.ExternalKey != "" || got.AcknowledgedBy != "" {
		t.Fatalf("public incident leaked internal fields: %+v", got)
	}

	// Authenticated preview keeps the detail (operators may need it).
	authed := newHandler(fs)
	arec := do(authed, o1Admin, http.MethodGet, "/api/v1/status-pages/sp1/render", "")
	if arec.Code != http.StatusOK {
		t.Fatalf("authed render = %d, want 200 (%s)", arec.Code, arec.Body.String())
	}
	if !strings.Contains(arec.Body.String(), "am-fp-SENTINEL") {
		t.Fatalf("authed render should retain external_key, got %s", arec.Body.String())
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
	// Monitor mon3 is in o2 → 400, and the message must be IDENTICAL to a truly
	// nonexistent id so it can't be used to enumerate cross-tenant monitor ids.
	crossOrg := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"X","monitor_id":"mon3"}`)
	if crossOrg.Code != http.StatusBadRequest {
		t.Fatalf("component with cross-org monitor = %d, want 400", crossOrg.Code)
	}
	missing := do(h, o1Admin, http.MethodPost, "/api/v1/status-pages/sp1/components", `{"name":"X","monitor_id":"does-not-exist"}`)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("component with missing monitor = %d, want 400", missing.Code)
	}
	if crossOrg.Body.String() != missing.Body.String() {
		t.Fatalf("cross-org and missing responses must be identical (no existence oracle): %q vs %q",
			crossOrg.Body.String(), missing.Body.String())
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

// FR-022 invariant 11, over the RAW unauthenticated bytes: a status page carries a
// SERVICE incident like any other, and carries no impact links at all.
//
// Two different promises are checked here because they fail differently. The anchor is
// an internal id of the same class as `monitor_id`: a page names its components by slug
// and an unauthenticated viewer has no use for the service's UUID, so the redaction list
// must have grown with the second anchor. The impact LINKS are absent for a different
// reason — §14.7 makes them a detail-only enrichment, so an unbounded public list never
// multiplies by them.
//
// The 🕸 note in the timeline is NOT a link: it is an ordinary system update, which
// §14.5 approved as rendering "through the existing system-update mechanism, unchanged".
// It has been public for monitor incidents since phase 3, and this test pins that a
// service incident inherits exactly that treatment rather than a new one.
func TestPublicRenderCarriesAServiceIncidentAndNoImpactLinks(t *testing.T) {
	fs := seededStore()
	inc := fs.incidents["inc1"]
	inc.MonitorID = ""
	inc.ServiceID = "svc-uuid-SENTINEL"
	inc.Source = domain.SourceAuto
	inc.Title = "checkout is degraded"
	fs.incidents["inc1"] = inc
	fs.incUpdates["inc1"] = append(fs.incUpdates["inc1"], domain.IncidentUpdate{
		ID: "iu-impact", IncidentID: "inc1", Status: domain.IncidentInvestigating,
		Body: domain.ImpactMarker + " probable root — db (via db → checkout).",
	})

	pub := newPublicHandler(fs)
	rec := do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "checkout is degraded") {
		t.Fatalf("the public page dropped the SERVICE incident entirely: %s", body)
	}
	if strings.Contains(body, "svc-uuid-SENTINEL") {
		t.Fatalf("the public render LEAKED the incident's service UUID — the second anchor is an "+
			"internal id of the same class as monitor_id and PublicRedacted must clear it "+
			"(FR-022 invariant 11): %s", body)
	}
	if strings.Contains(body, `"service_id"`) {
		t.Fatalf("the public render carries a service_id key at all: %s", body)
	}
	for _, k := range []string{`"probable_root"`, `"impacts"`, `"impact_links"`, `"path"`} {
		if strings.Contains(body, k) {
			t.Fatalf("the public render carries impact links (%s) — §14.7 makes them a detail-only "+
				"enrichment, never part of an unbounded public list: %s", k, body)
		}
	}
	if !strings.Contains(body, domain.ImpactMarker) {
		t.Errorf("the 🕸 timeline note vanished from the public page: §14.5 approved it rendering " +
			"through the unchanged system-update mechanism, and a service incident inherits that")
	}

	// The authenticated preview KEEPS the anchor, which is what makes the redaction targeted
	// rather than a blanket drop an operator would then have to work around.
	authed := newHandler(fs)
	arec := do(authed, o1Admin, http.MethodGet, "/api/v1/status-pages/sp1/render", "")
	if arec.Code != http.StatusOK {
		t.Fatalf("authed render = %d, want 200 (%s)", arec.Code, arec.Body.String())
	}
	if !strings.Contains(arec.Body.String(), "svc-uuid-SENTINEL") {
		t.Fatalf("the authenticated render lost the service anchor: %s", arec.Body.String())
	}
}

// D-0180 at the RENDER: the page's project set comes from the store's one owner, and a project-scoped
// page reports its OWN project's incidents even when no component points anywhere.
//
// The render used to rebuild the set from the resolved components, which is what let three surfaces
// disagree — and the review's point stands that rebuilding it "the same way" is not a single owner:
// removing the own-project arm from the handler's own loop left every API test green, because
// nothing here was asking the owner anything. This test fails if the handler stops asking.
func TestPublicRenderReportsThePagesOwnProject(t *testing.T) {
	fs := seededStore()
	// A project-scoped page whose only component is MANUAL — no monitor, no service, nothing that
	// contributes a project by itself.
	fs.pages["sp9"] = domain.StatusPage{
		ID: "sp9", OrgID: "o1", ProjectID: "p1", Slug: "own-status", Title: "Own",
		Visibility: domain.VisibilityPublic,
	}
	fs.components["c9"] = domain.Component{
		ID: "c9", StatusPageID: "sp9", OrgID: "o1", Name: "Typed by hand",
		Source: domain.ComponentSourceManual, ManualStatus: domain.CompOperational,
	}
	h := newPublicHandler(fs)

	rec := do(h, outsider, http.MethodGet, "/api/v1/public/status-pages/own-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d, want 200", rec.Code)
	}
	var render struct {
		ActiveIncidents []struct {
			ID string `json:"id"`
		} `json:"active_incidents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &render); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, i := range render.ActiveIncidents {
		if i.ID == "inc1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the page reports no incident of its own project p1 (%d active): a reader looking at "+
			"a project's status page sees nothing about that project's outage",
			len(render.ActiveIncidents))
	}
}
