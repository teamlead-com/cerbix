package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
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

	// Resolve the visible project set once, through VisibleScope(project:read) — the scoping
	// mirror of Can, allow-list included — rather than by the principal's user id, which for an
	// API token is synthetic and not a uuid (sec-hardening-2 H-3 fix 5). A token whose allow-list
	// omits project:read is refused; nil means no filter (global admin).
	if p.ViaToken && !p.AllowListed(authz.ActionProjectRead) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	allOrgs, orgIDs, projectIDs := p.VisibleScope(authz.ActionProjectRead)
	var visible map[string]bool
	if !allOrgs {
		visible = make(map[string]bool, len(projectIDs))
		for _, id := range projectIDs {
			visible[id] = true
		}
		for _, orgID := range orgIDs {
			projects, err := h.store.ListProjectsByOrg(r.Context(), orgID)
			if err != nil {
				h.serverError(w, "list_projects_by_org", err)
				return
			}
			for _, pr := range projects {
				visible[pr.ID] = true
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	sub, unsub := h.eventSrc.Subscribe()
	defer unsub()

	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			// A real named event, not an SSE comment: comments never reach the
			// browser's EventSource API, so the client watchdog could not tell
			// a healthy-quiet stream from a dead socket. Any bytes keep
			// proxies from idling the connection out.
			if _, err := fmt.Fprint(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
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
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// EventSource subscribes to live status events. Implemented by events.Broker.
type EventSource interface {
	Subscribe() (<-chan events.Event, func())
}
