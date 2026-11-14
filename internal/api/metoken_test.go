package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// totpErrStore is the fake whose TOTP read fails the way a database does.
type totpErrStore struct{ *fakeStore }

func (totpErrStore) GetTOTP(context.Context, string) (string, bool, error) {
	return "", false, errors.New("connection reset")
}

// Review of iter-0211, finding 1: a failed TOTP read is a server error, not the user's "start
// enrollment first" — that answer told a user with a working enrollment to start over.
func TestTOTPEnableStoreFailureIs500(t *testing.T) {
	h := api.New(totpErrStore{seededStore()}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
	human := authz.Principal{UserID: "u1"}
	rec := do(h, human, http.MethodPost, "/api/v1/me/totp/enable", `{"code":"123456"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("enable with a failing TOTP read = %d %s, want 500", rec.Code, rec.Body.String())
	}
}

// lookupStore records every GetUser id.
type lookupStore struct {
	*fakeStore
	mu  sync.Mutex
	ids []string
}

func (s *lookupStore) GetUser(ctx context.Context, id string) (domain.User, error) {
	s.mu.Lock()
	s.ids = append(s.ids, id)
	s.mu.Unlock()
	return s.fakeStore.GetUser(ctx, id)
}

// Review of iter-0211, finding 1: resolveUserEmails never looks up a synthetic token id as a user —
// a webhook or token created by an API token records "apitoken:<uuid>", which names no user and,
// on PostgreSQL, is not even a uuid.
func TestIssuerEmailsNeverLookUpATokenIDAsAUser(t *testing.T) {
	fs := seededStore()
	fs.webhooks["wh-tok"] = domain.Webhook{ID: "wh-tok", OrgID: "o1", URL: "https://hooks.example/a", CreatedBy: authz.SyntheticTokenActorPrefix + "t1"}
	fs.webhooks["wh-oa"] = domain.Webhook{ID: "wh-oa", OrgID: "o1", URL: "https://hooks.example/b", CreatedBy: "oa"}
	fs.apiTokens["tk-tok"] = domain.ApiToken{ID: "tk-tok", OrgID: "o1", Name: "x", Role: domain.RoleViewer, CreatedBy: authz.SyntheticTokenActorPrefix + "t2"}
	ls := &lookupStore{fakeStore: fs}
	h := api.New(ls, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
	for _, path := range []string{"/api/v1/organizations/o1/webhooks", "/api/v1/organizations/o1/tokens"} {
		if rec := do(h, o1Admin, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
	}
	for _, id := range ls.ids {
		if strings.HasPrefix(id, authz.SyntheticTokenActorPrefix) {
			t.Errorf("GetUser(%q): a token id was looked up as a user", id)
		}
	}
}
