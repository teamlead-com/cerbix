package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/store"
)

// This file enforces same-project ownership on cross-entity references. Without it a
// user with write access to their own project could point a monitor, escalation
// policy, or on-call schedule at ANOTHER tenant's escalation policy / channel /
// schedule id — the DB FKs enforce existence, not ownership, and step targets are an
// opaque JSONB blob with no FK at all. At fire time those ids resolve with no project
// scoping, so a mis-scoped reference would page across a tenant boundary.

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

func (h *Handler) routingReferenceError(w http.ResponseWriter, operation string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrRoutingReferenceNotInProject) {
		writeError(w, http.StatusBadRequest, "routing reference is not in this project")
		return true
	}
	h.serverError(w, operation, err)
	return true
}
