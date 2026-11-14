package appmux

// S7 (sec-hardening-2 H-7): a TOTP code authenticates at most once. The sign-in records the step
// it accepted with one compare-and-set, so a replayed code — sequential or concurrent — is refused,
// while enabling 2FA does NOT spend the step: a person who enables 2FA and signs in with the same
// code inside the same 30 s (what e2e/tests/timeflows.spec.ts does) succeeds both times. Enabling
// is single-use by its own guard (`… WHERE NOT totp_enabled`). Driven through the real composition
// on PostgreSQL: the compare-and-set is SQL, and a fake would prove nothing about it.

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/totp"
)

// enrollTOTP starts enrollment for the session's user and returns the pending secret.
func (s *stack) enrollTOTP(sess string) string {
	s.t.Helper()
	var en struct {
		Secret string `json:"secret"`
	}
	s.must(s.send(http.MethodPost, "/api/v1/me/totp/enroll", nil, withCookie(sess)), http.StatusOK, &en, "enroll")
	if en.Secret == "" {
		s.t.Fatal("enroll returned no secret")
	}
	return en.Secret
}

func totpAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.Code(secret, at)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return c
}

// login posts the local sign-in with a second factor and returns the status.
func (s *stack) login(email, code string) int {
	s.t.Helper()
	return s.send(http.MethodPost, "/auth/local/login", map[string]string{
		"username": email, "password": fixturePassword, "totp": code,
	}).Code
}

func TestATOTPCodeAuthenticatesAtMostOnce(t *testing.T) {
	s := newStack(t)
	const email = "totp-once@fixture.test"
	_, sess := s.user(email, false)
	secret := s.enrollTOTP(sess)

	now := time.Now()
	code := totpAt(t, secret, now)
	s.must(s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": code}, withCookie(sess)), http.StatusOK, nil, "enable")

	// Enabling did not spend the step: the same code signs in, in the same 30 s.
	if got := s.login(email, code); got != http.StatusOK {
		t.Fatalf("sign-in with the code that enabled 2FA = %d, want 200 (enable must not record the step)", got)
	}
	// ...and only once.
	if got := s.login(email, code); got != http.StatusUnauthorized {
		t.Errorf("the same code replayed = %d, want 401", got)
	}

	// Concurrent submissions of one fresh code (the next step's, inside the ±1 window): exactly one
	// passes, whatever the interleaving.
	next := totpAt(t, secret, now.Add(30*time.Second))
	if next == code {
		t.Skip("two consecutive steps share a code (p = 1e-6); nothing to race")
	}
	const n = 12
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = s.login(email, next)
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
		default:
			t.Errorf("concurrent sign-in answered %d", c)
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d concurrent sign-ins with one code succeeded, want exactly 1 (statuses %v)", ok, n, codes)
	}
}

// Enabling is single-use: concurrent confirmations of one pending secret enable it once, and the
// recovery codes the winner was shown are the ones stored.
func TestEnablingTOTPIsSingleUseAndKeepsTheWinnersRecoveryCodes(t *testing.T) {
	s := newStack(t)
	const email = "totp-enable@fixture.test"
	_, sess := s.user(email, false)
	secret := s.enrollTOTP(sess)
	code := totpAt(t, secret, time.Now())

	const n = 8
	type answer struct {
		status int
		codes  []string
	}
	answers := make([]answer, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": code}, withCookie(sess))
			answers[i].status = rec.Code
			if rec.Code == http.StatusOK {
				var out struct {
					RecoveryCodes []string `json:"recovery_codes"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				answers[i].codes = out.RecoveryCodes
			}
		}(i)
	}
	close(start)
	wg.Wait()
	var winner []string
	ok := 0
	for _, a := range answers {
		if a.status == http.StatusOK {
			ok++
			winner = a.codes
		} else if a.status != http.StatusBadRequest {
			t.Errorf("concurrent enable answered %d", a.status)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent enables succeeded, want exactly 1", ok, n)
	}
	if len(winner) == 0 {
		t.Fatal("the winning enable returned no recovery codes")
	}
	// The recovery code shown to the person signs in: the losers did not overwrite the set.
	if got := s.login(email, winner[0]); got != http.StatusOK {
		t.Errorf("sign-in with the winner's recovery code = %d, want 200", got)
	}
}

// A fresh enrollment forgets the last accepted step: disabling and enrolling again lets the new
// secret's current code sign in, even if its step is not above the old one.
func TestReEnrollmentForgetsTheLastStep(t *testing.T) {
	s := newStack(t)
	const email = "totp-reenroll@fixture.test"
	_, sess := s.user(email, false)
	secret := s.enrollTOTP(sess)
	now := time.Now()
	s.must(s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": totpAt(t, secret, now)}, withCookie(sess)), http.StatusOK, nil, "enable")
	// Spend the NEXT step, so the recorded step is ahead of the clock.
	if got := s.login(email, totpAt(t, secret, now.Add(30*time.Second))); got != http.StatusOK {
		t.Fatalf("sign-in with the next step's code = %d", got)
	}
	s.must(s.send(http.MethodPost, "/api/v1/me/totp/disable", map[string]string{"password": fixturePassword}, withCookie(sess)), http.StatusNoContent, nil, "disable")

	secret2 := s.enrollTOTP(sess)
	now2 := time.Now()
	s.must(s.send(http.MethodPost, "/api/v1/me/totp/enable", map[string]string{"code": totpAt(t, secret2, now2)}, withCookie(sess)), http.StatusOK, nil, "re-enable")
	if got := s.login(email, totpAt(t, secret2, now2)); got != http.StatusOK {
		t.Errorf("sign-in with the new secret's current code = %d, want 200 (the old step must be forgotten)", got)
	}
}
