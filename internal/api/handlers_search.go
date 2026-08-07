package api

import (
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// search returns projects, monitors and incidents matching ?q, restricted to what
// the caller may see. Tenant isolation is pushed into the query (store.SearchScope),
// so the per-type LIMIT ranks only rows the caller is allowed to see — another
// tenant's matches can neither leak nor crowd out the caller's. A query shorter than
// 2 characters returns nothing.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "hits": []domain.SearchHit{}})
		return
	}
	allOrgs, orgIDs, projectIDs := p.VisibleScope(authz.ActionProjectRead)
	if !allOrgs && len(orgIDs) == 0 && len(projectIDs) == 0 {
		// The caller can see nothing — skip the query entirely.
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "hits": []domain.SearchHit{}})
		return
	}
	hits, err := h.store.Search(r.Context(), q, 8, store.SearchScope{AllOrgs: allOrgs, OrgIDs: orgIDs, ProjectIDs: projectIDs})
	if err != nil {
		h.serverError(w, "search", err)
		return
	}
	if hits == nil {
		hits = []domain.SearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "hits": hits})
}
