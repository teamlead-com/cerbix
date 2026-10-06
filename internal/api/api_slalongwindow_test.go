package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// iter-0199 / D-0268: a window the store reports as incomplete carries `data_from`, and latency that
// starts later than availability carries `latency_from`, on both SLA reads; a monitor-backed
// status-page component carries the PUBLIC `uptime_since`. Absent when the store reports nothing.

type slaWindowJSON struct {
	Window      string  `json:"window"`
	DataFrom    *string `json:"data_from"`
	LatencyFrom *string `json:"latency_from"`
}

func readSLAWindows(t *testing.T, body []byte) []slaWindowJSON {
	t.Helper()
	var resp struct {
		Windows []slaWindowJSON `json:"windows"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Windows) == 0 {
		t.Fatal("no windows in the SLA response")
	}
	return resp.Windows
}

func TestSLAWindowsStateTheirCoverage(t *testing.T) {
	for _, path := range []string{"/api/v1/monitors/mon1/sla", "/api/v1/projects/p1/sla"} {
		fs := seededStore()
		h := newHandler(fs)
		for _, w := range readSLAWindows(t, do(h, o1Viewer, http.MethodGet, path, "").Body.Bytes()) {
			if w.DataFrom != nil || w.LatencyFrom != nil {
				t.Fatalf("%s %s: complete window carried data_from=%v latency_from=%v", path, w.Window, w.DataFrom, w.LatencyFrom)
			}
		}

		dataFrom := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
		latencyFrom := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
		fs.sliDataFrom, fs.sliLatencyFrom = &dataFrom, &latencyFrom
		for _, w := range readSLAWindows(t, do(h, o1Viewer, http.MethodGet, path, "").Body.Bytes()) {
			if w.DataFrom == nil || *w.DataFrom != "2026-08-24T00:00:00Z" {
				t.Errorf("%s %s: data_from = %v, want 2026-08-24T00:00:00Z", path, w.Window, w.DataFrom)
			}
			if w.LatencyFrom == nil || *w.LatencyFrom != "2026-09-06T00:00:00Z" {
				t.Errorf("%s %s: latency_from = %v, want 2026-09-06T00:00:00Z", path, w.Window, w.LatencyFrom)
			}
		}
	}
}

func TestPublicStatusComponentStatesUptimeSince(t *testing.T) {
	fs := seededStore()
	// A fresh handler per render: the public render is cached per page, so a second read through
	// the same handler would serve the first body regardless of the store.
	render := func() map[string]any {
		t.Helper()
		rec := do(newPublicHandler(fs), outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("public render = %d", rec.Code)
		}
		var out struct {
			Components []map[string]any `json:"components"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Components) != 1 {
			t.Fatalf("decode: %v %s", err, rec.Body.String())
		}
		return out.Components[0]
	}
	if _, present := render()["uptime_since"]; present {
		t.Fatal("a complete window carried uptime_since")
	}
	fs.monitorUptimeSince = map[string]time.Time{"mon1": time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)}
	if got := render()["uptime_since"]; got != "2026-08-24T00:00:00Z" {
		t.Fatalf("uptime_since = %v, want 2026-08-24T00:00:00Z", got)
	}
}
