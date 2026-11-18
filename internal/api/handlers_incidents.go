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
	// FR-041a D21: each item carries its short reference. domain.Incident does not (P14), so the
	// list wraps it the way the detail does. A failed read leaves the references out — the list is
	// still the list.
	if len(incidents) > 0 {
		ids := make([]string, len(incidents))
		for i, inc := range incidents {
			ids[i] = inc.ID
		}
		refs, err := h.store.IncidentRefs(r.Context(), ids)
		if err != nil {
			h.logEvent(r, "incident_refs_read_failed", "project_id", proj.ID, "error", err.Error())
		}
		items := make([]incidentListItem, len(incidents))
		for i, inc := range incidents {
			items[i] = incidentListItem{Incident: inc, Ref: refs[inc.ID]}
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	writeJSON(w, http.StatusOK, incidents)
}

// incidentListItem is one row of the authenticated incident list: the incident and its short
// reference (FR-041a D21). The reference is deliberately NOT a field of domain.Incident, which the
// webhooks, the outbox payloads, the public projection and the feeds embed.
type incidentListItem struct {
	domain.Incident
	Ref string `json:"ref,omitempty"`
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
	created, err := h.store.CreateIncidentByPrincipal(r.Context(), inc, domain.ImpactBasisManual, body.Body, p.UserID, auditActor(p))
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
	// Members is the member set the incident's SERVICE had AT OPEN TIME (FR-022
	// invariant 13, spec D6). A POINTER because three states must stay distinguishable
	// and two of them are empty: no snapshot at all (a monitor or project-level
	// incident — the key is absent), a snapshot of a service that genuinely had no
	// members (`[]`), and a snapshot that could not be read (`members_unavailable`).
	// Collapsing the first two would make a monitor incident claim a service's empty
	// membership.
	Members            *[]domain.IncidentMember `json:"members,omitempty"`
	MembersUnavailable bool                     `json:"members_unavailable,omitempty"`
	// ImpactBasis is the mapping in force when the incident was classified (FR-042 D15). Like
	// Impacts it is deliberately NOT a field of domain.Incident, which the public projection, the
	// feeds and the webhooks embed. ALWAYS serialized: the object, or null when not recorded (D12)
	// or unreadable; impact_basis_unavailable says which.
	ImpactBasis            *domain.ImpactBasis `json:"impact_basis"`
	ImpactBasisUnavailable bool                `json:"impact_basis_unavailable,omitempty"`
	// Ref is the incident's short reference for the phone (FR-041a D21), absent only when the read
	// failed. Not a field of domain.Incident, for the reason ImpactBasis is not.
	Ref string `json:"ref,omitempty"`
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
	if ref, err := h.store.IncidentRef(r.Context(), inc.ID); err != nil {
		h.logEvent(r, "incident_ref_read_failed", "incident_id", inc.ID, "error", err.Error())
	} else {
		out.Ref = ref
	}
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
	// The basis of the impact, by its own project-scoped read, on the same terms: the incident still
	// renders when it fails and the failure is disclosed.
	if basis, err := h.store.IncidentImpactBasis(r.Context(), inc.ProjectID, inc.ID); err != nil {
		h.logEvent(r, "incident_impact_basis_read_failed", "incident_id", inc.ID, "error", err.Error())
		out.ImpactBasisUnavailable = true
	} else {
		out.ImpactBasis = basis
	}
	// The member snapshot, on the same terms as the impacts: the incident still renders
	// when the read fails, and the failure is DISCLOSED rather than served as "no
	// members". A postmortem is read after the world moved, so this is the only answer
	// that keeps naming a member deleted since.
	if members, present, err := h.store.IncidentMemberSnapshot(r.Context(), inc.ID); err != nil {
		h.logEvent(r, "incident_member_snapshot_read_failed", "incident_id", inc.ID, "error", err.Error())
		out.MembersUnavailable = true
	} else if present {
		if members == nil {
			members = []domain.IncidentMember{}
		}
		out.Members = &members
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
	// A plain comment keeps the current status — and says so by sending NOTHING, rather than by
	// echoing the status this handler read a moment ago. Materializing it here is what let a comment
	// revert a transition committed between that read and this write: the store resolves the intent
	// against the row it holds a lock on. `inc` is still needed, for its id and for access control.
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
	created, err := h.store.AddIncidentUpdateByPrincipal(r.Context(), upd, auditActor(p))
	// The pre-check above is a fast, friendly answer; THIS is the one that holds. It reads the
	// incident outside the store's transaction, so a concurrent resolve between the two lands
	// here, and the store refuses rather than reopening a closed incident.
	if errors.Is(err, store.ErrIncidentTerminal) {
		writeError(w, http.StatusBadRequest, "incident is resolved")
		return
	}
	if errors.Is(err, store.ErrStatusRegression) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
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
	pm, err := h.store.UpsertPostmortemByPrincipal(r.Context(), inc.ID, body.Body, p.UserID, auditActor(p))
	if err != nil {
		h.serverError(w, "upsert_postmortem", err)
		return
	}
	writeJSON(w, http.StatusOK, pm)
}
