package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 phase 2, changeset 2 (iter-0141): the reporting API. The handlers return the
// reviewed domain payloads verbatim; these tests pin the HTTP contract — authz, tenant 404s,
// parameter validation, passthrough — over the fake store's canned payloads.

// seedReportService plants a service with a REAL uuid-shaped ID directly in the fake: the
// production store mints gen_random_uuid(), and the reporting routes validate the uuid at
// the transport, so the fake's "svc-"+slug fiction cannot ride these paths.
func seedReportService(fs *fakeStore, projectID string) string {
	id := "12345678-9abc-4def-8123-456789abcdef"
	fs.serviceStore()[id] = &fakeService{svc: domain.Service{ID: id, ProjectID: projectID, Slug: "checkout", Name: "Checkout"}}
	return id
}

func TestServiceReportingAuthzAndTenancy(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedReportService(fs, "p1")

	reads := []string{
		"/api/v1/projects/p1/services/" + id + "/reliability",
		"/api/v1/projects/p1/services/" + id + "/health",
		"/api/v1/projects/p1/services/" + id + "/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=hour",
	}
	for _, path := range reads {
		if rec := do(h, p1Viewer, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}
	// The SLA target is a write.
	if rec := do(h, p1Viewer, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/sla-target",
		`{"window":"30d","objective":99.9}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer PUT sla-target = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/sla-target",
		`{"window":"7d","objective":99.5}`); rec.Code != http.StatusOK {
		t.Errorf("editor PUT sla-target = %d: %s", rec.Code, rec.Body.String())
	}
	if fs.reporting().slaWindow != "7d" || fs.reporting().slaObj != 99.5 {
		t.Errorf("sla target recorded %q/%v, want 7d/99.5", fs.reporting().slaWindow, fs.reporting().slaObj)
	}

	// Cross-tenant and well-formed-but-absent IDs are 404 — a report must not double as an
	// existence oracle — while a MALFORMED ID is the transport's own 400, so the store never
	// sees a value PostgreSQL would reject with a uuid cast error ([192] P1-2).
	absent := "00000000-0000-0000-0000-00000000dead"
	for _, path := range []string{
		"/api/v1/projects/p3/services/" + id + "/reliability",
		"/api/v1/projects/p3/services/" + id + "/health",
		"/api/v1/projects/p3/services/" + id + "/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=hour",
		"/api/v1/projects/p1/services/" + absent + "/reliability",
		"/api/v1/projects/p1/services/" + absent + "/health",
	} {
		if rec := do(h, p1Editor, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	for _, path := range []string{
		"/api/v1/projects/p1/services/nonexistent/reliability",
		"/api/v1/projects/p1/services/nonexistent/health",
		"/api/v1/projects/p1/services/nonexistent/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=hour",
	} {
		if rec := do(h, p1Editor, http.MethodGet, path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 (malformed uuid is the transport's problem)", path, rec.Code)
		}
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/nonexistent/sla-target",
		`{"window":"30d","objective":99.9}`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed-uuid PUT sla-target = %d, want 400", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p3/services/"+id+"/sla-target",
		`{"window":"30d","objective":99.9}`); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant PUT sla-target = %d, want 404", rec.Code)
	}
}

func TestServiceReportingValidatesItsParameters(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedReportService(fs, "p1")
	base := "/api/v1/projects/p1/services/" + id

	// Windows are the standard set only; the default is 30d.
	if rec := do(h, p1Editor, http.MethodGet, base+"/reliability?window=5m", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("window=5m = %d, want 400", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodGet, base+"/reliability?window=90d", ""); rec.Code != http.StatusOK {
		t.Errorf("window=90d = %d", rec.Code)
	}
	if fs.reporting().lastWindow != "90d" {
		t.Errorf("store received window %q, want 90d", fs.reporting().lastWindow)
	}
	if rec := do(h, p1Editor, http.MethodGet, base+"/reliability", ""); rec.Code != http.StatusOK {
		t.Errorf("default window = %d", rec.Code)
	}
	if fs.reporting().lastWindow != "30d" {
		t.Errorf("default window reached the store as %q, want 30d", fs.reporting().lastWindow)
	}

	// The series is bounded and typed: RFC3339, ordered, capped at 90d, step hour|day.
	for _, q := range []string{
		"from=yesterday&to=2026-08-02T00:00:00Z&step=hour",
		"from=2026-08-01T00:00:00Z&to=tomorrow&step=hour",
		"from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z&step=hour",
		"from=2026-08-01T00:00:00Z&to=2026-08-01T00:00:00Z&step=hour",
		"from=2026-01-01T00:00:00Z&to=2026-08-01T00:00:00Z&step=hour",
		"from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=minute",
		"from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z",
	} {
		if rec := do(h, p1Editor, http.MethodGet, base+"/reliability/series?"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("series?%s = %d, want 400", q, rec.Code)
		}
	}
	if rec := do(h, p1Editor, http.MethodGet, base+"/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=day", ""); rec.Code != http.StatusOK {
		t.Errorf("valid series = %d", rec.Code)
	}
	if fs.reporting().lastStep != 24*time.Hour {
		t.Errorf("store received step %s, want 24h", fs.reporting().lastStep)
	}

	// The SLA-target body is {window, objective} and NOTHING else (§13, invariant 47):
	// a burn field of any spelling is 400 at the decoder, before any store call.
	for _, body := range []string{
		`{"window":"30d","objective":99.9,"burn_alert":true}`,
		`{"window":"30d","objective":99.9,"burn_rules":[]}`,
		`{"window":"30d","objective":99.9}{"burn_alert":true}`,
		`{"window":"5m","objective":99.9}`,
		`{"window":"30d","objective":0}`,
		`{"window":"30d","objective":0.00001}`,
		`{"window":"30d","objective":100}`,
		`{"window":"30d","objective":99.99995}`,
		`{"window":"30d","objective":100.00004}`,
		`{"window":"30d","objective":101}`,
	} {
		if rec := do(h, p1Editor, http.MethodPut, base+"/sla-target", body); rec.Code != http.StatusBadRequest {
			t.Errorf("sla-target %s = %d, want 400", body, rec.Code)
		}
	}

	// The contract's admissible boundary is REPRESENTABLE and the echo is the canonical
	// value ([192] P1-1, [195] P0/D-0165): the maximum objective is 99.9999, exactly.
	for _, tc := range []struct {
		body string
		want float64
	}{
		{`{"window":"30d","objective":99.9999}`, 99.9999},
		{`{"window":"30d","objective":99.99994}`, 99.9999},
		{`{"window":"30d","objective":99.5}`, 99.5},
	} {
		rec := do(h, p1Editor, http.MethodPut, base+"/sla-target", tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("sla-target %s = %d: %s", tc.body, rec.Code, rec.Body.String())
		}
		var out struct {
			Objective float64 `json:"objective"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Objective != tc.want {
			t.Errorf("sla-target %s echoed %v (%v), want the canonical %v", tc.body, out.Objective, err, tc.want)
		}
		if fs.reporting().slaObj != tc.want {
			t.Errorf("store received %v for %s, want the canonical %v", fs.reporting().slaObj, tc.body, tc.want)
		}
	}

	// An empty interval is a NON-NULL array on the wire ([192] P2-1), whatever the store
	// hands back.
	fs.reporting().series = nil
	rec := do(h, p1Editor, http.MethodGet, base+"/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=hour", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty series = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"points":[]`) {
		t.Errorf("empty series wire = %s, want \"points\":[]", rec.Body.String())
	}
}

// The payload IS the reviewed domain type: what iter-0138…0140 shipped is what the wire
// carries — statuses, reasons, withheld labels, burn verdict fields, the unstable flag.
func TestServiceReportingReturnsTheDomainPayloadVerbatim(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedReportService(fs, "p1")

	avail := 99.25
	sealed := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fs.reporting().report = domain.ServiceWindowReport{
		AsOf: sealed.Add(time.Minute), SealedThrough: &sealed,
		Status: domain.ServiceReportPartial, Reason: domain.ServiceReportReasonLowCoverage,
		StorageContinuity: true, ExpectedBuckets: 43200, SealedBuckets: 43200, Coverage: 0.9,
		Availability:      &avail,
		AggregateWithheld: "",
		Burn: []domain.ServiceBurnWindow{{
			Window: "1h", Status: domain.ServiceReportInsufficientSealed,
			Reason:          domain.ServiceReportReasonStaleWatermark,
			ExpectedBuckets: 60, SealedBuckets: 60, StorageContinuity: true, Coverage: 1,
		}},
		Segments: []domain.ReliabilitySegment{{
			RevisionID: "rev", Revision: 1, EpochID: "ep", EpochSeq: 1,
			DeclaredReconstruction: true, Coverage: 0.9,
		}},
	}
	fs.reporting().health = domain.ServiceHealthNow{
		Unstable: true, AsOf: sealed, SLI: "degraded", Diagnostics: "failing",
		FailingMonitors: []string{"redis"},
	}
	fs.reporting().series = []domain.ReliabilitySeriesPoint{{
		EpochID: "ep", RevisionID: "rev", Provisional: true, Buckets: 30,
	}}

	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id+"/reliability?window=24h", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reliability = %d: %s", rec.Code, rec.Body.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep["status"] != "partial" || rep["reason"] != "decidable_coverage_below_min" {
		t.Errorf("status/reason = %v/%v", rep["status"], rep["reason"])
	}
	if rep["availability"] != 99.25 || rep["window"] != "24h" || rep["service_id"] != id {
		t.Errorf("availability/window/service = %v/%v/%v", rep["availability"], rep["window"], rep["service_id"])
	}
	burn := rep["burn"].([]any)[0].(map[string]any)
	if burn["storage_continuity"] != true || burn["coverage"] != 1.0 || burn["reason"] != "sealed_through_behind_window" {
		t.Errorf("burn verdict fields lost on the wire: %v", burn)
	}
	seg := rep["segments"].([]any)[0].(map[string]any)
	if seg["declared_reconstruction"] != true {
		t.Errorf("reconstruction label lost: %v", seg)
	}

	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id+"/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	var hlt map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &hlt); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if hlt["unstable"] != true || hlt["sli"] != "degraded" || hlt["diagnostics"] != "failing" {
		t.Errorf("health payload = %v", hlt)
	}

	rec = do(h, p1Viewer, http.MethodGet,
		"/api/v1/projects/p1/services/"+id+"/reliability/series?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z&step=hour", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series = %d", rec.Code)
	}
	var ser map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ser); err != nil {
		t.Fatalf("decode series: %v", err)
	}
	if ser["step"] != "hour" {
		t.Errorf("series step echo = %v", ser["step"])
	}
	pt := ser["points"].([]any)[0].(map[string]any)
	if pt["provisional"] != true || pt["epoch_id"] != "ep" {
		t.Errorf("series point lost fields: %v", pt)
	}
}

// [200] P1-1: D-0165's open interval binds the PRE-EXISTING monitor surface too — one
// objective rule, both scopes, at the HTTP boundary.
func TestMonitorSLATargetSharesTheObjectiveRule(t *testing.T) {
	h := newHandler(seededStore())
	for _, body := range []string{
		`{"objective":100,"window":"30d"}`,
		`{"objective":99.99995,"window":"30d"}`,
		`{"objective":100.00004,"window":"30d"}`,
	} {
		if rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", body); rec.Code != http.StatusBadRequest {
			t.Errorf("monitor sla-target %s = %d, want 400", body, rec.Code)
		}
	}
	// The maximum admissible objective passes and the stored target carries the canonical value.
	rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":99.99994,"window":"30d"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor sla-target 99.99994 = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Objective float64 `json:"objective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Objective != 99.9999 {
		t.Errorf("monitor target echoed %v (%v), want the canonical 99.9999", out.Objective, err)
	}
}
