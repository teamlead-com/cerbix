package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-024 (func-reliability-gate.md D7, D9, D12, D13a, D15, §5, §5a; iter-0163 changeset 4): the
// HTTP surface of the reliability gate against the scripted fake store. What these pin is the
// CONTRACT a pipeline and the SPA read — the status matrix of D13a route by route, strict body
// decoding that names the offending field (a duplicate `clauses` key included, which
// encoding/json would otherwise swallow), the tenant order of D15 (a malformed id is 400 before
// the store is asked), the §5a bounds refusing BEFORE any store call with the exact Retry-After,
// the D7 presence of the decision JSON as raw keys, the sentinel-to-status mapping, the D12
// actions on real principals, and that every metric moves exactly when the spec says.

const (
	gateSvcID   = "0b6c1c2e-4f3a-4d5e-8a9b-1c2d3e4f5a6b"
	gateP2SvcID = "1b6c1c2e-4f3a-4d5e-8a9b-1c2d3e4f5a6c"
	gateOvID    = "2c7d2d3f-5a4b-4e6f-9bac-2d3e4f5a6b7c"
	gateOvID2   = "3c7d2d3f-5a4b-4e6f-9bac-2d3e4f5a6b7d"
	gateDecID   = "019906c0-1234-7abc-8def-0123456789ab"

	gatePolicyBody   = `{"expected_revision":null,"schema_version":1,"window":"30d","clauses":{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"},"budget_consumed_percent":90,"max_seal_lag_seconds":900,"unknown_behavior":"warn"}`
	gateOverrideBody = `{"policy_revision":3,"reason":"hotfix for INC-42","expires_at":"2026-08-30T10:00:00Z"}`
)

var (
	gateViewer = authz.Principal{UserID: "gate-viewer", AuditLabel: "viewer@acme", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleViewer}}}
	gateEditor = authz.Principal{UserID: "gate-editor", AuditLabel: "editor@acme", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	gateAdmin  = authz.Principal{UserID: "gate-admin", AuditLabel: "admin@acme", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
	// gateToken is a cerbix API token principal as the auth layer builds it: a synthetic identity
	// (mapped to a NULL actor by AuditUserID), via_token, and the `token:<name>` label.
	gateToken = authz.Principal{UserID: authz.SyntheticTokenActorPrefix + "tok-1", ViaToken: true, AuditLabel: "token:ci",
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}
)

// gateFakeMetrics records every gate recorder call so a test can assert exactly what moved.
type gateFakeMetrics struct {
	mu        sync.Mutex
	decisions []string // state/action/overridden
	rejected  []string
	errors    []string
	durations int
}

func (m *gateFakeMetrics) RecordGateDecision(state, action string, overridden bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, fmt.Sprintf("%s/%s/%t", state, action, overridden))
	return nil
}
func (m *gateFakeMetrics) RecordGateEvaluateRejected(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejected = append(m.rejected, reason)
	return nil
}
func (m *gateFakeMetrics) RecordGateEvaluateError(kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, kind)
	return nil
}
func (m *gateFakeMetrics) ObserveGateDecisionDuration(time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations++
}
func (m *gateFakeMetrics) snapshot() (decisions, rejected, errs []string, durations int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.decisions...), append([]string(nil), m.rejected...), append([]string(nil), m.errors...), m.durations
}

func gateLimits() api.GateLimits {
	return api.GateLimits{InflightProcess: 8, InflightPrincipal: 2, RatePrincipalPerMinute: 10, RateProcessPerMinute: 60, TxBudget: 5 * time.Second}
}

func newGateHandler(fs *fakeStore, limits api.GateLimits, m api.GateMetrics) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithGate(limits, m).Router()
}

// seedGateService plants a uuid-shaped service in a project.
func seedGateService(fs *fakeStore, projectID, id string) {
	fs.serviceStore()[id] = &fakeService{svc: domain.Service{ID: id, ProjectID: projectID, Slug: "checkout", Name: "Checkout"}}
}

func gateP(svc string) string { return "/api/v1/projects/p1/services/" + svc + "/gate" }

func gatePtr[T any](v T) *T { return &v }

var gateT0 = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

func gateDecisionFixture(state domain.GateState, action *domain.GateAction) domain.GateDecision {
	return domain.GateDecision{
		SchemaVersion: domain.GateDecisionSchemaV1, DecisionID: gateDecID, EvaluatedAt: gateT0,
		ServiceID: gatePtr(gateSvcID), ServiceSlug: "checkout", ServiceName: "Checkout",
		State: state, Action: action, Reasons: []domain.GateReasonEntry{},
	}
}

func gateOverrideFixture(id string, status domain.GateOverrideStatus) store.GateOverrideRecord {
	return store.GateOverrideRecord{GateOverride: domain.GateOverride{
		ID: id, ServiceID: gateSvcID, ProjectID: "p1", PolicyRevision: 3,
		ActorUserID: gatePtr("gate-admin"), ActorLabel: "admin@acme",
		Reason: "hotfix", CreatedAt: gateT0, ExpiresAt: gateT0.Add(2 * time.Hour),
	}, Status: status}
}

func jsonKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func wantKeys(t *testing.T, body []byte, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := jsonKeys(t, body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v\n%s", got, want, body)
	}
}

func errorOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return e.Error
}

func wantStatus(t *testing.T, rec interface {
	Result() *http.Response
}, code int, errText string) []byte {
	t.Helper()
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != code {
		t.Fatalf("status = %d, want %d (%s)", res.StatusCode, code, body)
	}
	if errText != "" {
		if got := errorOf(t, body); got != errText && !strings.Contains(got, errText) {
			t.Fatalf("error = %q, want %q", got, errText)
		}
	}
	return body
}

// ── the decision ─────────────────────────────────────────────────────────────────────────────

func TestGateDecisionHappyPath(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	fs.gateState().decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	m := &gateFakeMetrics{}
	h := newGateHandler(fs, gateLimits(), m)

	for _, body := range []string{"{}", "", "  \n"} {
		rec := do(h, gateViewer, http.MethodPost, gateP(gateSvcID), body)
		out := wantStatus(t, rec, http.StatusOK, "")
		var dec struct {
			State, Action, DecisionID string `json:"-"`
		}
		_ = dec
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		if got["state"] != "ALLOW" || got["action"] != "ALLOW" || got["decision_id"] != gateDecID || got["schema_version"] != float64(1) {
			t.Fatalf("decision = %s", out)
		}
	}
	if calls := fs.gateCalls(); len(calls) != 3 || calls[0] != "DecideGate" {
		t.Fatalf("store calls = %v", calls)
	}
	g := fs.gateState()
	if len(g.budgets) != 3 || g.budgets[0] != 5*time.Second {
		t.Fatalf("budget handed to the store = %v, want 5s (gate.evaluate_tx_budget_ms)", g.budgets)
	}
	decisions, rejected, errs, durations := m.snapshot()
	if len(decisions) != 3 || decisions[0] != "ALLOW/ALLOW/false" || len(rejected) != 0 || len(errs) != 0 || durations != 3 {
		t.Fatalf("metrics = %v %v %v %d", decisions, rejected, errs, durations)
	}
}

// The decision body is empty or `{}` and nothing else: an override or an actor supplied here is
// refused as an unknown field naming it (D9, invariant 8) and the store is never asked.
func TestGateDecisionBodyIsStrict(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	for body, want := range map[string]string{
		`{"override":"` + gateOvID + `"}`: "override",
		`{"actor":"me"}`:                  "actor",
		`{"action":"ALLOW"}`:              "action",
		`[]`:                              "empty JSON object",
		`null`:                            "empty JSON object", // review [41]: encoding/json accepts null into a struct
		`1`:                               "empty JSON object",
		`"x"`:                             "empty JSON object",
		`{}{}`:                            "single JSON value",
		`nope`:                            "empty JSON object",
	} {
		rec := do(h, gateViewer, http.MethodPost, gateP(gateSvcID), body)
		wantStatus(t, rec, http.StatusBadRequest, want)
	}
	if calls := fs.gateCalls(); len(calls) != 0 {
		t.Fatalf("a refused body still reached the store: %v", calls)
	}
}

// Every store sentinel maps to exactly one answer and one error kind (D6a, D10, §5a); a
// foreign or unknown service is the tenant 404 and NO evaluation error.
func TestGateDecisionErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		text string
		kind string
	}{
		{store.ErrNotFound, http.StatusNotFound, "not found", ""},
		{store.ErrGateSnapshotConflict, http.StatusServiceUnavailable, "snapshot_conflict", "snapshot_conflict"},
		{store.ErrGateLedgerUnwritable, http.StatusServiceUnavailable, "ledger_unwritable", "ledger_unwritable"},
		{store.ErrGateBudgetExceeded, http.StatusServiceUnavailable, "timeout", "timeout"},
		{fmt.Errorf("wrapped: %w", store.ErrGateBudgetExceeded), http.StatusServiceUnavailable, "timeout", "timeout"},
		{context.DeadlineExceeded, http.StatusServiceUnavailable, "timeout", "timeout"},
		{errors.New("boom"), http.StatusInternalServerError, "internal error", "error"},
	}
	for _, tc := range cases {
		fs := seededStore()
		seedGateService(fs, "p1", gateSvcID)
		fs.gateState().decideErr = tc.err
		m := &gateFakeMetrics{}
		h := newGateHandler(fs, gateLimits(), m)
		rec := do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}")
		wantStatus(t, rec, tc.code, tc.text)
		decisions, _, errs, durations := m.snapshot()
		if len(decisions) != 0 {
			t.Errorf("%v: a failed evaluation counted a decision", tc.err)
		}
		if durations != 1 {
			t.Errorf("%v: duration observed %d times, want 1 (every admitted evaluation)", tc.err, durations)
		}
		if tc.kind == "" && len(errs) != 0 {
			t.Errorf("%v: error kinds %v, want none", tc.err, errs)
		}
		if tc.kind != "" && (len(errs) != 1 || errs[0] != tc.kind) {
			t.Errorf("%v: error kinds %v, want [%s]", tc.err, errs, tc.kind)
		}
	}
}

// NOT_CONFIGURED is a 200 with that state, its reason and NO action key at all (D4, D7,
// invariant 2) — asserted on the raw JSON, and counted as action="none".
func TestGateDecisionNotConfiguredHasNoActionKey(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	dec := gateDecisionFixture(domain.GateStateNotConfigured, nil)
	dec.Reasons = []domain.GateReasonEntry{{Code: string(domain.GateReasonNotConfigured), Docs: domain.GateDocsURL}}
	fs.gateState().decision = dec
	m := &gateFakeMetrics{}
	h := newGateHandler(fs, gateLimits(), m)
	body := wantStatus(t, do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")
	wantKeys(t, body, "schema_version", "decision_id", "evaluated_at", "service_id", "service_slug", "service_name", "state", "reasons")
	if !strings.Contains(string(body), `"state":"NOT_CONFIGURED"`) || !strings.Contains(string(body), `"code":"not_configured"`) {
		t.Fatalf("body = %s", body)
	}
	decisions, _, _, _ := m.snapshot()
	if len(decisions) != 1 || decisions[0] != "NOT_CONFIGURED//false" {
		t.Fatalf("metric = %v, want NOT_CONFIGURED with no action", decisions)
	}
}

// An overridden BLOCK keeps state=BLOCK and reasons, carries action=ALLOW,
// unoverridden_action=BLOCK and the override's id, and is counted overridden="true" (D9).
func TestGateDecisionOverriddenBlock(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	dec := gateDecisionFixture(domain.GateStateBlock, gatePtr(domain.GateActionAllow))
	dec.Reasons = []domain.GateReasonEntry{{Code: "matched", Clause: domain.ClauseBudgetExhausted, Assignment: domain.ClauseAssignBlock, Value: 100.0, Source: "report"}}
	dec.PolicyRevision, dec.Window, dec.OverrideID = gatePtr(int64(3)), gatePtr("30d"), gatePtr(gateOvID)
	dec.UnoverriddenAction = gatePtr(domain.GateActionBlock)
	dec.Override = &domain.GateOverrideApplied{ID: gateOvID, ActorLabel: "token:ci", Reason: "hotfix", ExpiresAt: gateT0.Add(time.Hour)}
	fs.gateState().decision = dec
	m := &gateFakeMetrics{}
	h := newGateHandler(fs, gateLimits(), m)
	body := wantStatus(t, do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")

	var got map[string]json.RawMessage
	_ = json.Unmarshal(body, &got)
	for k, want := range map[string]string{
		"state": `"BLOCK"`, "action": `"ALLOW"`, "unoverridden_action": `"BLOCK"`, "override_id": `"` + gateOvID + `"`, "policy_revision": "3",
	} {
		if string(got[k]) != want {
			t.Errorf("%s = %s, want %s", k, got[k], want)
		}
	}
	var ov struct {
		ID, ActorLabel string
	}
	_ = json.Unmarshal(got["override"], &struct {
		ID         *string `json:"id"`
		ActorLabel *string `json:"actor_label"`
	}{&ov.ID, &ov.ActorLabel})
	if ov.ID != gateOvID || ov.ActorLabel != "token:ci" {
		t.Fatalf("override = %s", got["override"])
	}
	decisions, _, _, _ := m.snapshot()
	if len(decisions) != 1 || decisions[0] != "BLOCK/ALLOW/true" {
		t.Fatalf("metric = %v", decisions)
	}
}

// cliGateDecision mirrors, tag for tag, the decode types of internal/cli/gate.go (gateDecision,
// gateReason, gateOverride), which are unexported. The handler's bytes must decode into it with
// every field the CLI reads populated — the field names are the contract between the two.
type cliGateDecision struct {
	SchemaVersion int    `json:"schema_version"`
	DecisionID    string `json:"decision_id"`
	EvaluatedAt   string `json:"evaluated_at"`
	State         string `json:"state"`
	Action        string `json:"action,omitempty"`
	Reasons       []struct {
		Code       string          `json:"code"`
		Clause     string          `json:"clause,omitempty"`
		Assignment string          `json:"assignment,omitempty"`
		Value      json.RawMessage `json:"value,omitempty"`
		Details    string          `json:"details,omitempty"`
		Docs       string          `json:"docs,omitempty"`
	} `json:"reasons"`
	Override *struct {
		ID         string `json:"id"`
		ActorLabel string `json:"actor_label"`
	} `json:"override,omitempty"`
	UnoverriddenAction string `json:"unoverridden_action,omitempty"`
}

func TestGateDecisionIsReadableByTheCLI(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	dec := gateDecisionFixture(domain.GateStateBlock, gatePtr(domain.GateActionAllow))
	dec.Reasons = []domain.GateReasonEntry{{Code: "matched", Clause: domain.ClauseBudgetConsumed, Assignment: domain.ClauseAssignWarn, Value: 93, Source: "report"}}
	dec.OverrideID, dec.UnoverriddenAction = gatePtr(gateOvID), gatePtr(domain.GateActionBlock)
	dec.Override = &domain.GateOverrideApplied{ID: gateOvID, ActorLabel: "token:ci", Reason: "r", ExpiresAt: gateT0}
	fs.gateState().decision = dec
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	body := wantStatus(t, do(h, gateToken, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")
	var cli cliGateDecision
	if err := json.Unmarshal(body, &cli); err != nil {
		t.Fatalf("the CLI's decode fails on the handler's bytes: %v", err)
	}
	if cli.SchemaVersion != 1 || cli.DecisionID != gateDecID || cli.EvaluatedAt != "2026-08-29T10:00:00Z" ||
		cli.State != "BLOCK" || cli.Action != "ALLOW" || cli.UnoverriddenAction != "BLOCK" ||
		cli.Override == nil || cli.Override.ID != gateOvID || cli.Override.ActorLabel != "token:ci" ||
		len(cli.Reasons) != 1 || cli.Reasons[0].Code != "matched" || cli.Reasons[0].Clause != "budget_consumed" ||
		cli.Reasons[0].Assignment != "warn" || string(cli.Reasons[0].Value) != "93" {
		t.Fatalf("CLI view = %+v from %s", cli, body)
	}
}

// Without WithGate the routes answer 501 rather than run unbounded.
func TestGateRoutesRequireWiring(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newHandler(fs)
	wantStatus(t, do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusNotImplemented, "gate_not_wired")
	if len(fs.gateCalls()) != 0 {
		t.Fatal("an unwired gate reached the store")
	}
}

// ── tenant contract (D15, invariant 10) ──────────────────────────────────────────────────────

// gateRoutes is every gate route with a body that would be valid, so a refusal is the id's.
func gateRoutes(svc, ov string) []struct{ method, path, body string } {
	base := "/api/v1/projects/p1/services/" + svc + "/gate"
	return []struct{ method, path, body string }{
		{http.MethodPost, base, "{}"},
		{http.MethodGet, base + "/policy", ""},
		{http.MethodPut, base + "/policy", gatePolicyBody},
		{http.MethodDelete, base + "/policy?expected_revision=1", ""},
		{http.MethodGet, base + "/override", ""},
		{http.MethodPost, base + "/override", gateOverrideBody},
		{http.MethodGet, base + "/overrides", ""},
		{http.MethodGet, base + "/overrides/" + ov, ""},
		{http.MethodDelete, base + "/overrides/" + ov, ""},
	}
}

func TestGateRoutesRefuseMalformedIDsBeforeTheStore(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	for _, rt := range gateRoutes("not-a-uuid", gateOvID) {
		wantStatus(t, do(h, o1Admin, rt.method, rt.path, rt.body), http.StatusBadRequest, "serviceID must be a UUID")
	}
	for _, rt := range gateRoutes(gateSvcID, "not-a-uuid")[7:] {
		wantStatus(t, do(h, o1Admin, rt.method, rt.path, rt.body), http.StatusBadRequest, "overrideID must be a UUID")
	}
	wantStatus(t, do(h, o1Admin, http.MethodGet, "/api/v1/projects/p1/gate/decisions/not-a-uuid", ""), http.StatusBadRequest, "decisionID must be a UUID")
	if calls := fs.gateCalls(); len(calls) != 0 {
		t.Fatalf("a malformed id reached the store: %v", calls)
	}
}

func TestGateRoutesAnswer404ForForeignOrUnknownService(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p2", gateP2SvcID) // exists, but in a sibling project
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	for _, svc := range []string{gateP2SvcID, gateSvcID} {
		for _, rt := range gateRoutes(svc, gateOvID) {
			rec := do(h, o1Admin, rt.method, rt.path, rt.body)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404 (%s)", rt.method, rt.path, rec.Code, rec.Body)
			}
		}
	}
}

// ── the policy (D13a, D14) ───────────────────────────────────────────────────────────────────

func TestGatePolicyGet(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/policy", ""), http.StatusNotFound, "not_configured")

	fs.gateState().policy = &domain.GatePolicy{ServiceID: gateSvcID, ProjectID: "p1", Window: "30d", SchemaVersion: 1,
		Clauses:               map[domain.GateClause]domain.ClauseAssignment{domain.ClauseBudgetExhausted: domain.ClauseAssignBlock},
		BudgetConsumedPercent: 90, MaxSealLagSeconds: 900, UnknownBehavior: domain.GateUnknownWarn, Revision: 4, UpdatedAt: gateT0, UpdatedBy: "editor@acme"}
	body := wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/policy", ""), http.StatusOK, "")
	wantKeys(t, body, "schema_version", "window", "clauses", "budget_consumed_percent", "max_seal_lag_seconds", "unknown_behavior", "revision", "updated_at", "updated_by")
	if !strings.Contains(string(body), `"revision":4`) || !strings.Contains(string(body), `"updated_by":"editor@acme"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestGatePolicyPut(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	g := fs.gateState()
	g.putRevision = 1
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})

	body := wantStatus(t, do(h, gateEditor, http.MethodPut, gateP(gateSvcID)+"/policy", gatePolicyBody), http.StatusOK, "")
	if string(body) != "{\"revision\":1}\n" {
		t.Fatalf("body = %q", body)
	}
	if g.lastExpected != nil {
		t.Fatalf("expected_revision null must reach the store as nil, got %d", *g.lastExpected)
	}
	doc := g.lastDoc
	if doc.SchemaVersion != 1 || doc.Window != "30d" || doc.BudgetConsumedPercent != 90 || doc.MaxSealLagSeconds != 900 ||
		doc.UnknownBehavior != domain.GateUnknownWarn || len(doc.Clauses) != 5 || doc.Clauses[0] != (domain.GateClauseEntry{Clause: domain.ClauseBudgetExhausted, Assignment: domain.ClauseAssignBlock}) {
		t.Fatalf("document handed to the store = %+v", doc)
	}
	if a := g.actors[0]; a != (store.GateActor{ActorUserID: "gate-editor", ViaToken: false, Label: "editor@acme"}) {
		t.Fatalf("actor = %+v", a)
	}

	// A concrete expected_revision is passed through as is; a stale one is 409.
	withRev := strings.Replace(gatePolicyBody, `"expected_revision":null`, `"expected_revision":3`, 1)
	g.putRevision = 4
	wantStatus(t, do(h, gateEditor, http.MethodPut, gateP(gateSvcID)+"/policy", withRev), http.StatusOK, "")
	if g.lastExpected == nil || *g.lastExpected != 3 {
		t.Fatalf("expected_revision = %v, want 3", g.lastExpected)
	}
	g.putErr = store.ErrGateRevisionConflict
	wantStatus(t, do(h, gateEditor, http.MethodPut, gateP(gateSvcID)+"/policy", withRev), http.StatusConflict, "revision_conflict")
}

// The PUT body is the whole D11/D14 document, decoded strictly: an unknown field, a missing
// field, a wrong type, a missing/unknown/duplicate clause, and every range refusal names the
// field. Shape refusals never reach the store; the domain refusals reach it and come back named.
func TestGatePolicyPutIsStrict(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	replace := func(old, new string) string { return strings.Replace(gatePolicyBody, old, new, 1) }
	cases := []struct {
		name, body, want string
		reachesStore     bool
	}{
		{"unknown field", replace(`{"expected_revision":null`, `{"extra":1,"expected_revision":null`), "extra: unknown field", false},
		{"missing budget_consumed_percent", replace(`"budget_consumed_percent":90,`, ``), "budget_consumed_percent: is required", false},
		{"missing expected_revision", replace(`"expected_revision":null,`, ``), "expected_revision: is required", false},
		{"expected_revision string", replace(`"expected_revision":null`, `"expected_revision":"3"`), "expected_revision: must be a non-negative integer or null", false},
		{"expected_revision negative", replace(`"expected_revision":null`, `"expected_revision":-1`), "expected_revision: must be a non-negative integer or null", false},
		{"expected_revision fraction", replace(`"expected_revision":null`, `"expected_revision":1.5`), "expected_revision: must be a non-negative integer or null", false},
		{"schema_version wrong type", replace(`"schema_version":1`, `"schema_version":"1"`), "schema_version: must be an integer, got string", false},
		{"unknown_behavior missing", replace(`,"unknown_behavior":"warn"`, ``), "unknown_behavior: is required", false},
		{"clauses missing", replace(`"clauses":{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"},`, ``), "clauses: is required", false},
		{"clauses not an object", replace(`"clauses":{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"}`, `"clauses":["budget_exhausted"]`), "clauses: must be an object", false},
		{"clause value not a string", replace(`"budget_exhausted":"block"`, `"budget_exhausted":1`), "clauses.budget_exhausted: must be a string", false},
		{"trailing value", gatePolicyBody + `{}`, "single JSON value", false},
		{"missing clause", replace(`,"service_incident_open":"warn"`, ``), "clauses.service_incident_open", true},
		{"unknown clause", replace(`"service_incident_open":"warn"`, `"service_incident_open":"warn","foo":"block"`), "clauses.foo", true},
		{"duplicate clause", replace(`"budget_consumed":"warn"`, `"budget_consumed":"warn","budget_consumed":"block"`), "clauses.budget_consumed: clause \"budget_consumed\" is assigned more than once", true},
		{"bad assignment", replace(`"budget_consumed":"warn"`, `"budget_consumed":"maybe"`), "clauses.budget_consumed", true},
		{"seal lag below the floor", replace(`"max_seal_lag_seconds":900`, `"max_seal_lag_seconds":240`), "max_seal_lag_seconds: must be between 300", true},
		{"seal lag not whole minutes", replace(`"max_seal_lag_seconds":900`, `"max_seal_lag_seconds":901`), "max_seal_lag_seconds: must be a whole number of minutes", true},
		{"percent out of range", replace(`"budget_consumed_percent":90`, `"budget_consumed_percent":0`), "budget_consumed_percent: must be an integer between 1 and 100", true},
		{"unknown_behavior invalid", replace(`"unknown_behavior":"warn"`, `"unknown_behavior":"ignore"`), "unknown_behavior: must be warn|block", true},
		{"schema_version unknown", replace(`"schema_version":1`, `"schema_version":2`), "schema_version: must be 1", true},
	}
	for _, tc := range cases {
		before := len(fs.gateCalls())
		rec := do(h, gateEditor, http.MethodPut, gateP(gateSvcID)+"/policy", tc.body)
		res := rec.Result()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, res.StatusCode, body)
			continue
		}
		if got := errorOf(t, body); !strings.Contains(got, tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, got, tc.want)
		}
		if reached := len(fs.gateCalls()) > before; reached != tc.reachesStore {
			t.Errorf("%s: reached the store = %v, want %v", tc.name, reached, tc.reachesStore)
		}
	}
}

func TestGatePolicyDelete(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	g := fs.gateState()
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	path := gateP(gateSvcID) + "/policy"

	wantStatus(t, do(h, gateEditor, http.MethodDelete, path, ""), http.StatusBadRequest, "expected_revision_required")
	for _, v := range []string{"abc", "-1", "1.5", ""} {
		wantStatus(t, do(h, gateEditor, http.MethodDelete, path+"?expected_revision="+v, ""), http.StatusBadRequest, "expected_revision_invalid")
	}
	if len(fs.gateCalls()) != 0 {
		t.Fatal("a refused expected_revision reached the store")
	}
	wantStatus(t, do(h, gateEditor, http.MethodDelete, path+"?expected_revision=3", ""), http.StatusNoContent, "")
	if g.lastDeleteExpected != 3 || g.actors[0].Label != "editor@acme" {
		t.Fatalf("delete handed %d / %+v to the store", g.lastDeleteExpected, g.actors)
	}
	g.deleteErr = store.ErrGateRevisionConflict
	wantStatus(t, do(h, gateEditor, http.MethodDelete, path+"?expected_revision=2", ""), http.StatusConflict, "revision_conflict")
	g.deleteErr = store.ErrGatePolicyNotConfigured
	wantStatus(t, do(h, gateEditor, http.MethodDelete, path+"?expected_revision=0", ""), http.StatusNotFound, "not_configured")
}

// ── the override (D9, D13a) ──────────────────────────────────────────────────────────────────

func TestGateOverrideActiveRead(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/override", ""), http.StatusNotFound, "none_active")
	fs.gateState().active = gatePtr(gateOverrideFixture(gateOvID, domain.GateOverrideActive))
	body := wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/override", ""), http.StatusOK, "")
	wantKeys(t, body, "id", "reason", "expires_at", "created_at", "actor_label", "policy_revision")
}

// Every override record carries EVERY field, never absent: an active row has the five closure
// fields null; a system closure has revoked_at + revoked_reason set and the three attribution
// fields null; a manual closure by a token has revoked_by_label `token:<name>`,
// revoked_via_token true and revoked_by_user_id null (D13a, invariant 17).
func TestGateOverrideRecordShape(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	active := gateOverrideFixture(gateOvID, domain.GateOverrideActive)
	system := gateOverrideFixture(gateOvID2, domain.GateOverrideExpired)
	system.RevokedAt, system.RevokedReason = gatePtr(gateT0.Add(3*time.Hour)), domain.GateRevokedExpired
	manual := gateOverrideFixture("4c7d2d3f-5a4b-4e6f-9bac-2d3e4f5a6b7e", domain.GateOverrideRevoked)
	manual.RevokedAt, manual.RevokedReason = gatePtr(gateT0.Add(time.Hour)), domain.GateRevokedManual
	manual.RevokedByLabel, manual.RevokedViaToken = gatePtr("token:ci"), gatePtr(true)
	g := fs.gateState()
	g.overrides = map[string]store.GateOverrideRecord{active.ID: active, system.ID: system, manual.ID: manual}
	g.history = []store.GateOverrideRecord{manual, system, active}
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})

	allKeys := []string{"id", "reason", "expires_at", "created_at", "policy_revision", "actor_label", "actor_user_id", "via_token",
		"status", "revoked_at", "revoked_reason", "revoked_by_label", "revoked_by_user_id", "revoked_via_token"}
	read := func(id string) map[string]json.RawMessage {
		body := wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/overrides/"+id, ""), http.StatusOK, "")
		wantKeys(t, body, allKeys...)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		return m
	}
	a := read(active.ID)
	for _, k := range []string{"revoked_at", "revoked_reason", "revoked_by_label", "revoked_by_user_id", "revoked_via_token"} {
		if string(a[k]) != "null" {
			t.Errorf("active %s = %s, want null", k, a[k])
		}
	}
	if string(a["status"]) != `"active"` || string(a["actor_user_id"]) != `"gate-admin"` || string(a["via_token"]) != "false" {
		t.Errorf("active = %v", a)
	}
	s := read(system.ID)
	if string(s["status"]) != `"expired"` || string(s["revoked_reason"]) != `"expired"` || string(s["revoked_at"]) == "null" ||
		string(s["revoked_by_label"]) != "null" || string(s["revoked_by_user_id"]) != "null" || string(s["revoked_via_token"]) != "null" {
		t.Errorf("system closure = %v", s)
	}
	m := read(manual.ID)
	if string(m["status"]) != `"revoked"` || string(m["revoked_reason"]) != `"manual"` || string(m["revoked_by_label"]) != `"token:ci"` ||
		string(m["revoked_via_token"]) != "true" || string(m["revoked_by_user_id"]) != "null" {
		t.Errorf("manual closure = %v", m)
	}

	// The history lists the same records, newest first as the store ordered them.
	body := wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/overrides", ""), http.StatusOK, "")
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(body, &list)
	if len(list.Items) != 3 || string(list.Items[0]["id"]) != `"`+manual.ID+`"` || len(list.Items[0]) != len(allKeys) {
		t.Fatalf("list = %s", body)
	}
	wantStatus(t, do(h, gateViewer, http.MethodGet, gateP(gateSvcID)+"/overrides/5c7d2d3f-5a4b-4e6f-9bac-2d3e4f5a6b7f", ""), http.StatusNotFound, "not found")
}

func TestGateOverrideCreate(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	g := fs.gateState()
	g.createID = gateOvID
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	path := gateP(gateSvcID) + "/override"

	body := wantStatus(t, do(h, gateAdmin, http.MethodPost, path, gateOverrideBody), http.StatusCreated, "")
	if string(body) != "{\"id\":\""+gateOvID+"\"}\n" {
		t.Fatalf("body = %q", body)
	}
	if g.lastCreate.policyRevision != 3 || g.lastCreate.reason != "hotfix for INC-42" || !g.lastCreate.expiresAt.Equal(time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("create handed %+v to the store", g.lastCreate)
	}
	if a := g.actors[0]; a != (store.GateActor{ActorUserID: "gate-admin", Label: "admin@acme"}) {
		t.Fatalf("actor = %+v", a)
	}
	// A token principal is the typed-token triple: NULL user, via_token, `token:<name>` (D9).
	wantStatus(t, do(h, gateToken, http.MethodPost, path, gateOverrideBody), http.StatusCreated, "")
	if a := g.actors[1]; a != (store.GateActor{ActorUserID: "", ViaToken: true, Label: "token:ci"}) {
		t.Fatalf("token actor = %+v", a)
	}

	g.createErr = store.ErrGateOverrideActive
	wantStatus(t, do(h, gateAdmin, http.MethodPost, path, gateOverrideBody), http.StatusConflict, "override_active")
	g.createErr = store.ErrGateRevisionConflict
	wantStatus(t, do(h, gateAdmin, http.MethodPost, path, gateOverrideBody), http.StatusConflict, "revision_conflict")
	g.createErr = &store.GateValidationError{Field: "expires_at", Msg: "must be at most 168h0m0s ahead"}
	wantStatus(t, do(h, gateAdmin, http.MethodPost, path, gateOverrideBody), http.StatusBadRequest, "expires_at: must be at most")
	g.createErr = &store.GateValidationError{Field: "reason", Msg: "must be between 1 and 500 characters, got 0"}
	wantStatus(t, do(h, gateAdmin, http.MethodPost, path, strings.Replace(gateOverrideBody, "hotfix for INC-42", "", 1)), http.StatusBadRequest, "reason: must be between 1 and 500")
}

// The override body has no `action` and no actor; both are refused as unknown fields, as is any
// missing field or an unparseable expiry — and none of them reaches the store.
func TestGateOverrideCreateIsStrict(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	path := gateP(gateSvcID) + "/override"
	for body, want := range map[string]string{
		`{"policy_revision":3,"reason":"r","expires_at":"2026-08-30T10:00:00Z","action":"ALLOW"}`:     "action: unknown field",
		`{"policy_revision":3,"reason":"r","expires_at":"2026-08-30T10:00:00Z","actor":"me"}`:         "actor: unknown field",
		`{"policy_revision":3,"reason":"r","expires_at":"2026-08-30T10:00:00Z","actor_label":"me"}`:   "actor_label: unknown field",
		`{"reason":"r","expires_at":"2026-08-30T10:00:00Z"}`:                                          "policy_revision: is required",
		`{"policy_revision":3,"expires_at":"2026-08-30T10:00:00Z"}`:                                   "reason: is required",
		`{"policy_revision":3,"reason":"r"}`:                                                          "expires_at: is required",
		`{"policy_revision":3,"reason":"r","expires_at":"tomorrow"}`:                                  "expires_at: must be an RFC3339 timestamp",
		`{"policy_revision":"3","reason":"r","expires_at":"2026-08-30T10:00:00Z"}`:                    "policy_revision: must be an integer, got string",
		`{"policy_revision":3,"reason":"r","expires_at":"2026-08-30T10:00:00Z"}{"policy_revision":4}`: "single JSON value",
	} {
		wantStatus(t, do(h, gateAdmin, http.MethodPost, path, body), http.StatusBadRequest, want)
	}
	if calls := fs.gateCalls(); len(calls) != 0 {
		t.Fatalf("a refused override body reached the store: %v", calls)
	}
}

func TestGateOverrideRevoke(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	g := fs.gateState()
	g.overrides = map[string]store.GateOverrideRecord{gateOvID: gateOverrideFixture(gateOvID, domain.GateOverrideActive)}
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	path := gateP(gateSvcID) + "/overrides/"

	wantStatus(t, do(h, gateAdmin, http.MethodDelete, path+gateOvID, ""), http.StatusNoContent, "")
	if g.lastRevokeID != gateOvID || g.actors[0].Label != "admin@acme" {
		t.Fatalf("revoke handed %q / %+v to the store", g.lastRevokeID, g.actors)
	}
	wantStatus(t, do(h, gateAdmin, http.MethodDelete, path+gateOvID2, ""), http.StatusNotFound, "not found")
	g.revokeErr = store.ErrGateOverrideNotActive
	wantStatus(t, do(h, gateAdmin, http.MethodDelete, path+gateOvID, ""), http.StatusConflict, "override_not_active")
}

// ── the ledger (D10, §5) ─────────────────────────────────────────────────────────────────────

// The by-id read is project-scoped and asks nothing about the service: no service is seeded here
// at all, and the row still answers 200 — the moment the evidence is wanted is the moment the
// service is gone.
func TestGateDecisionByIDHasNoServiceCheck(t *testing.T) {
	fs := seededStore()
	dec := gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	dec.ServiceID = nil // deleted since: present and null
	g := fs.gateState()
	g.decisions = map[string]domain.GateDecision{gateDecID: dec}
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})

	body := wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/gate/decisions/"+gateDecID, ""), http.StatusOK, "")
	if !strings.Contains(string(body), `"service_id":null`) || !strings.Contains(string(body), `"decision_id":"`+gateDecID+`"`) {
		t.Fatalf("body = %s", body)
	}
	if calls := fs.gateCalls(); len(calls) != 1 || calls[0] != "GetGateDecision" {
		t.Fatalf("calls = %v", calls)
	}
	wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/gate/decisions/019906c0-1234-7abc-8def-0123456789ac", ""), http.StatusNotFound, "not found")
	// A row persisted under another project is invisible here, whatever the caller's roles.
	g.decisionProjects = map[string]string{gateDecID: "p2"}
	wantStatus(t, do(h, o1Admin, http.MethodGet, "/api/v1/projects/p1/gate/decisions/"+gateDecID, ""), http.StatusNotFound, "not found")
}

func TestGateDecisionListValidation(t *testing.T) {
	fs := seededStore()
	g := fs.gateState()
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	const base = "/api/v1/projects/p1/gate/decisions"
	const from, to = "2026-08-01T00:00:00Z", "2026-08-15T00:00:00Z"
	for q, want := range map[string]string{
		"":                              "range_required",
		"?from=" + from:                 "range_required",
		"?to=" + to:                     "range_required",
		"?from=yesterday&to=" + to:      "range_invalid",
		"?from=" + to + "&to=" + from:   "range_invalid",
		"?from=" + from + "&to=" + from: "range_invalid",
		"?from=" + from + "&to=2026-09-02T00:00:00Z":                                   "range_too_wide",
		"?from=" + from + "&to=" + to + "&limit=0":                                     "limit_invalid",
		"?from=" + from + "&to=" + to + "&limit=-5":                                    "limit_invalid",
		"?from=" + from + "&to=" + to + "&limit=abc":                                   "limit_invalid",
		"?from=" + from + "&to=" + to + "&limit=201":                                   "limit_invalid",
		"?from=" + from + "&to=" + to + "&cursor=!!!":                                  "cursor_invalid",
		"?from=" + from + "&to=" + to + "&cursor=" + store.GateCursor{}.Encode() + "x": "cursor_invalid",
		"?from=" + from + "&to=" + to + "&service_id=checkout":                         "service_id must be a UUID",
		// `state`: every occurrence must be one of the five, case-sensitively, and the refusal
		// names the offending value — the first bad one, however many good ones surround it.
		"?from=" + from + "&to=" + to + "&state=bogus":                       `state_invalid: "bogus"`,
		"?from=" + from + "&to=" + to + "&state=":                            `state_invalid: ""`,
		"?from=" + from + "&to=" + to + "&state=block":                       `state_invalid: "block"`,
		"?from=" + from + "&to=" + to + "&state=BLOCK&state=nope&state=WARN": `state_invalid: "nope"`,
	} {
		wantStatus(t, do(h, gateViewer, http.MethodGet, base+q, ""), http.StatusBadRequest, want)
	}
	if len(fs.gateCalls()) != 0 {
		t.Fatal("a refused listing query reached the store")
	}

	// Defaults and pass-through: limit 50, no service, no cursor, no states (nil, not an empty
	// slice); an empty page is `[]`, not null, and the last page carries next_cursor null.
	body := wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?from="+from+"&to="+to, ""), http.StatusOK, "")
	if string(body) != "{\"items\":[],\"next_cursor\":null}\n" {
		t.Fatalf("empty page = %q", body)
	}
	if g.lastList.limit != 50 || g.lastList.serviceID != nil || g.lastList.cursor != nil || g.lastList.states != nil ||
		!g.lastList.from.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) || !g.lastList.to.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("store saw %+v", g.lastList)
	}
	// Exactly 31 days is allowed; 200 is the cap; cursor and service_id pass through; a next
	// page is encoded from the cursor the store returned.
	cursor := store.GateCursor{EvaluatedAt: gateT0, ID: gateDecID}
	g.listItems = []domain.GateDecisionSummary{gateDecisionFixture(domain.GateStateWarn, gatePtr(domain.GateActionWarn)).Summary()}
	g.listNext = &cursor
	body = wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?from="+from+"&to=2026-09-01T00:00:00Z&limit=200&service_id="+gateSvcID+"&cursor="+cursor.Encode(), ""), http.StatusOK, "")
	var page struct {
		Items      []map[string]json.RawMessage `json:"items"`
		NextCursor *string                      `json:"next_cursor"`
	}
	_ = json.Unmarshal(body, &page)
	if len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != cursor.Encode() {
		t.Fatalf("page = %s", body)
	}
	wantKeys(t, mustMarshal(t, page.Items[0]), "schema_version", "decision_id", "evaluated_at", "service_id", "service_slug", "service_name", "state", "action", "reasons")
	if g.lastList.limit != 200 || g.lastList.serviceID == nil || *g.lastList.serviceID != gateSvcID || g.lastList.cursor == nil || g.lastList.cursor.ID != gateDecID {
		t.Fatalf("store saw %+v", g.lastList)
	}
	g.listErr = store.ErrGateCursorInvalid
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+"?from="+from+"&to="+to, ""), http.StatusBadRequest, "cursor_invalid")
}

// The `state` filter reaches the store as a deduplicated SET of domain values in first-seen
// order — one occurrence is a one-element set, several are OR-ed, a repeat is folded — and no
// occurrence at all is nil, so the store's "every state" path is the same whether the SPA sends
// nothing or a client never learned the parameter. It rides along with service_id, cursor and
// limit in the same call; a bad value never reaches the store (TestGateDecisionListValidation).
func TestGateDecisionListStateFilter(t *testing.T) {
	fs := seededStore()
	g := fs.gateState()
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	const base = "/api/v1/projects/p1/gate/decisions?from=2026-08-01T00:00:00Z&to=2026-08-15T00:00:00Z"
	for _, tc := range []struct {
		q    string
		want []domain.GateState
	}{
		{"", nil},
		{"&state=BLOCK", []domain.GateState{domain.GateStateBlock}},
		{"&state=BLOCK&state=WARN", []domain.GateState{domain.GateStateBlock, domain.GateStateWarn}},
		{"&state=WARN&state=BLOCK&state=WARN", []domain.GateState{domain.GateStateWarn, domain.GateStateBlock}},
		{"&state=NOT_CONFIGURED&state=UNKNOWN&state=ALLOW", []domain.GateState{domain.GateStateNotConfigured, domain.GateStateUnknown, domain.GateStateAllow}},
	} {
		wantStatus(t, do(h, gateViewer, http.MethodGet, base+tc.q, ""), http.StatusOK, "")
		if !reflect.DeepEqual(g.lastList.states, tc.want) {
			t.Errorf("%q: store saw states %#v, want %#v", tc.q, g.lastList.states, tc.want)
		}
	}
	if n := len(fs.gateCalls()); n != 5 {
		t.Fatalf("%d store calls, want one per accepted query", n)
	}
	// Composition: service_id, cursor, limit and the state set travel in ONE call.
	cursor := store.GateCursor{EvaluatedAt: gateT0, ID: gateDecID}
	wantStatus(t, do(h, gateViewer, http.MethodGet, base+"&service_id="+gateSvcID+"&cursor="+cursor.Encode()+"&state=BLOCK&limit=7", ""), http.StatusOK, "")
	if g.lastList.serviceID == nil || *g.lastList.serviceID != gateSvcID || g.lastList.cursor == nil || g.lastList.cursor.ID != gateDecID ||
		g.lastList.limit != 7 || !reflect.DeepEqual(g.lastList.states, []domain.GateState{domain.GateStateBlock}) {
		t.Fatalf("store saw %+v", g.lastList)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ── authorization (D12) ──────────────────────────────────────────────────────────────────────

func TestGateAuthzMatrix(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	g := fs.gateState()
	g.decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	g.putRevision, g.createID = 1, gateOvID
	g.overrides = map[string]store.GateOverrideRecord{gateOvID: gateOverrideFixture(gateOvID, domain.GateOverrideActive)}
	g.active = gatePtr(gateOverrideFixture(gateOvID, domain.GateOverrideActive))
	g.policy = &domain.GatePolicy{Revision: 1, Clauses: map[domain.GateClause]domain.ClauseAssignment{}}
	g.decisions = map[string]domain.GateDecision{gateDecID: g.decision}
	h := newGateHandler(fs, gateLimits(), &gateFakeMetrics{})
	base := gateP(gateSvcID)
	cases := []struct {
		who          authz.Principal
		method, path string
		body         string
		want         int
	}{
		// viewer: gate:evaluate only
		{gateViewer, http.MethodPost, base, "{}", http.StatusOK},
		{gateViewer, http.MethodGet, base + "/policy", "", http.StatusOK},
		{gateViewer, http.MethodGet, base + "/override", "", http.StatusOK},
		{gateViewer, http.MethodGet, base + "/overrides", "", http.StatusOK},
		{gateViewer, http.MethodGet, base + "/overrides/" + gateOvID, "", http.StatusOK},
		{gateViewer, http.MethodGet, "/api/v1/projects/p1/gate/decisions/" + gateDecID, "", http.StatusOK},
		{gateViewer, http.MethodGet, "/api/v1/projects/p1/gate/decisions?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z", "", http.StatusOK},
		{gateViewer, http.MethodPut, base + "/policy", gatePolicyBody, http.StatusForbidden},
		{gateViewer, http.MethodDelete, base + "/policy?expected_revision=1", "", http.StatusForbidden},
		{gateViewer, http.MethodPost, base + "/override", gateOverrideBody, http.StatusForbidden},
		{gateViewer, http.MethodDelete, base + "/overrides/" + gateOvID, "", http.StatusForbidden},
		// editor: + gate:policy:write, still no gate:override
		{gateEditor, http.MethodPut, base + "/policy", gatePolicyBody, http.StatusOK},
		{gateEditor, http.MethodDelete, base + "/policy?expected_revision=1", "", http.StatusNoContent},
		{gateEditor, http.MethodPost, base + "/override", gateOverrideBody, http.StatusForbidden},
		{gateEditor, http.MethodDelete, base + "/overrides/" + gateOvID, "", http.StatusForbidden},
		// project admin: everything
		{gateAdmin, http.MethodPut, base + "/policy", gatePolicyBody, http.StatusOK},
		{gateAdmin, http.MethodPost, base + "/override", gateOverrideBody, http.StatusCreated},
		{gateAdmin, http.MethodDelete, base + "/overrides/" + gateOvID, "", http.StatusNoContent},
		{gateAdmin, http.MethodDelete, base + "/policy?expected_revision=1", "", http.StatusNoContent},
		// org admin and global admin hold all three; an outsider sees no project at all
		{o1Admin, http.MethodPost, base + "/override", gateOverrideBody, http.StatusCreated},
		{globalAdmin, http.MethodPost, base + "/override", gateOverrideBody, http.StatusCreated},
		{outsider, http.MethodPost, base, "{}", http.StatusNotFound},
		{outsider, http.MethodGet, "/api/v1/projects/p1/gate/decisions/" + gateDecID, "", http.StatusNotFound},
		// a token asks the gate with the role it already has (D12)
		{gateToken, http.MethodPost, base, "{}", http.StatusOK},
	}
	for _, tc := range cases {
		before := len(fs.gateCalls())
		rec := do(h, tc.who, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s %s %s = %d, want %d (%s)", tc.who.UserID, tc.method, tc.path, rec.Code, tc.want, rec.Body)
		}
		if (tc.want == http.StatusForbidden || tc.want == http.StatusNotFound) && len(fs.gateCalls()) != before {
			t.Errorf("%s %s %s: a refused request reached the gate store", tc.who.UserID, tc.method, tc.path)
		}
	}
}

// ── the §5a bounds over HTTP ─────────────────────────────────────────────────────────────────

// holdDecisions fires n decision requests as `who` and returns once every one of them is INSIDE
// the store's DecideGate (blocked on the fake's release channel), plus a function that releases
// them and waits for their 200s.
func holdDecisions(t *testing.T, h http.Handler, fs *fakeStore, who []authz.Principal) func() {
	t.Helper()
	g := fs.gateState()
	g.hold, g.started, g.release = len(who), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for _, p := range who {
		wg.Add(1)
		go func(p authz.Principal) {
			defer wg.Done()
			if rec := do(h, p, http.MethodPost, gateP(gateSvcID), "{}"); rec.Code != http.StatusOK {
				t.Errorf("held decision for %s = %d (%s)", p.UserID, rec.Code, rec.Body)
			}
		}(p)
	}
	for range who {
		<-g.started
	}
	return func() { close(g.release); wg.Wait() }
}

func TestGateInflightPrincipalCapOverHTTP(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	fs.gateState().decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	m := &gateFakeMetrics{}
	limits := gateLimits()
	limits.InflightPrincipal = 2
	h := newGateHandler(fs, limits, m)

	release := holdDecisions(t, h, fs, []authz.Principal{gateViewer, gateViewer})
	rec := do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}")
	wantStatus(t, rec, http.StatusTooManyRequests, "principal_inflight")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if n := len(fs.gateCalls()); n != 2 {
		t.Fatalf("the refused request reached the store: %d DecideGate calls, want 2", n)
	}
	// Another principal is not blocked by the first one's in-flight count.
	wantStatus(t, do(h, gateEditor, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")
	release()
	_, rejected, _, durations := m.snapshot()
	if len(rejected) != 1 || rejected[0] != "principal_inflight" || durations != 3 {
		t.Fatalf("metrics rejected=%v durations=%d", rejected, durations)
	}
}

func TestGateInflightProcessCapOverHTTP(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	fs.gateState().decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	m := &gateFakeMetrics{}
	limits := gateLimits()
	limits.InflightProcess, limits.InflightPrincipal = 2, 2
	h := newGateHandler(fs, limits, m)

	release := holdDecisions(t, h, fs, []authz.Principal{gateViewer, gateEditor})
	rec := do(h, gateAdmin, http.MethodPost, gateP(gateSvcID), "{}")
	wantStatus(t, rec, http.StatusTooManyRequests, "process_inflight")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	// A ledger read takes the same permits and is refused the same way, consuming no token.
	rec = do(h, gateAdmin, http.MethodGet, "/api/v1/projects/p1/gate/decisions/"+gateDecID, "")
	wantStatus(t, rec, http.StatusTooManyRequests, "process_inflight")
	if n := len(fs.gateCalls()); n != 2 {
		t.Fatalf("store calls = %v", fs.gateCalls())
	}
	release()
	_, rejected, _, _ := m.snapshot()
	if strings.Join(rejected, ",") != "process_inflight,process_inflight" {
		t.Fatalf("rejected = %v", rejected)
	}
}

// A sequential flood from one principal at concurrency 1 is rate-limited: with 3/min the 4th
// request is 429 principal_rate with Retry-After 20 (one token per 20 s), no store call, no
// decision counted; the mutation removing the rate bound lets it through and fails here.
func TestGateSequentialFloodIsRateLimitedOverHTTP(t *testing.T) {
	fs := seededStore()
	seedGateService(fs, "p1", gateSvcID)
	fs.gateState().decision = gateDecisionFixture(domain.GateStateAllow, gatePtr(domain.GateActionAllow))
	m := &gateFakeMetrics{}
	limits := gateLimits()
	limits.RatePrincipalPerMinute = 3
	h := newGateHandler(fs, limits, m)

	for i := 0; i < 3; i++ {
		wantStatus(t, do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}"), http.StatusOK, "")
	}
	rec := do(h, gateViewer, http.MethodPost, gateP(gateSvcID), "{}")
	wantStatus(t, rec, http.StatusTooManyRequests, "principal_rate")
	if got := rec.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20", got)
	}
	if n := len(fs.gateCalls()); n != 3 {
		t.Fatalf("the rate-refused request reached the store: %d calls", n)
	}
	// Ledger reads take no rate token: the same drained principal reads freely.
	fs.gateState().decisions = map[string]domain.GateDecision{gateDecID: fs.gateState().decision}
	for i := 0; i < 5; i++ {
		wantStatus(t, do(h, gateViewer, http.MethodGet, "/api/v1/projects/p1/gate/decisions/"+gateDecID, ""), http.StatusOK, "")
	}
	decisions, rejected, _, durations := m.snapshot()
	if len(decisions) != 3 || len(rejected) != 1 || rejected[0] != "principal_rate" || durations != 3 {
		t.Fatalf("metrics decisions=%v rejected=%v durations=%d", decisions, rejected, durations)
	}
}
