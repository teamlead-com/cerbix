package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// incidentAccess loads an incident and checks the principal may act on its
// project. Writes 404 (hidden)/403 and returns ok=false on failure.
func (h *Handler) incidentAccess(w http.ResponseWriter, r *http.Request, action authz.Action) (domain.Incident, bool) {
	inc, err := h.store.GetIncident(r.Context(), r.PathValue("incidentID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Incident{}, false
	}
	if err != nil {
		h.serverError(w, "get_incident", err)
		return domain.Incident{}, false
	}
	if _, ok := h.projectAccess(w, r, inc.ProjectID, action); !ok {
		return domain.Incident{}, false
	}
	return inc, true
}

// listIncidents lists a project's incidents (newest first).
func (h *Handler) listIncidents(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	incidents, err := h.store.ListIncidentsByProject(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_incidents", err)
		return
	}
	writeJSON(w, http.StatusOK, incidents)
}

// createIncident opens an incident with an opening timeline update (editor+).
// The source is manual for a cookie session; API-token and auto sources arrive
// with FR-012 phase 2.
func (h *Handler) createIncident(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	p, _ := h.principal(r)
	var body struct {
		Title     string `json:"title"`
		Status    string `json:"status"`
		Impact    string `json:"impact"`
		Body      string `json:"body"`
		MonitorID string `json:"monitor_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Status == "" {
		body.Status = string(domain.IncidentInvestigating)
	}
	if body.Impact == "" {
		body.Impact = string(domain.ImpactMinor)
	}
	// An optional affected monitor must belong to this project.
	if body.MonitorID != "" {
		mon, err := h.store.GetMonitor(r.Context(), body.MonitorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && mon.ProjectID != proj.ID) {
			writeError(w, http.StatusBadRequest, "monitor is not in this project")
			return
		}
		if err != nil {
			h.serverError(w, "get_monitor", err)
			return
		}
	}
	source := domain.SourceManual
	if p.ViaToken {
		source = domain.SourceAPI
	}
	inc := domain.Incident{
		ProjectID: proj.ID,
		MonitorID: body.MonitorID,
		Title:     body.Title,
		Status:    domain.IncidentStatus(body.Status),
		Impact:    domain.IncidentImpact(body.Impact),
		Source:    source,
	}
	if err := inc.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateIncident(r.Context(), inc, body.Body, p.UserID)
	if err != nil {
		h.serverError(w, "create_incident", err)
		return
	}
	h.logEvent(r, "incident_opened", "incident_id", created.ID, "title", created.Title, "impact", string(created.Impact), "source", string(created.Source), "project_id", created.ProjectID)
	if h.metrics != nil {
		h.metrics.RecordIncidentOpened()
	}
	// The webhook event is enqueued transactionally by CreateIncident; the outbox
	// worker delivers it.
	writeJSON(w, http.StatusCreated, created)
}

// authedIncidentView is the AUTHENTICATED incident detail (FR-021 §14.4):
// the incident plus its impact links. Impacts are deliberately NOT a field of
// domain.Incident — the public status-page serializer embeds that model and
// redacts by reverse allowlist, so a shared field would ride into
// unauthenticated JSON the moment one future redactor forgot it (invariant 59).
type authedIncidentView struct {
	domain.Incident
	// Impacts is null — never [] — when the links could not be READ ([288] P1-4):
	// an empty array is the honest statement "this incident has no links", and a
	// failed read must not borrow it. impacts_unavailable says which one happened,
	// so a client can render "unknown" instead of "none".
	Impacts            []domain.ServiceImpactLink `json:"impacts"`
	ImpactsUnavailable bool                       `json:"impacts_unavailable,omitempty"`
}

// getIncident returns one incident with its impact links (authenticated detail
// only; the incident LIST carries no impacts — §14.7, invariant 60).
func (h *Handler) getIncident(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	// Resolve the acknowledging actor for display; best-effort (the user may be gone).
	if inc.AcknowledgedBy != "" {
		if u, err := h.store.GetUser(r.Context(), inc.AcknowledgedBy); err == nil {
			inc.AcknowledgedByName = u.DisplayName
			if inc.AcknowledgedByName == "" {
				inc.AcknowledgedByName = u.Email
			}
		}
	}
	out := authedIncidentView{Incident: inc, Impacts: []domain.ServiceImpactLink{}}
	// The incident itself must still render when the impact read fails — but the
	// failure is DISCLOSED, not disguised as "no impacts" ([288] P1-4).
	// Tenant-scoped at the store boundary on top of incidentAccess ([276] P0-1).
	impacts, err := h.store.ListIncidentImpacts(r.Context(), inc.ProjectID, inc.ID)
	switch {
	case err != nil:
		h.logEvent(r, "incident_impacts_read_failed", "incident_id", inc.ID, "error", err.Error())
		out.Impacts = nil
		out.ImpactsUnavailable = true
	case impacts != nil:
		out.Impacts = impacts
	}
	writeJSON(w, http.StatusOK, out)
}

// listIncidentUpdates returns an incident's timeline (chronological).
func (h *Handler) listIncidentUpdates(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	updates, err := h.store.ListIncidentUpdates(r.Context(), inc.ID)
	if err != nil {
		h.serverError(w, "list_incident_updates", err)
		return
	}
	writeJSON(w, http.StatusOK, updates)
}

// addIncidentUpdate appends a timeline update and advances the incident's status
// (editor+). A resolved incident is terminal and rejects further updates.
func (h *Handler) addIncidentUpdate(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if inc.Status.Terminal() {
		writeError(w, http.StatusBadRequest, "incident is resolved")
		return
	}
	p, _ := h.principal(r)
	var body struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Status == "" {
		body.Status = string(inc.Status) // a plain comment keeps the current status
	}
	upd := domain.IncidentUpdate{
		IncidentID: inc.ID,
		Status:     domain.IncidentStatus(body.Status),
		Body:       body.Body,
		Author:     p.UserID,
	}
	if err := upd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.AddIncidentUpdate(r.Context(), upd)
	if err != nil {
		h.serverError(w, "add_incident_update", err)
		return
	}
	h.logEvent(r, "incident_update_posted", "incident_id", upd.IncidentID, "status", string(created.Status))
	// The lifecycle event (updated/resolved) is enqueued transactionally by
	// AddIncidentUpdate; the outbox worker delivers it.
	writeJSON(w, http.StatusCreated, created)
}

// getPostmortem returns an incident's postmortem, or 404 if none is published.
func (h *Handler) getPostmortem(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	pm, err := h.store.GetPostmortem(r.Context(), inc.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_postmortem", err)
		return
	}
	writeJSON(w, http.StatusOK, pm)
}

// putPostmortem attaches or replaces the postmortem for a resolved incident
// (editor+). GUI and API callers share this path.
func (h *Handler) putPostmortem(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if inc.Status != domain.IncidentResolved {
		writeError(w, http.StatusBadRequest, "postmortem requires a resolved incident")
		return
	}
	p, _ := h.principal(r)
	var body struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	pm, err := h.store.UpsertPostmortem(r.Context(), inc.ID, body.Body, p.UserID)
	if err != nil {
		h.serverError(w, "upsert_postmortem", err)
		return
	}
	writeJSON(w, http.StatusOK, pm)
}
