package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// iter-0205 (func-admin-users.md "Last active"): a request authenticated by a session cookie
// refreshes the user's last_seen_at, at most once per 5 minutes per user from this process;
// machine credentials never do; a failed refresh never denies the request.
func TestSessionRequestsRefreshLastSeenAtMostEveryFiveMinutes(t *testing.T) {
	fs := newFakeStore()
	a := testAuthenticator(t, fs, newMockOIDC(t, "cerbix"))
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	a.clock = func() time.Time { return now }

	ctx := context.Background()
	u, _ := fs.UpsertUserByOIDCIdentity(ctx, "https://idp.test", "kc-seen", "seen@x", "Seen")
	v, _ := fs.UpsertUserByOIDCIdentity(ctx, "https://idp.test", "kc-other", "other@x", "Other")
	// The fake checks expiry against the real clock, not a.clock.
	_, _ = fs.CreateSession(ctx, u.ID, "tok-seen", time.Now().Add(time.Hour))
	_, _ = fs.CreateSession(ctx, v.ID, "tok-other", time.Now().Add(time.Hour))
	fs.apiTokens[store.HashToken("cbx_machine")] = domain.ApiToken{ID: "tk-m", OrgID: "o1", Role: domain.RoleEditor, Name: "ci"}

	guarded := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(cookie, bearer string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "cerbix_session", Value: cookie})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code
	}
	seen := func() []string {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return append([]string(nil), fs.seenTouched...)
	}

	if code := call("tok-seen", ""); code != http.StatusNoContent {
		t.Fatalf("session request = %d", code)
	}
	if got := seen(); len(got) != 1 || got[0] != u.ID {
		t.Fatalf("after the first session request touched %v, want [%s]", got, u.ID)
	}

	// Within five minutes: no second write for the same user...
	now = now.Add(4*time.Minute + 59*time.Second)
	call("tok-seen", "")
	if got := seen(); len(got) != 1 {
		t.Fatalf("a request 4m59s later touched again: %v", got)
	}
	// ...but another user is stamped on their own schedule.
	call("tok-other", "")
	if got := seen(); len(got) != 2 || got[1] != v.ID {
		t.Fatalf("another user's first request touched %v, want it appended", got)
	}

	// At five minutes the same user is refreshed again.
	now = now.Add(time.Second)
	call("tok-seen", "")
	if got := seen(); len(got) != 3 || got[2] != u.ID {
		t.Fatalf("a request 5m later touched %v, want a third touch of %s", got, u.ID)
	}

	// A machine credential never moves last_seen_at.
	if code := call("", "cbx_machine"); code != http.StatusNoContent {
		t.Fatalf("bearer request = %d", code)
	}
	if got := seen(); len(got) != 3 {
		t.Fatalf("a bearer-token request touched last_seen_at: %v", got)
	}

	// A failing refresh is logged, never fatal — and is retried on the next request instead of
	// being throttled as if it had been written.
	fs.mu.Lock()
	fs.seenErr = errors.New("db down")
	fs.mu.Unlock()
	now = now.Add(10 * time.Minute)
	if code := call("tok-seen", ""); code != http.StatusNoContent {
		t.Fatalf("a failed refresh changed the response to %d", code)
	}
	fs.mu.Lock()
	fs.seenErr = nil
	fs.mu.Unlock()
	now = now.Add(time.Second)
	call("tok-seen", "")
	if got := seen(); len(got) != 4 || got[3] != u.ID {
		t.Fatalf("after a failed refresh the next request touched %v, want a retry for %s", got, u.ID)
	}
}
