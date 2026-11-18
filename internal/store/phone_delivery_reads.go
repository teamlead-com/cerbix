package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The two operator reads of phone_deliveries (FR-041a §4.5, R13). They return the mask and the
// provider's status, code and reference — never the number, its hash or an answer text (P3, D34) —
// and what the delivery was about (revision 7): its subject, resolved now from the row's monitor or
// service, the service state and escalation step stored with it, and the recipient's display name,
// resolved now from the row's user — never an email.

// phoneDeliveryViewColumns reads a row aliased d with its channel c. The subject is the service when
// the row names one — an escalation step of a service incident names both, and is about the service,
// as its message is (R17) — else the monitor; NULL when the one named is gone. Both lookups stay in
// the row's project. The recipient is named by recipientNameSQL, as on the channel.
var phoneDeliveryViewColumns = `d.id::text, d.channel_id::text, c.name, d.kind, d.alert_kind, d.number_masked,
	d.reason, d.notes, d.provider_status, d.provider_code, d.provider_ref, d.segments, d.attempts,
	d.created_at, d.updated_at,
	CASE WHEN d.service_id IS NOT NULL THEN '` + domain.PhoneDeliverySubjectService + `'
	     WHEN d.monitor_id IS NOT NULL THEN '` + domain.PhoneDeliverySubjectMonitor + `' END,
	CASE WHEN d.service_id IS NOT NULL
	     THEN (SELECT ds.name FROM services ds WHERE ds.id = d.service_id AND ds.project_id = d.project_id)
	     ELSE (SELECT dm.name FROM monitors dm WHERE dm.id = d.monitor_id AND dm.project_id = d.project_id) END,
	d.service_state, d.escalation_step,
	(SELECT ` + recipientNameSQL("du") + ` FROM users du WHERE du.id = d.user_id)`

// ListIncidentPhoneDeliveries returns every phone delivery of an incident, oldest first — the order of
// the incident's timeline it is shown under (FR-041a review P3-2). An incident with none (or none at
// all) answers an empty, non-nil list.
func (s *Store) ListIncidentPhoneDeliveries(ctx context.Context, incidentID string) ([]domain.PhoneDeliveryView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+phoneDeliveryViewColumns+`
		  FROM phone_deliveries d
		  JOIN notification_channels c ON c.id = d.channel_id
		 WHERE d.incident_id = $1
		 ORDER BY d.created_at, d.id`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("store: list incident phone deliveries: %w", err)
	}
	defer rows.Close()
	return scanPhoneDeliveryViews(rows, false)
}

// ListChannelPhoneDeliveries returns a channel's last limit phone deliveries, newest first, each
// with the incident it concerned (id and reference) when it still exists.
func (s *Store) ListChannelPhoneDeliveries(ctx context.Context, channelID string, limit int) ([]domain.PhoneDeliveryView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+phoneDeliveryViewColumns+`, i.id::text, i.ref
		  FROM phone_deliveries d
		  JOIN notification_channels c ON c.id = d.channel_id
		  LEFT JOIN incidents i ON i.id = d.incident_id
		 WHERE d.channel_id = $1
		 ORDER BY d.created_at DESC, d.id
		 LIMIT $2`, channelID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list channel phone deliveries: %w", err)
	}
	defer rows.Close()
	return scanPhoneDeliveryViews(rows, true)
}

func scanPhoneDeliveryViews(rows pgx.Rows, withIncident bool) ([]domain.PhoneDeliveryView, error) {
	out := []domain.PhoneDeliveryView{}
	for rows.Next() {
		var v domain.PhoneDeliveryView
		var subjectKind, subjectName *string
		dest := []any{&v.ID, &v.ChannelID, &v.ChannelName, &v.Kind, &v.AlertKind, &v.NumberMasked, &v.Reason,
			&v.Notes, &v.ProviderStatus, &v.ProviderCode, &v.ProviderRef, &v.Segments, &v.Attempts,
			&v.CreatedAt, &v.UpdatedAt, &subjectKind, &subjectName, &v.ServiceState, &v.EscalationStep,
			&v.RecipientDisplayName}
		var incID, incRef *string
		if withIncident {
			dest = append(dest, &incID, &incRef)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan phone delivery: %w", err)
		}
		if incID != nil {
			v.IncidentID = *incID
		}
		if incRef != nil {
			v.IncidentRef = *incRef
		}
		if v.Notes == nil {
			v.Notes = []string{}
		}
		if subjectKind != nil && subjectName != nil {
			v.Subject = &domain.PhoneDeliverySubject{Kind: *subjectKind, Name: *subjectName}
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate phone deliveries: %w", err)
	}
	return out, nil
}
