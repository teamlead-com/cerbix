package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-042 §4.6 / D15 / I11: the basis of an incident's impact is served by the authenticated
// incident detail and by nothing else.

func basisShapes() map[string]*domain.ImpactBasis {
	m := domain.ImpactMapping{Degraded: domain.ImpactMinor, Down: domain.ImpactCritical, Unknown: domain.ImpactNone}
	return map[string]*domain.ImpactBasis{
		"service_mapping": {Source: domain.ImpactBasisServiceMapping, Mapping: &m, State: domain.ServiceAlertDown,
			Service: &domain.ImpactBasisService{Name: "Checkout", Slug: "checkout"}},
		"project_default": {Source: domain.ImpactBasisProjectDefault},
		"manual":          {Source: domain.ImpactBasisManual},
		"alertmanager":    {Source: domain.ImpactBasisAlertmanager},
	}
}

func TestIncidentDetailCarriesTheBasisInEachShape(t *testing.T) {
	for name, want := range basisShapes() {
		t.Run(name, func(t *testing.T) {
			fs := seededStore()
			fs.impactBases = map[string]*domain.ImpactBasis{"inc1": want}
			rec := do(newHandler(fs), o1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				ImpactBasis *domain.ImpactBasis `json:"impact_basis"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got.ImpactBasis)
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("impact_basis = %s, want %s", gotJSON, wantJSON)
			}
			if strings.Contains(rec.Body.String(), "impact_basis_unavailable") {
				t.Errorf("a readable basis flagged unavailable: %s", rec.Body.String())
			}
		})
	}
	t.Run("not recorded", func(t *testing.T) {
		rec := do(newHandler(seededStore()), o1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `"impact_basis":null`) ||
			strings.Contains(body, "impact_basis_unavailable") {
			t.Errorf("NULL basis = %d %s", rec.Code, body)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		fs := seededStore()
		fs.impactBasisErr = errors.New("boom")
		rec := do(newHandler(fs), o1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `"impact_basis":null`) ||
			!strings.Contains(body, `"impact_basis_unavailable":true`) {
			t.Errorf("read error = %d %s", rec.Code, body)
		}
	})
}

func TestTheBasisAppearsNowhereElse(t *testing.T) {
	fs := seededStore()
	fs.impactBases = map[string]*domain.ImpactBasis{"inc1": basisShapes()["service_mapping"]}
	// Make the incident visible on the public page and feeds.
	now := time.Now()
	inc := fs.incidents["inc1"]
	inc.StartedAt = now
	fs.incidents["inc1"] = inc
	// The history pages list RESOLVED incidents only, so they get one of their own, with a basis.
	resolvedAt := now.Add(-time.Hour)
	fs.incidents["incH"] = domain.Incident{ID: "incH", ProjectID: "p1", Title: "resolved earlier",
		Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual,
		StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &resolvedAt}
	fs.impactBases["incH"] = basisShapes()["service_mapping"]
	authed, public := newHandler(fs), newPublicHandler(fs)
	for _, c := range []struct {
		name, method, path string
		public             bool
	}{
		{"list", http.MethodGet, "/api/v1/projects/p1/incidents", false},
		{"acknowledge", http.MethodPost, "/api/v1/incidents/inc1/acknowledge", false},
		{"public page", http.MethodGet, "/api/v1/public/status-pages/acme-status", true},
		{"public feed", http.MethodGet, "/api/v1/public/status-pages/acme-status/feed", true},
		{"public atom", http.MethodGet, "/api/v1/public/status-pages/acme-status/feed?format=atom", true},
		{"public json", http.MethodGet, "/api/v1/public/status-pages/acme-status/feed?format=json", true},
		{"authed feed", http.MethodGet, "/api/v1/status-pages/sp1/feed", false},
		{"public history", http.MethodGet, "/api/v1/public/status-pages/acme-status/history", true},
		{"authed render", http.MethodGet, "/api/v1/status-pages/sp1/render", false},
		{"authed history", http.MethodGet, "/api/v1/status-pages/sp1/history", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := authed
			if c.public {
				h = public
			}
			p := p1Editor
			if strings.HasPrefix(c.name, "authed ") {
				p = o1Admin
			}
			rec := do(h, p, c.method, c.path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d: %s", c.path, rec.Code, rec.Body.String())
			}
			shown := "inc1"
			if strings.HasSuffix(c.name, "history") {
				shown = "incH"
			}
			if !strings.Contains(rec.Body.String(), shown) && !strings.Contains(rec.Body.String(), fs.incidents[shown].Title) {
				t.Fatalf("%s does not carry the incident at all, so the absence below proves nothing: %s", c.path, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "impact_basis") {
				t.Errorf("%s leaked the basis: %s", c.path, rec.Body.String())
			}
		})
	}
}

func TestNoRouteEditsAnExistingIncidentsImpact(t *testing.T) {
	fs := seededStore()
	fs.incidents["incR"] = domain.Incident{ID: "incR", ProjectID: "p1", Title: "done",
		Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual}
	h := newHandler(fs)

	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/incidents/inc1/updates",
		`{"body":"x","impact":"critical"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("update with impact = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/incidents/incR/postmortem",
		`{"body":"x","impact":"critical"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("postmortem with impact = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	before := fs.incidents["inc1"].Impact
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/incidents/inc1/acknowledge", `{"impact":"critical"}`); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge with impact = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if after := fs.incidents["inc1"].Impact; after != before {
		t.Errorf("acknowledge changed the impact %s -> %s", before, after)
	}
	for _, m := range []string{http.MethodPatch, http.MethodPut} {
		rec := do(h, p1Editor, m, "/api/v1/incidents/inc1", `{"impact":"critical"}`)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s /incidents/inc1 = %d, want 405 or 404", m, rec.Code)
		}
	}
	if fs.incidents["inc1"].Impact != before {
		t.Errorf("the impact moved")
	}
}
