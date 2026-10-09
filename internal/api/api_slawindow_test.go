package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// iter-0204: the dashboard reads one window per card (30d) but every SLA request computed all four
// standard windows — on the pilot ~1 s per project and ~140 ms per monitor, the 24h, 7d and 90d
// results thrown away. `?window=` names the windows to compute; without it, all four as before.

func slaWindows(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Windows []struct {
			Window string `json:"window"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	out := make([]string, len(resp.Windows))
	for i, w := range resp.Windows {
		out[i] = w.Window
	}
	return out
}

func TestSLAWindowParameterComputesOnlyTheNamedWindows(t *testing.T) {
	routes := map[string]string{
		"monitor": "/api/v1/monitors/mon1/sla",
		"project": "/api/v1/projects/p1/sla",
	}
	cases := []struct {
		query string
		want  string
	}{
		{"", "24h,7d,30d,90d"},
		{"?window=30d", "30d"},
		{"?window=30d,7d", "7d,30d"},                  // standard order, whatever the request's
		{"?window=7d&window=30d&window=7d", "7d,30d"}, // repeated and duplicated
	}
	for scope, route := range routes {
		for _, c := range cases {
			fs := seededStore()
			rec := do(newHandler(fs), p1Viewer, http.MethodGet, route+c.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %q: %d %s", scope, c.query, rec.Code, rec.Body.String())
			}
			if got := strings.Join(slaWindows(t, rec.Body.Bytes()), ","); got != c.want {
				t.Errorf("%s %q: windows %s, want %s", scope, c.query, got, c.want)
			}
			if fs.sliCalls != strings.Count(c.want, ",")+1 {
				t.Errorf("%s %q: %d SLI computations for %s", scope, c.query, fs.sliCalls, c.want)
			}
		}
	}
}

func TestSLAWindowParameterRefusesAnUnknownWindowBeforeComputing(t *testing.T) {
	for _, route := range []string{"/api/v1/monitors/mon1/sla", "/api/v1/projects/p1/sla"} {
		for _, q := range []string{"?window=1y", "?window=30d,bogus", "?window=", "?window=30D"} {
			fs := seededStore()
			rec := do(newHandler(fs), p1Viewer, http.MethodGet, route+q, "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_window") {
				t.Errorf("%s%s: %d %s, want 400 invalid_window", route, q, rec.Code, rec.Body.String())
			}
			if fs.sliCalls != 0 {
				t.Errorf("%s%s: %d SLI computations before the parameter was validated", route, q, fs.sliCalls)
			}
		}
	}
}
