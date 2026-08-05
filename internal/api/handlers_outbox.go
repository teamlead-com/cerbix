package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/store"
)

// requireGlobalAdmin returns the principal if it may manage global resources,
// else writes 403 and returns ok=false. The outbox is a system-wide operational
// surface, not scoped to an org/project.
func (h *Handler) requireGlobalAdmin(w http.ResponseWriter, r *http.Request) bool {
	p, _ := h.principal(r)
	if !p.Can(authz.ActionGlobalManage, "", "") {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// listDeadOutbox returns dead-lettered outbox events (global admin only).
func (h *Handler) listDeadOutbox(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := h.store.ListDeadOutbox(r.Context(), limit)
	if err != nil {
		h.serverError(w, "list_dead_outbox", err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// replayDeadOutbox requeues one dead event for delivery (global admin only).
func (h *Handler) replayDeadOutbox(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	id := r.PathValue("eventID")
	if err := h.store.ReplayDeadOutbox(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no dead outbox event with that id")
			return
		}
		h.serverError(w, "replay_dead_outbox", err)
		return
	}
	h.logger.Info("outbox_dead_replayed", "event_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// replayAllDeadOutbox requeues every dead event (global admin only).
func (h *Handler) replayAllDeadOutbox(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	n, err := h.store.ReplayAllDeadOutbox(r.Context())
	if err != nil {
		h.serverError(w, "replay_all_dead_outbox", err)
		return
	}
	h.logger.Info("outbox_dead_replayed_all", "count", n)
	writeJSON(w, http.StatusOK, map[string]int{"replayed": n})
}
