package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

func TestCreateMonitorMethodAndGrace(t *testing.T) {
	h := newHandler(seededStore())
	var m struct {
		Method       string `json:"method"`
		GraceSeconds int    `json:"grace_seconds"`
	}

	// HTTP method is normalized to upper-case and echoed back.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"api","type":"http","target":"https://x","method":"post","interval_seconds":60,"timeout_seconds":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create http = %d, want 201", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Method != "POST" {
		t.Fatalf("method = %q, want POST", m.Method)
	}

	// An unsupported method is rejected.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"x","type":"http","target":"https://x","method":"TRACE","interval_seconds":60,"timeout_seconds":5}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid method = %d, want 400", rec.Code)
	}

	// Push grace persists; a non-http monitor carries no method (omitted from JSON).
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"cron","type":"push","interval_seconds":600,"grace_seconds":120}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create push = %d, want 201", rec.Code)
	}
	var push struct {
		Method       string `json:"method"`
		GraceSeconds int    `json:"grace_seconds"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &push)
	if push.GraceSeconds != 120 || push.Method != "" {
		t.Fatalf("push monitor = %+v, want grace 120 / no method", push)
	}
}

func TestCreateMonitorAutoIncident(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	// Omitted → defaults to true (auto-incidents on).
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"a","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	var created struct {
		ID           string `json:"id"`
		AutoIncident bool   `json:"auto_incident"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if !created.AutoIncident {
		t.Fatalf("auto_incident should default to true: %+v", created)
	}

	// Explicit false is honored.
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"b","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"auto_incident":false}`)
	var off struct {
		ID           string `json:"id"`
		AutoIncident bool   `json:"auto_incident"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &off)
	if off.AutoIncident {
		t.Fatalf("auto_incident=false should be honored: %+v", off)
	}
	if fs.monitors[off.ID].AutoIncident {
		t.Fatal("stored monitor should have AutoIncident=false")
	}

	// PATCH toggles it back on; omitting it leaves it unchanged.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+off.ID, `{"auto_incident":true}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200", rec.Code)
	}
	if !fs.monitors[off.ID].AutoIncident {
		t.Fatal("PATCH auto_incident=true not applied")
	}
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+off.ID, `{"name":"b2"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch name = %d, want 200", rec.Code)
	}
	if !fs.monitors[off.ID].AutoIncident {
		t.Fatal("omitting auto_incident in PATCH should leave it unchanged (true)")
	}
}

func TestCreateMonitorFailureThreshold(t *testing.T) {
	h := newHandler(seededStore())
	// Omitted → normalized to 1 (immediate).
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"a","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5}`)
	var a struct {
		FailureThreshold int `json:"failure_threshold"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	if a.FailureThreshold != 1 {
		t.Fatalf("omitted failure_threshold = %d, want 1", a.FailureThreshold)
	}
	// Explicit values are kept (failure_threshold + renotify_seconds).
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"b","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"failure_threshold":3,"renotify_seconds":300}`)
	var b struct {
		ID               string `json:"id"`
		FailureThreshold int    `json:"failure_threshold"`
		RenotifySeconds  int    `json:"renotify_seconds"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.FailureThreshold != 3 || b.RenotifySeconds != 300 {
		t.Fatalf("thresholds = %d/%d, want 3/300", b.FailureThreshold, b.RenotifySeconds)
	}
	// PATCH updates it.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+b.ID, `{"failure_threshold":5}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200", rec.Code)
	}
}

func TestCreateCompositeMonitor(t *testing.T) {
	h := newHandler(seededStore())
	// Composite with an in-project child (mon1 is in p1) → 201.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"group","type":"composite","interval_seconds":60,"config":{"children":"mon1","mode":"all"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("valid composite = %d, want 201", rec.Code)
	}
	// A cross-project child (mon3 is in p3) → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"group","type":"composite","interval_seconds":60,"config":{"children":"mon3"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-project child = %d, want 400", rec.Code)
	}
	// No children → 400 (domain validation).
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"group","type":"composite","interval_seconds":60}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no children = %d, want 400", rec.Code)
	}
}

func TestPostgresMonitorRedactionAndPreserve(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	// Create with a password → the response redacts it, but the store keeps it.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"db","type":"postgres","target":"db:5432","interval_seconds":60,"timeout_seconds":5,"config":{"username":"cerbix","password":"s3cr3t","database":"app"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create postgres = %d, want 201", rec.Code)
	}
	var created struct {
		ID     string            `json:"id"`
		Config map[string]string `json:"config"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Config["password"] != "" {
		t.Fatalf("password leaked in create response: %q", created.Config["password"])
	}
	if created.Config["username"] != "cerbix" {
		t.Fatalf("non-secret config lost: %+v", created.Config)
	}
	if fs.monitors[created.ID].Config["password"] != "s3cr3t" {
		t.Fatalf("stored password = %q, want s3cr3t", fs.monitors[created.ID].Config["password"])
	}

	// GET redacts too.
	rec = do(h, o1Admin, http.MethodGet, "/api/v1/monitors/"+created.ID, "")
	var got struct {
		Config map[string]string `json:"config"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Config["password"] != "" {
		t.Fatalf("GET leaked password: %q", got.Config["password"])
	}

	// PATCH with an empty password preserves the stored secret; other fields update.
	rec = do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+created.ID,
		`{"config":{"username":"cerbix2","password":"","database":"app"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200", rec.Code)
	}
	if fs.monitors[created.ID].Config["password"] != "s3cr3t" {
		t.Fatalf("password not preserved on empty submit: %q", fs.monitors[created.ID].Config["password"])
	}
	if fs.monitors[created.ID].Config["username"] != "cerbix2" {
		t.Fatalf("username not updated: %q", fs.monitors[created.ID].Config["username"])
	}
}

func TestCreateMonitorTags(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"t","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"tags":["env:prod"," env:prod ","team:api"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	var m struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	// Normalized: trimmed + de-duped.
	if len(m.Tags) != 2 || m.Tags[0] != "env:prod" || m.Tags[1] != "team:api" {
		t.Fatalf("tags = %#v, want [env:prod team:api]", m.Tags)
	}
	// PATCH replaces the set.
	if rec := do(h, o1Admin, http.MethodPatch, "/api/v1/monitors/"+m.ID, `{"tags":["tier:1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("patch tags = %d, want 200", rec.Code)
	}
	if len(fs.monitors[m.ID].Tags) != 1 || fs.monitors[m.ID].Tags[0] != "tier:1" {
		t.Fatalf("patched tags = %#v", fs.monitors[m.ID].Tags)
	}
}

func TestCreateMonitorRegion(t *testing.T) {
	h := newHandler(seededStore())
	// Omitted region → defaults to core.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"a","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	var a struct {
		ID     string `json:"id"`
		Region string `json:"region"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	if a.Region != "core" {
		t.Fatalf("omitted region = %q, want core", a.Region)
	}
	// Explicit region round-trips (normalized to lowercase).
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"b","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"region":"GEO1"}`)
	var b struct {
		Region string `json:"region"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.Region != "geo1" {
		t.Fatalf("region = %q, want geo1", b.Region)
	}
	// A bad region slug is rejected.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"c","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"region":"has spaces"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad region = %d, want 400", rec.Code)
	}
	// A composite monitor is auto-pinned to core even if a region is requested
	// (composite needs the database, which a remote worker lacks).
	rec = do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"g","type":"composite","interval_seconds":60,"region":"geo1","config":{"children":"mon1"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("composite create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var g struct {
		Region string `json:"region"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	if g.Region != "core" {
		t.Fatalf("composite region = %q, want core (auto-pinned)", g.Region)
	}
}

func TestListRegions(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	// Seed a geo1 monitor so the region list has more than core.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"g","type":"http","target":"https://x","interval_seconds":60,"timeout_seconds":5,"region":"geo1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d", rec.Code)
	}
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("regions = %d, want 200", rec.Code)
	}
	var out struct {
		Regions []struct {
			Name string `json:"name"`
			Live bool   `json:"live"`
		} `json:"regions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var hasCore, hasGeo1 bool
	for _, r := range out.Regions {
		hasCore = hasCore || r.Name == "core"
		hasGeo1 = hasGeo1 || r.Name == "geo1"
		if r.Live { // no live-region source wired in tests
			t.Fatalf("region %q unexpectedly live", r.Name)
		}
	}
	if !hasCore || !hasGeo1 {
		t.Fatalf("regions = %#v, want core+geo1", out.Regions)
	}
}

type fakeProbe struct {
	hb  domain.Heartbeat
	err error
}

func (f fakeProbe) RunTest(context.Context, domain.Monitor) (domain.Heartbeat, error) {
	return f.hb, f.err
}

func TestTestMonitor(t *testing.T) {
	fs := seededStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.New(fs, logger, 8).WithTester(fakeProbe{hb: domain.Heartbeat{Up: true, LatencyMS: 42, Code: 200, Msg: "ok"}}).Router()

	// A testable http monitor → 200 with the probe result.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test",
		`{"type":"http","target":"https://x","timeout_seconds":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test http = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var res struct {
		Up        bool  `json:"up"`
		LatencyMS int64 `json:"latency_ms"`
		Code      int   `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Up || res.LatencyMS != 42 || res.Code != 200 {
		t.Fatalf("test result = %+v", res)
	}
	// Push and composite are not testable.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test", `{"type":"push"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("test push = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test", `{"type":"composite"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("test composite = %d, want 400", rec.Code)
	}
	// Viewer may not test (project write).
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/projects/p1/monitors/test", `{"type":"http","target":"https://x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer test = %d, want 403", rec.Code)
	}

	// No worker in the target region → 502 (result unknown, not a target outage).
	hNoWorker := api.New(fs, logger, 8).
		WithTester(fakeProbe{err: errors.New(`no worker responded in region "geo1"`)}).Router()
	if rec := do(hNoWorker, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors/test",
		`{"type":"http","target":"https://x","region":"geo1"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("test no-worker = %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
}

// TestMonitorTargetWithUserinfoIsRefused is the API half of the D-0145 addendum. Go's
// net/http turns https://user:pass@host into an Authorization header on its own, so such a
// target WORKS — and the password then sits in `monitors.target`, which Redacted() does not
// blank, so every viewer of the project reads it in the monitor list. The file provider has
// always refused it; this proves the UI/API surface refuses it too, and that the refusal does
// not echo the credential back to the caller.
func TestMonitorTargetWithUserinfoIsRefused(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"prom","type":"promql","target":"https://scanner:s3cr3t-v4lue@prom.internal:9090","interval_seconds":60,"timeout_seconds":5,"config":{"query":"up"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with userinfo = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cr3t-v4lue") {
		t.Fatalf("the refusal echoed the credential: %s", rec.Body.String())
	}
	// The same monitor without credentials in the target is accepted, so the guard is about
	// the userinfo and not about the type or the URL.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors",
		`{"name":"prom","type":"promql","target":"https://prom.internal:9090","interval_seconds":60,"timeout_seconds":5,"config":{"query":"up"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create without userinfo = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

// TestSyntheticScenarioIsWriterOnly is the API half of FR-028 stage 1: a viewer's detail and
// list carry no scenario, a writer's do. The point is not the response shape but the
// authorization boundary — a viewer who can read a monitor must not read the credential the
// scenario may still hold until stage 2.
func TestSyntheticScenarioIsWriterOnly(t *testing.T) {
	fs := seededStore()
	fs.monitors["m-syn"] = domain.Monitor{
		ID: "m-syn", ProjectID: "p1", Name: "journey", Type: domain.MonitorSynthetic,
		IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
		Config: map[string]string{"scenario": `{"steps":[{"url":"https://x","headers":{"authorization":"Bearer s3cr3t-bearer"}}]}`},
	}
	h := newHandler(fs)

	for _, tc := range []struct {
		name  string
		who   authz.Principal
		wants bool
	}{
		{"viewer", p1Viewer, false},
		{"admin", o1Admin, true},
	} {
		t.Run(tc.name+" detail", func(t *testing.T) {
			rec := do(h, tc.who, http.MethodGet, "/api/v1/monitors/m-syn", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("detail = %d (%s)", rec.Code, rec.Body.String())
			}
			carries := strings.Contains(rec.Body.String(), "s3cr3t-bearer")
			if carries != tc.wants {
				t.Fatalf("%s detail carries the scenario = %v, want %v: %s", tc.name, carries, tc.wants, rec.Body.String())
			}
		})
		t.Run(tc.name+" list", func(t *testing.T) {
			rec := do(h, tc.who, http.MethodGet, "/api/v1/projects/p1/monitors", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list = %d (%s)", rec.Code, rec.Body.String())
			}
			carries := strings.Contains(rec.Body.String(), "s3cr3t-bearer")
			if carries != tc.wants {
				t.Fatalf("%s list carries the scenario = %v, want %v", tc.name, carries, tc.wants)
			}
		})
	}
}
