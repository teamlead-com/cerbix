package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// RequireAuth is middleware that resolves the caller into a principal and stores
// it in the request context. It accepts either a session cookie (interactive
// users) or an `Authorization: Bearer <token>` service-account token. Requests
// with neither get 401.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Session cookie.
		if c, err := r.Cookie(a.sessionCfg.CookieName); err == nil && c.Value != "" {
			sess, err := a.store.SessionByToken(r.Context(), c.Value)
			switch {
			case errors.Is(err, store.ErrNotFound):
				a.clearSessionCookie(w) // stale cookie; fall through to bearer
			case err != nil:
				a.logger.Error("session_lookup_failed", "error", err.Error())
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			default:
				principal, err := a.loadPrincipal(r.Context(), sess.UserID)
				if err != nil {
					a.logger.Error("principal_load_failed", "error", err.Error())
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				ctx := WithSessionToken(WithPrincipal(r.Context(), principal), c.Value)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// 2. Bearer credential: a cerbix service-account token, else a OIDC
		// client-credentials JWT.
		if tok := bearerToken(r); tok != "" {
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
			// Not a service token — try a OIDC client-credentials JWT.
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
			return
		}

		unauthorized(w)
	})
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
	}, nil
}

// principalFromJWT validates a OIDC client-credentials access token (issuer +
// signature, audience relaxed), JIT-provisions a user keyed by the token subject,
// and builds a principal from that identity's memberships. Returns ok=false (not
// an error) when OIDC is disabled or the token isn't a valid JWT for this issuer,
// so the caller can fall through to 401. Authorization stays membership-driven —
// a service account with no grants gets no access.
func (a *Authenticator) principalFromJWT(ctx context.Context, raw string) (authz.Principal, bool, error) {
	rt := a.rt()
	if rt == nil {
		return authz.Principal{}, false, nil
	}
	idt, err := rt.ccVerifier.Verify(ctx, raw)
	if err != nil {
		return authz.Principal{}, false, nil // not a valid token for us; fall through to 401
	}
	var claims struct {
		Email             string `json:"email"`
		Azp               string `json:"azp"`
		PreferredUsername string `json:"preferred_username"`
	}
	_ = idt.Claims(&claims)
	display := claims.PreferredUsername
	if display == "" {
		display = claims.Azp
	}
	email := claims.Email
	if email == "" {
		email = idt.Subject + "@clients" // service accounts often carry no email
	}
	user, err := a.store.UpsertUserByOIDCIdentity(ctx, idt.Issuer, idt.Subject, email, display)
	if err != nil {
		return authz.Principal{}, false, err
	}
	principal, err := a.loadPrincipal(ctx, user.ID)
	if err != nil {
		return authz.Principal{}, false, err
	}
	principal.ViaToken = true
	return principal, true, nil
}

// bearerToken extracts the token from an `Authorization: Bearer <token>` header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
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
