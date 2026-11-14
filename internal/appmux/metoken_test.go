package appmux

// Review of iter-0211, finding 1: the /me write routes describe a USER. An API token's principal
// id is synthetic ("apitoken:<uuid>"); handed to a users.id (uuid) column it answered 500 on
// PostgreSQL. Every POST /api/v1/me/* refuses a token with 403 before any store call.

import (
	"net/http"
	"testing"
)

func TestTokensAreRefusedOnEveryMeWriteRoute(t *testing.T) {
	w := newWorld(t)
	for _, tok := range []string{w.token("org_admin", "", nil), w.token("viewer", w.project, nil)} {
		for _, c := range []struct {
			path string
			body map[string]string
		}{
			{"/api/v1/me/password", map[string]string{"current_password": fixturePassword, "new_password": "another-password-1"}},
			{"/api/v1/me/totp/enroll", nil},
			{"/api/v1/me/totp/enable", map[string]string{"code": "123456"}},
			{"/api/v1/me/totp/disable", map[string]string{"password": fixturePassword}},
		} {
			var body any
			if c.body != nil {
				body = c.body
			}
			rec := w.send(http.MethodPost, c.path, body, withBearer(tok))
			if rec.Code != http.StatusForbidden {
				t.Errorf("token: POST %s = %d %s, want 403", c.path, rec.Code, rec.Body.String())
			}
		}
	}
}
