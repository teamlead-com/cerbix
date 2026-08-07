package auth

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Store is the persistence surface the auth layer needs. *store.Store satisfies
// it; tests can supply a fake.
type Store interface {
	UpsertUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, error)
	SetGlobalAdmin(ctx context.Context, id string, admin bool) error
	GetUser(ctx context.Context, id string) (domain.User, error)
	ListMembershipsForUser(ctx context.Context, userID string) ([]domain.Membership, error)
	CreateSession(ctx context.Context, userID, token string, expiresAt time.Time) (store.Session, error)
	SessionByToken(ctx context.Context, token string) (store.Session, error)
	DeleteSession(ctx context.Context, token string) error
	CreateAuthFlow(ctx context.Context, f store.AuthFlow) error
	TakeAuthFlow(ctx context.Context, state string) (store.AuthFlow, error)
	CreateLocalUser(ctx context.Context, email, displayName, passwordHash string, globalAdmin bool) (domain.User, error)
	LocalCredentialByEmail(ctx context.Context, email string) (store.LocalCredential, error)
	ConsumeRecoveryCode(ctx context.Context, userID, codeHash string) (bool, error)
	SetPassword(ctx context.Context, id, passwordHash string) error
	CreatePasswordResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	ConsumePasswordResetToken(ctx context.Context, tokenHash string) (string, error)
	ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) (string, error)
	CountUsers(ctx context.Context) (int64, error)
	ApiTokenByHash(ctx context.Context, hash string) (domain.ApiToken, error)
	TouchApiToken(ctx context.Context, id string) error
	GetOIDCSettings(ctx context.Context) (domain.OIDCSettings, error)
}

type ctxKey int

const (
	principalKey ctxKey = iota
	sessionTokenKey
)

// WithPrincipal returns a context carrying the principal.
func WithPrincipal(ctx context.Context, p authz.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom extracts the principal placed by the auth middleware.
func PrincipalFrom(ctx context.Context) (authz.Principal, bool) {
	p, ok := ctx.Value(principalKey).(authz.Principal)
	return p, ok
}

// WithSessionToken carries the raw session-cookie token so a handler can identify
// (and preserve) the caller's own session — e.g. a password change invalidates every
// OTHER session but this one. Empty for bearer/JWT callers (no interactive session).
func WithSessionToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, sessionTokenKey, token)
}

// SessionTokenFrom returns the raw session token for the request, or "" if the caller
// did not authenticate with a session cookie.
func SessionTokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(sessionTokenKey).(string)
	return t
}
