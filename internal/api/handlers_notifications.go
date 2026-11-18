package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
	out, err := h.channelsOut(r.Context(), channels)
	if err != nil {
		h.serverError(w, "only_route_for", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createChannelRequest is the POST body (openapi CreateNotificationChannel). recipient,
// recipient_user_id and number belong to the sms and voice types (FR-041a §4.3).
type createChannelRequest struct {
	Type            string            `json:"type" contract:"required"`
	Name            string            `json:"name" contract:"required"`
	Config          map[string]string `json:"config"`
	Enabled         *bool             `json:"enabled"`
	Recipient       *string           `json:"recipient"`
	RecipientUserID *string           `json:"recipient_user_id" contract:"format=uuid"`
	Number          *string           `json:"number"`
}

func (b createChannelRequest) hasPhoneFields() bool {
	return b.Recipient != nil || b.RecipientUserID != nil || b.Number != nil
}

// updateChannelRequest is the PATCH body (openapi UpdateNotificationChannel).
type updateChannelRequest struct {
	Name            *string           `json:"name"`
	Config          map[string]string `json:"config"`
	Enabled         *bool             `json:"enabled"`
	Recipient       *string           `json:"recipient"`
	RecipientUserID *string           `json:"recipient_user_id" contract:"format=uuid"`
	Number          *string           `json:"number"`
}

func (b updateChannelRequest) hasPhoneFields() bool {
	return b.Recipient != nil || b.RecipientUserID != nil || b.Number != nil
}

// errPhoneFieldsOnOtherType answers a phone field sent for one of the four other types.
const errPhoneFieldsOnOtherType = "recipient, recipient_user_id and number belong to sms and voice channels only"

// createChannel creates a notification channel (editor+). An sms or voice channel follows D28 and
// its own gates (createPhoneChannel): the project's visibility first (404), then D28 alone — so a
// token whose list is only project:manage creates a `number` channel (FR-041a review P1-2). The four
// text types keep project:write, judged before the body is read, exactly as before.
func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectVisible(w, r, r.PathValue("projectID"))
	if !ok {
		return
	}
	if !h.mayWriteChannels(r, proj) && !(h.phoneAuthority(r, proj) != phoneAuthNone && phoneChannelBody(r)) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body createChannelRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if domain.IsPhoneChannelType(domain.ChannelType(body.Type)) {
		h.createPhoneChannel(w, r, proj, body)
		return
	}
	if !h.mayWriteChannels(r, proj) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if body.hasPhoneFields() {
		writeError(w, http.StatusBadRequest, errPhoneFieldsOnOtherType)
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

// updateChannel edits a notification channel (editor+): its name, its config, its
// enabled flag, or any combination of the three — every field is optional, and a
// body naming none of them is a 400 rather than a silent no-op. Disabled is not
// deleted — the config survives a pause; delivery skips disabled channels.
//
// `type` is not editable: another type requires another config, which is a new
// channel and not an edit; the strict decoder answers a body carrying one with a
// 400 naming the field. Secrets are merged rather than overwritten
// (domain.MergeChannelConfig) because the API never sends them out, so a form that
// posts back what it was given would blank them.
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
	proj, ok := h.channelProjectAccess(w, r, ch)
	if !ok {
		return
	}
	var body updateChannelRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if domain.IsPhoneChannelType(ch.Type) {
		h.updatePhoneChannel(w, r, ch, proj, body)
		return
	}
	if body.hasPhoneFields() {
		writeError(w, http.StatusBadRequest, errPhoneFieldsOnOtherType)
		return
	}
	if body.Enabled == nil && body.Name == nil && body.Config == nil {
		writeError(w, http.StatusBadRequest, "enabled, name or config is required")
		return
	}
	if body.Name != nil || body.Config != nil {
		edited := ch
		if body.Name != nil {
			edited.Name = strings.TrimSpace(*body.Name)
		}
		if body.Config != nil {
			edited.Config = domain.MergeChannelConfig(ch.Config, body.Config)
		}
		if err := edited.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		updated, err := h.store.UpdateNotificationChannel(r.Context(), edited)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			h.serverError(w, "update_channel", err)
			return
		}
		ch = updated
	}
	if body.Enabled != nil {
		if err := h.store.SetNotificationChannelEnabled(r.Context(), ch.ID, *body.Enabled); err != nil {
			h.serverError(w, "set_channel_enabled", err)
			return
		}
		ch.Enabled = *body.Enabled
	}
	writeJSON(w, http.StatusOK, ch.Redacted())
}

// deleteChannel removes a notification channel (editor+ on its project).
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
	proj, ok := h.channelProjectAccess(w, r, ch)
	if !ok {
		return
	}
	if domain.IsPhoneChannelType(ch.Type) {
		h.deletePhoneChannel(w, r, ch, proj)
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
	out, err := h.channelsOut(r.Context(), channels)
	if err != nil {
		h.serverError(w, "only_route_for", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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

// channelsOut is chs as a channel answer carries them: secrets blanked (redactChannels), and every
// sms and voice channel with its `only_route_for` (FR-041a §4.3), which the store computes with the
// route predicate itself. A list without a phone channel reads nothing more and is answered exactly
// as before (P14).
func (h *Handler) channelsOut(ctx context.Context, chs []domain.NotificationChannel) ([]domain.NotificationChannel, error) {
	out := redactChannels(chs)
	var ids []string
	for _, ch := range out {
		if domain.IsPhoneChannelType(ch.Type) {
			ids = append(ids, ch.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	routes, err := h.store.PhoneChannelsOnlyRouteFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if domain.IsPhoneChannelType(out[i].Type) {
			out[i].SetOnlyRouteFor(routes[out[i].ID])
		}
	}
	return out, nil
}

// writeChannel answers with one channel as channelsOut shapes it, or a 500 when its only_route_for
// cannot be read.
func (h *Handler) writeChannel(w http.ResponseWriter, r *http.Request, status int, ch domain.NotificationChannel) {
	out, err := h.channelsOut(r.Context(), []domain.NotificationChannel{ch})
	if err != nil {
		h.serverError(w, "only_route_for", err)
		return
	}
	writeJSON(w, status, out[0])
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
	if err := h.store.LinkMonitorChannel(r.Context(), mon.ID, body.ChannelID); h.routingReferenceError(w, "link_monitor_channel", err) {
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
