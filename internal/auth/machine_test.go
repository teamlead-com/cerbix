package auth

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// refusalLog records RecordOIDCBearerRefused calls (the metric sink).
type refusalLog struct {
	mu      sync.Mutex
	reasons []string
}

func (r *refusalLog) RecordOIDCBearerRefused(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *refusalLog) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reasons) == 0 {
		return ""
	}
	return r.reasons[len(r.reasons)-1]
}

// bearerFixture is an authenticator over the mock IdP with machine clients listed, its refusal
// sink, and a captured log.
type bearerFixture struct {
	fs   *fakeStore
	mock *mockOIDC
	a    *Authenticator
	refs *refusalLog
	logs *bytes.Buffer
}

func newBearerFixture(t *testing.T, machineClients ...string) *bearerFixture {
	t.Helper()
	f := &bearerFixture{fs: newFakeStore(), mock: newMockOIDC(t, "cerbix"), refs: &refusalLog{}, logs: &bytes.Buffer{}}
	f.a = testAuthenticatorWith(t, f.fs, f.mock, func(c *config.Config) { c.OIDC.MachineClients = machineClients })
	f.a.logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.a.WithMetrics(f.refs)
	return f
}

// mint signs a token for sub with the given extra claims (no email, no nonce unless given).
func (f *bearerFixture) mint(t *testing.T, sub string, extra map[string]any) string {
	t.Helper()
	f.mock.sub, f.mock.email, f.mock.name, f.mock.nn = sub, "", "", ""
	f.mock.extra = extra
	return f.mock.mintIDToken(t)
}

// call presents token as a bearer and returns the status and the principal the handler saw.
func (f *bearerFixture) call(token string) (int, authz.Principal) {
	var got authz.Principal
	h := f.a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, got
}

func (f *bearerFixture) users() map[string]domain.User {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	out := make(map[string]domain.User, len(f.fs.usersBySub))
	for k, v := range f.fs.usersBySub {
		out[k] = v
	}
	return out
}

// expectRefused asserts a 401 with the given reason, and that no user row changed.
func (f *bearerFixture) expectRefused(t *testing.T, token, reason string) {
	t.Helper()
	before := f.users()
	code, _ := f.call(token)
	if code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 (%s)", code, reason)
	}
	if got := f.refs.last(); got != reason {
		t.Fatalf("refusal reason = %q, want %q", got, reason)
	}
	if after := f.users(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused bearer changed users:\nbefore %+v\nafter  %+v", before, after)
	}
}

// sec-hardening-2 H-1: the client-credentials bearer path accepts only listed machine clients.

// testAuthenticatorWith is testAuthenticator with a hook to adjust the bootstrap config first.
func testAuthenticatorWith(t *testing.T, fs *fakeStore, mock *mockOIDC, adjust func(*config.Config)) *Authenticator {
	t.Helper()
	cfg := &config.Config{
		OIDC: config.OIDCConfig{
			Issuer:       mock.server.URL,
			ClientID:     mock.clientID,
			ClientSecret: "secret",
			RedirectURL:  "http://cerbix.test/auth/callback",
			Scopes:       []string{"openid", "email", "profile"},
		},
		Session: config.SessionConfig{CookieName: "cerbix_session", TTL: config.Duration(time.Hour), Secure: false},
	}
	if adjust != nil {
		adjust(cfg)
	}
	a, err := New(context.Background(), cfg, fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := a.SyncOIDC(context.Background()); err != nil {
		t.Fatalf("sync oidc: %v", err)
	}
	return a
}

func runtimeMachineClients(a *Authenticator) string {
	rt := a.rt()
	if rt == nil {
		return "<inactive>"
	}
	var ids []string
	for id := range rt.machineClients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// TestResolveEffectiveMachineClients: the YAML list is the bootstrap; a saved DB override replaces
// it entirely (resolveEffective), like every other OIDC field.
func TestResolveEffectiveMachineClients(t *testing.T) {
	fs := newFakeStore()
	mock := newMockOIDC(t, "cerbix")
	a := testAuthenticatorWith(t, fs, mock, func(c *config.Config) {
		c.OIDC.MachineClients = []string{" yaml-svc ", "cerbix"} // the own id is dropped defensively
	})
	if got := runtimeMachineClients(a); got != "yaml-svc" {
		t.Fatalf("YAML bootstrap: runtime machine clients %q, want yaml-svc", got)
	}
	fs.oidc = &domain.OIDCSettings{Enabled: true, Issuer: mock.server.URL, ClientID: "cerbix",
		RedirectURL: "http://cerbix.test/auth/callback", MachineClients: []string{"db-svc"}}
	if err := a.SyncOIDC(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if got := runtimeMachineClients(a); got != "db-svc" {
		t.Fatalf("DB override: runtime machine clients %q, want db-svc only", got)
	}
}

// The finding itself: a token another application obtained for a human authenticates as nobody.
func TestBearerForeignClientWithHumanSubIs401(t *testing.T) {
	f := newBearerFixture(t, "svc")
	if _, err := f.fs.upsertHuman(context.Background(), f.mock.server.URL, "human-1", "h@x.com", "H"); err != nil {
		t.Fatalf("human: %v", err)
	}
	f.expectRefused(t, f.mint(t, "human-1", map[string]any{"azp": "other-app"}), "client_not_listed")
	logs := f.logs.String()
	for _, want := range []string{"oidc_bearer_refused", "reason=client_not_listed", "client_claim=azp", "client=other-app"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("refusal log lacks %q:\n%s", want, logs)
		}
	}
}

func TestBearerOwnInteractiveTokenIs401(t *testing.T) {
	f := newBearerFixture(t, "svc")
	if _, err := f.fs.upsertHuman(context.Background(), f.mock.server.URL, "human-1", "h@x.com", "H"); err != nil {
		t.Fatalf("human: %v", err)
	}
	f.mock.sub, f.mock.nn, f.mock.extra = "human-1", "n-1", map[string]any{"azp": "cerbix"}
	f.expectRefused(t, f.mock.mintIDToken(t), "client_not_listed")
}

func TestBearerListedClientAuthenticates(t *testing.T) {
	f := newBearerFixture(t, "svc")
	code, p := f.call(f.mint(t, "svc-sa", map[string]any{"azp": "svc"}))
	if code != http.StatusNoContent || !p.ViaToken || p.UserID == "" {
		t.Fatalf("listed client: code %d principal %+v", code, p)
	}
	if u := f.users()["svc-sa"]; u.Kind != domain.UserKindMachine || u.Email != "svc-sa@clients" {
		t.Fatalf("provisioned %+v, want a machine with the synthetic address", u)
	}
}

func TestBearerEmptyListIs401(t *testing.T) {
	f := newBearerFixture(t)
	f.expectRefused(t, f.mint(t, "svc-sa", map[string]any{"azp": "svc"}), "machine_clients_empty")
}

func TestBearerInvalidTokenCounted(t *testing.T) {
	f := newBearerFixture(t, "svc")
	f.expectRefused(t, "not.a.valid.jwt", "invalid_token")
}

func TestBearerDelegatedMarkersRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sub    string
		nonce  string
		extra  map[string]any
		reason string
	}{
		{"nonce", "svc", "n-1", map[string]any{"azp": "svc"}, "delegated_nonce"},
		{"sid", "svc", "", map[string]any{"azp": "svc", "sid": "session-1"}, "delegated_sid"},
		{"okta cid != sub", "user-1", "", map[string]any{"cid": "svc"}, "delegated_cid_sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBearerFixture(t, "svc")
			f.mock.sub, f.mock.email, f.mock.nn, f.mock.extra = tc.sub, "", tc.nonce, tc.extra
			f.expectRefused(t, f.mock.mintIDToken(t), tc.reason)
		})
	}
}

func TestBearerOktaAndEntraShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		sub   string
		extra map[string]any
	}{
		"okta cid == sub": {"svc", map[string]any{"cid": "svc"}},
		"entra v1 appid":  {"0f1e-sp", map[string]any{"appid": "svc"}},
		"client_id":       {"svc-sa", map[string]any{"client_id": "svc"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newBearerFixture(t, "svc")
			if code, p := f.call(f.mint(t, tc.sub, tc.extra)); code != http.StatusNoContent || !p.ViaToken {
				t.Fatalf("code %d principal %+v (refusal %q)", code, p, f.refs.last())
			}
		})
	}
}

func TestBearerClientClaimsDisagree(t *testing.T) {
	f := newBearerFixture(t, "svc")
	f.expectRefused(t, f.mint(t, "svc-sa", map[string]any{"azp": "svc", "client_id": "other"}), "client_claims_disagree")
	if code, _ := f.call(f.mint(t, "svc-sa", map[string]any{"azp": "svc", "client_id": "svc"})); code != http.StatusNoContent {
		t.Fatalf("agreeing claims: code %d (refusal %q)", code, f.refs.last())
	}
}

func TestBearerClientClaimMissing(t *testing.T) {
	f := newBearerFixture(t, "svc")
	f.expectRefused(t, f.mint(t, "svc-sa", nil), "client_claim_missing")
}

// Review Focus 1: a claim of the wrong JSON type fails closed, never 500 and never accepted.
func TestBearerNonStringClaimIs401(t *testing.T) {
	f := newBearerFixture(t, "svc")
	f.expectRefused(t, f.mint(t, "svc-sa", map[string]any{"azp": []string{"svc"}}), "invalid_claims")
	f.expectRefused(t, f.mint(t, "svc-sa", map[string]any{"azp": "svc", "sid": 42}), "invalid_claims")
}

// Rule 6: the email allowlist is an interactive-login rule; a machine token carries no verified email.
func TestBearerIgnoresEmailAllowlist(t *testing.T) {
	f := newBearerFixture(t, "svc")
	f.a.WithSettings(fakePolicy{p: domain.AuthPolicy{AllowedEmailDomains: []string{"x.com"}}})
	if code, _ := f.call(f.mint(t, "svc-sa", map[string]any{"azp": "svc"})); code != http.StatusNoContent {
		t.Fatalf("machine token under an email allowlist: code %d (refusal %q)", code, f.refs.last())
	}
}

func TestBearerAgainstHumanRowIs401(t *testing.T) {
	f := newBearerFixture(t, "svc")
	if _, err := f.fs.upsertHuman(context.Background(), f.mock.server.URL, "h-2", "h2@x.com", "H2"); err != nil {
		t.Fatalf("human: %v", err)
	}
	f.expectRefused(t, f.mint(t, "h-2", map[string]any{"azp": "svc"}), "subject_is_human")
}

// Review of iter-0209 (S1: no machine is a global admin): even a machine row whose flag was set
// outside the API — by hand, or by a path that bypassed the PATCH refusal — yields a principal
// without instance-wide rights.
func TestBearerPrincipalNeverGlobalAdmin(t *testing.T) {
	f := newBearerFixture(t, "svc")
	token := f.mint(t, "svc-sa", map[string]any{"azp": "svc"})
	if code, _ := f.call(token); code != http.StatusNoContent {
		t.Fatalf("first call: %d", code)
	}
	u := f.users()["svc-sa"]
	if err := f.fs.SetGlobalAdmin(context.Background(), u.ID, true); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	code, p := f.call(token)
	if code != http.StatusNoContent || p.IsGlobalAdmin {
		t.Fatalf("machine principal: code %d, IsGlobalAdmin %v; want 204 without global admin", code, p.IsGlobalAdmin)
	}
}

// The kind that decides must be read no earlier than the memberships: a sign-in that flips the
// machine to human between the two reads must not yield a principal carrying the grants.
func TestBearerKindReadAfterMemberships(t *testing.T) {
	f := newBearerFixture(t, "svc")
	token := f.mint(t, "svc-sa", map[string]any{"azp": "svc"})
	if code, _ := f.call(token); code != http.StatusNoContent {
		t.Fatalf("first call: %d", code)
	}
	u := f.users()["svc-sa"]
	f.fs.memberships[u.ID] = []domain.Membership{{UserID: u.ID, OrgID: "o1", Role: domain.RoleOrgAdmin}}
	var once sync.Once
	f.fs.afterListMemberships = func(string) {
		once.Do(func() {
			_, _, _ = f.fs.UpsertHumanUserByOIDCIdentity(context.Background(), f.mock.server.URL, "svc-sa", "p@x.com", "P")
		})
	}
	code, p := f.call(token)
	if code != http.StatusUnauthorized || len(p.Memberships) != 0 {
		t.Fatalf("flip between reads: code %d principal %+v, want 401", code, p)
	}
	if got := f.refs.last(); got != "subject_is_human" {
		t.Fatalf("reason %q, want subject_is_human", got)
	}
}

// A machine row a person signs in to: the flip is logged, and the bootstrap-admin rule re-grants
// global admin only on a verified bootstrap email.
func TestCallbackFlipLogsAndRegrantsBootstrapOnlyIfVerified(t *testing.T) {
	for _, verified := range []bool{true, false} {
		fs := newFakeStore()
		mock := newMockOIDC(t, "cerbix")
		a := testAuthenticator(t, fs, mock, "admin@x.com")
		var logs bytes.Buffer
		a.logger = slog.New(slog.NewTextHandler(&logs, nil))
		m, err := fs.UpsertMachineUserByOIDCIdentity(context.Background(), mock.server.URL, "kc-flip", "kc-flip@clients", "svc")
		if err != nil {
			t.Fatalf("machine: %v", err)
		}
		// Real state on the machine row — a grant and the global-admin flag — so the fake itself
		// computes the flip the callback reports.
		if err := fs.SetGlobalAdmin(context.Background(), m.ID, true); err != nil {
			t.Fatalf("fixture admin: %v", err)
		}
		fs.memberships[m.ID] = []domain.Membership{{UserID: m.ID, OrgID: "o1", Role: domain.RoleOrgAdmin}}
		_ = fs.CreateAuthFlow(context.Background(), store.AuthFlow{
			State: "s1", Nonce: "n1", PKCEVerifier: "verifier-123456789012345678901234567890123456789012",
			RedirectTo: "/", ExpiresAt: time.Now().Add(time.Minute),
		})
		mock.sub, mock.email, mock.name, mock.nn, mock.emailVerified = "kc-flip", "admin@x.com", "Admin", "n1", verified
		rec := httptest.NewRecorder()
		a.CallbackHandler(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=abc", nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("verified=%v: callback %d %s", verified, rec.Code, rec.Body.String())
		}
		u := fs.usersBySub["kc-flip"]
		if u.Kind != domain.UserKindHuman || u.IsGlobalAdmin != verified {
			t.Fatalf("verified=%v: user %+v", verified, u)
		}
		if n := len(fs.memberships[m.ID]); n != 0 {
			t.Fatalf("verified=%v: %d machine grants survived the flip", verified, n)
		}
		for _, want := range []string{"oidc_machine_became_human", "revoked_memberships=1", "was_global_admin=true"} {
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("verified=%v: log lacks %q:\n%s", verified, want, logs.String())
			}
		}
	}
}
