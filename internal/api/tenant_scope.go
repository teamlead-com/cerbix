package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// This file enforces same-project ownership on cross-entity references. Without it a
// user with write access to their own project could point a monitor, escalation
// policy, or on-call schedule at ANOTHER tenant's escalation policy / channel /
// schedule id — the DB FKs enforce existence, not ownership, and step targets are an
// opaque JSONB blob with no FK at all. At fire time those ids resolve with no project
// scoping, so a mis-scoped reference would page across a tenant boundary.

// channelInProject reports whether channelID is a notification channel in projectID.
func (h *Handler) channelInProject(w http.ResponseWriter, r *http.Request, channelID, projectID string) bool {
	ch, err := h.store.GetNotificationChannel(r.Context(), channelID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ch.ProjectID != projectID) {
		writeError(w, http.StatusBadRequest, "notification channel "+channelID+" is not in this project")
		return false
	}
	if err != nil {
		h.serverError(w, "scope_channel", err)
		return false
	}
	return true
}

// scheduleInProject reports whether scheduleID is an on-call schedule in projectID.
func (h *Handler) scheduleInProject(w http.ResponseWriter, r *http.Request, scheduleID, projectID string) bool {
	sc, err := h.store.GetOnCallSchedule(r.Context(), scheduleID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sc.ProjectID != projectID) {
		writeError(w, http.StatusBadRequest, "on-call schedule "+scheduleID+" is not in this project")
		return false
	}
	if err != nil {
		h.serverError(w, "scope_schedule", err)
		return false
	}
	return true
}

// escalationPolicyInProject reports whether policyID is an escalation policy in
// projectID. A blank id (no policy set) is trivially fine.
func (h *Handler) escalationPolicyInProject(w http.ResponseWriter, r *http.Request, policyID, projectID string) bool {
	if policyID == "" {
		return true
	}
	p, err := h.store.GetEscalationPolicy(r.Context(), policyID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.ProjectID != projectID) {
		writeError(w, http.StatusBadRequest, "escalation policy "+policyID+" is not in this project")
		return false
	}
	if err != nil {
		h.serverError(w, "scope_policy", err)
		return false
	}
	return true
}

// escalationStepsInProject verifies every step target (channel or schedule) belongs
// to projectID, so a policy can only ever page its own project's targets.
func (h *Handler) escalationStepsInProject(w http.ResponseWriter, r *http.Request, steps []domain.EscalationStep, projectID string) bool {
	for _, s := range steps {
		for _, t := range s.Targets {
			switch t.Type {
			case domain.EscalationTargetChannel:
				if !h.channelInProject(w, r, t.ID, projectID) {
					return false
				}
			case domain.EscalationTargetSchedule:
				if !h.scheduleInProject(w, r, t.ID, projectID) {
					return false
				}
			}
		}
	}
	return true
}

// channelsInProject verifies every id in ids is a channel in projectID (on-call
// schedule participants are channel ids).
func (h *Handler) channelsInProject(w http.ResponseWriter, r *http.Request, ids []string, projectID string) bool {
	for _, id := range ids {
		if !h.channelInProject(w, r, id, projectID) {
			return false
		}
	}
	return true
}
