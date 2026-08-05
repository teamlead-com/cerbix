package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAvailabilityEndpoints(t *testing.T) {
	h := newHandler(seededStore())

	// Monitor availability (viewer can read).
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/monitors/mon1/availability?days=90", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor availability = %d, want 200", rec.Code)
	}
	var days []struct {
		Up            int64   `json:"up"`
		Total         int64   `json:"total"`
		UptimePercent float64 `json:"uptime_percent"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &days)
	if len(days) != 2 || days[1].UptimePercent != 100 {
		t.Fatalf("monitor availability body = %+v", days)
	}

	// Project availability.
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/availability", ""); rec.Code != http.StatusOK {
		t.Fatalf("project availability = %d, want 200", rec.Code)
	}

	// Isolation: another org's project hidden.
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p3/availability", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project availability = %d, want 404", rec.Code)
	}
}
