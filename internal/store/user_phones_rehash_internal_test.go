package store

import (
	"context"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// Pre-review 2. During a rolling key rotation two processes on different primary keys can each
// accept the same number for two people: the process still on key A matches the number only under
// A, so a row stored under B's hash by the other process is invisible to it. `cerbix reencrypt`
// then rehashes the row under A to B's hash and meets the other row at user_phones_number_hash_key.
// It settles the duplicate by I-1's yield rule in the rehash's own transaction, as the controller
// ruled: an unverified row yields to a verified one; of two verified rows the one verified first
// keeps the number; of two unverified rows the one with a running code keeps it, else the one
// written first. The loser's row is deleted and logged; its code rows stay. The command always
// goes on to the monitor and TOTP secrets.

// rotationWitnesses are secrets the command rewrites AFTER the phone numbers: a monitor password
// and a TOTP secret, encrypted under the old key.
type rotationWitnesses struct{ monitorID, userID string }

func seedRotationWitnesses(t *testing.T, st *Store, ctx context.Context, old *secret.Cipher) rotationWitnesses {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "rot", "Rot")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "rot", "Rot")
	if err != nil {
		t.Fatal(err)
	}
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		RecoveryThreshold: 1, ProjectID: proj.ID, Name: "db", Type: domain.MonitorTCP, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, Config: map[string]string{"password": "dbpass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	usr, err := st.CreateLocalUser(ctx, "totp@example.com", "T", "pwhash", false)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := old.Encrypt("totpsecret")
	if _, err := st.pool.Exec(ctx, `UPDATE users SET totp_secret = $2 WHERE id = $1`, usr.ID, enc); err != nil {
		t.Fatal(err)
	}
	return rotationWitnesses{monitorID: mon.ID, userID: usr.ID}
}

// assertRotated fails unless the witnesses read under the new key alone.
func (w rotationWitnesses) assertRotated(t *testing.T, st *Store, ctx context.Context, newKey *secret.Cipher) {
	t.Helper()
	var pw, totp string
	if err := st.pool.QueryRow(ctx, `SELECT config->>'password' FROM monitors WHERE id = $1`, w.monitorID).Scan(&pw); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT totp_secret FROM users WHERE id = $1`, w.userID).Scan(&totp); err != nil {
		t.Fatal(err)
	}
	if got, err := newKey.Decrypt(pw); err != nil || got != "dbpass" {
		t.Fatalf("monitor password not re-encrypted: %q %v", got, err)
	}
	if got, err := newKey.Decrypt(totp); err != nil || got != "totpsecret" {
		t.Fatalf("TOTP secret not re-encrypted: %q %v", got, err)
	}
}

// rehashCollision is a store where `onNew` holds phoneA under key B's hash and `onOld` holds it under
// key A's, the store's keyring is [B, A], and the witnesses wait under A.
type rehashCollision struct {
	st             *Store
	ctx            context.Context
	cA, cB         *secret.Cipher
	onNew, onOld   string
	hashA, hashB   string
	witnesses      rotationWitnesses
	logs           interface{ String() string }
	resetLogs      func()
	rowOf          func(userID string) (hash string, ok bool)
	verifiedNumber func(userID string) (string, error)
}

// row shapes one side of the collision.
type rowShape struct {
	verifiedAgo string // "" = unverified, else an interval: when it was verified
	activeCode  bool   // the code SetUserPhone issued is left running
	// exhaustedCode spends that code with five wrong attempts: no longer running (P2-4).
	exhaustedCode bool
	updatedAgo    string // an interval: when the row was last written
}

func newRehashCollision(t *testing.T, onNewShape, onOldShape rowShape) *rehashCollision {
	t.Helper()
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	cB, _ := secret.New(keyB)
	logs := captureStoreLog(st)
	st.WithCipher(cA)
	c := &rehashCollision{st: st, ctx: ctx, cA: cA, cB: cB, logs: logs, resetLogs: logs.Reset}
	c.witnesses = seedRotationWitnesses(t, st, ctx, cA)
	c.onNew = phoneUser(t, st, ctx, "on-new@example.com")
	c.onOld = phoneUser(t, st, ctx, "on-old@example.com")
	c.hashA, _ = cA.KeyedHash("phone-number", []byte(phoneA))
	c.hashB, _ = cB.KeyedHash("phone-number", []byte(phoneA))

	// The process already on B stores the number for one person; the process still on A, which
	// matches it only under A, accepts it for another.
	st.WithCipher(cB)
	mustSetPhone(t, st, ctx, c.onNew, phoneA)
	st.WithCipher(cA)
	mustSetPhone(t, st, ctx, c.onOld, phoneA)

	for user, shape := range map[string]rowShape{c.onNew: onNewShape, c.onOld: onOldShape} {
		if !shape.activeCode {
			endPhoneCodes(t, st, ctx, user)
		}
		if shape.exhaustedCode {
			if _, err := st.pool.Exec(ctx, `UPDATE phone_verifications SET attempts = $2
				WHERE subject_kind = 'user' AND subject_id = $1`, user, domain.PhoneCodeMaxAttempts); err != nil {
				t.Fatal(err)
			}
		}
		verified := "NULL"
		if shape.verifiedAgo != "" {
			verified = "now() - interval '" + shape.verifiedAgo + "'"
		}
		if _, err := st.pool.Exec(ctx, `UPDATE user_phones SET verified_at = `+verified+`,
			updated_at = now() - $2::interval WHERE user_id = $1`, user, shape.updatedAgo); err != nil {
			t.Fatal(err)
		}
	}
	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	c.rowOf = func(userID string) (string, bool) {
		var hash string
		err := st.pool.QueryRow(ctx, `SELECT number_hash FROM user_phones WHERE user_id = $1`, userID).Scan(&hash)
		if noRows(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return hash, true
	}
	return c
}

// assertSettled fails unless keeper holds the number under B's hash, readable under B alone, the
// loser holds nothing, the loser's code rows are kept, no code row is left under A's hash, and one
// release line names both ids without the number.
func (c *rehashCollision) assertSettled(t *testing.T, keeper, loser string, keeperVerified bool) {
	t.Helper()
	st, ctx := c.st, c.ctx
	if hash, ok := c.rowOf(keeper); !ok || hash != c.hashB {
		t.Fatalf("keeper %s: row=%v hash under B=%v", keeper, ok, hash == c.hashB)
	}
	if _, ok := c.rowOf(loser); ok {
		t.Fatalf("loser %s still holds a row", loser)
	}
	if got, _ := st.GetUserPhone(ctx, keeper); got == nil || got.Verified != keeperVerified {
		t.Fatalf("keeper's phone: %+v, want verified=%v", got, keeperVerified)
	}
	var enc string
	if err := st.pool.QueryRow(ctx, `SELECT number_enc FROM user_phones WHERE user_id = $1`, keeper).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if pt, err := c.cB.DecryptBytes(enc, userPhoneAAD(keeper)); err != nil || string(pt) != phoneA {
		t.Fatalf("keeper's number not readable under B alone: %v", err)
	}
	if n := verificationRows(t, st, ctx, domain.PhoneSubjectUser, loser); n == 0 {
		t.Fatal("the loser's code rows were deleted; they count for the limits")
	}
	var stale int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM phone_verifications WHERE number_hash = $1`, c.hashA).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("code rows still under A's hash: %d %v", stale, err)
	}
	var active int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM phone_verifications
		WHERE subject_kind = 'user' AND subject_id = $1 AND expires_at > now()`, loser).Scan(&active); err != nil || active != 0 {
		t.Fatalf("the loser kept %d running codes (%v)", active, err)
	}
	line := strings.TrimSpace(c.logs.String())
	if strings.Count(line, "user_phone_released") != 1 {
		t.Fatalf("want one user_phone_released line, got %q", line)
	}
	for _, want := range []string{"user_id=" + loser, "kept_by_user_id=" + keeper, "cause=reencrypt"} {
		if !strings.Contains(line, want) {
			t.Fatalf("release line lacks %q: %s", want, line)
		}
	}
	assertNoPhoneA(t, line)
	c.witnesses.assertRotated(t, st, ctx, c.cB)
}

func TestReencryptSettlesARehashCollision(t *testing.T) {
	cases := []struct {
		name           string
		onNew, onOld   rowShape
		keeperIsOld    bool
		keeperVerified bool
	}{
		{"unverified yields to verified (the row being rehashed yields)",
			rowShape{verifiedAgo: "1 hour", updatedAgo: "1 hour"}, rowShape{updatedAgo: "2 hours"}, false, true},
		{"unverified yields to verified (the other row yields)",
			rowShape{updatedAgo: "2 hours"}, rowShape{verifiedAgo: "1 hour", updatedAgo: "1 hour"}, true, true},
		{"both verified: the one verified first keeps it",
			rowShape{verifiedAgo: "1 hour", updatedAgo: "3 hours"}, rowShape{verifiedAgo: "2 hours", updatedAgo: "1 hour"}, true, true},
		{"both verified: the one verified first keeps it, the other way round",
			rowShape{verifiedAgo: "2 hours", updatedAgo: "1 hour"}, rowShape{verifiedAgo: "1 hour", updatedAgo: "3 hours"}, false, true},
		{"both unverified: a running code keeps it",
			rowShape{updatedAgo: "3 hours"}, rowShape{activeCode: true, updatedAgo: "1 minute"}, true, false},
		{"both unverified, no running code: the one written first keeps it",
			rowShape{updatedAgo: "1 hour"}, rowShape{updatedAgo: "2 hours"}, true, false},
		{"both unverified: a code spent by five attempts is not running (P2-4)",
			rowShape{updatedAgo: "3 hours"}, rowShape{activeCode: true, exhaustedCode: true, updatedAgo: "1 minute"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newRehashCollision(t, tc.onNew, tc.onOld)
			keeper, loser := c.onNew, c.onOld
			if tc.keeperIsOld {
				keeper, loser = c.onOld, c.onNew
			}
			res, err := c.st.ReencryptSecrets(c.ctx)
			if err != nil {
				t.Fatalf("reencrypt: %v", err)
			}
			if res.PhoneNumbersReleased != 1 || res.PhoneRehashesSkipped != 0 {
				t.Fatalf("result %+v, want one release and no skip", res)
			}
			c.assertSettled(t, keeper, loser, tc.keeperVerified)

			// A second run is a quiet success: nothing left to settle.
			c.resetLogs()
			res, err = c.st.ReencryptSecrets(c.ctx)
			if err != nil {
				t.Fatalf("second reencrypt: %v", err)
			}
			if res.PhoneNumbersReleased != 0 || res.PhoneRehashesSkipped != 0 {
				t.Fatalf("second run result %+v, want nothing settled", res)
			}
			if strings.Contains(c.logs.String(), "user_phone") {
				t.Fatalf("the second run settled something again: %s", c.logs.String())
			}
			if hash, ok := c.rowOf(keeper); !ok || hash != c.hashB {
				t.Fatal("the second run moved the number")
			}
		})
	}
}

// A collision that cannot be settled now — here the other row is held by a transaction in flight —
// is logged by user ids, counted and skipped; the run still re-encrypts the monitor and TOTP secrets
// and succeeds. The next run, with the row free, settles it, and the one after has nothing to do.
func TestReencryptSkipsACollisionItCannotSettleNow(t *testing.T) {
	c := newRehashCollision(t,
		rowShape{verifiedAgo: "1 hour", updatedAgo: "1 hour"}, rowShape{updatedAgo: "2 hours"})
	st, ctx := c.st, c.ctx

	// KEY SHARE on the other row: the plain rewrite of that row (its hash does not change) goes
	// through, while settling the collision needs both rows FOR UPDATE and finds this one taken.
	holder, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM user_phones WHERE user_id = $1 FOR KEY SHARE`, c.onNew); err != nil {
		t.Fatal(err)
	}

	res, err := st.ReencryptSecrets(ctx)
	if err != nil {
		t.Fatalf("reencrypt with a busy row: %v", err)
	}
	if res.PhoneRehashesSkipped != 1 || res.PhoneNumbersReleased != 0 {
		t.Fatalf("result %+v, want one skip and no release", res)
	}
	c.witnesses.assertRotated(t, st, ctx, c.cB)
	if hash, ok := c.rowOf(c.onOld); !ok || hash != c.hashA {
		t.Fatal("the skipped row was changed")
	}
	if hash, ok := c.rowOf(c.onNew); !ok || hash != c.hashB {
		t.Fatal("the other row was changed")
	}
	line := strings.TrimSpace(c.logs.String())
	for _, want := range []string{"user_phone_rehash_skipped", "user_id=" + c.onOld, "holder_user_id=" + c.onNew, "reason=busy"} {
		if !strings.Contains(line, want) {
			t.Fatalf("skip line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "user_phone_released") {
		t.Fatalf("a skipped collision released a row: %s", line)
	}
	assertNoPhoneA(t, line)

	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	c.resetLogs()
	res, err = st.ReencryptSecrets(ctx)
	if err != nil || res.PhoneNumbersReleased != 1 || res.PhoneRehashesSkipped != 0 {
		t.Fatalf("second run: %+v %v; want the collision settled", res, err)
	}
	c.assertSettled(t, c.onNew, c.onOld, true)

	c.resetLogs()
	res, err = st.ReencryptSecrets(ctx)
	if err != nil || res.PhoneNumbersReleased != 0 || res.PhoneRehashesSkipped != 0 {
		t.Fatalf("third run: %+v %v; want nothing left", res, err)
	}
	if strings.Contains(c.logs.String(), "user_phone") {
		t.Fatalf("the third run settled something: %s", c.logs.String())
	}
}

// Channel numbers carry no unique constraint: two `number` channels may hold one number, and a
// rotation rehashing both cannot meet a 23505.
func TestChannelNumberHashIsNotUnique(t *testing.T) {
	st, ctx := outboxTestStore(t)
	var unique bool
	if err := st.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_index i
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		 WHERE i.indrelid = 'notification_channels'::regclass AND i.indisunique AND a.attname = 'number_hash')`).
		Scan(&unique); err != nil {
		t.Fatal(err)
	}
	if unique {
		t.Fatal("notification_channels.number_hash is in a unique index: the channel rehash can now meet a 23505 and needs a settle")
	}
}
