package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-021 phase 3, changeset 2 (§14.4): the dedicated edge routes, the
// authenticated-detail impacts enrichment, and the read-side bounds.

// Attribution fixtures ([288] P1-3): a session human carries the email label the
// auth layer resolved; an API-token principal carries the synthetic id, ViaToken
// and the token NAME — exactly what auth/middleware.go builds in production.
var (
	p1EditorHuman = authz.Principal{
		UserID:      "3f2b1c5e-1111-4a2b-9c3d-000000000001",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		AuditLabel:  "seymur@teamlead.com",
	}
	p1TokenEditor = authz.Principal{
		UserID:      authz.SyntheticTokenActorPrefix + "3f2b1c5e-2222-4a2b-9c3d-000000000002",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
		ViaToken:    true,
		AuditLabel:  "token:deploy-bot",
	}
)

func graphView(t *testing.T, body []byte) (out struct {
	GraphGeneration int64 `json:"graph_generation"`
	DependsOn       []struct {
		ID     string `json:"id"`
		Slug   string `json:"slug"`
		Health *struct {
			SLI string `json:"sli"`
		} `json:"health"`
	} `json:"depends_on"`
	DependedOnBy []struct {
		Slug string `json:"slug"`
	} `json:"depended_on_by"`
}) {
	t.Helper()
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode graph view: %v", err)
	}
	return out
}

// The PUT/GET round-trip: the editor replaces the set with the CAS token, the
// response carries the new token naming the new set, both directions render,
// and neighbour health arrives from the ONE batched snapshot call.
func TestServiceDependenciesRoundTrip(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")
	parent := createSvc(t, h, "payments")
	fs.neighbourHealth = map[string]domain.ServiceHealthNow{
		parent: {SLI: "down", Diagnostics: "down"},
	}

	rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":["`+parent+`"],"graph_generation":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	v := graphView(t, rec.Body.Bytes())
	if v.GraphGeneration != 1 || len(v.DependsOn) != 1 || v.DependsOn[0].Slug != "payments" {
		t.Fatalf("put view = %+v", v)
	}
	if v.DependsOn[0].Health == nil || v.DependsOn[0].Health.SLI != "down" {
		t.Fatalf("neighbour health missing from the edge view: %+v", v.DependsOn[0])
	}

	// The reverse direction from the parent's read.
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+parent+"/dependencies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	pv := graphView(t, rec.Body.Bytes())
	if len(pv.DependedOnBy) != 1 || pv.DependedOnBy[0].Slug != "checkout" {
		t.Fatalf("parent view = %+v", pv)
	}
}

// The wire contract of the write errors: a stale token is 409 naming the CAS,
// a viewer may not write, a foreign project is 404.
func TestServiceDependenciesWriteErrors(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")
	parent := createSvc(t, h, "payments")

	if rec := do(h, p1Viewer, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":[],"graph_generation":0}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer put = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":["`+parent+`"],"graph_generation":7}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "graph_generation_stale") {
		t.Fatalf("stale put = %d: %s, want 409 graph_generation_stale", rec.Code, rec.Body.String())
	}
	fs.graphErr = store.ErrServiceGraphCycle
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":["`+parent+`"],"graph_generation":0}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "dependency_cycle") {
		t.Fatalf("cycle put = %d: %s, want 400 dependency_cycle", rec.Code, rec.Body.String())
	}
	fs.graphErr = nil
	if rec := do(h, outsider, http.MethodGet, "/api/v1/projects/p1/services/"+child+"/dependencies", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider get = %d, want 404", rec.Code)
	}
}

// Create-with-edges (§14.4): depends_on on service create lands the edges
// atomically through the same validator.
func TestCreateServiceWithDependencies(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	parent := createSvc(t, h, "payments")

	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services",
		`{"slug":"checkout","name":"Checkout","depends_on":["`+parent+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+created.ID+"/dependencies", "")
	v := graphView(t, rec.Body.Bytes())
	if len(v.DependsOn) != 1 || v.DependsOn[0].Slug != "payments" {
		t.Fatalf("created edges = %+v", v)
	}
}

// The service DETAIL carries the dependencies block, and neighbour health is
// fetched by exactly ONE batched call — never a per-neighbour loop (§14.7,
// invariant 60).
func TestServiceDetailDependenciesBatchedHealth(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")
	p1 := createSvc(t, h, "payments")
	p2 := createSvc(t, h, "session-redis")
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":["`+p1+`","`+p2+`"],"graph_generation":0}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	fs.neighbourHealthCalls = 0
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+child, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Dependencies *struct {
			DependsOn []struct {
				Slug string `json:"slug"`
			} `json:"depends_on"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Dependencies == nil || len(detail.Dependencies.DependsOn) != 2 {
		t.Fatalf("detail dependencies = %+v", detail.Dependencies)
	}
	if fs.neighbourHealthCalls != 1 {
		t.Fatalf("neighbour health calls = %d, want exactly 1 batched call for the whole set", fs.neighbourHealthCalls)
	}
}

// Impacts are an authenticated-DETAIL enrichment (invariants 59–60): present
// on the incident detail, absent from the incident list, and absent from the
// raw UNAUTHENTICATED status-page JSON even when the incident has links.
func TestIncidentImpactsDetailOnlyAndNeverPublic(t *testing.T) {
	fs := seededStore()
	fs.impacts = map[string][]domain.ServiceImpactLink{
		"inc1": {{ServiceID: "svc-x", Slug: "impact-svc-secret", Name: "Impact Secret",
			Role: domain.ImpactProbableRoot, Path: []string{"impact-svc-secret", "checkout"}}},
	}
	h := newHandler(fs)

	// Detail: impacts present.
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	var det struct {
		Impacts []struct {
			Slug string   `json:"slug"`
			Role string   `json:"role"`
			Path []string `json:"path"`
		} `json:"impacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(det.Impacts) != 1 || det.Impacts[0].Slug != "impact-svc-secret" || len(det.Impacts[0].Path) != 2 {
		t.Fatalf("detail impacts = %+v", det.Impacts)
	}

	// List: NO impacts field on any row (the unbounded-history bound of §14.7).
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/incidents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "impacts") || strings.Contains(rec.Body.String(), "impact-svc-secret") {
		t.Fatalf("the incident LIST leaked impacts: %s", rec.Body.String())
	}

	// Public status page: the raw unauthenticated JSON carries no impact ids,
	// slugs, names or paths (invariant 59 — inc1 is the page's active incident).
	pub := newPublicHandler(fs)
	rec = do(pub, outsider, http.MethodGet, "/api/v1/public/status-pages/acme-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public render = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"impacts", "impact-svc-secret", "Impact Secret", "probable_root"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public status JSON leaked %q", leak)
		}
	}
}

// The strict request contract ([288] P1-1): an omitted or null depends_on is
// NEVER a silent delete, an omitted graph_generation is never a passing zero,
// and malformed UUIDs are 400s at transport — none of them touch the set.
func TestServiceDependenciesStrictRequestContract(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")
	parent := createSvc(t, h, "payments")
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":["`+parent+`"],"graph_generation":0}`); rec.Code != http.StatusOK {
		t.Fatalf("seed put = %d: %s", rec.Code, rec.Body.String())
	}

	cases := []struct{ name, body, want string }{
		{"omitted depends_on", `{"graph_generation":1}`, "depends_on is required"},
		{"null depends_on", `{"depends_on":null,"graph_generation":1}`, "depends_on is required"},
		{"omitted generation", `{"depends_on":[]}`, "graph_generation is required"},
		{"empty-string id", `{"depends_on":[""],"graph_generation":1}`, "depends_on entries must be UUIDs"},
		{"malformed id", `{"depends_on":["not-a-uuid"],"graph_generation":1}`, "depends_on entries must be UUIDs"},
	}
	for _, c := range cases {
		rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s = %d: %s, want 400 %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	// The set and its token are untouched by every rejected request.
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+child+"/dependencies", "")
	v := graphView(t, rec.Body.Bytes())
	if v.GraphGeneration != 1 || len(v.DependsOn) != 1 {
		t.Fatalf("rejected requests changed the set: %+v", v)
	}
	// A malformed serviceID in the PATH is 400 at transport, never a store uuid cast.
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/not-a-uuid/dependencies", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed path id = %d, want 400", rec.Code)
	}
	// An EMPTY array is the legitimate way to clear the set.
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":[],"graph_generation":1}`); rec.Code != http.StatusOK {
		t.Fatalf("empty-array clear = %d: %s", rec.Code, rec.Body.String())
	}
}

// A file-owned service's edges are not UI-mutable ([288] P0): the PUT is 409
// managed_by_file, so the database keeps representing the provider's applied
// desired state.
func TestServiceDependenciesFileOwnedIs409(t *testing.T) {
	fs := seededStore()
	fs.graphErr = store.ErrServiceManagedByFile
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")
	rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":[],"graph_generation":0}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "managed_by_file") {
		t.Fatalf("file-owned put = %d: %s, want 409 managed_by_file", rec.Code, rec.Body.String())
	}
}

// Typed AND human-readable audit attribution reaches the store from the wire
// ([288] P1-3): a session user carries its email, an API token its name with
// via_token and a NULL user id.
func TestServiceDependenciesActorAttribution(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	child := createSvc(t, h, "checkout")

	if rec := do(h, p1EditorHuman, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":[],"graph_generation":0}`); rec.Code != http.StatusOK {
		t.Fatalf("human put = %d: %s", rec.Code, rec.Body.String())
	}
	// The human write bumped the generation; the token write must echo the CURRENT
	// token like any other client.
	if rec := do(h, p1TokenEditor, http.MethodPut, "/api/v1/projects/p1/services/"+child+"/dependencies",
		`{"depends_on":[],"graph_generation":1}`); rec.Code != http.StatusOK {
		t.Fatalf("token put = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.graphActors) != 2 {
		t.Fatalf("captured actors = %d, want 2", len(fs.graphActors))
	}
	human, token := fs.graphActors[0], fs.graphActors[1]
	if human.UserID == "" || human.ViaToken || !strings.Contains(human.Label, "@") {
		t.Errorf("human actor = %+v, want a real user id, via_token false and an email label", human)
	}
	if token.UserID != "" || !token.ViaToken || !strings.HasPrefix(token.Label, "token:") {
		t.Errorf("token actor = %+v, want NULL user id, via_token true and a token: label", token)
	}
}

// A failed impact read is DISCLOSED, never disguised as "no impacts"
// ([288] P1-4).
func TestIncidentImpactsUnavailableIsNotEmpty(t *testing.T) {
	fs := seededStore()
	fs.impactsErr = errors.New("impact read exploded")
	h := newHandler(fs)
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Impacts            []domain.ServiceImpactLink `json:"impacts"`
		ImpactsUnavailable bool                       `json:"impacts_unavailable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Impacts != nil || !out.ImpactsUnavailable {
		t.Fatalf("degraded read = impacts %v / unavailable %v, want null + true", out.Impacts, out.ImpactsUnavailable)
	}
	// The honest empty answer stays distinguishable. Decode into a FRESH value:
	// `impacts_unavailable` is omitempty, so reusing the struct would silently keep
	// the previous true and the assertion would pass for the wrong reason.
	fs.impactsErr = nil
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
	out = struct {
		Impacts            []domain.ServiceImpactLink `json:"impacts"`
		ImpactsUnavailable bool                       `json:"impacts_unavailable"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Impacts == nil || len(out.Impacts) != 0 || out.ImpactsUnavailable {
		t.Fatalf("no-links answer = impacts %v / unavailable %v, want [] + false", out.Impacts, out.ImpactsUnavailable)
	}
}

// Create-with-edges crosses the SAME transport fence as the PUT ([292]): a
// malformed or empty dependency id is a 400 BEFORE any store call, so it can
// never reach the uuid-typed membership query and surface as a 500 — and no
// service is created by a rejected request.
func TestCreateServiceWithDependenciesValidatesBodyUUIDs(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	for _, c := range []struct{ name, body string }{
		{"malformed id", `{"slug":"checkout","depends_on":["not-a-uuid"]}`},
		{"empty id", `{"slug":"checkout","depends_on":[""]}`},
	} {
		rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "depends_on entries must be UUIDs") {
			t.Errorf("%s = %d: %s, want 400 depends_on entries must be UUIDs", c.name, rec.Code, rec.Body.String())
		}
	}
	// No service was created by any rejected request.
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services", "")
	if strings.Contains(rec.Body.String(), "checkout") {
		t.Fatalf("a rejected create still made a service: %s", rec.Body.String())
	}
	// The valid path still works through the same fence.
	parent := createSvc(t, h, "payments")
	if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services",
		`{"slug":"checkout","depends_on":["`+parent+`"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("valid create = %d: %s", rec.Code, rec.Body.String())
	}
}
