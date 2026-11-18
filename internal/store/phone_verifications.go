package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Verification codes of a phone number (§4.2, R5, R6), shared by a person's own number (subject
// `user`) and a `number`-mode channel (subject `channel`, FR-041a Task 8). The subject's write — a
// number stored or replaced — and the code it is sent run in ONE transaction, so a refused send
// (the limits) leaves the stored number as it was. The code itself is sent by the caller after the
// commit; the store never talks to a provider.

// Keyed-hash labels (R2, R5). The labels keep a number's hash and a code's hash in separate domains
// under the same key.
const (
	phoneNumberHashLabel = "phone-number"
	phoneCodeHashLabel   = "phone-verification"
)

// newPhoneCode draws a six-digit code from crypto/rand, uniformly over 000000..999999. A variable so
// the package's tests can script codes.
var newPhoneCode = func() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("store: draw verification code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// phoneNumberHashes normalizes number and returns its keyed hash under the primary key (for a write)
// and under every configured key, primary first (for a lookup, D35). ErrPhoneKeyMissing without a
// key (D29); domain.ErrInvalidPhone for input that is not E.164 — neither error carries the input.
func (s *Store) phoneNumberHashes(number string) (normalized string, hashes []string, err error) {
	if s.cipher == nil {
		return "", nil, domain.ErrPhoneKeyMissing
	}
	normalized, err = domain.NormalizeE164(number)
	if err != nil {
		return "", nil, err
	}
	hashes, err = s.cipher.KeyedHashes(phoneNumberHashLabel, []byte(normalized))
	if err != nil {
		return "", nil, fmt.Errorf("store: hash phone number: %w", err)
	}
	return normalized, hashes, nil
}

// issuePhoneCodeTx issues a code for the subject's number inside the caller's transaction: it
// enforces R6 (3 codes per REQUESTER per rolling hour, across every number the requester verifies;
// 10 per number per rolling 24 h across every requester, the number matched under every configured
// key), ends the subject's active code (R5 — the row is kept, it counts for the limits), and stores
// the new code's hash with a ten-minute life. requester is the principal who asks for the code (a
// person's user id, an API token's "apitoken:<id>"; FR-041a review P2-3). numberHashes is the
// number's hash under every key the caller matches it by: [0] is stored, all of them are counted
// and locked.
//
// Three advisory locks, always in this order, make the counts exact under concurrency: the
// subject's, the requester's, then the number's (every hash, sorted). ErrPhoneSendLimited aborts
// nothing by itself; the caller rolls back.
func (s *Store) issuePhoneCodeTx(ctx context.Context, tx pgx.Tx, subjectKind, subjectID, requester string, numberHashes []string) (string, error) {
	if err := lockPhoneSubjectTx(ctx, tx, subjectKind, subjectID); err != nil {
		return "", err
	}
	spent, err := phoneRequesterBudgetSpentTx(ctx, tx, requester)
	if err != nil {
		return "", err
	}
	if err := lockPhoneVerificationNumberTx(ctx, tx, numberHashes); err != nil {
		return "", err
	}
	var perNumber int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM phone_verifications
		      WHERE number_hash = ANY($1) AND created_at > now() - interval '24 hours'`,
		numberHashes).Scan(&perNumber); err != nil {
		return "", fmt.Errorf("store: count verification codes: %w", err)
	}
	if spent || perNumber >= domain.PhoneCodesPerNumberPerDay {
		return "", domain.ErrPhoneSendLimited
	}
	if err := expirePhoneCodesTx(ctx, tx, subjectKind, subjectID); err != nil {
		return "", err
	}
	code, err := newPhoneCode()
	if err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return "", fmt.Errorf("store: verification id: %w", err)
	}
	codeHash, err := s.cipher.KeyedHash(phoneCodeHashLabel, []byte(id+":"+code))
	if err != nil {
		return "", fmt.Errorf("store: hash verification code: %w", err)
	}
	// clock_timestamp, not now(): now() is when the transaction BEGAN, and a send that waited on the
	// subject's lock behind another must still be the later row — checkPhoneCodeTx reads the latest.
	if _, err := tx.Exec(ctx, `INSERT INTO phone_verifications
		    (id, subject_kind, subject_id, requested_by, number_hash, code_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp(), clock_timestamp() + make_interval(secs => $7))`,
		id, subjectKind, subjectID, requester, numberHashes[0], codeHash, domain.PhoneCodeTTL.Seconds()); err != nil {
		return "", fmt.Errorf("store: insert verification code: %w", err)
	}
	return code, nil
}

// lockPhoneSubjectTx takes the subject's advisory lock for the transaction. It is the FIRST lock of
// every write of a subject's number or codes — set, verify, delete, a channel's update — and the
// owning row (user_phones, notification_channels) is locked after it, the verification rows last.
// One order on every path is what keeps a resend racing a correct code from deadlocking (Task 7
// review, Minor 1). Re-taking it in the same transaction (issuePhoneCodeTx) is free.
func lockPhoneSubjectTx(ctx context.Context, tx pgx.Tx, subjectKind, subjectID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_verification_subject'), hashtext($1 || ':' || $2))`,
		subjectKind, subjectID); err != nil {
		return fmt.Errorf("store: lock verification subject: %w", err)
	}
	return nil
}

// lockPhoneRequesterTx takes the requester's advisory lock for the transaction: the hourly budget is
// the requester's across subjects, so two sends by one requester for two different subjects must
// not count it side by side. It comes after the subject's lock and the owning row, before the
// number's. Re-taking it is free.
func lockPhoneRequesterTx(ctx context.Context, tx pgx.Tx, requester string) error {
	if requester == "" {
		return fmt.Errorf("store: a verification code needs the principal who asks for it")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_verification_requester'), hashtext($1))`,
		requester); err != nil {
		return fmt.Errorf("store: lock verification requester: %w", err)
	}
	return nil
}

// lockPhoneVerificationNumberTx takes the number's advisory lock for the transaction under EVERY hash
// in numberHashes, in sorted order (FR-041a review P2-5, carried M-6). A process restarted on a new
// primary key and one still on the old key share the old key's hash, so they exclude each other
// during a rotation, as the storm guard's lockPhoneNumberTx does; a lock on one key's hash let them
// count a number's codes, and claim it, side by side. It comes AFTER the subject's lock, the owning
// row and the requester's lock: a claim of a number (SetUserPhone) and every code sent to it are
// serialized on it. Re-taking it with the same hashes is free; a caller passes one set per
// transaction, so the sorted order holds across all of them.
func lockPhoneVerificationNumberTx(ctx context.Context, tx pgx.Tx, numberHashes []string) error {
	sorted := append([]string(nil), numberHashes...)
	sort.Strings(sorted)
	for _, h := range sorted {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_verification_number'), hashtext($1))`, h); err != nil {
			return fmt.Errorf("store: lock verification number: %w", err)
		}
	}
	return nil
}

// phoneRefusalMark is the number_hash and the code_hash of a row that records a refused claim of a
// number (number_in_use, final review M-9) against the requester's hourly budget. It names no number
// — nothing was sent, so it never counts for a number's daily limit — and no code: a keyed hash is
// never empty, so it never matches a lookup, and checkPhoneCodeTx never judges it.
const phoneRefusalMark = ""

// phoneRequesterBudgetSpentTx takes the requester's lock and reports whether the requester has used
// its codes of the rolling hour (R6), across every subject it asked codes for, refusals recorded by
// recordPhoneRefusalTx included (FR-041a review P2-3).
func phoneRequesterBudgetSpentTx(ctx context.Context, tx pgx.Tx, requester string) (bool, error) {
	if err := lockPhoneRequesterTx(ctx, tx, requester); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM phone_verifications
		  WHERE requested_by = $1 AND created_at > now() - interval '1 hour'`, requester).Scan(&n); err != nil {
		return false, fmt.Errorf("store: count verification codes: %w", err)
	}
	return n >= domain.PhoneCodesPerRequesterPerHour, nil
}

// recordPhoneRefusalTx spends one of the requester's codes of the hour on a refused claim, without a
// number or a code (phoneRefusalMark). The row is born expired: it ends no running code and is purged
// with the code rows. The caller holds the requester's lock (phoneRequesterBudgetSpentTx).
func recordPhoneRefusalTx(ctx context.Context, tx pgx.Tx, subjectKind, subjectID, requester string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO phone_verifications
		    (subject_kind, subject_id, requested_by, number_hash, code_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $4, clock_timestamp(), clock_timestamp())`,
		subjectKind, subjectID, requester, phoneRefusalMark); err != nil {
		return fmt.Errorf("store: record refused claim: %w", err)
	}
	return nil
}

// phoneCodeRunningSQL is "the code row v is a running code" (§4.2): neither expired nor used up by
// five wrong attempts (FR-041a review P2-4). A code spent by its attempts keeps its expires_at, so the
// expiry alone would let it hold a number for the rest of its ten minutes. Every reader of "a running
// code" — the yield rule of a claim (I-1) and the rehash's settlement in `cerbix reencrypt` — uses it.
func phoneCodeRunningSQL(v string) string {
	return fmt.Sprintf("(%[1]s.expires_at > now() AND %[1]s.attempts < %[2]d)", v, domain.PhoneCodeMaxAttempts)
}

// expirePhoneCodesTx ends every active code of the subject (a new send, a removed number).
func expirePhoneCodesTx(ctx context.Context, tx pgx.Tx, subjectKind, subjectID string) error {
	if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET expires_at = now()
		WHERE subject_kind = $1 AND subject_id = $2 AND expires_at > now()`, subjectKind, subjectID); err != nil {
		return fmt.Errorf("store: expire verification codes: %w", err)
	}
	return nil
}

// checkPhoneCodeTx judges code against the subject's latest code, locked for the transaction, and
// returns the number hash the code was sent for. The verdicts, in order:
//
//	no code ever, or the latest one used or expired   ErrPhoneCodeExpired
//	five incorrect attempts on it                      ErrPhoneCodeAttemptsExhausted
//	a different code                                   ErrPhoneCodeIncorrect (attempt counted)
//
// Only the LATEST code is judged. A code that a newer send superseded is therefore "a different
// code": ErrPhoneCodeIncorrect, and the attempt is counted against the newer code — not
// ErrPhoneCodeExpired. The superseded row is never looked at again. A refusal recorded against the
// budget (phoneRefusalMark) is not a code and is skipped.
//
// A match spends the code. ErrPhoneCodeIncorrect has WRITTEN the attempt: the caller commits on it
// (isPhoneCodeVerdict) — rolling back would give a guesser unlimited attempts.
func (s *Store) checkPhoneCodeTx(ctx context.Context, tx pgx.Tx, subjectKind, subjectID, code string) (string, error) {
	var (
		id, numberHash, codeHash string
		attempts                 int
		active                   bool
	)
	err := tx.QueryRow(ctx, `SELECT id::text, number_hash, code_hash, attempts, expires_at > now()
		  FROM phone_verifications WHERE subject_kind = $1 AND subject_id = $2 AND code_hash <> $3
		 ORDER BY created_at DESC, id DESC LIMIT 1 FOR UPDATE`, subjectKind, subjectID, phoneRefusalMark).
		Scan(&id, &numberHash, &codeHash, &attempts, &active)
	if noRows(err) {
		return "", domain.ErrPhoneCodeExpired
	}
	if err != nil {
		return "", fmt.Errorf("store: read verification code: %w", err)
	}
	if attempts >= domain.PhoneCodeMaxAttempts {
		return "", domain.ErrPhoneCodeAttemptsExhausted
	}
	if !active {
		return "", domain.ErrPhoneCodeExpired
	}
	// The code was hashed under the primary key of its day; a rotation since then keeps it matching
	// while the old key is configured.
	candidates, err := s.cipher.KeyedHashes(phoneCodeHashLabel, []byte(id+":"+code))
	if err != nil {
		return "", fmt.Errorf("store: hash verification code: %w", err)
	}
	match := 0
	for _, c := range candidates {
		match |= subtle.ConstantTimeCompare([]byte(c), []byte(codeHash))
	}
	if match != 1 {
		if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return "", fmt.Errorf("store: count verification attempt: %w", err)
		}
		return "", domain.ErrPhoneCodeIncorrect
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_verifications SET expires_at = now() WHERE id = $1`, id); err != nil {
		return "", fmt.Errorf("store: spend verification code: %w", err)
	}
	return numberHash, nil
}

// isPhoneCodeVerdict reports whether err is checkPhoneCodeTx's answer about the code rather than a
// failure: the transaction is committed on it.
func isPhoneCodeVerdict(err error) bool {
	return errors.Is(err, domain.ErrPhoneCodeIncorrect) || errors.Is(err, domain.ErrPhoneCodeExpired) ||
		errors.Is(err, domain.ErrPhoneCodeAttemptsExhausted)
}
