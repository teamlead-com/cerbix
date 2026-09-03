package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// --- Escalation policies ---

func (h *Handler) listEscalationPolicies(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	policies, err := h.store.ListEscalationPolicies(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_escalation_policies", err)
		return
	}
	writeJSON(w, http.StatusOK, policies)
}

type escalationPolicyBody struct {
	Name       string                  `json:"name"`
	RepeatLast bool                    `json:"repeat_last"`
	Steps      []domain.EscalationStep `json:"steps"`
}

func (h *Handler) createEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body escalationPolicyBody
	if !decodeJSON(w, r, &body) {
		return
	}
	p := domain.EscalationPolicy{ProjectID: proj.ID, Name: body.Name, RepeatLast: body.RepeatLast, Steps: body.Steps}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.escalationStepsInProject(w, r, p.Steps, proj.ID) {
		return
	}
	created, err := h.store.CreateEscalationPolicy(r.Context(), p)
	if err != nil {
		h.serverError(w, "create_escalation_policy", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) updateEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.escalationPolicyAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body escalationPolicyBody
	if !decodeJSON(w, r, &body) {
		return
	}
	p.Name, p.RepeatLast, p.Steps = body.Name, body.RepeatLast, body.Steps
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.escalationStepsInProject(w, r, p.Steps, p.ProjectID) {
		return
	}
	updated, err := h.store.UpdateEscalationPolicy(r.Context(), p)
	if err != nil {
		h.serverError(w, "update_escalation_policy", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.escalationPolicyAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if err := h.store.DeleteEscalationPolicy(r.Context(), p.ID); err != nil {
		h.serverError(w, "delete_escalation_policy", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// escalationPolicyAccess loads a policy by path id and checks project access.
func (h *Handler) escalationPolicyAccess(w http.ResponseWriter, r *http.Request, action authz.Action) (domain.EscalationPolicy, bool) {
	p, err := h.store.GetEscalationPolicy(r.Context(), r.PathValue("policyID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.EscalationPolicy{}, false
	}
	if err != nil {
		h.serverError(w, "get_escalation_policy", err)
		return domain.EscalationPolicy{}, false
	}
	if _, ok := h.projectAccess(w, r, p.ProjectID, action); !ok {
		return domain.EscalationPolicy{}, false
	}
	return p, true
}

// --- On-call schedules ---

func (h *Handler) listOnCallSchedules(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	schedules, err := h.store.ListOnCallSchedules(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_oncall_schedules", err)
		return
	}
	writeJSON(w, http.StatusOK, schedules)
}

type onCallScheduleBody struct {
	Name         string    `json:"name"`
	ShiftSeconds int       `json:"shift_seconds"`
	AnchorAt     time.Time `json:"anchor_at"`
	Participants []string  `json:"participants"`
}

func (h *Handler) createOnCallSchedule(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body onCallScheduleBody
	if !decodeJSON(w, r, &body) {
		return
	}
	sc := domain.OnCallSchedule{
		ProjectID: proj.ID, Name: body.Name, ShiftSeconds: body.ShiftSeconds,
		AnchorAt: body.AnchorAt, Participants: body.Participants,
	}
	if err := sc.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.channelsInProject(w, r, sc.Participants, proj.ID) {
		return
	}
	created, err := h.store.CreateOnCallSchedule(r.Context(), sc)
	if err != nil {
		h.serverError(w, "create_oncall_schedule", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) updateOnCallSchedule(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.onCallScheduleAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body onCallScheduleBody
	if !decodeJSON(w, r, &body) {
		return
	}
	sc.Name, sc.ShiftSeconds, sc.AnchorAt, sc.Participants = body.Name, body.ShiftSeconds, body.AnchorAt, body.Participants
	if err := sc.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.channelsInProject(w, r, sc.Participants, sc.ProjectID) {
		return
	}
	updated, err := h.store.UpdateOnCallSchedule(r.Context(), sc)
	if err != nil {
		h.serverError(w, "update_oncall_schedule", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) deleteOnCallSchedule(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.onCallScheduleAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if err := h.store.DeleteOnCallSchedule(r.Context(), sc.ID); err != nil {
		h.serverError(w, "delete_oncall_schedule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// currentOnCall returns the channel currently on call for a schedule (honoring
// overrides), for the "who is on call now" view.
func (h *Handler) currentOnCall(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.onCallScheduleAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schedule_id": sc.ID,
		"channel_id":  sc.OnCall(time.Now()),
	})
}

// listOverrides lists a schedule's on-call overrides (viewer+).
func (h *Handler) listOverrides(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.onCallScheduleAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	overrides, err := h.store.ListOnCallOverrides(r.Context(), sc.ID)
	if err != nil {
		h.serverError(w, "list_overrides", err)
		return
	}
	writeJSON(w, http.StatusOK, overrides)
}

// addOverride adds a vacation-cover override to a schedule (editor+).
func (h *Handler) addOverride(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.onCallScheduleAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		ChannelID string    `json:"channel_id"`
		StartsAt  time.Time `json:"starts_at"`
		EndsAt    time.Time `json:"ends_at"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	o := domain.OnCallOverride{ScheduleID: sc.ID, ChannelID: body.ChannelID, StartsAt: body.StartsAt, EndsAt: body.EndsAt}
	if err := o.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.channelInProject(w, r, o.ChannelID, sc.ProjectID) {
		return
	}
	created, err := h.store.AddOnCallOverride(r.Context(), o)
	if err != nil {
		h.serverError(w, "add_override", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// deleteOverride removes an override (editor+ on its schedule's project).
func (h *Handler) deleteOverride(w http.ResponseWriter, r *http.Request) {
	o, err := h.store.GetOnCallOverride(r.Context(), r.PathValue("overrideID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_override", err)
		return
	}
	sc, err := h.store.GetOnCallSchedule(r.Context(), o.ScheduleID)
	if err != nil {
		h.serverError(w, "get_override_schedule", err)
		return
	}
	if _, ok := h.projectAccess(w, r, sc.ProjectID, authz.ActionProjectWrite); !ok {
		return
	}
	if err := h.store.DeleteOnCallOverride(r.Context(), o.ID); err != nil {
		h.serverError(w, "delete_override", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) onCallScheduleAccess(w http.ResponseWriter, r *http.Request, action authz.Action) (domain.OnCallSchedule, bool) {
	sc, err := h.store.GetOnCallSchedule(r.Context(), r.PathValue("scheduleID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.OnCallSchedule{}, false
	}
	if err != nil {
		h.serverError(w, "get_oncall_schedule", err)
		return domain.OnCallSchedule{}, false
	}
	if _, ok := h.projectAccess(w, r, sc.ProjectID, action); !ok {
		return domain.OnCallSchedule{}, false
	}
	return sc, true
}

// --- Incident acknowledgement ---

// acknowledgeIncident marks an incident as acknowledged (stopping escalation). Requires
// project-write on the incident's project.
func (h *Handler) acknowledgeIncident(w http.ResponseWriter, r *http.Request) {
	inc, err := h.store.GetIncident(r.Context(), r.PathValue("incidentID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_incident", err)
		return
	}
	if _, ok := h.projectAccess(w, r, inc.ProjectID, authz.ActionProjectWrite); !ok {
		return
	}
	p, _ := h.principal(r)
	acked, err := h.store.AcknowledgeIncidentByPrincipal(r.Context(), inc.ID, p.UserID, auditActor(p))
	if err == nil {
		h.logEvent(r, "incident_acknowledged", "incident_id", inc.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "incident is already resolved")
		return
	}
	if err != nil {
		h.serverError(w, "acknowledge_incident", err)
		return
	}
	writeJSON(w, http.StatusOK, acked)
}
