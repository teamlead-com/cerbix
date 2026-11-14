package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Routes registers the auth endpoints on mux, conditionally on which methods are
// enabled.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/config", a.ConfigHandler)
	// OIDC routes are always registered: the provider may be (re)configured at
	// runtime from the Settings UI. They return 503 while OIDC is inactive.
	mux.HandleFunc("GET /auth/login", a.LoginHandler)
	mux.HandleFunc("GET /auth/callback", a.CallbackHandler)
	if a.local {
		mux.Handle("POST /auth/local/login", capBody(a.LocalLoginHandler))
		mux.Handle("POST /auth/local/reset/request", capBody(a.ResetRequestHandler))
		mux.Handle("POST /auth/local/reset/confirm", capBody(a.ResetConfirmHandler))
	}
	mux.HandleFunc("POST /auth/logout", a.LogoutHandler)
	// No GET changes state (sec-hardening-2 H-4 fix 3): a GET /auth/logout — an <img src> on any
	// page could fire it — is answered 405 explicitly. Merely not registering it is not enough: the
	// application mux ends in the SPA catch-all, which would answer it 200 with index.html.
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
	})
}

// localAuthMaxBody caps the body of every unauthenticated /auth/local/* request
// (sec-hardening-2 H-6): their JSON is a few short fields. The cap wraps the reader, so it bites
// only when a handler reads — the per-IP limiter still runs before any read.
const localAuthMaxBody = 4 << 10 // 4 KiB

func capBody(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, localAuthMaxBody)
		h(w, r)
	})
}

// ConfigHandler reports which sign-in methods are enabled and the OIDC button
// label, so the login page can render itself without hardcoding a provider.
// Public (no session required).
func (a *Authenticator) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	label := "Continue with SSO"
	if rt := a.rt(); rt != nil && strings.TrimSpace(rt.buttonLabel) != "" {
		label = rt.buttonLabel
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"local":             a.local,
		"oidc":              a.oidcEnabled(),
		"oidc_button_label": label,
		"password_reset":    a.resetEnabled(),
	})
}

// LoginHandler starts the OIDC Authorization Code + PKCE flow.
func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	rt := a.rt()
	if rt == nil {
		http.Error(w, "sso is not available", http.StatusServiceUnavailable)
		return
	}
	state, err := randToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	flow := store.AuthFlow{
		State:        state,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		RedirectTo:   sanitizeRedirect(r.URL.Query().Get("redirect")),
		ExpiresAt:    time.Now().Add(authFlowTTL),
	}
	if err := a.store.CreateAuthFlow(r.Context(), flow); err != nil {
		a.logger.Error("auth_flow_create_failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	url := rt.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

// CallbackHandler completes the flow: exchange code, verify the ID token, JIT
// provision the user, create a session, and set the cookie.
func (a *Authenticator) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	rt := a.rt()
	if rt == nil {
		http.Error(w, "sso is not available", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "oidc error: "+e, http.StatusBadRequest)
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}

	flow, err := a.store.TakeAuthFlow(ctx, state)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "invalid or expired login state", http.StatusBadRequest)
		return
	}
	if err != nil {
		a.logger.Error("auth_flow_take_failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Exchange over the bounded OIDC client so a hung token endpoint fails fast
	// instead of blocking on http.DefaultClient's zero timeout.
	if rt.httpClient != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, rt.httpClient)
	}
	oauthToken, err := rt.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.PKCEVerifier))
	if err != nil {
		a.logger.Error("token_exchange_failed", "error", err.Error())
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := oauthToken.Extra("id_token").(string)
	if !ok || rawID == "" {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idToken, err := rt.verifier.Verify(ctx, rawID)
	if err != nil {
		a.logger.Error("id_token_verify_failed", "error", err.Error())
		http.Error(w, "invalid id token", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != flow.Nonce {
		http.Error(w, "nonce mismatch", http.StatusUnauthorized)
		return
	}

	var claims struct {
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "invalid claims", http.StatusUnauthorized)
		return
	}
	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}
	pol := a.authPolicy()
	// Instance policy: restrict which email domains may sign in via SSO. When an
	// allowlist is configured it is FAIL-CLOSED — a token with no email, an
	// unverified email, or a non-matching domain is rejected. (Without this, a token
	// that simply omits the email claim would slip past the allowlist and JIT-create
	// an empty-email user.)
	if pol.EmailAllowlistEnforced() {
		if claims.Email == "" || !claims.EmailVerified || !pol.EmailAllowed(claims.Email) {
			http.Error(w, "a verified, permitted email is required to sign in", http.StatusForbidden)
			return
		}
	}

	user, flip, err := a.store.UpsertHumanUserByOIDCIdentity(ctx, idToken.Issuer, idToken.Subject, claims.Email, displayName)
	if err != nil {
		a.logger.Error("user_provision_failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if flip.Flipped {
		// sec-hardening-2 H-1: a person signed in to an identity a client-credentials token had
		// provisioned as a machine. Its grants are gone (audited in the same transaction); say so.
		a.logger.Warn("oidc_machine_became_human", "user_id", user.ID,
			"revoked_memberships", len(flip.Revoked), "was_global_admin", flip.WasGlobalAdmin)
	}
	// Bootstrap-admin grant requires a VERIFIED email so an unverified email claim
	// matching a bootstrap address can't self-escalate to global admin.
	if claims.Email != "" && claims.EmailVerified && rt.bootstrapAdmin[claims.Email] && !user.IsGlobalAdmin {
		if err := a.store.SetGlobalAdmin(ctx, user.ID, true); err != nil {
			a.logger.Error("bootstrap_admin_failed", "user_id", user.ID, "error", err.Error())
		} else {
			a.logger.Info("bootstrap_admin_granted", "user_id", user.ID)
		}
	}

	if err := a.issueSession(ctx, w, user.ID); err != nil {
		a.logger.Error("session_create_failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.logger.Info("login_success", "user_id", user.ID)
	http.Redirect(w, r, flow.RedirectTo, http.StatusFound)
}

// LogoutHandler destroys the session, clears the cookie, and answers 200 {"redirect_to": …}
// (sec-hardening-2 H-9b). It used to answer a 302: the SPA calls it with fetch, which follows a
// redirect to an external post_logout_url as a cross-origin request `connect-src 'self'` blocks.
// The SPA now navigates itself, with window.location.assign — which executes `javascript:` where
// a 302 Location never did — so the destination is checked HERE, whatever its source (the config
// file, or a row saved before setOIDCSettings checked it): domain.PostLogoutURLAllowed or "/".
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.sessionCfg.CookieName); err == nil && c.Value != "" {
		if err := a.store.DeleteSession(r.Context(), c.Value); err != nil {
			a.logger.Error("session_delete_failed", "error", err.Error())
		}
	}
	a.clearSessionCookie(w)
	dest := "/"
	if rt := a.rt(); rt != nil && rt.postLogoutURL != "" {
		if domain.PostLogoutURLAllowed(rt.postLogoutURL) {
			dest = rt.postLogoutURL
		} else {
			a.logger.Warn("post_logout_url_refused", "reason", "not a local path or an http(s) URL with a host")
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect_to": dest})
}

// sanitizeRedirect only permits local, non-protocol-relative paths.
func sanitizeRedirect(target string) string {
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
		return target
	}
	return "/"
}
