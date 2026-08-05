package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/totp"
)

// localUser is a principal for the seeded local account u1 with a password on file.
func totpFixture(t *testing.T) (*fakeStore, http.Handler, authz.Principal) {
	t.Helper()
	fs := seededStore()
	hash, err := auth.HashPassword("pw12345678")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	fs.passwords = map[string]string{"u1": hash}
	return fs, newHandler(fs), authz.Principal{UserID: "u1", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}}}
}

func TestTOTPEnrollEnableDisable(t *testing.T) {
	fs, h, u1 := totpFixture(t)

	// Enroll → pending secret + otpauth URI, 2FA not yet enabled.
	rec := do(h, u1, http.MethodPost, "/api/v1/me/totp/enroll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll code=%d body=%s", rec.Code, rec.Body.String())
	}
	var enroll struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("enroll decode: %v", err)
	}
	if enroll.Secret == "" || enroll.URI == "" {
		t.Fatalf("enroll returned empty secret/uri: %+v", enroll)
	}
	if _, enabled, _ := fs.GetTOTP(nil, "u1"); enabled {
		t.Fatal("2FA should not be enabled right after enroll")
	}

	// Enable with a wrong code → 400.
	if rec := do(h, u1, http.MethodPost, "/api/v1/me/totp/enable", `{"code":"000000"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enable wrong code=%d, want 400", rec.Code)
	}

	// Enable with the correct current code → recovery codes, 2FA on.
	code, err := totp.Code(enroll.Secret, time.Now())
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	rec = do(h, u1, http.MethodPost, "/api/v1/me/totp/enable", `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	var en struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &en); err != nil {
		t.Fatalf("enable decode: %v", err)
	}
	if len(en.RecoveryCodes) != 8 {
		t.Fatalf("expected 8 recovery codes, got %d", len(en.RecoveryCodes))
	}
	if _, enabled, _ := fs.GetTOTP(nil, "u1"); !enabled {
		t.Fatal("2FA should be enabled after a valid enable")
	}

	// /me now reports totp_enabled=true.
	rec = do(h, u1, http.MethodGet, "/api/v1/me", "")
	var me struct {
		TOTPEnabled bool `json:"totp_enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if !me.TOTPEnabled {
		t.Fatalf("me should report totp_enabled=true: %s", rec.Body.String())
	}

	// Disable with wrong password → 400, still enabled.
	if rec := do(h, u1, http.MethodPost, "/api/v1/me/totp/disable", `{"password":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("disable wrong pw code=%d, want 400", rec.Code)
	}
	// Disable with correct password → 204, off.
	if rec := do(h, u1, http.MethodPost, "/api/v1/me/totp/disable", `{"password":"pw12345678"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, enabled, _ := fs.GetTOTP(nil, "u1"); enabled {
		t.Fatal("2FA should be off after disable")
	}
}

func TestTOTPEnrollRejectsTokenPrincipal(t *testing.T) {
	_, h, _ := totpFixture(t)
	tok := authz.Principal{UserID: "u1", ViaToken: true, Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}}
	if rec := do(h, tok, http.MethodPost, "/api/v1/me/totp/enroll", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("token enroll code=%d, want 403", rec.Code)
	}
}
