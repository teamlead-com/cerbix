package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/deliveryfail"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// A person's own phone number (FR-041a §4.2) and the settings' test SMS (§4.1). A number is never in
// a response, an error, a log line or an audit target (P3): responses carry the mask, errors are
// fixed sentences, audit targets name the user.

// Machine reasons of the phone-number routes (R7).
const (
	reasonTelephonyDisabled     = "telephony_disabled"
	reasonNumberInvalid         = "number_invalid"
	reasonNumberInUse           = "number_in_use"
	reasonCodeIncorrect         = "code_incorrect"
	reasonCodeExpired           = "code_expired"
	reasonCodeAttemptsExhausted = "code_attempts_exhausted"
	reasonSendLimited           = "send_limited"
	reasonNoVerifiedNumber      = "no_verified_number"
	reasonProviderRejected      = "provider_rejected"
	reasonProviderUnreachable   = "provider_unreachable"
	// reasonNoNumber and reasonNumberAlreadyVerified refuse a new code for the stored number
	// (POST /api/v1/me/phone/verify/send, §4.2): there is none, or it needs no code.
	reasonNoNumber              = "no_number"
	reasonNumberAlreadyVerified = "number_already_verified"
	// reasonViaInvalid refuses a send of a code whose `via` is neither "sms" nor "voice" (FR-041a
	// revision 9, §4.2). A `via: voice` the provider cannot place is reasonVoiceUnsupported.
	reasonViaInvalid = "via_invalid"
)

// The two ways a verification code goes (FR-041a revision 9, §4.2): sent by SMS, or spoken by a call.
// It is the same code either way — one hash, ten minutes, five attempts, the same allowances — so
// `via` is never stored: only the send differs.
const (
	codeViaSMS   = "sms"
	codeViaVoice = "voice"
)

// providerSendTimeout bounds one code or test SMS: the request waits for the provider's answer.
const providerSendTimeout = 15 * time.Second

// maxPhoneBody bounds the small JSON bodies of these routes.
const maxPhoneBody = 4 << 10

// setUserPhoneRequest is the PUT /api/v1/me/phone body (openapi SetUserPhone).
type setUserPhoneRequest struct {
	Number string `json:"number" contract:"required"`
	// Via is how the code goes: "sms" (absent or null) or "voice" (FR-041a revision 9, §4.2).
	Via *string `json:"via"`
}

// sendPhoneCodeRequest is the optional body of the two verify/send routes (openapi SendPhoneCode):
// empty, or `{ "via" }` (FR-041a revision 9, §4.2).
type sendPhoneCodeRequest struct {
	Via *string `json:"via"`
}

// verifyPhoneRequest is the body of a code check (openapi VerifyPhone) — a person's own number
// here, a `number` channel's in FR-041a's channel routes.
type verifyPhoneRequest struct {
	Code string `json:"code" contract:"required"`
}

// setUserPhonePreferencesRequest is the PATCH body (openapi SetUserPhonePreferences). An absent
// field is unchanged; `language: null` clears the language. FR-041c adds quiet_hours.
type setUserPhonePreferencesRequest struct {
	Language *string `json:"language"`
}

// telephonyTestSMSResult is the test SMS's answer (openapi TelephonyTestSMSResult): how the provider
// answered, never its text (D34). TransportClass is set only when the provider was not reached — the
// class of the failure from deliveryfail's closed set, never free text (§4.1, FR-041a review P3-3).
type telephonyTestSMSResult struct {
	Outcome        string `json:"outcome"`
	HTTPStatus     int    `json:"http_status"`
	ProviderCode   string `json:"provider_code"`
	TransportClass string `json:"transport_class,omitempty"`
}

// testSMSOutcomes names a provider outcome in the test SMS's answer.
var testSMSOutcomes = map[telephony.Outcome]string{
	telephony.Sent: "sent", telephony.Rejected: "rejected", telephony.Unreachable: "unreachable",
}

// phoneOwner admits a signed-in PERSON: a token principal (an API token or a client-credentials
// machine) and a machine user have no phone (§4.2). It writes the refusal and reports false.
func (h *Handler) phoneOwner(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := h.principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return p, false
	}
	if p.ViaToken {
		writeError(w, http.StatusForbidden, "forbidden")
		return p, false
	}
	u, err := h.store.GetUser(r.Context(), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusForbidden, "forbidden")
		return p, false
	}
	if err != nil {
		h.serverError(w, "get_user", err)
		return p, false
	}
	if u.Kind == domain.UserKindMachine {
		writeError(w, http.StatusForbidden, "forbidden")
		return p, false
	}
	return p, true
}

// phoneProvider resolves the provider for a send from the settings stored now (FR-041a review P2-8),
// or writes the refusal.
func (h *Handler) phoneProvider(w http.ResponseWriter, r *http.Request) (telephony.Provider, bool) {
	if h.phones == nil {
		writeError(w, http.StatusNotImplemented, "phone alerts are not available on this server")
		return nil, false
	}
	p, err := h.phones.CurrentProvider(r.Context())
	switch {
	case err == nil:
		return p, true
	case errors.Is(err, telephony.ErrTelephonyDisabled):
		writeErrorReason(w, http.StatusConflict, reasonTelephonyDisabled, "phone alerts are turned off on this instance")
	case errors.Is(err, telephony.ErrSecretsUnreadable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		h.serverError(w, "telephony_provider", err)
	}
	return nil, false
}

// telephonyEnabled answers from the typed column the route predicates read (D36), or writes the
// error and reports false.
func (h *Handler) telephonyEnabled(w http.ResponseWriter, r *http.Request) (on, ok bool) {
	on, err := h.store.TelephonyEnabled(r.Context())
	if err != nil {
		h.serverError(w, "telephony_enabled", err)
		return false, false
	}
	return on, true
}

// writePhoneError answers a store refusal of the phone routes with its reason, or reports false
// for an error it does not know.
func writePhoneError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no phone number is stored")
	case errors.Is(err, domain.ErrInvalidPhone):
		writeErrorReason(w, http.StatusBadRequest, reasonNumberInvalid, "the number must be in international format, e.g. +14155550100")
	case errors.Is(err, domain.ErrPhoneKeyMissing):
		writeErrorReason(w, http.StatusConflict, reasonEncryptionKeyMissing, err.Error())
	case errors.Is(err, domain.ErrPhoneNotHuman):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, domain.ErrPhoneNumberInUse):
		writeErrorReason(w, http.StatusConflict, reasonNumberInUse, err.Error())
	case errors.Is(err, domain.ErrPhoneSendLimited):
		writeErrorReason(w, http.StatusTooManyRequests, reasonSendLimited, err.Error())
	case errors.Is(err, domain.ErrPhoneCodeIncorrect):
		writeErrorReason(w, http.StatusBadRequest, reasonCodeIncorrect, err.Error())
	case errors.Is(err, domain.ErrPhoneCodeExpired):
		writeErrorReason(w, http.StatusGone, reasonCodeExpired, err.Error())
	case errors.Is(err, domain.ErrPhoneCodeAttemptsExhausted):
		writeErrorReason(w, http.StatusGone, reasonCodeAttemptsExhausted, err.Error())
	case errors.Is(err, domain.ErrPhoneNoNumber):
		writeErrorReason(w, http.StatusConflict, reasonNoNumber, err.Error())
	case errors.Is(err, domain.ErrPhoneAlreadyVerified):
		writeErrorReason(w, http.StatusConflict, reasonNumberAlreadyVerified, err.Error())
	default:
		return false
	}
	return true
}

// codeVia reads a send's `via` (FR-041a revision 9, §4.2): absent or null is "sms"; "sms" and
// "voice" are themselves; anything else is refused with 400 via_invalid before anything is stored,
// issued or sent. It writes the refusal and reports false.
func codeVia(w http.ResponseWriter, via *string) (string, bool) {
	switch {
	case via == nil:
		return codeViaSMS, true
	case *via == codeViaSMS, *via == codeViaVoice:
		return *via, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonViaInvalid, `via must be "sms" or "voice"`)
	return "", false
}

// decodeSendPhoneCode reads the optional body of a verify/send route: an empty body is an absent
// `via`; anything else is one JSON object under decodeJSONBody's rules (JSON content type, the size
// bound, unknown fields refused, one value). It writes the refusal and reports false.
func decodeSendPhoneCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPhoneBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return codeViaSMS, true
	}
	if !hasJSONContentType(w, r) {
		return "", false
	}
	var body sendPhoneCodeRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return "", false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must be a single JSON value")
		return "", false
	}
	return codeVia(w, body.Via)
}

// providerCanSend refuses a code by a call while the provider in force cannot place calls (D3:
// generic_http, or twilio whose `from` is a Messaging Service SID) — 400 voice_unsupported_by_provider,
// decided BEFORE the code is issued, so nothing is stored, no allowance is spent and nothing is sent
// (FR-041a revision 9). There is no fallback to an SMS: the person chooses the way. It writes the
// refusal and reports false.
func providerCanSend(w http.ResponseWriter, p telephony.Provider, via string) bool {
	if via == codeViaVoice && !p.SupportsVoice() {
		writeErrorReason(w, http.StatusBadRequest, reasonVoiceUnsupported,
			"the telephony provider cannot place calls; ask for the code by SMS")
		return false
	}
	return true
}

// codeLanguage is the language a spoken code is in (FR-041a revision 9, §4.2): the person's own when
// the code is for their own number and they set one (personal), otherwise the instance's
// voice_language, read as GET /api/v1/me reads it. A channel's code passes no personal language.
func (h *Handler) codeLanguage(ctx context.Context, personal *string) (string, error) {
	if personal != nil && *personal != "" {
		return *personal, nil
	}
	return h.store.TelephonyVoiceLanguage(ctx)
}

// sendVerificationCode sends the code — by SMS, or spoken by a call in language when via is "voice"
// — the only thing cerbix delivers to an unverified number (P1). A code is not an alert: it writes
// no phone_deliveries row and is outside the storm guard either way (§4.2). For a failed send it
// writes the answer: 502 with the provider outcome as the reason (R7), the same for both ways.
// Returns the provider's answer and whether it accepted the code.
func sendVerificationCode(ctx context.Context, w http.ResponseWriter, p telephony.Provider, number, code, via, language string) (telephony.Result, bool) {
	ctx, cancel := context.WithTimeout(ctx, providerSendTimeout)
	defer cancel()
	var res telephony.Result
	if via == codeViaVoice {
		res = p.PlaceCall(ctx, number, telephony.VerificationCodeVoice(code, language))
	} else {
		res = p.SendSMS(ctx, number, telephony.VerificationCodeText(code))
	}
	switch res.Outcome {
	case telephony.Sent:
		return res, true
	case telephony.Unreachable:
		writeErrorReason(w, http.StatusBadGateway, reasonProviderUnreachable,
			"the telephony provider could not be reached; try again")
	default:
		writeErrorReason(w, http.StatusBadGateway, reasonProviderRejected,
			"the telephony provider refused to send the code")
	}
	return res, false
}

// logCodeSendFailed records a refused or unanswered code send with what the operator needs to act
// on it — the way it went (sms or voice), the provider's outcome, HTTP status and code, all D34-safe
// — and the subject's id. Never the number (P3), whichever way the code went.
func (h *Handler) logCodeSendFailed(subjectKey, subjectID, via string, res telephony.Result) {
	h.logger.Warn("phone_code_send_failed", subjectKey, subjectID, "via", via, "outcome", string(res.Outcome),
		"http_status", res.HTTPStatus, "provider_code", res.Code)
}

func userAuditTarget(userID string) string { return "user " + userID }

// writeMe completes GET /api/v1/me (§4.2): the phone as everyone sees it — the mask (D35) — or null,
// whether the instance's telephony is on, so the SPA can say "phone alerts are off" without waiting
// for a refusal, whether the instance can place calls — telephony on and a provider that calls (D3,
// FR-041a revision 9) — so the form offers a code by a call only when one can be made, and the
// instance's voice language, so the language choice can say what "instance default" means to
// someone who cannot read the settings. It says nothing about any project.
//
// The phone part never fails the page: when any of its reads fails, all four fields degrade
// together — phone null, telephony off, no calls, voice language null — and one line names the user
// and the error's class. Never the error's text, which is not ours to vouch for (P3).
func (h *Handler) writeMe(w http.ResponseWriter, r *http.Request, me map[string]any) {
	p, _ := h.principal(r)
	me["phone"], me["telephony_enabled"], me["telephony_voice_language"] = (*domain.UserPhone)(nil), false, (*string)(nil)
	me["telephony_can_call"] = false
	phone, err := h.store.GetUserPhone(r.Context(), p.UserID)
	if err != nil {
		h.logMePhoneDegraded(p.UserID, "get_user_phone", err)
		writeJSON(w, http.StatusOK, me)
		return
	}
	telephonyOn, err := h.store.TelephonyEnabled(r.Context())
	if err != nil {
		h.logMePhoneDegraded(p.UserID, "telephony_enabled", err)
		writeJSON(w, http.StatusOK, me)
		return
	}
	voiceLanguage, err := h.store.TelephonyVoiceLanguage(r.Context())
	if err != nil {
		h.logMePhoneDegraded(p.UserID, "telephony_voice_language", err)
		writeJSON(w, http.StatusOK, me)
		return
	}
	canCall, err := h.store.TelephonyCanCall(r.Context())
	if err != nil {
		h.logMePhoneDegraded(p.UserID, "telephony_can_call", err)
		writeJSON(w, http.StatusOK, me)
		return
	}
	me["phone"] = phone
	me["telephony_enabled"] = telephonyOn
	me["telephony_voice_language"] = voiceLanguage
	me["telephony_can_call"] = canCall
	writeJSON(w, http.StatusOK, me)
}

func (h *Handler) logMePhoneDegraded(userID, op string, err error) {
	h.logger.Warn("me_phone_degraded", "user_id", userID, "op", op, "error_class", errorClass(err))
}

// errorClass names what kind of failure err is without quoting it: a cancellation, a timeout, a
// database refusal by its SQLSTATE, or a plain error.
func errorClass(err error) string {
	var sqlErr interface{ SQLState() string }
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &sqlErr):
		return "sqlstate_" + sqlErr.SQLState()
	default:
		return "error"
	}
}

// putMyPhone stores the caller's number unverified and sends it a code — by SMS, or spoken by a call
// with `via: voice` (FR-041a revision 9). Refused before anything is stored while telephony is off,
// without the at-rest key, when no provider can be built, and for a call the provider cannot place —
// a number nobody can verify is not kept. A re-PUT of the number already verified is a no-op.
func (h *Handler) putMyPhone(w http.ResponseWriter, r *http.Request) {
	p, ok := h.phoneOwner(w, r)
	if !ok {
		return
	}
	var body setUserPhoneRequest
	if !decodeJSONBody(w, r, maxPhoneBody, &body) {
		return
	}
	via, ok := codeVia(w, body.Via)
	if !ok {
		return
	}
	number, err := domain.NormalizeE164(body.Number)
	if err != nil {
		writePhoneError(w, err)
		return
	}
	on, ok := h.telephonyEnabled(w, r)
	if !ok {
		return
	}
	if !on {
		writeErrorReason(w, http.StatusConflict, reasonTelephonyDisabled, "phone alerts are turned off on this instance")
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
	if via == codeViaVoice && !provider.SupportsVoice() {
		// The answer about the number's state comes first: the verified number again is a no-op 200.
		if verified, err := h.store.VerifiedUserPhoneNumber(r.Context(), p.UserID); err == nil && verified == number {
			if phone, err := h.store.GetUserPhone(r.Context(), p.UserID); err == nil && phone != nil {
				writeJSON(w, http.StatusOK, phone)
				return
			}
		}
	}
	if !providerCanSend(w, provider, via) {
		return
	}
	// The instance's language is read before the code is issued, so a failed read spends nothing.
	// The person's own language comes with the stored phone below: a new number keeps it (§4.2).
	var instanceLanguage string
	if via == codeViaVoice {
		if instanceLanguage, err = h.codeLanguage(r.Context(), nil); err != nil {
			h.serverError(w, "telephony_voice_language", err)
			return
		}
	}
	phone, code, err := h.store.SetUserPhone(r.Context(), p.UserID, number)
	if err != nil {
		if !writePhoneError(w, err) {
			h.serverError(w, "set_user_phone", err)
		}
		return
	}
	if code == "" { // the verified number again: nothing changed, nothing sent
		writeJSON(w, http.StatusOK, phone)
		return
	}
	// The number is stored from here on, whether or not the provider takes the code.
	h.audit(r, "", "user.phone.set", userAuditTarget(p.UserID))
	language := instanceLanguage
	if phone.Language != nil && *phone.Language != "" {
		language = *phone.Language
	}
	if res, sent := sendVerificationCode(r.Context(), w, provider, number, code, via, language); !sent {
		h.logCodeSendFailed("user_id", p.UserID, via, res)
		return
	}
	writeJSON(w, http.StatusOK, phone)
}

// sendMyPhoneCode sends a new code to the caller's STORED unverified number (§4.2): the API never
// returns a number, so after a reload there is nothing to PUT again. Its body is empty or
// `{ "via" }` (FR-041a revision 9). The refusals and the limits are the first send's (putMyPhone, R6)
// — telephony off, no at-rest key, no provider, a call the provider cannot place — plus 409
// `no_number` without a stored number and `number_already_verified` once it is verified.
// No audit row: nothing about the number changes, as the channel's verify/send writes none either.
func (h *Handler) sendMyPhoneCode(w http.ResponseWriter, r *http.Request) {
	p, ok := h.phoneOwner(w, r)
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
		writeErrorReason(w, http.StatusConflict, reasonTelephonyDisabled, "phone alerts are turned off on this instance")
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
	if via == codeViaVoice && !provider.SupportsVoice() {
		// The answers about the number's state come before the call refusal: nothing stored (409
		// no_number) and an already verified number (409 number_already_verified).
		phone, err := h.store.GetUserPhone(r.Context(), p.UserID)
		if err != nil {
			h.serverError(w, "get_user_phone", err)
			return
		}
		if phone == nil {
			writePhoneError(w, domain.ErrPhoneNoNumber)
			return
		}
		if phone.Verified {
			writePhoneError(w, domain.ErrPhoneAlreadyVerified)
			return
		}
	}
	if !providerCanSend(w, provider, via) {
		return
	}
	// The language is settled before the code is issued, so a failed read spends nothing: the
	// person's own when they set one, else the instance's.
	var language string
	if via == codeViaVoice {
		phone, err := h.store.GetUserPhone(r.Context(), p.UserID)
		if err != nil {
			h.serverError(w, "get_user_phone", err)
			return
		}
		var personal *string
		if phone != nil {
			personal = phone.Language
		}
		if language, err = h.codeLanguage(r.Context(), personal); err != nil {
			h.serverError(w, "telephony_voice_language", err)
			return
		}
	}
	number, code, err := h.store.SendUserPhoneCode(r.Context(), p.UserID)
	if err != nil {
		if !writePhoneError(w, err) {
			h.serverError(w, "send_user_phone_code", err)
		}
		return
	}
	if res, sent := sendVerificationCode(r.Context(), w, provider, number, code, via, language); !sent {
		h.logCodeSendFailed("user_id", p.UserID, via, res)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifyMyPhone checks the code sent to the caller's number.
func (h *Handler) verifyMyPhone(w http.ResponseWriter, r *http.Request) {
	p, ok := h.phoneOwner(w, r)
	if !ok {
		return
	}
	var body verifyPhoneRequest
	if !decodeJSONBody(w, r, maxPhoneBody, &body) {
		return
	}
	// Input that cannot be a code is not an attempt: it cannot guess anything.
	if !domain.ValidPhoneCode(body.Code) {
		writeErrorReason(w, http.StatusBadRequest, reasonCodeIncorrect, "the code is six digits")
		return
	}
	phone, err := h.store.VerifyUserPhone(r.Context(), p.UserID, body.Code)
	if err != nil {
		if !writePhoneError(w, err) {
			h.serverError(w, "verify_user_phone", err)
		}
		return
	}
	h.audit(r, "", "user.phone.verify", userAuditTarget(p.UserID))
	writeJSON(w, http.StatusOK, phone)
}

// deleteMyPhone removes the caller's number; their `user` channels stop being routes (P4).
func (h *Handler) deleteMyPhone(w http.ResponseWriter, r *http.Request) {
	p, ok := h.phoneOwner(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteUserPhone(r.Context(), p.UserID); err != nil {
		if !writePhoneError(w, err) {
			h.serverError(w, "delete_user_phone", err)
		}
		return
	}
	h.audit(r, "", "user.phone.delete", userAuditTarget(p.UserID))
	w.WriteHeader(http.StatusNoContent)
}

// patchMyPhonePreferences changes the language of the caller's phone alerts, never the number or
// its verification (§4.2).
func (h *Handler) patchMyPhonePreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := h.phoneOwner(w, r)
	if !ok {
		return
	}
	var body setUserPhonePreferencesRequest
	present, ok := decodeJSONBodyKeys(w, r, maxPhoneBody, &body)
	if !ok {
		return
	}
	if !present["language"] {
		phone, err := h.store.GetUserPhone(r.Context(), p.UserID)
		if err != nil {
			h.serverError(w, "get_user_phone", err)
			return
		}
		if phone == nil {
			writePhoneError(w, store.ErrNotFound)
			return
		}
		writeJSON(w, http.StatusOK, phone)
		return
	}
	if body.Language != nil && !domain.ValidPhoneLanguage(*body.Language) {
		writeError(w, http.StatusBadRequest, "language must be a BCP-47 tag such as en-US, or null")
		return
	}
	phone, err := h.store.SetUserPhoneLanguage(r.Context(), p.UserID, body.Language)
	if err != nil {
		if !writePhoneError(w, err) {
			h.serverError(w, "set_user_phone_language", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, phone)
}

// postTelephonyTestSMS sends the fixed test text to the calling admin's own VERIFIED number (§4.1).
// It is not an alert: no delivery row (R12). The answer is how the provider answered — 200 even for a
// refusal, which is the information the admin asked for.
func (h *Handler) postTelephonyTestSMS(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	p, _ := h.principal(r)
	if p.ViaToken {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	on, ok := h.telephonyEnabled(w, r)
	if !ok {
		return
	}
	if !on {
		writeErrorReason(w, http.StatusConflict, reasonTelephonyDisabled, "phone alerts are turned off on this instance")
		return
	}
	number, err := h.store.VerifiedUserPhoneNumber(r.Context(), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusConflict, reasonNoVerifiedNumber,
			"verify your own phone number in your profile first; the test message goes only there")
		return
	}
	if err != nil {
		// Without the at-rest key the stored number cannot be opened: the 409 every phone route
		// answers for it, not a 500 (Task 7 review, Minor 3).
		if !writePhoneError(w, err) {
			h.serverError(w, "verified_user_phone", err)
		}
		return
	}
	provider, ok := h.phoneProvider(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerSendTimeout)
	defer cancel()
	res := provider.SendSMS(ctx, number, telephony.TestMessageText)
	out := telephonyTestSMSResult{Outcome: testSMSOutcomes[res.Outcome], HTTPStatus: res.HTTPStatus, ProviderCode: res.Code}
	if out.Outcome == "" {
		out.Outcome = testSMSOutcomes[telephony.Rejected]
	}
	if res.Outcome == telephony.Unreachable && res.HTTPStatus == 0 && deliveryfail.IsClass(res.TransportClass) {
		out.TransportClass = res.TransportClass
	}
	h.logEvent(r, "telephony_test_sms", "provider", provider.Name(), "outcome", out.Outcome,
		"http_status", res.HTTPStatus, "provider_code", res.Code, "transport_class", out.TransportClass)
	writeJSON(w, http.StatusOK, out)
}

// decodeJSONBodyKeys is decodeJSONBody for a PATCH whose fields distinguish absent from null: it
// decodes dst under the same rules (JSON content type, size bound, unknown fields refused, one value)
// and also reports which top-level keys the body named.
func decodeJSONBodyKeys(w http.ResponseWriter, r *http.Request, max int64, dst any) (map[string]bool, bool) {
	if !hasJSONContentType(w, r) {
		return nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must be a single JSON value")
		return nil, false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	present := make(map[string]bool, len(keys))
	for k := range keys {
		// encoding/json matches field names case-insensitively; presence must agree with it.
		present[strings.ToLower(k)] = true
	}
	return present, true
}
