package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/store"
)

// listAllUsers returns every user of the instance with aggregated memberships
// (global admin only) — including users outside any organization, who are
// invisible in the org-scoped member lists.
func (h *Handler) listAllUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	users, err := h.store.ListAllUsers(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		h.serverError(w, "list_users", err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// updateAdminUser toggles the global-admin flag (global admin only). Changing
// your own flag is rejected, and the last global admin cannot be demoted.
func (h *Handler) updateAdminUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	p, _ := h.principal(r)
	userID := r.PathValue("userID")
	var body struct {
		IsGlobalAdmin *bool `json:"is_global_admin"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.IsGlobalAdmin == nil {
		writeError(w, http.StatusBadRequest, "is_global_admin is required")
		return
	}
	if userID == p.UserID {
		writeError(w, http.StatusBadRequest, "cannot change your own global-admin flag")
		return
	}
	u, err := h.store.GetUser(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_user", err)
		return
	}
	if u.IsGlobalAdmin && !*body.IsGlobalAdmin {
		n, err := h.store.CountGlobalAdmins(r.Context())
		if err != nil {
			h.serverError(w, "count_global_admins", err)
			return
		}
		if n <= 1 {
			writeError(w, http.StatusBadRequest, "cannot demote the last global admin")
			return
		}
	}
	if err := h.store.SetGlobalAdmin(r.Context(), userID, *body.IsGlobalAdmin); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.serverError(w, "set_global_admin", err)
		return
	}
	h.audit(r, "", "user.global_admin", fmt.Sprintf("%s · user %s → %v", u.Email, userID, *body.IsGlobalAdmin))
	h.logEvent(r, "user_global_admin_changed", "target_user_id", userID, "email", u.Email, "is_global_admin", *body.IsGlobalAdmin)
	u.IsGlobalAdmin = *body.IsGlobalAdmin
	writeJSON(w, http.StatusOK, u)
}

// deleteAdminUser removes a user entirely (global admin only): memberships and
// sessions cascade in the database. Self-deletion and deleting the last global
// admin are rejected.
func (h *Handler) deleteAdminUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	p, _ := h.principal(r)
	userID := r.PathValue("userID")
	if userID == p.UserID {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}
	u, err := h.store.GetUser(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_user", err)
		return
	}
	if u.IsGlobalAdmin {
		n, err := h.store.CountGlobalAdmins(r.Context())
		if err != nil {
			h.serverError(w, "count_global_admins", err)
			return
		}
		if n <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete the last global admin")
			return
		}
	}
	if err := h.store.DeleteUser(r.Context(), userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.serverError(w, "delete_user", err)
		return
	}
	h.audit(r, "", "user.delete", u.Email+" · user "+userID)
	h.logEvent(r, "user_deleted", "target_user_id", userID, "email", u.Email)
	w.WriteHeader(http.StatusNoContent)
}
