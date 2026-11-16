package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-043 I10 at the API create surface (func-recovery-threshold §4.5 "Absent is not zero"): an
// omitted recovery_threshold is 1, an explicit value — 0 included — is copied verbatim and judged by
// Monitor.Validate, so 0 and 11 are refused, not clamped, and a push or composite monitor takes
// exactly 1 (D8). Every refusal is a 400 that names the rule.
func TestRecoveryThresholdOnCreate(t *testing.T) {
	h := newHandler(seededStore())
	create := func(body string) (int, string) {
		rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", body)
		return rec.Code, rec.Body.String()
	}

	refused := []struct {
		name, body, rule string
	}{
		{"explicit zero", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":0}`,
			"recovery_threshold must be between 1 and 10, got 0"},
		{"above ten", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":11}`,
			"recovery_threshold must be between 1 and 10, got 11"},
		{"negative", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":-1}`,
			"recovery_threshold must be between 1 and 10, got -1"},
		{"push above one", `{"name":"a","type":"push","interval_seconds":3600,"recovery_threshold":2}`,
			"recovery_threshold must be 1 for push monitors, got 2"},
		{"composite above one", `{"name":"a","type":"composite","interval_seconds":60,"config":{"children":"mon1","mode":"all"},"recovery_threshold":2}`,
			"recovery_threshold must be 1 for composite monitors, got 2"},
	}
	for _, tc := range refused {
		code, body := create(tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.rule) {
			t.Errorf("%s: %d %s, want 400 naming %q", tc.name, code, strings.TrimSpace(body), tc.rule)
		}
	}

	accepted := []struct {
		name, body string
		want       int
	}{
		{"omitted", `{"name":"a","type":"tcp","target":"db:5432"}`, 1},
		{"explicit one", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":1}`, 1},
		{"three", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":3}`, 3},
		{"ten", `{"name":"a","type":"tcp","target":"db:5432","recovery_threshold":10}`, 10},
		{"push omitted", `{"name":"a","type":"push","interval_seconds":3600}`, 1},
		{"push explicit one", `{"name":"a","type":"push","interval_seconds":3600,"recovery_threshold":1}`, 1},
		{"composite omitted", `{"name":"a","type":"composite","interval_seconds":60,"config":{"children":"mon1","mode":"all"}}`, 1},
	}
	for _, tc := range accepted {
		code, body := create(tc.body)
		if code != http.StatusCreated {
			t.Errorf("%s: %d %s, want 201", tc.name, code, strings.TrimSpace(body))
			continue
		}
		var got struct {
			RecoveryThreshold    int     `json:"recovery_threshold"`
			ConsecutiveSuccesses *int    `json:"consecutive_successes"`
			RecoveryStartedAt    *string `json:"recovery_started_at"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.RecoveryThreshold != tc.want {
			t.Errorf("%s: recovery_threshold = %d, want %d", tc.name, got.RecoveryThreshold, tc.want)
		}
		// The read model carries the run as runtime fields: 0 and null on a monitor that has none.
		if got.ConsecutiveSuccesses == nil || *got.ConsecutiveSuccesses != 0 || got.RecoveryStartedAt != nil {
			t.Errorf("%s: run fields = %v / %v, want 0 / null", tc.name, got.ConsecutiveSuccesses, got.RecoveryStartedAt)
		}
	}
}

// FR-043 I10 at the API update surface: an omitted recovery_threshold keeps the STORED value (not 1,
// not 0), an explicit one is judged by Validate exactly as on create, and an existing push or
// composite monitor cannot be raised above 1 (D8).
func TestRecoveryThresholdOnUpdate(t *testing.T) {
	fs := seededStore()
	fs.monitors["monr"] = domain.Monitor{RecoveryThreshold: 3, ID: "monr", ProjectID: "p1", Name: "soak",
		Type: domain.MonitorTCP, Target: "db:5432", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	fs.monitors["monc"] = domain.Monitor{RecoveryThreshold: 1, ID: "monc", ProjectID: "p1", Name: "group",
		Type: domain.MonitorComposite, IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
		Config: map[string]string{"children": "mon1", "mode": "all"}}
	h := newHandler(fs)
	patch := func(id, body string) (int, string) {
		rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+id, body)
		return rec.Code, rec.Body.String()
	}

	// Omitted: the stored 3 survives an unrelated edit, both in the response and in the store.
	code, body := patch("monr", `{"name":"soak-renamed"}`)
	if code != http.StatusOK {
		t.Fatalf("omitting patch: %d %s", code, body)
	}
	var got struct {
		RecoveryThreshold int `json:"recovery_threshold"`
	}
	_ = json.Unmarshal([]byte(body), &got)
	if got.RecoveryThreshold != 3 || fs.monitors["monr"].RecoveryThreshold != 3 {
		t.Fatalf("omitted recovery_threshold: response %d, stored %d, want 3 / 3",
			got.RecoveryThreshold, fs.monitors["monr"].RecoveryThreshold)
	}

	refused := []struct {
		name, id, body, rule string
	}{
		{"explicit zero", "monr", `{"recovery_threshold":0}`, "recovery_threshold must be between 1 and 10, got 0"},
		{"above ten", "monr", `{"recovery_threshold":11}`, "recovery_threshold must be between 1 and 10, got 11"},
		{"existing push above one", "monp", `{"recovery_threshold":2}`, "recovery_threshold must be 1 for push monitors, got 2"},
		{"existing composite above one", "monc", `{"recovery_threshold":2}`, "recovery_threshold must be 1 for composite monitors, got 2"},
	}
	for _, tc := range refused {
		before := fs.monitors[tc.id].RecoveryThreshold
		code, body := patch(tc.id, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.rule) {
			t.Errorf("%s: %d %s, want 400 naming %q", tc.name, code, strings.TrimSpace(body), tc.rule)
		}
		if after := fs.monitors[tc.id].RecoveryThreshold; after != before {
			t.Errorf("%s: a refused update changed the stored value %d → %d", tc.name, before, after)
		}
	}

	// Explicit values within the rule are written verbatim, lowering included.
	for _, step := range []struct {
		id, body string
		want     int
	}{
		{"monr", `{"recovery_threshold":10}`, 10},
		{"monr", `{"recovery_threshold":1}`, 1},
		{"mon1", `{"recovery_threshold":4}`, 4},
		{"monp", `{"recovery_threshold":1}`, 1},
		{"monc", `{"recovery_threshold":1}`, 1},
	} {
		if code, body := patch(step.id, step.body); code != http.StatusOK {
			t.Errorf("%s %s: %d %s, want 200", step.id, step.body, code, strings.TrimSpace(body))
			continue
		}
		if got := fs.monitors[step.id].RecoveryThreshold; got != step.want {
			t.Errorf("%s %s: stored %d, want %d", step.id, step.body, got, step.want)
		}
	}
}

// FR-043 I16: test-connection is not a write surface for the field (§4.5). Its request body stays
// probe-only and strict — its schema is not extended — so recovery_threshold is refused as an unknown
// field — by the decoder, not by a validation rule — while a body accepted before this change is
// still accepted.
func TestTestConnectionRefusesRecoveryThresholdAsUnknown(t *testing.T) {
	tester := &countingProbe{}
	h := api.New(seededStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithTester(tester).Router()

	for _, body := range []string{
		`{"type":"tcp","target":"localhost:5432"}`,
		`{"type":"http","target":"https://x.example","method":"GET","region":"core","timeout_seconds":5,"interval_seconds":30,"conditions":["[STATUS] == 200"],"config":{}}`,
	} {
		if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/monitors/test", body); rec.Code != http.StatusOK {
			t.Errorf("body accepted before FR-043 now answers %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	for _, v := range []string{"1", "3", "0"} {
		rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/monitors/test",
			`{"type":"tcp","target":"localhost:5432","recovery_threshold":`+v+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON body") {
			t.Errorf("recovery_threshold %s on test-connection: %d %s, want 400 as an unknown field",
				v, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}
