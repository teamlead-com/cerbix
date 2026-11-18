package store

import (
	"context"
	"fmt"
	"time"
)

// What the outbox's phone path reads beside an event's payload (FR-041a D16, D32, R15): the incident a
// phone alert links to. Which phone channels an event has comes from the text half's own read
// (domain.PhoneChannelsSeen); only when that read failed does the outbox ask here for them.

// PhoneLink is the incident a phone alert links to: its id and its short reference (D21). The zero
// value is "no incident".
type PhoneLink struct {
	IncidentID string
	Ref        string
}

// MonitorPhoneChannelIDs returns the enabled sms and voice channels linked to a monitor, newest first
// as the text path lists them. It is the outbox's fallback when the text half's own read of the
// monitor's channels failed (Task 12 re-review N-1): it opens no channel config, so one Slack or
// webhook config that cannot be decrypted does not hide the phone channels beside it.
func (s *Store) MonitorPhoneChannelIDs(ctx context.Context, monitorID string) ([]string, error) {
	return s.phoneChannelIDs(ctx, "monitor", `SELECT c.id::text FROM notification_channels c
		  JOIN monitor_notifications mn ON mn.channel_id = c.id
		 WHERE mn.monitor_id = $1 AND c.enabled AND c.type IN ('sms', 'voice')
		 ORDER BY c.created_at DESC, c.id`, monitorID)
}

// PhoneChannelIDs returns the enabled sms and voice channels among ids, in the order given: the same
// fallback for a service alert's snapshot and an escalation step's channels.
func (s *Store) PhoneChannelIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.phoneChannelIDs(ctx, "snapshot", `SELECT c.id::text FROM notification_channels c
		 WHERE c.id = ANY($1::uuid[]) AND c.enabled AND c.type IN ('sms', 'voice')
		 ORDER BY array_position($1::uuid[], c.id)`, ids)
}

func (s *Store) phoneChannelIDs(ctx context.Context, of, query string, arg any) ([]string, error) {
	rows, err := s.pool.Query(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("store: %s phone channels: %w", of, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan %s phone channel: %w", of, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate %s phone channels: %w", of, err)
	}
	return out, nil
}

// MonitorPhoneLink is the incident a monitor's phone alert links to (D32): the monitor's OPEN
// auto-incident; failing that, when resolvedSince is not zero (a recovery), its latest auto-incident
// resolved at or after resolvedSince — the outbox event's `created_at`, never the payload's time,
// which no writer sets. An older outage, a manual incident and another monitor's are never linked.
// The zero PhoneLink when there is none.
func (s *Store) MonitorPhoneLink(ctx context.Context, monitorID string, resolvedSince time.Time) (PhoneLink, error) {
	return s.autoIncidentLink(ctx, "monitor_id", monitorID, resolvedSince)
}

// ServicePhoneLink is the incident a service alert's phone delivery links to (R15): the service's open
// auto-incident, else its latest auto-incident resolved at or after resolvedSince, else none.
func (s *Store) ServicePhoneLink(ctx context.Context, serviceID string, resolvedSince time.Time) (PhoneLink, error) {
	return s.autoIncidentLink(ctx, "service_id", serviceID, resolvedSince)
}

// autoIncidentLink is both links: subject is the column that anchors the incident to its subject, a
// constant of this file and never input. An open incident wins over any resolved one; among resolved
// ones the latest resolution does. "Open auto-incident" is the shared text of
// openMonitorAutoIncidentFrom (autoIncidentSQL, incidentUnresolvedSQL), extended by D32's recovery
// rule: or resolved at or after resolvedSince.
func (s *Store) autoIncidentLink(ctx context.Context, subject, id string, resolvedSince time.Time) (PhoneLink, error) {
	var since *time.Time
	if !resolvedSince.IsZero() {
		since = &resolvedSince
	}
	var l PhoneLink
	err := s.pool.QueryRow(ctx, `SELECT id::text, ref FROM incidents
		 WHERE `+subject+` = $1 AND `+autoIncidentSQL+`
		   AND (`+incidentUnresolvedSQL+` OR ($2::timestamptz IS NOT NULL AND resolved_at >= $2::timestamptz))
		 ORDER BY (`+incidentUnresolvedSQL+`) DESC, resolved_at DESC NULLS LAST, started_at DESC
		 LIMIT 1`, id, since).Scan(&l.IncidentID, &l.Ref)
	if noRows(err) {
		return PhoneLink{}, nil
	}
	if err != nil {
		return PhoneLink{}, fmt.Errorf("store: phone incident link: %w", err)
	}
	return l, nil
}
