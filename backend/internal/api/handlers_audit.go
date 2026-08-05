package api

import (
	"net/http"
	"strconv"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// audit records an access-relevant action for the request's principal,
// best-effort (a failure is logged, never fatal to the mutation that triggered it).
func (h *Handler) audit(r *http.Request, orgID, action, target string) {
	p, _ := h.principal(r)
	err := h.store.RecordAudit(r.Context(), domain.AuditEntry{
		OrgID:       orgID,
		ActorUserID: p.UserID,
		ViaToken:    p.ViaToken,
		Action:      action,
		Target:      target,
	})
	if err != nil && h.logger != nil {
		h.logger.Warn("audit_record_failed", "action", action, "org", orgID, "error", err.Error())
	}
}

// listAudit returns an org's recent audit entries (org admin).
func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request) {
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
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			limit = n
		}
	}
	entries, err := h.store.ListAuditByOrg(r.Context(), orgID, limit)
	if err != nil {
		h.serverError(w, "list_audit", err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
