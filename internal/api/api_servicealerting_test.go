package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 phase 5 (§16.6a): the HTTP write surface of alerting ownership.
//
// The behaviour these pin is not "the handler compiles a struct". It is the small set of ways a
// paging declaration can be destroyed by a well-meaning request: a PATCH that mentions one field
// silently disowning a service, `[]` being read as absence, a server-owned latch arriving on the
// wire and being accepted, an invalid declaration reaching the database. Every refusal below also
// asserts that NOTHING was written, because a 400 that half-applied is the failure mode that
// matters.

const alertingSvcID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// seedAlertingService plants a service with a real uuid-shaped id and, optionally, a 30d SLA
// target to hang burn alerting off. `alerting` nil means "never declared", which is the schema
// default (`down`, not unknown, confirmed over 2) rather than a zero struct.
func seedAlertingService(fs *fakeStore, projectID string, alerting *domain.ServiceAlertPolicy, withTarget bool) string {
	s := &fakeService{
		svc:      domain.Service{ID: alertingSvcID, ProjectID: projectID, Slug: "checkout", Name: "Checkout"},
		alerting: alerting,
	}
	if withTarget {
		s.burnTargets = map[string]*fakeBurnTarget{"30d": {}}
	}
	fs.serviceStore()[alertingSvcID] = s
	return alertingSvcID
}

func alertingPath(projectID, serviceID string) string {
	return "/api/v1/projects/" + projectID + "/services/" + serviceID + "/alerting"
}

func burnPath(projectID, serviceID string) string {
	return "/api/v1/projects/" + projectID + "/services/" + serviceID + "/sla-target/burn-alerting"
}

func decodeAlerting(t *testing.T, body []byte) struct {
	OwnsPaging         bool     `json:"owns_paging"`
	PageOn             []string `json:"page_on"`
	PageOnUnknown      bool     `json:"page_on_unknown"`
	ConfirmEvaluations int      `json:"confirm_evaluations"`
} {
	t.Helper()
	var out struct {
		OwnsPaging         bool     `json:"owns_paging"`
		PageOn             []string `json:"page_on"`
		PageOnUnknown      bool     `json:"page_on_unknown"`
		ConfirmEvaluations int      `json:"confirm_evaluations"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode alerting response: %v (%s)", err, body)
	}
	return out
}

// A PATCH that mentions ONE field must leave the others exactly as they were. This is the pin that
// keeps a partial edit from disowning a service: with a plain `bool`, a body of
// `{"confirm_evaluations":4}` would arrive as owns_paging=false and close every open announcement
// the service has, on both signals, with `ownership_disabled`.
func TestPatchServiceAlertingLeavesOmittedFieldsAlone(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", &domain.ServiceAlertPolicy{
		OwnsPaging:         true,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown, domain.ServiceAlertDegraded},
		PageOnUnknown:      true,
		ConfirmEvaluations: 3,
	}, false)

	rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"confirm_evaluations":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeAlerting(t, rec.Body.Bytes())
	if !got.OwnsPaging {
		t.Error("owns_paging was silently turned OFF by a PATCH that never mentioned it")
	}
	if !got.PageOnUnknown {
		t.Error("page_on_unknown was silently cleared")
	}
	if len(got.PageOn) != 2 || got.PageOn[0] != "degraded" || got.PageOn[1] != "down" {
		t.Errorf("page_on = %v, want [degraded down] unchanged", got.PageOn)
	}
	if got.ConfirmEvaluations != 4 {
		t.Errorf("confirm_evaluations = %d, want 4", got.ConfirmEvaluations)
	}

	// And the same in the other direction: an explicit `false` is a real edit, not absence.
	rec = do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"owns_paging":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH owns_paging=false = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeAlerting(t, rec.Body.Bytes()); got.OwnsPaging || got.ConfirmEvaluations != 4 {
		t.Errorf("owns_paging=false lost the rest of the policy: %+v", got)
	}
}

// `page_on: []` is a LEGAL declaration — "page for no state", which dis-arms LIVE coverage — and it
// is not the same statement as omitting the field.
func TestPatchServiceAlertingDistinguishesEmptyListFromAbsent(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", &domain.ServiceAlertPolicy{
		OwnsPaging:         true,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDown},
		ConfirmEvaluations: 2,
	}, false)

	rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"page_on":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH page_on=[] = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeAlerting(t, rec.Body.Bytes())
	if got.PageOn == nil {
		t.Fatal("page_on came back null; `[]` must round-trip as an empty array")
	}
	if len(got.PageOn) != 0 {
		t.Errorf("page_on = %v, want []", got.PageOn)
	}
	if stored := fs.serviceStore()[id].alertPolicy(); len(stored.PageOn) != 0 || stored.PageOn == nil {
		t.Errorf("stored page_on = %v (nil=%v), want an empty non-nil list", stored.PageOn, stored.PageOn == nil)
	}

	// Absence now means "leave the empty list alone" — it must not restore the default.
	rec = do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"page_on_unknown":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("follow-up PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeAlerting(t, rec.Body.Bytes()); len(got.PageOn) != 0 {
		t.Errorf("an omitted page_on resurrected %v; absent is unchanged, and unchanged was []", got.PageOn)
	}
}

// The echo is the CANONICAL STORED policy, not the request: `page_on` comes back sorted and
// deduplicated, because that is what the next PATCH will merge onto.
func TestPatchServiceAlertingEchoesTheCanonicalStoredPolicy(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, false)

	rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id),
		`{"owns_paging":true,"page_on":["down","degraded","down"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeAlerting(t, rec.Body.Bytes())
	if len(got.PageOn) != 2 || got.PageOn[0] != "degraded" || got.PageOn[1] != "down" {
		t.Errorf("page_on = %v, want the canonical [degraded down] — sorted and deduped", got.PageOn)
	}
	// The undeclared service's schema default survives the merge: confirm_evaluations was never
	// mentioned and must still be 2, not 0.
	if got.ConfirmEvaluations != 2 {
		t.Errorf("confirm_evaluations = %d, want the untouched default 2", got.ConfirmEvaluations)
	}
	if stored := fs.serviceStore()[id].alertPolicy(); len(stored.PageOn) != 2 {
		t.Errorf("stored page_on = %v, want the deduplicated pair", stored.PageOn)
	}
}

// Server-owned fields are REFUSED, not ignored. The refusal is structural — the request types do
// not declare them and `decodeJSONBody` disallows unknown fields — so it covers every latch, lease
// and generation column without a denylist anybody has to maintain.
func TestPatchServiceAlertingRejectsServerOwnedFields(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, false)

	for _, body := range []string{
		`{"owns_paging":true,"alert_config_generation":7}`,
		`{"alert_config_generation":0}`,
		`{"owns_paging":true,"live_firing":true}`,
		`{"owns_paging":true,"emitted_state":"down"}`,
		`{"owns_paging":true,"live_lease_expires_at":"2026-08-18T00:00:00Z"}`,
		`{"owns_paging":true,"evaluated_at":"2026-08-18T00:00:00Z"}`,
		`{"owns_paging":true,"episode_id":"x"}`,
		// A trailing second JSON value must not smuggle one past the decoder either.
		`{"owns_paging":true}{"alert_config_generation":7}`,
	} {
		rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", body, rec.Code)
		}
	}
	if fs.serviceStore()[id].alertWrites != 0 {
		t.Errorf("a rejected body still reached the store %d times", fs.serviceStore()[id].alertWrites)
	}
}

// The bounds and the vocabulary are the ONE domain validator's, and a violation is a 400 that
// writes nothing — never a store error surfacing as a 500.
func TestPatchServiceAlertingRefusesInvalidDeclarations(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, false)

	for _, body := range []string{
		`{"confirm_evaluations":0}`,
		`{"confirm_evaluations":11}`,
		`{"confirm_evaluations":-1}`,
		// `unknown` has its own switch; listing it in page_on would be a second way to say one thing.
		`{"page_on":["unknown"]}`,
		`{"page_on":["excluded"]}`,
		`{"page_on":["healthy"]}`,
		`{"page_on":["down","nonsense"]}`,
	} {
		rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" {
			t.Errorf("PATCH %s returned no error message", body)
		}
	}
	if fs.serviceStore()[id].alertWrites != 0 {
		t.Errorf("an invalid declaration reached the store %d times", fs.serviceStore()[id].alertWrites)
	}
}

// A file-managed service refuses UI paging edits with 409 and renders read-only: these fields are
// part of the desired state, so the bundle owns them (§16.6a).
func TestServiceAlertingRefusesFileManagedService(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)
	fs.serviceStore()[id].fileManaged = true

	rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"owns_paging":true}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("PATCH on a file-managed service = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	rec = do(h, p1Editor, http.MethodPut, burnPath("p1", id),
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page"}]}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("burn PUT on a file-managed service = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// Reading the declaration is still allowed — read-only is not invisible.
	if rec := do(h, p1Editor, http.MethodGet, alertingPath("p1", id), ""); rec.Code != http.StatusOK {
		t.Errorf("GET on a file-managed service = %d, want 200", rec.Code)
	}
}

// A service that is not this project's is indistinguishable from one that does not exist, and
// neither writes anything. A malformed id is the transport's own 400 — the store must never see a
// value PostgreSQL would reject with a uuid cast error.
func TestServiceAlertingTenancyAndIDShape(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)
	absent := "00000000-0000-4000-8000-00000000dead"

	// o1Admin can write p2, but the service lives in p1: the store's honest 404.
	o1Admin := authz.Principal{UserID: "oa", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}}
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, alertingPath("p2", id), `{"owns_paging":true}`},
		{http.MethodGet, alertingPath("p2", id), ""},
		{http.MethodPut, burnPath("p2", id), `{"window":"30d","burn_alert_enabled":true}`},
		{http.MethodPatch, alertingPath("p1", absent), `{"owns_paging":true}`},
		{http.MethodGet, alertingPath("p1", absent), ""},
		{http.MethodPut, burnPath("p1", absent), `{"window":"30d","burn_alert_enabled":true}`},
	} {
		if rec := do(h, o1Admin, tc.method, tc.path, tc.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if s := fs.serviceStore()[id]; s.alertWrites != 0 || s.burnWrites != 0 {
		t.Errorf("a cross-tenant request wrote: alert=%d burn=%d", s.alertWrites, s.burnWrites)
	}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, alertingPath("p1", "not-a-uuid"), `{"owns_paging":true}`},
		{http.MethodGet, alertingPath("p1", "not-a-uuid"), ""},
		{http.MethodPut, burnPath("p1", "not-a-uuid"), `{"window":"30d"}`},
	} {
		if rec := do(h, p1Editor, tc.method, tc.path, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400 (malformed uuid is the transport's problem)", tc.method, tc.path, rec.Code)
		}
	}
}

// Paging is a WRITE. A viewer may read the declaration and may not change it.
func TestServiceAlertingAuthz(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	if rec := do(h, p1Viewer, http.MethodGet, alertingPath("p1", id), ""); rec.Code != http.StatusOK {
		t.Errorf("viewer GET alerting = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Viewer, http.MethodPatch, alertingPath("p1", id), `{"owns_paging":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer PATCH alerting = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodPut, burnPath("p1", id), `{"window":"30d","burn_alert_enabled":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer PUT burn-alerting = %d, want 403", rec.Code)
	}
	if s := fs.serviceStore()[id]; s.alertWrites != 0 || s.burnWrites != 0 {
		t.Errorf("a forbidden request wrote: alert=%d burn=%d", s.alertWrites, s.burnWrites)
	}
}

// The declaration is readable on its own route and on the service detail, and NEITHER carries a
// latch, a lease, a generation or an episode.
func TestGetServiceAlertingAndDetailBlock(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", &domain.ServiceAlertPolicy{
		OwnsPaging:         true,
		PageOn:             []domain.ServiceAlertState{domain.ServiceAlertDegraded, domain.ServiceAlertDown},
		ConfirmEvaluations: 3,
	}, false)

	rec := do(h, p1Editor, http.MethodGet, alertingPath("p1", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET alerting = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeAlerting(t, rec.Body.Bytes())
	if !got.OwnsPaging || got.ConfirmEvaluations != 3 || len(got.PageOn) != 2 {
		t.Errorf("GET alerting = %+v, want the declared policy", got)
	}
	for _, forbidden := range []string{
		"alert_config_generation", "live_firing", "emitted_state", "lease", "episode", "evaluated_at",
	} {
		if body := rec.Body.String(); contains(body, forbidden) {
			t.Errorf("the alerting read exposed a server-owned field %q: %s", forbidden, body)
		}
	}

	rec = do(h, p1Editor, http.MethodGet, "/api/v1/projects/p1/services/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET service detail = %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Alerting *struct {
			OwnsPaging         bool     `json:"owns_paging"`
			PageOn             []string `json:"page_on"`
			ConfirmEvaluations int      `json:"confirm_evaluations"`
		} `json:"alerting"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Alerting == nil {
		t.Fatal("the service detail carries no alerting block")
	}
	if !detail.Alerting.OwnsPaging || detail.Alerting.ConfirmEvaluations != 3 {
		t.Errorf("detail alerting block = %+v, want the declared policy", *detail.Alerting)
	}
}

// ── Burn alerting for a SERVICE target ──────────────────────────────────────

func TestSetServiceBurnAlerting(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	body := `{"window":"30d","burn_alert_enabled":true,"burn_rules":[` +
		`{"long_window_seconds":21600,"short_window_seconds":1800,"threshold":6,"severity":"ticket"},` +
		`{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page"}]}`
	rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT burn-alerting = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Window           string            `json:"window"`
		BurnAlertEnabled bool              `json:"burn_alert_enabled"`
		BurnRules        []domain.BurnRule `json:"burn_rules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode burn response: %v", err)
	}
	if out.Window != "30d" || !out.BurnAlertEnabled || len(out.BurnRules) != 2 {
		t.Fatalf("burn echo = %+v", out)
	}
	// The echo is the canonical KEY ORDER the store persists, not the order the client typed.
	if out.BurnRules[0].Key() > out.BurnRules[1].Key() {
		t.Errorf("burn rules echoed out of canonical key order: %v", out.BurnRules)
	}
	for _, r := range out.BurnRules {
		if r.Firing {
			t.Errorf("the echo claims a rule is firing: %+v", r)
		}
	}
	target := fs.serviceStore()[id].burnTargets["30d"]
	if !target.enabled || len(target.rules) != 2 {
		t.Errorf("stored burn target = %+v", target)
	}

	// The window defaults to 30d, exactly as the objective write does.
	if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), `{"burn_alert_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("default-window burn PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if target.enabled {
		t.Error("disabling burn alerting did not reach the store")
	}
	if len(target.rules) != 0 {
		t.Errorf("a PUT with no burn_rules left %d rules; the body IS the declaration", len(target.rules))
	}
}

// `firing` is the SERVER's latch (§16.4b). It must be refused on the wire, not silently zeroed:
// a request that says "this rule is firing" and gets a 200 has been told it was believed.
func TestSetServiceBurnAlertingRejectsFiring(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	for _, body := range []string{
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page","firing":true}]}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page","firing":false}]}`,
		`{"window":"30d","burn_alert_enabled":true,"alert_generation":3}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page","rule_key":"page/3600/300/14.4"}]}`,
	} {
		if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), body); rec.Code != http.StatusBadRequest {
			t.Errorf("burn PUT %s = %d, want 400", body, rec.Code)
		}
	}
	if fs.serviceStore()[id].burnWrites != 0 {
		t.Errorf("a rejected burn body reached the store %d times", fs.serviceStore()[id].burnWrites)
	}
}

// The rule bounds and the canonical-key collision check are the ONE domain validator's, and a
// violation writes nothing: two rules with the same key would make one latch answer for both.
func TestSetServiceBurnAlertingValidatesRules(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	page := `{"long_window_seconds":3600,"short_window_seconds":300,"threshold":14.4,"severity":"page"}`
	for _, body := range []string{
		// Same severity, same window pair, same threshold — one canonical key, two rules.
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[` + page + `,` + page + `]}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":0,"severity":"page"}]}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":300,"short_window_seconds":3600,"threshold":2,"severity":"page"}]}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":30,"threshold":2,"severity":"page"}]}`,
		`{"window":"30d","burn_alert_enabled":true,"burn_rules":[{"long_window_seconds":3600,"short_window_seconds":300,"threshold":2,"severity":"loud"}]}`,
	} {
		if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), body); rec.Code != http.StatusBadRequest {
			t.Errorf("burn PUT %s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	// A window outside the standard set is a 400 before the store is reached; a standard window
	// with no declared objective is the store's 404, because burn alerting needs a number to
	// measure against.
	if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), `{"window":"5m","burn_alert_enabled":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown window = %d, want 400", rec.Code)
	}
	if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), `{"window":"7d","burn_alert_enabled":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("window with no objective = %d, want 404", rec.Code)
	}
	if fs.serviceStore()[id].burnWrites != 0 {
		t.Errorf("an invalid burn declaration reached the store %d times", fs.serviceStore()[id].burnWrites)
	}
}

// Every paging-config change is audited with its actor, so the actor must reach the store — the
// audit row is written INSIDE the mutation transaction and there is no second chance to add it.
func TestServiceAlertingCarriesTheActor(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	if rec := do(h, p1Editor, http.MethodPatch, alertingPath("p1", id), `{"owns_paging":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, p1Editor, http.MethodPut, burnPath("p1", id), `{"window":"30d","burn_alert_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("burn PUT = %d: %s", rec.Code, rec.Body.String())
	}
	actors := fs.serviceStore()[id].alertActors
	if len(actors) != 2 {
		t.Fatalf("store saw %d actors, want 2", len(actors))
	}
	for _, a := range actors {
		if a.ActorUserID != "pe" || a.ViaToken {
			t.Errorf("actor = %+v, want the session's user id and via_token=false", a)
		}
	}
}

// The objective write is untouched by phase 5: its body is still {window, objective} and NOTHING
// else, so a burn field there is a 400 pointing at the wrong endpoint rather than a silent merge.
func TestSLATargetStillRefusesBurnFields(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, true)

	for _, body := range []string{
		`{"window":"30d","objective":99.9,"burn_alert_enabled":true}`,
		`{"window":"30d","objective":99.9,"burn_rules":[]}`,
	} {
		if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/sla-target", body); rec.Code != http.StatusBadRequest {
			t.Errorf("sla-target %s = %d, want 400", body, rec.Code)
		}
	}
	if rec := do(h, p1Editor, http.MethodPut, "/api/v1/projects/p1/services/"+id+"/sla-target",
		`{"window":"30d","objective":99.9}`); rec.Code != http.StatusOK {
		t.Errorf("objective-only sla-target = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The detail carries what the declaration is PRODUCING, not just what it says — and it carries no
// server-owned field, because a read that could return a latch is a read somebody eventually sends
// back as a write.
func TestServiceDetailCarriesCoverageWithoutServerOwnedState(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	id := seedAlertingService(fs, "p1", nil, false)

	rec := do(h, p1Editor, http.MethodGet, "/api/v1/projects/p1/services/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AlertingState *struct {
			Live struct {
				Armed       bool   `json:"armed"`
				Reason      string `json:"reason"`
				EvaluatedAt string `json:"evaluated_at"`
			} `json:"live"`
			Burn struct {
				Armed bool `json:"armed"`
			} `json:"burn"`
		} `json:"alerting_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AlertingState == nil {
		t.Fatal("the detail carries no coverage block")
	}
	if body.AlertingState.Live.Armed {
		t.Fatalf("a service that owns no paging reported armed coverage: %+v", body.AlertingState)
	}
	if body.AlertingState.Live.Reason == "" {
		t.Fatal("coverage is not armed and does not say why; `not armed` has eleven causes")
	}

	// No latch, lease-owner, generation or episode field may appear anywhere in the block.
	raw := rec.Body.String()
	for _, forbidden := range []string{
		"alert_config_generation", "alert_generation", "live_firing", "emitted_state",
		"emitted_seq", "last_verdict", "episode_id", "rule_key",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the service detail exposes the server-owned field %q", forbidden)
		}
	}
}
