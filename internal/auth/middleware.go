package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// RequireAuth is middleware that resolves the caller into a principal and stores it in the request
// context. The credential is chosen by the SCHEME of the Authorization header (sec-hardening-2 H-4
// fix 1):
//   - a request that presents a Bearer credential (HasBearerAuth) is authenticated by that one
//     credential or not at all — a cerbix service-account token, else an OIDC client-credentials
//     JWT. Its session cookie is NEVER read: the forgery check exempts bearer requests, so a
//     cookie beside a junk, empty or duplicated Bearer must answer 401, never run as the session;
//   - any other request — no Authorization, or a proxy's Basic/Negotiate beside the browser's
//     cookie — is authenticated by the session cookie alone.
//
// Requests with neither get 401.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if HasBearerAuth(r) {
			a.serveBearer(w, r, next)
			return
		}
		c, err := r.Cookie(a.sessionCfg.CookieName)
		if err != nil || c.Value == "" {
			unauthorized(w)
			return
		}
		sess, err := a.store.SessionByToken(r.Context(), c.Value)
		switch {
		case errors.Is(err, store.ErrNotFound):
			a.clearSessionCookie(w) // stale cookie
			unauthorized(w)
			return
		case err != nil:
			a.logger.Error("session_lookup_failed", "error", err.Error())
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		principal, err := a.loadPrincipal(r.Context(), sess.UserID)
		if err != nil {
			a.logger.Error("principal_load_failed", "error", err.Error())
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		a.refreshSeen(r.Context(), sess.UserID)
		ctx := WithSessionToken(WithPrincipal(r.Context(), principal), c.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serveBearer authenticates a request that presents a Bearer credential: exactly one
// Authorization value carrying a non-empty token, which is a cerbix service-account token or an
// OIDC client-credentials JWT. Anything else is 401.
func (a *Authenticator) serveBearer(w http.ResponseWriter, r *http.Request, next http.Handler) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		unauthorized(w)
		return
	}
	tok := bearerToken(values[0])
	if tok == "" {
		unauthorized(w)
		return
	}
	principal, err := a.principalFromToken(r.Context(), tok)
	switch {
	case err == nil:
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		return
	case !errors.Is(err, store.ErrNotFound):
		a.logger.Error("token_lookup_failed", "error", err.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	// Not a service token — try an OIDC client-credentials JWT.
	jwtPrincipal, ok, jerr := a.principalFromJWT(r.Context(), tok)
	if jerr != nil {
		a.logger.Error("jwt_principal_failed", "error", jerr.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if ok {
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), jwtPrincipal)))
		return
	}
	unauthorized(w)
}

// HasBearerAuth reports whether some Authorization value names the Bearer scheme,
// case-insensitively — with or without a token after it. It is the one predicate both RequireAuth
// (which then never reads the cookie) and the forgery check (which then exempts an /api/*
// request) decide by, so the two cannot disagree about what a bearer request is.
func HasBearerAuth(r *http.Request) bool {
	for _, v := range r.Header.Values("Authorization") {
		scheme, _, _ := strings.Cut(strings.TrimSpace(v), " ")
		if strings.EqualFold(scheme, "Bearer") {
			return true
		}
	}
	return false
}

// refreshSeen stamps users.last_seen_at for a request made with a session, at most once per
// store.UserSeenRefresh per user from this process (iter-0205, func-admin-users.md "Last
// active"). Best effort: a failure is logged, never denies the request, and is not remembered —
// the next request tries again. Machine credentials do not come through here.
func (a *Authenticator) refreshSeen(ctx context.Context, userID string) {
	now := time.Now()
	if a.clock != nil {
		now = a.clock()
	}
	a.seenMu.Lock()
	last, ok := a.seenAt[userID]
	a.seenMu.Unlock()
	if ok && now.Sub(last) < store.UserSeenRefresh {
		return
	}
	if err := a.store.TouchUserSeen(ctx, userID); err != nil {
		a.logger.Warn("user_seen_touch_failed", "error", err.Error())
		return
	}
	a.seenMu.Lock()
	if a.seenAt == nil {
		a.seenAt = map[string]time.Time{}
	}
	a.seenAt[userID] = now
	a.seenMu.Unlock()
}

// principalFromToken hashes a presented bearer token, looks it up, and builds a
// scoped principal from its org/project/role. Authorization stays role-driven;
// the principal is flagged ViaToken so writes can be attributed to the machine.
func (a *Authenticator) principalFromToken(ctx context.Context, presented string) (authz.Principal, error) {
	t, err := a.store.ApiTokenByHash(ctx, store.HashToken(presented))
	if err != nil {
		return authz.Principal{}, err
	}
	// Best-effort usage stamp; a failure here must not deny an otherwise valid call.
	if err := a.store.TouchApiToken(ctx, t.ID); err != nil {
		a.logger.Warn("token_touch_failed", "error", err.Error())
	}
	return authz.Principal{
		UserID:      authz.SyntheticTokenActorPrefix + t.ID,
		Memberships: []domain.Membership{{OrgID: t.OrgID, ProjectID: t.ProjectID, Role: t.Role}},
		ViaToken:    true,
		// The token's NAME is what an operator recognizes in an audit row; the id
		// stays in UserID for the typed columns ([288] P1-3).
		AuditLabel: "token:" + t.Name,
		// The allow-list rides onto the principal untouched (FR-025 D12): nil stays nil (the
		// role decides), a list stays a list; authz.Can is the only reader.
		Actions: tokenActions(t.Actions),
	}, nil
}

// tokenActions converts the stored list to the authz type, preserving nil.
func tokenActions(actions []string) []authz.Action {
	if actions == nil {
		return nil
	}
	out := make([]authz.Action, len(actions))
	for i, a := range actions {
		out[i] = authz.Action(a)
	}
	return out
}

// bearerToken extracts the token from one `Authorization: Bearer <token>` value.
func bearerToken(h string) string {
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
