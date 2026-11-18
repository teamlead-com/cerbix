package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The write side of phone_deliveries (FR-041a §4.5): the reservation committed before a provider is
// called (D33), the storm guard counted inside it (§6.2), the attachment of a monitor's DOWN rows to
// the auto-incident that opens for the outage (D32), the provider's answer, and the passes the
// scheduler leader runs. The rows are read elsewhere.
//
// Locks, always in this order: the monitor's (a monitor DOWN only), then the number's hashes sorted,
// then a delivery row (FOR UPDATE). The incident opener takes only the monitor's and then its rows;
// Finish takes only its row. The D32 attachment locks its rows in id order, so it cannot deadlock
// another write of this file.
//
// The leader's two passes never wait for a row (FR-041a review P1-3): the stale sweep and the 90-day
// purge take their rows FOR UPDATE SKIP LOCKED, and a row another transaction holds is left to the
// next pass. That matters for cascades: deleting a channel, a project or an organization deletes its
// delivery rows in the order its scan meets them — not in id order — so a pass that waited could hold
// one row and wait for another the cascade holds while the cascade waits for its row (40P01, and a
// delete answered 500). A pass that never waits is no side of such a cycle. A project's and an
// organization's cascade find the rows through phone_deliveries_project_idx.
//
// The D32 attachment is the one multi-row writer that still waits, and a delete can still meet it:
// deleting an incident sets incident_id NULL in scan order, and deleting a channel, a project or an
// organization deletes rows in scan order, so either can hold one of a monitor's DOWN rows while the
// attachment, holding a lower one in id order, waits for it — when the outage being attached holds two
// or more rows the delete touches (two channels of the monitor, or two events on one channel). That
// cycle is 40P01: if the attachment is the victim, it recovers through the auto-incident opener's
// bounded retry; if the delete is, that delete fails (a 500 to an API caller) and can simply be sent
// again. It is left as it is: FR-041a review P1-3 and §14 take only the leader's passes.

// phoneSlotReasons are the reasons that hold a storm-guard slot (§6.2): a reserved call in flight
// and every call the provider received or may have received. `provider_unreachable` frees its slot.
const phoneSlotReasons = `('sending', 'sent', 'provider_rejected', 'outcome_unknown')`

// phoneDeliveryColumns is what a reservation reports of a row.
var phoneDeliveryColumns = `id::text, reason, COALESCE(incident_id::text, ''), attempts,
	updated_at > now() - make_interval(secs => ` + phoneStaleSecsSQL + `)`

// phoneStaleSecsSQL is domain.PhoneDeliveryStaleAfter in SQL.
var phoneStaleSecsSQL = fmt.Sprint(int(domain.PhoneDeliveryStaleAfter / time.Second))

// mergePhoneNotesSQL appends the text[] parameter $n to `notes`, keeping the first occurrence of each
// note in its order.
func mergePhoneNotesSQL(param string) string {
	return `ARRAY(SELECT n FROM unnest(phone_deliveries.notes || ` + param + `::text[]) WITH ORDINALITY AS t(n, o)
	              GROUP BY n ORDER BY min(o))`
}

// phoneDeliveryRow is a row as a reservation meets it.
type phoneDeliveryRow struct {
	id, reason, incidentID string
	attempts               int
	young                  bool // updated_at within domain.PhoneDeliveryStaleAfter
}

func scanPhoneDeliveryRow(row pgx.Row) (phoneDeliveryRow, error) {
	var r phoneDeliveryRow
	err := row.Scan(&r.id, &r.reason, &r.incidentID, &r.attempts, &r.young)
	return r, err
}

func (r phoneDeliveryRow) reservation(status domain.PhoneReservationStatus) domain.PhoneReservation {
	return domain.PhoneReservation{ID: r.id, Status: status, Reason: r.reason, IncidentID: r.incidentID, Attempts: r.attempts}
}

// phoneDeliveryTarget is a delivery's number as stored, and the incident the row carries.
type phoneDeliveryTarget struct {
	hashes     []string // every configured key, primary first; nil without a number
	masked     string
	incidentID string
}

func (t phoneDeliveryTarget) hash() *string {
	if len(t.hashes) == 0 {
		return nil
	}
	return &t.hashes[0]
}

func validPhoneDeliveryInput(in domain.PhoneReserveInput) error {
	if in.ProjectID == "" || in.ChannelID == "" {
		return errors.New("store: phone delivery: project and channel are required")
	}
	if !domain.IsPhoneChannelType(in.Kind) {
		return fmt.Errorf("store: phone delivery: kind %q is not sms or voice", in.Kind)
	}
	if !domain.ValidPhoneAlertKind(in.AlertKind) {
		return fmt.Errorf("store: phone delivery: unknown alert kind %q", in.AlertKind)
	}
	if in.AlertKind == domain.PhoneAlertMonitorDown && in.MonitorID == "" {
		return errors.New("store: phone delivery: a monitor DOWN names its monitor (D32)")
	}
	return nil
}

// prepareTx hashes the number (when there is one) and, for a monitor DOWN, takes the monitor's lock
// and looks its open auto-incident up (D32). Any other alert keeps the incident it carries.
func (s *Store) preparePhoneDeliveryTx(ctx context.Context, tx pgx.Tx, in domain.PhoneReserveInput) (phoneDeliveryTarget, error) {
	var t phoneDeliveryTarget
	if in.Number != "" {
		normalized, hashes, err := s.phoneNumberHashes(in.Number)
		if err != nil {
			return t, err
		}
		t.hashes, t.masked = hashes, domain.MaskPhone(normalized)
	}
	t.incidentID = in.IncidentID
	if in.AlertKind == domain.PhoneAlertMonitorDown && in.MonitorID != "" {
		if err := lockPhoneMonitorTx(ctx, tx, in.MonitorID); err != nil {
			return t, err
		}
		id, err := openMonitorAutoIncidentTx(ctx, tx, in.MonitorID)
		if err != nil {
			return t, err
		}
		t.incidentID = id
	}
	return t, nil
}

// lockPhoneMonitorTx takes the monitor's D32 lock for the transaction: an advisory lock keyed by the
// monitor's id rather than the monitor's row, which every check result updates. Taken by a monitor
// DOWN reservation and by the opening of the monitor's auto-incident, and by nothing else.
func lockPhoneMonitorTx(ctx context.Context, tx pgx.Tx, monitorID string) error {
	// Keyed by the CANONICAL uuid text, so any spelling of one id (case, braces) takes the same lock —
	// as the change ledger's service lock does (change.go).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_delivery_monitor'), hashtext($1::uuid::text))`,
		monitorID); err != nil {
		return fmt.Errorf("store: lock phone delivery monitor: %w", err)
	}
	return nil
}

// lockPhoneNumberTx takes the storm guard's lock on the number under every configured key, in sorted
// order. A process restarted with a new primary key and one not yet restarted share the old key's
// hash, so they still exclude each other during a rotation (D35).
func lockPhoneNumberTx(ctx context.Context, tx pgx.Tx, hashes []string) error {
	sorted := append([]string(nil), hashes...)
	sort.Strings(sorted)
	for _, h := range sorted {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('phone_delivery_number'), hashtext($1))`, h); err != nil {
			return fmt.Errorf("store: lock phone delivery number: %w", err)
		}
	}
	return nil
}

// openMonitorAutoIncidentTx returns the monitor's open auto-incident, or "" when it has none: the
// incident FindOpenAutoIncidentByMonitor finds (openMonitorAutoIncidentFrom).
func openMonitorAutoIncidentTx(ctx context.Context, tx pgx.Tx, monitorID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text `+openMonitorAutoIncidentFrom, monitorID).Scan(&id)
	if noRows(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: phone delivery incident: %w", err)
	}
	return id, nil
}

// phoneStormBoundsTx reads the storm guard's bounds from the stored telephony group, with §6.2's
// defaults for a bound that is absent or not positive (an unsaved group has none).
func phoneStormBoundsTx(ctx context.Context, tx pgx.Tx) (domain.TelephonyStorm, error) {
	var b domain.TelephonyStorm
	err := tx.QueryRow(ctx, `SELECT
		       CASE WHEN jsonb_typeof(telephony #> '{storm,per_incident_per_hour}') = 'number'
		            THEN (telephony #>> '{storm,per_incident_per_hour}')::numeric::int ELSE 0 END,
		       CASE WHEN jsonb_typeof(telephony #> '{storm,per_number_per_hour}') = 'number'
		            THEN (telephony #>> '{storm,per_number_per_hour}')::numeric::int ELSE 0 END
		  FROM instance_settings WHERE id = true`).Scan(&b.PerIncidentPerHour, &b.PerNumberPerHour)
	if err != nil && !noRows(err) {
		return domain.TelephonyStorm{}, fmt.Errorf("store: read phone storm bounds: %w", err)
	}
	return b.WithDefaults(), nil
}

// phoneStormLimitedTx counts the slots held on the number in the last hour (§6.2), excluding the row
// excludeID (a re-armed row), and reports which bound one more would exceed — the note the row then
// carries (§4.5), PhoneNoteStormPerNumber or PhoneNoteStormPerIncident — or "" when none. The number's
// bound is checked first and named when both are reached. The bounds are read
// here too, from the stored telephony group: under the number's lock they are as current as the
// count, whatever settings snapshot the calling process holds (P13). The subject bound
// is per incident when the delivery has one; else per monitor for a monitor alert other than burn —
// a DOWN whose incident is not found yet (D32), counted over the monitor's rows that have no
// incident either; else per service for a service's health or recovery alert whose incident is not
// found (§6.2, FR-041a review P2-7), counted over the service's such rows without an incident; both
// named storm_per_incident too. Else there is none, and only the number's bound applies (a burn
// alert). The caller holds the number's lock, so the count and the insert that follows are one step
// for every other reservation of the number.
func phoneStormLimitedTx(ctx context.Context, tx pgx.Tx, in domain.PhoneReserveInput, t phoneDeliveryTarget,
	excludeID string) (string, error) {
	storm, err := phoneStormBoundsTx(ctx, tx)
	if err != nil {
		return "", err
	}
	subjectMonitor, subjectService := "", ""
	switch {
	case t.incidentID != "" || domain.IsPhoneBurnAlert(in.AlertKind):
	case in.MonitorID != "":
		subjectMonitor = in.MonitorID
	case in.ServiceID != "" && (in.AlertKind == domain.PhoneAlertServiceHealth || in.AlertKind == domain.PhoneAlertServiceRecovered):
		subjectService = in.ServiceID
	}
	var perNumber, perSubject int
	if err := tx.QueryRow(ctx, `SELECT count(*),
		       count(*) FILTER (WHERE incident_id = NULLIF($2, '')::uuid
		                           OR (monitor_id = NULLIF($3, '')::uuid AND incident_id IS NULL
		                               AND alert_kind NOT IN ('`+domain.PhoneAlertMonitorBurn+`', '`+domain.PhoneAlertServiceBurn+`'))
		                           OR (service_id = NULLIF($5, '')::uuid AND incident_id IS NULL
		                               AND alert_kind IN ('`+domain.PhoneAlertServiceHealth+`', '`+domain.PhoneAlertServiceRecovered+`')))
		  FROM phone_deliveries
		 WHERE number_hash = ANY($1) AND reason IN `+phoneSlotReasons+`
		   AND created_at > now() - interval '1 hour'
		   AND id <> COALESCE(NULLIF($4, '')::uuid, '00000000-0000-0000-0000-000000000000')`,
		t.hashes, t.incidentID, subjectMonitor, excludeID, subjectService).Scan(&perNumber, &perSubject); err != nil {
		return "", fmt.Errorf("store: count phone storm: %w", err)
	}
	if perNumber >= storm.PerNumberPerHour {
		return domain.PhoneNoteStormPerNumber, nil
	}
	if (t.incidentID != "" || subjectMonitor != "" || subjectService != "") && perSubject >= storm.PerIncidentPerHour {
		return domain.PhoneNoteStormPerIncident, nil
	}
	return "", nil
}

// withPhoneNote is in with note appended to a copy of its notes; in itself is not changed.
func withPhoneNote(in domain.PhoneReserveInput, note string) domain.PhoneReserveInput {
	if note != "" {
		in.Notes = append(append([]string(nil), in.Notes...), note)
	}
	return in
}

// phoneServiceState is the row's service_state: the input's state when it is one of the service
// alert vocabulary (the column's CHECK), else NULL — a label is never worth losing a page over.
func phoneServiceState(in domain.PhoneReserveInput) *string {
	if in.ServiceState == "" || !domain.ValidServiceAlertState(domain.ServiceAlertState(in.ServiceState)) {
		return nil
	}
	return &in.ServiceState
}

// phoneEscalationStep is the row's escalation_step: the 1-based step, NULL for 0 (no step).
func phoneEscalationStep(in domain.PhoneReserveInput) *int {
	if in.EscalationStep < 1 {
		return nil
	}
	return &in.EscalationStep
}

// ReservePhoneDelivery reserves one delivery before its provider call (D33) and commits, so that a
// retry, a callback and the leader's pass all find the row. In one transaction it takes the locks,
// finds the incident of a monitor DOWN (D32), and then:
//
//   - no row for (OutboxEventID, ChannelID): counts the storm guard (§6.2) and inserts the row as
//     `sending` (Proceed) or, over a bound, as the final `storm_limited` (StormLimited) with the
//     bound named in its notes (§4.5);
//   - a final row: changes nothing (Existing);
//   - a `sending` row younger than domain.PhoneDeliveryStaleAfter: domain.ErrPhoneDeliveryInFlight,
//     nothing changed — the event must be retried, not reported delivered;
//   - an older `sending` row: closes it as `outcome_unknown` (ClosedStale) and makes no call;
//   - a `provider_unreachable` row: counts the guard again without the row itself, and sets the SAME
//     row back to `sending` with one more attempt and updated_at now (Proceed), or to
//     `storm_limited` (StormLimited).
//
// in.Number is required (it is hashed and masked, never stored); domain.ErrPhoneKeyMissing without
// the at-rest key; domain.ErrPhoneChannelGone when the channel or its project was deleted meanwhile
// (FR-041a review P3-4).
func (s *Store) ReservePhoneDelivery(ctx context.Context, in domain.PhoneReserveInput) (domain.PhoneReservation, error) {
	if err := validPhoneDeliveryInput(in); err != nil {
		return domain.PhoneReservation{}, err
	}
	if in.Number == "" {
		return domain.PhoneReservation{}, errors.New("store: reserve phone delivery: a number is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.PhoneReservation{}, fmt.Errorf("store: begin reserve phone delivery: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	t, err := s.preparePhoneDeliveryTx(ctx, tx, in)
	if err != nil {
		return domain.PhoneReservation{}, err
	}
	if err := lockPhoneNumberTx(ctx, tx, t.hashes); err != nil {
		return domain.PhoneReservation{}, err
	}
	res, err := reservePhoneDeliveryTx(ctx, tx, in, t)
	if err != nil {
		return domain.PhoneReservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.PhoneReservation{}, fmt.Errorf("store: commit reserve phone delivery: %w", err)
	}
	return res, nil
}

func reservePhoneDeliveryTx(ctx context.Context, tx pgx.Tx, in domain.PhoneReserveInput, t phoneDeliveryTarget) (domain.PhoneReservation, error) {
	// Two passes at most: the second only when a concurrent reservation of the same key — one hashed
	// under other keys, so not excluded by the number's lock — inserted between our look and our
	// insert. The INSERT waited for it to commit; the second look sees its row.
	for pass := 0; pass < 2; pass++ {
		if in.OutboxEventID != "" {
			existing, err := scanPhoneDeliveryRow(tx.QueryRow(ctx, `SELECT `+phoneDeliveryColumns+`
				  FROM phone_deliveries WHERE outbox_event_id = $1 AND channel_id = $2 FOR UPDATE`,
				in.OutboxEventID, in.ChannelID))
			if err == nil {
				return retryPhoneDeliveryTx(ctx, tx, in, t, existing)
			}
			if !noRows(err) {
				return domain.PhoneReservation{}, fmt.Errorf("store: find phone delivery: %w", err)
			}
		}
		limit, err := phoneStormLimitedTx(ctx, tx, in, t, "")
		if err != nil {
			return domain.PhoneReservation{}, err
		}
		reason, status := domain.PhoneReasonSending, domain.PhoneReservationProceed
		if limit != "" {
			reason, status = domain.PhoneReasonStormLimited, domain.PhoneReservationStormLimited
		}
		row, err := insertPhoneDeliveryTx(ctx, tx, withPhoneNote(in, limit), t, reason)
		if noRows(err) {
			continue
		}
		if err != nil {
			return domain.PhoneReservation{}, err
		}
		return row.reservation(status), nil
	}
	return domain.PhoneReservation{}, errors.New("store: reserve phone delivery: the row vanished during the reservation")
}

// retryPhoneDeliveryTx is D33's judgement of the row a redelivered event meets (locked FOR UPDATE).
func retryPhoneDeliveryTx(ctx context.Context, tx pgx.Tx, in domain.PhoneReserveInput, t phoneDeliveryTarget,
	r phoneDeliveryRow) (domain.PhoneReservation, error) {
	switch r.reason {
	case domain.PhoneReasonSending:
		if r.young {
			return domain.PhoneReservation{}, domain.ErrPhoneDeliveryInFlight
		}
		closed, err := scanPhoneDeliveryRow(tx.QueryRow(ctx, `UPDATE phone_deliveries
			   SET reason = 'outcome_unknown', updated_at = now()
			 WHERE id = $1 RETURNING `+phoneDeliveryColumns, r.id))
		if err != nil {
			return domain.PhoneReservation{}, fmt.Errorf("store: close stale phone delivery: %w", err)
		}
		return closed.reservation(domain.PhoneReservationClosedStale), nil
	case domain.PhoneReasonProviderUnreachable:
		// The row keeps an incident it already has; a monitor DOWN's lookup can only add one.
		if r.incidentID != "" {
			t.incidentID = r.incidentID
		}
		limit, err := phoneStormLimitedTx(ctx, tx, in, t, r.id)
		if err != nil {
			return domain.PhoneReservation{}, err
		}
		reason, status, attempt := domain.PhoneReasonSending, domain.PhoneReservationProceed, 1
		if limit != "" {
			reason, status, attempt = domain.PhoneReasonStormLimited, domain.PhoneReservationStormLimited, 0
		}
		in = withPhoneNote(in, limit)
		// The row is this attempt's (FR-041a review P2-6): the person and the number it calls — a
		// `user` channel follows its user, retargeted or with a new number (P4) — and its notes, which
		// replace the earlier attempt's (a `no_incident_ref` of an attempt before the incident opened
		// does not outlive the attempt that linked it). The previous attempt's answer goes: this
		// attempt has none yet, and a row closed later as outcome_unknown must not show a status
		// beside it.
		rearmed, err := scanPhoneDeliveryRow(tx.QueryRow(ctx, `UPDATE phone_deliveries
			   SET reason = $2, attempts = attempts + $3, updated_at = now(),
			       provider_status = NULL, provider_code = NULL, provider_ref = NULL,
			       incident_id = COALESCE(incident_id, NULLIF($4, '')::uuid),
			       user_id = NULLIF($8, '')::uuid,
			       number_hash = $5, number_masked = $6, notes = $7::text[]
			 WHERE id = $1 RETURNING `+phoneDeliveryColumns,
			r.id, reason, attempt, t.incidentID, t.hash(), t.masked, notesOrEmpty(in.Notes), in.UserID))
		if err != nil {
			return domain.PhoneReservation{}, fmt.Errorf("store: re-arm phone delivery: %w", err)
		}
		return rearmed.reservation(status), nil
	default:
		return r.reservation(domain.PhoneReservationExisting), nil
	}
}

// insertPhoneDeliveryTx inserts the row with reason; pgx.ErrNoRows when the (event, channel) key is
// already taken (ON CONFLICT DO NOTHING).
func insertPhoneDeliveryTx(ctx context.Context, tx pgx.Tx, in domain.PhoneReserveInput, t phoneDeliveryTarget,
	reason string) (phoneDeliveryRow, error) {
	var seq *int64
	if in.MonitorSeq > 0 {
		seq = &in.MonitorSeq
	}
	row, err := scanPhoneDeliveryRow(tx.QueryRow(ctx, `INSERT INTO phone_deliveries
		    (project_id, channel_id, outbox_event_id, incident_id, monitor_id, monitor_seq, service_id, user_id,
		     number_hash, number_masked, kind, alert_kind, reason, notes, service_state, escalation_step)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, $6,
		        NULLIF($7, '')::uuid, NULLIF($8, '')::uuid, $9, NULLIF($10, ''), $11, $12, $13, $14, $15, $16)
		ON CONFLICT (outbox_event_id, channel_id) WHERE outbox_event_id IS NOT NULL DO NOTHING
		RETURNING `+phoneDeliveryColumns,
		in.ProjectID, in.ChannelID, in.OutboxEventID, t.incidentID, in.MonitorID, seq, in.ServiceID, in.UserID,
		t.hash(), t.masked, string(in.Kind), in.AlertKind, reason, notesOrEmpty(in.Notes),
		phoneServiceState(in), phoneEscalationStep(in)))
	if isPhoneChannelGone(err) {
		return row, domain.ErrPhoneChannelGone
	}
	if err != nil && !noRows(err) {
		return row, fmt.Errorf("store: insert phone delivery: %w", err)
	}
	return row, err
}

// isPhoneChannelGone reports a delivery row refused because its channel or its project no longer
// exists — deleted after the delivery read its targets (FR-041a review P3-4). Not the incident's key:
// that one is ON DELETE SET NULL and is looked up again by the retry.
func isPhoneChannelGone(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" &&
		(pgErr.ConstraintName == "phone_deliveries_channel_id_fkey" || pgErr.ConstraintName == "phone_deliveries_project_id_fkey")
}

func notesOrEmpty(notes []string) []string {
	if notes == nil {
		return []string{}
	}
	return notes
}

// RecordUnsentPhoneDelivery writes the final row of a channel that is not a route at send (§4.5
// "when a row is written"): `not_deliverable` (the cause in in.Notes), `telephony_disabled` or
// `voice_unsupported_by_provider`. No storm count and no number lock — nothing is sent. in.Number is
// optional (R9: a user without a verified number has none to record). A monitor DOWN takes D32's
// lock and incident like a reservation, so the row joins its incident either way.
//
// Idempotent on (OutboxEventID, ChannelID): an existing final or `sending` row is left alone, and a
// `provider_unreachable` row of an earlier attempt is settled with this reason rather than left
// retryable for ever — with this attempt's person, number and notes, and without the earlier
// attempt's provider answer (FR-041a review P2-6). Reports whether a row was written, and the reason the event's row for the
// channel has afterwards — this one, or the existing row's: a retry that meets an earlier `sent` row
// learns that the person was reached (D20). A channel — or project — deleted meanwhile is
// domain.ErrPhoneChannelGone (FR-041a review P3-4).
func (s *Store) RecordUnsentPhoneDelivery(ctx context.Context, in domain.PhoneReserveInput, reason string) (wrote bool, rowReason string, err error) {
	switch reason {
	case domain.PhoneReasonNotDeliverable, domain.PhoneReasonTelephonyDisabled, domain.PhoneReasonVoiceUnsupportedByProvider:
	default:
		return false, "", fmt.Errorf("store: record unsent phone delivery: %q is not a reason for an unsent delivery", reason)
	}
	if err := validPhoneDeliveryInput(in); err != nil {
		return false, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("store: begin record unsent phone delivery: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	t, err := s.preparePhoneDeliveryTx(ctx, tx, in)
	if err != nil {
		return false, "", err
	}
	var seq *int64
	if in.MonitorSeq > 0 {
		seq = &in.MonitorSeq
	}
	tag, err := tx.Exec(ctx, `INSERT INTO phone_deliveries
		    (project_id, channel_id, outbox_event_id, incident_id, monitor_id, monitor_seq, service_id, user_id,
		     number_hash, number_masked, kind, alert_kind, reason, notes, service_state, escalation_step)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, $6,
		        NULLIF($7, '')::uuid, NULLIF($8, '')::uuid, $9, NULLIF($10, ''), $11, $12, $13, $14, $15, $16)
		ON CONFLICT (outbox_event_id, channel_id) WHERE outbox_event_id IS NOT NULL DO UPDATE
		   SET reason = EXCLUDED.reason, updated_at = now(),
		       incident_id = COALESCE(phone_deliveries.incident_id, EXCLUDED.incident_id),
		       user_id = EXCLUDED.user_id, number_hash = EXCLUDED.number_hash,
		       number_masked = EXCLUDED.number_masked, notes = EXCLUDED.notes,
		       provider_status = NULL, provider_code = NULL, provider_ref = NULL
		 WHERE phone_deliveries.reason = 'provider_unreachable'`,
		in.ProjectID, in.ChannelID, in.OutboxEventID, t.incidentID, in.MonitorID, seq, in.ServiceID, in.UserID,
		t.hash(), t.masked, string(in.Kind), in.AlertKind, reason, notesOrEmpty(in.Notes),
		phoneServiceState(in), phoneEscalationStep(in))
	if isPhoneChannelGone(err) {
		return false, "", domain.ErrPhoneChannelGone
	}
	if err != nil {
		return false, "", fmt.Errorf("store: record unsent phone delivery: %w", err)
	}
	wrote, rowReason = tag.RowsAffected() == 1, reason
	if !wrote {
		err := tx.QueryRow(ctx, `SELECT reason FROM phone_deliveries
			 WHERE outbox_event_id = $1::uuid AND channel_id = $2`, in.OutboxEventID, in.ChannelID).Scan(&rowReason)
		if noRows(err) {
			// The row the INSERT met is gone: its channel was deleted, and the cascade took it.
			return false, "", domain.ErrPhoneChannelGone
		}
		if err != nil {
			return false, "", fmt.Errorf("store: read the existing phone delivery: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("store: commit record unsent phone delivery: %w", err)
	}
	return wrote, rowReason, nil
}

// FinishPhoneDelivery stores the provider's answer on a reserved row: `sent`, `provider_unreachable`
// or `provider_rejected`, the HTTP status (0 → NULL), the code and reference ("" → NULL: the column
// refuses an empty code, and a 2xx or a transport failure has none), the segments, and out.Notes
// appended. Only a `sending` row is finished: ErrNotFound when the id names no such row (unknown, or
// already closed — by a retry or the leader's pass — as `outcome_unknown`), which also matches
// domain.ErrPhoneDeliveryClosed for a caller that cannot import this package (notify).
func (s *Store) FinishPhoneDelivery(ctx context.Context, id string, out domain.PhoneOutcome) error {
	switch out.Reason {
	case domain.PhoneReasonSent, domain.PhoneReasonProviderUnreachable, domain.PhoneReasonProviderRejected:
	default:
		return fmt.Errorf("store: finish phone delivery: %q is not a provider outcome", out.Reason)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE phone_deliveries
		   SET reason = $2, provider_status = NULLIF($3, 0), provider_code = NULLIF($4, ''),
		       provider_ref = NULLIF($5, ''), segments = $6, notes = `+mergePhoneNotesSQL("$7")+`,
		       updated_at = now()
		 WHERE id = $1 AND reason = 'sending'`,
		id, out.Reason, out.HTTPStatus, out.Code, out.Ref, out.Segments, notesOrEmpty(out.Notes))
	if err != nil {
		return fmt.Errorf("store: finish phone delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: finish phone delivery %s: no sending row: %w (%w)", id, ErrNotFound, domain.ErrPhoneDeliveryClosed)
	}
	return nil
}

// ClosedPhoneDelivery is a row CloseStalePhoneDeliveries closed, as the leader counts it in
// cerbix_phone_deliveries_total (P7): its channel kind, and the provider in force when it was closed
// ("" when none is stored) — the label a retry that closes a stale row gives it too.
type ClosedPhoneDelivery struct {
	Kind     domain.ChannelType
	Provider string
}

// CloseStalePhoneDeliveries is the leader's pass of D33: every `sending` row whose updated_at is
// older than domain.PhoneDeliveryStaleAfter becomes `outcome_unknown` — the rows no retry comes back
// to. updated_at, not created_at: a row re-armed for its fourth attempt is older than that by its
// creation and has a call in flight. Returns one entry per row closed.
//
// A row another transaction holds is skipped (FR-041a review P1-3): it is being finished by its
// delivery, re-armed by a retry or deleted by a cascade, and if it is still stale the next pass, 15 s
// later, closes it. Nothing in D33 needs the pass to wait, and a pass that never waits cannot deadlock.
func (s *Store) CloseStalePhoneDeliveries(ctx context.Context) ([]ClosedPhoneDelivery, error) {
	rows, err := s.pool.Query(ctx, `WITH stale AS (
		    SELECT id FROM phone_deliveries
		     WHERE reason = 'sending' AND updated_at < now() - make_interval(secs => `+phoneStaleSecsSQL+`)
		     ORDER BY id FOR UPDATE SKIP LOCKED)
		UPDATE phone_deliveries d SET reason = 'outcome_unknown', updated_at = now()
		  FROM stale WHERE d.id = stale.id
		RETURNING d.kind, COALESCE((SELECT ts.telephony_provider FROM instance_settings ts WHERE ts.id), '')`)
	if err != nil {
		return nil, fmt.Errorf("store: close stale phone deliveries: %w", err)
	}
	defer rows.Close()
	var out []ClosedPhoneDelivery
	for rows.Next() {
		var c ClosedPhoneDelivery
		if err := rows.Scan(&c.Kind, &c.Provider); err != nil {
			return nil, fmt.Errorf("store: scan closed phone delivery: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: close stale phone deliveries: %w", err)
	}
	return out, nil
}

// phonePurgeBatch is how many rows one statement of PurgePhoneDeliveries deletes.
var phonePurgeBatch = 1000

// PurgePhoneDeliveries deletes delivery rows created more than olderThan ago (§8: kept 90 days); a
// non-positive olderThan is 90 days. Returns the number deleted. Leader tick.
//
// It deletes in batches of phonePurgeBatch, each its own statement, so no single statement holds the
// locks of a whole day's rows; and it skips a row another transaction holds (FR-041a review P1-3),
// which the next day's purge takes. It stops at the first batch that deletes nothing.
func (s *Store) PurgePhoneDeliveries(ctx context.Context, olderThan time.Duration) (int, error) {
	secs := int(olderThan.Seconds())
	if secs <= 0 {
		secs = 90 * 24 * 3600
	}
	total := 0
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM phone_deliveries WHERE id IN (
			    SELECT id FROM phone_deliveries
			     WHERE created_at < now() - make_interval(secs => $1)
			     LIMIT $2 FOR UPDATE SKIP LOCKED)`, secs, phonePurgeBatch)
		if err != nil {
			return total, fmt.Errorf("store: purge phone deliveries: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// PurgePhoneVerifications deletes verification codes created more than olderThan ago (R6: the send
// limits look back 24 hours, so nothing older counts); a non-positive olderThan is 24 hours. Returns
// the number deleted. Leader tick.
func (s *Store) PurgePhoneVerifications(ctx context.Context, olderThan time.Duration) (int, error) {
	secs := int(olderThan.Seconds())
	if secs <= 0 {
		secs = 24 * 3600
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM phone_verifications WHERE created_at < now() - make_interval(secs => $1)`, secs)
	if err != nil {
		return 0, fmt.Errorf("store: purge phone verifications: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// attachMonitorPhoneDeliveriesTx is the opener's half of D32, run by the transaction that opens a
// monitor's auto-incident after it took lockPhoneMonitorTx and inserted the incident: the monitor's
// DOWN rows of THIS outage — no incident yet, and the monitor_seq of the monitor's current
// state_sequence — join the incident. A row of an earlier outage (an older seq) and a burn alert stay
// out. updated_at is not touched: it times a `sending` row's call (D33), and an attachment is not one.
func attachMonitorPhoneDeliveriesTx(ctx context.Context, tx pgx.Tx, incidentID, monitorID string) error {
	// The rows are locked in id order first (only them: never the monitor's row, which every check
	// result updates), as the leader's sweep locks its rows, so the two cannot deadlock.
	if _, err := tx.Exec(ctx, `WITH outage AS (
		    SELECT d.id FROM phone_deliveries d JOIN monitors m ON m.id = d.monitor_id
		     WHERE m.id = $2 AND d.incident_id IS NULL
		       AND d.alert_kind = '`+domain.PhoneAlertMonitorDown+`' AND d.monitor_seq = m.state_sequence
		     ORDER BY d.id FOR UPDATE OF d)
		UPDATE phone_deliveries d SET incident_id = $1
		  FROM outage WHERE d.id = outage.id`,
		incidentID, monitorID); err != nil {
		return fmt.Errorf("store: attach phone deliveries to incident: %w", err)
	}
	return nil
}
