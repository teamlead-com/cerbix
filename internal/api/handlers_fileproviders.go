package api

import (
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FileProviderRuntimeStatus is this process's live view of one configured provider (§15):
// leadership, last scan/success, counts, and last error. Process-local (a non-leader replica
// reports Leader=false and its own scan times). Configured-but-idle providers appear too.
type FileProviderRuntimeStatus struct {
	Provider        string `json:"provider"`
	ScopeType       string `json:"scope_type"`
	ScopeOrg        string `json:"scope_org,omitempty"`
	ScopeProject    string `json:"scope_project,omitempty"`
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
	// Optional ?provider= filter (§15 named-provider selection on the operator side).
	if name := r.URL.Query().Get("provider"); name != "" {
		filtered := diags[:0:0]
		for _, d := range diags {
			if d.Provider == name {
				filtered = append(filtered, d)
			}
		}
		diags = filtered
	}
	if diags == nil {
		diags = []store.FileProviderDiagnostic{}
	}
	body := map[string]any{
		"bundles":   diags,
		"providers": []FileProviderRuntimeStatus{},
	}
	if h.fpStatus != nil {
		runtime := h.fpStatus.FileProviderRuntimeStatuses()
		if name := r.URL.Query().Get("provider"); name != "" {
			out := runtime[:0:0]
			for _, s := range runtime {
				if s.Provider == name {
					out = append(out, s)
				}
			}
			runtime = out
		}
		if runtime == nil {
			runtime = []FileProviderRuntimeStatus{}
		}
		body["providers"] = runtime
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
	if diags == nil {
		diags = []store.FileProviderDiagnostic{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bundles": diags})
}
