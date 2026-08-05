package api

import (
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// search returns projects, monitors and incidents matching ?q, restricted to what
// the caller may see (tenant isolation is enforced here, after the store's broad
// match). A query shorter than 2 characters returns nothing.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "hits": []domain.SearchHit{}})
		return
	}
	raw, err := h.store.Search(r.Context(), q, 8)
	if err != nil {
		h.serverError(w, "search", err)
		return
	}
	hits := make([]domain.SearchHit, 0, len(raw))
	for _, hit := range raw {
		if p.VisibleProject(hit.OrgID, hit.ProjectID) {
			hits = append(hits, hit)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "hits": hits})
}
