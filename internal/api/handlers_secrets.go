package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Project secret inventory endpoints (spec func-secret-inventory §5). Values are
// write-only: no response, error, log, or audit entry ever carries a submitted
// value — only names and metadata leave the server.

// secretMaxBody caps create/update request bodies. A secret value is at most
// 4 KiB (enforced by the store); 16 KiB leaves headroom for JSON framing while
// bounding memory against an oversized body.
const secretMaxBody = 32 << 10 // 32 KiB: a 4096-byte value can JSON-encode at ~6x (\u0000 escapes)

// secretUsedByView is the reference-count block of a secret listing row.
type secretUsedByView struct {
	Total       int `json:"total"`
	FileManaged int `json:"file_managed"`
}

// secretView is the no-value listing DTO (spec §4.4.2: names, dates, and used-by
// counts only — there is no value field by construction).
type secretView struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	CreatedAt time.Time        `json:"created_at"`
	RotatedAt *time.Time       `json:"rotated_at"`
	UsedBy    secretUsedByView `json:"used_by"`
}

// secretActor carries the request's principal into the store, which writes the
// audit row INSIDE the mutation transaction (atomic with the change, unlike the
// best-effort post-commit h.audit path).
func (h *Handler) secretActor(r *http.Request) store.SecretActor {
	p, _ := h.principal(r)
	// Audit-identity contract: a synthetic API-token id maps to a NULL actor (via_token
	// carries the machine attribution); an OIDC client-credentials principal has a real
	// JIT user uuid and keeps it. A non-uuid actor would abort the in-tx audit insert
	// and roll back the whole mutation (fail-closed by design).
	return store.SecretActor{ActorUserID: p.AuditUserID(), ViaToken: p.ViaToken}
}

// secretsFeatureEnabled gates every inventory endpoint on the instance-wide
// feature switch (spec §4.1: secrets.enabled=false → 404 feature_disabled).
// Checked before authz on purpose: the switch is instance configuration, not
// tenant data, so the early 404 leaks nothing.
func (h *Handler) secretsFeatureEnabled(w http.ResponseWriter) bool {
	if h.secretsEnabled {
		return true
	}
	writeError(w, http.StatusNotFound, "feature_disabled")
	return false
}

// listSecrets returns the project's secret inventory: names and metadata, never
// values (ProjectRead — names are metadata, matching the monitor list).
func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	// §5: the whole secret surface is uncacheable — including errors/404/409.
	w.Header().Set("Cache-Control", "no-store")
	if !h.secretsFeatureEnabled(w) {
		return
	}
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	secrets, err := h.store.ListProjectSecrets(r.Context(), proj.ID)
	if h.writeSecretError(w, err) { // ErrNotFound: project vanished after authz
		return
	}
	out := make([]secretView, 0, len(secrets))
	for _, s := range secrets {
		out = append(out, secretView{
			ID: s.ID, Name: s.Name, CreatedAt: s.CreatedAt, RotatedAt: s.RotatedAt,
			UsedBy: secretUsedByView{Total: s.UsedByTotal, FileManaged: s.UsedByFileManaged},
		})
	}
	// Names are still sensitive metadata: keep the listing out of shared caches.
	writeJSON(w, http.StatusOK, out)
}

// createSecret adds a named secret to the project inventory (editor+). The
// value is accepted, encrypted, and never echoed back.
func (h *Handler) createSecret(w http.ResponseWriter, r *http.Request) {
	// §5: the whole secret surface is uncacheable — including errors/404/409.
	w.Header().Set("Cache-Control", "no-store")
	if !h.secretsFeatureEnabled(w) {
		return
	}
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, secretMaxBody)
	var body struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// The store writes the secret.create audit row inside the mutation tx.
	created, err := h.store.CreateProjectSecret(r.Context(), h.secretActor(r), proj.ID, body.Name, body.Value)
	if h.writeSecretError(w, err) {
		return
	}
	h.logEvent(r, "secret_created", "project_id", proj.ID, "name", created.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": created.ID, "name": created.Name, "created_at": created.CreatedAt,
	})
}

// updateSecret renames and/or rotates a secret in one transaction (editor+).
// At least one of name/value must be provided; 204 on success.
func (h *Handler) updateSecret(w http.ResponseWriter, r *http.Request) {
	// §5: the whole secret surface is uncacheable — including errors/404/409.
	w.Header().Set("Cache-Control", "no-store")
	if !h.secretsFeatureEnabled(w) {
		return
	}
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, secretMaxBody)
	var body struct {
		Name  *string `json:"name"`
		Value *string `json:"value"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == nil && body.Value == nil {
		writeError(w, http.StatusBadRequest, "nothing to update: provide name and/or value")
		return
	}
	// The store writes the secret.update audit row (renamed/rotated flags + re-point
	// count) inside the mutation tx.
	renamed, rotated, repointed, err := h.store.UpdateProjectSecret(r.Context(), h.secretActor(r), proj.ID, name, body.Name, body.Value)
	if h.writeSecretError(w, err) {
		return
	}
	h.logEvent(r, "secret_updated", "project_id", proj.ID, "name", name,
		"renamed", renamed, "rotated", rotated, "repointed", repointed)
	w.WriteHeader(http.StatusNoContent)
}

// deleteSecret removes an unreferenced secret (editor+). While monitor settings
// still reference it, the delete is refused with the reference count.
func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	// §5: the whole secret surface is uncacheable — including errors/404/409.
	w.Header().Set("Cache-Control", "no-store")
	if !h.secretsFeatureEnabled(w) {
		return
	}
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	name := r.PathValue("name")
	// The store writes the secret.delete audit row inside the mutation tx.
	if err := h.store.DeleteProjectSecret(r.Context(), h.secretActor(r), proj.ID, name); h.writeSecretError(w, err) {
		return
	}
	h.logEvent(r, "secret_deleted", "project_id", proj.ID, "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// writeSecretError maps the store's typed secret errors onto the API contract
// (spec §5) and reports whether it wrote a response. Messages name the violated
// rule and never include the submitted value.
func (h *Handler) writeSecretError(w http.ResponseWriter, err error) bool {
	var inUse store.SecretInUseError
	var renamedInUse store.SecretRenamedInUseError
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrSecretExists):
		writeError(w, http.StatusConflict, "secret_exists")
	case errors.Is(err, store.ErrSecretQuota):
		writeError(w, http.StatusConflict, "secret_quota")
	case errors.Is(err, store.ErrSecretNameInvalid):
		writeError(w, http.StatusBadRequest, "invalid secret name: must match ^[a-z][a-z0-9-]{0,62}$")
	case errors.Is(err, store.ErrSecretValueInvalid):
		writeError(w, http.StatusBadRequest, "invalid secret value: must be non-empty valid UTF-8, at most 4096 bytes")
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "secret_in_use", "count": inUse.Count})
	case errors.As(err, &renamedInUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "secret_renamed_in_use", "count": renamedInUse.Count})
	default:
		h.serverError(w, "project_secret", err)
	}
	return true
}
