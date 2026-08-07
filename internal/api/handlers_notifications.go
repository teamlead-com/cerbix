package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// listChannels lists a project's notification channels (viewer+).
func (h *Handler) listChannels(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	channels, err := h.store.ListChannelsByProject(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_channels", err)
		return
	}
	writeJSON(w, http.StatusOK, redactChannels(channels))
}

// createChannel creates a notification channel (editor+).
func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Type    string            `json:"type"`
		Name    string            `json:"name"`
		Config  map[string]string `json:"config"`
		Enabled *bool             `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	ch := domain.NotificationChannel{
		ProjectID: proj.ID,
		Type:      domain.ChannelType(body.Type),
		Name:      body.Name,
		Config:    body.Config,
		Enabled:   enabled,
	}
	if err := ch.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.store.CreateNotificationChannel(r.Context(), ch)
	if err != nil {
		h.serverError(w, "create_channel", err)
		return
	}
	writeJSON(w, http.StatusCreated, created.Redacted())
}

// deleteChannel removes a notification channel (editor+ on its project).
// updateChannel toggles a notification channel (editor+). Disabled is not
// deleted — the config survives a pause; delivery skips disabled channels.
func (h *Handler) updateChannel(w http.ResponseWriter, r *http.Request) {
	ch, err := h.store.GetNotificationChannel(r.Context(), r.PathValue("channelID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_channel", err)
		return
	}
	if _, ok := h.projectAccess(w, r, ch.ProjectID, authz.ActionProjectWrite); !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := h.store.SetNotificationChannelEnabled(r.Context(), ch.ID, *body.Enabled); err != nil {
		h.serverError(w, "set_channel_enabled", err)
		return
	}
	ch.Enabled = *body.Enabled
	writeJSON(w, http.StatusOK, ch.Redacted())
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	ch, err := h.store.GetNotificationChannel(r.Context(), r.PathValue("channelID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_channel", err)
		return
	}
	if _, ok := h.projectAccess(w, r, ch.ProjectID, authz.ActionProjectWrite); !ok {
		return
	}
	if err := h.store.DeleteNotificationChannel(r.Context(), ch.ID); err != nil {
		h.serverError(w, "delete_channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listMonitorChannels lists the channels linked to a monitor (viewer+).
func (h *Handler) listMonitorChannels(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	channels, err := h.store.ListMonitorChannels(r.Context(), mon.ID)
	if err != nil {
		h.serverError(w, "list_monitor_channels", err)
		return
	}
	writeJSON(w, http.StatusOK, redactChannels(channels))
}

// redactChannels blanks secret config values in a channel slice before it is
// returned to a client. List responses are visible to viewers, so decrypted
// credentials (bot tokens, SMTP passwords, secret-bearing webhook URLs) must
// never leave the server in them.
func redactChannels(chs []domain.NotificationChannel) []domain.NotificationChannel {
	out := make([]domain.NotificationChannel, len(chs))
	for i, ch := range chs {
		out[i] = ch.Redacted()
	}
	return out
}

// linkMonitorChannel links a channel to a monitor (editor+). The channel must be
// in the monitor's project.
func (h *Handler) linkMonitorChannel(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		ChannelID string `json:"channel_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ch, err := h.store.GetNotificationChannel(r.Context(), body.ChannelID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ch.ProjectID != mon.ProjectID) {
		writeError(w, http.StatusBadRequest, "channel is not in this monitor's project")
		return
	}
	if err != nil {
		h.serverError(w, "get_channel", err)
		return
	}
	if err := h.store.LinkMonitorChannel(r.Context(), mon.ID, ch.ID); err != nil {
		h.serverError(w, "link_monitor_channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unlinkMonitorChannel removes a monitor↔channel link (editor+).
func (h *Handler) unlinkMonitorChannel(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if err := h.store.UnlinkMonitorChannel(r.Context(), mon.ID, r.PathValue("channelID")); err != nil {
		h.serverError(w, "unlink_monitor_channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
