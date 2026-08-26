package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.1 — the delegation lookup, which is the only thing that can silence a monitor.
//
// Two design rounds shaped this function, and every clause below exists because its absence loses a
// page:
//
//   - `owns_paging` alone silences NOTHING. Coverage must be ARMED per SIGNAL: a policy that can page,
//     a QUOTABLE last verdict, the CURRENT generations and effective revision, a FRESH DB-clock lease,
//     and a resolvable recipient.
//   - a HOLD is a SUCCESSFUL evaluation that cannot fire, so it dis-arms burn: a rule sitting at CLEAR
//     with a held window is not a replacement for a member's burn alert.
//   - ROUTABILITY is checked HERE rather than cached, because schedule membership and channel
//     enablement change without touching any column on the service.
//   - membership is the CURRENT EFFECTIVE definition revision, never `service_member_refs` — those are
//     rewritten when a declaration is AUTHORED, while it becomes effective on its bucket boundary, so
//     a monitor added at 12:00:30 for a 12:01 revision would otherwise be muted at 12:00:40 by a
//     service still measuring the old definition.
//
// Everything ambiguous FAILS OPEN: the caller pages. A page that was not needed is noise; a page that
// was owed and never sent is the failure this feature exists to prevent.

// DelegationSignal is which replacement a monitor's alert would be delegated to.
type DelegationSignal string

const (
	// DelegationLive covers DOWN transitions (reminders included) and escalation steps.
	DelegationLive DelegationSignal = "live"
	// DelegationBurn covers FIRING burn alerts.
	DelegationBurn DelegationSignal = "burn"
)

// DelegationOwner is one service that is ACTIVELY covering a signal for a monitor.
type DelegationOwner struct {
	ServiceID string
	Slug      string
	Name      string
}

// DelegationVerdict is the answer for one (monitor, signal).
type DelegationVerdict struct {
	// Owners is non-empty only when suppression is warranted. Ordered by slug so the note text and
	// the tests are stable.
	Owners []DelegationOwner
	// FailOpenReason is set when the lookup could not conclude that a replacement is active. It is
	// the metric label and the UI's degradation reason, and it is never empty when Owners is empty:
	// "why is this monitor still paging" must be answerable.
	FailOpenReason string
}

// Suppress reports whether the caller may withhold delivery.
func (v DelegationVerdict) Suppress() bool { return len(v.Owners) > 0 }

// ActiveDelegation resolves whether ANY service actively covers `signal` for this monitor.
//
// The query answers all of arming in one statement, because a multi-step lookup would leave windows
// in which a service is armed for one clause and not another. It is deliberately NOT cached: a stale
// "no owner" pages twice and a stale "owner" pages never.
func (s *Store) ActiveDelegation(
	ctx context.Context, monitorID, projectID string, signal DelegationSignal,
) (DelegationVerdict, error) {
	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	var err error

	switch signal {
	case DelegationLive:
		rows, err = s.pool.Query(ctx, activeLiveDelegationSQL, monitorID, projectID)
	case DelegationBurn:
		rows, err = s.pool.Query(ctx, activeBurnDelegationSQL, monitorID, projectID)
	default:
		return DelegationVerdict{}, fmt.Errorf("store: unknown delegation signal %q", signal)
	}
	if err != nil {
		// The caller treats an error as fail-open; returning it lets the caller name the reason and
		// count it rather than guessing here.
		return DelegationVerdict{}, fmt.Errorf("store: active delegation: %w", err)
	}
	defer rows.Close()

	var out DelegationVerdict
	for rows.Next() {
		var o DelegationOwner
		if err := rows.Scan(&o.ServiceID, &o.Slug, &o.Name); err != nil {
			return DelegationVerdict{}, fmt.Errorf("store: scan delegation owner: %w", err)
		}
		out.Owners = append(out.Owners, o)
	}
	if err := rows.Err(); err != nil {
		return DelegationVerdict{}, fmt.Errorf("store: iterate delegation owners: %w", err)
	}
	if len(out.Owners) == 0 {
		out.FailOpenReason = "no_active_owner"
	}
	return out, nil
}

// The routable predicate, shared by both signals. Both halves are live reads: a channel disabled after
// arming must dis-arm immediately, which a generation stamped on the service cannot see.
//
// A schedule arms only if one of its participants is STILL a live channel of this project.
// `jsonb_array_length(participants) > 0` was the old test, and it counts ids that nothing prunes:
// deleting a notification channel removes its row and leaves the id in every schedule that named it,
// and disabling one changes no JSON at all. A service could therefore suppress its members' alerts on
// the strength of a rotation resolving to a deleted channel, while its own page went nowhere — both
// paths silent, which is the single outcome §16.1 exists to prevent.
//
// It is deliberately no MORE permissive than `resolveServiceRecipientsTx`: this asks for a live
// participant, and the resolver falls back to the project's enabled channels when the current
// rotation names a dead one (§16.6's "a deleted or empty target falls back"). So anything armed here
// has somewhere to land.
const routablePredicate = `(
		    EXISTS (SELECT 1 FROM oncall_schedules os
		             JOIN notification_channels pc
		               ON pc.project_id = os.project_id AND pc.enabled
		              AND os.participants @> to_jsonb(pc.id::text)
		            WHERE os.id = s.oncall_schedule_id AND os.project_id = s.project_id)
		    OR EXISTS (SELECT 1 FROM notification_channels nc
		                WHERE nc.project_id = s.project_id AND nc.enabled)
		)`

// routableClause is that predicate as a WHERE conjunct. Both spellings exist so the badge read
// (`ServiceAlertingState`) and this lookup share the PREDICATE TEXT itself rather than two copies of
// it: a badge that decided "routable" its own way is exactly how a UI comes to say ARMED while
// delivery says otherwise.
const routableClause = `
		AND ` + routablePredicate

// §16.1's live policy clause, as TWO questions, because they fail for different reasons and an
// operator needs the difference.
//
// The old spelling was `cardinality(page_on) > 0 OR page_on_unknown` — "this policy pages SOMETHING".
// §16.1 asks whether it can page the CURRENT state, and the gap between those is a lost page: a
// service observed DEGRADED with `page_on = {down}` announces nothing at all, while its member's DOWN
// alert was suppressed on the strength of a policy that does not cover what is actually happening.
//
// The second question is newer and exists because of D-0176. A withheld onset leaves the service
// pageable-in-principle and silent in fact, so restoring the route would arm delegation in the
// instant BEFORE the next evaluation announces anything — the member falls silent first and the
// service speaks second. Coverage for a pageable state therefore requires the announcement to have
// been COMMITTED: `live_firing` set, and `emitted_state` equal to what is being observed now.
//
// `healthy`/`excluded` cover without either: there is nothing service-level to announce, so there is
// nothing a member's alert would be replacing.
// The policy must page SOMETHING at all — `page_on = {}` with `page_on_unknown` off is legal and
// replaces nothing, whatever the service is doing — AND it must cover the state the service is IN.
// Both halves are needed: dropping the first would arm a page-for-nothing policy while the service
// happens to be healthy, and a member going down would then be suppressed in the window before the
// service's own verdict caught up.
const livePageableStateSQL = `(
		    (cardinality(s.page_on) > 0 OR s.page_on_unknown)
		    AND (
		        st.observed_state IN ('healthy', 'excluded')
		        OR st.observed_state = ANY(s.page_on)
		        OR (st.observed_state = 'unknown' AND s.page_on_unknown)
		    )
		)`

const liveOnsetCommittedSQL = `(
		    st.observed_state IN ('healthy', 'excluded')
		    OR (st.live_firing AND st.emitted_state = st.observed_state)
		)`

// THE definition of "the revision that governs right now", written once and shared by every arming
// clause. Two spellings of this question is how a membership check and a revision stamp come to
// disagree about which declaration is in force, so there is only one. The ordering matches the epoch
// resolver (`serviceepochs.go`): the latest boundary that has passed, and the highest revision when
// two share it.
const effectiveRevisionSQL = `
		    SELECT r.id
		      FROM service_definition_revisions r
		     WHERE r.service_id = s.id AND r.state = 'effective' AND r.effective_at <= now()
		     ORDER BY r.effective_at DESC, r.revision DESC
		     LIMIT 1`

// The effective-membership clause. `service_definition_revisions` is the declaration axis; the
// EFFECTIVE one is the latest revision whose boundary has passed, and its members are the SLI of that
// revision. Anything else would suppress on an authored-but-not-yet-effective declaration.
const effectiveSLIClause = `
		AND EXISTS (
		    SELECT 1 FROM service_definition_members m
		     WHERE m.revision_id = (` + effectiveRevisionSQL + `)
		       AND m.monitor_id = $1 AND m.role = 'sli'
		)`

// The revision half of the LIVE arming conjunction (§16.1): the successful evaluation must be OF the
// declaration that governs now, not merely fresh under the current config generation. Membership
// alone is not that check — it asks whether the monitor is an SLI of the current revision, while this
// asks whether the SERVICE'S VERDICT was computed from it. Without it a service still measuring the
// PREVIOUS definition suppresses a member of the NEW one it has never looked at, which is a page
// nobody sends. `revision_id IS NULL` (a pre-stamp row, or an evaluation that found no governing
// declaration) dis-arms, because absence of evidence is not coverage.
const evaluatedCurrentRevisionClause = `
		AND st.revision_id IS NOT NULL
		AND st.revision_id = (` + effectiveRevisionSQL + `)`

// LIVE coverage: a policy that can page something, a fresh successful evaluation of the CURRENT
// generation and effective revision, no evaluation error, and a route.
var activeLiveDelegationSQL = `
	SELECT s.id, s.slug, s.name
	  FROM services s
	  JOIN service_alert_state st ON st.service_id = s.id AND st.project_id = s.project_id
	 WHERE s.project_id = $2
	   AND s.owns_paging
	   AND ` + livePageableStateSQL + `
	   AND ` + liveOnsetCommittedSQL + `
	   AND st.config_generation = s.alert_config_generation
	   AND st.last_error IS NULL
	   AND now() < st.lease_until` +
	evaluatedCurrentRevisionClause + effectiveSLIClause + routableClause + `
	 ORDER BY s.slug`

// The predicate ONE burn rule's latch must satisfy to be a replacement: quotable (a HOLD is a
// successful evaluation that cannot fire), for the current target and config generations, error-free
// and fresh. Written once because it is asked twice below, in opposite directions.
const burnRuleCoversSQL = `
		           bs.last_verdict IN ('fire', 'clear')
		       AND bs.config_generation = s.alert_config_generation
		       AND bs.target_generation = t.alert_generation
		       AND bs.last_error IS NULL
		       AND now() < bs.lease_until`

// BURN coverage: §16.1's "**Any HOLD dis-arms BURN coverage**", which is a claim about the SERVICE's
// coverage and not about one row of it.
//
// The two halves are deliberate. The first says a replacement EXISTS at all; the second says NOTHING
// the service owns is blind. They differ the moment a service owns more than one rule — `burn_rules`
// is an array of up to four, and 00077 keys service targets by (service_id, window_name) so one
// service may hold several enabled targets — and a single JOIN over the rows answers only the first
// question. It would arm on "some rule is quotable" while a sibling rule sits at HOLD, which is the
// P0 §16.1 was written against, one level down: the member's own burn alert is muted by a
// replacement that is only partly able to speak.
//
// An enabled target with NO latch row at all (never evaluated, or no rules) also dis-arms: absence of
// evidence is not coverage. The consequence is a monitor burn page that may be redundant; the
// alternative is a burn page that never happens, and §16.1 does not trade the second for the first.
// burnReplacementExistsSQL is the FIRST half of burn coverage as a standalone predicate over `s`:
// a replacement exists at all. Named so the badge read can ask the same question in the same words.
const burnReplacementExistsSQL = `EXISTS (
	       SELECT 1
	         FROM sla_targets t
	         JOIN service_burn_alert_state bs
	           ON bs.service_id = s.id AND bs.project_id = s.project_id AND bs.sla_target_id = t.id
	        WHERE t.service_id = s.id AND t.burn_alert_enabled
	          AND (` + burnRuleCoversSQL + `))`

// burnNothingBlindSQL is the SECOND half: nothing the service owns is unable to speak. It walks the
// rows that EXIST, and an enabled target with no row at all fails it through the LEFT JOIN.
const burnNothingBlindSQL = `NOT EXISTS (
	       SELECT 1
	         FROM sla_targets t
	         LEFT JOIN service_burn_alert_state bs
	           ON bs.service_id = s.id AND bs.project_id = s.project_id AND bs.sla_target_id = t.id
	        WHERE t.service_id = s.id AND t.burn_alert_enabled
	          AND (bs.sla_target_id IS NULL OR NOT (` + burnRuleCoversSQL + `)))`

// burnEveryRuleLatchedSQL is the THIRD: a latch for EVERY declared rule, not merely for the rules
// that happen to have one. The clause above cannot see a rule declared with no row at all — adding a
// second rule to an armed target left coverage armed while the new rule had never been evaluated,
// and deleting one of two latch rows did the same. Cardinality is the honest test because the write
// path rejects duplicate canonical keys and prunes the rows of rules that no longer exist, so a
// declared rule and its latch row are one to one. Counting is also the only spelling that does not
// re-implement the canonical key in SQL — a second owner of that format is how a latch and a page
// come to disagree about identity.
const burnEveryRuleLatchedSQL = `NOT EXISTS (
	       SELECT 1
	         FROM sla_targets t
	        WHERE t.service_id = s.id AND t.burn_alert_enabled
	          AND (SELECT count(*)
	                 FROM service_burn_alert_state bs
	                WHERE bs.service_id = s.id AND bs.project_id = s.project_id
	                  AND bs.sla_target_id = t.id) <> jsonb_array_length(t.burn_rules))`

var activeBurnDelegationSQL = `
	SELECT s.id, s.slug, s.name
	  FROM services s
	 WHERE s.project_id = $2
	   AND s.owns_paging
	   AND ` + burnReplacementExistsSQL + `
	   AND ` + burnNothingBlindSQL + `
	   AND ` + burnEveryRuleLatchedSQL +
	effectiveSLIClause + routableClause + `
	 ORDER BY s.slug`

// RecordSuppression writes the visibility half of §16.1 and annotates the monitor's open incident, in
// ONE scoped operation — the same operation that resolved the owners, so a suppression cannot be
// counted without also being explainable.
//
// The row key is `(outbox_event_id, service_id, topic)`: the outbox is at-least-once, so a redelivery
// re-runs this and the key is what keeps an audit-shaped table from growing a duplicate per retry.
// Any error here FAILS OPEN — the caller delivers — because a suppression nobody can see is worse
// than a duplicate page.
func (s *Store) RecordSuppression(
	ctx context.Context, eventID, monitorID, projectID, topic string, owners []DelegationOwner,
) error {
	if len(owners) == 0 {
		return fmt.Errorf("store: record suppression with no owners")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin record suppression: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	names := make([]string, 0, len(owners))
	for _, o := range owners {
		names = append(names, o.Name)
		if _, err := tx.Exec(ctx, `
			INSERT INTO alert_suppressions (outbox_event_id, monitor_id, project_id, service_id, topic, reason)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (outbox_event_id, service_id, topic) DO NOTHING`,
			eventID, monitorID, projectID, o.ServiceID, topic, string(domain.SuppressionServiceDelegation),
		); err != nil {
			return fmt.Errorf("store: record suppression row: %w", err)
		}
	}
	// The note is idempotent on its marker prefix plus the OWNER SET: a changed set of covering
	// services is a different statement and deserves its own line, while a redelivery of the same
	// one adds nothing.
	if _, err := appendSuppressionNoteTx(ctx, tx, monitorID, names); err != nil {
		return fmt.Errorf("store: annotate suppression: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit record suppression: %w", err)
	}
	return nil
}

// appendSuppressionNoteTx is the delegation flavour of the dependency note, inside the caller's
// transaction so the row and the explanation cannot diverge.
//
// Its idempotency key is the marker prefix PLUS the rendered owner set: a redelivery of the same
// suppression adds nothing, while a CHANGED set of covering services is a different statement about
// who is answering and earns its own line.
func appendSuppressionNoteTx(
	ctx context.Context, tx pgx.Tx, monitorID string, ownerNames []string,
) (bool, error) {
	sorted := append([]string(nil), ownerNames...)
	sort.Strings(sorted)
	body := domain.SuppressionMarker + " notification delegated to " +
		strings.Join(sorted, ", ") + ", which owns paging for this monitor."
	ct, err := tx.Exec(ctx, `
		INSERT INTO incident_updates (incident_id, status, body, author)
		SELECT i.id, i.status, $2, 'system'
		  FROM incidents i
		 WHERE i.monitor_id = $1 AND i.source = 'auto' AND i.status <> 'resolved'
		   AND NOT EXISTS (
		       SELECT 1 FROM incident_updates u
		        WHERE u.incident_id = i.id AND u.author = 'system' AND u.body = $2
		   )
		 ORDER BY i.started_at DESC
		 LIMIT 1`, monitorID, body)
	if err != nil {
		return false, fmt.Errorf("store: append delegation note: %w", err)
	}
	// No open incident is a VALID outcome, not a failure: a monitor can be suppressed while its
	// incident has already resolved, and inventing one would be worse than saying nothing.
	return ct.RowsAffected() == 1, nil
}

// ServiceAlertSequence returns the CURRENT sequence for the latch the given alert belongs to, which
// delivery compares against the sequence stamped into that payload (§16.5).
//
// It takes the WHOLE payload rather than a few of its fields on purpose. The burn latch's identity is
// the tuple (service, project, target, rule) — 00077 keys service targets by (service_id,
// window_name), so one service may hold a 7d and a 30d target whose rules share a canonical key, and
// a lookup missing the target silently reads whichever of the two rows Postgres returns first. That
// is not a wrong number in a log line: it is one target's onset being dropped as superseded by the
// other target's sequence. Passing the payload makes an under-scoped call unwritable.
//
// `ErrNotFound` means the service, its target or its rule is gone — the caller decides what that
// implies, and the answers differ: an ONSET for a vanished service has nothing to announce, while a
// CLOSE must still reach the people who heard the onset, which is exactly why the episode outlives
// the service, the target and the rule.
func (s *Store) ServiceAlertSequence(ctx context.Context, a domain.ServiceAlert) (int64, error) {
	var seq int64
	var err error
	if a.Signal == domain.ServiceSignalBurn {
		// A burn payload without its identity cannot be fenced, and delivering it unfenced would
		// hand somebody a page whose ordering nobody checked. Fail loudly instead: the outbox
		// retries and dead-letters, which is visible, unlike a silently unfenced delivery.
		if a.SLATargetID == "" || a.RuleKey == "" {
			return 0, fmt.Errorf(
				"store: burn alert sequence: payload carries no target/rule identity (service %s)",
				a.ServiceID)
		}
		err = s.pool.QueryRow(ctx,
			`SELECT emitted_seq FROM service_burn_alert_state
			  WHERE service_id = $1 AND project_id = $2 AND sla_target_id = $3 AND rule_key = $4`,
			a.ServiceID, a.ProjectID, a.SLATargetID, a.RuleKey).Scan(&seq)
	} else {
		err = s.pool.QueryRow(ctx,
			`SELECT emitted_seq FROM service_alert_state WHERE service_id = $1 AND project_id = $2`,
			a.ServiceID, a.ProjectID).Scan(&seq)
	}
	if noRows(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: service alert sequence: %w", err)
	}
	return seq, nil
}

// MonitorDelegation is what BOTH signals conclude for one monitor, which is what a monitor's own
// page has to show: FR-021's approved design keeps a delegated monitor at full strength — its real
// status pill, plus a chip naming who pages instead of it — and that sentence needs the owner and
// the per-signal answer, not a boolean.
type MonitorDelegation struct {
	Live DelegationVerdict
	Burn DelegationVerdict
}

// MonitorDelegation resolves both signals through the SAME lookup delivery uses.
//
// Two queries rather than one on purpose: the two conjunctions are genuinely different (a service
// can replace DOWN transitions while its budget signal is held), and merging them into one statement
// would mean a third spelling of arming — which is precisely how a page and a badge come to
// disagree.
func (s *Store) MonitorDelegation(
	ctx context.Context, monitorID, projectID string,
) (MonitorDelegation, error) {
	var out MonitorDelegation
	var err error
	if out.Live, err = s.ActiveDelegation(ctx, monitorID, projectID, DelegationLive); err != nil {
		return MonitorDelegation{}, err
	}
	if out.Burn, err = s.ActiveDelegation(ctx, monitorID, projectID, DelegationBurn); err != nil {
		return MonitorDelegation{}, err
	}
	return out, nil
}

// IncidentEventSequence returns an incident's CURRENT lifecycle sequence, which delivery compares
// against the one stamped into an `incident_event` payload (D-0177).
//
// `ErrNotFound` means the incident is gone. The caller decides what that implies, and the answers
// differ by direction exactly as they do for a service alert: a resolution still deserves delivery,
// an opening for a vanished incident does not.
func (s *Store) IncidentEventSequence(ctx context.Context, incidentID string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT event_seq FROM incidents WHERE id = $1`, incidentID).Scan(&seq)
	if noRows(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: incident event sequence: %w", err)
	}
	return seq, nil
}
