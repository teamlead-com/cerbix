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
	if h.metrics != nil {
		h.metrics.RecordIncidentOpened()
	}
	// The webhook event is enqueued transactionally by CreateIncident; the outbox
	// worker delivers it.
	writeJSON(w, http.StatusCreated, created)
}

// getIncident returns one incident.
func (h *Handler) getIncident(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, inc)
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
