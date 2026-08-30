package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-025 (func-change-intelligence.md D2, D3, D5, D6, D7, D8, D11, D12, §5a, §7 "Authorisation
// and tokens" / "Bounds and shape" / "Timeline" / "Comparison"; iter-0165 changeset 3): the HTTP
// surface of change intelligence against the scripted fake store. What these pin is the CONTRACT
// a pipeline, the CLI and the SPA read — the record normalizing every text field BEFORE the store
// sees it, the strict body naming the offending field, the domain's validators refusing before a
// rate token is spent, the closed codes mapped onto 400/404/409 with the code first in the body,
// the §5a bounds refusing before any store call with the exact Retry-After, the presence
// discipline of the timeline (decision absent/live/aged out, incidents always an array), the
// three side shapes of the comparison, the D12 allow-list proven through the real authz.Can, and
// every metric moving exactly when the spec says.

const (
	changeIncID   = "4d8e3e4f-6b5c-4f7a-abcd-3e4f5a6b7c8d"
	changeP2IncID = "5d8e3e4f-6b5c-4f7a-abcd-3e4f5a6b7c8e"
	changeChgID   = "019906c0-aaaa-7abc-8def-0123456789ab"
	changeChgID2  = "019906c0-bbbb-7abc-8def-0123456789ab"

	// changeBody is a canonical record; tests mutate it with changeWith.
	changeBody = `{"kind":"deploy","phase":"succeeded","source":"github-actions","external_id":"run-42","ref":"v4.2.1","url":"https://ci.example/run/42","occurred_at":"2026-08-29T09:58:00Z"}`
)

// changeCIToken is the CI token of D12: role editor under the allow-list [gate:evaluate,
// change:record], as the auth middleware builds it from the token row.
var changeCIToken = authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "tok-ci", ViaToken: true, AuditLabel: "token:ci",
	Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}},
	Actions:     []authz.Action{authz.ActionGateEvaluate, authz.ActionChangeRecord}}

// changeFakeMetrics records every change recorder call so a test can assert exactly what moved.
type changeFakeMetrics struct {
	mu       sync.Mutex
	recorded []string // kind/phase/replayed
	rejected []string
	compared []string
}

func (m *changeFakeMetrics) RecordChangeRecorded(kind, phase string, replayed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recorded = append(m.recorded, fmt.Sprintf("%s/%s/%t", kind, phase, replayed))
	return nil
}
func (m *changeFakeMetrics) RecordChangeRecordRejected(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejected = append(m.rejected, reason)
	return nil
}
func (m *changeFakeMetrics) RecordChangeCompare(outcome string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compared = append(m.compared, outcome)
	return nil
}
func (m *changeFakeMetrics) snapshot() (recorded, rejected, compared []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.recorded...), append([]string(nil), m.rejected...), append([]string(nil), m.compared...)
}

func changeLimits() api.ChangeLimits {
	return api.ChangeLimits{RecordInflightProcess: 4, RecordRatePrincipalPerMinute: 10, RecordRateProcessPerMinute: 60,
		ReadInflightProcess: 4, MaxPast: 24 * time.Hour, MaxFuture: 5 * time.Minute}
}

// newChangeHandler wires the gate too, so the CI-token scenario can ask the gate on the same router.
func newChangeHandler(fs *fakeStore, limits api.ChangeLimits, m api.ChangeMetrics) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithGate(gateLimits(), nil).WithChange(limits, m).Router()
}

func changeP(svc string) string { return "/api/v1/projects/p1/services/" + svc + "/changes" }

func changeWith(t *testing.T, base string, set map[string]any, drop ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(base), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = v
	}
	for _, k := range drop {
		delete(m, k)
	}
	out, _ := json.Marshal(m)
	return string(out)
}

func changeRange(from, to time.Time) string {
	return "?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339))
}

func changeRow(id string, phase domain.ChangePhase, at time.Time) domain.ChangePhaseRow {
	return domain.ChangePhaseRow{ID: id, ProjectID: "p1", ServiceID: gateSvcID, Source: "github-actions", ExternalID: "run-42",
		Kind: domain.ChangeKindDeploy, Phase: phase, Ref: "v4.2.1", URL: "https://ci.example/run/42", OccurredAt: at,
		ActorLabel: "token:ci", ViaToken: true, RecordedAt: at.Add(time.Second)}
}

func changeGroup(phases ...domain.ChangePhaseRow) domain.ChangeGroup {
	g := domain.ChangeGroup{Source: "github-actions", ExternalID: "run-42", Kind: domain.ChangeKindDeploy,
		Phases: phases, Incidents: []domain.ChangeIncidentLink{}}
	for _, p := range phases {
		if p.OccurredAt.After(g.LatestOccurredAt) {
			g.LatestOccurredAt = p.OccurredAt
		}
	}
	return g
}

func changeFigure(avail float64) *domain.ChangeCompareFigure {
	return &domain.ChangeCompareFigure{Availability: avail, GoodSeconds: 3540, BadSeconds: 60, UnknownSeconds: 0, ExcludedSeconds: 0, Buckets: 60}
}

func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// ── the record ───────────────────────────────────────────────────────────────────────────────

// D2's transport layer: every text field is NFC + trimmed BEFORE the store sees it — a
// decomposed é and a trailing space arrive composed and trimmed — and D5's actor is the
// principal, never the body. The response is the row (identity + phase), `replayed` false, 201.
func TestChangeRecordHappyPathNormalizesBeforeTheStore(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	m := &changeFakeMetrics{}
	h := newChangeHandler(fs, changeLimits(), m)

	decomposed := "café-42 " // e + COMBINING ACUTE, trailing space
	body := changeWith(t, changeBody, map[string]any{"external_id": decomposed, "ref": "  v4.2.1\t", "url": " https://ci.example/run/42 ", "source": " github-actions "})
	rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), body)
	out := wantStatus(t, rec, http.StatusCreated, "")
	wantKeys(t, out, "replayed", "change")

	inputs := fs.changeInputs()
	if len(inputs) != 1 {
		t.Fatalf("store calls = %v", fs.changeCalls())
	}
	in := inputs[0]
	if in.ExternalID != "café-42" || in.Ref != "v4.2.1" || in.URL != "https://ci.example/run/42" || in.Source != "github-actions" {
		t.Fatalf("the store saw un-normalized text: %q %q %q %q", in.ExternalID, in.Ref, in.URL, in.Source)
	}
	if in.Kind != domain.ChangeKindDeploy || in.Phase != domain.ChangePhaseSucceeded || in.ProjectID != "p1" || in.ServiceID != gateSvcID {
		t.Fatalf("input = %+v", in)
	}
	if !in.OccurredAt.Equal(time.Date(2026, 8, 29, 9, 58, 0, 0, time.UTC)) || in.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at = %s, want the body's instant in UTC", in.OccurredAt)
	}
	if in.MaxPast != 24*time.Hour || in.MaxFuture != 5*time.Minute {
		t.Fatalf("clock bounds handed to the store = %s/%s, want change.max_past/max_future", in.MaxPast, in.MaxFuture)
	}
	if in.Actor.Label != "editor@acme" || in.Actor.ActorUserID != "gate-editor" || in.Actor.ViaToken {
		t.Fatalf("actor = %+v, want the principal", in.Actor)
	}
	if in.DecisionID != nil {
		t.Fatalf("decision_id = %v, want none", *in.DecisionID)
	}

	got := decodeMap(t, out)
	if got["replayed"] != false {
		t.Fatalf("replayed = %v", got["replayed"])
	}
	change := got["change"].(map[string]any)
	for k, want := range map[string]any{"id": changeChgID, "service_id": gateSvcID, "source": "github-actions", "external_id": "café-42",
		"kind": "deploy", "phase": "succeeded", "ref": "v4.2.1", "url": "https://ci.example/run/42", "actor_label": "editor@acme",
		"actor_user_id": "gate-editor", "via_token": false} {
		if change[k] != want {
			t.Fatalf("change.%s = %v, want %v (%s)", k, change[k], want, out)
		}
	}
	if _, has := change["decision_id"]; has {
		t.Fatalf("decision_id must be absent when none was given: %s", out)
	}
	recorded, rejected, _ := m.snapshot()
	if len(recorded) != 1 || recorded[0] != "deploy/succeeded/false" || len(rejected) != 0 {
		t.Fatalf("metrics = %v %v", recorded, rejected)
	}
}

// `occurred_at` absent defaults to the server clock; `decision_id` rides through normalized; a
// token principal is a NULL actor_user_id with via_token true and the `token:<name>` label.
func TestChangeRecordDefaultsAndTokenActor(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})
	before := time.Now().UTC()
	body := changeWith(t, changeBody, map[string]any{"decision_id": " " + strings.ToUpper(gateDecID) + " "}, "occurred_at", "ref", "url")
	rec := do(h, gateToken, http.MethodPost, changeP(gateSvcID), body)
	out := wantStatus(t, rec, http.StatusCreated, "")
	in := fs.changeInputs()[0]
	if in.OccurredAt.Before(before) || in.OccurredAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("occurred_at default = %s, want the server's now", in.OccurredAt)
	}
	if in.Ref != "" || in.URL != "" {
		t.Fatalf("absent ref/url must be empty, got %q %q", in.Ref, in.URL)
	}
	if in.DecisionID == nil || *in.DecisionID != strings.ToUpper(gateDecID) {
		t.Fatalf("decision_id = %v, want the trimmed value (case is the store's to fold)", in.DecisionID)
	}
	if in.Actor.Label != "token:ci" || in.Actor.ActorUserID != "" || !in.Actor.ViaToken {
		t.Fatalf("token actor = %+v", in.Actor)
	}
	change := decodeMap(t, out)["change"].(map[string]any)
	if v, has := change["actor_user_id"]; !has || v != nil {
		t.Fatalf("actor_user_id must be present and null for a token: %s", out)
	}
	if change["via_token"] != true || change["actor_label"] != "token:ci" {
		t.Fatalf("change = %s", out)
	}
}

// D3: an identical replay is 200 with the ORIGINAL row and `replayed: true`; the metric says replayed.
func TestChangeRecordIdenticalReplayIs200(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	original := changeRow(changeChgID2, domain.ChangePhaseSucceeded, gateT0.Add(-2*time.Minute))
	original.ActorLabel, original.RecordedAt = "token:old-pipeline", gateT0.Add(-time.Minute)
	c.row, c.replayed = original, true
	m := &changeFakeMetrics{}
	h := newChangeHandler(fs, changeLimits(), m)
	rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody)
	out := wantStatus(t, rec, http.StatusOK, "")
	got := decodeMap(t, out)
	change := got["change"].(map[string]any)
	if got["replayed"] != true || change["id"] != changeChgID2 || change["actor_label"] != "token:old-pipeline" {
		t.Fatalf("replay must answer the original row: %s", out)
	}
	recorded, _, _ := m.snapshot()
	if len(recorded) != 1 || recorded[0] != "deploy/succeeded/true" {
		t.Fatalf("metrics = %v", recorded)
	}
}

// D2: the body is strict — an unknown field (an `actor` included, D5), a wrong type, a missing
// required field, a trailing value or an unparseable `occurred_at` is 400 naming it, the store is
// never asked, and each is counted as `body_invalid`.
func TestChangeRecordBodyIsStrict(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	m := &changeFakeMetrics{}
	h := newChangeHandler(fs, changeLimits(), m)
	cases := map[string]string{
		changeWith(t, changeBody, map[string]any{"actor": "me"}):               "actor: unknown field",
		changeWith(t, changeBody, map[string]any{"actor_label": "me"}):         "actor_label: unknown field",
		changeWith(t, changeBody, map[string]any{"repository": "org/repo"}):    "repository: unknown field",
		changeWith(t, changeBody, map[string]any{"kind": 1}):                   "kind: must be a string, got number",
		changeWith(t, changeBody, map[string]any{"ref": []string{"a"}}):        "ref: must be a string, got array",
		changeWith(t, changeBody, nil, "kind"):                                 "kind: is required",
		changeWith(t, changeBody, nil, "phase"):                                "phase: is required",
		changeWith(t, changeBody, nil, "source"):                               "source: is required",
		changeWith(t, changeBody, nil, "external_id"):                          "external_id: is required",
		changeWith(t, changeBody, map[string]any{"occurred_at": "yesterday"}):  "occurred_at: must be an RFC3339 timestamp",
		changeWith(t, changeBody, map[string]any{"occurred_at": "2026-08-29"}): "occurred_at: must be an RFC3339 timestamp",
		changeBody + changeBody:                                                "single JSON value",
		``:                                                                     "body: must be a JSON object",
		`[]`:                                                                   "body: must be an object",
	}
	for body, want := range cases {
		rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), body)
		wantStatus(t, rec, http.StatusBadRequest, want)
	}
	if calls := fs.changeCalls(); len(calls) != 0 {
		t.Fatalf("a refused body still reached the store: %v", calls)
	}
	_, rejected, _ := m.snapshot()
	if len(rejected) != len(cases) {
		t.Fatalf("rejected = %v, want %d × body_invalid", rejected, len(cases))
	}
	for _, r := range rejected {
		if r != "body_invalid" {
			t.Fatalf("rejected = %v, want only body_invalid", rejected)
		}
	}
}

// D2's domain layer runs in the handler on the canonical values, BEFORE the §5a bounds: every
// bounds-and-shape refusal of §7 is 400 with its closed code first, the store is never asked, and
// no rate token is spent — ten refusals against a ten-per-minute bucket leave the eleventh, valid
// request admitted.
func TestChangeRecordDomainValidationBeforeTheBounds(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	m := &changeFakeMetrics{}
	h := newChangeHandler(fs, changeLimits(), m)
	cases := []struct {
		set  map[string]any
		code string
	}{
		{map[string]any{"kind": "config"}, "kind_invalid"},
		{map[string]any{"kind": "Deploy"}, "kind_invalid"},
		{map[string]any{"phase": "running"}, "phase_invalid"},
		{map[string]any{"source": "Deploy_Bot"}, "source_invalid"},
		{map[string]any{"source": ""}, "source_invalid"},
		{map[string]any{"external_id": strings.Repeat("x", 129)}, "external_id_invalid"},
		{map[string]any{"external_id": "   "}, "external_id_invalid"},
		{map[string]any{"ref": "v1​"}, "ref_invalid"},
		{map[string]any{"ref": "v1\nv2"}, "ref_invalid"},
		{map[string]any{"url": "http://ci.example/run/42"}, "url_invalid"},
	}
	for _, tc := range cases {
		rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeWith(t, changeBody, tc.set))
		out := wantStatus(t, rec, http.StatusBadRequest, tc.code)
		if got := errorOf(t, out); !strings.HasPrefix(got, tc.code) {
			t.Fatalf("error = %q, want the code first", got)
		}
	}
	if calls := fs.changeCalls(); len(calls) != 0 {
		t.Fatalf("a refused record reached the store: %v", calls)
	}
	// The eleventh request is the first to reach the bucket: 201, not 429.
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	_, rejected, _ := m.snapshot()
	want := make([]string, 0, len(cases))
	for _, tc := range cases {
		want = append(want, tc.code)
	}
	if strings.Join(rejected, ",") != strings.Join(want, ",") {
		t.Fatalf("rejected = %v, want %v", rejected, want)
	}
}

// The store's closed codes map onto D3/D11's answers without string matching — the order and
// identity conflicts 409, the rest 400 — with the code first in the body; a foreign service is
// the tenant 404 and a plain failure 500, neither counted as a rejection.
func TestChangeRecordStoreCodesMapToStatuses(t *testing.T) {
	cases := []struct {
		err    error
		status int
		text   string
		metric string
	}{
		{domain.NewChangeError(domain.ChangeErrPhaseOrder, "phase", "succeeded already recorded"), http.StatusConflict, "phase_order (phase): succeeded already recorded", "phase_order"},
		{domain.NewChangeError(domain.ChangeErrPhaseExists, "ref", "succeeded is already recorded with a different ref"), http.StatusConflict, "phase_exists (ref)", "phase_exists"},
		{domain.NewChangeError(domain.ChangeErrKindMismatch, "kind", "this change is a deploy"), http.StatusConflict, "kind_mismatch", "kind_mismatch"},
		{domain.NewChangeError(domain.ChangeErrDecisionUnknown, "decision_id", "not a decision of this service"), http.StatusBadRequest, "decision_unknown", "decision_unknown"},
		{domain.NewChangeError(domain.ChangeErrOccurredBeforeStart, "occurred_at", "must not precede started"), http.StatusBadRequest, "occurred_at_before_start", "occurred_at_before_start"},
		{domain.NewChangeError(domain.ChangeErrOccurredOutOfBounds, "occurred_at", "must be within 24h"), http.StatusBadRequest, "occurred_at_out_of_bounds", "occurred_at_out_of_bounds"},
		{fmt.Errorf("wrapped: %w", domain.NewChangeError(domain.ChangeErrRefInvalid, "ref", "must be Unicode NFC")), http.StatusBadRequest, "ref_invalid", "ref_invalid"},
		{store.ErrNotFound, http.StatusNotFound, "not found", ""},
		{errors.New("boom"), http.StatusInternalServerError, "internal error", ""},
	}
	for _, tc := range cases {
		fs := seededStore()
		seedGateService(fs, "p1", gateSvcID)
		fs.changeState().recordErr = tc.err
		m := &changeFakeMetrics{}
		h := newChangeHandler(fs, changeLimits(), m)
		rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody)
		out := wantStatus(t, rec, tc.status, tc.text)
		if got := errorOf(t, out); tc.metric != "" && !strings.HasPrefix(got, tc.metric) {
			t.Fatalf("error = %q, want the code %q first", got, tc.metric)
		}
		recorded, rejected, _ := m.snapshot()
		if len(recorded) != 0 {
			t.Fatalf("%v: a refused record counted as recorded", tc.err)
		}
		if tc.metric == "" && len(rejected) != 0 {
			t.Fatalf("%v: a tenant 404 or a server error must not count as a rejection: %v", tc.err, rejected)
		}
		if tc.metric != "" && (len(rejected) != 1 || rejected[0] != tc.metric) {
			t.Fatalf("%v: rejected = %v, want [%s]", tc.err, rejected, tc.metric)
		}
	}
}

// D12/D15 on the record: `change:record` is editor+ — a viewer is 403 with no store call, an
// editor and an admin 201; an invisible project is 404; a malformed service id is 400 BEFORE the
// store; a service of another project is the store's tenant 404.
func TestChangeRecordAuthorizationAndTenantOrder(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	seedGateService(fs, "p2", gateP2SvcID)
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})

	wantStatus(t, do(h, gateViewer, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, outsider, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusNotFound, "not found")
	wantStatus(t, do(h, gateEditor, http.MethodPost, "/api/v1/projects/p3/services/"+gateSvcID+"/changes", changeBody), http.StatusNotFound, "not found")
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP("not-a-uuid"), changeBody), http.StatusBadRequest, "serviceID must be a UUID")
	if calls := fs.changeCalls(); len(calls) != 0 {
		t.Fatalf("a refusal before the store still reached it: %v", calls)
	}
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP(gateP2SvcID), changeBody), http.StatusNotFound, "not found")
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	wantStatus(t, do(h, gateAdmin, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	if calls := fs.changeCalls(); len(calls) != 3 {
		t.Fatalf("store calls = %v, want the foreign 404 and the two 201s", calls)
	}
}

// §7 "Authorisation and tokens", through the REAL authz.Can in the handlers (no mock): a token
// `role: editor, actions: [gate:evaluate, change:record]` is 201 on record, 200 on POST …/gate,
// 403 on GET …/services, 403 on PUT …/gate/policy and 403 on the timeline (project:read is not
// in its list) — its project stays visible, so those are 403 and never 404. An empty list grants
// nothing; a nil list is the editor it always was.
func TestChangeCITokenAllowListThroughAuthzCan(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	fs.gateState().decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})

	wantStatus(t, do(h, changeCIToken, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	wantStatus(t, do(h, changeCIToken, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")
	wantStatus(t, do(h, changeCIToken, http.MethodGet, "/api/v1/projects/p1/services", ""), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, changeCIToken, http.MethodPut, gateP(gateSvcID)+"/policy", gatePolicyBody), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, changeCIToken, http.MethodGet, changeP(gateSvcID)+changeRange(gateT0.Add(-time.Hour), gateT0), ""), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, changeCIToken, http.MethodGet, changeP(gateSvcID)+"/compare?source=github-actions&external_id=run-42", ""), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, changeCIToken, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeIncID+"/changes", ""), http.StatusForbidden, "forbidden")

	none := changeCIToken
	none.Actions = []authz.Action{}
	wantStatus(t, do(h, none, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusForbidden, "forbidden")
	wantStatus(t, do(h, none, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusForbidden, "forbidden")

	unrestricted := changeCIToken
	unrestricted.Actions = nil
	wantStatus(t, do(h, unrestricted, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	wantStatus(t, do(h, unrestricted, http.MethodGet, "/api/v1/projects/p1/services", ""), http.StatusOK, "")

	// The list never widens the role: a viewer token listing change:record still cannot record.
	viewerCI := gateViewer
	viewerCI.Actions = []authz.Action{authz.ActionChangeRecord}
	wantStatus(t, do(h, viewerCI, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusForbidden, "forbidden")

	if calls := fs.changeCalls(); len(calls) != 2 {
		t.Fatalf("store record calls = %v, want exactly the two 201s", calls)
	}
}

// §5a on the record (invariant 21): permits first — a held record fills the one permit and the
// next is 429 `process_inflight` with Retry-After 1 and no bucket touched — then the atomic
// bucket debit — the third request against a two-per-minute principal bucket is 429
// `principal_rate` with Retry-After ≥ 1 — and a second principal against a drained process bucket
// is `process_rate`. Every refusal is counted under its reason and reaches no store. Unwired: 501.
func TestChangeRecordLimiterPermitsThenBucket(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	c.hold, c.started, c.release = 1, make(chan struct{}, 1), make(chan struct{})
	m := &changeFakeMetrics{}
	limits := changeLimits()
	limits.RecordInflightProcess = 1
	limits.RecordRatePrincipalPerMinute = 2
	limits.RecordRateProcessPerMinute = 3
	h := newChangeHandler(fs, limits, m)

	done := make(chan int, 1)
	go func() { done <- do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody).Code }()
	<-c.started
	rec := do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody)
	wantStatus(t, rec, http.StatusTooManyRequests, "process_inflight")
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want 1 for an in-flight refusal", ra)
	}
	close(c.release)
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("the held record = %d, want 201", code)
	}
	// One token spent by the held record, one by this: the third is the bucket's refusal.
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	rec = do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody)
	wantStatus(t, rec, http.StatusTooManyRequests, "principal_rate")
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" || ra[0] == '-' {
		t.Fatalf("Retry-After = %q, want ceil ≥ 1", ra)
	}
	// The process bucket held 3: two spent above; the admin's first is the third, the next is process_rate.
	wantStatus(t, do(h, gateAdmin, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	rec = do(h, gateAdmin, http.MethodPost, changeP(gateSvcID), changeBody)
	wantStatus(t, rec, http.StatusTooManyRequests, "process_rate")
	if calls := fs.changeCalls(); len(calls) != 3 {
		t.Fatalf("store calls = %v, want exactly the three admitted records", calls)
	}
	recorded, rejected, _ := m.snapshot()
	if len(recorded) != 3 || strings.Join(rejected, ",") != "process_inflight,principal_rate,process_rate" {
		t.Fatalf("metrics = %v %v", recorded, rejected)
	}

	unwired := api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
	wantStatus(t, do(unwired, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusNotImplemented, "change_not_wired")
	wantStatus(t, do(unwired, gateViewer, http.MethodGet, changeP(gateSvcID)+changeRange(gateT0.Add(-time.Hour), gateT0), ""), http.StatusNotImplemented, "change_not_wired")
}

// ── the timeline ─────────────────────────────────────────────────────────────────────────────

// D6's query contract: the range is explicit and half-open, at most 92 days; `limit` 1..200
// defaulting to 50; `kind` a deduplicated set handed to the store; `source` a slug; `cursor`
// decoded and passed through. Every refusal is one of the closed codes and reaches no store; a
// viewer reads (project:read).
func TestChangeTimelineQueryContract(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})
	from, to := gateT0.Add(-30*24*time.Hour), gateT0
	base := changeP(gateSvcID)

	for q, want := range map[string]string{
		"": "range_required",
		"?from=" + url.QueryEscape(from.Format(time.RFC3339)):                    "range_required",
		"?to=" + url.QueryEscape(to.Format(time.RFC3339)):                        "range_required",
		changeRange(to, from):                                                    "range_invalid",
		changeRange(from, from):                                                  "range_invalid",
		"?from=yesterday&to=" + url.QueryEscape(to.Format(time.RFC3339)):         "range_invalid",
		changeRange(to.Add(-93*24*time.Hour), to):                                "range_too_wide",
		changeRange(from, to) + "&limit=0":                                       "limit_invalid",
		changeRange(from, to) + "&limit=201":                                     "limit_invalid",
		changeRange(from, to) + "&limit=x":                                       "limit_invalid",
		changeRange(from, to) + "&kind=nope":                                     "kind_invalid",
		changeRange(from, to) + "&kind=deploy&kind=config":                       "kind_invalid",
		changeRange(from, to) + "&source=Deploy_Bot":                             "source_invalid",
		changeRange(from, to) + "&source=":                                       "source_invalid",
		changeRange(from, to) + "&cursor=!!!":                                    "cursor_invalid",
		changeRange(from, to) + "&cursor=" + url.QueryEscape("bm90LWEtY3Vyc29y"): "cursor_invalid",
	} {
		wantStatus(t, do(h, gateViewer, http.MethodGet, base+q, ""), http.StatusBadRequest, want)
	}
	if calls := fs.changeCalls(); len(calls) != 0 {
		t.Fatalf("a refused query reached the store: %v", calls)
	}

	// 92 days exactly is accepted, with the defaults: limit 50, no filters, no cursor.
	rec := do(h, gateViewer, http.MethodGet, base+changeRange(to.Add(-92*24*time.Hour), to), "")
	out := wantStatus(t, rec, http.StatusOK, "")
	if string(out) != "{\"items\":[],\"next_cursor\":null}\n" {
		t.Fatalf("empty page = %s, want items [] and next_cursor null", out)
	}
	if c.lastList.limit != 50 || c.lastList.kinds != nil || c.lastList.source != nil || c.lastList.cursor != nil ||
		!c.lastList.from.Equal(to.Add(-92*24*time.Hour)) || !c.lastList.to.Equal(to) {
		t.Fatalf("store got %+v", c.lastList)
	}

	// Filters and the cursor ride through: kind is a SET (deduplicated, order kept), source one slug.
	cur := store.ChangeCursor{LatestOccurredAt: gateT0.Add(-time.Hour), Source: "github-actions", ExternalID: "run-41"}
	q := changeRange(from, to) + "&kind=deploy&kind=flag&kind=deploy&source=github-actions&limit=2&cursor=" + url.QueryEscape(cur.Encode())
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+q, ""), http.StatusOK, "")
	if got := fmt.Sprint(c.lastList.kinds); got != "[deploy flag]" {
		t.Fatalf("kinds = %s, want [deploy flag]", got)
	}
	if c.lastList.source == nil || *c.lastList.source != "github-actions" || c.lastList.limit != 2 {
		t.Fatalf("store got %+v", c.lastList)
	}
	if c.lastList.cursor == nil || *c.lastList.cursor != cur {
		t.Fatalf("cursor = %+v, want %+v", c.lastList.cursor, cur)
	}

	// Tenant order: a malformed id is 400 before the store; a foreign service is the store's 404;
	// an invisible project 404.
	wantStatus(t, do(h, gateViewer, http.MethodGet, changeP("nope")+changeRange(from, to), ""), http.StatusBadRequest, "serviceID must be a UUID")
	wantStatus(t, do(h, gateViewer, http.MethodGet, changeP(gateP2SvcID)+changeRange(from, to), ""), http.StatusNotFound, "not found")
	wantStatus(t, do(h, outsider, http.MethodGet, base+changeRange(from, to), ""), http.StatusNotFound, "not found")
	fs.changeState().listErr = errors.New("boom")
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+changeRange(from, to), ""), http.StatusInternalServerError, "internal error")
}

// D6/D7/D11 presence: a group is {source, external_id, kind, ref, url, latest_occurred_at,
// phases, incidents} always, `decision` ABSENT when none was given, `{decision_id, state,
// action, overridden}` for a live ledger row (action absent for NOT_CONFIGURED), `{decision_id,
// aged_out: true}` once the row is gone; `incidents` is ALWAYS an array; `next_cursor` is the
// store's cursor encoded, null on the last page.
func TestChangeTimelinePresenceContract(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	started := changeRow(changeChgID, domain.ChangePhaseStarted, gateT0.Add(-10*time.Minute))
	started.Ref, started.URL = "", ""
	succeeded := changeRow(changeChgID2, domain.ChangePhaseSucceeded, gateT0.Add(-8*time.Minute))
	succeeded.DecisionID = gatePtr(gateDecID)
	live := changeGroup(started, succeeded)
	live.Decision = &domain.ChangeDecisionLink{ID: gateDecID, State: gatePtr(domain.GateStateBlock), Action: gatePtr(domain.GateActionAllow), Overridden: true}
	live.Incidents = []domain.ChangeIncidentLink{{IncidentID: changeIncID, OpenedAt: gateT0, Role: "own_service", LagSeconds: 480, ChangeID: changeChgID2}}
	aged := changeGroup(changeRow("019906c0-cccc-7abc-8def-0123456789ab", domain.ChangePhaseFailed, gateT0.Add(-time.Hour)))
	aged.ExternalID = "run-41"
	aged.Decision = &domain.ChangeDecisionLink{ID: "019906c0-dddd-7abc-8def-0123456789ab", AgedOut: true}
	notConfigured := changeGroup(changeRow("019906c0-eeee-7abc-8def-0123456789ab", domain.ChangePhaseSucceeded, gateT0.Add(-2*time.Hour)))
	notConfigured.ExternalID = "run-40"
	notConfigured.Decision = &domain.ChangeDecisionLink{ID: "019906c0-ffff-7abc-8def-0123456789ab", State: gatePtr(domain.GateStateNotConfigured)}
	none := changeGroup(changeRow("019906c0-1111-7abc-8def-0123456789ab", domain.ChangePhaseStarted, gateT0.Add(-3*time.Hour)))
	none.ExternalID = "run-39"
	none.Incidents = nil // a careless store: the wire must still say []
	c.groups = []domain.ChangeGroup{live, aged, notConfigured, none}
	c.next = &store.ChangeCursor{LatestOccurredAt: none.LatestOccurredAt, Source: "github-actions", ExternalID: "run-39"}
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})

	out := wantStatus(t, do(h, gateViewer, http.MethodGet, changeP(gateSvcID)+changeRange(gateT0.Add(-24*time.Hour), gateT0), ""), http.StatusOK, "")
	wantKeys(t, out, "items", "next_cursor")
	var page struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(out, &page); err != nil || len(page.Items) != 4 {
		t.Fatalf("page = %s (%v)", out, err)
	}
	if page.NextCursor == nil || *page.NextCursor != c.next.Encode() {
		t.Fatalf("next_cursor = %v, want %q", page.NextCursor, c.next.Encode())
	}

	// The live group: every group key, a live decision, one incident, both phases in order.
	wantKeys(t, page.Items[0], "source", "external_id", "kind", "ref", "url", "latest_occurred_at", "phases", "decision", "incidents")
	g := decodeMap(t, page.Items[0])
	if g["ref"] != "v4.2.1" || g["url"] != "https://ci.example/run/42" || g["kind"] != "deploy" {
		t.Fatalf("group-level ref/url must be the LATEST phase's: %s", page.Items[0])
	}
	dec, _ := json.Marshal(g["decision"])
	wantKeys(t, dec, "decision_id", "state", "action", "overridden")
	if d := g["decision"].(map[string]any); d["decision_id"] != gateDecID || d["state"] != "BLOCK" || d["action"] != "ALLOW" || d["overridden"] != true {
		t.Fatalf("decision = %s", dec)
	}
	phases := g["phases"].([]any)
	if len(phases) != 2 {
		t.Fatalf("phases = %v", phases)
	}
	p0, _ := json.Marshal(phases[0])
	wantKeys(t, p0, "id", "phase", "occurred_at", "ref", "url", "actor_label", "actor_user_id", "via_token", "recorded_at")
	p1, _ := json.Marshal(phases[1])
	wantKeys(t, p1, "id", "phase", "occurred_at", "ref", "url", "decision_id", "actor_label", "actor_user_id", "via_token", "recorded_at")
	if phases[0].(map[string]any)["phase"] != "started" || phases[1].(map[string]any)["phase"] != "succeeded" {
		t.Fatalf("phases out of the store's order: %s", page.Items[0])
	}
	inc, _ := json.Marshal(g["incidents"].([]any)[0])
	wantKeys(t, inc, "incident_id", "opened_at", "role", "lag_seconds", "change_id")
	if strings.Contains(string(out), "caused") {
		t.Fatalf("the API must never say caused: %s", out)
	}

	// Aged out: exactly {decision_id, aged_out: true}.
	aDec, _ := json.Marshal(decodeMap(t, page.Items[1])["decision"])
	wantKeys(t, aDec, "decision_id", "aged_out")
	if a := decodeMap(t, aDec); a["decision_id"] != "019906c0-dddd-7abc-8def-0123456789ab" || a["aged_out"] != true {
		t.Fatalf("aged-out decision = %s", aDec)
	}
	// A live NOT_CONFIGURED decision has no action, as the gate's own response.
	nDec, _ := json.Marshal(decodeMap(t, page.Items[2])["decision"])
	wantKeys(t, nDec, "decision_id", "state", "overridden")
	// None linked: the key is ABSENT, and incidents is [] even when the store said nil.
	wantKeys(t, page.Items[3], "source", "external_id", "kind", "ref", "url", "latest_occurred_at", "phases", "incidents")
	if n := decodeMap(t, page.Items[3]); fmt.Sprint(n["incidents"]) != "[]" || n["ref"] != "v4.2.1" {
		t.Fatalf("group without links = %s", page.Items[3])
	}
}

// Reads take the read permits of §5a and no rate token: twenty timeline reads against a
// two-per-minute record bucket are all 200, a held read fills the one read permit and the next
// read is 429 `process_inflight` — counted nowhere, because the rejected family is the record's.
func TestChangeReadsTakePermitsNotRateTokens(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	m := &changeFakeMetrics{}
	limits := changeLimits()
	limits.RecordRatePrincipalPerMinute, limits.RecordRateProcessPerMinute = 2, 2
	limits.ReadInflightProcess = 1
	h := newChangeHandler(fs, limits, m)
	path := changeP(gateSvcID) + changeRange(gateT0.Add(-time.Hour), gateT0)
	for i := 0; i < 20; i++ {
		wantStatus(t, do(h, gateViewer, http.MethodGet, path, ""), http.StatusOK, "")
	}
	c.hold, c.started, c.release = 1, make(chan struct{}, 1), make(chan struct{})
	done := make(chan int, 1)
	go func() { done <- do(h, gateViewer, http.MethodGet, path, "").Code }()
	<-c.started
	rec := do(h, gateViewer, http.MethodGet, path, "")
	wantStatus(t, rec, http.StatusTooManyRequests, "process_inflight")
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q", ra)
	}
	// The record pool is separate: a record is admitted while the read permit is held.
	wantStatus(t, do(h, gateEditor, http.MethodPost, changeP(gateSvcID), changeBody), http.StatusCreated, "")
	close(c.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the held read = %d, want 200", code)
	}
	_, rejected, _ := m.snapshot()
	if len(rejected) != 0 {
		t.Fatalf("a read refusal moved the record's rejected family: %v", rejected)
	}
}

// ── the comparison ───────────────────────────────────────────────────────────────────────────

// D8's request contract: `source` and `external_id` are required (normalized as a body field —
// a decomposed é finds the composed row), `horizon` defaults to 1h and is one of the four (2h is
// 400 `horizon_invalid`), a started-only group is 404 `no_terminal_phase`, an unknown identity
// or a foreign service 404, and the horizon is echoed in the request's spelling.
func TestChangeCompareRequestContract(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	c := fs.changeState()
	terminal := changeRow(changeChgID, domain.ChangePhaseSucceeded, gateT0.Add(-2*time.Hour))
	c.comparison = domain.ChangeComparison{Source: "github-actions", ExternalID: "run-42", Change: terminal, T: terminal.OccurredAt,
		Before: domain.ChangeCompareSide{Figure: changeFigure(99.9)}, After: domain.ChangeCompareSide{Figure: changeFigure(99.5)}, AsOf: gateT0}
	m := &changeFakeMetrics{}
	h := newChangeHandler(fs, changeLimits(), m)
	base := changeP(gateSvcID) + "/compare"

	for q, want := range map[string]string{
		"":                                      "source_invalid",
		"?source=github-actions":                "external_id_invalid",
		"?source=Deploy_Bot&external_id=run-42": "source_invalid",
		"?source=github-actions&external_id=run-42&horizon=2h":     "horizon_invalid",
		"?source=github-actions&external_id=run-42&horizon=7d":     "horizon_invalid",
		"?source=github-actions&external_id=run-42&horizon=1h0m0s": "horizon_invalid",
		"?source=github-actions&external_id=run-42&horizon=":       "horizon_invalid",
	} {
		wantStatus(t, do(h, gateViewer, http.MethodGet, base+q, ""), http.StatusBadRequest, want)
	}
	if calls := fs.changeCalls(); len(calls) != 0 {
		t.Fatalf("a refused query reached the store: %v", calls)
	}
	for _, tc := range []struct {
		q, name string
		d       time.Duration
	}{
		{"", "1h", time.Hour}, {"&horizon=15m", "15m", 15 * time.Minute}, {"&horizon=1h", "1h", time.Hour},
		{"&horizon=6h", "6h", 6 * time.Hour}, {"&horizon=24h", "24h", 24 * time.Hour},
	} {
		out := wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?source=github-actions&external_id="+url.QueryEscape("café-42 ")+tc.q, ""), http.StatusOK, "")
		if c.lastCompare.horizon != tc.d || c.lastCompare.source != "github-actions" || c.lastCompare.externalID != "café-42" {
			t.Fatalf("store got %+v for %q", c.lastCompare, tc.q)
		}
		if got := decodeMap(t, out)["horizon"]; got != tc.name {
			t.Fatalf("horizon echoed as %v, want %q", got, tc.name)
		}
	}
	c.compareErr = domain.NewChangeError(domain.ChangeErrNoTerminalPhase, "phase", "only started is recorded")
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?source=github-actions&external_id=run-42", ""), http.StatusNotFound, "no_terminal_phase")
	c.compareErr = store.ErrNotFound
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?source=github-actions&external_id=run-42", ""), http.StatusNotFound, "not found")
	c.compareErr = domain.NewChangeError(domain.ChangeErrHorizonInvalid, "horizon", "planted")
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?source=github-actions&external_id=run-42", ""), http.StatusBadRequest, "horizon_invalid")
	c.compareErr = nil
	wantStatus(t, do(h, gateViewer, http.MethodGet, changeP(gateP2SvcID)+"/compare?source=github-actions&external_id=run-42", ""), http.StatusNotFound, "not found")
	wantStatus(t, do(h, outsider, http.MethodGet, base+"?source=github-actions&external_id=run-42", ""), http.StatusNotFound, "not found")
	_, _, compared := m.snapshot()
	if len(compared) != 5 {
		t.Fatalf("compare metric = %v, want one per served comparison (five), none for a refusal", compared)
	}
}

// D8/D-0211 side shapes, serialized exactly: a FIGURE is {from, to, availability, good_seconds,
// bad_seconds, unknown_seconds, excluded_seconds, buckets}; WITHHELD is {from, to, withheld,
// detail?}; PENDING — on EITHER side — is {from, to, pending, sealed_through}. `delta` is present
// only with two figures; `sealed_through` is present at the top, null when nothing is sealed;
// the compare metric's outcome is pending > withheld > figure.
func TestChangeCompareSideShapes(t *testing.T) {
	terminal := changeRow(changeChgID, domain.ChangePhaseFailed, gateT0.Add(-2*time.Hour))
	tAt := terminal.OccurredAt
	sealed := gateT0.Add(-30 * time.Minute)
	side := func(from, to time.Time) domain.ChangeCompareSide { return domain.ChangeCompareSide{From: from, To: to} }
	withFigure := func(s domain.ChangeCompareSide, avail float64) domain.ChangeCompareSide {
		s.Figure = changeFigure(avail)
		return s
	}
	withheld := func(s domain.ChangeCompareSide, reason, detail string) domain.ChangeCompareSide {
		s.Withheld = &domain.ChangeCompareWithheld{Reason: reason, Detail: detail, SealedThrough: &sealed}
		return s
	}
	cases := []struct {
		name       string
		cmp        domain.ChangeComparison
		beforeKeys []string
		afterKeys  []string
		delta      bool
		outcome    string
	}{
		{
			name: "two figures",
			cmp: domain.ChangeComparison{Before: withFigure(side(tAt.Add(-time.Hour), tAt), 99.9), After: withFigure(side(tAt, tAt.Add(time.Hour)), 99.5),
				Delta: gatePtr(-0.4), SealedThrough: &sealed},
			beforeKeys: []string{"from", "to", "availability", "good_seconds", "bad_seconds", "unknown_seconds", "excluded_seconds", "buckets"},
			afterKeys:  []string{"from", "to", "availability", "good_seconds", "bad_seconds", "unknown_seconds", "excluded_seconds", "buckets"},
			delta:      true, outcome: "figure",
		},
		{
			name: "after pending",
			cmp: domain.ChangeComparison{Before: withFigure(side(tAt.Add(-time.Hour), tAt), 99.9),
				After: withheld(side(tAt, tAt.Add(time.Hour)), domain.ChangeCompareWithheldPending, ""), SealedThrough: &sealed},
			beforeKeys: []string{"from", "to", "availability", "good_seconds", "bad_seconds", "unknown_seconds", "excluded_seconds", "buckets"},
			afterKeys:  []string{"from", "to", "pending", "sealed_through"},
			outcome:    "pending",
		},
		{
			name: "before pending too (D-0211)",
			cmp: domain.ChangeComparison{Before: withheld(side(tAt.Add(-time.Hour), tAt), domain.ChangeCompareWithheldPending, ""),
				After: withheld(side(tAt, tAt.Add(time.Hour)), domain.ChangeCompareWithheldPending, ""), SealedThrough: &sealed},
			beforeKeys: []string{"from", "to", "pending", "sealed_through"},
			afterKeys:  []string{"from", "to", "pending", "sealed_through"},
			outcome:    "pending",
		},
		{
			name: "before undecidable with the page's reason",
			cmp: domain.ChangeComparison{Before: withheld(side(tAt.Add(-time.Hour), tAt), domain.ChangeCompareWithheldUndecidable, "storage_gap"),
				After: withFigure(side(tAt, tAt.Add(time.Hour)), 99.5), SealedThrough: &sealed},
			beforeKeys: []string{"from", "to", "withheld", "detail"},
			afterKeys:  []string{"from", "to", "availability", "good_seconds", "bad_seconds", "unknown_seconds", "excluded_seconds", "buckets"},
			outcome:    "withheld",
		},
		{
			name: "definition changed and no facts, nothing sealed",
			cmp: domain.ChangeComparison{Before: withheld(side(tAt.Add(-time.Hour), tAt), domain.ChangeCompareWithheldDefinitionChanged, ""),
				After: withheld(side(tAt, tAt.Add(time.Hour)), domain.ChangeCompareWithheldNoFacts, "")},
			beforeKeys: []string{"from", "to", "withheld"},
			afterKeys:  []string{"from", "to", "withheld"},
			outcome:    "withheld",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := seededStore()
			seedGateService(fs, "p1", gateSvcID)
			c := fs.changeState()
			cmp := tc.cmp
			cmp.Source, cmp.ExternalID, cmp.Change, cmp.T, cmp.Horizon, cmp.AsOf = "github-actions", "run-42", terminal, tAt, "1h0m0s", gateT0
			c.comparison = cmp
			m := &changeFakeMetrics{}
			h := newChangeHandler(fs, changeLimits(), m)
			out := wantStatus(t, do(h, gateViewer, http.MethodGet, changeP(gateSvcID)+"/compare?source=github-actions&external_id=run-42", ""), http.StatusOK, "")
			top := []string{"source", "external_id", "kind", "ref", "change_id", "terminal_phase", "t", "horizon", "sealed_through", "as_of", "before", "after"}
			if tc.delta {
				top = append(top, "delta")
			}
			wantKeys(t, out, top...)
			got := decodeMap(t, out)
			if got["terminal_phase"] != "failed" || got["change_id"] != changeChgID || got["kind"] != "deploy" || got["ref"] != "v4.2.1" || got["horizon"] != "1h" {
				t.Fatalf("top-level = %s", out)
			}
			if (got["sealed_through"] == nil) != (tc.cmp.SealedThrough == nil) {
				t.Fatalf("sealed_through presence = %v, want null iff nothing sealed: %s", got["sealed_through"], out)
			}
			before, _ := json.Marshal(got["before"])
			after, _ := json.Marshal(got["after"])
			wantKeys(t, before, tc.beforeKeys...)
			wantKeys(t, after, tc.afterKeys...)
			if b := decodeMap(t, before); b["pending"] == true && b["sealed_through"] == nil {
				t.Fatalf("a pending side must state sealed_through: %s", before)
			}
			if a := decodeMap(t, after); a["withheld"] == "undecidable" && a["detail"] == nil {
				t.Fatalf("undecidable must carry the page's reason: %s", after)
			}
			if strings.Contains(string(out), "caused") || strings.Contains(string(out), "durations") {
				t.Fatalf("the comparison states seconds and never causation: %s", out)
			}
			_, _, compared := m.snapshot()
			if len(compared) != 1 || compared[0] != tc.outcome {
				t.Fatalf("compare metric = %v, want [%s]", compared, tc.outcome)
			}
		})
	}
}

// ── the incident side ────────────────────────────────────────────────────────────────────────

// D7/invariant 9 from the incident side: the link rows with the anchored change (identity +
// phase), the copied instant and lag, the group's live phases, under `items` — ALWAYS an array.
// An incident of another project is 404, a malformed id 400 before the store, a viewer reads.
func TestIncidentChangesRoute(t *testing.T) {
	fs := seededStore()
	fs.incidents[changeIncID] = domain.Incident{ID: changeIncID, ProjectID: "p1", Title: "checkout degraded", Status: domain.IncidentInvestigating, ServiceID: gateSvcID}
	fs.incidents[changeP2IncID] = domain.Incident{ID: changeP2IncID, ProjectID: "p2", Title: "other", Status: domain.IncidentInvestigating}
	c := fs.changeState()
	started := changeRow(changeChgID, domain.ChangePhaseStarted, gateT0.Add(-10*time.Minute))
	succeeded := changeRow(changeChgID2, domain.ChangePhaseSucceeded, gateT0.Add(5*time.Minute)) // recorded after the open
	c.incidentLinks = map[string][]domain.IncidentChangeLink{changeIncID: {{
		Change: started, Role: "own_service", OccurredAt: started.OccurredAt, LagSeconds: 600, ComputedAt: gateT0.Add(time.Second),
		Phases: []domain.ChangePhaseRow{started, succeeded},
	}}}
	h := newChangeHandler(fs, changeLimits(), &changeFakeMetrics{})

	out := wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeIncID+"/changes", ""), http.StatusOK, "")
	wantKeys(t, out, "items")
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Items) != 1 {
		t.Fatalf("items = %s", out)
	}
	wantKeys(t, resp.Items[0], "change", "role", "occurred_at", "lag_seconds", "computed_at", "phases")
	item := decodeMap(t, resp.Items[0])
	ch, _ := json.Marshal(item["change"])
	wantKeys(t, ch, "service_id", "source", "external_id", "kind", "id", "phase", "occurred_at", "ref", "url", "actor_label", "actor_user_id", "via_token", "recorded_at")
	if item["role"] != "own_service" || item["lag_seconds"] != float64(600) || item["change"].(map[string]any)["id"] != changeChgID {
		t.Fatalf("link = %s", resp.Items[0])
	}
	if phases := item["phases"].([]any); len(phases) != 2 || phases[1].(map[string]any)["phase"] != "succeeded" {
		t.Fatalf("live phases = %v", item["phases"])
	}
	if strings.Contains(string(out), "caused") {
		t.Fatalf("never caused: %s", out)
	}

	// An incident with no links is an empty array, never null.
	delete(c.incidentLinks, changeIncID)
	out = wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeIncID+"/changes", ""), http.StatusOK, "")
	if string(out) != "{\"items\":[]}\n" {
		t.Fatalf("empty = %s", out)
	}
	wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeP2IncID+"/changes", ""), http.StatusNotFound, "not found")
	wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/inc1/changes", ""), http.StatusBadRequest, "incidentID must be a UUID")
	wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeChgID+"/changes", ""), http.StatusNotFound, "not found")
	wantStatus(t, do(h, outsider, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeIncID+"/changes", ""), http.StatusNotFound, "not found")
	if calls := fs.changeCalls(); len(calls) != 4 {
		t.Fatalf("store calls = %v, want the two 200s and the two well-formed 404s", calls)
	}
	c.incidentErr = context.Canceled
	wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/incidents/"+changeIncID+"/changes", ""), http.StatusInternalServerError, "internal error")
}
