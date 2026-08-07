package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/totp"
)

// verifyPasswordFn is the password check, a var so tests can assert it is invoked on
// BOTH the wrong-password and the unknown-user (decoy) paths — the anti-enumeration
// timing guarantee — without flaky wall-clock measurement.
var verifyPasswordFn = VerifyPassword

// secondFactorOK validates a login's second factor: a current TOTP code, or a
// single-use recovery code (consumed on match).
func (a *Authenticator) secondFactorOK(ctx context.Context, cred store.LocalCredential, input string) bool {
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	if totp.Validate(cred.TOTPSecret, input) {
		return true
	}
	ok, err := a.store.ConsumeRecoveryCode(ctx, cred.UserID, store.HashToken(input))
	if err != nil {
		a.logger.Error("consume_recovery_code_failed", "error", err.Error())
		return false
	}
	return ok
}

// LocalLoginHandler authenticates a username(email)/password pair and, on
// success, issues a session cookie. Failures return a uniform 401 to avoid user
// enumeration; excessive attempts from one IP get 429.
func (a *Authenticator) LocalLoginHandler(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimiter.allow(clientIP(r, a.trustedProxies), time.Now()) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "too many login attempts, try again later"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"` // 6-digit TOTP or a recovery code, when 2FA is enabled
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(body.Username)
	cred, err := a.store.LocalCredentialByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		// Unknown user: still run the SAME Argon2id verification against a decoy hash
		// so this path costs the same time as a wrong password on a real account —
		// otherwise the skipped hash makes a fast 401 a user-enumeration timing oracle
		// (CWE-203). Result is discarded; the decoy can never authenticate.
		_, _ = verifyPasswordFn(a.decoyHash, body.Password)
		unauthorized(w)
		return
	}
	if err != nil {
		a.logger.Error("local_credential_lookup_failed", "error", err.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	// Known user: a session is issued ONLY when the real hash verifies (structurally,
	// the decoy path above can never reach here).
	ok, verr := verifyPasswordFn(cred.PasswordHash, body.Password)
	if verr != nil || !ok {
		unauthorized(w)
		return
	}
	pol := a.authPolicy()
	// Instance policy: restrict which email domains may sign in.
	if !pol.EmailAllowed(email) {
		unauthorized(w)
		return
	}
	// Second factor, when enrolled: a valid TOTP or an unused recovery code.
	if cred.TOTPEnabled && !a.secondFactorOK(r.Context(), cred, body.TOTP) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "two-factor code required", "totp_required": true})
		return
	}
	// Instance policy: TOTP may be mandatory even when not yet enrolled.
	if !cred.TOTPEnabled && pol.RequireTOTP != domain.TOTPNone {
		if u, uerr := a.store.GetUser(r.Context(), cred.UserID); uerr == nil && pol.TOTPRequiredFor(u.IsGlobalAdmin) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "two-factor is required for your account — set it up to sign in", "totp_setup_required": true})
			return
		}
	}
	if err := a.issueSession(r.Context(), w, cred.UserID); err != nil {
		a.logger.Error("session_create_failed", "error", err.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	a.logger.Info("local_login_success", "user_id", cred.UserID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// EnsureBootstrapAdmin creates the configured local global-admin account when
// local login is enabled, a bootstrap email+password are set, and no users
// exist yet. It never generates or logs a password.
func (a *Authenticator) EnsureBootstrapAdmin(ctx context.Context) error {
	if !a.local || a.localBootEmail == "" {
		return nil
	}
	if a.localBootPassword == "" {
		a.logger.Info("bootstrap_admin_skipped", "reason", "security.admin_password not set")
		return nil
	}
	n, err := a.store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := HashPassword(a.localBootPassword)
	if err != nil {
		return err
	}
	u, err := a.store.CreateLocalUser(ctx, a.localBootEmail, "Administrator", hash, true)
	if err != nil {
		return err
	}
	a.logger.Info("bootstrap_admin_created", "email", a.localBootEmail, "user_id", u.ID)
	return nil
}
