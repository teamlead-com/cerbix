package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Project-scoped member management (iter-0206, FR-040, func-project-members.md): a project admin
// manages the members of their own project. Every handler opens with the same two gates, in this
// order, BEFORE any membership is loaded:
//
//  1. a machine principal (a cbx_ API token or an OIDC client-credentials JWT — ViaToken) is
//     refused with 403, the same answer for any project id. No handler honoured project:manage
//     before this iteration, so letting tokens through would silently give every existing
//     project-scoped project_admin token the power to grant people lasting access;
//  2. projectAccess(project:manage): unknown or invisible project → 404, a visible project the
//     caller cannot manage → 403. An editor probing membership ids therefore always gets 403.
//
// The scope is then held again in the SQL (store.*Project* statements name the project).

// projectMembersAccess runs both gates and returns the project.
func (h *Handler) projectMembersAccess(w http.ResponseWriter, r *http.Request) (domain.Project, bool) {
	if p, _ := h.principal(r); p.ViaToken {
		writeError(w, http.StatusForbidden, "project members are managed by people, not tokens")
		return domain.Project{}, false
	}
	return h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectManage)
}

// projectMembersResponse is GET /projects/{projectID}/members.
type projectMembersResponse struct {
	Members   []domain.Member          `json:"members"`
	Inherited []domain.InheritedMember `json:"inherited"`
}

func (h *Handler) listProjectMembers(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectMembersAccess(w, r)
	if !ok {
		return
	}
	members, err := h.store.ListProjectMembers(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_project_members", err)
		return
	}
	inherited, err := h.store.ListProjectInheritedMembers(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_inherited_members", err)
		return
	}
	if members == nil {
		members = []domain.Member{}
	}
	if inherited == nil {
		inherited = []domain.InheritedMember{}
	}
	writeJSON(w, http.StatusOK, projectMembersResponse{Members: members, Inherited: inherited})
}

func (h *Handler) addProjectMember(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectMembersAccess(w, r)
	if !ok {
		return
	}
	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Role   string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	role := domain.Role(body.Role)
	if !role.ValidForScope(domain.ScopeProject) {
		writeError(w, http.StatusBadRequest, "role must be one of project_admin, editor, viewer")
		return
	}
	userID := body.UserID
	if userID == "" && strings.TrimSpace(body.Email) != "" {
		user, err := h.store.GetUserByEmail(r.Context(), strings.TrimSpace(body.Email))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "no user with that email; they must sign in once first")
			return
		}
		if err != nil {
			h.serverError(w, "get_user_by_email", err)
			return
		}
		userID = user.ID
	}
	if userID == "" {
		writeError(w, http.StatusBadRequest, "email or user_id is required")
		return
	}
	created, err := h.store.CreateProjectMembership(r.Context(), proj.ID, userID, role)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already a member of this project")
		return
	case errors.Is(err, store.ErrUnknownUser):
		writeError(w, http.StatusBadRequest, "unknown user")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		h.serverError(w, "add_project_member", err)
		return
	}
	h.audit(r, proj.OrgID, "member.add", memberAuditTarget(created))
	h.logEvent(r, "member_added", "org_id", proj.OrgID, "project_id", proj.ID, "target_user_id", created.UserID, "role", string(created.Role))
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) updateProjectMember(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectMembersAccess(w, r)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	role := domain.Role(body.Role)
	if !role.ValidForScope(domain.ScopeProject) {
		writeError(w, http.StatusBadRequest, "role must be one of project_admin, editor, viewer")
		return
	}
	updated, err := h.store.UpdateProjectMembershipRole(r.Context(), proj.ID, r.PathValue("membershipID"), role)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "update_project_member", err)
		return
	}
	h.audit(r, proj.OrgID, "member.role_change", memberAuditTarget(updated))
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) removeProjectMember(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectMembersAccess(w, r)
	if !ok {
		return
	}
	gone, err := h.store.DeleteProjectMembership(r.Context(), proj.ID, r.PathValue("membershipID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "remove_project_member", err)
		return
	}
	h.audit(r, proj.OrgID, "member.remove", memberRemoveAuditTarget(gone))
	w.WriteHeader(http.StatusNoContent)
}

// memberAuditTarget is the audit target of an add or role change, the same on the org and the
// project paths: "<role> · user <id>", plus " · project <id>" for a project-scoped membership.
func memberAuditTarget(m domain.Membership) string {
	return string(m.Role) + " · " + memberRemoveAuditTarget(m)
}

// memberRemoveAuditTarget is the audit target of a removal: "user <id>[ · project <id>]".
func memberRemoveAuditTarget(m domain.Membership) string {
	t := "user " + m.UserID
	if m.ProjectID != "" {
		t += " · project " + m.ProjectID
	}
	return t
}
