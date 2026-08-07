package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

// resetTokenTTL is how long a password-reset link stays valid.
const resetTokenTTL = time.Hour

// ResetRequestHandler starts a self-service password reset: it emails a one-time
// reset link to the address if it belongs to a local account. The response is
// always 200 with a generic message so it can't be used to enumerate accounts.
func (a *Authenticator) ResetRequestHandler(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimiter.allow(clientIP(r, a.trustedProxies), time.Now()) {
		writeJSONError(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.TrimSpace(body.Email)
	// Always respond the same way; do the work best-effort behind the uniform reply.
	defer func() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}()
	if email == "" || !a.resetEnabled() {
		return
	}
	cred, err := a.store.LocalCredentialByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.logger.Error("reset_lookup_failed", "error", err.Error())
		}
		return // unknown/non-local email → silent (no enumeration)
	}
	token, err := randToken()
	if err != nil {
		a.logger.Error("reset_token_generate_failed", "error", err.Error())
		return
	}
	if err := a.store.CreatePasswordResetToken(r.Context(), cred.UserID, store.HashToken(token), time.Now().Add(resetTokenTTL)); err != nil {
		a.logger.Error("reset_token_store_failed", "error", err.Error())
		return
	}
	link := strings.TrimRight(a.mailer.BaseURL(), "/") + "/reset?token=" + token
	subject := "Reset your cerbix password"
	msg := "Someone requested a password reset for your cerbix account.\n\n" +
		"Open this link to choose a new password (valid for 1 hour):\n" + link +
		"\n\nIf you didn't request this, you can ignore this email."
	if err := a.mailer.Send(email, subject, msg); err != nil {
		a.logger.Error("reset_email_send_failed", "error", err.Error())
		return
	}
	a.logger.Info("password_reset_requested", "user_id", cred.UserID)
}

// ResetConfirmHandler completes a reset: it consumes a valid token and sets the
// new password. Tokens are single-use and expire after resetTokenTTL.
func (a *Authenticator) ResetConfirmHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Live policy (DB → config → default), not the startup config snapshot.
	if len(body.NewPassword) < a.authPolicy().MinPasswordLen {
		writeJSONError(w, http.StatusBadRequest, "new password too short")
		return
	}
	userID, err := a.store.ConsumePasswordResetToken(r.Context(), store.HashToken(strings.TrimSpace(body.Token)))
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusBadRequest, "invalid or expired reset link")
		return
	}
	if err != nil {
		a.logger.Error("reset_consume_failed", "error", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	hash, err := HashPassword(body.NewPassword)
	if err != nil {
		a.logger.Error("reset_hash_failed", "error", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := a.store.SetPassword(r.Context(), userID, hash); err != nil {
		a.logger.Error("reset_set_password_failed", "error", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	a.logger.Info("password_reset_completed", "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// writeJSONError writes a JSON {"error": msg} with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
