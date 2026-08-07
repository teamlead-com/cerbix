package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// pushHeartbeat is the unauthenticated push endpoint for dead-man's-switch monitors: the
// watched service POSTs here every interval. Each call records an up heartbeat (or a down
// one with ?status=down) via the dedicated PushRecorder — a trusted server-side entrypoint
// (NOT the shared ResultSink), applied durably within the request and running the same
// status/notification/incident post-commit flow as an active check. received_at is the DB
// clock captured at the token lookup, so ordering never degrades into processed_at.
func (h *Handler) pushHeartbeat(w http.ResponseWriter, r *http.Request) {
	mon, receivedAt, err := h.store.GetMonitorByPushToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_monitor_by_push_token", err)
		return
	}
	if h.pushRecorder == nil {
		writeError(w, http.StatusNotImplemented, "push ingestion is not available")
		return
	}
	up := r.URL.Query().Get("status") != "down"
	msg := r.URL.Query().Get("msg")
	if msg == "" {
		if up {
			msg = "push ok"
		} else {
			msg = "push reported down"
		}
	}
	// observedAt (raw client timestamp) is not accepted by this endpoint yet → zero/absent.
	h.pushRecorder.Record(r.Context(), mon.ID, up, msg, receivedAt, time.Time{})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "monitor_id": mon.ID, "up": up})
}

// generatePushToken returns a new push-endpoint secret.
func generatePushToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error()) // fail closed — never issue a predictable secret
	}
	return "cbxp_" + hex.EncodeToString(b)
}
