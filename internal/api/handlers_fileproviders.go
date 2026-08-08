package api

import (
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
)

// FileProviderRuntimeStatus is this process's live view of one configured provider (§15):
// leadership, last scan/success, counts, and last error. Process-local (a non-leader replica
// reports Leader=false and its own scan times). Configured-but-idle providers appear too.
type FileProviderRuntimeStatus struct {
	Provider        string `json:"provider"`
	ScopeType       string `json:"scope_type"`
	Leader          bool   `json:"leader"`
	LastScanUnix    int64  `json:"last_scan_unix,omitempty"`
	LastSuccessUnix int64  `json:"last_success_unix,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Managed         int    `json:"managed_monitors"`
	Orphaned        int    `json:"orphaned_monitors"`
	BundleErrors    int    `json:"bundle_errors"`
}

// FileProviderStatusSource supplies the process-local runtime status (implemented by an
// adapter over the reconcile runtime's StatusRegistry).
type FileProviderStatusSource interface {
	FileProviderRuntimeStatuses() []FileProviderRuntimeStatus
}

// listFileProvidersAdmin returns every file-provider bundle's persisted diagnostics plus this
// process's runtime status (global-admin). Spec §15: configured providers, leadership, last
// scan, last successful generation, bounded errors, relative paths — never absolute paths.
func (h *Handler) listFileProvidersAdmin(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	diags, err := h.store.FileProviderDiagnostics(r.Context(), "")
	if err != nil {
		h.serverError(w, "file_provider_diagnostics", err)
		return
	}
	body := map[string]any{"bundles": diags}
	if h.fpStatus != nil {
		body["providers"] = h.fpStatus.FileProviderRuntimeStatuses()
	}
	writeJSON(w, http.StatusOK, body)
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
	writeJSON(w, http.StatusOK, map[string]any{"bundles": diags})
}
