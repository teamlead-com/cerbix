package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// publicRender fetches a page through the UNAUTHENTICATED router, which is the surface every
// assertion about "what a customer sees" has to go through.
func publicRender(t *testing.T, fs *fakeStore, slug string) *httptest.ResponseRecorder {
	t.Helper()
	return do(newPublicHandler(fs), authz.Principal{}, http.MethodGet,
		"/api/v1/public/status-pages/"+slug, "")
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

// The public render no longer reports an unknown as health, on any of the three inherited paths
// (§17 / D-0167), and it never publishes the internal source or the operator's reason.
func TestPublicRenderNeverPublishesUnknownAsHealth(t *testing.T) {
	fs := seededStore()
	// A manual component whose operator has said nothing, and a pending monitor.
	fs.pages["spx"] = domain.StatusPage{ID: "spx", OrgID: "o1", Slug: "x-status", Title: "X",
		Visibility: domain.VisibilityPublic}
	fs.components["cm"] = domain.Component{ID: "cm", StatusPageID: "spx", OrgID: "o1",
		Name: "Third-party CDN", Source: domain.ComponentSourceManual}
	fs.monitors["monp"] = domain.Monitor{ID: "monp", ProjectID: "p1", Name: "new",
		Type: domain.MonitorHTTP, Status: domain.StatusPending, Enabled: true}
	fs.components["cp"] = domain.Component{ID: "cp", StatusPageID: "spx", OrgID: "o1",
		Name: "New API", Source: domain.ComponentSourceMonitor, SourceProject: "p1", MonitorID: "monp"}

	var body struct {
		Summary      string `json:"summary"`
		SummaryState string `json:"summary_state"`
		Unmeasured   int    `json:"unmeasured_count"`
		Components   []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Source string `json:"source"`
			Reason string `json:"reason"`
		} `json:"components"`
	}
	res := publicRender(t, fs, "x-status")
	if res.Code != http.StatusOK {
		t.Fatalf("public render = %d: %s", res.Code, res.Body.String())
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Summary != string(domain.CompNoData) || body.SummaryState != string(domain.SummaryNoData) {
		t.Fatalf("summary = %q/%q, want no_data — nothing on this page was measured",
			body.Summary, body.SummaryState)
	}
	if body.Unmeasured != 2 {
		t.Fatalf("unmeasured = %d, want 2", body.Unmeasured)
	}
	for _, c := range body.Components {
		if c.Status != string(domain.CompNoData) {
			t.Errorf("%s = %q, want no_data", c.Name, c.Status)
		}
		// Internal topology and operator diagnostics stay OFF the public payload.
		if c.Source != "" || c.Reason != "" {
			t.Errorf("%s leaked source=%q reason=%q to the public page", c.Name, c.Source, c.Reason)
		}
	}
}

// An EMPTY page says so, instead of "all systems operational" with no systems.
func TestPublicRenderEmptyPageIsNotOperational(t *testing.T) {
	fs := seededStore()
	fs.pages["spe"] = domain.StatusPage{ID: "spe", OrgID: "o1", Slug: "empty-status", Title: "Empty",
		Visibility: domain.VisibilityPublic}
	res := publicRender(t, fs, "empty-status")
	if res.Code != http.StatusOK {
		t.Fatalf("render = %d: %s", res.Code, res.Body.String())
	}
	var body struct {
		Summary      string `json:"summary"`
		SummaryState string `json:"summary_state"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SummaryState != string(domain.SummaryEmpty) || body.Summary == string(domain.CompOperational) {
		t.Fatalf("empty page = %q/%q, want the empty state and not operational",
			body.Summary, body.SummaryState)
	}
}

// The render branches on the DISCRIMINATOR, not on "which column is populated": a component
// converted to a service must stop reporting its dormant monitor.
func TestPublicRenderIgnoresDormantBindings(t *testing.T) {
	fs := seededStore()
	fs.pages["spd"] = domain.StatusPage{ID: "spd", OrgID: "o1", Slug: "d-status", Title: "D",
		Visibility: domain.VisibilityPublic}
	fs.monitors["mdown"] = domain.Monitor{ID: "mdown", ProjectID: "p1", Name: "old probe",
		Type: domain.MonitorHTTP, Status: domain.StatusDown, Enabled: true}
	// Service ACTIVE, monitor DORMANT — and the dormant monitor is DOWN, so reading the wrong
	// one is loudly visible instead of a subtle mismatch.
	fs.components["cd"] = domain.Component{ID: "cd", StatusPageID: "spd", OrgID: "o1",
		Name: "Checkout", Source: domain.ComponentSourceService, SourceProject: "p1",
		ServiceID: "svc-1", MonitorID: "mdown", ManualStatus: domain.CompMajorOutage}
	fs.projections["svc-1"] = store.ServiceStatusProjection{
		ServiceID: "svc-1", SLI: "healthy", SealedThrough: time.Now().UTC(),
	}
	res := publicRender(t, fs, "d-status")
	var body struct {
		Summary    string `json:"summary"`
		Components []struct {
			Status string `json:"status"`
		} `json:"components"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Components) != 1 || body.Components[0].Status != string(domain.CompOperational) {
		t.Fatalf("component = %+v, want operational from the ACTIVE service source", body.Components)
	}
	if body.Summary != string(domain.CompOperational) {
		t.Fatalf("summary = %q — a dormant binding reached the render", body.Summary)
	}
}

// Invariant 71a: an active service the projection cannot read is a FAILED read. The public page
// degrades that component and NEVER publishes the calm statement `no_data` as if it were a fact
// about the service.
func TestPublicRenderDegradesUnreadableService(t *testing.T) {
	fs := seededStore()
	fs.pages["spu"] = domain.StatusPage{ID: "spu", OrgID: "o1", Slug: "u-status", Title: "U",
		Visibility: domain.VisibilityPublic}
	fs.components["cu"] = domain.Component{ID: "cu", StatusPageID: "spu", OrgID: "o1",
		Name: "Ghost", Source: domain.ComponentSourceService, SourceProject: "p1", ServiceID: "gone"}
	// No projection entry for "gone" — the unreadable case.
	res := publicRender(t, fs, "u-status")
	if res.Code != http.StatusOK {
		t.Fatalf("render = %d", res.Code)
	}
	// The AUTHENTICATED view is where the failure is visible as a failure.
	authed := do(newHandler(fs), o1Admin, http.MethodGet, "/api/v1/status-pages/spu/render", "")
	var body struct {
		Components []struct {
			Status      string `json:"status"`
			Reason      string `json:"reason"`
			Unavailable bool   `json:"unavailable"`
		} `json:"components"`
	}
	if err := json.Unmarshal(authed.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Components) != 1 {
		t.Fatalf("components = %d", len(body.Components))
	}
	if !body.Components[0].Unavailable || body.Components[0].Reason != "service_unreadable" {
		t.Fatalf("component = %+v, want the read failure marked and named", body.Components[0])
	}
}

// The projection is BATCHED: one call per project regardless of how many service components a
// page carries (invariant 71b). A per-component loop would pass every behavioural assertion
// above, so the batching needs its own witness.
func TestPublicRenderBatchesServiceProjections(t *testing.T) {
	fs := seededStore()
	fs.pages["spb"] = domain.StatusPage{ID: "spb", OrgID: "o1", Slug: "b-status", Title: "B",
		Visibility: domain.VisibilityPublic}
	for i := 0; i < 12; i++ {
		id := string(rune('a'+i)) + "-svc"
		fs.components[id] = domain.Component{ID: id, StatusPageID: "spb", OrgID: "o1",
			Name: id, Source: domain.ComponentSourceService, SourceProject: "p1", ServiceID: id}
		fs.projections[id] = store.ServiceStatusProjection{ServiceID: id, SLI: "healthy"}
	}
	fs.projectionCalls = 0
	if res := publicRender(t, fs, "b-status"); res.Code != http.StatusOK {
		t.Fatalf("render = %d: %s", res.Code, res.Body.String())
	}
	if fs.projectionCalls != 1 {
		t.Fatalf("projection calls = %d for 12 service components, want exactly 1", fs.projectionCalls)
	}
}

// The conversion is previewed and CAS-fenced end to end, and confirming without the tokens the
// preview issued is refused rather than silently treated as consent.
func TestComponentConversionPreviewAndConfirm(t *testing.T) {
	fs := seededStore()
	fs.projections["00000000-0000-4000-8000-000000000001"] = store.ServiceStatusProjection{
		ServiceID: "00000000-0000-4000-8000-000000000001", SLI: "healthy",
	}
	h := newHandler(fs)

	// Preview: monitor → service.
	res := do(h, o1Admin, http.MethodPost, "/api/v1/components/c1/conversion/preview",
		jsonBody(t, map[string]any{"source": "service", "service_id": "00000000-0000-4000-8000-000000000001"}))
	if res.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", res.Code, res.Body.String())
	}
	var plan struct {
		Component      struct{ Status, Source string } `json:"component"`
		Proposed       struct{ Status, Source string } `json:"proposed"`
		Summary        domain.PageSummary              `json:"summary"`
		ProposedSum    domain.PageSummary              `json:"proposed_summary"`
		Revision       int64                           `json:"revision"`
		PageGeneration int64                           `json:"page_generation"`
		NoOp           bool                            `json:"no_op"`
		Notes          []string                        `json:"notes"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if plan.Proposed.Source != string(domain.ComponentSourceService) {
		t.Fatalf("proposed source = %q", plan.Proposed.Source)
	}
	if plan.NoOp {
		t.Fatal("a real source change was reported as a no-op")
	}
	if len(plan.Notes) == 0 {
		t.Fatal("the preview stated no consequences at all")
	}

	// Confirming WITHOUT the tokens is a 400: the CAS must not be optional in practice.
	bad := do(h, o1Admin, http.MethodPost, "/api/v1/components/c1/conversion",
		jsonBody(t, map[string]any{"source": "service", "service_id": "00000000-0000-4000-8000-000000000001"}))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("tokenless confirm = %d, want 400", bad.Code)
	}
	// A STALE token is a 409 naming the contract.
	stale := do(h, o1Admin, http.MethodPost, "/api/v1/components/c1/conversion",
		jsonBody(t, map[string]any{
			"source": "service", "service_id": "00000000-0000-4000-8000-000000000001",
			"revision": plan.Revision + 5, "page_generation": plan.PageGeneration,
		}))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale confirm = %d, want 409", stale.Code)
	}
	if !strings.Contains(stale.Body.String(), "page_configuration_stale") {
		t.Fatalf("stale body = %s, want the named contract", stale.Body.String())
	}
	// The real confirmation applies and keeps the monitor binding dormant.
	ok := do(h, o1Admin, http.MethodPost, "/api/v1/components/c1/conversion",
		jsonBody(t, map[string]any{
			"source": "service", "service_id": "00000000-0000-4000-8000-000000000001",
			"revision": plan.Revision, "page_generation": plan.PageGeneration,
		}))
	if ok.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", ok.Code, ok.Body.String())
	}
	var got domain.Component
	if err := json.Unmarshal(ok.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode confirm: %v", err)
	}
	if got.Source != domain.ComponentSourceService || got.MonitorID != "mon1" {
		t.Fatalf("converted = %+v, want service active and mon1 dormant", got)
	}
}

// A viewer may not convert, and a member of another org cannot even see the component.
func TestComponentConversionAuthorization(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	if res := do(h, o1Viewer, http.MethodPost, "/api/v1/components/c1/conversion/preview",
		jsonBody(t, map[string]any{"source": "manual"})); res.Code != http.StatusForbidden {
		t.Fatalf("viewer preview = %d, want 403", res.Code)
	}
	if res := do(h, outsider, http.MethodPost, "/api/v1/components/c1/conversion/preview",
		jsonBody(t, map[string]any{"source": "manual"})); res.Code != http.StatusNotFound {
		t.Fatalf("outsider preview = %d, want 404", res.Code)
	}
}

// The composite lifecycle: the link changes nothing, retire changes both facts, and the pair is
// reversible. Each is its own endpoint because each is its own act.
func TestCompositeLifecycleEndpoints(t *testing.T) {
	fs := seededStore()
	// §15.5 is a COMPOSITE section, so the lifecycle actions target a composite. The previous
	// version of this test used an HTTP monitor and thereby PINNED surface the phase was never
	// authorized to add ([314] P1-6).
	fs.monitors["comp2"] = domain.Monitor{ID: "comp2", ProjectID: "p1", Name: "Edge", Slug: "edge2",
		Type: domain.MonitorComposite, Enabled: true,
		Config: map[string]string{"children": "mon1", "mode": "all"}}
	h := newHandler(fs)

	// Every lifecycle action refuses a non-composite, with a reason rather than a 500.
	for _, path := range []string{"retire", "reactivate", "convert-to-service"} {
		if res := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/mon1/"+path, ""); res.Code != http.StatusBadRequest {
			t.Fatalf("%s on an http monitor = %d, want 400", path, res.Code)
		}
	}
	if res := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/successor",
		jsonBody(t, map[string]any{"service_id": ""})); res.Code != http.StatusBadRequest {
		t.Fatalf("successor on an http monitor = %d, want 400", res.Code)
	}

	// The annotation.
	res := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/comp2/successor",
		jsonBody(t, map[string]any{"service_id": "svc-nope"}))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("bogus successor = %d, want 400", res.Code)
	}

	// Retire, then the same call again.
	res = do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp2/retire", "")
	if res.Code != http.StatusOK {
		t.Fatalf("retire = %d: %s", res.Code, res.Body.String())
	}
	var m domain.Monitor
	if err := json.Unmarshal(res.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.RetiredAt == nil || m.Enabled {
		t.Fatalf("retired monitor = retired:%v enabled:%v — both facts must change together",
			m.RetiredAt != nil, m.Enabled)
	}
	if res := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp2/retire", ""); res.Code != http.StatusConflict {
		t.Fatalf("second retire = %d, want 409", res.Code)
	}
	// Reversible.
	res = do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp2/reactivate", "")
	if res.Code != http.StatusOK {
		t.Fatalf("reactivate = %d: %s", res.Code, res.Body.String())
	}
	// A FRESH target: `retired_at` is omitempty, so decoding a cleared field into the struct
	// above would leave the retired timestamp standing and the assertion would test nothing.
	var back domain.Monitor
	if err := json.Unmarshal(res.Body.Bytes(), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.RetiredAt != nil || !back.Enabled || back.Status != domain.StatusPending {
		t.Fatalf("reactivated = %+v, want enabled and pending", back)
	}
	// A viewer may not do any of it.
	if res := do(h, o1Viewer, http.MethodPost, "/api/v1/monitors/comp2/retire", ""); res.Code != http.StatusForbidden {
		t.Fatalf("viewer retire = %d, want 403", res.Code)
	}
}

// Converting a composite is a 201 the first time and a 200 no-op afterwards, never a second
// service.
func TestConvertCompositeEndpointIsIdempotent(t *testing.T) {
	fs := seededStore()
	fs.monitors["comp1"] = domain.Monitor{ID: "comp1", ProjectID: "p1", Name: "Edge",
		Slug: "edge", Type: domain.MonitorComposite, Enabled: true,
		Config: map[string]string{"children": "mon1", "mode": "all"}}
	h := newHandler(fs)

	// The SLI is REQUIRED: omitting it is a 400, never a silent "all children" (§15.5).
	if res := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp1/convert-to-service", ""); res.Code != http.StatusBadRequest {
		t.Fatalf("convert with no sli = %d, want 400", res.Code)
	}
	// And an SLI member that is not a child of THIS composite is refused by name.
	if res := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp1/convert-to-service",
		jsonBody(t, map[string]any{"sli": []string{"mon-elsewhere"}})); res.Code != http.StatusBadRequest {
		t.Fatalf("convert with a foreign sli member = %d, want 400", res.Code)
	}
	first := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp1/convert-to-service",
		jsonBody(t, map[string]any{"sli": []string{"mon1"}}))
	if first.Code != http.StatusCreated {
		t.Fatalf("convert = %d: %s", first.Code, first.Body.String())
	}
	var got struct {
		Service          domain.Service `json:"service"`
		AlreadyConverted bool           `json:"already_converted"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AlreadyConverted || got.Service.ID == "" {
		t.Fatalf("first convert = %+v", got)
	}
	second := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/comp1/convert-to-service",
		jsonBody(t, map[string]any{"sli": []string{"mon1"}}))
	if second.Code != http.StatusOK {
		t.Fatalf("re-convert = %d, want 200", second.Code)
	}
	var again struct {
		Service          domain.Service `json:"service"`
		AlreadyConverted bool           `json:"already_converted"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !again.AlreadyConverted || again.Service.ID != got.Service.ID {
		t.Fatalf("re-convert = %+v, want the same service reported as already converted", again)
	}
	// A non-composite is a 400, not a 500.
	if res := do(h, o1Admin, http.MethodPost, "/api/v1/monitors/mon1/convert-to-service",
		jsonBody(t, map[string]any{"sli": []string{"mon1"}})); res.Code != http.StatusBadRequest {
		t.Fatalf("non-composite convert = %d, want 400", res.Code)
	}
}

// [314] P1-3 — the page is ONE batch per source, across projects. 500 components spread over 250
// projects must cost the SAME calls as one: an org-level page spans projects, and the first
// implementation took a snapshot per project while claiming one snapshot per page.
func TestPublicRenderIsBoundedAtTheCeiling(t *testing.T) {
	fs := seededStore()
	fs.pages["spmax"] = domain.StatusPage{ID: "spmax", OrgID: "o1", Slug: "max-status", Title: "Max",
		Visibility: domain.VisibilityPublic}
	const n = 500
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%03d", i)
		project := fmt.Sprintf("p%03d", i/2) // 250 distinct projects
		if i%2 == 0 {
			svc := fmt.Sprintf("s%03d", i)
			fs.components[id] = domain.Component{ID: id, StatusPageID: "spmax", OrgID: "o1",
				Name: id, Source: domain.ComponentSourceService, SourceProject: project, ServiceID: svc}
			fs.projections[svc] = store.ServiceStatusProjection{ServiceID: svc, SLI: "healthy"}
			continue
		}
		mon := fmt.Sprintf("m%03d", i)
		fs.monitors[mon] = domain.Monitor{ID: mon, ProjectID: project, Name: mon,
			Type: domain.MonitorHTTP, Status: domain.StatusUp, Enabled: true}
		fs.components[id] = domain.Component{ID: id, StatusPageID: "spmax", OrgID: "o1",
			Name: id, Source: domain.ComponentSourceMonitor, SourceProject: project, MonitorID: mon}
	}

	// Incidents across many of those projects, each with a timeline and a postmortem: the render's
	// OTHER growth path, which the first version of this test never touched ([318] P1-1).
	resolved := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("inc%03d", i)
		project := fmt.Sprintf("p%03d", i)
		status, res := domain.IncidentInvestigating, (*time.Time)(nil)
		if i%2 == 0 {
			status, res = domain.IncidentResolved, &resolved
		}
		fs.incidents[id] = domain.Incident{ID: id, ProjectID: project, Title: id,
			Status: status, Impact: domain.ImpactMinor, Source: domain.SourceManual, ResolvedAt: res}
		fs.incUpdates[id] = []domain.IncidentUpdate{{ID: id + "-u", IncidentID: id,
			Status: domain.IncidentInvestigating, Body: "looking"}}
		fs.postmortems[id] = domain.Postmortem{ID: id + "-pm", IncidentID: id, Body: "why"}
		fs.maintenance[id+"-mw"] = domain.MaintenanceWindow{ID: id + "-mw", ProjectID: project,
			StartsAt: time.Now().Add(time.Hour), EndsAt: time.Now().Add(2 * time.Hour)}
	}

	fs.projectionCalls, fs.monitorProjectionCalls = 0, 0
	fs.pageIncidentCalls, fs.pageMaintenanceCalls = 0, 0
	fs.timelineCalls, fs.postmortemCalls = 0, 0
	res := publicRender(t, fs, "max-status")
	if res.Code != http.StatusOK {
		t.Fatalf("render at the ceiling = %d: %s", res.Code, res.Body.String())
	}
	// One call per source, and one per context read — for 500 components, 250 projects and 40
	// incidents. Every one of these was a loop before.
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"service projections", fs.projectionCalls, 1},
		{"monitor projections", fs.monitorProjectionCalls, 1},
		{"page incidents", fs.pageIncidentCalls, 1},
		{"page maintenance", fs.pageMaintenanceCalls, 1},
		// Two enrich passes (active, recent) — a constant, not a per-incident count.
		{"incident timelines", fs.timelineCalls, 2},
		{"postmortems", fs.postmortemCalls, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s calls = %d, want %d — the render still grows with the page", c.name, c.got, c.want)
		}
	}
	var body struct {
		Components []struct{} `json:"components"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Components) != n {
		t.Fatalf("rendered %d of %d components — nothing may be silently truncated", len(body.Components), n)
	}
}

// [314] P1-3/71b — ABOVE the absolute ceiling the public page refuses AS A WHOLE and names both
// numbers, while the authenticated management view still lists everything so the operator can fix
// it. A truncated subset posing as the complete page is the outcome this forbids.
func TestPublicRenderRefusesOverTheSafeLimit(t *testing.T) {
	fs := seededStore()
	fs.pages["spover"] = domain.StatusPage{ID: "spover", OrgID: "o1", Slug: "over-status", Title: "Over",
		Visibility: domain.VisibilityPublic}
	const n = 501
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("o%04d", i)
		fs.components[id] = domain.Component{ID: id, StatusPageID: "spover", OrgID: "o1",
			Name: id, Source: domain.ComponentSourceManual, ManualStatus: domain.CompOperational}
	}
	res := publicRender(t, fs, "over-status")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-limit public render = %d, want 503", res.Code)
	}
	if !strings.Contains(res.Body.String(), "status_page_over_safe_limit") {
		t.Fatalf("body = %s, want the named refusal", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "501") || !strings.Contains(res.Body.String(), "500") {
		t.Fatalf("body = %s, want both the count and the limit named", res.Body.String())
	}
	// The authenticated view still serves the whole page.
	authed := do(newHandler(fs), o1Admin, http.MethodGet, "/api/v1/status-pages/spover/render", "")
	if authed.Code != http.StatusOK {
		t.Fatalf("authenticated render = %d, want the operator to still see it", authed.Code)
	}
	var body struct {
		Components []struct{} `json:"components"`
	}
	if err := json.Unmarshal(authed.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Components) != n {
		t.Fatalf("authenticated render listed %d of %d", len(body.Components), n)
	}
}

// [314] P1-7 — a FAILED read is distinguishable in the PUBLIC payload and it is COUNTED. Before
// this, the public bytes for an unreadable service were identical to a calm "no data".
func TestUnreadableServiceIsVisibleAndCountedPublicly(t *testing.T) {
	fs := seededStore()
	fs.pages["spun"] = domain.StatusPage{ID: "spun", OrgID: "o1", Slug: "un-status", Title: "Un",
		Visibility: domain.VisibilityPublic}
	// One component whose service cannot be read, and one honestly unmeasured manual component —
	// the pair is the point: their public bytes must NOT be the same.
	fs.components["cbroken"] = domain.Component{ID: "cbroken", StatusPageID: "spun", OrgID: "o1",
		Name: "Ghost", Source: domain.ComponentSourceService, SourceProject: "p1", ServiceID: "gone"}
	fs.components["cquiet"] = domain.Component{ID: "cquiet", StatusPageID: "spun", OrgID: "o1",
		Name: "Quiet", Source: domain.ComponentSourceManual}

	counter := &countingMetrics{}
	h := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithMetrics(counter).PublicRouter()
	res := do(h, authz.Principal{}, http.MethodGet, "/api/v1/public/status-pages/un-status", "")
	if res.Code != http.StatusOK {
		t.Fatalf("render = %d", res.Code)
	}
	var body struct {
		Components []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Unavailable bool   `json:"unavailable"`
			Reason      string `json:"reason"`
		} `json:"components"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var broken, quiet bool
	for _, c := range body.Components {
		switch c.Name {
		case "Ghost":
			broken = true
			if !c.Unavailable {
				t.Error("the failed read is not marked in the public payload")
			}
			if c.Reason != "" {
				t.Errorf("the public payload leaked the internal reason %q", c.Reason)
			}
		case "Quiet":
			quiet = true
			if c.Unavailable {
				t.Error("an honestly unmeasured component was marked unavailable")
			}
		}
	}
	if !broken || !quiet {
		t.Fatalf("components = %+v", body.Components)
	}
	if counter.unreadable != 1 {
		t.Fatalf("unreadable counter = %d, want 1", counter.unreadable)
	}
}

// [314] P1-7 — the short-TTL cache is a RATE bound, and it must not become a privacy hole: an
// unlisted page's bytes are keyed to its token, and a refusal is never cached.
func TestPublicRenderCacheIsKeyedAndShortLived(t *testing.T) {
	fs := seededStore()
	fs.pages["spc"] = domain.StatusPage{ID: "spc", OrgID: "o1", Slug: "c-status", Title: "C",
		Visibility: domain.VisibilityUnlisted, UnlistedToken: "tok-abc"}
	fs.components["cc"] = domain.Component{ID: "cc", StatusPageID: "spc", OrgID: "o1",
		Name: "One", Source: domain.ComponentSourceManual, ManualStatus: domain.CompOperational}
	h := newPublicHandler(fs)

	first := do(h, authz.Principal{}, http.MethodGet, "/api/v1/public/status-pages/c-status?token=tok-abc", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first render = %d: %s", first.Code, first.Body.String())
	}
	second := do(h, authz.Principal{}, http.MethodGet, "/api/v1/public/status-pages/c-status?token=tok-abc", "")
	if second.Header().Get("X-Cerbix-Cache") != "hit" {
		t.Fatal("the second identical request was not served from the cache")
	}
	if second.Body.String() != first.Body.String() {
		t.Fatal("the cached body differs from the one that produced it")
	}
	// WITHOUT the token the page is a 404 — the cached bytes must be unreachable.
	none := do(h, authz.Principal{}, http.MethodGet, "/api/v1/public/status-pages/c-status", "")
	if none.Code != http.StatusNotFound {
		t.Fatalf("tokenless request = %d, want 404 — the cache leaked an unlisted render", none.Code)
	}
	if strings.Contains(none.Body.String(), "One") {
		t.Fatal("the tokenless response carried the unlisted page's component")
	}
	// A WRONG token likewise.
	wrong := do(h, authz.Principal{}, http.MethodGet, "/api/v1/public/status-pages/c-status?token=nope", "")
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong-token request = %d, want 404", wrong.Code)
	}
}

// countingMetrics is the api.Metrics surface, counting only what these tests assert.
type countingMetrics struct {
	incidents  int
	unreadable int
}

func (c *countingMetrics) RecordIncidentOpened()                { c.incidents++ }
func (c *countingMetrics) RecordStatusPageUnreadableComponent() { c.unreadable++ }
