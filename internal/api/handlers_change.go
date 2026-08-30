package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-025 — change intelligence's HTTP surface (func-change-intelligence.md D2, D3, D5, D6, D7,
// D8, D11, D12, §5a, §7; iter-0165 changeset 3). The shape is the gate's, route for route:
//
// Authorization first (projectAccess — 404 for an invisible project, 403 for a missing action;
// `change:record` on the write, `project:read` on every read, and no handler compares roles —
// D12), then the path (a malformed service or incident id is 400 BEFORE the store is asked),
// then the body or query — the body decoded STRICTLY so an unknown field, a missing field or a
// wrong type is 400 naming it (D2), every text field NORMALIZED (Unicode NFC + trim, the
// transport's half of D2's three layers) before anything validates it, and the domain's
// validators run HERE on the canonical values so a malformed record costs no rate token — then
// the §5a bounds (permits, then the atomic bucket debit; a 429 reaches no store), and only then
// the ONE store call. The store's closed codes (*domain.ChangeError) map to 400/404/409 without
// string matching, and the body carries the domain's own rendering — the code first, then the
// field, then the rule — so the CLI can print it verbatim.

// ── request and response shapes ──────────────────────────────────────────────────────────────

// changeRecordRequest is the POST …/changes body (D2). Every field is a pointer so absence is
// distinguishable from a zero: `kind`, `phase`, `source`, `external_id` are required; `ref`,
// `url` default to ""; `occurred_at` defaults to the server clock; `decision_id` to none. There
// is NO actor field — the actor is the principal (D5) — and anything else is an unknown field.
type changeRecordRequest struct {
	Kind       *string `json:"kind"`
	Phase      *string `json:"phase"`
	OccurredAt *string `json:"occurred_at"`
	Source     *string `json:"source"`
	ExternalID *string `json:"external_id"`
	Ref        *string `json:"ref"`
	URL        *string `json:"url"`
	DecisionID *string `json:"decision_id"`
}

// changePhaseView is one phase row as every change response nests it: the row id, the phase,
// its instant, the bounded `ref`/`url`, the decision link when one was given, and the actor
// triple of D5 — `actor_user_id` is ALWAYS present and null for a token, as the gate's override
// record spells it.
type changePhaseView struct {
	ID          string             `json:"id"`
	Phase       domain.ChangePhase `json:"phase"`
	OccurredAt  time.Time          `json:"occurred_at"`
	Ref         string             `json:"ref"`
	URL         string             `json:"url"`
	DecisionID  *string            `json:"decision_id,omitempty"`
	ActorLabel  string             `json:"actor_label"`
	ActorUserID *string            `json:"actor_user_id"`
	ViaToken    bool               `json:"via_token"`
	RecordedAt  time.Time          `json:"recorded_at"`
}

// changeRowView is a phase row WITH its identity — the record response and the incident route's
// anchored change, where the row stands alone and must say which service and which change.
type changeRowView struct {
	ServiceID  string            `json:"service_id"`
	Source     string            `json:"source"`
	ExternalID string            `json:"external_id"`
	Kind       domain.ChangeKind `json:"kind"`
	changePhaseView
}

func newChangePhaseView(r domain.ChangePhaseRow) changePhaseView {
	return changePhaseView{
		ID: r.ID, Phase: r.Phase, OccurredAt: r.OccurredAt, Ref: r.Ref, URL: r.URL, DecisionID: r.DecisionID,
		ActorLabel: r.ActorLabel, ActorUserID: r.ActorUserID, ViaToken: r.ViaToken, RecordedAt: r.RecordedAt,
	}
}

func newChangeRowView(r domain.ChangePhaseRow) changeRowView {
	return changeRowView{ServiceID: r.ServiceID, Source: r.Source, ExternalID: r.ExternalID, Kind: r.Kind, changePhaseView: newChangePhaseView(r)}
}

// changeRecordResponse is POST …/changes' answer, the same shape for 201 (recorded) and 200
// (an identical replay, D3): `replayed` says which, `change` is the row — the ORIGINAL row on a
// replay, original actor and recorded_at.
type changeRecordResponse struct {
	Replayed bool          `json:"replayed"`
	Change   changeRowView `json:"change"`
}

// changeDecisionView is a group's decision link (D11), presence by state: a LIVE ledger row is
// `{decision_id, state, action?, overridden}` — `action` absent for a NOT_CONFIGURED decision,
// as the gate's own response — and an aged-out row is `{decision_id, aged_out: true}`.
type changeDecisionView struct {
	DecisionID string             `json:"decision_id"`
	State      *domain.GateState  `json:"state,omitempty"`
	Action     *domain.GateAction `json:"action,omitempty"`
	Overridden *bool              `json:"overridden,omitempty"`
	AgedOut    bool               `json:"aged_out,omitempty"`
}

func newChangeDecisionView(l domain.ChangeDecisionLink) changeDecisionView {
	if l.AgedOut {
		return changeDecisionView{DecisionID: l.ID, AgedOut: true}
	}
	overridden := l.Overridden
	return changeDecisionView{DecisionID: l.ID, State: l.State, Action: l.Action, Overridden: &overridden}
}

// changeIncidentLinkView is one incident the group PRECEDED (D7, the change side of the link
// table): never "caused".
type changeIncidentLinkView struct {
	IncidentID string    `json:"incident_id"`
	OpenedAt   time.Time `json:"opened_at"`
	Role       string    `json:"role"`
	LagSeconds int64     `json:"lag_seconds"`
	ChangeID   string    `json:"change_id"`
}

// changeGroupView is one timeline item (D6): the identity, the kind, `ref`/`url` of the LATEST
// phase, the group key's instant, every phase nested in the domain's order, the decision link
// ABSENT when none was given, and `incidents` ALWAYS an array.
type changeGroupView struct {
	Source           string                   `json:"source"`
	ExternalID       string                   `json:"external_id"`
	Kind             domain.ChangeKind        `json:"kind"`
	Ref              string                   `json:"ref"`
	URL              string                   `json:"url"`
	LatestOccurredAt time.Time                `json:"latest_occurred_at"`
	Phases           []changePhaseView        `json:"phases"`
	Decision         *changeDecisionView      `json:"decision,omitempty"`
	Incidents        []changeIncidentLinkView `json:"incidents"`
}

func newChangeGroupView(g domain.ChangeGroup) changeGroupView {
	v := changeGroupView{
		Source: g.Source, ExternalID: g.ExternalID, Kind: g.Kind, LatestOccurredAt: g.LatestOccurredAt,
		Phases: make([]changePhaseView, 0, len(g.Phases)), Incidents: make([]changeIncidentLinkView, 0, len(g.Incidents)),
	}
	for _, p := range g.Phases {
		v.Phases = append(v.Phases, newChangePhaseView(p))
	}
	if n := len(g.Phases); n > 0 {
		v.Ref, v.URL = g.Phases[n-1].Ref, g.Phases[n-1].URL
	}
	if g.Decision != nil {
		d := newChangeDecisionView(*g.Decision)
		v.Decision = &d
	}
	for _, l := range g.Incidents {
		v.Incidents = append(v.Incidents, changeIncidentLinkView{
			IncidentID: l.IncidentID, OpenedAt: l.OpenedAt, Role: l.Role, LagSeconds: l.LagSeconds, ChangeID: l.ChangeID,
		})
	}
	return v
}

// changeGroupListResponse is the timeline page (D6): `next_cursor` is null on the last page.
type changeGroupListResponse struct {
	Items      []changeGroupView `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// changeCompareSideView is one side of the comparison (D8), exactly one of three shapes over
// its `[from, to)`: the FIGURE (`availability`, `good_seconds`, `bad_seconds`, `unknown_seconds`,
// `excluded_seconds`, `buckets`), WITHHELD (`withheld` naming the reason, `detail` carrying the
// reliability page's own reason string when the reason is `undecidable`), or PENDING —
// either side whose end lies past sealed_through (D-0211) — `pending: true` with
// `sealed_through` stated and NO partial figure.
type changeCompareSideView struct {
	From            time.Time  `json:"from"`
	To              time.Time  `json:"to"`
	Availability    *float64   `json:"availability,omitempty"`
	GoodSeconds     *float64   `json:"good_seconds,omitempty"`
	BadSeconds      *float64   `json:"bad_seconds,omitempty"`
	UnknownSeconds  *float64   `json:"unknown_seconds,omitempty"`
	ExcludedSeconds *float64   `json:"excluded_seconds,omitempty"`
	Buckets         *int64     `json:"buckets,omitempty"`
	Withheld        string     `json:"withheld,omitempty"`
	Detail          string     `json:"detail,omitempty"`
	Pending         bool       `json:"pending,omitempty"`
	SealedThrough   *time.Time `json:"sealed_through,omitempty"`
}

// changeSidePending reports whether a side is withheld as `pending` — its end lies past
// sealed_through. Either side may be (D-0211); the wire shape is the same for both.
func changeSidePending(s domain.ChangeCompareSide) bool {
	return s.Withheld != nil && s.Withheld.Reason == domain.ChangeCompareWithheldPending
}

func newChangeCompareSideView(s domain.ChangeCompareSide) changeCompareSideView {
	v := changeCompareSideView{From: s.From, To: s.To}
	switch {
	case s.Figure != nil:
		f := *s.Figure
		v.Availability, v.GoodSeconds, v.BadSeconds = &f.Availability, &f.GoodSeconds, &f.BadSeconds
		v.UnknownSeconds, v.ExcludedSeconds, v.Buckets = &f.UnknownSeconds, &f.ExcludedSeconds, &f.Buckets
	case changeSidePending(s):
		v.Pending, v.SealedThrough = true, s.Withheld.SealedThrough
	case s.Withheld != nil:
		v.Withheld, v.Detail = s.Withheld.Reason, s.Withheld.Detail
	}
	return v
}

// changeCompareView is GET …/changes/compare (D8): the identity, the terminal phase the
// comparison rests on, T (floored to the canonical bucket), the horizon in the request's
// spelling, `sealed_through` (present, null when the service has sealed nothing), the snapshot
// instant, both sides, and `delta` (after − before, availability points) ONLY when both sides
// are figures.
type changeCompareView struct {
	Source        string                `json:"source"`
	ExternalID    string                `json:"external_id"`
	Kind          domain.ChangeKind     `json:"kind"`
	Ref           string                `json:"ref"`
	ChangeID      string                `json:"change_id"`
	TerminalPhase domain.ChangePhase    `json:"terminal_phase"`
	T             time.Time             `json:"t"`
	Horizon       string                `json:"horizon"`
	SealedThrough *time.Time            `json:"sealed_through"`
	AsOf          time.Time             `json:"as_of"`
	Before        changeCompareSideView `json:"before"`
	After         changeCompareSideView `json:"after"`
	Delta         *float64              `json:"delta,omitempty"`
}

func newChangeCompareView(c domain.ChangeComparison, horizon string) changeCompareView {
	return changeCompareView{
		Source: c.Source, ExternalID: c.ExternalID, Kind: c.Change.Kind, Ref: c.Change.Ref,
		ChangeID: c.Change.ID, TerminalPhase: c.Change.Phase, T: c.T, Horizon: horizon,
		SealedThrough: c.SealedThrough, AsOf: c.AsOf,
		Before: newChangeCompareSideView(c.Before), After: newChangeCompareSideView(c.After), Delta: c.Delta,
	}
}

// incidentChangeView is one link from the incident side (D7): the anchored phase with its copied
// instant and lag, and the group's CURRENT phases read live beside it.
type incidentChangeView struct {
	Change     changeRowView     `json:"change"`
	Role       string            `json:"role"`
	OccurredAt time.Time         `json:"occurred_at"`
	LagSeconds int64             `json:"lag_seconds"`
	ComputedAt time.Time         `json:"computed_at"`
	Phases     []changePhaseView `json:"phases"`
}

type incidentChangesResponse struct {
	Items []incidentChangeView `json:"items"`
}

// changeListDefaultLimit is the timeline page size when `limit` is absent (D6).
const changeListDefaultLimit = 50

// changeErrRangeRequired is D6's code for a timeline read without an explicit `[from, to)`. It
// is the transport's refusal — the store never sees a missing range — so it lives here beside
// the handler that raises it, as the gate's `range_required` does.
const changeErrRangeRequired = "range_required"

// changeErrBodyInvalid is the reason a record refusal WITHOUT a closed code is counted under —
// an unknown field, a missing field, a wrong type, an unparseable `occurred_at` (D15).
const changeErrBodyInvalid = "body_invalid"

// changeHorizons is the closed horizon vocabulary of D8 in the request's spelling, in order.
var changeHorizons = []struct {
	name string
	d    time.Duration
}{{"15m", 15 * time.Minute}, {"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour}}

func parseChangeHorizon(s string) (time.Duration, string, bool) {
	for _, h := range changeHorizons {
		if h.name == s {
			return h.d, h.name, true
		}
	}
	return 0, "", false
}

// ── path parameters, principal, bounds ───────────────────────────────────────────────────────

// incidentIDParam follows serviceIDParam's convention: transport owns the format, so a malformed
// id is 400 here and the store never sees a value PostgreSQL would refuse with a cast error.
func incidentIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("incidentID")
	if !serviceUUIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "incidentID must be a UUID")
		return "", false
	}
	return id, true
}

// changeAdmit takes the §5a bounds for this request — the record pool (permits and both rate
// buckets) when `record` is true, the read pool (permits only) otherwise — and writes the 429
// with its Retry-After when refused. Without a wired limiter the routes answer 501: a record
// route with no bound is the load §5a exists to shed.
func (h *Handler) changeAdmit(w http.ResponseWriter, r *http.Request, record bool) (func(), bool) {
	if h.change == nil {
		writeError(w, http.StatusNotImplemented, "change_not_wired")
		return nil, false
	}
	p, _ := h.principal(r)
	var release func()
	var refusal *gateRefusal
	if record {
		release, refusal = h.change.acquireRecord(gatePrincipalKey(p))
	} else {
		release, refusal = h.change.acquireRead(gatePrincipalKey(p))
	}
	if refusal != nil {
		if record {
			h.recordChangeRejected(refusal.reason)
		}
		w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfter))
		writeError(w, http.StatusTooManyRequests, refusal.reason)
		return nil, false
	}
	return release, true
}

func (h *Handler) recordChangeRejected(reason string) {
	if h.changeMetrics == nil {
		return
	}
	if err := h.changeMetrics.RecordChangeRecordRejected(reason); err != nil {
		h.logger.Error("change_metric", "op", "rejected", "error", err.Error())
	}
}

func (h *Handler) recordChangeRecorded(row domain.ChangePhaseRow, replayed bool) {
	if h.changeMetrics == nil {
		return
	}
	if err := h.changeMetrics.RecordChangeRecorded(string(row.Kind), string(row.Phase), replayed); err != nil {
		h.logger.Error("change_metric", "op", "recorded", "error", err.Error())
	}
}

func (h *Handler) recordChangeCompare(c domain.ChangeComparison) {
	if h.changeMetrics == nil {
		return
	}
	// D-0211: EITHER side may be `pending` (its end past sealed_through); pending outranks any
	// other withholding, which outranks two figures.
	outcome := "figure"
	switch {
	case changeSidePending(c.Before) || changeSidePending(c.After):
		outcome = "pending"
	case c.Before.Withheld != nil || c.After.Withheld != nil:
		outcome = "withheld"
	}
	if err := h.changeMetrics.RecordChangeCompare(outcome); err != nil {
		h.logger.Error("change_metric", "op", "compare", "error", err.Error())
	}
}

// ── error mapping ────────────────────────────────────────────────────────────────────────────

// changeErrorStatus maps a closed code onto its HTTP answer: the order and identity conflicts
// are 409, a change without a terminal phase is 404 (the comparison does not exist yet), every
// other refusal is 400.
func changeErrorStatus(code string) int {
	switch code {
	case domain.ChangeErrPhaseOrder, domain.ChangeErrPhaseExists, domain.ChangeErrKindMismatch:
		return http.StatusConflict
	case domain.ChangeErrNoTerminalPhase:
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// writeChangeError maps a change store error onto its answer and returns the closed code it was
// refused with ("" for a tenant 404 or a server error) and whether it wrote a response. A
// *domain.ChangeError carries the code, the field and the rule; the body is its own rendering.
func (h *Handler) writeChangeError(w http.ResponseWriter, op string, err error) (string, bool) {
	var changeErr *domain.ChangeError
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return "", true
	case errors.As(err, &changeErr):
		writeError(w, changeErrorStatus(changeErr.Code), changeErr.Error())
		return changeErr.Code, true
	default:
		h.serverError(w, op, err)
		return "", true
	}
}

// ── the record ───────────────────────────────────────────────────────────────────────────────

// changeRecordReject writes a 400/409 for the record route and counts it under its code.
func (h *Handler) changeRecordReject(w http.ResponseWriter, code, msg string) {
	h.recordChangeRejected(code)
	writeError(w, changeErrorStatus(code), msg)
}

// normalizeChangeField is the transport's half of D2 for one optional body field: NFC + trim of
// the value when present, "" when absent.
func normalizeChangeField(s *string) string {
	if s == nil {
		return ""
	}
	return domain.NormalizeChangeText(*s)
}

// recordChange is POST …/services/{s}/changes (D2, D3, D5): authorization, the path, the strict
// body, normalization, the domain's validators on the canonical values, the §5a bounds, then ONE
// store call under the identity lock. 201 for a new row, 200 for an identical replay.
func (h *Handler) recordChange(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionChangeRecord)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	if h.change == nil {
		// The clock bounds below are the limiter's; without WithChange there is nothing to bound
		// the record with, and the route answers as changeAdmit would.
		writeError(w, http.StatusNotImplemented, "change_not_wired")
		return
	}
	var req changeRecordRequest
	if !gateDecodeBody(w, r, &req) {
		h.recordChangeRejected(changeErrBodyInvalid)
		return
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"kind", req.Kind != nil},
		{"phase", req.Phase != nil},
		{"source", req.Source != nil},
		{"external_id", req.ExternalID != nil},
	} {
		if !f.present {
			h.changeRecordReject(w, changeErrBodyInvalid, f.name+": is required")
			return
		}
	}
	// Normalize EVERY text field before anything validates it (D2: the transport's layer).
	in := store.RecordChangeInput{
		ProjectID:  proj.ID,
		ServiceID:  serviceID,
		Kind:       domain.ChangeKind(normalizeChangeField(req.Kind)),
		Phase:      domain.ChangePhase(normalizeChangeField(req.Phase)),
		Source:     normalizeChangeField(req.Source),
		ExternalID: normalizeChangeField(req.ExternalID),
		Ref:        normalizeChangeField(req.Ref),
		URL:        normalizeChangeField(req.URL),
		Actor:      h.gateActor(r),
		MaxPast:    h.change.limits.MaxPast,
		MaxFuture:  h.change.limits.MaxFuture,
	}
	if req.DecisionID != nil {
		id := domain.NormalizeChangeText(*req.DecisionID)
		in.DecisionID = &id
	}
	if req.OccurredAt == nil {
		in.OccurredAt = time.Now().UTC()
	} else {
		t, err := time.Parse(time.RFC3339Nano, domain.NormalizeChangeText(*req.OccurredAt))
		if err != nil {
			h.changeRecordReject(w, changeErrBodyInvalid, "occurred_at: must be an RFC3339 timestamp")
			return
		}
		in.OccurredAt = t.UTC()
	}
	// The domain's validators on the canonical values (D2: the domain is the ONE Unicode
	// authority); a refusal here costs no rate token. The store runs them again before SQL.
	if err := validateChangeRecord(in); err != nil {
		h.changeRecordReject(w, err.Code, err.Error())
		return
	}
	release, ok := h.changeAdmit(w, r, true)
	if !ok {
		return
	}
	defer release()

	row, replayed, err := h.store.RecordChangePhase(r.Context(), in)
	if code, wrote := h.writeChangeError(w, "change_record", err); wrote {
		if code != "" {
			h.recordChangeRejected(code)
		}
		return
	}
	h.recordChangeRecorded(row, replayed)
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, changeRecordResponse{Replayed: replayed, Change: newChangeRowView(row)})
}

// validateChangeRecord runs D2's domain validators over a normalized record, in the body's field
// order, and returns the first refusal.
func validateChangeRecord(in store.RecordChangeInput) *domain.ChangeError {
	if !domain.ValidChangeKind(in.Kind) {
		return domain.NewChangeError(domain.ChangeErrKindInvalid, "kind", "must be one of deploy|rollback|flag, got %q", in.Kind)
	}
	if !domain.ValidChangePhase(in.Phase) {
		return domain.NewChangeError(domain.ChangeErrPhaseInvalid, "phase", "must be one of started|succeeded|failed|cancelled, got %q", in.Phase)
	}
	var changeErr *domain.ChangeError
	for _, check := range []func() error{
		func() error { return domain.ValidateChangeSource(in.Source) },
		func() error { _, err := domain.ValidateChangeExternalID(in.ExternalID); return err },
		func() error { _, err := domain.ValidateChangeRef(in.Ref); return err },
		func() error { _, err := domain.ValidateChangeURL(in.URL); return err },
	} {
		if err := check(); err != nil {
			if errors.As(err, &changeErr) {
				return changeErr
			}
			return domain.NewChangeError(changeErrBodyInvalid, "", "%s", err.Error())
		}
	}
	return nil
}

// ── the timeline ─────────────────────────────────────────────────────────────────────────────

// listChanges is GET …/services/{s}/changes (D6): `from`/`to` required, half-open, at most 92
// days; `limit` 1..200 defaulting to 50; `kind` repeatable (a set, OR); `source` one slug; an
// opaque keyset `cursor`. Reads take the read permits of §5a, no rate token.
func (h *Handler) listChanges(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	fromS, toS := q.Get("from"), q.Get("to")
	if fromS == "" || toS == "" {
		writeError(w, http.StatusBadRequest, changeErrRangeRequired)
		return
	}
	from, errFrom := time.Parse(time.RFC3339Nano, fromS)
	to, errTo := time.Parse(time.RFC3339Nano, toS)
	if errFrom != nil || errTo != nil || !to.After(from) {
		writeError(w, http.StatusBadRequest, domain.ChangeErrRangeInvalid)
		return
	}
	if to.Sub(from) > store.ChangeRangeMax {
		writeError(w, http.StatusBadRequest, domain.ChangeErrRangeTooWide)
		return
	}
	limit := changeListDefaultLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > store.ChangeListLimitMax {
			writeError(w, http.StatusBadRequest, domain.ChangeErrLimitInvalid)
			return
		}
		limit = n
	}
	var kinds []domain.ChangeKind
	if raw := q["kind"]; len(raw) > 0 {
		seen := make(map[domain.ChangeKind]bool, len(raw))
		for _, v := range raw {
			k := domain.ChangeKind(domain.NormalizeChangeText(v))
			if !domain.ValidChangeKind(k) {
				writeError(w, http.StatusBadRequest, domain.ChangeErrKindInvalid+": "+strconv.Quote(v))
				return
			}
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, k)
			}
		}
	}
	var source *string
	if q.Has("source") {
		s := domain.NormalizeChangeText(q.Get("source"))
		if err := domain.ValidateChangeSource(s); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		source = &s
	}
	var cursor *store.ChangeCursor
	if q.Has("cursor") {
		c, err := store.DecodeChangeCursor(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, domain.ChangeErrCursorInvalid)
			return
		}
		cursor = &c
	}
	release, ok := h.changeAdmit(w, r, false)
	if !ok {
		return
	}
	defer release()
	groups, next, err := h.store.ListChangeGroups(r.Context(), proj.ID, serviceID, from.UTC(), to.UTC(), kinds, source, cursor, limit)
	if _, wrote := h.writeChangeError(w, "change_list", err); wrote {
		return
	}
	resp := changeGroupListResponse{Items: make([]changeGroupView, 0, len(groups))}
	for _, g := range groups {
		resp.Items = append(resp.Items, newChangeGroupView(g))
	}
	if next != nil {
		enc := next.Encode()
		resp.NextCursor = &enc
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── the comparison ───────────────────────────────────────────────────────────────────────────

// compareChange is GET …/services/{s}/changes/compare?source&external_id&horizon (D8): the
// identity is normalized as a body field would be, `horizon` is one of the four (default 1h),
// and the store answers from ONE snapshot — a started-only group is 404 `no_terminal_phase`, an
// unknown identity or a foreign service 404. Nothing is stored or cached.
func (h *Handler) compareChange(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	source := domain.NormalizeChangeText(q.Get("source"))
	if err := domain.ValidateChangeSource(source); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	externalID, err := domain.ValidateChangeExternalID(domain.NormalizeChangeText(q.Get("external_id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	horizon, horizonName := domain.ChangeCompareDefaultHorizon, "1h"
	if q.Has("horizon") {
		d, name, ok := parseChangeHorizon(q.Get("horizon"))
		if !ok {
			writeError(w, http.StatusBadRequest, domain.ChangeErrHorizonInvalid+": must be one of 15m|1h|6h|24h, got "+strconv.Quote(q.Get("horizon")))
			return
		}
		horizon, horizonName = d, name
	}
	release, ok := h.changeAdmit(w, r, false)
	if !ok {
		return
	}
	defer release()
	cmp, err := h.store.ServiceReliabilityCompare(r.Context(), proj.ID, serviceID, source, externalID, horizon)
	if _, wrote := h.writeChangeError(w, "change_compare", err); wrote {
		return
	}
	h.recordChangeCompare(cmp)
	writeJSON(w, http.StatusOK, newChangeCompareView(cmp, horizonName))
}

// ── the incident side ────────────────────────────────────────────────────────────────────────

// listIncidentChanges is GET …/projects/{p}/incidents/{i}/changes (D7, invariant 9): the same
// link rows the timeline's `incidents[]` comes from, read from the incident side, scoped to the
// caller's project at the link rows — an incident of another project is 404.
func (h *Handler) listIncidentChanges(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	incidentID, ok := incidentIDParam(w, r)
	if !ok {
		return
	}
	release, ok := h.changeAdmit(w, r, false)
	if !ok {
		return
	}
	defer release()
	links, err := h.store.ListIncidentChanges(r.Context(), proj.ID, incidentID)
	if _, wrote := h.writeChangeError(w, "incident_changes", err); wrote {
		return
	}
	resp := incidentChangesResponse{Items: make([]incidentChangeView, 0, len(links))}
	for _, l := range links {
		v := incidentChangeView{
			Change: newChangeRowView(l.Change), Role: l.Role, OccurredAt: l.OccurredAt, LagSeconds: l.LagSeconds,
			ComputedAt: l.ComputedAt, Phases: make([]changePhaseView, 0, len(l.Phases)),
		}
		for _, p := range l.Phases {
			v.Phases = append(v.Phases, newChangePhaseView(p))
		}
		resp.Items = append(resp.Items, v)
	}
	writeJSON(w, http.StatusOK, resp)
}
