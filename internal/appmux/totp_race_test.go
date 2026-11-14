package appmux

// Review of iter-0212, P1-1: the three interleavings of enabling 2FA with a concurrent disable or
// re-enrollment. Each is driven deterministically through the real handlers on PostgreSQL: the
// API's store is wrapped, and the concurrent operation runs from a hook placed right after the
// handler's own read of the TOTP state — the window between "read and validated" and "written".
// In no outcome may totp_enabled be true with an empty or an unconfirmed secret, and enrollment
// may never switch off a live 2FA.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/store"
)

// hookedStore runs after() once, right after the first GetTOTP that arm() armed.
type hookedStore struct {
	*store.Store
	after func()
}

func (h *hookedStore) GetTOTP(ctx context.Context, userID string) (string, bool, error) {
	secret, enabled, err := h.Store.GetTOTP(ctx, userID)
	if f := h.after; f != nil {
		h.after = nil
		f()
	}
	return secret, enabled, err
}

func newHookedStack(t *testing.T) (*stack, *hookedStore) {
	t.Helper()
	var hs *hookedStore
	s := newStackStore(t, true, func(st *store.Store) api.Store {
		hs = &hookedStore{Store: st}
		return hs
	})
	return s, hs
}

// state is the stored TOTP state of the user.
func (s *stack) totpState(userID string) (secret string, enabled bool) {
	s.t.Helper()
	secret, enabled, err := s.st.GetTOTP(s.ctx, userID)
	if err != nil {
		s.t.Fatalf("read totp state: %v", err)
	}
	return secret, enabled
}

func (s *stack) recoveryCodeCount(userID string) int {
	s.t.Helper()
	conn, err := pgx.Connect(s.ctx, probeDSN)
	if err != nil {
		s.t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // test read
	var n int
	if err := conn.QueryRow(s.ctx, `SELECT count(*) FROM totp_recovery_codes WHERE user_id = $1`, userID).Scan(&n); err != nil {
		s.t.Fatalf("count recovery codes: %v", err)
	}
	return n
}

// enable ∥ disable: the disable lands between enable's read and its write.
func TestEnableRacingADisableNeverEnablesAnEmptySecret(t *testing.T) {
	s, hs := newHookedStack(t)
	id, sess := s.user("race-disable@fixture.test", false)
	secret := s.enrollTOTP(sess)
	hs.after = func() {
		if err := s.st.DisableTOTP(s.ctx, id); err != nil {
			t.Errorf("concurrent disable: %v", err)
		}
	}
	rec := s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": totpAt(t, secret, time.Now())}, withCookie(sess))
	stored, enabled := s.totpState(id)
	if enabled {
		t.Errorf("enable (%d) after a concurrent disable left totp_enabled = true with secret %q", rec.Code, stored)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("enable after a concurrent disable = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if n := s.recoveryCodeCount(id); n != 0 {
		t.Errorf("a refused enable stored %d recovery codes", n)
	}
}

// enable ∥ enroll, enroll first: the new secret lands between enable's read and its write.
func TestEnableRacingAReEnrollmentNeverEnablesAnUnconfirmedSecret(t *testing.T) {
	s, hs := newHookedStack(t)
	id, sess := s.user("race-enroll-a@fixture.test", false)
	confirmed := s.enrollTOTP(sess)
	const other = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	hs.after = func() {
		if err := s.st.SetTOTPSecret(s.ctx, id, other); err != nil {
			t.Errorf("concurrent enroll: %v", err)
		}
	}
	rec := s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": totpAt(t, confirmed, time.Now())}, withCookie(sess))
	stored, enabled := s.totpState(id)
	if enabled && stored != confirmed {
		t.Errorf("enable (%d) confirmed one secret and enabled another: the person's app cannot produce its codes", rec.Code)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("enable after a concurrent re-enrollment = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if n := s.recoveryCodeCount(id); n != 0 {
		t.Errorf("a refused enable stored %d recovery codes", n)
	}
}

// enroll ∥ enable, enable first: a whole enable lands between enroll's check and its write.
func TestReEnrollmentRacingAnEnableNeverSwitchesOffALiveTwoFactor(t *testing.T) {
	s, hs := newHookedStack(t)
	id, sess := s.user("race-enroll-b@fixture.test", false)
	confirmed := s.enrollTOTP(sess)
	hs.after = func() {
		rec := s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": totpAt(t, confirmed, time.Now())}, withCookie(sess))
		if rec.Code != http.StatusOK {
			t.Errorf("the concurrent enable = %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := s.send(http.MethodPost, "/api/v1/me/totp/enroll", nil, withCookie(sess))
	stored, enabled := s.totpState(id)
	if !enabled || stored != confirmed {
		t.Errorf("enroll (%d) after a concurrent enable: enabled = %v, secret confirmed = %v — enrollment switched off a live 2FA", rec.Code, enabled, stored == confirmed)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("enroll after a concurrent enable = %d %s, want 400", rec.Code, rec.Body.String())
	}
}
