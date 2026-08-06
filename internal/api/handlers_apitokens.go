package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// listApiTokens lists an org's service-account tokens (org admin). Secrets are
// never returned — only metadata.
// resolveUserEmails maps the distinct user ids produced by iter to emails,
// best-effort (deleted users simply resolve to nothing). Lists here are small
// (tokens/webhooks of one org), so per-id lookups are fine.
func (h *Handler) resolveUserEmails(r *http.Request, iter func(yield func(string))) map[string]string {
	out := map[string]string{}
	iter(func(id string) {
		if id == "" {
			return
		}
		if _, seen := out[id]; seen {
			return
		}
		if u, err := h.store.GetUser(r.Context(), id); err == nil {
			out[id] = u.Email
		} else {
			out[id] = ""
		}
	})
	return out
}

func (h *Handler) listApiTokens(w http.ResponseWriter, r *http.Request) {
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
	tokens, err := h.store.ListApiTokensByOrg(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "list_api_tokens", err)
		return
	}
	emails := h.resolveUserEmails(r, func(yield func(string)) {
		for _, t := range tokens {
			yield(t.CreatedBy)
		}
	})
	for i := range tokens {
		tokens[i].CreatedByEmail = emails[tokens[i].CreatedBy]
	}
	writeJSON(w, http.StatusOK, tokens)
}

// createApiToken issues a service-account token (org admin). The plaintext secret
// is returned once, in this response only; the store keeps just its hash.
func (h *Handler) createApiToken(w http.ResponseWriter, r *http.Request) {
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
		Name      string `json:"name"`
		Role      string `json:"role"`
		ProjectID string `json:"project_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// A project-scoped token must target a project within this org.
	if body.ProjectID != "" {
		proj, err := h.store.GetProject(r.Context(), body.ProjectID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && proj.OrgID != orgID) {
			writeError(w, http.StatusBadRequest, "project is not in this organization")
			return
		}
		if err != nil {
			h.serverError(w, "get_project", err)
			return
		}
	}
	tok := domain.ApiToken{
		OrgID:     orgID,
		ProjectID: body.ProjectID,
		Name:      body.Name,
		Role:      domain.Role(body.Role),
		CreatedBy: p.UserID,
	}
	if err := tok.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext := generateTokenSecret()
	created, err := h.store.CreateApiToken(r.Context(), tok, store.HashToken(plaintext))
	if err != nil {
		h.serverError(w, "create_api_token", err)
		return
	}
	h.audit(r, orgID, "token.create", string(created.Role)+" · "+created.Name)
	h.logEvent(r, "api_token_issued", "token_id", created.ID, "name", created.Name, "org_id", orgID)
	// The only response that ever carries the secret.
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":     plaintext,
		"api_token": created,
	})
}

// deleteApiToken revokes a token (org admin on its org).
func (h *Handler) deleteApiToken(w http.ResponseWriter, r *http.Request) {
	tok, err := h.store.GetApiToken(r.Context(), r.PathValue("tokenID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_api_token", err)
		return
	}
	p, _ := h.principal(r)
	if !p.InOrg(tok.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, tok.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.DeleteApiToken(r.Context(), tok.ID); err != nil {
		h.serverError(w, "delete_api_token", err)
		return
	}
	h.audit(r, tok.OrgID, "token.delete", tok.Name)
	h.logEvent(r, "api_token_revoked", "token_id", tok.ID, "name", tok.Name, "org_id", tok.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

// generateTokenSecret returns a new bearer secret with a recognizable prefix.
func generateTokenSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "cbx_" + hex.EncodeToString(b)
}
