package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// version returns the running build (any authenticated user) — surfaced in
// the SPA sidebar footer.
func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Current())
}

// me returns the current user and their memberships.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := h.store.GetUser(r.Context(), p.UserID)
	if err != nil {
		h.serverError(w, "get_user", err)
		return
	}
	_, totpEnabled, _ := h.store.GetTOTP(r.Context(), p.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":         user,
		"memberships":  p.Memberships,
		"totp_enabled": totpEnabled,
	})
}

// listOrganizations lists organizations visible to the caller.
func (h *Handler) listOrganizations(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	var (
		orgs []domain.Organization
		err  error
	)
	if p.IsGlobalAdmin {
		orgs, err = h.store.ListOrganizations(r.Context())
	} else {
		orgs, err = h.store.ListOrganizationsForUser(r.Context(), p.UserID)
	}
	if err != nil {
		h.serverError(w, "list_organizations", err)
		return
	}
	writeJSON(w, http.StatusOK, orgs)
}

// createOrganization creates an organization (global admin only).
func (h *Handler) createOrganization(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	if !p.Can(authz.ActionGlobalManage, "", "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct{ Slug, Name string }
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Slug) == "" || strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "slug and name are required")
		return
	}
	org, err := h.store.CreateOrganization(r.Context(), body.Slug, body.Name)
	if err != nil {
		h.serverError(w, "create_organization", err)
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// getOrganization returns one organization if the caller belongs to it.
func (h *Handler) getOrganization(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	org, err := h.store.GetOrganization(r.Context(), orgID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_organization", err)
		return
	}
	writeJSON(w, http.StatusOK, org)
}

// listProjects lists the projects of an org visible to the caller.
func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	all, err := h.store.ListProjectsByOrg(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "list_projects", err)
		return
	}
	visible := make([]domain.Project, 0, len(all))
	for _, proj := range all {
		if p.VisibleProject(orgID, proj.ID) {
			visible = append(visible, proj)
		}
	}
	writeJSON(w, http.StatusOK, visible)
}

// createProject creates a project in an org (org admin).
func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct{ Slug, Name string }
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Slug) == "" || strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "slug and name are required")
		return
	}
	proj, err := h.store.CreateProject(r.Context(), orgID, body.Slug, body.Name)
	if err != nil {
		h.serverError(w, "create_project", err)
		return
	}
	writeJSON(w, http.StatusCreated, proj)
}

// getProject returns one project if the caller may see it.
func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	proj, err := h.store.GetProject(r.Context(), r.PathValue("projectID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_project", err)
		return
	}
	if !p.VisibleProject(proj.OrgID, proj.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

// listMembers lists an org's memberships (org admin).
func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	members, err := h.store.ListOrgMembers(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "list_members", err)
		return
	}
	writeJSON(w, http.StatusOK, members)
}

// memberAccess loads a membership and checks the caller may manage the org it
// belongs to. Writes 404 (hidden) / 403 and returns ok=false on failure.
func (h *Handler) memberAccess(w http.ResponseWriter, r *http.Request) (domain.Membership, bool) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	m, err := h.store.GetMembership(r.Context(), r.PathValue("membershipID"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.OrgID != orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Membership{}, false
	}
	if err != nil {
		h.serverError(w, "get_membership", err)
		return domain.Membership{}, false
	}
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Membership{}, false
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return domain.Membership{}, false
	}
	return m, true
}

// isLastOrgAdmin reports whether m is the org's sole org-level admin, so
// demoting or removing it would leave the org without an admin.
func (h *Handler) isLastOrgAdmin(r *http.Request, m domain.Membership) (bool, error) {
	if m.ProjectID != "" || m.Role != domain.RoleOrgAdmin {
		return false, nil
	}
	n, err := h.store.CountOrgAdmins(r.Context(), m.OrgID)
	if err != nil {
		return false, err
	}
	return n <= 1, nil
}

// updateMember changes a member's role (org admin). The last org-level admin
// cannot be demoted, to avoid locking the org out.
func (h *Handler) updateMember(w http.ResponseWriter, r *http.Request) {
	m, ok := h.memberAccess(w, r)
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
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "invalid role")
		return
	}
	// A global admin may demote an org's last org_admin (and appoint a new
	// one afterwards) — consistent with deleting the user account entirely.
	if p, _ := h.principal(r); role != domain.RoleOrgAdmin && !p.IsGlobalAdmin {
		last, err := h.isLastOrgAdmin(r, m)
		if err != nil {
			h.serverError(w, "count_org_admins", err)
			return
		}
		if last {
			writeError(w, http.StatusBadRequest, "cannot demote the last org admin")
			return
		}
	}
	// An org-level membership may only hold an org role; a project membership a
	// project role. Re-validate the resulting membership.
	m.Role = role
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.store.UpdateMembershipRole(r.Context(), m.ID, role)
	if err != nil {
		h.serverError(w, "update_member", err)
		return
	}
	h.audit(r, m.OrgID, "member.role_change", string(role)+" · user "+updated.UserID)
	writeJSON(w, http.StatusOK, updated)
}

// removeMember revokes a membership (org admin). The last org-level admin
// cannot be removed.
func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	m, ok := h.memberAccess(w, r)
	if !ok {
		return
	}
	// A global admin may leave an org without an org_admin (and appoint a new
	// one afterwards) — consistent with deleting the user account entirely.
	if p, _ := h.principal(r); !p.IsGlobalAdmin {
		last, err := h.isLastOrgAdmin(r, m)
		if err != nil {
			h.serverError(w, "count_org_admins", err)
			return
		}
		if last {
			writeError(w, http.StatusBadRequest, "cannot remove the last org admin")
			return
		}
	}
	if err := h.store.DeleteMembership(r.Context(), m.ID); err != nil {
		h.serverError(w, "remove_member", err)
		return
	}
	h.audit(r, m.OrgID, "member.remove", "user "+m.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// addMember grants a user a role in the org or one of its projects (org admin).
func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	orgID := r.PathValue("orgID")
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		UserID    string `json:"user_id"`
		Email     string `json:"email"`
		ProjectID string `json:"project_id"`
		Role      string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// Add by email (an already-provisioned user) when no user_id is given.
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
	m := domain.Membership{
		UserID:    userID,
		OrgID:     orgID,
		ProjectID: body.ProjectID,
		Role:      domain.Role(body.Role),
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateMembership(r.Context(), m)
	if err != nil {
		// FK/constraint violations (unknown user, cross-org project, duplicate).
		writeError(w, http.StatusBadRequest, "could not create membership")
		return
	}
	h.audit(r, orgID, "member.add", string(created.Role)+" · user "+created.UserID)
	h.logEvent(r, "member_added", "org_id", orgID, "target_user_id", created.UserID, "role", string(created.Role))
	writeJSON(w, http.StatusCreated, created)
}

// changePassword lets a local user change their own password.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.NewPassword) < h.effectiveMinPasswordLen() {
		writeError(w, http.StatusBadRequest, "new password too short")
		return
	}
	hash, err := h.store.PasswordHashByID(r.Context(), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "not a local account")
		return
	}
	if err != nil {
		h.serverError(w, "password_hash", err)
		return
	}
	valid, err := auth.VerifyPassword(hash, body.CurrentPassword)
	if err != nil || !valid {
		writeError(w, http.StatusBadRequest, "current password incorrect")
		return
	}
	newHash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		h.serverError(w, "hash_password", err)
		return
	}
	if err := h.store.SetPassword(r.Context(), p.UserID, newHash); err != nil {
		h.serverError(w, "set_password", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// logEvent records an operational INFO event stamped with the acting principal
// — the log-side counterpart of the tenant-facing audit trail.
func (h *Handler) logEvent(r *http.Request, msg string, kv ...any) {
	p, _ := h.principal(r)
	h.logger.Info(msg, append([]any{"user_id", p.UserID}, kv...)...)
}

func (h *Handler) serverError(w http.ResponseWriter, op string, err error) {
	// A client abandoning the request (page closed mid-load) is not a server
	// failure — keep it out of the ERROR stream.
	if errors.Is(err, context.Canceled) {
		h.logger.Debug("request_canceled", "op", op)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.Error("api_error", "op", op, "error", err.Error())
	writeError(w, http.StatusInternalServerError, "internal error")
}
