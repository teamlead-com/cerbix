package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// A person's own phone number (FR-041a §4.2, D35). Stored three ways: AEAD ciphertext bound to the
// user (R2) to send to, a keyed hash under the primary key to find and to keep it unique, and the
// mask to show. Every read below but VerifiedUserPhoneNumber is decrypt-free.

// userPhoneAAD binds a user's number ciphertext to that user (R2).
func userPhoneAAD(userID string) []byte { return secret.CanonicalAAD("user_phone", userID) }

// SetUserPhone stores number as the user's phone, UNVERIFIED, and issues a verification code for it,
// in one transaction; the caller sends the code after the commit. Replacing a number clears its
// verification and keeps the language. A re-PUT of the number the user has already VERIFIED is a
// no-op: the answer is the stored phone and an empty code. A re-PUT of the unverified number is a
// resend (a new code, R5).
//
// A number another user holds is matched under every configured key, so a rotation cannot open a
// hole. It is refused (domain.ErrPhoneNumberInUse) while the holder has verified it or holds a running
// code for it; otherwise the holder's row YIELDS (final review I-1): it is deleted in this transaction
// and the number changes hands. An unverified number whose code has run out is nobody's, and keeping
// it would let anyone block a person's own number for good by storing it first. The holder's code
// rows stay: they keep counting for the limits (R6).
//
// The person is the requester of the code: the hourly limit counts it with the codes they asked for
// their `number` channels (FR-041a review P2-3).
//
// Refusals: domain.ErrPhoneKeyMissing (D29), domain.ErrInvalidPhone, ErrNotFound (no such user),
// domain.ErrPhoneNotHuman (a machine user), domain.ErrPhoneSendLimited (R6) and
// domain.ErrPhoneNumberInUse. None writes anything but number_in_use, which spends one of the
// person's codes of the hour (recordPhoneRefusalTx, M-9): who holds a number cannot be asked
// without limit. A person whose hour is spent is told send_limited before the number is looked at.
func (s *Store) SetUserPhone(ctx context.Context, userID, number string) (domain.UserPhone, string, error) {
	normalized, hashes, err := s.phoneNumberHashes(number)
	if err != nil {
		return domain.UserPhone{}, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.UserPhone{}, "", fmt.Errorf("store: begin set user phone: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The subject's lock first (lockPhoneSubjectTx), then the user's row: NO KEY UPDATE leaves
	// foreign-key readers alone.
	if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectUser, userID); err != nil {
		return domain.UserPhone{}, "", err
	}
	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&kind)
	if noRows(err) {
		return domain.UserPhone{}, "", ErrNotFound
	}
	if err != nil {
		return domain.UserPhone{}, "", fmt.Errorf("store: lock user for phone: %w", err)
	}
	if kind != domain.UserKindHuman {
		return domain.UserPhone{}, "", domain.ErrPhoneNotHuman
	}

	// The user's own phone row next, locked BEFORE any holder's row (pre-review 3): two people
	// swapping unverified numbers each hold their own row first, so neither can lock the other's and
	// then wait for its own — the claim below takes a holder's row without waiting.
	var (
		sameNumber, verified bool
		current              domain.UserPhone
	)
	err = tx.QueryRow(ctx, `SELECT number_hash = ANY($2), verified_at IS NOT NULL, number_masked, language
		  FROM user_phones WHERE user_id = $1 FOR UPDATE`, userID, hashes).
		Scan(&sameNumber, &verified, &current.NumberMasked, &current.Language)
	if err != nil && !noRows(err) {
		return domain.UserPhone{}, "", fmt.Errorf("store: read user phone: %w", err)
	}
	if err == nil && sameNumber && verified {
		current.Verified = true
		return current, "", nil
	}

	spent, err := phoneRequesterBudgetSpentTx(ctx, tx, userID)
	if err != nil {
		return domain.UserPhone{}, "", err
	}
	if spent {
		return domain.UserPhone{}, "", domain.ErrPhoneSendLimited
	}
	// The number's lock (issuePhoneCodeTx's last lock, taken early, under every key) serializes every
	// claim of the number: no code can start for the holder while the holder's row is judged.
	if err := lockPhoneVerificationNumberTx(ctx, tx, hashes); err != nil {
		return domain.UserPhone{}, "", err
	}
	s.phoneClaimStepHook(userID, "before_claim")
	taken, released, err := s.claimUserPhoneNumberTx(ctx, tx, userID, hashes)
	if isDeadlockError(err) {
		return domain.UserPhone{}, "", domain.ErrPhoneNumberInUse
	}
	if err != nil {
		return domain.UserPhone{}, "", err
	}
	s.phoneClaimStepHook(userID, "claimed")
	if taken {
		if err := recordPhoneRefusalTx(ctx, tx, domain.PhoneSubjectUser, userID, userID); err != nil {
			return domain.UserPhone{}, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.UserPhone{}, "", fmt.Errorf("store: commit refused phone claim: %w", err)
		}
		return domain.UserPhone{}, "", domain.ErrPhoneNumberInUse
	}

	enc, err := s.cipher.EncryptBytes([]byte(normalized), userPhoneAAD(userID))
	if err != nil {
		return domain.UserPhone{}, "", fmt.Errorf("store: encrypt phone number: %w", err)
	}
	var phone domain.UserPhone
	err = tx.QueryRow(ctx, `INSERT INTO user_phones (user_id, number_enc, number_hash, number_masked, verified_at, updated_at)
		VALUES ($1, $2, $3, $4, NULL, now())
		ON CONFLICT (user_id) DO UPDATE SET number_enc = EXCLUDED.number_enc, number_hash = EXCLUDED.number_hash,
		    number_masked = EXCLUDED.number_masked, verified_at = NULL, updated_at = now()
		RETURNING number_masked, language`,
		userID, enc, hashes[0], domain.MaskPhone(normalized)).Scan(&phone.NumberMasked, &phone.Language)
	if isUniqueViolation(err) {
		// A row under the same hash that the claim above did not judge. Not expected under the
		// number's lock; the number was contested, and the answer is the one the claimant can act on.
		// Two processes on different primary keys do NOT meet here: the number's lock under every
		// key serializes them (exact counts, FR-041a review P2-5), but the one still on the old key
		// cannot see a row stored under the new key's hash and stores its own — the duplicate
		// `cerbix reencrypt` settles (pre-review 2, settleUserPhoneRehashTx).
		return domain.UserPhone{}, "", domain.ErrPhoneNumberInUse
	}
	if isDeadlockError(err) {
		// Not expected with the lock order above; should one still arise, the number was contested,
		// and the answer is the one the claimant could act on — not a 500.
		return domain.UserPhone{}, "", domain.ErrPhoneNumberInUse
	}
	if err != nil {
		return domain.UserPhone{}, "", fmt.Errorf("store: write user phone: %w", err)
	}
	code, err := s.issuePhoneCodeTx(ctx, tx, domain.PhoneSubjectUser, userID, userID, hashes)
	if err != nil {
		return domain.UserPhone{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.UserPhone{}, "", fmt.Errorf("store: commit set user phone: %w", err)
	}
	for _, holder := range released {
		s.logUserPhoneReleased(holder, "claim", "claimant_user_id", userID)
	}
	return phone, code, nil
}

// phoneClaimStepHook calls the test-only seam, when one is set.
func (s *Store) phoneClaimStepHook(userID, step string) {
	if s.phoneClaimStep != nil {
		s.phoneClaimStep(userID, step)
	}
}

// logUserPhoneReleased records that a user's number left them — the person is told nowhere else.
// The line carries user ids and the cause; never the number or its mask (P3).
func (s *Store) logUserPhoneReleased(userID, cause, otherKey, otherID string) {
	s.log().Warn("user_phone_released", "user_id", userID, "cause", cause, otherKey, otherID)
}

// claimUserPhoneNumberTx judges the rows of OTHER users that hold the number (one per key it was
// hashed under) and reports whether one of them keeps it: verified, or with a running code for it
// (phoneCodeRunningSQL: a code spent by five wrong attempts holds nothing, FR-041a review P2-4).
// When none keeps it, every one of them is deleted, and their user ids are returned for the caller
// to log after its commit. The caller holds the claimant's subject lock, its user row, its own
// user_phones row and the number's lock, in that order; the holders' user_phones rows are locked
// here, last, and WITHOUT waiting (pre-review 3): a holder's row another transaction holds is a
// number being written right now — its owner verifying it, or claiming another number while this
// one is claimed from it — and the answer is that the number is in use. Waiting there is what
// deadlocked two people swapping unverified numbers.
func (s *Store) claimUserPhoneNumberTx(ctx context.Context, tx pgx.Tx, userID string, hashes []string) (taken bool, released []string, err error) {
	sp, err := tx.Begin(ctx) // a savepoint: a busy holder's refusal must not abort the claimant's transaction
	if err != nil {
		return false, nil, fmt.Errorf("store: savepoint before number holders: %w", err)
	}
	rows, err := sp.Query(ctx, `SELECT user_id::text FROM user_phones
		 WHERE number_hash = ANY($2) AND user_id <> $1 ORDER BY user_id FOR UPDATE NOWAIT`, userID, hashes)
	var holders []string
	if err == nil {
		holders, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if isLockNotAvailable(err) {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return false, nil, fmt.Errorf("store: release busy number holders: %w", rerr)
		}
		return true, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("store: lock number holders: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("store: release savepoint of number holders: %w", err)
	}
	if len(holders) == 0 {
		return false, nil, nil
	}
	// A statement of its own, after the locks: under READ COMMITTED it sees a verification that
	// committed while this transaction waited for a holder's row.
	var keeps bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		    SELECT 1 FROM user_phones p
		     WHERE p.user_id = ANY($1::uuid[])
		       AND (p.verified_at IS NOT NULL OR EXISTS (
		            SELECT 1 FROM phone_verifications v
		             WHERE v.subject_kind = $2 AND v.subject_id = p.user_id
		               AND v.number_hash = ANY($3) AND `+phoneCodeRunningSQL("v")+`)))`,
		holders, domain.PhoneSubjectUser, hashes).Scan(&keeps); err != nil {
		return false, nil, fmt.Errorf("store: judge number holders: %w", err)
	}
	if keeps {
		return true, nil, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_phones WHERE user_id = ANY($1::uuid[])`, holders); err != nil {
		return false, nil, fmt.Errorf("store: release unverified number: %w", err)
	}
	return false, holders, nil
}

// SendUserPhoneCode issues a new code for the user's STORED unverified number and returns the number
// to send it to (§4.2): the API never returns a number, so after a reload the person has nothing to
// PUT again. The code is issued by issuePhoneCodeTx under exactly the limits of the first send (R6:
// 3 per requester per hour — the person, across their channels too — and 10 per number per day) and
// ends the code before it (R5). The caller sends
// it after the commit. Refusals: domain.ErrPhoneKeyMissing (D29), domain.ErrPhoneNoNumber,
// domain.ErrPhoneAlreadyVerified, domain.ErrPhoneSendLimited; nothing is written by any of them.
//
// The code row carries the hash the user's row holds — not necessarily the primary key's, between a
// key change and `cerbix reencrypt` — so the verification's comparison of the two (VerifyUserPhone)
// holds; the per-number count matches, and the number's lock covers, the number under every
// configured key and the stored hash either way (carried M-6).
func (s *Store) SendUserPhoneCode(ctx context.Context, userID string) (number, code string, err error) {
	if s.cipher == nil {
		return "", "", domain.ErrPhoneKeyMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: begin send user phone code: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The lock order of every phone write: the subject, the owning row, then the code rows.
	if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectUser, userID); err != nil {
		return "", "", err
	}
	var (
		enc, storedHash string
		verified        bool
	)
	err = tx.QueryRow(ctx, `SELECT number_enc, number_hash, verified_at IS NOT NULL
		  FROM user_phones WHERE user_id = $1 FOR NO KEY UPDATE`, userID).Scan(&enc, &storedHash, &verified)
	if noRows(err) {
		return "", "", domain.ErrPhoneNoNumber
	}
	if err != nil {
		return "", "", fmt.Errorf("store: lock user phone: %w", err)
	}
	if verified {
		return "", "", domain.ErrPhoneAlreadyVerified
	}
	pt, err := s.cipher.DecryptBytes(enc, userPhoneAAD(userID))
	if err != nil {
		return "", "", fmt.Errorf("store: decrypt user phone number: %w", err)
	}
	number = string(pt)
	wipe(pt)
	hashes, err := s.cipher.KeyedHashes(phoneNumberHashLabel, []byte(number))
	if err != nil {
		return "", "", fmt.Errorf("store: hash user phone number: %w", err)
	}
	ordered := []string{storedHash}
	for _, h := range hashes {
		if h != storedHash {
			ordered = append(ordered, h)
		}
	}
	if code, err = s.issuePhoneCodeTx(ctx, tx, domain.PhoneSubjectUser, userID, userID, ordered); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit send user phone code: %w", err)
	}
	return number, code, nil
}

// VerifyUserPhone judges code against the user's active code (checkPhoneCodeTx's verdicts) and, on a
// match, marks the number it was sent to verified. ErrNotFound without a stored number.
func (s *Store) VerifyUserPhone(ctx context.Context, userID, code string) (domain.UserPhone, error) {
	if s.cipher == nil {
		return domain.UserPhone{}, domain.ErrPhoneKeyMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.UserPhone{}, fmt.Errorf("store: begin verify user phone: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The lock order of every phone write: the subject, the owning row, then the code row (in
	// checkPhoneCodeTx). Verifying the other way round deadlocked against a concurrent resend.
	if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectUser, userID); err != nil {
		return domain.UserPhone{}, err
	}
	err = tx.QueryRow(ctx, `SELECT 1 FROM user_phones WHERE user_id = $1 FOR NO KEY UPDATE`, userID).Scan(new(int))
	if noRows(err) {
		return domain.UserPhone{}, ErrNotFound
	}
	if err != nil {
		return domain.UserPhone{}, fmt.Errorf("store: lock user phone: %w", err)
	}
	numberHash, err := s.checkPhoneCodeTx(ctx, tx, domain.PhoneSubjectUser, userID, code)
	if isPhoneCodeVerdict(err) {
		if cerr := tx.Commit(ctx); cerr != nil {
			return domain.UserPhone{}, fmt.Errorf("store: commit verification attempt: %w", cerr)
		}
		return domain.UserPhone{}, err
	}
	if err != nil {
		return domain.UserPhone{}, err
	}
	// The code verifies the number it was SENT to: every write of the number ends the codes before it,
	// so the hashes agree; the comparison keeps that true even if a future writer forgets to.
	phone, err := scanUserPhone(tx.QueryRow(ctx, `UPDATE user_phones SET verified_at = now(), updated_at = now()
		 WHERE user_id = $1 AND number_hash = $2
		RETURNING number_masked, verified_at IS NOT NULL, language`, userID, numberHash))
	if noRows(err) {
		return domain.UserPhone{}, domain.ErrPhoneCodeExpired
	}
	if err != nil {
		return domain.UserPhone{}, fmt.Errorf("store: mark user phone verified: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.UserPhone{}, fmt.Errorf("store: commit verify user phone: %w", err)
	}
	return phone, nil
}

// DeleteUserPhone removes the user's number and ends its active code. ErrNotFound without one.
func (s *Store) DeleteUserPhone(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete user phone: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectUser, userID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM user_phones WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("store: delete user phone: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := expirePhoneCodesTx(ctx, tx, domain.PhoneSubjectUser, userID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete user phone: %w", err)
	}
	return nil
}

// SetUserPhoneLanguage sets (or, with nil, clears) the language of the user's phone alerts. It never
// touches the number or its verification (§4.2). The caller validates the tag. ErrNotFound without a
// stored number.
func (s *Store) SetUserPhoneLanguage(ctx context.Context, userID string, language *string) (domain.UserPhone, error) {
	phone, err := scanUserPhone(s.pool.QueryRow(ctx, `UPDATE user_phones SET language = $2, updated_at = now()
		 WHERE user_id = $1 RETURNING number_masked, verified_at IS NOT NULL, language`, userID, language))
	if noRows(err) {
		return domain.UserPhone{}, ErrNotFound
	}
	if err != nil {
		return domain.UserPhone{}, fmt.Errorf("store: set user phone language: %w", err)
	}
	return phone, nil
}

// GetUserPhone returns the user's phone as GET /api/v1/me shows it, or nil without one.
func (s *Store) GetUserPhone(ctx context.Context, userID string) (*domain.UserPhone, error) {
	phone, err := scanUserPhone(s.pool.QueryRow(ctx,
		`SELECT number_masked, verified_at IS NOT NULL, language FROM user_phones WHERE user_id = $1`, userID))
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get user phone: %w", err)
	}
	return &phone, nil
}

// VerifiedUserPhoneNumber decrypts the user's number to send to it — only a VERIFIED one (P1).
// ErrNotFound without a verified number. The one read of this file that opens the ciphertext.
func (s *Store) VerifiedUserPhoneNumber(ctx context.Context, userID string) (string, error) {
	var enc string
	err := s.pool.QueryRow(ctx,
		`SELECT number_enc FROM user_phones WHERE user_id = $1 AND verified_at IS NOT NULL`, userID).Scan(&enc)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read user phone number: %w", err)
	}
	if s.cipher == nil {
		return "", domain.ErrPhoneKeyMissing
	}
	pt, err := s.cipher.DecryptBytes(enc, userPhoneAAD(userID))
	if err != nil {
		return "", fmt.Errorf("store: decrypt user phone number: %w", err)
	}
	return string(pt), nil
}

func scanUserPhone(row pgx.Row) (domain.UserPhone, error) {
	var p domain.UserPhone
	err := row.Scan(&p.NumberMasked, &p.Verified, &p.Language)
	return p, err
}

// reencryptUserPhones rewrites every user's number under the primary key and recomputes its hash
// (D35), with the old ciphertext as the CAS: a write that lands in between wins. The verification
// rows that carry the old hash follow it in the same transaction, so a code in flight still verifies
// and the per-number send limit keeps counting across the rotation.
//
// A recomputed hash another user's row already holds is a number two processes on different
// primary keys each accepted during a rolling rotation (pre-review 2). It is settled in the rehash's
// transaction (settleUserPhoneRehashTx); one that cannot be settled now is logged, counted in
// skipped and left for the next run, and the rotation goes on.
func (s *Store) reencryptUserPhones(ctx context.Context) (released, skipped int, err error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id::text, number_enc, number_hash FROM user_phones`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: scan user phones for reencrypt: %w", err)
	}
	type row struct{ userID, enc, hash string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.userID, &r.enc, &r.hash); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan user phone: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: iterate user phones: %w", err)
	}
	for _, r := range all {
		out, err := s.reencryptUserPhone(ctx, r.userID, r.enc, r.hash)
		if err != nil {
			return released, skipped, err
		}
		switch {
		case out.skipped:
			skipped++
			s.log().Warn("user_phone_rehash_skipped", "user_id", r.userID, "holder_user_id", out.holder,
				"reason", out.reason)
		case out.released != "":
			released++
			s.logUserPhoneReleased(out.released, "reencrypt", "kept_by_user_id", out.keptBy)
		}
	}
	return released, skipped, nil
}

func (s *Store) reencryptUserPhone(ctx context.Context, userID, oldEnc, oldHash string) (rehashOutcome, error) {
	return s.reencryptPhoneNumber(ctx, phoneNumberOwner{
		what: "user " + userID, aad: userPhoneAAD(userID), subjectKind: domain.PhoneSubjectUser,
		rewrite: `UPDATE user_phones SET number_enc = $2, number_hash = $3 WHERE user_id = $1 AND number_enc = $4`,
		settle:  s.settleUserPhoneRehashTx,
	}, userID, oldEnc, oldHash)
}

// phoneNumberOwner is a row that holds a number (a person's, a `number` channel's): the AAD its
// ciphertext is bound to, and the CAS rewrite of its ciphertext and hash ($1 id, $2 new ciphertext,
// $3 new hash, $4 old ciphertext).
type phoneNumberOwner struct {
	what string // for errors: "user <id>", "channel <id>" — never the number
	aad  []byte
	// subjectKind is the kind of the row's verification subject (the row's id is the subject id):
	// the rehash moves that subject's code rows, never another holder's (FR-041a review P2-5).
	subjectKind string
	rewrite     string
	// settle resolves a rewrite that hit a unique violation, inside the rewrite's transaction. Nil
	// where the hash is not unique: a `number` channel's number_hash has no unique constraint, so
	// its rewrite cannot meet one.
	settle func(ctx context.Context, tx pgx.Tx, r phoneRehash) (rehashOutcome, error)
}

// phoneRehash is one number's move to the primary key.
type phoneRehash struct {
	id, oldEnc, newEnc, oldHash, newHash string
	rewrite                              string
}

// rehashOutcome is what a rehash did beyond the rewrite. released names a user whose row was
// deleted to settle a collision, and keptBy the user who kept the number; skipped means nothing was
// written and holder/reason say why.
type rehashOutcome struct {
	released, keptBy string
	skipped          bool
	holder, reason   string
	// unchanged: a concurrent write replaced the number first; nothing to rehash.
	unchanged bool
}

// reencryptPhoneNumber rewrites one stored number under the primary key and recomputes its hash, with
// the old ciphertext as the CAS: a write that lands in between wins. The row's OWN verification rows
// carrying the old hash follow it in the same transaction, so a code in flight still verifies and the
// per-number send limit keeps counting across the rotation — also when a collision deleted the row.
//
// Another subject's rows under the old hash move only when that subject does not hold the number
// under the old hash itself (FR-041a review P2-5): a person whose row is not rehashed yet — skipped
// as busy, or written by a process still on the old key after the scan — keeps a code that matches
// their row, and VerifyUserPhone compares the two exactly. The rows that do move are a deleted
// channel's, a released person's and a subject's that has since changed its number — each of those
// paths ends the codes — and a live number channel's own, which can be a running code under the old
// hash. Moving that one is harmless: channel verification matches under every key. Moving the rest
// only keeps the number's daily count.
func (s *Store) reencryptPhoneNumber(ctx context.Context, owner phoneNumberOwner, id, oldEnc, oldHash string) (rehashOutcome, error) {
	pt, err := s.cipher.DecryptBytes(oldEnc, owner.aad)
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("store: decrypt phone number of %s: %w", owner.what, err)
	}
	defer wipe(pt)
	enc, err := s.cipher.EncryptBytes(pt, owner.aad)
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("store: encrypt phone number of %s: %w", owner.what, err)
	}
	hash, err := s.cipher.KeyedHash(phoneNumberHashLabel, pt)
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("store: hash phone number of %s: %w", owner.what, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("store: begin reencrypt phone number: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A savepoint around the rewrite: a unique violation must leave the transaction usable to settle it.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("store: savepoint before rewrite of %s: %w", owner.what, err)
	}
	var out rehashOutcome
	tag, err := sp.Exec(ctx, owner.rewrite, id, enc, hash, oldEnc)
	switch {
	case isUniqueViolation(err) && owner.settle != nil:
		if rerr := sp.Rollback(ctx); rerr != nil {
			return rehashOutcome{}, fmt.Errorf("store: roll back rewrite of %s: %w", owner.what, rerr)
		}
		out, err = owner.settle(ctx, tx, phoneRehash{
			id: id, oldEnc: oldEnc, newEnc: enc, oldHash: oldHash, newHash: hash, rewrite: owner.rewrite,
		})
		if err != nil {
			return rehashOutcome{}, fmt.Errorf("store: settle rehash of %s: %w", owner.what, err)
		}
		if out.skipped || out.unchanged {
			return out, nil // the transaction rolls back: nothing of this number is written
		}
	case err != nil:
		return rehashOutcome{}, fmt.Errorf("store: rewrite phone number of %s: %w", owner.what, err)
	default:
		if err := sp.Commit(ctx); err != nil {
			return rehashOutcome{}, fmt.Errorf("store: release rewrite savepoint of %s: %w", owner.what, err)
		}
		if tag.RowsAffected() == 0 {
			return rehashOutcome{unchanged: true}, nil // a concurrent write replaced the number; it is already under the primary
		}
	}
	if hash != oldHash {
		if _, err := tx.Exec(ctx, `UPDATE phone_verifications v SET number_hash = $2
			 WHERE v.number_hash = $1
			   AND ((v.subject_kind = $3 AND v.subject_id = $4::uuid)
			        OR NOT (EXISTS (SELECT 1 FROM user_phones p
			                         WHERE v.subject_kind = 'user' AND p.user_id = v.subject_id AND p.number_hash = $1)
			             OR EXISTS (SELECT 1 FROM notification_channels c
			                         WHERE v.subject_kind = 'channel' AND c.id = v.subject_id AND c.number_hash = $1)))`,
			oldHash, hash, owner.subjectKind, id); err != nil {
			return rehashOutcome{}, fmt.Errorf("store: rehash verification codes of %s: %w", owner.what, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return rehashOutcome{}, fmt.Errorf("store: commit reencrypt phone number: %w", err)
	}
	return out, nil
}

// userPhoneHolder is one side of a rehash collision, as the yield rule judges it.
type userPhoneHolder struct {
	userID, enc, hash string
	verifiedAt        *time.Time
	updatedAt         time.Time
	runningCode       bool
}

// keepsNumberOver reports whether a keeps a number both a and b hold, by I-1's yield rule as the
// controller ruled it for a rotation (pre-review 2): a verified row over an unverified one; of two
// verified rows, the one verified first; of two unverified rows, the one with a running code (a code
// spent by five wrong attempts is not one, FR-041a review P2-4). Every
// tie left falls to the row written first, then to the smaller user id, so the answer is total.
func keepsNumberOver(a, b userPhoneHolder) bool {
	aVerified, bVerified := a.verifiedAt != nil, b.verifiedAt != nil
	switch {
	case aVerified != bVerified:
		return aVerified
	case aVerified && !a.verifiedAt.Equal(*b.verifiedAt):
		return a.verifiedAt.Before(*b.verifiedAt)
	case !aVerified && a.runningCode != b.runningCode:
		return a.runningCode
	case !a.updatedAt.Equal(b.updatedAt):
		return a.updatedAt.Before(b.updatedAt)
	default:
		return a.userID < b.userID
	}
}

// settleUserPhoneRehashTx settles a rehash whose new hash another user's row already holds. It
// takes both subjects' locks (in user-id order, so two settlements cannot deadlock), then both rows
// WITHOUT waiting, and deletes the row the yield rule says loses; the loser's running codes end and
// its code rows stay, as on DeleteUserPhone. When the rehashed row wins, its rewrite then runs.
// Outcomes that write nothing: unchanged (the rehashed row was rewritten in between — the CAS lost),
// skipped "busy" (a row is being written right now) and skipped "holder_changed" (the other row moved
// or went away in between). The next run settles a skipped row.
func (s *Store) settleUserPhoneRehashTx(ctx context.Context, tx pgx.Tx, r phoneRehash) (rehashOutcome, error) {
	var other string
	err := tx.QueryRow(ctx, `SELECT user_id::text FROM user_phones WHERE number_hash = $1 AND user_id <> $2`,
		r.newHash, r.id).Scan(&other)
	if noRows(err) {
		return rehashOutcome{skipped: true, reason: "holder_changed"}, nil
	}
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("find the other holder: %w", err)
	}
	first, second := r.id, other
	if second < first {
		first, second = second, first
	}
	for _, u := range []string{first, second} {
		if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectUser, u); err != nil {
			return rehashOutcome{}, err
		}
	}
	busy := rehashOutcome{skipped: true, holder: other, reason: "busy"}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM user_phones WHERE user_id = ANY($1::uuid[]) ORDER BY user_id FOR UPDATE NOWAIT`,
		[]string{first, second}); isLockNotAvailable(err) || isDeadlockError(err) {
		return busy, nil
	} else if err != nil {
		return rehashOutcome{}, fmt.Errorf("lock both holders: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT p.user_id::text, p.number_enc, p.number_hash, p.verified_at, p.updated_at,
		       EXISTS (SELECT 1 FROM phone_verifications v
		                WHERE v.subject_kind = $2 AND v.subject_id = p.user_id
		                  AND v.number_hash = ANY($3) AND `+phoneCodeRunningSQL("v")+`)
		  FROM user_phones p WHERE p.user_id = ANY($1::uuid[])`,
		[]string{first, second}, domain.PhoneSubjectUser, []string{r.oldHash, r.newHash})
	if err != nil {
		return rehashOutcome{}, fmt.Errorf("read both holders: %w", err)
	}
	held := map[string]userPhoneHolder{}
	for rows.Next() {
		var h userPhoneHolder
		if err := rows.Scan(&h.userID, &h.enc, &h.hash, &h.verifiedAt, &h.updatedAt, &h.runningCode); err != nil {
			rows.Close()
			return rehashOutcome{}, fmt.Errorf("read both holders: %w", err)
		}
		held[h.userID] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rehashOutcome{}, fmt.Errorf("read both holders: %w", err)
	}
	mine, ok := held[r.id]
	if !ok || mine.enc != r.oldEnc {
		return rehashOutcome{unchanged: true}, nil
	}
	theirs, ok := held[other]
	if !ok || theirs.hash != r.newHash {
		return rehashOutcome{skipped: true, holder: other, reason: "holder_changed"}, nil
	}

	keeper, loser := mine, theirs
	if !keepsNumberOver(mine, theirs) {
		keeper, loser = theirs, mine
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_phones WHERE user_id = $1`, loser.userID); err != nil {
		return rehashOutcome{}, fmt.Errorf("release the yielding row: %w", err)
	}
	if err := expirePhoneCodesTx(ctx, tx, domain.PhoneSubjectUser, loser.userID); err != nil {
		return rehashOutcome{}, err
	}
	if keeper.userID == r.id {
		if _, err := tx.Exec(ctx, r.rewrite, r.id, r.newEnc, r.newHash, r.oldEnc); err != nil {
			return rehashOutcome{}, fmt.Errorf("rewrite after the release: %w", err)
		}
	}
	return rehashOutcome{released: loser.userID, keptBy: keeper.userID}, nil
}
