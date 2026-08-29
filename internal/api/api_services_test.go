package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/store"
)

// Service reliability, phase 1 (spec func-service-reliability §21).

func createSvc(t *testing.T, h http.Handler, slug string) string {
	t.Helper()
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services",
		`{"slug":"`+slug+`","name":"`+slug+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s = %d: %s", slug, rec.Code, rec.Body.String())
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return v.ID
}

func TestServicesAuthzMatrix(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")

	// Reading is ProjectRead…
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services", ""); rec.Code != http.StatusOK {
		t.Errorf("viewer list = %d, want 200", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, ""); rec.Code != http.StatusOK {
		t.Errorf("viewer get = %d, want 200", rec.Code)
	}
	// …and every write is ProjectWrite. A declaration states what availability MEANS for a
	// whole service, so it is emphatically not a viewer operation.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/projects/p1/services", `{"slug":"cart","name":"Cart"}`},
		{http.MethodPut, "/api/v1/projects/p1/services/" + id + "/declaration", `{"expected_revision":0,"monitors":[],"sli":[]}`},
		{http.MethodDelete, "/api/v1/projects/p1/services/" + id, ""},
	} {
		if rec := do(h, p1Viewer, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// Existence is hidden from outsiders and from members of another project.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/projects/p1/services", ""); rec.Code != http.StatusNotFound {
		t.Errorf("outsider list = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodGet, "/api/v1/projects/p3/services", ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign project list = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodGet, "/api/v1/projects/p3/services/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant get = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p3/services/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant delete = %d, want 404", rec.Code)
	}
}

// A service with no declaration is a COMPLETE state, not a half-finished one — and it reports
// no availability rather than 100%. This is the whole point of the feature, so the payload
// says so structurally: `declaration` and `epoch` are null, and there is no number anywhere.
func TestAServiceWithNoDeclarationReportsNothingRatherThanPerfect(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")

	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v["declaration"] != nil {
		t.Errorf("declaration = %v, want null", v["declaration"])
	}
	if v["epoch"] != nil {
		t.Errorf("epoch = %v, want null", v["epoch"])
	}
	// The key must be PRESENT and null: absent could be read as "the field moved", null says
	// the system has no answer.
	if got, ok := v["reliability"]; !ok || got != nil {
		t.Errorf("reliability = %v (present=%v), want a present null", got, ok)
	}
	m, _ := v["materialization"].(map[string]any)
	if m == nil {
		t.Fatal("no materialization block")
	}
	if m["sealed_through"] != nil {
		t.Errorf("sealed_through = %v on a service that has evaluated nothing", m["sealed_through"])
	}
	if rr, ok := m["repairing"].([]any); !ok || len(rr) != 0 {
		t.Errorf("repairing = %v, want an empty array (never null: a client must not branch on it)", m["repairing"])
	}
}

// Phase 1 ships no SLO, no error budget and no burn rate. They are ABSENT rather than zero:
// a `0%` a client would render as a number is exactly the confident falsehood this feature
// exists to prevent, so the guard is on the serialized bytes, not on a struct field.
func TestPhase1PayloadCarriesNoReliabilityNumbers(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")
	body := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, "").Body.String()
	for _, forbidden := range []string{
		"slo", "error_budget", "burn_rate", "availability", "uptime", "coverage",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("phase-1 detail payload mentions %q: %s", forbidden, body)
		}
	}
}

// The SLI is a SUBSET of the evaluation context by construction. A member outside it would be
// a number with no visible source, so it is refused at the edge rather than stored.
func TestDeclarationRefusesAnSLIOutsideTheContext(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")
	rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/declaration",
		`{"expected_revision":0,"monitors":["m1"],"sli":["m1","ghost"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sli_not_in_monitors") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// Two operators editing the same SLI have made two DIFFERENT statements about what
// availability means. Merging them silently is the worst of the three options, so a stale
// expected_revision is a 409 the human has to resolve.
func TestConcurrentDeclarationsConflictRatherThanMerge(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")

	first := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/declaration",
		`{"expected_revision":0,"monitors":["m1"],"sli":["m1"]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first declaration = %d: %s", first.Code, first.Body.String())
	}
	var out struct {
		Declaration struct {
			Revision int64 `json:"revision"`
		} `json:"declaration"`
		Epoch struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"epoch"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Declaration.Revision != 1 {
		t.Errorf("revision = %d, want 1", out.Declaration.Revision)
	}
	// Every revision comes with the epoch that makes it evaluable — a revision alone is a
	// reference no fact could ever point at.
	if out.Epoch.ID == "" {
		t.Error("the write returned a revision with no epoch")
	}

	second := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/declaration",
		`{"expected_revision":0,"monitors":["m1"],"sli":["m1"]}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("stale write = %d, want 409: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "revision_conflict") {
		t.Errorf("body = %q", second.Body.String())
	}
}

func TestServiceCreateValidation(t *testing.T) {
	h := newHandler(seededStore())
	for _, tc := range []struct{ what, body string }{
		{"empty slug", `{"name":"Checkout"}`},
		{"uppercase slug", `{"slug":"Checkout"}`},
		{"slug with a space", `{"slug":"check out"}`},
		{"unknown field", `{"slug":"checkout","bogus":1}`},
		{"malformed json", `{"slug":`},
	} {
		if rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services", tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", tc.what, rec.Code, rec.Body.String())
		}
	}
	// An omitted name falls back to the slug rather than creating a nameless row.
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services", `{"slug":"checkout"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"checkout"`) {
		t.Errorf("body = %q, want the slug as the fallback name", rec.Body.String())
	}
}

// The declaration body is bounded. Without the cap a single request could pin an arbitrary
// amount of memory, and the bound has to be enforced on the READER, not checked after the
// fact on an already-buffered body.
func TestDeclarationBodyIsBounded(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")
	huge := `{"expected_revision":0,"monitors":["` + strings.Repeat("m", 70<<10) + `"],"sli":[]}`
	rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/declaration", huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized declaration = %d, want 400", rec.Code)
	}
}

func TestDeleteServiceThenGetIs404(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/services/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/services/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}

func TestDuplicateServiceSlugIs409(t *testing.T) {
	h := newHandler(seededStore())
	createSvc(t, h, "checkout")
	rec := do(h, p1Editor, http.MethodPost, "/api/v1/projects/p1/services", `{"slug":"checkout","name":"again"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("= %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// The LIST is where an operator picks which service to open, so it carries the watermark and
// the two member counts. A list that omitted `sealed_through` would let a service that
// stopped materializing an hour ago look exactly like a healthy one.
func TestServicesListCarriesTheWatermarkAndBothCounts(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/declaration",
		`{"expected_revision":0,"monitors":["m1","m2"],"sli":["m1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("declare = %d: %s", rec.Code, rec.Body.String())
	}

	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		Service        map[string]any `json:"service"`
		Revision       int64          `json:"revision"`
		ContextMembers int            `json:"context_members"`
		SLIMembers     int            `json:"sli_members"`
		EpochSeq       int64          `json:"epoch_seq"`
		SealedThrough  *string        `json:"sealed_through"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	// Two INDEPENDENT counts. A row that reported only one would hide the distinction the
	// whole declaration model rests on.
	if rows[0].ContextMembers != 2 || rows[0].SLIMembers != 1 {
		t.Errorf("counts = %d context / %d sli, want 2 / 1", rows[0].ContextMembers, rows[0].SLIMembers)
	}
	if rows[0].Revision != 1 || rows[0].EpochSeq != 1 {
		t.Errorf("revision=%d epoch=%d, want 1/1", rows[0].Revision, rows[0].EpochSeq)
	}
	// Nothing has been materialized, so the watermark is absent rather than a stand-in date.
	if rows[0].SealedThrough != nil {
		t.Errorf("sealed_through = %v before anything was sealed", *rows[0].SealedThrough)
	}

	// An undeclared service still lists — with revision 0, which is a state, not a gap.
	createSvc(t, h, "cart")
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if rows[0].Service["slug"] != "cart" {
		t.Errorf("rows are not slug-ordered: %v first", rows[0].Service["slug"])
	}
	if rows[0].Revision != 0 || rows[0].ContextMembers != 0 {
		t.Errorf("undeclared service row = %+v, want revision 0 and no members", rows[0])
	}
}

// An empty project lists as `[]`, never `null` — a client must not have to branch.
func TestServicesListIsAnArrayWhenEmpty(t *testing.T) {
	h := newHandler(seededStore())
	if got := strings.TrimSpace(do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services", "").Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

// §14.2: deleting a service that an applied file-owned service names in
// depends_on is a 409 naming the provider and the referencing service — the
// store's typed pin error must reach the wire as an actionable conflict, not
// as a 500.
func TestDeleteServicePinnedByFileIs409(t *testing.T) {
	fs := seededStore()
	fs.deleteServiceErr = store.ErrServicePinnedByFile{Provider: "file:shop.yaml", Service: "storefront"}
	h := newHandler(fs)
	id := createSvc(t, h, "checkout")
	rec := do(h, p1Editor, http.MethodDelete, "/api/v1/projects/p1/services/"+id, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("pinned delete = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"pinned_by_file", "file:shop.yaml", "storefront"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("409 body %q misses %q", rec.Body.String(), want)
		}
	}
}

// The detail carries the service's SLO target INVENTORY (FR-024 AC-0163-8): the gate policy
// editor offers only the windows a target exists for, so the list must be present and an ARRAY
// even when empty — a null or an absent key would leave the editor unable to tell "no targets"
// from "unknown". The fake returns a nil slice for the empty case on purpose: what the first
// half proves is the handler's normalization, not the fake's courtesy.
func TestServiceDetailCarriesItsSLOTargetInventory(t *testing.T) {
	h := newHandler(seededStore())
	id := createSvc(t, h, "checkout")

	body := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, "").Body.String()
	if !strings.Contains(body, `"sla_targets":[]`) {
		t.Fatalf("detail without targets must serialize an empty array, got: %s", body)
	}

	// Two targets declared OUT of canonical order come back IN it, each with its objective.
	for _, put := range []string{`{"window":"90d","objective":99.5}`, `{"window":"24h","objective":99}`} {
		if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/sla-target", put); rec.Code != http.StatusOK {
			t.Fatalf("PUT sla-target %s = %d: %s", put, rec.Code, rec.Body.String())
		}
	}
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1/services/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var v struct {
		SLATargets []struct {
			Window    string  `json:"window"`
			Objective float64 `json:"objective"`
			UpdatedAt string  `json:"updated_at"`
		} `json:"sla_targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(v.SLATargets) != 2 {
		t.Fatalf("sla_targets = %+v, want two entries", v.SLATargets)
	}
	if v.SLATargets[0].Window != "24h" || v.SLATargets[0].Objective != 99 {
		t.Errorf("first entry = %+v, want 24h/99 (canonical order, not write order)", v.SLATargets[0])
	}
	if v.SLATargets[1].Window != "90d" || v.SLATargets[1].Objective != 99.5 {
		t.Errorf("second entry = %+v, want 90d/99.5", v.SLATargets[1])
	}
	for _, e := range v.SLATargets {
		if e.UpdatedAt == "" || strings.HasPrefix(e.UpdatedAt, "0001-") {
			t.Errorf("entry %s carries no updated_at: %+v", e.Window, e)
		}
	}
}
