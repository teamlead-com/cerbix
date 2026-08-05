package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/events"
)

// events streams live monitor status changes as Server-Sent Events, filtered to
// the projects the caller may see. Requires an event source (SSE enabled).
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if h.eventSrc == nil {
		writeError(w, http.StatusNotImplemented, "realtime events are not enabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	p, _ := h.principal(r)

	// Resolve the visible project set once. Global admins see everything (nil
	// filter); others are limited to the projects they belong to.
	var visible map[string]bool
	if !p.IsGlobalAdmin {
		projects, err := h.store.ListProjectsForUser(r.Context(), p.UserID)
		if err != nil {
			h.serverError(w, "list_projects_for_user", err)
			return
		}
		visible = make(map[string]bool, len(projects))
		for _, pr := range projects {
			visible[pr.ID] = true
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	sub, unsub := h.eventSrc.Subscribe()
	defer unsub()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if visible != nil && !visible[ev.ProjectID] {
				continue // not visible to this caller
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}

// EventSource subscribes to live status events. Implemented by events.Broker.
type EventSource interface {
	Subscribe() (<-chan events.Event, func())
}
