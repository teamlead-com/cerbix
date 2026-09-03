package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestSearch(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api-gateway", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	})
	inc, _ := st.CreateIncidentBySystem(ctx, domain.Incident{
		ProjectID: proj.ID, Title: "gateway latency spike", Status: domain.IncidentInvestigating,
		Impact: domain.ImpactMajor, Source: domain.SourceManual,
	}, "opening", "author")

	byType := func(hits []domain.SearchHit) map[string]domain.SearchHit {
		m := map[string]domain.SearchHit{}
		for _, h := range hits {
			m[h.Type] = h
		}
		return m
	}

	// "gate" matches the monitor name and the incident title.
	hits, err := st.Search(ctx, "gate", 8, SearchScope{AllOrgs: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	m := byType(hits)
	if m["monitor"].ID != mon.ID || m["monitor"].OrgID != org.ID || m["monitor"].Sub != "http" {
		t.Fatalf("monitor hit = %+v", m["monitor"])
	}
	if m["incident"].ID != inc.ID || m["incident"].Label != "gateway latency spike" {
		t.Fatalf("incident hit = %+v", m["incident"])
	}

	// "api" matches the project (name/slug); a project hit's ProjectID is its own id.
	hits, _ = st.Search(ctx, "api", 8, SearchScope{AllOrgs: true})
	m = byType(hits)
	if m["project"].ID != proj.ID || m["project"].ProjectID != proj.ID {
		t.Fatalf("project hit = %+v", m["project"])
	}

	// A LIKE wildcard is matched literally, not as "everything".
	if hits, _ := st.Search(ctx, "%", 8, SearchScope{AllOrgs: true}); len(hits) != 0 {
		t.Fatalf("'%%' matched %d rows, want 0 (wildcard must be escaped)", len(hits))
	}
}
