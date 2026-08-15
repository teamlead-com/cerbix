package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The retroactive-maintenance contract has to be on the PRODUCT PATH. The handlers called the
// legacy store functions — a direct INSERT and a hard DELETE — so a retroactive window
// silently restated sealed numbers with no preview, no fence and no audit, and a delete
// destroyed the retained exclusion row that explained why a sealed window excluded what it
// did. The whole checked family existed and was referenced only by its own tests.
func TestRetroactiveMaintenanceIsGatedOnTheProductPath(t *testing.T) {
	fs := seededStore()
	fs.sealedThrough = time.Now().UTC().Add(-time.Hour)
	h := newHandler(fs)

	start := time.Now().UTC().Add(-3 * time.Hour)
	end := start.Add(30 * time.Minute)
	body := func(previewID string) string {
		return `{"monitor_id":"mon1","starts_at":"` + start.Format(time.RFC3339Nano) +
			`","ends_at":"` + end.Format(time.RFC3339Nano) + `","reason":"retro","preview_id":"` + previewID + `"}`
	}

	// Without a token the write is refused, not quietly applied.
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance", body(""))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "preview_required") {
		t.Fatalf("retroactive write without a preview = %d %s, want 409 preview_required", rec.Code, rec.Body.String())
	}

	// A token issued for THIS mutation lets it through…
	rec = do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance/preview",
		`{"monitor_id":"mon1","mutation":"create","starts_at":"`+start.Format(time.RFC3339Nano)+
			`","ends_at":"`+end.Format(time.RFC3339Nano)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body.String())
	}
	var p struct {
		PreviewID string `json:"preview_id"`
		Services  []any  `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if p.PreviewID == "" {
		t.Fatal("no preview token issued")
	}
	if p.Services == nil {
		t.Error("services must be an array (never null): the operator is being shown what would be restated")
	}
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance", body(p.PreviewID)); rec.Code != http.StatusCreated {
		t.Fatalf("the mutation its own token authorized = %d %s", rec.Code, rec.Body.String())
	}

	// …and the SAME token does not authorize a different range.
	rec = do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance",
		`{"monitor_id":"mon1","starts_at":"`+start.Format(time.RFC3339Nano)+
			`","ends_at":"`+start.Add(9*time.Hour).Format(time.RFC3339Nano)+
			`","reason":"widened","preview_id":"`+p.PreviewID+`"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "preview_stale") {
		t.Errorf("a token authorized a widened window: %d %s", rec.Code, rec.Body.String())
	}
}

// A prospective window changes no settled number, so it needs no ceremony. Without this the
// gate would be a tax on the ordinary case.
func TestProspectiveMaintenanceNeedsNoPreview(t *testing.T) {
	fs := seededStore()
	fs.sealedThrough = time.Now().UTC().Add(-time.Hour)
	h := newHandler(fs)

	start := time.Now().UTC().Add(2 * time.Hour)
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance",
		`{"monitor_id":"mon1","starts_at":"`+start.Format(time.RFC3339Nano)+
			`","ends_at":"`+start.Add(time.Hour).Format(time.RFC3339Nano)+`","reason":"planned"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("prospective window = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// DELETE archives; it does not destroy. Removing a window's PAST effect is a different
// operation, and it is called annul.
func TestDeletingAMaintenanceWindowArchivesRatherThanDestroys(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	start := time.Now().UTC().Add(2 * time.Hour)
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/maintenance",
		`{"monitor_id":"mon1","starts_at":"`+start.Format(time.RFC3339Nano)+
			`","ends_at":"`+start.Add(time.Hour).Format(time.RFC3339Nano)+`","reason":"planned"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/maintenance/mw-new", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	list := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/maintenance", "")
	if !strings.Contains(list.Body.String(), "archived") {
		t.Errorf("the window is gone rather than archived: %s", list.Body.String())
	}
}
