package store

import (
	"context"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// PhoneTargets returns the enabled sms and voice channels among ids, in the order of ids, each with
// its D31 rule-1 verdict and the verified number it would be sent to (FR-041a §4.5, P1, P4). Other
// channel types, disabled channels — skipped at send as any disabled channel is — and unknown ids are
// left out.
//
// The verdict is phoneRouteReasonSQL, the fragment the route predicates and the channel DTO embed,
// never a Go re-implementation; it and the numbers come from one statement, so they describe the same
// moment. A number is opened only when it is verified: the user's CURRENT verified number in `user`
// mode (P4), the channel's own in `number` mode. It is opened into memory for this send and never
// stored or returned anywhere else; a non-route keeps it only so that its row can carry the mask. A
// number that cannot be opened leaves its target without one and with NumberErr; the read succeeds.
func (s *Store) PhoneTargets(ctx context.Context, ids []string) ([]domain.PhoneTarget, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id::text, c.project_id::text, c.type, COALESCE(c.recipient, ''),
		       COALESCE(c.recipient_user_id::text, ''), COALESCE(`+phoneRouteReasonSQL("c")+`, ''),
		       COALESCE(up.language, ''),
		       CASE WHEN c.recipient = 'number' AND c.number_verified_at IS NOT NULL THEN c.number_enc
		            WHEN c.recipient = 'user' AND up.verified_at IS NOT NULL THEN up.number_enc END
		  FROM notification_channels c
		  LEFT JOIN user_phones up ON c.recipient = 'user' AND up.user_id = c.recipient_user_id
		 WHERE c.id = ANY($1::uuid[]) AND c.enabled AND c.type IN ('sms', 'voice')
		 ORDER BY array_position($1::uuid[], c.id)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: phone targets: %w", err)
	}
	defer rows.Close()
	type sealed struct {
		t   domain.PhoneTarget
		enc *string
	}
	var found []sealed
	for rows.Next() {
		var r sealed
		if err := rows.Scan(&r.t.ChannelID, &r.t.ProjectID, &r.t.Kind, &r.t.Recipient, &r.t.UserID,
			&r.t.NotRouteReason, &r.t.Language, &r.enc); err != nil {
			return nil, fmt.Errorf("store: scan phone target: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate phone targets: %w", err)
	}
	out := make([]domain.PhoneTarget, 0, len(found))
	for _, r := range found {
		if r.enc != nil {
			// A number that will not open fails its own target only (NumberErr): the event's other
			// channels are still phoned, and this one is retried with the event (D29 stays fail-closed
			// for it — it is never sent without its number).
			r.t.Number, r.t.NumberErr = s.openPhoneTargetNumber(r.t, *r.enc)
		}
		out = append(out, r.t)
	}
	return out, nil
}

// openPhoneTargetNumber decrypts a target's number under the AAD of where it is stored. Fail-closed
// without the at-rest key (D29). The error names the channel, never the number.
func (s *Store) openPhoneTargetNumber(t domain.PhoneTarget, enc string) (string, error) {
	if s.cipher == nil {
		return "", domain.ErrPhoneKeyMissing
	}
	aad := channelNumberAAD(t.ChannelID)
	if t.Recipient == domain.PhoneRecipientUser {
		aad = userPhoneAAD(t.UserID)
	}
	pt, err := s.cipher.DecryptBytes(enc, aad)
	if err != nil {
		return "", fmt.Errorf("store: open the number of phone channel %s: %w", t.ChannelID, err)
	}
	return string(pt), nil
}
