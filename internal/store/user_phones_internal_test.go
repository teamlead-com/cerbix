package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const (
	phoneA       = "+905321116869"
	phoneADigits = "5321116869"
	phoneB       = "+447700900199"
)

// scriptPhoneCodes makes the next verification codes the given ones, in order; past the end the
// codes are "000000".
func scriptPhoneCodes(t *testing.T, codes ...string) {
	t.Helper()
	prev := newPhoneCode
	t.Cleanup(func() { newPhoneCode = prev })
	newPhoneCode = func() (string, error) {
		if len(codes) == 0 {
			return "000000", nil
		}
		c := codes[0]
		codes = codes[1:]
		return c, nil
	}
}

func phoneUser(t *testing.T, st *Store, ctx context.Context, email string) string {
	t.Helper()
	u, err := st.CreateLocalUser(ctx, email, "Person "+email, "x", false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

func mustSetPhone(t *testing.T, st *Store, ctx context.Context, userID, number string) string {
	t.Helper()
	_, code, err := st.SetUserPhone(ctx, userID, number)
	if err != nil {
		t.Fatalf("set phone: %v", err)
	}
	return code
}

func verificationRows(t *testing.T, st *Store, ctx context.Context, kind, subjectID string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM phone_verifications WHERE subject_kind = $1 AND subject_id = $2`, kind, subjectID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUserPhoneSetStoresUnverifiedCiphertextAndIssuesCode(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")

	phone, code, err := st.SetUserPhone(ctx, uid, "+90 532 111 68 69")
	if err != nil {
		t.Fatal(err)
	}
	if code != "482193" {
		t.Fatalf("code = %q, want the issued one", code)
	}
	if phone.NumberMasked != "+90•••••6869" || phone.Verified || phone.Language != nil {
		t.Fatalf("phone = %+v", phone)
	}
	got, err := st.GetUserPhone(ctx, uid)
	if err != nil || got == nil || *got != phone {
		t.Fatalf("GetUserPhone = %+v, %v; want %+v", got, err, phone)
	}

	var enc, hash, masked string
	if err := st.pool.QueryRow(ctx,
		`SELECT number_enc, number_hash, number_masked FROM user_phones WHERE user_id = $1`, uid).
		Scan(&enc, &hash, &masked); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{enc, hash} {
		if strings.Contains(col, phoneADigits) {
			t.Fatalf("a stored column carries the number: %q", col)
		}
	}
	pt, err := c.DecryptBytes(enc, secret.CanonicalAAD("user_phone", uid))
	if err != nil || string(pt) != phoneA {
		t.Fatalf("number_enc opens to %q, %v; want the normalized number under AAD(user_phone, id)", pt, err)
	}
	if want, _ := c.KeyedHash("phone-number", []byte(phoneA)); hash != want {
		t.Fatalf("number_hash = %s, want KeyedHash(phone-number, number)", hash)
	}

	var codeHash, numberHash, id string
	var attempts int
	var ttlOK bool
	if err := st.pool.QueryRow(ctx, `SELECT id, code_hash, number_hash, attempts,
		       expires_at BETWEEN created_at + interval '9 minutes 59 seconds' AND created_at + interval '10 minutes 1 second'
		  FROM phone_verifications WHERE subject_kind = 'user' AND subject_id = $1`, uid).
		Scan(&id, &codeHash, &numberHash, &attempts, &ttlOK); err != nil {
		t.Fatal(err)
	}
	if want, _ := c.KeyedHash("phone-verification", []byte(id+":482193")); codeHash != want {
		t.Fatal("code_hash is not KeyedHash(phone-verification, id:code)")
	}
	if numberHash != hash || attempts != 0 || !ttlOK {
		t.Fatalf("verification row: number_hash match %v, attempts %d, ten-minute ttl %v", numberHash == hash, attempts, ttlOK)
	}
}

func TestUserPhoneVerifyIncorrectThenCorrect(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)

	if _, err := st.VerifyUserPhone(ctx, uid, "111111"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("wrong code: err = %v, want ErrPhoneCodeIncorrect", err)
	}
	var attempts int
	if err := st.pool.QueryRow(ctx, `SELECT attempts FROM phone_verifications WHERE subject_id = $1`, uid).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts = %d, %v; want the wrong attempt counted and committed", attempts, err)
	}
	phone, err := st.VerifyUserPhone(ctx, uid, "482193")
	if err != nil || !phone.Verified || phone.NumberMasked != "+90•••••6869" {
		t.Fatalf("right code: %+v, %v", phone, err)
	}
	if got, _ := st.GetUserPhone(ctx, uid); got == nil || !got.Verified {
		t.Fatalf("after verify: %+v", got)
	}
	// A used code is spent.
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); !errors.Is(err, domain.ErrPhoneCodeExpired) {
		t.Fatalf("reused code: err = %v, want ErrPhoneCodeExpired", err)
	}
}

func TestUserPhoneCodeExpires(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	if _, err := st.pool.Exec(ctx,
		`UPDATE phone_verifications SET expires_at = now() - interval '1 second', created_at = now() - interval '11 minutes'
		  WHERE subject_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); !errors.Is(err, domain.ErrPhoneCodeExpired) {
		t.Fatalf("expired code: err = %v, want ErrPhoneCodeExpired", err)
	}
	if got, _ := st.GetUserPhone(ctx, uid); got == nil || got.Verified {
		t.Fatalf("an expired code verified the number: %+v", got)
	}
}

func TestUserPhoneCodeExhaustedAfterFiveAttempts(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	for i := 1; i <= domain.PhoneCodeMaxAttempts; i++ {
		if _, err := st.VerifyUserPhone(ctx, uid, "111111"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
			t.Fatalf("attempt %d: err = %v, want ErrPhoneCodeIncorrect", i, err)
		}
	}
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); !errors.Is(err, domain.ErrPhoneCodeAttemptsExhausted) {
		t.Fatalf("right code after five wrong ones: err = %v, want ErrPhoneCodeAttemptsExhausted", err)
	}
	if got, _ := st.GetUserPhone(ctx, uid); got == nil || got.Verified {
		t.Fatalf("an exhausted code verified the number: %+v", got)
	}
}

func TestUserPhoneNewSendSupersedesTheActiveCode(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "111111", "222222")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	mustSetPhone(t, st, ctx, uid, phoneA) // re-sent to the same unverified number

	var active int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM phone_verifications WHERE subject_id = $1 AND expires_at > now()`, uid).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active codes = %d, %v; want one (R5)", active, err)
	}
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, uid); n != 2 {
		t.Fatalf("rows = %d, want the superseded row kept for the send limits", n)
	}
	if _, err := st.VerifyUserPhone(ctx, uid, "111111"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("superseded code: err = %v, want ErrPhoneCodeIncorrect", err)
	}
	if phone, err := st.VerifyUserPhone(ctx, uid, "222222"); err != nil || !phone.Verified {
		t.Fatalf("new code: %+v, %v", phone, err)
	}
}

func TestUserPhoneSendLimitPerSubject(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "a@example.com")
	for i := 0; i < domain.PhoneCodesPerRequesterPerHour; i++ {
		mustSetPhone(t, st, ctx, uid, phoneA)
	}
	if _, _, err := st.SetUserPhone(ctx, uid, phoneB); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("fourth send in the hour: err = %v, want ErrPhoneSendLimited", err)
	}
	if got, _ := st.GetUserPhone(ctx, uid); got == nil || got.NumberMasked != "+90•••••6869" {
		t.Fatalf("the refused write changed the stored number: %+v", got)
	}
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, uid); n != 3 {
		t.Fatalf("rows = %d, want the refused send to leave no row", n)
	}
	// The hour is rolling.
	if _, err := st.pool.Exec(ctx,
		`UPDATE phone_verifications SET created_at = created_at - interval '61 minutes' WHERE subject_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetUserPhone(ctx, uid, phoneB); err != nil {
		t.Fatalf("send after the hour: %v", err)
	}
}

// Concurrent sends to one subject are serialized by its lock: exactly the allowed number succeed.
func TestUserPhoneSendLimitHoldsUnderConcurrency(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "a@example.com")
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := st.SetUserPhone(ctx, uid, phoneA)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok, limited := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrPhoneSendLimited):
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != domain.PhoneCodesPerRequesterPerHour || limited != n-ok {
		t.Fatalf("sent %d, limited %d; want %d sent", ok, limited, domain.PhoneCodesPerRequesterPerHour)
	}
	if rows := verificationRows(t, st, ctx, domain.PhoneSubjectUser, uid); rows != domain.PhoneCodesPerRequesterPerHour {
		t.Fatalf("rows = %d", rows)
	}
}

func TestUserPhoneSendLimitPerNumber(t *testing.T) {
	st, ctx, c := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "a@example.com")
	hash, _ := c.KeyedHash("phone-number", []byte(phoneA))
	seed := func(n int, age string) {
		for i := 0; i < n; i++ {
			if _, err := st.pool.Exec(ctx, `INSERT INTO phone_verifications
				(subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at, created_at)
				VALUES ('channel', gen_random_uuid(), gen_random_uuid()::text, $1, 'x', now(), now() - $2::interval)`, hash, age); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(3, "25 hours") // outside the rolling day
	seed(domain.PhoneCodesPerNumberPerDay, "23 hours")
	if _, _, err := st.SetUserPhone(ctx, uid, phoneA); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("eleventh code to one number in a day: err = %v, want ErrPhoneSendLimited", err)
	}
	if got, _ := st.GetUserPhone(ctx, uid); got != nil {
		t.Fatalf("the refused write stored the number: %+v", got)
	}
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM phone_verifications WHERE id = (SELECT id FROM phone_verifications
		  WHERE created_at > now() - interval '24 hours' LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetUserPhone(ctx, uid, phoneA); err != nil {
		t.Fatalf("tenth code in the day: %v", err)
	}
}

// Between the key change and `cerbix reencrypt`, rows hashed under the previous key still count for
// the per-number limit (lookups under every configured key, D35).
func TestUserPhoneSendLimitPerNumberCountsThePreviousKey(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	oldHash, _ := cA.KeyedHash("phone-number", []byte(phoneA))
	for i := 0; i < domain.PhoneCodesPerNumberPerDay; i++ {
		if _, err := st.pool.Exec(ctx, `INSERT INTO phone_verifications
			(subject_kind, subject_id, requested_by, number_hash, code_hash, expires_at)
			VALUES ('channel', gen_random_uuid(), gen_random_uuid()::text, $1, 'x', now())`, oldHash); err != nil {
			t.Fatal(err)
		}
	}
	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	uid := phoneUser(t, st, ctx, "a@example.com")
	if _, _, err := st.SetUserPhone(ctx, uid, phoneA); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("err = %v, want the rows under the previous key counted", err)
	}
}

func TestUserPhoneNumberInUseAcrossUsers(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	a := phoneUser(t, st, ctx, "a@example.com")
	b := phoneUser(t, st, ctx, "b@example.com")
	mustSetPhone(t, st, ctx, a, phoneA)
	if _, _, err := st.SetUserPhone(ctx, b, "+90 (532) 111-68-69"); !errors.Is(err, domain.ErrPhoneNumberInUse) {
		t.Fatalf("second user, same number: err = %v, want ErrPhoneNumberInUse", err)
	}
	if got, _ := st.GetUserPhone(ctx, b); got != nil {
		t.Fatalf("the refused user got a phone: %+v", got)
	}
	if n := issuedCodes(t, st, ctx, domain.PhoneSubjectUser, b); n != 0 {
		t.Fatalf("the refused write issued %d codes", n)
	}
	// The refusal itself is recorded against b's hour (final review M-9).
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, b); n != 1 {
		t.Fatalf("rows of the refused user = %d, want the one refusal", n)
	}
}

// Between the key change and `cerbix reencrypt` a number is still unique and still matched (§11
// Settings, D35): the other user's row is hashed under the previous key.
func TestUserPhoneNumberInUseUnderThePreviousKey(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA)
	scriptPhoneCodes(t, "482193")
	a := phoneUser(t, st, ctx, "a@example.com")
	b := phoneUser(t, st, ctx, "b@example.com")
	mustSetPhone(t, st, ctx, a, phoneA)
	if _, err := st.VerifyUserPhone(ctx, a, "482193"); err != nil {
		t.Fatal(err)
	}

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	if _, _, err := st.SetUserPhone(ctx, b, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
		t.Fatalf("err = %v, want ErrPhoneNumberInUse against the row under the previous key", err)
	}
	// The owner's own re-PUT of the verified number is still recognised as the same number.
	phone, code, err := st.SetUserPhone(ctx, a, phoneA)
	if err != nil || code != "" || !phone.Verified {
		t.Fatalf("owner re-PUT under the new key: %+v code=%q %v; want the no-op", phone, code, err)
	}
}

func TestUserPhoneSameVerifiedNumberIsNoOp(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); err != nil {
		t.Fatal(err)
	}
	phone, code, err := st.SetUserPhone(ctx, uid, "+90 532 111 68 69")
	if err != nil || code != "" || !phone.Verified {
		t.Fatalf("same verified number: %+v code=%q %v; want verified and no code", phone, code, err)
	}
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, uid); n != 1 {
		t.Fatalf("rows = %d, want no new code for the no-op", n)
	}
}

func TestUserPhoneReplaceClearsVerificationAndKeepsLanguage(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); err != nil {
		t.Fatal(err)
	}
	lang := "tr-TR"
	if _, err := st.SetUserPhoneLanguage(ctx, uid, &lang); err != nil {
		t.Fatal(err)
	}
	phone, code, err := st.SetUserPhone(ctx, uid, phoneB)
	if err != nil || code == "" {
		t.Fatalf("replace: code=%q %v", code, err)
	}
	if phone.Verified || phone.NumberMasked != "+44•••••0199" || phone.Language == nil || *phone.Language != "tr-TR" {
		t.Fatalf("after replace: %+v", phone)
	}
}

func TestUserPhoneRefusedForMachineUserAndWithoutKey(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "m@example.com")
	if _, err := st.pool.Exec(ctx, `UPDATE users SET kind = 'machine' WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetUserPhone(ctx, uid, phoneA); !errors.Is(err, domain.ErrPhoneNotHuman) {
		t.Fatalf("machine user: err = %v, want ErrPhoneNotHuman", err)
	}
	if _, _, err := st.SetUserPhone(ctx, "00000000-0000-0000-0000-000000000000", phoneA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: err = %v, want ErrNotFound", err)
	}

	human := phoneUser(t, st, ctx, "h@example.com")
	st.WithCipher(nil)
	if _, _, err := st.SetUserPhone(ctx, human, phoneA); !errors.Is(err, domain.ErrPhoneKeyMissing) {
		t.Fatalf("no key: err = %v, want ErrPhoneKeyMissing", err)
	}
	if _, _, err := st.SetUserPhone(ctx, human, "12345"); err == nil {
		t.Fatal("an invalid number was accepted")
	}
	var rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM user_phones`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("user_phones rows = %d, %v; want none", rows, err)
	}
}

func TestUserPhoneInvalidNumberRefused(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "a@example.com")
	_, _, err := st.SetUserPhone(ctx, uid, "0532 111 68 69")
	if !errors.Is(err, domain.ErrInvalidPhone) {
		t.Fatalf("err = %v, want ErrInvalidPhone", err)
	}
	if strings.Contains(err.Error(), "1116869") {
		t.Fatalf("the error quotes the input: %v", err)
	}
}

func TestUserPhoneDeleteEndsTheCode(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	mustSetPhone(t, st, ctx, uid, phoneA)
	if err := st.DeleteUserPhone(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetUserPhone(ctx, uid); err != nil || got != nil {
		t.Fatalf("after delete: %+v, %v", got, err)
	}
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify after delete: err = %v, want ErrNotFound", err)
	}
	var active int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM phone_verifications WHERE subject_id = $1 AND expires_at > now()`, uid).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active codes after delete = %d, %v", active, err)
	}
	if err := st.DeleteUserPhone(ctx, uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
	// A deleted number is free for another user.
	other := phoneUser(t, st, ctx, "b@example.com")
	mustSetPhone(t, st, ctx, other, phoneA)
}

func TestUserPhoneLanguage(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	uid := phoneUser(t, st, ctx, "a@example.com")
	lang := "tr-TR"
	if _, err := st.SetUserPhoneLanguage(ctx, uid, &lang); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no phone: err = %v, want ErrNotFound", err)
	}
	mustSetPhone(t, st, ctx, uid, phoneA)
	phone, err := st.SetUserPhoneLanguage(ctx, uid, &lang)
	if err != nil || phone.Language == nil || *phone.Language != "tr-TR" || phone.NumberMasked != "+90•••••6869" {
		t.Fatalf("set language: %+v, %v", phone, err)
	}
	phone, err = st.SetUserPhoneLanguage(ctx, uid, nil)
	if err != nil || phone.Language != nil {
		t.Fatalf("clear language: %+v, %v", phone, err)
	}
}

func TestVerifiedUserPhoneNumber(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193")
	uid := phoneUser(t, st, ctx, "a@example.com")
	if _, err := st.VerifiedUserPhoneNumber(ctx, uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no phone: err = %v, want ErrNotFound", err)
	}
	mustSetPhone(t, st, ctx, uid, phoneA)
	if _, err := st.VerifiedUserPhoneNumber(ctx, uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified: err = %v, want ErrNotFound", err)
	}
	if _, err := st.VerifyUserPhone(ctx, uid, "482193"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.VerifiedUserPhoneNumber(ctx, uid); err != nil || got != phoneA {
		t.Fatalf("verified: %q, %v", got, err)
	}
}

func TestReencryptCoversUserPhones(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	cB, _ := secret.New(keyB)
	st.WithCipher(cA)
	scriptPhoneCodes(t, "482193", "777777")
	a := phoneUser(t, st, ctx, "a@example.com")
	b := phoneUser(t, st, ctx, "b@example.com")
	mustSetPhone(t, st, ctx, a, phoneA)
	if _, err := st.VerifyUserPhone(ctx, a, "482193"); err != nil {
		t.Fatal(err)
	}
	mustSetPhone(t, st, ctx, b, phoneB) // unverified, with an active code

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	if _, err := st.ReencryptSecrets(ctx); err != nil {
		t.Fatalf("reencrypt: %v", err)
	}

	// A code issued before the rotation still verifies the (unchanged) number while the old key is
	// configured: its verification row now carries the recomputed number hash.
	if phone, err := st.VerifyUserPhone(ctx, b, "777777"); err != nil || !phone.Verified {
		t.Fatalf("code across the rotation: %+v, %v", phone, err)
	}

	st.WithCipher(cB)
	if got, err := st.VerifiedUserPhoneNumber(ctx, a); err != nil || got != phoneA {
		t.Fatalf("under key B alone: %q, %v", got, err)
	}
	for uid, number := range map[string]string{a: phoneA, b: phoneB} {
		var hash string
		if err := st.pool.QueryRow(ctx, `SELECT number_hash FROM user_phones WHERE user_id = $1`, uid).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if want, _ := cB.KeyedHash("phone-number", []byte(number)); hash != want {
			t.Fatalf("number_hash of %s not recomputed under the new primary", uid)
		}
		var stale int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM phone_verifications WHERE subject_id = $1 AND number_hash <> $2`, uid, hash).Scan(&stale); err != nil || stale != 0 {
			t.Fatalf("verification rows of %s still hashed under the old key: %d, %v", uid, stale, err)
		}
	}
	if got, _ := st.GetUserPhone(ctx, a); got == nil || !got.Verified {
		t.Fatalf("reencrypt lost the verification: %+v", got)
	}
	st.WithCipher(cA)
	if _, err := st.VerifiedUserPhoneNumber(ctx, a); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("the old key still opens the rewritten number: err = %v", err)
	}
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// Task 7 review, Minor 1: every write of a phone number or its codes takes its locks in ONE order —
// the subject's advisory lock, then the owning row, then the verification rows. Before, a set locked
// user_phones then the code row while a verify locked the code row then user_phones, and a resend
// racing a correct code could deadlock (40P01, answered as a 500).
func TestUserPhoneSetAndVerifyDoNotDeadlock(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	prev := newPhoneCode
	t.Cleanup(func() { newPhoneCode = prev })
	newPhoneCode = func() (string, error) { return "123456", nil }
	uid := phoneUser(t, st, ctx, "racer@example.com")
	for i := 0; i < 80; i++ {
		// Only the newest code stays inside the send-limit windows.
		if _, err := st.pool.Exec(ctx, `UPDATE phone_verifications SET created_at = created_at - interval '2 days'`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.SetUserPhone(ctx, uid, phoneB); err != nil {
			t.Fatalf("round %d: set: %v", i, err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); _, err := st.VerifyUserPhone(ctx, uid, "123456"); errs <- err }()
		go func() { defer wg.Done(); _, _, err := st.SetUserPhone(ctx, uid, phoneB); errs <- err }()
		go func() { defer wg.Done(); errs <- st.DeleteUserPhone(ctx, uid) }()
		wg.Wait()
		close(errs)
		for err := range errs {
			if isDeadlock(err) {
				t.Fatalf("round %d: deadlock: %v", i, err)
			}
			if err != nil && !isPhoneCodeVerdict(err) && !errors.Is(err, ErrNotFound) &&
				!errors.Is(err, domain.ErrPhoneSendLimited) {
				t.Fatalf("round %d: %v", i, err)
			}
		}
	}
}
