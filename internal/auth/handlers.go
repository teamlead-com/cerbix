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
		mux.HandleFunc("POST /auth/local/login", a.LocalLoginHandler)
		mux.HandleFunc("POST /auth/local/reset/request", a.ResetRequestHandler)
		mux.HandleFunc("POST /auth/local/reset/confirm", a.ResetConfirmHandler)
	}
	mux.HandleFunc("POST /auth/logout", a.LogoutHandler)
	mux.HandleFunc("GET /auth/logout", a.LogoutHandler)
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
	// Instance policy: restrict which email domains may sign in via SSO.
	if claims.Email != "" && !a.authPolicy().EmailAllowed(claims.Email) {
		http.Error(w, "your email domain is not permitted to sign in", http.StatusForbidden)
		return
	}

	user, err := a.store.UpsertUserByOIDCSub(ctx, idToken.Subject, claims.Email, displayName)
	if err != nil {
		a.logger.Error("user_provision_failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if claims.Email != "" && rt.bootstrapAdmin[claims.Email] && !user.IsGlobalAdmin {
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

// LogoutHandler destroys the session and clears the cookie.
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.sessionCfg.CookieName); err == nil && c.Value != "" {
		if err := a.store.DeleteSession(r.Context(), c.Value); err != nil {
			a.logger.Error("session_delete_failed", "error", err.Error())
		}
	}
	a.clearSessionCookie(w)
	dest := "/"
	if rt := a.rt(); rt != nil && rt.postLogoutURL != "" {
		dest = rt.postLogoutURL
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// sanitizeRedirect only permits local, non-protocol-relative paths.
func sanitizeRedirect(target string) string {
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
		return target
	}
	return "/"
}
