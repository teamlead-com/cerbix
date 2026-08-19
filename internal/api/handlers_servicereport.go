package api

import (
	"net/http"
	"regexp"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
)

// FR-021 phase 2, changeset 2 (iter-0141): the reporting API. Every endpoint returns the
// REVIEWED domain payloads of iter-0138…0140 verbatim — no API-side re-mapping exists to
// drift from §11.2's response contract, §12.1's segmentation rules or D-0164's live-signal
// semantics. A nonexistent or foreign service is 404 from the store's own tenant check —
// never a no_sli-shaped 200 that doubles as an existence oracle.

// serviceUUIDPattern is strict 8-4-4-4-12 hex: transport owns request format, so a
// malformed ID is 400 HERE — the store must never see a value PostgreSQL rejects with a
// uuid cast error that would surface as 500 — while a well-formed but absent UUID is the
// store's honest 404.
var serviceUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func serviceIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("serviceID")
	if !serviceUUIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "serviceID must be a UUID")
		return "", false
	}
	return id, true
}

// serviceReliability returns the §11 window report. The window is one of the product's
// standard names (24h/7d/30d/90d); anything else is 400 — arbitrary windows would be
// arbitrary scans.
func (h *Handler) serviceReliability(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	name := r.URL.Query().Get("window")
	if name == "" {
		name = "30d"
	}
	window, valid := sla.WindowByName(name)
	if !valid {
		writeError(w, http.StatusBadRequest, "unknown window")
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	rep, err := h.store.ServiceReliabilityReport(r.Context(), proj.ID, serviceID, window)
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// serviceHealth returns the categorical live signal (§11.3, D-0164) — deliberately its own
// endpoint: it is "a different named thing" than the percentage report and must never look
// like a field of it.
func (h *Handler) serviceHealth(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	health, err := h.store.ServiceHealthNow(r.Context(), proj.ID, serviceID)
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// seriesMaxSpan bounds one series request at the longest supported window (§10.10's
// StandardWindows top out at 90d): the response size is the contract, not the client's
// restraint.
const seriesMaxSpan = 90 * 24 * time.Hour

type serviceSeriesResponse struct {
	From   time.Time                       `json:"from"`
	To     time.Time                       `json:"to"`
	Step   string                          `json:"step"`
	Points []domain.ReliabilitySeriesPoint `json:"points"`
}

// serviceReliabilitySeries returns the on-read hour/day rollups (§10.2, §12.1): exact
// epoch-keyed sums, never merged across an epoch boundary, provisional time rolled up
// separately.
func (h *Handler) serviceReliabilitySeries(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from must be RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to must be RFC3339")
		return
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "to must be after from")
		return
	}
	if to.Sub(from) > seriesMaxSpan {
		writeError(w, http.StatusBadRequest, "range exceeds the longest supported window (90d)")
		return
	}
	var step time.Duration
	switch q.Get("step") {
	case "hour":
		step = time.Hour
	case "day":
		step = 24 * time.Hour
	default:
		writeError(w, http.StatusBadRequest, "step must be hour or day")
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	points, err := h.store.ServiceReliabilitySeries(r.Context(), proj.ID, serviceID, from, to, step)
	if h.writeServiceError(w, err) {
		return
	}
	if points == nil {
		// The wire contract is a non-null array ([192] P2-1), independent of any store's
		// nil-vs-empty habits.
		points = []domain.ReliabilitySeriesPoint{}
	}
	writeJSON(w, http.StatusOK, serviceSeriesResponse{
		From: from.UTC(), To: to.UTC(), Step: q.Get("step"), Points: points,
	})
}

// setServiceSLATarget sets the service-scoped objective for one standard window. The body is
// {window, objective} and NOTHING else, and it stays that way now that phase 5 has landed:
// service burn alerting is expressible, but on its own route
// (`PUT …/sla-target/burn-alerting`, handlers_servicealerting.go), because the objective and
// the burn declaration are two store transactions with two audit actions and two families of
// lifecycle close. Accepting burn fields HERE would have made a single request that no layer
// can commit atomically, so `DisallowUnknownFields` still turns any of them into a 400 — the
// refusal now means "wrong endpoint", not "not implemented".
func (h *Handler) setServiceSLATarget(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Window    string  `json:"window"`
		Objective float64 `json:"objective"`
	}
	if !decodeJSONBody(w, r, serviceMaxBody, &body) {
		return
	}
	if body.Window == "" {
		body.Window = "30d"
	}
	if _, valid := sla.WindowByName(body.Window); !valid {
		writeError(w, http.StatusBadRequest, "unknown window")
		return
	}
	objective, err := domain.CanonicalObjective(body.Objective)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	if err := h.store.UpsertServiceSLATarget(r.Context(), proj.ID, serviceID, body.Window, objective); h.writeServiceError(w, err) {
		return
	}
	// The echo IS the canonical stored value: numeric(7,4) holds every 4-decimal value in
	// the open (0,100) (D-0165), so this number and the row are the same number.
	writeJSON(w, http.StatusOK, map[string]any{
		"window": body.Window, "objective": objective,
	})
}
