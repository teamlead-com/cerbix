package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

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
		// Actions is the optional allow-list (FR-025 D12): absent or null leaves the role in
		// charge (nil), a list — even `[]` — is stored as given and intersected with the role
		// inside authz.Can. Immutable after create: there is no route that changes it.
		Actions []string `json:"actions"`
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
		Actions:   body.Actions,
	}
	if err := tok.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTokenActions(tok); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext := generateTokenSecret()
	created, err := h.store.CreateApiToken(r.Context(), tok, store.HashToken(plaintext))
	var changeErr *domain.ChangeError
	if errors.As(err, &changeErr) {
		// The store's own catalogue check (`action_unknown`), the second line behind the one above.
		writeError(w, http.StatusBadRequest, changeErr.Error())
		return
	}
	if err != nil {
		h.serverError(w, "create_api_token", err)
		return
	}
	h.audit(r, orgID, "token.create", tokenAuditTarget(created))
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

// validateTokenActions is the D12 check on create as the transport runs it: every entry of a
// non-nil allow-list must name an Action of the central catalogue (`action_unknown`, the store's
// own rule, run here first so the refusal names the entry before any write), and must be an
// action the token's ROLE grants (`action_not_granted`) — the list can only narrow the role, so a
// list naming an action the role lacks would be a token that can never do what its author wrote
// down. The grant question is asked of authz.Can through a principal shaped exactly as the auth
// layer will build it from this token, never of a role table copied into a handler.
func validateTokenActions(tok domain.ApiToken) error {
	if tok.Actions == nil {
		return nil
	}
	shadow := authz.Principal{Memberships: []domain.Membership{{OrgID: tok.OrgID, ProjectID: tok.ProjectID, Role: tok.Role}}}
	for _, a := range tok.Actions {
		action := authz.Action(a)
		if !authz.ValidAction(action) {
			return domain.NewChangeError(domain.ChangeErrActionUnknown, "actions", "%q is not an action", a)
		}
		if !shadow.Can(action, tok.OrgID, tok.ProjectID) {
			return domain.NewChangeError(tokenErrActionNotGranted, "actions", "%q is not granted to role %s", a, tok.Role)
		}
	}
	return nil
}

// tokenErrActionNotGranted refuses an allow-list entry the token's role does not grant.
const tokenErrActionNotGranted = "action_not_granted"

// tokenAuditTarget is the `token.create` audit row's target text: `<role> · <name>`, plus the
// allow-list when one was given (D12: the list is visible in the audit row).
func tokenAuditTarget(t domain.ApiToken) string {
	target := string(t.Role) + " · " + t.Name
	if t.Actions != nil {
		target += " · actions: " + strings.Join(t.Actions, ",")
		if len(t.Actions) == 0 {
			target += "(none)"
		}
	}
	return target
}

// generateTokenSecret returns a new bearer secret with a recognizable prefix.
func generateTokenSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error()) // fail closed — never issue a predictable secret
	}
	return "cbx_" + hex.EncodeToString(b)
}
