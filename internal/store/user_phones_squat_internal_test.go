package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// Final review I-1 and M-9. An UNVERIFIED number of another user with no active code yields to a new
// claim; a verified one, or one whose code is still running, does not. A refused claim counts against
// the claimant's hourly code budget, so `number_in_use` cannot be asked without limit.

// endPhoneCodes ends every code of the user, as ten minutes passing would.
func endPhoneCodes(t *testing.T, st *Store, ctx context.Context, userID string) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `UPDATE phone_verifications SET expires_at = now() - interval '1 minute'
		WHERE subject_kind = 'user' AND subject_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
}

// issuedCodes counts the codes issued for a subject: a refusal recorded against the budget is not one.
func issuedCodes(t *testing.T, st *Store, ctx context.Context, kind, subjectID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM phone_verifications
		WHERE subject_kind = $1 AND subject_id = $2 AND code_hash <> ''`, kind, subjectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func numberCodes(t *testing.T, st *Store, ctx context.Context, c *secret.Cipher, number string) int {
	t.Helper()
	hash, err := c.KeyedHash("phone-number", []byte(number))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM phone_verifications WHERE number_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUserPhoneUnverifiedSquatterWithoutActiveCodeYields(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	scriptPhoneCodes(t, "111111", "222222")
	squatter := phoneUser(t, st, ctx, "squatter@example.com")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	mustSetPhone(t, st, ctx, squatter, phoneA)
	endPhoneCodes(t, st, ctx, squatter)

	phone, code, err := st.SetUserPhone(ctx, owner, phoneA)
	if err != nil || code != "222222" || phone.Verified {
		t.Fatalf("owner's claim: %+v code=%q %v; want the number stored unverified with a code", phone, code, err)
	}
	if got, err := st.GetUserPhone(ctx, squatter); err != nil || got != nil {
		t.Fatalf("squatter's phone after the claim: %+v %v; want none", got, err)
	}
	if got, _ := st.GetUserPhone(ctx, owner); got == nil || got.NumberMasked != "+90•••••6869" {
		t.Fatalf("owner's phone: %+v", got)
	}
	if verified, err := st.VerifyUserPhone(ctx, owner, "222222"); err != nil || !verified.Verified {
		t.Fatalf("owner verifies: %+v %v", verified, err)
	}
	// The squatter's code rows stay: they count for the limits (R6).
	if n := issuedCodes(t, st, ctx, domain.PhoneSubjectUser, squatter); n != 1 {
		t.Fatalf("squatter's codes = %d, want 1 kept", n)
	}
	if n := numberCodes(t, st, ctx, c, phoneA); n != 2 {
		t.Fatalf("codes to the number = %d, want 2", n)
	}
}

// captureStoreLog gives the store a logger writing into the returned buffer.
func captureStoreLog(st *Store) *bytes.Buffer {
	var buf bytes.Buffer
	st.WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	return &buf
}

// assertNoPhoneA fails when s carries phoneA in any form: the number, its national part, a run of
// its digits, or its mask (P3).
func assertNoPhoneA(t *testing.T, s string) {
	t.Helper()
	for _, part := range []string{"905321116869", phoneADigits, "532111", "1116869", "•••••", "+90"} {
		if strings.Contains(s, part) {
			t.Fatalf("log carries the number (%q): %s", part, s)
		}
	}
}

// Pre-review 4: a person losing an unverified number to someone else's claim is told nowhere else,
// so the store logs it — both users' ids and the cause, never the number or its mask (P3).
func TestUserPhoneDisplacementIsLogged(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	logs := captureStoreLog(st)
	squatter := phoneUser(t, st, ctx, "squatter@example.com")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	mustSetPhone(t, st, ctx, squatter, phoneA)
	endPhoneCodes(t, st, ctx, squatter)
	if strings.Contains(logs.String(), "user_phone_released") {
		t.Fatalf("a first claim logged a release: %s", logs.String())
	}

	mustSetPhone(t, st, ctx, owner, phoneA)
	line := strings.TrimSpace(logs.String())
	if strings.Count(line, "user_phone_released") != 1 {
		t.Fatalf("want one user_phone_released line, got %q", logs.String())
	}
	for _, want := range []string{"user_id=" + squatter, "claimant_user_id=" + owner, "cause=claim"} {
		if !strings.Contains(line, want) {
			t.Fatalf("release line lacks %q: %s", want, line)
		}
	}
	assertNoPhoneA(t, line)
}

func TestUserPhoneVerifiedOwnerIsNeverDisplaced(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "111111")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	other := phoneUser(t, st, ctx, "other@example.com")
	mustSetPhone(t, st, ctx, owner, phoneA)
	if _, err := st.VerifyUserPhone(ctx, owner, "111111"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetUserPhone(ctx, other, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
		t.Fatalf("claim of a verified number: err = %v, want ErrPhoneNumberInUse", err)
	}
	if got, _ := st.GetUserPhone(ctx, owner); got == nil || !got.Verified {
		t.Fatalf("owner's phone after the refused claim: %+v", got)
	}
	if got, _ := st.GetUserPhone(ctx, other); got != nil {
		t.Fatalf("the refused user got a phone: %+v", got)
	}
}

func TestUserPhoneActiveCodeKeepsTheRow(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "111111")
	first := phoneUser(t, st, ctx, "first@example.com")
	second := phoneUser(t, st, ctx, "second@example.com")
	mustSetPhone(t, st, ctx, first, phoneA)
	if _, _, err := st.SetUserPhone(ctx, second, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
		t.Fatalf("claim while the holder's code runs: err = %v, want ErrPhoneNumberInUse", err)
	}
	if got, _ := st.GetUserPhone(ctx, first); got == nil {
		t.Fatal("the holder lost the number while its code was running")
	}
	if verified, err := st.VerifyUserPhone(ctx, first, "111111"); err != nil || !verified.Verified {
		t.Fatalf("the holder verifies after the refused claim: %+v %v", verified, err)
	}
}

// The squatter's code rows count for the number's daily limit after the squatter is gone, and a claim
// refused by that limit rolls the displacement back.
func TestUserPhoneDisplacementKeepsTheSendLimits(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	squatter := phoneUser(t, st, ctx, "squatter@example.com")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	late := phoneUser(t, st, ctx, "late@example.com")
	mustSetPhone(t, st, ctx, squatter, phoneA)
	endPhoneCodes(t, st, ctx, squatter)
	hash, _ := c.KeyedHash("phone-number", []byte(phoneA))
	for i := 0; i < domain.PhoneCodesPerNumberPerDay-2; i++ {
		if _, err := st.pool.Exec(ctx, `INSERT INTO phone_verifications
			(subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at, created_at)
			VALUES ('channel', gen_random_uuid(), gen_random_uuid()::text, $1, 'x', now() - interval '1 hour', now() - interval '2 hours')`, hash); err != nil {
			t.Fatal(err)
		}
	}
	// The tenth code of the day displaces the squatter.
	if _, _, err := st.SetUserPhone(ctx, owner, phoneA); err != nil {
		t.Fatalf("owner's claim, the tenth code: %v", err)
	}
	endPhoneCodes(t, st, ctx, owner)
	// The eleventh is refused: the squatter's code still counts, and nothing changes.
	if _, _, err := st.SetUserPhone(ctx, late, phoneA); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("eleventh code to the number: err = %v, want ErrPhoneSendLimited", err)
	}
	if got, _ := st.GetUserPhone(ctx, owner); got == nil {
		t.Fatal("a claim refused by the limit displaced the holder")
	}
	if got, _ := st.GetUserPhone(ctx, late); got != nil {
		t.Fatalf("the refused claim stored a phone: %+v", got)
	}
	// The squatter's own hourly budget is still spent by its code.
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, squatter); n != 1 {
		t.Fatalf("squatter's rows = %d, want 1", n)
	}
}

// Two people claiming one free-to-take number at once: exactly one holds it, the other is told the
// number is in use, and neither gets a 500 (a deadlock or a unique violation).
func TestUserPhoneConcurrentClaimsYieldOneOwner(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	squatter := phoneUser(t, st, ctx, "squatter@example.com")
	for round := 0; round < 20; round++ {
		if _, err := st.pool.Exec(ctx, `DELETE FROM user_phones`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `DELETE FROM phone_verifications`); err != nil {
			t.Fatal(err)
		}
		mustSetPhone(t, st, ctx, squatter, phoneA)
		endPhoneCodes(t, st, ctx, squatter)
		users := []string{
			phoneUser(t, st, ctx, "b"+string(rune('a'+round))+"@example.com"),
			phoneUser(t, st, ctx, "c"+string(rune('a'+round))+"@example.com"),
		}
		errs := make([]error, len(users))
		var wg sync.WaitGroup
		for i, u := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = st.SetUserPhone(ctx, u, phoneA)
			}()
		}
		wg.Wait()
		ok, inUse := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrPhoneNumberInUse):
				inUse++
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if ok != 1 || inUse != 1 {
			t.Fatalf("round %d: %d claims took the number, %d refused; want exactly one each", round, ok, inUse)
		}
		var holders int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM user_phones`).Scan(&holders); err != nil {
			t.Fatal(err)
		}
		if holders != 1 {
			t.Fatalf("round %d: %d rows hold the number, want 1", round, holders)
		}
	}
}

// M-9: a refused claim spends one of the claimant's codes of the hour, so the answer cannot be asked
// without limit. It does not count against the NUMBER's daily limit (nothing was sent to it), and it
// does not end the claimant's own running code.
func TestUserPhoneNumberInUseCountsAgainstTheHourlyBudget(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	scriptPhoneCodes(t, "111111", "222222")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	prober := phoneUser(t, st, ctx, "prober@example.com")
	mustSetPhone(t, st, ctx, owner, phoneA)
	if _, err := st.VerifyUserPhone(ctx, owner, "111111"); err != nil {
		t.Fatal(err)
	}
	mustSetPhone(t, st, ctx, prober, phoneB) // the prober's own number, code 222222

	for i := 1; i < domain.PhoneCodesPerRequesterPerHour; i++ {
		if _, _, err := st.SetUserPhone(ctx, prober, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
			t.Fatalf("probe %d: err = %v, want ErrPhoneNumberInUse", i, err)
		}
	}
	// The budget of the hour is spent: the next probe learns nothing.
	if _, _, err := st.SetUserPhone(ctx, prober, phoneA); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("probe past the budget: err = %v, want ErrPhoneSendLimited", err)
	}
	if n := numberCodes(t, st, ctx, c, phoneA); n != 1 {
		t.Fatalf("codes counted against the probed number = %d, want only the owner's 1", n)
	}
	if n := issuedCodes(t, st, ctx, domain.PhoneSubjectUser, prober); n != 1 {
		t.Fatalf("prober's issued codes = %d, want 1", n)
	}
	// The refusals did not end the prober's running code.
	if verified, err := st.VerifyUserPhone(ctx, prober, "222222"); err != nil || !verified.Verified {
		t.Fatalf("prober verifies its own number after the refusals: %+v %v", verified, err)
	}
}

// claimRendezvous holds two claims at each named step of SetUserPhone until both have reached it, or
// the other has already returned. It makes an interleaving certain instead of likely.
type claimRendezvous struct {
	steps map[string]*sync.WaitGroup
	once  map[string]map[string]*sync.Once // step -> user -> arrival
}

func newClaimRendezvous(users []string, steps ...string) *claimRendezvous {
	r := &claimRendezvous{steps: map[string]*sync.WaitGroup{}, once: map[string]map[string]*sync.Once{}}
	for _, step := range steps {
		wg := &sync.WaitGroup{}
		wg.Add(len(users))
		r.steps[step] = wg
		r.once[step] = map[string]*sync.Once{}
		for _, u := range users {
			r.once[step][u] = &sync.Once{}
		}
	}
	return r
}

func (r *claimRendezvous) arrive(user, step string) {
	if o := r.once[step][user]; o != nil {
		o.Do(r.steps[step].Done)
	}
}

// at is the store's seam: arrive, then wait for the other claim.
func (r *claimRendezvous) at(user, step string) {
	if r.steps[step] == nil || r.once[step][user] == nil {
		return
	}
	r.arrive(user, step)
	r.steps[step].Wait()
}

// gone releases every step for a claim that has returned.
func (r *claimRendezvous) gone(user string) {
	for step := range r.steps {
		r.arrive(user, step)
	}
}

// Pre-review 3: A holds X unverified and claims Y while B holds Y unverified and claims X. Both
// claims are held at the same two points — before either judges the holder, and after both have —
// so the interleaving that deadlocked (each locking the other's row, then waiting for its own) runs
// every time. Neither may answer anything but success or number_in_use, and no number may end up
// with two owners.
func TestUserPhoneSwapOfUnverifiedNumbersDoesNotDeadlock(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	a := phoneUser(t, st, ctx, "swap-a@example.com")
	b := phoneUser(t, st, ctx, "swap-b@example.com")
	mustSetPhone(t, st, ctx, a, phoneA)
	mustSetPhone(t, st, ctx, b, phoneB)
	endPhoneCodes(t, st, ctx, a)
	endPhoneCodes(t, st, ctx, b)

	meet := newClaimRendezvous([]string{a, b}, "before_claim", "claimed")
	st.phoneClaimStep = meet.at
	t.Cleanup(func() { st.phoneClaimStep = nil })

	claims := map[string]string{a: phoneB, b: phoneA}
	errs := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for user, number := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := st.SetUserPhone(ctx, user, number)
			meet.gone(user)
			mu.Lock()
			errs[user] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	st.phoneClaimStep = nil

	for user, err := range errs {
		if err != nil && !errors.Is(err, domain.ErrPhoneNumberInUse) {
			t.Fatalf("claim of %s: %v; want success or ErrPhoneNumberInUse", user, err)
		}
	}
	for user, number := range claims {
		if errs[user] != nil {
			continue
		}
		hash, _ := c.KeyedHash("phone-number", []byte(number))
		var holder string
		if err := st.pool.QueryRow(ctx, `SELECT user_id::text FROM user_phones WHERE number_hash = $1`, hash).
			Scan(&holder); err != nil || holder != user {
			t.Fatalf("a successful claim of %s left the number with %q (%v)", user, holder, err)
		}
	}
	var dup int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT number_hash FROM user_phones
		GROUP BY number_hash HAVING count(*) > 1) d`).Scan(&dup); err != nil || dup != 0 {
		t.Fatalf("numbers with more than one owner: %d (%v)", dup, err)
	}
	var unique bool
	if err := st.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conname = 'user_phones_number_hash_key' AND contype = 'u')`).Scan(&unique); err != nil || !unique {
		t.Fatalf("user_phones_number_hash_key is gone: %v", err)
	}
}
