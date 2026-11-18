package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// D31 rule 1 — "deliverable by configuration" — as ONE SQL fragment. A phone channel is a route only
// when every clause below holds; the first clause that fails is the reason it is not (R11). The
// fragment reads typed columns and the instance's typed telephony columns only (D36), so every
// predicate that embeds it stays SQL. A voice channel's clause reads telephony_can_place_calls — D3's
// one condition "the provider can place calls" (not generic_http, not twilio with a Messaging Service
// SID as `from`), generated beside telephony_provider (migration 00120).
//
// The channel DTO's `deliverable` (scanChannel) reads phoneRouteReasonSQL; the route predicates —
// routablePredicate, resolveServiceRecipientsTx, step resolution and the send-time check — embed
// phoneRouteSQL (FR-041a Task 9). One text for all of them, so the DTO never says "route" about a
// channel a predicate refuses, or the other way round.
//
// For the four other channel types the reason is NULL and phoneRouteSQL is TRUE: they are judged
// exactly as today wherever the fragment is added (P14) — a disabled Slack channel on an escalation
// step is still what it was.

// phoneRouteReasonTemplate is the reason expression over a channel row aliased {c}. The order of the
// clauses is the order a person fixes them in: who is phoned, whether the channel is on, whether
// telephony is in effect, whether the person can still be phoned.
const phoneRouteReasonTemplate = `(CASE
	WHEN {c}.type NOT IN ('sms', 'voice') THEN NULL
	WHEN {c}.recipient = 'user' AND {c}.recipient_user_id IS NULL THEN 'recipient_deleted'
	WHEN {c}.recipient = 'number' AND {c}.number_verified_at IS NULL THEN 'number_unverified'
	WHEN NOT {c}.enabled THEN 'disabled'
	WHEN NOT COALESCE((SELECT {c}_ts.telephony_enabled FROM instance_settings {c}_ts WHERE {c}_ts.id), false)
		THEN 'telephony_disabled'
	WHEN {c}.type = 'voice'
	 AND NOT COALESCE((SELECT {c}_ts.telephony_can_place_calls FROM instance_settings {c}_ts WHERE {c}_ts.id), true)
		THEN 'voice_unsupported_by_provider'
	WHEN {c}.recipient = 'user' AND NOT ` + phoneRecipientEligibleTemplate + ` THEN 'recipient_not_eligible'
	WHEN {c}.recipient = 'user' AND NOT EXISTS (
		SELECT 1 FROM user_phones {c}_up
		 WHERE {c}_up.user_id = {c}.recipient_user_id AND {c}_up.verified_at IS NOT NULL)
		THEN 'no_verified_number'
	ELSE NULL
END)`

// phoneRecipientEligibleTemplate is §4.3's eligible recipient of the channel {c}'s project, for the
// user {u}: a human who reaches the project through a project membership, an organization-level
// membership of its organization, or global admin. Any role counts — access, not permission, makes
// a person reachable.
const phoneRecipientEligibleTemplate = `EXISTS (
		SELECT 1 FROM users {c}_u JOIN projects {c}_p ON {c}_p.id = {c}.project_id
		 WHERE {c}_u.id = {u} AND {c}_u.kind = 'human'
		   AND ({c}_u.is_global_admin OR EXISTS (
		        SELECT 1 FROM memberships {c}_m
		         WHERE {c}_m.user_id = {c}_u.id AND {c}_m.org_id = {c}_p.org_id
		           AND ({c}_m.project_id IS NULL OR {c}_m.project_id = {c}_p.id))))`

// phoneRouteReasonSQL is the reason a channel row aliased alias is not a route, NULL when it is one
// or is not a phone channel. alias must be a plain SQL identifier.
func phoneRouteReasonSQL(alias string) string {
	return strings.ReplaceAll(strings.ReplaceAll(phoneRouteReasonTemplate, "{u}", "{c}.recipient_user_id"), "{c}", alias)
}

// phoneRouteSQL is TRUE when a channel row aliased alias is a route as far as D31 rule 1 goes: every
// non-phone channel, and a phone channel with no reason against it.
func phoneRouteSQL(alias string) string {
	return "(" + phoneRouteReasonSQL(alias) + " IS NULL)"
}

// phoneRecipientEligibleSQL is the eligibility clause of a `user` channel row aliased alias for the
// user id bound at userParam (a placeholder such as "$2"): the check of a create or a retarget, made
// against the row before it names the user.
func phoneRecipientEligibleSQL(alias, userParam string) string {
	return strings.ReplaceAll(strings.ReplaceAll(phoneRecipientEligibleTemplate, "{u}", userParam), "{c}", alias)
}

// PhoneChannelsOnlyRouteFor answers §4.3's `only_route_for` for each sms and voice channel among ids:
// the paging services (owns_paging) of the channel's project that are routable now and would not be
// routable without the channel — routablePredicate and routablePredicateWithout, the route predicate
// of D31 itself, so the warning and the routing cannot disagree. At most maxServicesWithoutRoute per
// channel, ordered by project name then slug like the 409 of a telephony save (R10) — in byte order,
// COLLATE "C", as that 409 sorts in Go, whatever the database's collation — with the total.
// Every phone channel among ids has an entry, an empty one when it is nobody's only route (a channel
// that is not a route itself is never one); the four other types and unknown ids have none.
func (s *Store) PhoneChannelsOnlyRouteFor(ctx context.Context, ids []string) (map[string]domain.OnlyRouteFor, error) {
	out := map[string]domain.OnlyRouteFor{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text FROM notification_channels c
		 WHERE c.id = ANY($1::uuid[]) AND c.type IN ('sms', 'voice')`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: phone channels for only_route_for: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan phone channel for only_route_for: %w", err)
		}
		out[id] = domain.OnlyRouteFor{Services: []domain.ServiceWithoutRoute{}}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: phone channels for only_route_for: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	rows, err = s.pool.Query(ctx, `
		SELECT channel_id, project, service, slug, total FROM (
		    SELECT c.id::text AS channel_id, p.name AS project, s.name AS service, s.slug AS slug,
		           count(*) OVER (PARTITION BY c.id) AS total,
		           row_number() OVER (PARTITION BY c.id ORDER BY p.name COLLATE "C", s.slug COLLATE "C", s.id) AS n
		      FROM notification_channels c
		      JOIN services s ON s.project_id = c.project_id
		      JOIN projects p ON p.id = s.project_id
		     WHERE c.id = ANY($1::uuid[]) AND c.type IN ('sms', 'voice')
		       AND s.owns_paging
		       AND `+routablePredicate+`
		       AND NOT `+routablePredicateWithout("c.id")+`
		) x
		 WHERE n <= $2
		 ORDER BY channel_id, n`, ids, maxServicesWithoutRoute)
	if err != nil {
		return nil, fmt.Errorf("store: only_route_for: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var svc domain.ServiceWithoutRoute
		var total int
		if err := rows.Scan(&id, &svc.Project, &svc.Service, &svc.Slug, &total); err != nil {
			return nil, fmt.Errorf("store: scan only_route_for: %w", err)
		}
		o := out[id]
		o.Services, o.Total = append(o.Services, svc), total
		out[id] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: only_route_for: %w", err)
	}
	return out, nil
}
