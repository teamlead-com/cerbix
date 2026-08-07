package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// listWebhooks lists an org's webhook subscriptions (org admin).
func (h *Handler) listWebhooks(w http.ResponseWriter, r *http.Request) {
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
	hooks, err := h.store.ListWebhooksByOrg(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "list_webhooks", err)
		return
	}
	// Never expose secrets in a listing.
	emails := h.resolveUserEmails(r, func(yield func(string)) {
		for _, wh := range hooks {
			yield(wh.CreatedBy)
		}
	})
	for i := range hooks {
		hooks[i].Secret = ""
		hooks[i].CreatedByEmail = emails[hooks[i].CreatedBy]
	}
	writeJSON(w, http.StatusOK, hooks)
}

// createWebhook registers a webhook (org admin). A signing secret is generated
// when none is supplied, and returned once in this response.
func (h *Handler) createWebhook(w http.ResponseWriter, r *http.Request) {
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
		URL       string `json:"url"`
		Secret    string `json:"secret"`
		ProjectID string `json:"project_id"`
		Enabled   *bool  `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
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
	secret := body.Secret
	if secret == "" {
		secret = generateWebhookSecret()
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	hook := domain.Webhook{
		OrgID: orgID, ProjectID: body.ProjectID, URL: body.URL,
		Secret: secret, Enabled: enabled, CreatedBy: p.UserID,
	}
	if err := hook.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateWebhook(r.Context(), hook)
	if err != nil {
		h.serverError(w, "create_webhook", err)
		return
	}
	// The secret is included here (creation only) so the subscriber can verify signatures.
	writeJSON(w, http.StatusCreated, created)
}

// deleteWebhook removes a webhook (org admin on its org).
// updateWebhook toggles deliveries for a webhook (org admin). Disabled is not
// deleted — the secret and config survive a pause.
func (h *Handler) updateWebhook(w http.ResponseWriter, r *http.Request) {
	hook, err := h.store.GetWebhook(r.Context(), r.PathValue("webhookID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_webhook", err)
		return
	}
	p, _ := h.principal(r)
	if !p.InOrg(hook.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, hook.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := h.store.SetWebhookEnabled(r.Context(), hook.ID, *body.Enabled); err != nil {
		h.serverError(w, "set_webhook_enabled", err)
		return
	}
	h.audit(r, hook.OrgID, "webhook.toggle", hook.URL+" → enabled="+strconv.FormatBool(*body.Enabled))
	h.logEvent(r, "webhook_toggled", "webhook_id", hook.ID, "enabled", *body.Enabled, "org_id", hook.OrgID)
	hook.Enabled = *body.Enabled
	hook.Secret = ""
	writeJSON(w, http.StatusOK, hook)
}

func (h *Handler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	hook, err := h.store.GetWebhook(r.Context(), r.PathValue("webhookID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_webhook", err)
		return
	}
	p, _ := h.principal(r)
	if !p.InOrg(hook.OrgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, hook.OrgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.DeleteWebhook(r.Context(), hook.ID); err != nil {
		h.serverError(w, "delete_webhook", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// generateWebhookSecret returns a new signing secret.
func generateWebhookSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error()) // fail closed — never issue a predictable secret
	}
	return "whsec_" + hex.EncodeToString(b)
}
