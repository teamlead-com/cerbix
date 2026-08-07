// Package auth handles authentication: the OIDC login flow (any OpenID Connect
// provider), server-side sessions, just-in-time user provisioning, and the
// middleware that turns a session cookie into an authz.Principal for downstream
// handlers.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/mailer"
	"github.com/teamlead-com/cerbix/internal/store"
)

const (
	authFlowTTL   = 10 * time.Minute
	tokenNumBytes = 32
	// oidcHTTPTimeout bounds every OIDC network call (discovery, JWKS fetch, token
	// exchange). Without it a hung IdP TCP connection blocks on http.DefaultClient's
	// zero timeout — indefinitely, and on the boot path that once froze startup.
	oidcHTTPTimeout = 15 * time.Second
	// oidcRetryEvery is how often the background reloader retries building the OIDC
	// provider while it is intended-enabled but not yet active (e.g. the IdP was
	// unreachable at boot).
	oidcRetryEvery = 30 * time.Second
)

// oidcRuntime is the fully-built OIDC state, swapped atomically so a live
// reconfiguration (from the Settings UI) is visible to in-flight requests without
// locks. A nil runtime means OIDC login is currently inactive.
type oidcRuntime struct {
	provider       *oidc.Provider
	verifier       *oidc.IDTokenVerifier
	ccVerifier     *oidc.IDTokenVerifier // client-credentials bearer (audience-relaxed)
	oauth          oauth2.Config
	httpClient     *http.Client // bounded client for the token exchange
	postLogoutURL  string
	buttonLabel    string
	bootstrapAdmin map[string]bool
}

// EffectiveOIDC is the resolved OIDC configuration (from the DB override if one is
// saved, otherwise the config-file bootstrap).
type EffectiveOIDC struct {
	Enabled         bool
	Issuer          string
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	Scopes          []string
	PostLogoutURL   string
	ButtonLabel     string
	BootstrapAdmins []string
}

// Authenticator wires OIDC (optional, live-reconfigurable) and/or local login,
// session handling, and JIT provisioning.
type Authenticator struct {
	store      Store
	logger     *slog.Logger
	oidc       atomic.Pointer[oidcRuntime] // nil = OIDC inactive; swapped on reload
	configOIDC config.OIDCConfig           // bootstrap seed used until a DB override is saved
	sessionCfg config.SessionConfig

	local             bool
	localBootEmail    string
	localBootPassword string
	minPasswordLen    int
	loginLimiter      *loginLimiter
	trustedProxies    int            // reverse-proxy hops in front (rate-limit client IP)
	decoyHash         string         // argon2id hash verified for unknown users (anti-enumeration timing)
	mailer            *mailer.Mailer // optional; enables self-service password reset
	policy            PolicySource   // optional; instance-wide auth policy
}

// PolicySource supplies the effective instance-wide auth policy (implemented by
// *settings.Service).
type PolicySource interface {
	AuthPolicy() domain.AuthPolicy
}

// WithSettings wires the instance auth policy (session TTL, TOTP enforcement,
// allowed email domains, min password length).
func (a *Authenticator) WithSettings(p PolicySource) *Authenticator {
	a.policy = p
	return a
}

// authPolicy returns the effective policy, falling back to config-derived values
// when no settings service is wired.
func (a *Authenticator) authPolicy() domain.AuthPolicy {
	if a.policy != nil {
		return a.policy.AuthPolicy()
	}
	return domain.AuthPolicy{
		MinPasswordLen:    a.minPasswordLen,
		SessionTTLSeconds: int(a.sessionCfg.TTL.Std().Seconds()),
		RequireTOTP:       domain.TOTPNone,
	}
}

// WithMailer attaches a mailer, enabling the password-reset flow (which needs to
// email a reset link). Without it, reset requests are accepted but no email is
// sent and /auth/config reports password_reset=false.
func (a *Authenticator) WithMailer(m *mailer.Mailer) *Authenticator {
	a.mailer = m
	return a
}

// resetEnabled reports whether self-service password reset can actually work
// (local login on + a mailer to send the link).
func (a *Authenticator) resetEnabled() bool { return a.local && a.mailer != nil && a.mailer.Enabled() }

// New builds an Authenticator. OIDC is not built here — call StartOIDC after
// construction so the provider is assembled asynchronously (discovery is a network
// call and must not block or crash startup; see the reloader). Local login is
// configured from cfg.Local.
func New(_ context.Context, cfg *config.Config, st Store, logger *slog.Logger) (*Authenticator, error) {
	a := &Authenticator{
		store:             st,
		logger:            logger,
		configOIDC:        cfg.OIDC,
		sessionCfg:        cfg.Session,
		local:             cfg.Local.Enabled,
		localBootEmail:    cfg.Security.AdminEmail,
		localBootPassword: cfg.Security.AdminPassword,
		minPasswordLen:    cfg.Local.MinPasswordLength,
		loginLimiter:      newLoginLimiter(cfg.Local.LoginRateLimitPerMinute),
		trustedProxies:    cfg.Server.TrustedProxyCount,
	}
	// Decoy hash for the anti-enumeration timing fix: a login for an unknown user
	// verifies the submitted password against THIS hash so it spends the same
	// Argon2id time as a wrong-password attempt on a real account. Generated at
	// startup so it always carries the current Argon2 params (no drift if they change);
	// no real password can match it (it's the hash of a random secret).
	if secret, err := randToken(); err == nil {
		if h, herr := HashPassword(secret); herr == nil {
			a.decoyHash = h
		}
	}
	return a, nil
}

// rt returns the current OIDC runtime, or nil when OIDC is inactive.
func (a *Authenticator) rt() *oidcRuntime { return a.oidc.Load() }

// oidcEnabled reports whether OIDC login is currently active (provider built).
func (a *Authenticator) oidcEnabled() bool { return a.rt() != nil }

// resolveEffective computes the OIDC settings in force: the DB override if one has
// been saved, otherwise the config-file bootstrap. Errors only on an unexpected DB
// failure (a missing row is the normal "not overridden yet" case).
func (a *Authenticator) resolveEffective(ctx context.Context) (EffectiveOIDC, error) {
	s, err := a.store.GetOIDCSettings(ctx)
	if errors.Is(err, store.ErrNotFound) {
		c := a.configOIDC
		return EffectiveOIDC{
			Enabled: c.Enabled(), Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
			RedirectURL: c.RedirectURL, Scopes: c.Scopes, PostLogoutURL: c.PostLogoutRedirectURL,
			ButtonLabel: c.ButtonLabel, BootstrapAdmins: c.BootstrapAdminEmails,
		}, nil
	}
	if err != nil {
		return EffectiveOIDC{}, err
	}
	return EffectiveOIDC{
		Enabled: s.Enabled, Issuer: s.Issuer, ClientID: s.ClientID, ClientSecret: s.ClientSecret,
		RedirectURL: s.RedirectURL, Scopes: s.Scopes, PostLogoutURL: s.PostLogoutURL,
		ButtonLabel: s.ButtonLabel, BootstrapAdmins: s.BootstrapAdmins,
	}, nil
}

// buildRuntime performs OIDC discovery and assembles the runtime. Returns nil, nil
// when the effective settings are disabled.
func (a *Authenticator) buildRuntime(ctx context.Context, e EffectiveOIDC) (*oidcRuntime, error) {
	if !e.Enabled {
		return nil, nil
	}
	// Bound every OIDC HTTP call. oidc.ClientContext makes discovery AND the
	// verifier's JWKS fetch use this client; the token exchange uses it via the
	// runtime's httpClient in the callback handler.
	httpClient := &http.Client{Timeout: oidcHTTPTimeout}
	ctx = oidc.ClientContext(ctx, httpClient)
	provider, err := oidc.NewProvider(ctx, e.Issuer)
	if err != nil {
		return nil, err
	}
	scopes := e.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	admins := map[string]bool{}
	for _, addr := range e.BootstrapAdmins {
		admins[addr] = true
	}
	return &oidcRuntime{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: e.ClientID}),
		// Machine (client-credentials) bearer tokens carry a different audience; we
		// trust the issuer's signature and gate access on cerbix memberships, so the
		// per-client audience check is relaxed here (authorization is unchanged).
		ccVerifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		oauth: oauth2.Config{
			ClientID:     e.ClientID,
			ClientSecret: e.ClientSecret,
			RedirectURL:  e.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
		},
		httpClient:     httpClient,
		postLogoutURL:  e.PostLogoutURL,
		buttonLabel:    e.ButtonLabel,
		bootstrapAdmin: admins,
	}, nil
}

// SyncOIDC resolves the effective settings and rebuilds the provider, swapping it
// in atomically. It sets the runtime to nil when OIDC is disabled. Returns any
// discovery/build error (the runtime is left nil in that case so the background
// reloader keeps retrying); callers persist settings separately.
func (a *Authenticator) SyncOIDC(ctx context.Context) error {
	e, err := a.resolveEffective(ctx)
	if err != nil {
		return err
	}
	rt, err := a.buildRuntime(ctx, e)
	if err != nil {
		a.oidc.Store(nil)
		a.logger.Warn("oidc_build_failed", "issuer", e.Issuer, "error", err.Error())
		return err
	}
	a.oidc.Store(rt)
	if rt != nil {
		a.logger.Info("oidc_active", "issuer", e.Issuer)
	} else {
		a.logger.Info("oidc_inactive")
	}
	return nil
}

// StartOIDC performs the first OIDC sync and then runs a background reloader that
// retries while OIDC is intended-enabled but not yet active (e.g. the IdP was
// unreachable at boot). It never blocks startup and never crashes the process.
func (a *Authenticator) StartOIDC(ctx context.Context) {
	go func() {
		// The first sync runs here, NOT inline, so a slow/hung IdP (even with the
		// bounded client) can never delay process startup. OIDC becomes active once
		// this returns; until then the login page offers local auth / retries.
		_ = a.SyncOIDC(ctx)
		ticker := time.NewTicker(oidcRetryEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Only retry when inactive; an active provider is already current, and a
				// UI save re-syncs synchronously.
				if a.rt() != nil {
					continue
				}
				if err := a.SyncOIDC(ctx); err == nil && a.rt() != nil {
					// became active; keep looping in case it's later disabled+re-enabled
				}
			}
		}
	}()
}

// OIDCActive reports whether the OIDC provider is currently built and serving.
func (a *Authenticator) OIDCActive() bool { return a.rt() != nil }

// GetOIDCSettings returns the persisted DB override with the secret redacted, or
// ErrNotFound if none is saved. Exposed for the settings API's read path.
func (a *Authenticator) GetOIDCSettings(ctx context.Context) (domain.OIDCSettings, bool, error) {
	s, err := a.store.GetOIDCSettings(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return domain.OIDCSettings{}, false, nil
	}
	if err != nil {
		return domain.OIDCSettings{}, false, err
	}
	return s, true, nil
}

// issueSession creates a session for a user and sets the cookie. Returns the
// session expiry.
func (a *Authenticator) issueSession(ctx context.Context, w http.ResponseWriter, userID string) error {
	token, err := randToken()
	if err != nil {
		return err
	}
	ttl := a.sessionCfg.TTL.Std()
	if secs := a.authPolicy().SessionTTLSeconds; secs > 0 {
		ttl = time.Duration(secs) * time.Second
	}
	expires := time.Now().Add(ttl)
	if _, err := a.store.CreateSession(ctx, userID, token, expires); err != nil {
		return err
	}
	a.setSessionCookie(w, token, expires)
	return nil
}

// loadPrincipal builds an authz.Principal for a user id.
func (a *Authenticator) loadPrincipal(ctx context.Context, userID string) (authz.Principal, error) {
	user, err := a.store.GetUser(ctx, userID)
	if err != nil {
		return authz.Principal{}, err
	}
	memberships, err := a.store.ListMembershipsForUser(ctx, userID)
	if err != nil {
		return authz.Principal{}, err
	}
	return authz.Principal{
		UserID:        user.ID,
		IsGlobalAdmin: user.IsGlobalAdmin,
		Memberships:   memberships,
	}, nil
}

// randToken returns a URL-safe random token.
func randToken() (string, error) {
	b := make([]byte, tokenNumBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *Authenticator) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.sessionCfg.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.sessionCfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Authenticator) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.sessionCfg.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.sessionCfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}
