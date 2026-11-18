package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// The sms and voice channels (FR-041a §4.2, §4.3, §9). Who may hold which channel is D28; the gates
// (telephony on, D27; no voice without calls, D3; the at-rest key, D29), the recipient's eligibility
// and the audit rows are the store's, in the write's transaction. No response, error or log line
// carries a number (P3): a channel answers with its mask.

// Machine reasons of the phone-channel routes (R7), beside those of handlers_phone.go.
const (
	reasonVoiceUnsupported     = "voice_unsupported_by_provider"
	reasonRecipientNotEligible = "recipient_not_eligible"
	reasonRecipientFixed       = "recipient_fixed"
	reasonNumberUnverified     = "number_unverified"
)

// phoneAuthority is D28: what a principal may do with the phone channels of one project.
type phoneAuthority int

const (
	phoneAuthNone phoneAuthority = iota
	// phoneAuthSelf: a signed-in editor (project:write) — a `user` channel that phones themselves.
	phoneAuthSelf
	// phoneAuthAny: a signed-in project admin, org admin or global admin (project:manage) — any
	// eligible recipient, and `number` channels.
	phoneAuthAny
	// phoneAuthNumberOnly: an API token (or a client-credentials machine) with project:manage. A token
	// is not a person and has no number, so it never names one: `number` channels only.
	phoneAuthNumberOnly
)

func phoneChannelAuthority(p authz.Principal, proj domain.Project) phoneAuthority {
	manage := p.Can(authz.ActionProjectManage, proj.OrgID, proj.ID)
	switch {
	case p.ViaToken && manage:
		return phoneAuthNumberOnly
	case p.ViaToken:
		return phoneAuthNone
	case manage:
		return phoneAuthAny
	case p.Can(authz.ActionProjectWrite, proj.OrgID, proj.ID):
		return phoneAuthSelf
	}
	return phoneAuthNone
}

// projectVisible is projectAccess's first half, for the channel writes whose rights are not one action
// (FR-041a review P1-2): the project loaded, and 404 — the answer an unknown project gets — when the
// principal cannot see it. The caller judges the rights itself.
func (h *Handler) projectVisible(w http.ResponseWriter, r *http.Request, projectID string) (domain.Project, bool) {
	p, _ := h.principal(r)
	proj, err := h.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Project{}, false
	}
	if err != nil {
		h.serverError(w, "get_project", err)
		return domain.Project{}, false
	}
	if !p.VisibleProject(proj.OrgID, proj.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Project{}, false
	}
	return proj, true
}

// phoneAuthority is phoneChannelAuthority for the request's principal.
func (h *Handler) phoneAuthority(r *http.Request, proj domain.Project) phoneAuthority {
	p, _ := h.principal(r)
	return phoneChannelAuthority(p, proj)
}

// mayWriteChannels is the four text types' right on a project's channels: project:write.
func (h *Handler) mayWriteChannels(r *http.Request, proj domain.Project) bool {
	p, _ := h.principal(r)
	return p.Can(authz.ActionProjectWrite, proj.OrgID, proj.ID)
}

// channelProjectAccess is the gate of a stored channel's update and delete (FR-041a review P1-2): the
// project's visibility first (404); then, for an sms or voice channel, D28 alone — a principal with no
// phone authority at all is 403 before its body is read, and the rest is judged against the row in the
// store's transaction — and for the four text types project:write, exactly as before.
func (h *Handler) channelProjectAccess(w http.ResponseWriter, r *http.Request, ch domain.NotificationChannel) (domain.Project, bool) {
	proj, ok := h.projectVisible(w, r, ch.ProjectID)
	if !ok {
		return domain.Project{}, false
	}
	allowed := h.mayWriteChannels(r, proj)
	if domain.IsPhoneChannelType(ch.Type) {
		allowed = h.phoneAuthority(r, proj) != phoneAuthNone
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden")
		return domain.Project{}, false
	}
	return proj, true
}

// phoneChannelBody reports whether the request's body names an sms or voice type, and leaves the body
// to be read again. It is consulted only for a principal that may hold phone channels but lacks
// project:write (a token whose list is project:manage alone): every other body is refused as before,
// before it is decoded. A body that is not JSON names no type.
func phoneChannelBody(r *http.Request) bool {
	raw, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	var peek struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return false
	}
	return domain.IsPhoneChannelType(domain.ChannelType(peek.Type))
}

// mayHold reports whether the authority may create, edit or delete a channel that phones recipient
// in mode — judged against the channel as stored and as patched (D28).
func (a phoneAuthority) mayHold(p authz.Principal, mode, recipient string) bool {
	switch a {
	case phoneAuthAny:
		return true
	case phoneAuthNumberOnly:
		return mode == domain.PhoneRecipientNumber
	case phoneAuthSelf:
		return mode == domain.PhoneRecipientUser && recipient != "" && recipient == p.UserID
	}
	return false
}

// errPhoneChannelForbidden is the handler's D28 refusal, raised inside the store's transaction.
var errPhoneChannelForbidden = errors.New("you may not manage this phone channel")

// writePhoneChannelError answers a refusal of the phone-channel writes, or reports false for an
// error it does not know.
func writePhoneChannelError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errPhoneChannelForbidden):
		writeError(w, http.StatusForbidden, "only a project admin manages a channel that phones someone else or a number of its own; "+
			"an editor manages the one that phones themselves, and an API token only channels with their own number")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrPhoneTelephonyDisabled):
		writeErrorReason(w, http.StatusConflict, reasonTelephonyDisabled, err.Error())
	case errors.Is(err, domain.ErrPhoneVoiceUnsupported):
		writeErrorReason(w, http.StatusBadRequest, reasonVoiceUnsupported, err.Error())
	case errors.Is(err, domain.ErrPhoneRecipientNotEligible):
		writeErrorReason(w, http.StatusBadRequest, reasonRecipientNotEligible, err.Error())
	case errors.Is(err, domain.ErrPhoneRecipientFixed):
		writeErrorReason(w, http.StatusBadRequest, reasonRecipientFixed, err.Error())
	case errors.Is(err, domain.ErrPhoneNumberUnverified):
		writeErrorReason(w, http.StatusConflict, reasonNumberUnverified, err.Error())
	case errors.Is(err, domain.ErrPhoneChannelNotNumber):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		return writePhoneError(w, err)
	}
	return true
}

// otherModeField reports whether a body names a field of the recipient mode the channel is not in: a
// `number` for a `user` channel, a `recipient_user_id` for a `number` one. Presence decides, not the
// value — an empty field of the other mode is as wrong as a full one (FR-041a review P2-9).
func otherModeField(mode string, recipientUserID, number *string) bool {
	switch mode {
	case domain.PhoneRecipientUser:
		return number != nil
	case domain.PhoneRecipientNumber:
		return recipientUserID != nil
	}
	return false
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// createPhoneChannel creates an sms or voice channel. A `number` channel is created disabled and
// is verified through verify/send and verify (§4.2); nothing is sent here.
func (h *Handler) createPhoneChannel(w http.ResponseWriter, r *http.Request, proj domain.Project, body createChannelRequest) {
	p, _ := h.principal(r)
	if len(body.Config) > 0 {
		writeError(w, http.StatusBadRequest, "an sms or voice channel carries no config")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	ch := domain.NotificationChannel{ProjectID: proj.ID, Type: domain.ChannelType(body.Type), Name: strings.TrimSpace(body.Name),
		Enabled: enabled, Recipient: strOr(body.Recipient), RecipientUserID: strOr(body.RecipientUserID)}
	if otherModeField(ch.Recipient, body.RecipientUserID, body.Number) {
		writePhoneChannelError(w, domain.ErrPhoneRecipientFixed)
		return
	}
	if err := ch.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ch.Recipient == domain.PhoneRecipientUser && strings.TrimSpace(ch.RecipientUserID) == "" {
		writePhoneChannelError(w, domain.ErrPhoneRecipientNotEligible)
		return
	}
	if !phoneChannelAuthority(p, proj).mayHold(p, ch.Recipient, ch.RecipientUserID) {
		writePhoneChannelError(w, errPhoneChannelForbidden)
		return
	}
	number := strOr(body.Number)
	if ch.Recipient == domain.PhoneRecipientNumber {
		normalized, err := domain.NormalizeE164(number)
		if err != nil {
			writePhoneError(w, err)
			return
		}
		number = normalized
	}
	created, err := h.store.CreatePhoneChannel(r.Context(), ch, number, auditActor(p))
	if err != nil {
		if !writePhoneChannelError(w, err) {
			h.serverError(w, "create_phone_channel", err)
		}
		return
	}
	h.writeChannel(w, r, http.StatusCreated, created)
}

// updatePhoneChannel edits an sms or voice channel: name, enabled, the recipient of a `user`
// channel, the number of a `number` one. The recipient mode is fixed (recipient_fixed). D28 is
// judged in the store's transaction against the row as locked and as patched.
func (h *Handler) updatePhoneChannel(w http.ResponseWriter, r *http.Request, ch domain.NotificationChannel, proj domain.Project, body updateChannelRequest) {
	p, _ := h.principal(r)
	if len(body.Config) > 0 {
		writeError(w, http.StatusBadRequest, "an sms or voice channel carries no config")
		return
	}
	// The mode is fixed: a different `recipient`, or a field of the other mode — present, even empty —
	// is refused before any value is looked at (FR-041a review P2-9). A `recipient` that repeats the
	// mode changes nothing and does not make a PATCH non-empty.
	if (body.Recipient != nil && *body.Recipient != ch.Recipient) || otherModeField(ch.Recipient, body.RecipientUserID, body.Number) {
		writePhoneChannelError(w, domain.ErrPhoneRecipientFixed)
		return
	}
	if body.Name == nil && body.Enabled == nil && body.RecipientUserID == nil && body.Number == nil {
		writeError(w, http.StatusBadRequest, "name, enabled, recipient_user_id or number is required")
		return
	}
	if body.RecipientUserID != nil && strings.TrimSpace(*body.RecipientUserID) == "" {
		writePhoneChannelError(w, domain.ErrPhoneRecipientNotEligible)
		return
	}
	patch := store.PhoneChannelPatch{Name: body.Name, Enabled: body.Enabled, RecipientUserID: body.RecipientUserID}
	if body.Number != nil {
		normalized, err := domain.NormalizeE164(*body.Number)
		if err != nil {
			writePhoneError(w, err)
			return
		}
		patch.Number = &normalized
	}
	if patch.Name != nil && strings.TrimSpace(*patch.Name) == "" {
		writeError(w, http.StatusBadRequest, "notification channel: name is required")
		return
	}
	authority := phoneChannelAuthority(p, proj)
	authorize := func(stored domain.NotificationChannel) error {
		next := stored.RecipientUserID
		if body.RecipientUserID != nil {
			next = *body.RecipientUserID
		}
		if !authority.mayHold(p, stored.Recipient, stored.RecipientUserID) || !authority.mayHold(p, stored.Recipient, next) {
			return errPhoneChannelForbidden
		}
		return nil
	}
	updated, err := h.store.UpdatePhoneChannel(r.Context(), ch.ID, patch, auditActor(p), authorize)
	if err != nil {
		if !writePhoneChannelError(w, err) {
			h.serverError(w, "update_phone_channel", err)
		}
		return
	}
	h.writeChannel(w, r, http.StatusOK, updated)
}

// deletePhoneChannel deletes an sms or voice channel under D28.
func (h *Handler) deletePhoneChannel(w http.ResponseWriter, r *http.Request, ch domain.NotificationChannel, proj domain.Project) {
	p, _ := h.principal(r)
	authority := phoneChannelAuthority(p, proj)
	err := h.store.DeletePhoneChannel(r.Context(), ch.ID, auditActor(p), func(stored domain.NotificationChannel) error {
		if !authority.mayHold(p, stored.Recipient, stored.RecipientUserID) {
			return errPhoneChannelForbidden
		}
		return nil
	})
	if err != nil {
		if !writePhoneChannelError(w, err) {
			h.serverError(w, "delete_phone_channel", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// numberChannelAccess loads the channel of the path and admits project:manage on its project, the
// permission of a `number` channel's verification (§9). Anything but a `number` channel is a 400.
func (h *Handler) numberChannelAccess(w http.ResponseWriter, r *http.Request) (domain.NotificationChannel, authz.Principal, bool) {
	p, _ := h.principal(r)
	ch, err := h.store.GetNotificationChannel(r.Context(), r.PathValue("channelID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return ch, p, false
	}
	if err != nil {
		h.serverError(w, "get_channel", err)
		return ch, p, false
	}
	if _, ok := h.projectAccess(w, r, ch.ProjectID, authz.ActionProjectManage); !ok {
		return ch, p, false
	}
	if !domain.IsPhoneChannelType(ch.Type) || ch.Recipient != domain.PhoneRecipientNumber {
		writePhoneChannelError(w, domain.ErrPhoneChannelNotNumber)
		return ch, p, false
	}
	return ch, p, true
}

// sendChannelVerificationCode sends a code to a `number` channel's number — by SMS, or spoken by a
// call in the instance's voice_language with `via: voice` (FR-041a revision 9) — the only thing
// cerbix delivers to an unverified number (P1). Its body is empty or `{ "via" }`. Refused while
// telephony is off, without the at-rest key, for a call the provider cannot place (before any code
// is issued), and with 409 `number_already_verified` for a verified channel (§4.2). The code is asked
// for by the request's principal — a person's user id, an API token's synthetic id — and spends that
// requester's hourly limit (FR-041a review P2-3), whichever way it goes.
func (h *Handler) sendChannelVerificationCode(w http.ResponseWriter, r *http.Request) {
	ch, p, ok := h.numberChannelAccess(w, r)
	if !ok {
		return
	}
	via, ok := decodeSendPhoneCode(w, r)
	if !ok {
		return
	}
	on, ok := h.telephonyEnabled(w, r)
	if !ok {
		return
	}
	if !on {
		writePhoneChannelError(w, domain.ErrPhoneTelephonyDisabled)
		return
	}
	if !h.store.EncryptionKeyConfigured() {
		writePhoneError(w, domain.ErrPhoneKeyMissing)
		return
	}
	provider, ok := h.phoneProvider(w, r)
	if !ok {
		return
	}
	if ch.Recipient == domain.PhoneRecipientNumber && ch.NumberVerified != nil && *ch.NumberVerified {
		// An already verified number is answered 409 before a call the provider cannot place is refused.
		if via == codeViaVoice && !provider.SupportsVoice() {
			writePhoneError(w, domain.ErrPhoneAlreadyVerified)
			return
		}
	}
	if !providerCanSend(w, provider, via) {
		return
	}
	// A channel's spoken code is in the instance's language (§4.2), read before the code is issued.
	var language string
	if via == codeViaVoice {
		var err error
		if language, err = h.codeLanguage(r.Context(), nil); err != nil {
			h.serverError(w, "telephony_voice_language", err)
			return
		}
	}
	number, code, err := h.store.SendChannelVerificationCode(r.Context(), ch.ID, p.UserID)
	if err != nil {
		if !writePhoneChannelError(w, err) {
			h.serverError(w, "send_channel_code", err)
		}
		return
	}
	if res, sent := sendVerificationCode(r.Context(), w, provider, number, code, via, language); !sent {
		h.logCodeSendFailed("channel_id", ch.ID, via, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifyChannelNumber checks the code sent to a `number` channel; a match verifies the number and
// is audited. The channel stays as enabled as it was.
func (h *Handler) verifyChannelNumber(w http.ResponseWriter, r *http.Request) {
	ch, p, ok := h.numberChannelAccess(w, r)
	if !ok {
		return
	}
	var body verifyPhoneRequest
	if !decodeJSONBody(w, r, maxPhoneBody, &body) {
		return
	}
	if !domain.ValidPhoneCode(body.Code) {
		writeErrorReason(w, http.StatusBadRequest, reasonCodeIncorrect, "the code is six digits")
		return
	}
	verified, err := h.store.VerifyChannelNumber(r.Context(), ch.ID, body.Code, auditActor(p))
	if err != nil {
		if !writePhoneChannelError(w, err) {
			h.serverError(w, "verify_channel_number", err)
		}
		return
	}
	h.writeChannel(w, r, http.StatusOK, verified)
}

// listPhoneRecipients answers whom a `user` channel of the project may phone (§4.3): everyone
// eligible for a project admin, the editor themselves for an editor, nobody for a token (D28).
func (h *Handler) listPhoneRecipients(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	p, _ := h.principal(r)
	all, err := h.store.ListPhoneRecipients(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_phone_recipients", err)
		return
	}
	authority := phoneChannelAuthority(p, proj)
	out := make([]domain.PhoneRecipient, 0, len(all))
	for _, rec := range all {
		if authority.mayHold(p, domain.PhoneRecipientUser, rec.UserID) {
			out = append(out, rec)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
