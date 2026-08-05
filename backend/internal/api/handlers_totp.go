package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"git.example.com/monitoring/cerbix/internal/auth"
	"git.example.com/monitoring/cerbix/internal/store"
	"git.example.com/monitoring/cerbix/internal/totp"
)

const totpIssuer = "cerbix"

// totpEnroll generates a pending TOTP secret for the current user (2FA not yet
// enabled) and returns it plus an otpauth:// URI for the authenticator app.
func (h *Handler) totpEnroll(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	if p.ViaToken {
		writeError(w, http.StatusForbidden, "2FA is for interactive local accounts")
		return
	}
	// Only local accounts have a password; OIDC users get MFA from their provider.
	if _, err := h.store.PasswordHashByID(r.Context(), p.UserID); err != nil {
		writeError(w, http.StatusBadRequest, "two-factor auth is only for local accounts")
		return
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		h.serverError(w, "totp_generate", err)
		return
	}
	if err := h.store.SetTOTPSecret(r.Context(), p.UserID, secret); err != nil {
		h.serverError(w, "totp_set_secret", err)
		return
	}
	u, _ := h.store.GetUser(r.Context(), p.UserID)
	account := u.Email
	if account == "" {
		account = p.UserID
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": totp.URI(secret, account, totpIssuer)})
}

// totpEnable confirms enrollment by verifying a code against the pending secret,
// enables 2FA, and returns freshly-generated single-use recovery codes.
func (h *Handler) totpEnable(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	var body struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	secret, enabled, err := h.store.GetTOTP(r.Context(), p.UserID)
	if err != nil || secret == "" {
		writeError(w, http.StatusBadRequest, "start enrollment first")
		return
	}
	if enabled {
		writeError(w, http.StatusBadRequest, "two-factor auth is already enabled")
		return
	}
	if !totp.Validate(secret, body.Code) {
		writeError(w, http.StatusBadRequest, "code did not match — check the time on your device")
		return
	}
	if err := h.store.EnableTOTP(r.Context(), p.UserID); err != nil {
		h.serverError(w, "totp_enable", err)
		return
	}
	codes, hashes, err := generateRecoveryCodes(8)
	if err != nil {
		h.serverError(w, "totp_recovery_generate", err)
		return
	}
	if err := h.store.ReplaceRecoveryCodes(r.Context(), p.UserID, hashes); err != nil {
		h.serverError(w, "totp_recovery_store", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// totpDisable turns off 2FA after re-verifying the account password.
func (h *Handler) totpDisable(w http.ResponseWriter, r *http.Request) {
	p, _ := h.principal(r)
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	hash, err := h.store.PasswordHashByID(r.Context(), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "not a local account")
		return
	}
	if err != nil {
		h.serverError(w, "password_hash", err)
		return
	}
	if ok, verr := auth.VerifyPassword(hash, body.Password); verr != nil || !ok {
		writeError(w, http.StatusBadRequest, "incorrect password")
		return
	}
	if err := h.store.DisableTOTP(r.Context(), p.UserID); err != nil {
		h.serverError(w, "totp_disable", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// generateRecoveryCodes returns n random codes plus their store hashes.
func generateRecoveryCodes(n int) (codes, hashes []string, err error) {
	for i := 0; i < n; i++ {
		b := make([]byte, 5)
		if _, err = rand.Read(b); err != nil {
			return nil, nil, err
		}
		code := hex.EncodeToString(b) // 10 hex chars
		codes = append(codes, code)
		hashes = append(hashes, store.HashToken(code))
	}
	return codes, hashes, nil
}
