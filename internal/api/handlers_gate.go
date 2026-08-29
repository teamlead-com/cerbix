package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-024 — the reliability gate's HTTP surface (func-reliability-gate.md D7, D9, D12, D13a, D15,
// §5, §5a; iter-0163 changeset 4).
//
// Every route follows one order, so each refusal leaves the database untouched and answers
// what it is about: authorization first (projectAccess — 404 for an invisible project, 403 for
// a missing action; the action names are D12's and no handler compares roles), then the path
// (a malformed service, override or decision id is 400 BEFORE the store is asked — D15), then
// the body or query (decoded STRICTLY: an unknown field, a missing field or a wrong type is
// 400 naming the field, and the server fills nothing in — D11, D14), then the §5a bounds (a
// 429 runs no report and writes no ledger row), and only then the store. The store's
// sentinels map to the status codes of D13a without string matching; the domain's and the
// store's validation errors already name the field, so a 400 carries `field: rule` verbatim.

// gatePolicyView is the GET …/gate/policy shape of D13a, field for field.
type gatePolicyView struct {
	SchemaVersion         int                                           `json:"schema_version"`
	Window                string                                        `json:"window"`
	Clauses               map[domain.GateClause]domain.ClauseAssignment `json:"clauses"`
	BudgetConsumedPercent int                                           `json:"budget_consumed_percent"`
	MaxSealLagSeconds     int                                           `json:"max_seal_lag_seconds"`
	UnknownBehavior       domain.GateUnknownBehavior                    `json:"unknown_behavior"`
	Revision              int64                                         `json:"revision"`
	UpdatedAt             time.Time                                     `json:"updated_at"`
	UpdatedBy             string                                        `json:"updated_by"`
}

func newGatePolicyView(p domain.GatePolicy) gatePolicyView {
	return gatePolicyView{
		SchemaVersion:         p.SchemaVersion,
		Window:                p.Window,
		Clauses:               p.Clauses,
		BudgetConsumedPercent: p.BudgetConsumedPercent,
		MaxSealLagSeconds:     p.MaxSealLagSeconds,
		UnknownBehavior:       p.UnknownBehavior,
		Revision:              p.Revision,
		UpdatedAt:             p.UpdatedAt,
		UpdatedBy:             p.UpdatedBy,
	}
}

// gatePolicyWriteRequest is the PUT body. Every value field is a pointer or a raw message so
// "absent" is distinguishable from a zero: an omitted field is a refused request, never a
// default (D11). `clauses` stays raw because encoding/json collapses a duplicate key silently,
// and D14 refuses a duplicate BY NAME — the object is walked token by token into the ordered
// list the domain validates. `expected_revision` stays raw because `null` ("I believe nothing is
// configured") and absent (a refused request) are different statements.
type gatePolicyWriteRequest struct {
	ExpectedRevision      json.RawMessage `json:"expected_revision"`
	SchemaVersion         *int            `json:"schema_version"`
	Window                *string         `json:"window"`
	Clauses               json.RawMessage `json:"clauses"`
	BudgetConsumedPercent *int            `json:"budget_consumed_percent"`
	MaxSealLagSeconds     *int            `json:"max_seal_lag_seconds"`
	UnknownBehavior       *string         `json:"unknown_behavior"`
}

// gateOverrideCreateRequest is the POST …/gate/override body (D13a). There is NO `action` and no
// actor: D9 fixes what an override does, and the actor is the authenticated principal; either
// arriving on the wire is an unknown field.
type gateOverrideCreateRequest struct {
	PolicyRevision *int64  `json:"policy_revision"`
	Reason         *string `json:"reason"`
	ExpiresAt      *string `json:"expires_at"`
}

// gateOverrideActiveView is GET …/gate/override: the active override only, without history or
// attribution detail (D13a).
type gateOverrideActiveView struct {
	ID             string    `json:"id"`
	Reason         string    `json:"reason"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	ActorLabel     string    `json:"actor_label"`
	PolicyRevision int64     `json:"policy_revision"`
}

// gateOverrideRecordView is one override record (D13a): EVERY field is always present — an
// active row carries the five closure fields as null; a system closure carries revoked_at and
// revoked_reason with the three attribution fields null; a manual closure carries all five.
type gateOverrideRecordView struct {
	ID              string                    `json:"id"`
	Reason          string                    `json:"reason"`
	ExpiresAt       time.Time                 `json:"expires_at"`
	CreatedAt       time.Time                 `json:"created_at"`
	PolicyRevision  int64                     `json:"policy_revision"`
	ActorLabel      string                    `json:"actor_label"`
	ActorUserID     *string                   `json:"actor_user_id"`
	ViaToken        bool                      `json:"via_token"`
	Status          domain.GateOverrideStatus `json:"status"`
	RevokedAt       *time.Time                `json:"revoked_at"`
	RevokedReason   *string                   `json:"revoked_reason"`
	RevokedByLabel  *string                   `json:"revoked_by_label"`
	RevokedByUserID *string                   `json:"revoked_by_user_id"`
	RevokedViaToken *bool                     `json:"revoked_via_token"`
}

func newGateOverrideRecordView(rec store.GateOverrideRecord) gateOverrideRecordView {
	o := rec.GateOverride
	v := gateOverrideRecordView{
		ID:              o.ID,
		Reason:          o.Reason,
		ExpiresAt:       o.ExpiresAt,
		CreatedAt:       o.CreatedAt,
		PolicyRevision:  o.PolicyRevision,
		ActorLabel:      o.ActorLabel,
		ActorUserID:     o.ActorUserID,
		ViaToken:        o.ViaToken,
		Status:          rec.Status,
		RevokedAt:       o.RevokedAt,
		RevokedByLabel:  o.RevokedByLabel,
		RevokedByUserID: o.RevokedByUserID,
		RevokedViaToken: o.RevokedViaToken,
	}
	if o.RevokedReason != "" {
		reason := string(o.RevokedReason)
		v.RevokedReason = &reason
	}
	return v
}

// gateDecisionListResponse is the §5 listing page: `next_cursor` is null on the last page.
type gateDecisionListResponse struct {
	Items      []domain.GateDecisionSummary `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
}

// gateListRangeMax bounds `to − from` on the listing (§5: "≤ 31 days").
const gateListRangeMax = 31 * 24 * time.Hour

// gateListDefaultLimit is the page size when `limit` is absent (§5).
const gateListDefaultLimit = 50

// ── path parameters ──────────────────────────────────────────────────────────────────────────

// overrideIDParam and decisionIDParam follow serviceIDParam's convention (D15): transport owns
// the format, so a malformed id is 400 HERE and the store never sees a value PostgreSQL would
// refuse with a cast error; a well-formed but absent id is the store's honest 404.
func overrideIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("overrideID")
	if !serviceUUIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "overrideID must be a UUID")
		return "", false
	}
	return id, true
}

func decisionIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("decisionID")
	if !serviceUUIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "decisionID must be a UUID")
		return "", false
	}
	return id, true
}

// ── principal, actor, bounds ─────────────────────────────────────────────────────────────────

// gateActor is the request's principal as every gate mutation stores it (D9): the typed
// attribution the audit log uses — AuditUserID() is "" (NULL) for a synthetic API-token
// identity, the real uuid otherwise — plus the immutable server-derived label, `token:<name>`
// for a token and the user for a person. Never client-supplied.
func (h *Handler) gateActor(r *http.Request) store.GateActor {
	p, _ := h.principal(r)
	return store.GateActor{ActorUserID: p.AuditUserID(), ViaToken: p.ViaToken, Label: p.AuditActorLabel()}
}

// gatePrincipalKey is the limiter's per-principal key (§5a: "token id or user id"): the
// authenticated identity the auth layer set — `apitoken:<id>` for a token, the user uuid for a
// person — never the label text, which is display data.
func gatePrincipalKey(p authz.Principal) string {
	if p.UserID == "" {
		return "anonymous"
	}
	return p.UserID
}

// gateAdmit takes the §5a bounds for this request — the in-flight permits, and the rate
// tokens when `rate` is true (decisions only; ledger reads take permits alone) — and writes
// the 429 with its Retry-After when refused. The returned release must be called when the
// request is done. Without a wired limiter the route answers 501: a gate with no bound is the
// load §5a exists to shed.
func (h *Handler) gateAdmit(w http.ResponseWriter, r *http.Request, rate bool) (func(), bool) {
	if h.gate == nil {
		writeError(w, http.StatusNotImplemented, "gate_not_wired")
		return nil, false
	}
	p, _ := h.principal(r)
	release, refusal := h.gate.acquire(gatePrincipalKey(p), rate)
	if refusal != nil {
		if h.gateMetrics != nil {
			if err := h.gateMetrics.RecordGateEvaluateRejected(refusal.reason); err != nil {
				h.logger.Error("gate_metric", "op", "rejected", "error", err.Error())
			}
		}
		w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfter))
		writeError(w, http.StatusTooManyRequests, refusal.reason)
		return nil, false
	}
	return release, true
}

func (h *Handler) recordGateDecision(dec domain.GateDecision) {
	if h.gateMetrics == nil {
		return
	}
	action := ""
	if dec.Action != nil {
		action = string(*dec.Action)
	}
	if err := h.gateMetrics.RecordGateDecision(string(dec.State), action, dec.Overridden()); err != nil {
		h.logger.Error("gate_metric", "op", "decision", "error", err.Error())
	}
}

func (h *Handler) recordGateEvaluateError(kind string) {
	if h.gateMetrics == nil {
		return
	}
	if err := h.gateMetrics.RecordGateEvaluateError(kind); err != nil {
		h.logger.Error("gate_metric", "op", "evaluate_error", "error", err.Error())
	}
}

// ── strict body decoding ─────────────────────────────────────────────────────────────────────

// gateDecodeBody decodes ONE JSON value strictly into dst and answers 400 naming the field on
// an unknown field or a wrong type (D13a: "an unknown field, a missing field or a wrong type is
// 400 naming the field"). Missing fields are the caller's to detect — the request structs use
// pointers so absence is visible. A trailing second value fails closed, as decodeJSONBody does.
func gateDecodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, serviceMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, gateDecodeError(err))
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must be a single JSON value")
		return false
	}
	return true
}

// gateDecodeError renders an encoding/json failure as `field: rule`. The unknown-field error
// has no typed form in the standard library, so its message is the one string compared here.
func gateDecodeError(err error) string {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return field + ": must be " + gateWantType(typeErr.Type) + ", got " + typeErr.Value
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return name + ": unknown field"
	case errors.Is(err, io.EOF):
		return "body: must be a JSON object"
	default:
		return "invalid request body"
	}
}

func gateWantType(t reflect.Type) string {
	if t == nil {
		return "a JSON value"
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Struct, reflect.Map:
		return "an object"
	case reflect.Slice, reflect.Array:
		return "an array"
	}
	return t.String()
}

// gateDecodeEmptyBody accepts the decision request's body: nothing, or an empty JSON object.
// Anything else — an `override`, an `actor`, any field at all — is refused as an unknown field
// naming it (D9, §7 "the same field in a decision body is refused as unknown-field"), so the
// override path stays the one endpoint that can create one.
func gateDecodeEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, serviceMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct{}{}); err != nil {
		writeError(w, http.StatusBadRequest, gateDecodeError(err))
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must be a single JSON value")
		return false
	}
	return true
}

// decodeGateClauses walks the `clauses` object token by token into the ORDERED entry list the
// domain validates, keeping a duplicate key so ValidateGatePolicyV1 can refuse it by name
// (D14). It returns the 400 text on a shape error, "" on success.
func decodeGateClauses(raw json.RawMessage) ([]domain.GateClauseEntry, string) {
	if len(raw) == 0 {
		return nil, "clauses: is required"
	}
	const shape = "clauses: must be an object mapping every clause of the schema version to block|warn|ignore"
	dec := json.NewDecoder(bytes.NewReader(raw))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return nil, shape
	}
	out := []domain.GateClauseEntry{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, shape
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, shape
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, shape
		}
		val, ok := valTok.(string)
		if !ok {
			return nil, "clauses." + key + ": must be a string, one of block|warn|ignore"
		}
		out = append(out, domain.GateClauseEntry{Clause: domain.GateClause(key), Assignment: domain.ClauseAssignment(val)})
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') {
		return nil, shape
	}
	return out, ""
}

// decodeExpectedRevision reads the PUT body's `expected_revision`: absent is a refused
// request; `null` is nil ("nothing configured"); otherwise a non-negative integer.
func decodeExpectedRevision(raw json.RawMessage) (*int64, string) {
	if len(raw) == 0 {
		return nil, "expected_revision: is required (null when nothing is configured)"
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ""
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n < 0 {
		return nil, "expected_revision: must be a non-negative integer or null"
	}
	return &n, ""
}

// ── error mapping ────────────────────────────────────────────────────────────────────────────

// writeGateError maps a policy/override/ledger store error onto D13a's answers. It returns true
// when it wrote a response.
func (h *Handler) writeGateError(w http.ResponseWriter, op string, err error) bool {
	var policyErr *domain.GatePolicyError
	var fieldErr *store.GateValidationError
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrGatePolicyNotConfigured):
		writeError(w, http.StatusNotFound, "not_configured")
	case errors.Is(err, store.ErrGateOverrideNone):
		writeError(w, http.StatusNotFound, "none_active")
	case errors.Is(err, store.ErrGateRevisionConflict):
		writeError(w, http.StatusConflict, "revision_conflict")
	case errors.Is(err, store.ErrGateOverrideActive):
		writeError(w, http.StatusConflict, "override_active")
	case errors.Is(err, store.ErrGateOverrideNotActive):
		writeError(w, http.StatusConflict, "override_not_active")
	case errors.Is(err, store.ErrGateCursorInvalid):
		writeError(w, http.StatusBadRequest, "cursor_invalid")
	case errors.As(err, &policyErr):
		writeError(w, http.StatusBadRequest, policyErr.Error())
	case errors.As(err, &fieldErr):
		writeError(w, http.StatusBadRequest, fieldErr.Error())
	default:
		h.serverError(w, op, err)
	}
	return true
}

// writeGateDecisionError maps a failed evaluation (D6a, D10, §5a): a transport-class failure is
// 503 with the error code the CLI prints, and the evaluation error counter moves by kind.
// A foreign or unknown service is the tenant 404 of D15 and no evaluation error at all.
func (h *Handler) writeGateDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrGateSnapshotConflict):
		h.recordGateEvaluateError("snapshot_conflict")
		writeError(w, http.StatusServiceUnavailable, "snapshot_conflict")
	case errors.Is(err, store.ErrGateLedgerUnwritable):
		h.recordGateEvaluateError("ledger_unwritable")
		writeError(w, http.StatusServiceUnavailable, "ledger_unwritable")
	case errors.Is(err, store.ErrGateBudgetExceeded), errors.Is(err, context.DeadlineExceeded):
		h.recordGateEvaluateError("timeout")
		writeError(w, http.StatusServiceUnavailable, "timeout")
	case errors.Is(err, context.Canceled):
		// The caller went away; nothing was evaluated for anybody to count.
		h.serverError(w, "gate_decide", err)
	default:
		h.recordGateEvaluateError("error")
		h.serverError(w, "gate_decide", err)
	}
}

// ── the decision ─────────────────────────────────────────────────────────────────────────────

// postGateDecision is the gate's one question (D6, D6a, D7): authorization, the path, an EMPTY
// body, the §5a bounds, then ONE store call under the transaction budget. NOT_CONFIGURED is a
// 200 carrying that state and no action — never an error (invariant 2).
func (h *Handler) postGateDecision(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	if !gateDecodeEmptyBody(w, r) {
		return
	}
	release, ok := h.gateAdmit(w, r, true)
	if !ok {
		return
	}
	defer release()

	started := time.Now()
	dec, err := h.store.DecideGate(r.Context(), proj.ID, serviceID, h.gate.limits.TxBudget)
	if h.gateMetrics != nil {
		h.gateMetrics.ObserveGateDecisionDuration(time.Since(started))
	}
	if err != nil {
		h.writeGateDecisionError(w, err)
		return
	}
	if dec.Reasons == nil {
		dec.Reasons = []domain.GateReasonEntry{}
	}
	h.recordGateDecision(dec)
	writeJSON(w, http.StatusOK, dec)
}

// ── the policy ───────────────────────────────────────────────────────────────────────────────

func (h *Handler) getGatePolicy(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetGatePolicy(r.Context(), proj.ID, serviceID)
	if h.writeGateError(w, "gate_policy_get", err) {
		return
	}
	writeJSON(w, http.StatusOK, newGatePolicyView(p))
}

// putGatePolicy creates or replaces the policy (D13a, D14). The body is decoded strictly and
// checked for presence here — transport owns shape — and handed to the store as the ORDERED
// document the domain validates exhaustively (unknown, missing and duplicate clauses by name;
// the ranges; the window the service has a target for). The CAS on `expected_revision` runs in
// the store before the no-op comparison, so a stale revision is 409 even for an identical body.
func (h *Handler) putGatePolicy(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGatePolicyWrite)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	var req gatePolicyWriteRequest
	if !gateDecodeBody(w, r, &req) {
		return
	}
	expected, msg := decodeExpectedRevision(req.ExpectedRevision)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"schema_version", req.SchemaVersion != nil},
		{"window", req.Window != nil},
		{"budget_consumed_percent", req.BudgetConsumedPercent != nil},
		{"max_seal_lag_seconds", req.MaxSealLagSeconds != nil},
		{"unknown_behavior", req.UnknownBehavior != nil},
	} {
		if !f.present {
			writeError(w, http.StatusBadRequest, f.name+": is required")
			return
		}
	}
	clauses, msg := decodeGateClauses(req.Clauses)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	doc := domain.GatePolicyDocument{
		SchemaVersion:         *req.SchemaVersion,
		Window:                *req.Window,
		Clauses:               clauses,
		BudgetConsumedPercent: *req.BudgetConsumedPercent,
		MaxSealLagSeconds:     *req.MaxSealLagSeconds,
		UnknownBehavior:       domain.GateUnknownBehavior(*req.UnknownBehavior),
	}
	revision, _, err := h.store.PutGatePolicy(r.Context(), proj.ID, serviceID, expected, doc, h.gateActor(r))
	if h.writeGateError(w, "gate_policy_put", err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"revision": revision})
}

// deleteGatePolicy tombstones the policy (D13a): `expected_revision` is REQUIRED in the query,
// a non-negative integer; the store bumps the generation and closes the active override.
func (h *Handler) deleteGatePolicy(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGatePolicyWrite)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !q.Has("expected_revision") {
		writeError(w, http.StatusBadRequest, "expected_revision_required")
		return
	}
	expected, err := strconv.ParseInt(q.Get("expected_revision"), 10, 64)
	if err != nil || expected < 0 {
		writeError(w, http.StatusBadRequest, "expected_revision_invalid")
		return
	}
	if h.writeGateError(w, "gate_policy_delete", h.store.DeleteGatePolicy(r.Context(), proj.ID, serviceID, expected, h.gateActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── the override ─────────────────────────────────────────────────────────────────────────────

// getGateOverride is the ACTIVE override only (D13a) — `status` computed at read time by the
// store — or 404 `none_active`.
func (h *Handler) getGateOverride(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	rec, err := h.store.ActiveGateOverride(r.Context(), proj.ID, serviceID)
	if h.writeGateError(w, "gate_override_active", err) {
		return
	}
	o := rec.GateOverride
	writeJSON(w, http.StatusOK, gateOverrideActiveView{
		ID: o.ID, Reason: o.Reason, ExpiresAt: o.ExpiresAt, CreatedAt: o.CreatedAt,
		ActorLabel: o.ActorLabel, PolicyRevision: o.PolicyRevision,
	})
}

// createGateOverride is the ONE way an override comes to exist (D9, invariant 8): the actor is
// the principal, the body carries exactly `policy_revision`, `reason` and `expires_at`, and the
// store enforces the slot, the revision binding and the seven-day bound on ITS clock.
func (h *Handler) createGateOverride(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateOverride)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	var req gateOverrideCreateRequest
	if !gateDecodeBody(w, r, &req) {
		return
	}
	switch {
	case req.PolicyRevision == nil:
		writeError(w, http.StatusBadRequest, "policy_revision: is required")
		return
	case req.Reason == nil:
		writeError(w, http.StatusBadRequest, "reason: is required")
		return
	case req.ExpiresAt == nil:
		writeError(w, http.StatusBadRequest, "expires_at: is required")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, *req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at: must be an RFC3339 timestamp")
		return
	}
	id, err := h.store.CreateGateOverride(r.Context(), proj.ID, serviceID, *req.PolicyRevision, *req.Reason, expiresAt, h.gateActor(r))
	if h.writeGateError(w, "gate_override_create", err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// listGateOverrides is the history: the newest 50 with their read-time status (D13a).
func (h *Handler) listGateOverrides(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	recs, err := h.store.ListGateOverrides(r.Context(), proj.ID, serviceID)
	if h.writeGateError(w, "gate_override_list", err) {
		return
	}
	items := make([]gateOverrideRecordView, 0, len(recs))
	for _, rec := range recs {
		items = append(items, newGateOverrideRecordView(rec))
	}
	writeJSON(w, http.StatusOK, map[string][]gateOverrideRecordView{"items": items})
}

// getGateOverrideByID is one record, whatever its status, with both attribution triples
// (D13a; invariant 17's "later read of the override").
func (h *Handler) getGateOverrideByID(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	overrideID, ok := overrideIDParam(w, r)
	if !ok {
		return
	}
	rec, err := h.store.GetGateOverride(r.Context(), proj.ID, serviceID, overrideID)
	if h.writeGateError(w, "gate_override_get", err) {
		return
	}
	writeJSON(w, http.StatusOK, newGateOverrideRecordView(rec))
}

// revokeGateOverride closes one override by its IMMUTABLE id, never "the current one" (D13a):
// a stale screen revoking an expired or superseded override learns it is stale with 409.
func (h *Handler) revokeGateOverride(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateOverride)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	overrideID, ok := overrideIDParam(w, r)
	if !ok {
		return
	}
	if h.writeGateError(w, "gate_override_revoke", h.store.RevokeGateOverride(r.Context(), proj.ID, serviceID, overrideID, h.gateActor(r))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── the ledger ───────────────────────────────────────────────────────────────────────────────

// getGateDecision is the PROJECT-scoped by-id read (D10, §5): no service-existence check on
// the path, because the evidence is wanted exactly when the service is gone. It takes the
// in-flight permits of §5a and no rate token.
func (h *Handler) getGateDecision(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	decisionID, ok := decisionIDParam(w, r)
	if !ok {
		return
	}
	release, ok := h.gateAdmit(w, r, false)
	if !ok {
		return
	}
	defer release()
	dec, err := h.store.GetGateDecision(r.Context(), proj.ID, decisionID)
	if h.writeGateError(w, "gate_decision_get", err) {
		return
	}
	if dec.Reasons == nil {
		dec.Reasons = []domain.GateReasonEntry{}
	}
	writeJSON(w, http.StatusOK, dec)
}

// listGateDecisions is the §5 listing: `from`/`to` required, half-open, at most 31 days;
// `limit` 1..200 defaulting to 50; an opaque keyset `cursor`; an optional `service_id` that
// yields an EMPTY page when foreign, never 404, because the ledger outlives services.
func (h *Handler) listGateDecisions(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionGateEvaluate)
	if !ok {
		return
	}
	q := r.URL.Query()
	fromS, toS := q.Get("from"), q.Get("to")
	if fromS == "" || toS == "" {
		writeError(w, http.StatusBadRequest, "range_required")
		return
	}
	from, errFrom := time.Parse(time.RFC3339Nano, fromS)
	to, errTo := time.Parse(time.RFC3339Nano, toS)
	if errFrom != nil || errTo != nil || !to.After(from) {
		writeError(w, http.StatusBadRequest, "range_invalid")
		return
	}
	if to.Sub(from) > gateListRangeMax {
		writeError(w, http.StatusBadRequest, "range_too_wide")
		return
	}
	limit := gateListDefaultLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > store.GateListLimitMax {
			writeError(w, http.StatusBadRequest, "limit_invalid")
			return
		}
		limit = n
	}
	var serviceID *string
	if q.Has("service_id") {
		v := q.Get("service_id")
		if !serviceUUIDPattern.MatchString(v) {
			writeError(w, http.StatusBadRequest, "service_id must be a UUID")
			return
		}
		serviceID = &v
	}
	var cursor *store.GateCursor
	if q.Has("cursor") {
		c, err := store.DecodeGateCursor(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor_invalid")
			return
		}
		cursor = &c
	}
	release, ok := h.gateAdmit(w, r, false)
	if !ok {
		return
	}
	defer release()
	items, next, err := h.store.ListGateDecisions(r.Context(), proj.ID, from.UTC(), to.UTC(), serviceID, cursor, limit)
	if h.writeGateError(w, "gate_decision_list", err) {
		return
	}
	if items == nil {
		items = []domain.GateDecisionSummary{}
	}
	for i := range items {
		if items[i].Reasons == nil {
			items[i].Reasons = []domain.GateReasonEntry{}
		}
	}
	resp := gateDecisionListResponse{Items: items}
	if next != nil {
		enc := next.Encode()
		resp.NextCursor = &enc
	}
	writeJSON(w, http.StatusOK, resp)
}
