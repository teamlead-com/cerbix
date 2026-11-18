package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// FR-041a code review, group B (§4.2, §14): who a code's hourly limit counts (P2-3), what a code
// spent by five attempts still does (P2-4), and the number's lock and rehash under a key rotation
// (P2-5, carried M-6).

// P2-3: the hourly limit is the REQUESTER's, across their own number and every `number` channel they
// verify. Counted per channel, a new channel was a new allowance.
func TestPhoneCodeHourlyLimitCountsTheRequesterAcrossChannels(t *testing.T) {
	w := newPhoneChannelWorld(t)
	numbers := []string{"+447700900101", "+447700900102", "+447700900103", "+447700900104"}
	var chans []string
	for _, n := range numbers {
		chans = append(chans, w.numberChannel(t, domain.ChannelSMS, n).ID)
	}
	for i := 0; i < domain.PhoneCodesPerRequesterPerHour; i++ {
		if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chans[i], w.admin); err != nil {
			t.Fatalf("code %d to its own channel: %v", i+1, err)
		}
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chans[3], w.admin); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("a fourth code in the hour through a new channel: %v, want ErrPhoneSendLimited", err)
	}
	if n := verificationRows(t, w.st, w.ctx, domain.PhoneSubjectChannel, chans[3]); n != 0 {
		t.Fatalf("the refused send wrote %d rows", n)
	}

	// Another person asking for the same channel has an hour of their own.
	other := w.member(t, "other-admin@example.com", domain.RoleViewer)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chans[3], other); err != nil {
		t.Fatalf("another requester: %v", err)
	}

	// An API token is counted as itself.
	const token = authzTokenRequester
	for i := 0; i < domain.PhoneCodesPerRequesterPerHour; i++ {
		if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chans[i], token); err != nil {
			t.Fatalf("token code %d: %v", i+1, err)
		}
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chans[3], token); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("the token's fourth code: %v, want ErrPhoneSendLimited", err)
	}
}

// authzTokenRequester is the requester of an API token principal: authz's synthetic user id.
const authzTokenRequester = "apitoken:00000000-0000-0000-0000-0000000000aa"

// P2-3: a person's own number and the channels they verify share one hour; a refused claim of a
// number (number_in_use, pre-review M-9) is one of them.
func TestPhoneCodeHourlyLimitSharedByOwnNumberChannelsAndRefusals(t *testing.T) {
	w := newPhoneChannelWorld(t)
	chA := w.numberChannel(t, domain.ChannelSMS, "+447700900111").ID
	chB := w.numberChannel(t, domain.ChannelSMS, "+447700900112").ID
	carol := w.member(t, "carol@example.com", domain.RoleViewer)

	mustSetPhone(t, w.st, w.ctx, carol, phoneB) // one: her own number
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chA, carol); err != nil {
		t.Fatalf("two: %v", err)
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chB, carol); err != nil {
		t.Fatalf("three: %v", err)
	}
	if _, _, err := w.st.SendUserPhoneCode(w.ctx, carol); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("a resend to her own number after two channel codes: %v, want ErrPhoneSendLimited", err)
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chA, carol); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("a fourth code: %v, want ErrPhoneSendLimited", err)
	}

	// A refusal counts on the person who asked, and a channel code then finds the hour spent.
	owner := w.member(t, "owner@example.com", domain.RoleViewer)
	w.verifiedPhone(t, owner, phoneA)
	dave := w.member(t, "dave@example.com", domain.RoleViewer)
	for i := 1; i < domain.PhoneCodesPerRequesterPerHour; i++ {
		if _, _, err := w.st.SetUserPhone(w.ctx, dave, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
			t.Fatalf("claim %d: %v, want ErrPhoneNumberInUse", i, err)
		}
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chB, dave); err != nil {
		t.Fatalf("the third of dave's hour: %v", err)
	}
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, chA, dave); !errors.Is(err, domain.ErrPhoneSendLimited) {
		t.Fatalf("after two refusals and a code: %v, want ErrPhoneSendLimited", err)
	}
}

// P2-3: a channel whose number is verified is sent no code (409 number_already_verified), and the
// refusal writes nothing and spends nothing.
func TestPhoneChannelVerifiedIsSentNoCode(t *testing.T) {
	w := newPhoneChannelWorld(t)
	ch := w.numberChannel(t, domain.ChannelSMS, phoneA)
	w.verifyNumberChannel(t, ch.ID)
	before := verificationRows(t, w.st, w.ctx, domain.PhoneSubjectChannel, ch.ID)
	other := w.member(t, "other-admin@example.com", domain.RoleViewer)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, other); !errors.Is(err, domain.ErrPhoneAlreadyVerified) {
		t.Fatalf("a code to a verified channel: %v, want ErrPhoneAlreadyVerified", err)
	}
	if after := verificationRows(t, w.st, w.ctx, domain.PhoneSubjectChannel, ch.ID); after != before {
		t.Fatalf("the refusal wrote: %d rows, had %d", after, before)
	}
	if got := w.get(t, ch.ID); got.NumberVerified == nil || !*got.NumberVerified {
		t.Fatal("the refusal touched the verification")
	}
}

// P2-4: a code spent by five wrong attempts is not a running code: the unverified number it was sent
// to yields to a new claim, as one whose code ran out does (I-1).
func TestUserPhoneExhaustedCodeDoesNotHoldTheNumber(t *testing.T) {
	st, ctx, _ := telephonyStore(t)
	scriptPhoneCodes(t, "482193", "222222")
	squatter := phoneUser(t, st, ctx, "squatter@example.com")
	owner := phoneUser(t, st, ctx, "owner@example.com")
	mustSetPhone(t, st, ctx, squatter, phoneA)
	for i := 1; i < domain.PhoneCodeMaxAttempts; i++ {
		if _, err := st.VerifyUserPhone(ctx, squatter, "111111"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Four attempts in, the code still runs: the number is held.
	if _, _, err := st.SetUserPhone(ctx, owner, phoneA); !errors.Is(err, domain.ErrPhoneNumberInUse) {
		t.Fatalf("claim against a code with an attempt left: %v, want ErrPhoneNumberInUse", err)
	}
	if _, err := st.VerifyUserPhone(ctx, squatter, "111111"); !errors.Is(err, domain.ErrPhoneCodeIncorrect) {
		t.Fatalf("fifth attempt: %v", err)
	}
	phone, code, err := st.SetUserPhone(ctx, owner, phoneA)
	if err != nil || code != "222222" || phone.Verified {
		t.Fatalf("claim against a code spent by five attempts: %+v %q %v; want the number and a code", phone, code, err)
	}
	if got, _ := st.GetUserPhone(ctx, squatter); got != nil {
		t.Fatalf("the squatter still holds a number: %+v", got)
	}
}

const numberLockClass = "phone_verification_number"

// advisoryLockWaiters counts the backends waiting for the advisory lock pg_advisory_xact_lock(
// hashtext(class), hashtext(key)) names.
func advisoryLockWaiters(t *testing.T, w phoneChannelWorld, class, key string) int {
	t.Helper()
	var n int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		 WHERE NOT l.granted AND l.locktype = 'advisory' AND a.datname = current_database()
		   AND l.pid <> pg_backend_pid() AND l.objsubid = 2
		   AND l.classid = hashtext($1)::oid AND l.objid = hashtext($2)::oid`, class, key).Scan(&n); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

// waitsOnAdvisoryLock runs op and fails unless it starts waiting on an advisory lock before it
// returns; release then lets it finish, and its error is returned.
// The wait must be on THIS lock: class and key name it (the pair hashtext() gives pg_advisory_xact_lock,
// which pg_locks shows as classid and objid), so another package's waiter in the same database cannot
// satisfy it.
func waitsOnAdvisoryLock(t *testing.T, w phoneChannelWorld, label, class, key string, release func(), op func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			release()
			t.Fatalf("%s: returned (%v) without waiting for the number's lock under the other key", label, err)
		default:
		}
		if n := advisoryLockWaiters(t, w, class, key); n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: never waited", label)
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: still waiting after the release", label)
		return nil
	}
}

// P2-5 and the carried M-6: every path that sends a code takes the number's lock under EVERY
// configured key. A process restarted on [new, old] and one still on [old] share the old key's hash,
// so they serialize the per-number count and a claim of the number; a lock on the primary key's hash
// alone let the two run side by side.
func TestPhoneVerificationNumberLockCoversEveryKey(t *testing.T) {
	w := newPhoneChannelWorld(t)
	keyOld, keyNew := randKey(t), randKey(t)
	cOld, _ := secret.New(keyOld)
	rotated, _ := secret.New(keyNew, keyOld)

	holdOldHash := func(t *testing.T, number string) (func(), string) {
		t.Helper()
		hOld, err := cOld.KeyedHash(phoneNumberHashLabel, []byte(number))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := w.st.pool.Begin(w.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(w.ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_verification_number'), hashtext($1))`, hOld); err != nil {
			t.Fatal(err)
		}
		release := func() { _ = tx.Rollback(w.ctx) }
		t.Cleanup(release)
		return release, hOld
	}

	w.st.WithCipher(rotated)
	t.Run("a claim of a number", func(t *testing.T) {
		u := w.member(t, "claimant@example.com", domain.RoleViewer)
		release, hOld := holdOldHash(t, "+447700900121")
		if err := waitsOnAdvisoryLock(t, w, "SetUserPhone", numberLockClass, hOld, release, func() error {
			_, _, err := w.st.SetUserPhone(w.ctx, u, "+447700900121")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a resend to a person's stored number (M-6)", func(t *testing.T) {
		u := w.member(t, "resender@example.com", domain.RoleViewer)
		mustSetPhone(t, w.st, w.ctx, u, "+447700900122") // stored under the new primary's hash
		release, hOld := holdOldHash(t, "+447700900122")
		if err := waitsOnAdvisoryLock(t, w, "SendUserPhoneCode", numberLockClass, hOld, release, func() error {
			_, _, err := w.st.SendUserPhoneCode(w.ctx, u)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a channel's code", func(t *testing.T) {
		ch := w.numberChannel(t, domain.ChannelSMS, "+447700900123")
		release, hOld := holdOldHash(t, "+447700900123")
		if err := waitsOnAdvisoryLock(t, w, "SendChannelVerificationCode", numberLockClass, hOld, release, func() error {
			_, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// P2-3 follow-up: the requester's lock is what makes the hourly budget the requester's. Two sends by
// ONE requester for two different subjects (their own number, a number channel) hold different
// subject locks, so only the requester's lock orders their count. The interleaving is fixed by
// holding that lock in a test transaction: both sends must reach it and wait there; released, they
// take it one after the other, so the second sees the first's row and the budget (3 an hour, two
// spent) ends in ONE success. Without the lock neither send waits, and the test fails on that.
func TestPhoneRequesterLockSerializesSendsAcrossSubjects(t *testing.T) {
	w := newPhoneChannelWorld(t)
	u := w.member(t, "requester@example.com", domain.RoleViewer)
	scriptPhoneCodes(t, "111111", "222222", "333333", "444444", "555555")
	spend := []string{w.numberChannel(t, domain.ChannelSMS, "+447700900131").ID, w.numberChannel(t, domain.ChannelSMS, "+447700900132").ID}
	for i, id := range spend {
		if _, _, err := w.st.SendChannelVerificationCode(w.ctx, id, u); err != nil {
			t.Fatalf("seed code %d: %v", i+1, err)
		}
	}
	chA := w.numberChannel(t, domain.ChannelSMS, "+447700900133").ID
	chB := w.numberChannel(t, domain.ChannelSMS, "+447700900134").ID
	ops := []func() error{
		func() error { _, _, err := w.st.SetUserPhone(w.ctx, u, "+447700900135"); return err },
		func() error { _, _, err := w.st.SendChannelVerificationCode(w.ctx, chA, u); return err },
		func() error { _, _, err := w.st.SendChannelVerificationCode(w.ctx, chB, u); return err },
	}

	hold, err := w.st.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = hold.Rollback(w.ctx)
		}
	}
	t.Cleanup(release)
	if _, err := hold.Exec(w.ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_verification_requester'), hashtext($1))`, u); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, len(ops))
	for _, op := range ops {
		go func() { results <- op() }()
	}
	deadline := time.Now().Add(15 * time.Second)
	for advisoryLockWaiters(t, w, "phone_verification_requester", u) < len(ops) {
		if len(results) > 0 {
			release()
			t.Fatalf("a send by the requester finished without waiting for the requester's lock")
		}
		if time.Now().After(deadline) {
			release()
			t.Fatalf("only %d of %d sends wait on the requester's lock", advisoryLockWaiters(t, w, "phone_verification_requester", u), len(ops))
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	ok, limited := 0, 0
	for range ops {
		select {
		case err := <-results:
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrPhoneSendLimited):
				limited++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a send is still waiting after the release")
		}
	}
	if want := domain.PhoneCodesPerRequesterPerHour - len(spend); ok != want || limited != len(ops)-want {
		t.Fatalf("%d sent, %d limited; want %d sent (budget %d, %d spent)", ok, limited, want, domain.PhoneCodesPerRequesterPerHour, len(spend))
	}
	var rows int
	if err := w.st.pool.QueryRow(w.ctx, `SELECT count(*) FROM phone_verifications WHERE requested_by = $1`, u).Scan(&rows); err != nil || rows != domain.PhoneCodesPerRequesterPerHour {
		t.Fatalf("rows of the requester = %d, %v; want %d", rows, err, domain.PhoneCodesPerRequesterPerHour)
	}
}

// P2-5: a `number` channel's rehash moves ITS OWN code rows to the new hash, never the code of a
// person who holds the same number under the old hash — that row is not rehashed yet (skipped as
// busy, or written by a process still on the old key after the scan), and its code must keep
// verifying it. Rows of a subject that no longer holds the number (a deleted channel) are never a
// running code; they move, so the number's daily count follows them across the rotation.
func TestReencryptChannelMovesOnlyItsOwnCodes(t *testing.T) {
	w := newPhoneChannelWorld(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	w.st.WithCipher(cA)
	hashA, _ := cA.KeyedHash(phoneNumberHashLabel, []byte(phoneA))
	cB, _ := secret.New(keyB)
	hashB, _ := cB.KeyedHash(phoneNumberHashLabel, []byte(phoneA))

	scriptPhoneCodes(t, "111111", "222222", "333333")
	alice := w.member(t, "alice@example.com", domain.RoleViewer)
	mustSetPhone(t, w.st, w.ctx, alice, phoneA) // 111111, under A
	ch := w.numberChannel(t, domain.ChannelSMS, phoneA)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, ch.ID, w.admin); err != nil { // 222222
		t.Fatal(err)
	}
	gone := w.numberChannel(t, domain.ChannelSMS, phoneA)
	if _, _, err := w.st.SendChannelVerificationCode(w.ctx, gone.ID, w.admin); err != nil { // 333333
		t.Fatal(err)
	}
	if err := w.st.DeletePhoneChannel(w.ctx, gone.ID, w.actor, nil); err != nil {
		t.Fatal(err)
	}

	rotated, _ := secret.New(keyB, keyA)
	w.st.WithCipher(rotated)
	if err := w.st.reencryptChannelNumbers(w.ctx); err != nil {
		t.Fatalf("reencrypt channel numbers: %v", err)
	}
	hashOf := func(kind, subject string) string {
		t.Helper()
		var h string
		if err := w.st.pool.QueryRow(w.ctx, `SELECT number_hash FROM phone_verifications
			 WHERE subject_kind = $1 AND subject_id = $2 AND code_hash <> ''`, kind, subject).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	if hashOf(domain.PhoneSubjectChannel, ch.ID) != hashB {
		t.Fatal("the channel's own code did not follow its number")
	}
	if hashOf(domain.PhoneSubjectUser, alice) != hashA {
		t.Fatal("the channel's rehash moved alice's code, whose row is still under the old hash")
	}
	if hashOf(domain.PhoneSubjectChannel, gone.ID) != hashB {
		t.Fatal("the deleted channel's code row was left under the old hash")
	}
	if phone, err := w.st.VerifyUserPhone(w.ctx, alice, "111111"); err != nil || !phone.Verified {
		t.Fatalf("alice's right code after the channel's rehash: %+v %v", phone, err)
	}
}
