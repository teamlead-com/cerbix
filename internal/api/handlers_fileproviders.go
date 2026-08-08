package api

import (
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
)

// listFileProvidersAdmin returns every file-provider bundle's diagnostics (global-admin).
// Spec §15: configured providers, last successful generation, bounded errors, and relative
// paths — never absolute filesystem paths.
func (h *Handler) listFileProvidersAdmin(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	diags, err := h.store.FileProviderDiagnostics(r.Context(), "")
	if err != nil {
		h.serverError(w, "file_provider_diagnostics", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": diags})
}

// listOrgFileProviders returns file-provider diagnostics scoped to one organization
// (org-admin). Bundles are filtered to the org in SQL, so no cross-tenant leak (spec §15).
func (h *Handler) listOrgFileProviders(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	p, ok := h.principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !p.InOrg(orgID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !p.Can(authz.ActionOrgManage, orgID, "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	diags, err := h.store.FileProviderDiagnostics(r.Context(), orgID)
	if err != nil {
		h.serverError(w, "file_provider_diagnostics", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": diags})
}
