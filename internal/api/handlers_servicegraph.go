package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-021 phase 3 §14.4: the dedicated edge routes. Edges are outside the
// declaration axes, so they carry their OWN concurrency token
// (graph_generation) instead of expected_revision, and their own typed audit
// actor. One validator, one mutator with the format-2 edge track.

// serviceEdgeView is one neighbour with the phase-2 two-layer health signal,
// reused verbatim — evaluated for the WHOLE neighbour set in one report
// snapshot, never a per-service loop (§14.7, invariant 60).
type serviceEdgeView struct {
	ID     string                   `json:"id"`
	Slug   string                   `json:"slug"`
	Name   string                   `json:"name"`
	Health *domain.ServiceHealthNow `json:"health,omitempty"`
}

// serviceGraphView is the edge set plus its concurrency token. The token and
// the sets come from ONE SQL snapshot in the store ([276] P2-1), so the token
// always names the returned set.
type serviceGraphView struct {
	GraphGeneration int64             `json:"graph_generation"`
	DependsOn       []serviceEdgeView `json:"depends_on"`
	DependedOnBy    []serviceEdgeView `json:"depended_on_by"`
}

// buildServiceGraphView enriches a store view with the batched neighbour
// health. Health is best-effort presentation: a health read failure degrades
// the pill to absent rather than failing the whole read.
func (h *Handler) buildServiceGraphView(r *http.Request, projectID string, v store.ServiceGraphView) serviceGraphView {
	ids := make([]string, 0, len(v.DependsOn)+len(v.DependedOnBy))
	for _, e := range v.DependsOn {
		ids = append(ids, e.ID)
	}
	for _, e := range v.DependedOnBy {
		ids = append(ids, e.ID)
	}
	health := map[string]domain.ServiceHealthNow{}
	if len(ids) > 0 {
		if m, err := h.store.ServiceNeighbourHealth(r.Context(), projectID, ids); err == nil {
			health = m
		} else {
			h.logEvent(r, "service_graph_health_failed", "error", err.Error())
		}
	}
	conv := func(es []store.ServiceEdge) []serviceEdgeView {
		out := make([]serviceEdgeView, 0, len(es))
		for _, e := range es {
			ev := serviceEdgeView{ID: e.ID, Slug: e.Slug, Name: e.Name}
			if hn, ok := health[e.ID]; ok {
				hh := hn
				ev.Health = &hh
			}
			out = append(out, ev)
		}
		return out
	}
	return serviceGraphView{
		GraphGeneration: v.GraphGeneration,
		DependsOn:       conv(v.DependsOn),
		DependedOnBy:    conv(v.DependedOnBy),
	}
}

// getServiceDependencies returns both edge directions with neighbour health
// and the graph_generation token the editor must echo back.
func (h *Handler) getServiceDependencies(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	v, err := h.store.GetServiceDependencies(r.Context(), proj.ID, serviceID)
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, h.buildServiceGraphView(r, proj.ID, v))
}

// putServiceDependenciesRequest is PRESENCE-aware ([288] P1-1): both fields are
// pointers because their absence is not their zero value — an omitted or null
// `depends_on` would otherwise read as "delete every edge", and an omitted
// `graph_generation` would read as 0 and pass the CAS on a fresh service. The
// contract the OpenAPI states (both required; `[]` is the legitimate way to
// clear the set) is therefore enforced, not assumed.
type putServiceDependenciesRequest struct {
	DependsOn       *[]string `json:"depends_on"`
	GraphGeneration *int64    `json:"graph_generation"`
}

// putServiceDependencies is the replace-set write (editor+): the CAS token is
// required, a stale one is 409 and first-committer-wins; validation errors
// come back as actionable 400s.
func (h *Handler) putServiceDependencies(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	var req putServiceDependenciesRequest
	if !decodeJSONBody(w, r, serviceMaxBody, &req) {
		return
	}
	if req.DependsOn == nil {
		writeError(w, http.StatusBadRequest, "depends_on is required (send [] to clear the set)")
		return
	}
	if req.GraphGeneration == nil {
		writeError(w, http.StatusBadRequest, "graph_generation is required (0 for a service with no edge writes yet)")
		return
	}
	if !validDependsOn(w, *req.DependsOn) {
		return
	}
	v, err := h.store.ReplaceServiceDependencies(r.Context(),
		proj.ID, serviceID, *req.DependsOn, *req.GraphGeneration, h.graphActor(r))
	if h.writeServiceGraphError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, h.buildServiceGraphView(r, proj.ID, v))
}

// validDependsOn is the ONE transport fence for dependency ids, called by BOTH
// the replace-set PUT and create-with-edges ([292]): transport owns request
// FORMAT, so a malformed or empty id must never reach the store's uuid-typed
// membership query — that is a pgx cast error surfacing as a 500, not the
// actionable 400 the contract promises. Writes the 400 itself and reports
// whether the handler may continue.
func validDependsOn(w http.ResponseWriter, ids []string) bool {
	for _, id := range ids {
		if !serviceUUIDPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, "depends_on entries must be UUIDs")
			return false
		}
	}
	return true
}

// graphActor is the typed audit attribution of §14.2: canonical user id when
// the principal is a real user, via_token for machine tokens, and the TRUSTED
// human-readable label the auth layer resolved (email / token name / client
// subject) — never a raw uuid ([288] P1-3).
func (h *Handler) graphActor(r *http.Request) store.GraphActor {
	p, ok := h.principal(r)
	if !ok {
		return store.GraphActor{}
	}
	return store.GraphActor{UserID: p.AuditUserID(), ViaToken: p.ViaToken, Label: p.AuditActorLabel()}
}

// writeServiceGraphError maps the graph-specific store errors; anything else
// falls through to writeServiceError. Kept separate so the generic service
// mapping does not grow cases only two handlers can produce.
func (h *Handler) writeServiceGraphError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrServiceGraphStale):
		writeError(w, http.StatusConflict, "graph_generation_stale: dependencies changed concurrently — reload and retry")
	case errors.Is(err, store.ErrServiceGraphCycle):
		writeError(w, http.StatusBadRequest, "dependency_cycle: the graph is a DAG")
	case errors.Is(err, store.ErrServiceGraphForeign):
		writeError(w, http.StatusBadRequest, "dependency_not_in_project")
	case errors.Is(err, store.ErrServiceGraphDepth):
		writeError(w, http.StatusBadRequest, "dependency_depth: chains are capped at 10 edges")
	case errors.Is(err, store.ErrServiceGraphLimit):
		writeError(w, http.StatusBadRequest, "too_many_dependencies: at most 20 direct dependencies")
	default:
		return h.writeServiceError(w, err)
	}
	return true
}
