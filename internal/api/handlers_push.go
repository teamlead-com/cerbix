package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// pushHeartbeat is the unauthenticated push endpoint for dead-man's-switch
// monitors: the watched service POSTs here every interval. Each call records an
// up heartbeat (or a down one with ?status=down) into the ingest pipeline, so it
// flows through the same status/notification/incident path as an active check.
func (h *Handler) pushHeartbeat(w http.ResponseWriter, r *http.Request) {
	mon, err := h.store.GetMonitorByPushToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_monitor_by_push_token", err)
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
	// Transitional bridge (P0a): stamp a server timestamp so the push ping is a valid
	// scheduled result under RecordScheduledResult's missing-timestamp gate. Step 3
	// replaces this with a dedicated PushResultRecorder that captures received_at at
	// ingress and calls RecordPushResult directly (off the shared ResultSink).
	hb := domain.Heartbeat{MonitorID: mon.ID, Up: up, Msg: msg, Ts: time.Now().UTC()}
	if h.results != nil {
		if err := h.results.PublishResult(r.Context(), hb); err != nil {
			h.serverError(w, "publish_push_result", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "monitor_id": mon.ID, "up": up})
}

// generatePushToken returns a new push-endpoint secret.
func generatePushToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cbxp_" + hex.EncodeToString(b)
}
