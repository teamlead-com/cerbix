package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// Phone channels (FR-041a §4.2, §4.3): the writes of the `sms` and `voice` types. They differ from
// the four other types' writers in four ways, each the reason they are separate:
//
//   - the gates are read in the write's transaction: telephony enabled (D27), no voice under a
//     provider without calls (D3), the at-rest key (D29), and the recipient's eligibility (§4.3);
//   - a `number` channel's number is stored three ways (R2, D35) and is verified before the channel
//     can be enabled; a number change disables it again in the same statement;
//   - every write is audited in its own transaction (§9): `notification_channel.create|update|
//     delete|verify`, target `channel <id> · <type>`, never a number;
//   - the caller's D28 decision runs against the row as LOCKED (authorize), so an edit is judged
//     against what it edits, not against a read that may have gone stale.
//
// Lock order, shared with a person's own number (user_phones.go): the verification subject's
// advisory lock, then the owning row, then the verification rows.

// channelNumberAAD binds a channel's number ciphertext to the channel (R2).
func channelNumberAAD(channelID string) []byte {
	return secret.CanonicalAAD("channel_number", channelID)
}

// phoneChannelAuditTarget is the audit target of a phone channel's writes (§9): never a number.
func phoneChannelAuditTarget(id string, typ domain.ChannelType) string {
	return "channel " + id + " · " + string(typ)
}

// Audit actions of the phone-channel writes (§9).
const (
	auditPhoneChannelCreate = "notification_channel.create"
	auditPhoneChannelUpdate = "notification_channel.update"
	auditPhoneChannelDelete = "notification_channel.delete"
	auditPhoneChannelVerify = "notification_channel.verify"
)

// PhoneChannelPatch is the editable part of a phone channel; nil leaves a field as stored. The
// recipient MODE is not here: it is fixed at creation.
type PhoneChannelPatch struct {
	Name *string
	// RecipientUserID retargets a `user` channel (domain.ErrPhoneRecipientFixed on a `number` one).
	RecipientUserID *string
	// Number replaces a `number` channel's number (domain.ErrPhoneRecipientFixed on a `user` one): the
	// verification is cleared and the channel disabled, in the same statement (§4.2).
	Number  *string
	Enabled *bool
}

// pureDisable reports whether the patch only switches the channel off — the one edit that is never
// refused for the channel's recipient: pausing must always be possible.
func (p PhoneChannelPatch) pureDisable() bool {
	return p.Name == nil && p.RecipientUserID == nil && p.Number == nil && p.Enabled != nil && !*p.Enabled
}

// telephonyGateTx reads the typed telephony columns under a share lock, so a settings save (which
// takes the row FOR UPDATE) cannot switch telephony off between this check and the commit.
// canPlaceCalls is D3's one condition, telephony_can_place_calls (true when nothing is stored).
func telephonyGateTx(ctx context.Context, tx pgx.Tx) (enabled, canPlaceCalls bool, err error) {
	err = tx.QueryRow(ctx, `SELECT telephony_enabled, telephony_can_place_calls
		  FROM instance_settings WHERE id = true FOR SHARE`).Scan(&enabled, &canPlaceCalls)
	if noRows(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: read telephony gate: %w", err)
	}
	return enabled, canPlaceCalls, nil
}

// checkTelephonyInEffect refuses a phone channel of type typ that telephony would not phone: off
// (D27), or a voice channel under a provider that cannot place calls (D3).
func checkTelephonyInEffect(ctx context.Context, tx pgx.Tx, typ domain.ChannelType) error {
	enabled, canPlaceCalls, err := telephonyGateTx(ctx, tx)
	if err != nil {
		return err
	}
	if !enabled {
		return domain.ErrPhoneTelephonyDisabled
	}
	if typ == domain.ChannelVoice && !canPlaceCalls {
		return domain.ErrPhoneVoiceUnsupported
	}
	return nil
}

// recipientEligibleTx is §4.3's eligibility of userID for projectID — the fragment the route
// predicate embeds (phoneroute.go), evaluated for a row that does not name the user yet.
//
// It locks the user FOR KEY SHARE first, so the write that goes on to name them serializes with
// DeleteUser (which locks the user FOR UPDATE): either the deletion waits for this write to commit
// and then disables the channel, or this lock waits for the deletion and finds no user.
func recipientEligibleTx(ctx context.Context, tx pgx.Tx, projectID, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return domain.ErrPhoneRecipientNotEligible
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1::uuid FOR KEY SHARE`, userID).Scan(&exists)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.ErrPhoneRecipientNotEligible
	}
	if err != nil {
		return fmt.Errorf("store: lock recipient: %w", err)
	}
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT `+phoneRecipientEligibleSQL("x", "$2::uuid")+`
		  FROM (SELECT $1::uuid AS project_id) x`, projectID, userID).Scan(&eligible)
	if isInvalidTextRepresentation(err) {
		return domain.ErrPhoneRecipientNotEligible
	}
	if err != nil {
		return fmt.Errorf("store: recipient eligibility: %w", err)
	}
	if !eligible {
		return domain.ErrPhoneRecipientNotEligible
	}
	return nil
}

// isRecipientUserFKViolation reports a channel naming a user who no longer exists (23503 on
// recipient_user_id): the recipient is not eligible, not a server error.
// Defence in depth only: recipientEligibleTx's KEY SHARE on the user already serializes the writers
// with DeleteUser, so the user cannot vanish under the write.
func isRecipientUserFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" &&
		pgErr.ConstraintName == "notification_channels_recipient_user_id_fkey"
}

// lockPhoneChannelTx takes a phone channel's locks in the shared order — the verification subject,
// then the row — and returns the row as locked, with the hash of its number ("" in `user` mode).
// ErrNotFound for no channel; an error for a channel of another type (the phone writers only).
func (s *Store) lockPhoneChannelTx(ctx context.Context, tx pgx.Tx, id string) (domain.NotificationChannel, string, error) {
	if err := lockPhoneSubjectTx(ctx, tx, domain.PhoneSubjectChannel, id); err != nil {
		return domain.NotificationChannel{}, "", err
	}
	var hash *string
	err := tx.QueryRow(ctx, `SELECT number_hash FROM notification_channels WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&hash)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.NotificationChannel{}, "", ErrNotFound
	}
	if err != nil {
		return domain.NotificationChannel{}, "", fmt.Errorf("store: lock channel: %w", err)
	}
	ch, err := s.readChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, "", err
	}
	if !domain.IsPhoneChannelType(ch.Type) {
		return domain.NotificationChannel{}, "", fmt.Errorf("store: channel %s is not a phone channel", id)
	}
	return ch, deref(hash), nil
}

func (s *Store) readChannelTx(ctx context.Context, tx pgx.Tx, id string) (domain.NotificationChannel, error) {
	ch, err := s.scanChannel(tx.QueryRow(ctx, `SELECT `+channelColumns("c")+` FROM notification_channels c WHERE c.id = $1`, id))
	if noRows(err) {
		return domain.NotificationChannel{}, ErrNotFound
	}
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: read channel: %w", err)
	}
	return ch, nil
}

// CreatePhoneChannel creates an `sms` or `voice` channel and audits it, in one transaction. A
// `user` channel names RecipientUserID, an eligible recipient of the project, and carries no number;
// a `number` channel carries number and is created DISABLED with disabled_reason number_unverified,
// whatever ch.Enabled says (§4.2). The caller has applied D28.
//
// Refusals, none of which writes anything: domain.ErrPhoneKeyMissing (D29), domain.ErrInvalidPhone,
// domain.ErrPhoneRecipientFixed (a number on a `user` channel or a recipient on a `number` one),
// domain.ErrPhoneTelephonyDisabled (D27), domain.ErrPhoneVoiceUnsupported (D3),
// domain.ErrPhoneRecipientNotEligible (no recipient, or one who does not reach the project).
func (s *Store) CreatePhoneChannel(ctx context.Context, ch domain.NotificationChannel, number string, actor AuditActor) (domain.NotificationChannel, error) {
	if !domain.IsPhoneChannelType(ch.Type) {
		return domain.NotificationChannel{}, fmt.Errorf("store: create phone channel: type %q is not sms or voice", ch.Type)
	}
	ch.Name = strings.TrimSpace(ch.Name)
	if err := ch.Validate(); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: invalid channel: %w", err)
	}
	if err := actor.valid(); err != nil {
		return domain.NotificationChannel{}, err
	}
	if s.cipher == nil {
		return domain.NotificationChannel{}, domain.ErrPhoneKeyMissing
	}
	var (
		normalized string
		hashes     []string
	)
	switch ch.Recipient {
	case domain.PhoneRecipientUser:
		if number != "" {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
	case domain.PhoneRecipientNumber:
		if ch.RecipientUserID != "" {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		var err error
		if normalized, hashes, err = s.phoneNumberHashes(number); err != nil {
			return domain.NotificationChannel{}, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: begin create phone channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkTelephonyInEffect(ctx, tx, ch.Type); err != nil {
		return domain.NotificationChannel{}, err
	}
	// The id exists before the ciphertext: the number is bound to it (R2).
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: channel id: %w", err)
	}
	if ch.Recipient == domain.PhoneRecipientUser {
		if err := recipientEligibleTx(ctx, tx, ch.ProjectID, ch.RecipientUserID); err != nil {
			return domain.NotificationChannel{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO notification_channels
			    (id, project_id, type, name, config, enabled, recipient, recipient_user_id)
			VALUES ($1, $2, $3, $4, '{}', $5, 'user', $6)`,
			id, ch.ProjectID, ch.Type, ch.Name, ch.Enabled, ch.RecipientUserID)
	} else {
		enc, eerr := s.cipher.EncryptBytes([]byte(normalized), channelNumberAAD(id))
		if eerr != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: encrypt channel number: %w", eerr)
		}
		_, err = tx.Exec(ctx, `INSERT INTO notification_channels
			    (id, project_id, type, name, config, enabled, recipient, number_enc, number_hash, number_masked, disabled_reason)
			VALUES ($1, $2, $3, $4, '{}', false, 'number', $5, $6, $7, 'number_unverified')`,
			id, ch.ProjectID, ch.Type, ch.Name, enc, hashes[0], domain.MaskPhone(normalized))
	}
	if isRecipientUserFKViolation(err) {
		return domain.NotificationChannel{}, domain.ErrPhoneRecipientNotEligible
	}
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: create phone channel: %w", err)
	}
	if err := insertProjectAudit(ctx, tx, ch.ProjectID, actor, auditPhoneChannelCreate, phoneChannelAuditTarget(id, ch.Type)); err != nil {
		return domain.NotificationChannel{}, err
	}
	created, err := s.readChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: commit create phone channel: %w", err)
	}
	return created, nil
}

// UpdatePhoneChannel applies patch to a phone channel and audits it, in one transaction. authorize,
// when not nil, is the caller's D28 decision about the row as locked; its error is returned as is
// and nothing is written.
//
//   - Every update of a `user` channel but a pure pause needs an eligible recipient (§4.3) — the one
//     it keeps or the one it is retargeted to. A retarget clears disabled_reason recipient_deleted.
//   - A different number (matched under every key, so a respelling is no change) clears the
//     verification, disables the channel, sets number_unverified and ends the codes in flight.
//   - Switching a channel ON is refused while telephony would not phone it (D27, D3) and, for a
//     `number` channel, while its number is unverified; it clears disabled_reason.
func (s *Store) UpdatePhoneChannel(ctx context.Context, id string, patch PhoneChannelPatch, actor AuditActor,
	authorize func(stored domain.NotificationChannel) error) (domain.NotificationChannel, error) {

	if err := actor.valid(); err != nil {
		return domain.NotificationChannel{}, err
	}
	var (
		normalized string
		hashes     []string
	)
	if patch.Number != nil {
		var err error
		if normalized, hashes, err = s.phoneNumberHashes(*patch.Number); err != nil {
			return domain.NotificationChannel{}, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: begin update phone channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, storedHash, err := s.lockPhoneChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if authorize != nil {
		if err := authorize(stored); err != nil {
			return domain.NotificationChannel{}, err
		}
	}

	next := stored
	if patch.Name != nil {
		next.Name = strings.TrimSpace(*patch.Name)
	}
	numberChanged := false
	switch stored.Recipient {
	case domain.PhoneRecipientUser:
		if patch.Number != nil {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		if patch.RecipientUserID != nil {
			next.RecipientUserID = *patch.RecipientUserID
			if next.RecipientUserID != stored.RecipientUserID && next.DisabledReason == domain.DeliverableRecipientDeleted {
				next.DisabledReason = ""
			}
		}
		if !patch.pureDisable() {
			if err := recipientEligibleTx(ctx, tx, stored.ProjectID, next.RecipientUserID); err != nil {
				return domain.NotificationChannel{}, err
			}
		}
	case domain.PhoneRecipientNumber:
		if patch.RecipientUserID != nil {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientFixed
		}
		if patch.Number != nil && !containsString(hashes, storedHash) {
			numberChanged = true
			verified := false
			next.NumberVerified = &verified
			next.Enabled = false
			next.DisabledReason = domain.DeliverableNumberUnverified
		}
	}
	switch {
	case patch.Enabled == nil: // as stored, unless a number change switched it off above
	case !*patch.Enabled:
		next.Enabled = false
	case numberChanged: // switched on in the write that leaves the number unverified
		return domain.NotificationChannel{}, domain.ErrPhoneNumberUnverified
	case stored.Enabled: // already on
	default:
		if stored.Recipient == domain.PhoneRecipientNumber && (stored.NumberVerified == nil || !*stored.NumberVerified) {
			return domain.NotificationChannel{}, domain.ErrPhoneNumberUnverified
		}
		if err := checkTelephonyInEffect(ctx, tx, stored.Type); err != nil {
			return domain.NotificationChannel{}, err
		}
		next.Enabled = true
		next.DisabledReason = ""
	}
	next.Config = nil
	if err := next.Validate(); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: invalid channel: %w", err)
	}

	var enc, hash, masked string
	if numberChanged {
		if enc, err = s.cipher.EncryptBytes([]byte(normalized), channelNumberAAD(id)); err != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: encrypt channel number: %w", err)
		}
		hash, masked = hashes[0], domain.MaskPhone(normalized)
	}
	// One statement: a number change, its cleared verification and the disable land together (§4.2).
	if _, err := tx.Exec(ctx, `UPDATE notification_channels SET
		    name = $2, enabled = $3, disabled_reason = NULLIF($4, ''), recipient_user_id = NULLIF($5, '')::uuid,
		    number_enc         = CASE WHEN $6 THEN $7 ELSE number_enc END,
		    number_hash        = CASE WHEN $6 THEN $8 ELSE number_hash END,
		    number_masked      = CASE WHEN $6 THEN $9 ELSE number_masked END,
		    number_verified_at = CASE WHEN $6 THEN NULL ELSE number_verified_at END
		WHERE id = $1`,
		id, next.Name, next.Enabled, next.DisabledReason, next.RecipientUserID, numberChanged, enc, hash, masked); err != nil {
		if isRecipientUserFKViolation(err) {
			return domain.NotificationChannel{}, domain.ErrPhoneRecipientNotEligible
		}
		return domain.NotificationChannel{}, fmt.Errorf("store: update phone channel: %w", err)
	}
	if numberChanged {
		if err := expirePhoneCodesTx(ctx, tx, domain.PhoneSubjectChannel, id); err != nil {
			return domain.NotificationChannel{}, err
		}
	}
	if err := insertProjectAudit(ctx, tx, stored.ProjectID, actor, auditPhoneChannelUpdate, phoneChannelAuditTarget(id, stored.Type)); err != nil {
		return domain.NotificationChannel{}, err
	}
	updated, err := s.readChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: commit update phone channel: %w", err)
	}
	return updated, nil
}

// DeletePhoneChannel deletes a phone channel, ends its codes and audits it, in one transaction.
// authorize is as UpdatePhoneChannel's. ErrNotFound for no channel.
func (s *Store) DeletePhoneChannel(ctx context.Context, id string, actor AuditActor,
	authorize func(stored domain.NotificationChannel) error) error {

	if err := actor.valid(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete phone channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, _, err := s.lockPhoneChannelTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if authorize != nil {
		if err := authorize(stored); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete phone channel: %w", err)
	}
	if err := expirePhoneCodesTx(ctx, tx, domain.PhoneSubjectChannel, id); err != nil {
		return err
	}
	if err := insertProjectAudit(ctx, tx, stored.ProjectID, actor, auditPhoneChannelDelete, phoneChannelAuditTarget(id, stored.Type)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete phone channel: %w", err)
	}
	return nil
}

// SendChannelVerificationCode issues a code for a `number` channel's stored number (R5; R6 with the
// channel as subject and requester — the principal who asks: a person's user id, an API token's
// "apitoken:<id>" — as the one whose hour it spends, FR-041a review P2-3) and returns the number and
// the code for the caller to send after the commit. The number leaves the store only here, only to
// receive a code (P1). Refusals, none of which writes anything: domain.ErrPhoneKeyMissing,
// ErrNotFound, domain.ErrPhoneChannelNotNumber, domain.ErrPhoneAlreadyVerified (a verified channel is
// sent no code, §4.2), domain.ErrPhoneSendLimited.
func (s *Store) SendChannelVerificationCode(ctx context.Context, id, requester string) (number, code string, err error) {
	if s.cipher == nil {
		return "", "", domain.ErrPhoneKeyMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: begin send channel code: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ch, _, err := s.lockPhoneChannelTx(ctx, tx, id)
	if err != nil {
		return "", "", err
	}
	if ch.Recipient == domain.PhoneRecipientNumber && ch.NumberVerified != nil && *ch.NumberVerified {
		return "", "", domain.ErrPhoneAlreadyVerified
	}
	number, hashes, err := s.channelNumberTx(ctx, tx, ch)
	if err != nil {
		return "", "", err
	}
	if code, err = s.issuePhoneCodeTx(ctx, tx, domain.PhoneSubjectChannel, id, requester, hashes); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit send channel code: %w", err)
	}
	return number, code, nil
}

// channelNumberTx opens a `number` channel's number and returns it with its hashes under every key.
func (s *Store) channelNumberTx(ctx context.Context, tx pgx.Tx, ch domain.NotificationChannel) (string, []string, error) {
	if ch.Recipient != domain.PhoneRecipientNumber {
		return "", nil, domain.ErrPhoneChannelNotNumber
	}
	var enc string
	if err := tx.QueryRow(ctx, `SELECT number_enc FROM notification_channels WHERE id = $1`, ch.ID).Scan(&enc); err != nil {
		return "", nil, fmt.Errorf("store: read channel number: %w", err)
	}
	pt, err := s.cipher.DecryptBytes(enc, channelNumberAAD(ch.ID))
	if err != nil {
		return "", nil, fmt.Errorf("store: decrypt channel number: %w", err)
	}
	hashes, err := s.cipher.KeyedHashes(phoneNumberHashLabel, pt)
	if err != nil {
		return "", nil, fmt.Errorf("store: hash channel number: %w", err)
	}
	return string(pt), hashes, nil
}

// VerifyChannelNumber judges code against the channel's active code (checkPhoneCodeTx's verdicts,
// committed so a wrong attempt counts) and, on a match for the number the channel holds now, marks
// it verified and audits `notification_channel.verify`. The channel stays as enabled as it was: a
// person switches it on (§4.2). Refusals as SendChannelVerificationCode's, plus the code verdicts.
func (s *Store) VerifyChannelNumber(ctx context.Context, id, code string, actor AuditActor) (domain.NotificationChannel, error) {
	if s.cipher == nil {
		return domain.NotificationChannel{}, domain.ErrPhoneKeyMissing
	}
	if err := actor.valid(); err != nil {
		return domain.NotificationChannel{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: begin verify channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ch, _, err := s.lockPhoneChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if ch.Recipient != domain.PhoneRecipientNumber {
		return domain.NotificationChannel{}, domain.ErrPhoneChannelNotNumber
	}
	numberHash, err := s.checkPhoneCodeTx(ctx, tx, domain.PhoneSubjectChannel, id, code)
	if isPhoneCodeVerdict(err) {
		if cerr := tx.Commit(ctx); cerr != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: commit verification attempt: %w", cerr)
		}
		return domain.NotificationChannel{}, err
	}
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	// The code verifies the number it was SENT to: compared under every key, since the stored hash
	// may predate a rotation while the code's row is under the primary.
	_, hashes, err := s.channelNumberTx(ctx, tx, ch)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if !containsString(hashes, numberHash) {
		return domain.NotificationChannel{}, domain.ErrPhoneCodeExpired
	}
	if _, err := tx.Exec(ctx, `UPDATE notification_channels SET number_verified_at = now(),
		    disabled_reason = NULLIF(disabled_reason, 'number_unverified') WHERE id = $1`, id); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: mark channel number verified: %w", err)
	}
	if err := insertProjectAudit(ctx, tx, ch.ProjectID, actor, auditPhoneChannelVerify, phoneChannelAuditTarget(id, ch.Type)); err != nil {
		return domain.NotificationChannel{}, err
	}
	verified, err := s.readChannelTx(ctx, tx, id)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: commit verify channel: %w", err)
	}
	return verified, nil
}

// ListPhoneRecipients is every eligible recipient of the project's `user` channels (§4.3), with
// whether each has a verified number — never the number, not even masked.
func (s *Store) ListPhoneRecipients(ctx context.Context, projectID string) ([]domain.PhoneRecipient, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id::text, u.display_name, u.email,
		       EXISTS (SELECT 1 FROM user_phones up WHERE up.user_id = u.id AND up.verified_at IS NOT NULL)
		  FROM users u, (SELECT $1::uuid AS project_id) x
		 WHERE `+phoneRecipientEligibleSQL("x", "u.id")+`
		 ORDER BY lower(u.display_name), lower(u.email), u.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list phone recipients: %w", err)
	}
	defer rows.Close()
	out := []domain.PhoneRecipient{}
	for rows.Next() {
		var r domain.PhoneRecipient
		if err := rows.Scan(&r.UserID, &r.DisplayName, &r.Email, &r.PhoneVerified); err != nil {
			return nil, fmt.Errorf("store: scan phone recipient: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate phone recipients: %w", err)
	}
	return out, nil
}

// reencryptChannelNumbers rewrites every `number` channel's number under the primary key and
// recomputes its hash, as reencryptUserPhones does for people (D35).
func (s *Store) reencryptChannelNumbers(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text, number_enc, number_hash FROM notification_channels WHERE number_enc IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("store: scan channel numbers for reencrypt: %w", err)
	}
	type row struct{ id, enc, hash string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.enc, &r.hash); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan channel number: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate channel numbers: %w", err)
	}
	for _, r := range all {
		// No settle: a channel's number_hash is not unique, so the rewrite cannot meet a 23505.
		if _, err := s.reencryptPhoneNumber(ctx, phoneNumberOwner{
			what: "channel " + r.id, aad: channelNumberAAD(r.id), subjectKind: domain.PhoneSubjectChannel,
			rewrite: `UPDATE notification_channels SET number_enc = $2, number_hash = $3 WHERE id = $1 AND number_enc = $4`,
		}, r.id, r.enc, r.hash); err != nil {
			return err
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
