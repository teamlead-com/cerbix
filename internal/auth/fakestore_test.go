package auth

import (
	"context"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// fakeStore is an in-memory auth.Store for hermetic tests.
type fakeStore struct {
	mu          sync.Mutex
	usersBySub  map[string]domain.User
	usersByID   map[string]domain.User
	memberships map[string][]domain.Membership   // by user id
	sessions    map[string]store.Session         // by token hash
	flows       map[string]store.AuthFlow        // by state
	localCreds  map[string]store.LocalCredential // by email
	recovery    map[string]bool                  // userID|hash -> available
	resetTokens map[string]resetTok              // tokenHash -> token
	passwords   map[string]string                // userID -> hash (SetPassword)
	apiTokens   map[string]domain.ApiToken       // by token hash
	touched     []string                         // token ids touched
	oidc        *domain.OIDCSettings             // nil = no DB override
	nextID      int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		usersBySub:  map[string]domain.User{},
		usersByID:   map[string]domain.User{},
		memberships: map[string][]domain.Membership{},
		sessions:    map[string]store.Session{},
		flows:       map[string]store.AuthFlow{},
		apiTokens:   map[string]domain.ApiToken{},
	}
}

// UpsertUserByOIDCIdentity keys by subject alone in the fake — the (issuer, sub)
// uniqueness is a DB-constraint concern exercised in store integration tests; the
// hermetic auth tests only need the JIT-provisioning shape.
func (f *fakeStore) UpsertUserByOIDCIdentity(_ context.Context, _, sub, email, displayName string) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usersBySub[sub]
	if !ok {
		f.nextID++
		u = domain.User{ID: "user-" + itoa(f.nextID), OIDCSub: sub}
	}
	u.Email = email
	u.DisplayName = displayName
	f.usersBySub[sub] = u
	f.usersByID[u.ID] = u
	return u, nil
}

func (f *fakeStore) SetGlobalAdmin(_ context.Context, id string, admin bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usersByID[id]
	if !ok {
		return store.ErrNotFound
	}
	u.IsGlobalAdmin = admin
	f.usersByID[id] = u
	f.usersBySub[u.OIDCSub] = u
	return nil
}

func (f *fakeStore) GetUser(_ context.Context, id string) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usersByID[id]
	if !ok {
		return domain.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) ListMembershipsForUser(_ context.Context, userID string) ([]domain.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memberships[userID], nil
}

func (f *fakeStore) CreateSession(_ context.Context, userID, token string, expiresAt time.Time) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := store.Session{ID: "sess-" + token, UserID: userID, CreatedAt: time.Now(), ExpiresAt: expiresAt}
	f.sessions[store.HashToken(token)] = s
	return s, nil
}

func (f *fakeStore) SessionByToken(_ context.Context, token string) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[store.HashToken(token)]
	if !ok || s.ExpiresAt.Before(time.Now()) {
		return store.Session{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) DeleteSession(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, store.HashToken(token))
	return nil
}

func (f *fakeStore) DeleteSessionsByUser(_ context.Context, userID, exceptToken string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := ""
	if exceptToken != "" {
		keep = store.HashToken(exceptToken)
	}
	var n int64
	for h, s := range f.sessions {
		if s.UserID == userID && h != keep {
			delete(f.sessions, h)
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) CreateAuthFlow(_ context.Context, fl store.AuthFlow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flows[fl.State] = fl
	return nil
}

func (f *fakeStore) TakeAuthFlow(_ context.Context, state string) (store.AuthFlow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.flows[state]
	if !ok || fl.ExpiresAt.Before(time.Now()) {
		return store.AuthFlow{}, store.ErrNotFound
	}
	delete(f.flows, state)
	return fl, nil
}

func (f *fakeStore) CreateLocalUser(_ context.Context, email, displayName, passwordHash string, globalAdmin bool) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	u := domain.User{ID: "user-" + itoa(f.nextID), Email: email, DisplayName: displayName, IsGlobalAdmin: globalAdmin}
	f.usersByID[u.ID] = u
	if f.localCreds == nil {
		f.localCreds = map[string]store.LocalCredential{}
	}
	f.localCreds[email] = store.LocalCredential{UserID: u.ID, PasswordHash: passwordHash, IsGlobalAdmin: globalAdmin}
	return u, nil
}

func (f *fakeStore) LocalCredentialByEmail(_ context.Context, email string) (store.LocalCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.localCreds[email]
	if !ok {
		return store.LocalCredential{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) CountUsers(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.usersByID)), nil
}

func (f *fakeStore) ApiTokenByHash(_ context.Context, hash string) (domain.ApiToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.apiTokens[hash]
	if !ok {
		return domain.ApiToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) TouchApiToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakeStore) GetOIDCSettings(_ context.Context) (domain.OIDCSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.oidc == nil {
		return domain.OIDCSettings{}, store.ErrNotFound
	}
	return *f.oidc, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *fakeStore) ConsumeRecoveryCode(_ context.Context, userID, codeHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := userID + "|" + codeHash
	if f.recovery != nil && f.recovery[key] {
		delete(f.recovery, key)
		return true, nil
	}
	return false, nil
}

type resetTok struct {
	userID  string
	expires time.Time
	used    bool
}

func (f *fakeStore) SetPassword(_ context.Context, id, passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[id] = passwordHash
	return nil
}

func (f *fakeStore) CreatePasswordResetToken(_ context.Context, userID, tokenHash string, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resetTokens == nil {
		f.resetTokens = map[string]resetTok{}
	}
	f.resetTokens[tokenHash] = resetTok{userID: userID, expires: expiresAt}
	return nil
}

func (f *fakeStore) ConsumePasswordResetToken(_ context.Context, tokenHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.resetTokens[tokenHash]
	if !ok || t.used || t.expires.Before(time.Now()) {
		return "", store.ErrNotFound
	}
	t.used = true
	f.resetTokens[tokenHash] = t
	return t.userID, nil
}

// ResetPasswordWithToken mirrors the store's atomic consume+set-password.
func (f *fakeStore) ResetPasswordWithToken(_ context.Context, tokenHash, passwordHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.resetTokens[tokenHash]
	if !ok || t.used || t.expires.Before(time.Now()) {
		return "", store.ErrNotFound
	}
	t.used = true
	f.resetTokens[tokenHash] = t
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[t.userID] = passwordHash
	return t.userID, nil
}
