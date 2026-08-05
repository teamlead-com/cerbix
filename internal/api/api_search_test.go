package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestSearchTenantFiltered(t *testing.T) {
	fs := seededStore()
	// The store returns a broad match; the handler must drop hits outside the
	// caller's visibility.
	fs.searchHits = []domain.SearchHit{
		{Type: "monitor", ID: "m1", OrgID: "o1", ProjectID: "p1", Label: "api-health", Sub: "http"},
		{Type: "project", ID: "p1", OrgID: "o1", ProjectID: "p1", Label: "API", Sub: "api"},
		{Type: "incident", ID: "i9", OrgID: "o2", ProjectID: "p9", Label: "other-org outage", Sub: "investigating"},
	}
	h := newHandler(fs)

	var r struct {
		Query string             `json:"query"`
		Hits  []domain.SearchHit `json:"hits"`
	}

	// A 1-char query returns nothing (no store call needed).
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/search?q=a", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Hits) != 0 {
		t.Fatalf("1-char query returned %d hits, want 0", len(r.Hits))
	}

	// o1Viewer sees the two o1 hits, never the o2 one.
	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/search?q=api", "")
	r.Hits = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Hits) != 2 {
		t.Fatalf("hits = %d, want 2 (o1 only): %+v", len(r.Hits), r.Hits)
	}
	for _, hit := range r.Hits {
		if hit.OrgID != "o1" {
			t.Fatalf("leaked a hit from %s", hit.OrgID)
		}
	}

	// An outsider (no memberships) sees nothing.
	rec = do(h, outsider, http.MethodGet, "/api/v1/search?q=api", "")
	r.Hits = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Hits) != 0 {
		t.Fatalf("outsider got %d hits, want 0", len(r.Hits))
	}
}
