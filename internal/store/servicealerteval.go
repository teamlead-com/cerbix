package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.3 / §16.7 — the LIVE evaluator.
//
// One BOUNDED SLICE per call, never "every alerting service in one transaction": per-project caps do
// not bound an installation, and a single global snapshot can monopolize the leader's connection and
// starve dispatch. The slice is keyset-ordered by the freshness the arming rule reads, so the service
// that has been un-evaluated longest is the one evaluated next and nothing starves.
//
// The evaluation itself calls `servicePageProjectionsTx` — the SAME path the public status page
// uses. That is not a convenience: invariant 81 requires the pager and the page to agree about what
// `down` means at one instant, and the only way to guarantee that is to run one implementation.

// ServiceAlertSliceCap bounds one evaluation slice. Fixed rather than configurable: a knob here
// trades a bound the operator cannot reason about against a cost they cannot see.
const ServiceAlertSliceCap = 50

// serviceAlertLeaseMultiplier derives `lease_until` from the cadence. Three, matching the freshness
// default the evaluator already applies to members: a signal is stale once it has missed roughly
// three chances to speak.
const serviceAlertLeaseMultiplier = 3

// ServiceAlertEvaluation is what one slice did, for the leader's telemetry.
type ServiceAlertEvaluation struct {
	Evaluated int
	Onsets    int
	Closes    int
	Errors    int
	// IncidentsOpened / IncidentsResolved count the FR-022 incidents this slice opened and resolved. They
	// are separate from Onsets/Closes on purpose: an onset whose service already had an open auto-incident
	// announces but opens nothing, and the difference between the two numbers is exactly the flapping the
	// per-service unique index absorbs.
	IncidentsOpened   int
	IncidentsResolved int
	// Withheld counts ONSETS this pass refused to announce, BY REASON. Two reasons exist and they are
	// different problems with different owners: `unroutable` says somebody's paging configuration is
	// broken right now, `no_governing_revision` says the declaration has not taken effect yet. A
	// single number would have reported the second as the first, which is what the first version of
	// this counter did.
	Withheld map[string]int
	// Lag is how far behind the oldest evaluated service was, so a stalled evaluator reads as lag
	// rather than as an absence of alerts — which is indistinguishable from "nothing is wrong".
	Lag time.Duration
}

// EvaluateServiceAlerts evaluates ONE slice of alerting services and emits the edges it finds.
//
// The state write and the outbox row commit in ONE transaction (invariant 80), so a delivered alert
// can never be forgotten and a forgotten one can never be delivered twice. Everything else about
// delivery is at-least-once and the payload's sequence is what keeps a duplicate from being a lie.
// The bounded vocabulary of reasons an onset is withheld (D-0176). Two values, fixed: this labels a
// metric, and a label that can grow with the data is a metric endpoint anybody can inflate.
const (
	// WithheldUnroutable: nothing could receive the announcement.
	WithheldUnroutable = "unroutable"
	// WithheldNoGoverningRevision: no declaration governs the service yet, so the verdict was
	// computed from nothing.
	WithheldNoGoverningRevision = "no_governing_revision"
)

func (s *Store) evaluateServiceAlertsOn(
	ctx context.Context, db alertConn, cadence time.Duration,
) (ServiceAlertEvaluation, error) {
	out := ServiceAlertEvaluation{Withheld: map[string]int{}}

	// A READ-WRITE snapshot: this path both evaluates (which the read-only report snapshot was built
	// for) and writes its verdicts, and the two must see one instant.
	tx, asOf, err := s.beginAlertSnapshot(ctx, db)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// (1) The slice: alerting services ordered by how long they have gone un-evaluated. A service
	// with no state row at all sorts first — it has never been evaluated, and until it has, its
	// members are paging for themselves.
	type candidate struct {
		serviceID, projectID, name, slug string
		policy                           domain.ServiceAlertPolicy
		configGeneration                 int64
		// revisionID is the declaration this verdict was computed FROM. Arming compares it against
		// the revision governing at delivery (§16.1), so a service still measuring the previous
		// definition dis-arms instead of suppressing a member it has never looked at.
		revisionID *string
		scheduleID string
		// The stored state, or the zero values for a service that has never been evaluated.
		candidateState domain.ServiceAlertState
		streak         int
		firing         bool
		emitted        domain.ServiceAlertState
		emittedSeq     int64
		// undeliveredSeq is the sequence of an announcement the worker CONDEMNED: attempted and
		// reached nobody, with no retry owed. Equal to `emittedSeq` means the current announcement
		// is known dead (D-0187).
		undeliveredSeq int64
		evaluatedAt    *time.Time
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.project_id, s.name, s.slug,
		       s.page_on, s.page_on_unknown, s.confirm_evaluations, s.alert_config_generation,
		       COALESCE(s.oncall_schedule_id::text, ''), rev.id::text,
		       st.candidate_state, st.streak, st.live_firing, st.emitted_state, st.emitted_seq,
		       st.evaluated_at, COALESCE(st.undelivered_seq, 0)
		  FROM services s
		  LEFT JOIN service_alert_state st ON st.service_id = s.id
		  -- The declaration governing THIS snapshot, resolved the one way the epoch owner resolves
		  -- it. NULL means no declaration governs yet, which the arming rule reads as dis-armed.
		  LEFT JOIN LATERAL (
		      SELECT r.id
		        FROM service_definition_revisions r
		       WHERE r.service_id = s.id AND r.state = 'effective' AND r.effective_at <= $2
		       ORDER BY r.effective_at DESC, r.revision DESC
		       LIMIT 1
		  ) rev ON true
		 WHERE s.owns_paging
		 ORDER BY st.evaluated_at ASC NULLS FIRST, s.id
		 LIMIT $1`, ServiceAlertSliceCap, asOf)
	if err != nil {
		return out, fmt.Errorf("store: service alert slice: %w", err)
	}
	var slice []candidate
	for rows.Next() {
		var c candidate
		var pageOn []string
		var candState, emitted *string
		var streak *int
		var firing *bool
		var seq *int64
		if err := rows.Scan(&c.serviceID, &c.projectID, &c.name, &c.slug,
			&pageOn, &c.policy.PageOnUnknown, &c.policy.ConfirmEvaluations, &c.configGeneration,
			&c.scheduleID, &c.revisionID,
			&candState, &streak, &firing, &emitted, &seq, &c.evaluatedAt,
			&c.undeliveredSeq); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: scan alert candidate: %w", err)
		}
		c.policy.OwnsPaging = true
		for _, p := range pageOn {
			c.policy.PageOn = append(c.policy.PageOn, domain.ServiceAlertState(p))
		}
		if candState != nil {
			c.candidateState = domain.ServiceAlertState(*candState)
		}
		if streak != nil {
			c.streak = *streak
		}
		if firing != nil {
			c.firing = *firing
		}
		if emitted != nil {
			c.emitted = domain.ServiceAlertState(*emitted)
		}
		if seq != nil {
			c.emittedSeq = *seq
		}
		slice = append(slice, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(slice) == 0 {
		// Zero alerting services costs exactly this one query.
		return out, tx.Commit(ctx)
	}

	// (1a) LINEARIZE against the configuration writers (§16.6a). The slice above was read from a
	// REPEATABLE READ snapshot, so without this an operator's edit can commit between the snapshot
	// and the emission and the evaluation then publishes a page for a policy that no longer exists —
	// and nothing closes it, because the writer looked for an open episode before the evaluator had
	// opened one. Generation mismatch dis-arms the MEMBERS' delegation afterwards; it does not
	// un-send the service's own stale page.
	//
	// Locking the rows the writers lock, in the same order, resolves it in whichever direction the
	// race actually went: if the evaluation gets there first the writer waits and then sees (and
	// closes) the episode; if the writer committed first, this statement raises a serialization
	// failure and the whole evaluation rolls back BEFORE it can emit anything.
	lockIDs := make([]string, 0, len(slice))
	for _, c := range slice {
		lockIDs = append(lockIDs, c.serviceID)
	}
	if err := lockAlertConfigRowsTx(ctx, tx, lockIDs); err != nil {
		return out, err
	}

	// (2) ONE snapshot for the whole slice, through the public page's own evaluation path.
	refs := make([]ServiceRef, 0, len(slice))
	for _, c := range slice {
		refs = append(refs, ServiceRef{ProjectID: c.projectID, ServiceID: c.serviceID})
	}
	projections, err := s.servicePageProjectionsTx(ctx, tx, refs, asOf, false)
	if err != nil {
		return out, err
	}

	lease := asOf.Add(time.Duration(serviceAlertLeaseMultiplier) * cadence)
	for _, c := range slice {
		if c.evaluatedAt != nil {
			if lag := asOf.Sub(*c.evaluatedAt); lag > out.Lag {
				out.Lag = lag
			}
		}
		p, ok := projections[c.serviceID]
		if !ok {
			// The service could not be evaluated. That is an ERROR state, not a healthy one: the row
			// records it, the lease collapses, and delegation dis-arms — which means the members page
			// for themselves until it can be evaluated again.
			if err := writeAlertErrorTx(ctx, tx, c.serviceID, c.projectID, c.configGeneration, asOf,
				"projection returned no row"); err != nil {
				return out, err
			}
			out.Errors++
			continue
		}
		observed := alertStateOf(p)
		candidateState, streak := observed, 1
		if c.candidateState == observed {
			candidateState, streak = c.candidateState, c.streak+1
		}
		decision := domain.DecideServiceAlert(c.policy, candidateState, streak, c.firing, c.emitted)

		// RE-ANNOUNCE an outage nobody heard about (D-0187).
		//
		// `DecideServiceAlert` compares the candidate state with what was emitted, so a service that
		// is still down and already "announced" produces no edge — correct, and it is exactly why a
		// failed announcement used to be the end of the story. D-0179 stopped the swallow by refusing
		// to arm coverage, so the members keep paging; it did not tell anybody. The incident is open,
		// the outage is real, and the service's own alert was never received.
		//
		// The trigger is the CONDEMNED sequence, not merely an undelivered one: an event still in the
		// outbox is also undelivered, and re-announcing on that would send a second copy of something
		// merely slow. `undelivered_seq = emitted_seq` means the worker attempted it, reached nobody,
		// and owes no retry.
		//
		// It re-uses the ordinary onset path deliberately, so the new announcement is an announcement
		// in every respect — new sequence, fresh recipient snapshot, its own episode — rather than a
		// special case that has to be remembered everywhere an announcement is handled. The episode
		// nobody heard is superseded as `undelivered`, which is a truthful reason where the onset
		// path's `policy_changed` would not be.
		reannounce := !decision.Notify && c.firing && c.emittedSeq > 0 &&
			c.undeliveredSeq == c.emittedSeq && c.emitted == candidateState &&
			c.policy.Pages(candidateState)
		if reannounce {
			decision.Notify, decision.Close = true, false
			decision.NextEmitted, decision.NextFiring = candidateState, true
		}

		// FR-022 invariant 5: a service opens an incident ONLY while that signal's coverage is ARMED,
		// and `owns_paging` — which the slice already filters on — is one of five conditions, not the
		// whole of it. Two of the others can be false here and were never checked:
		//
		//   * ROUTABILITY. With no enabled channel and no populated schedule the announcement reaches
		//     nobody, the arming conjunction says DIS-ARMED through its routable clause, and the
		//     members are correctly paging for themselves. Opening an incident on top of that is the
		//     thing the FR-022 test comment calls worse than opening none;
		//   * a GOVERNING DECLARATION. Before the first revision takes effect there is nothing the
		//     verdict was computed from, `revision_id` is NULL, and delegation is dis-armed for the
		//     same reason.
		//
		// The suppression covers ONSETS only. A CLOSE is never withheld (§16's polarity rule): an
		// announcement already made must be able to end, whatever the route looks like now.
		//
		// It also refuses to LATCH. Emitting nothing while advancing `live_firing` and `emitted_state`
		// would leave the service looking announced, so restoring the route later produces no edge and
		// the outage is never paged at all — a silence that outlives its cause.
		armed, withheldReason := true, ""
		if decision.Notify && !decision.Close {
			if c.revisionID == nil {
				// A different fact from a broken route, and it must not be reported as one: nothing
				// governs this service yet, so there is no declaration the verdict was computed
				// from. Somebody's paging configuration is fine; the declaration has not taken
				// effect.
				armed, withheldReason = false, WithheldNoGoverningRevision
			} else {
				route, rerr := resolveServiceRecipientsTx(ctx, tx, c.projectID, c.scheduleID, asOf)
				if rerr != nil {
					return out, rerr
				}
				if armed = len(route) > 0; !armed {
					withheldReason = WithheldUnroutable
				}
			}
		}
		if !decision.Notify || !armed {
			// Not an error and not an announcement: a successful evaluation that had nothing it was
			// allowed to say. The state below records the observation and refreshes the lease, so the
			// next pass sees the same candidate and the same streak.
			if decision.Notify {
				out.Withheld[withheldReason]++
			}
			decision.Notify = false
			decision.NextFiring = c.firing
		}

		emittedState, emittedSeq := c.emitted, c.emittedSeq
		if decision.Notify {
			emittedState = decision.NextEmitted
			emittedSeq = c.emittedSeq + 1
			if err := s.emitServiceAlertTx(ctx, tx, serviceAlertEmission{
				serviceID: c.serviceID, projectID: c.projectID, name: c.name, slug: c.slug,
				scheduleID: c.scheduleID, signal: domain.ServiceSignalHealth,
				close: decision.Close, closeReason: decision.CloseReason,
				episodeState: string(candidateState), seq: emittedSeq, asOf: asOf,
				supersedeReason: supersedeReasonFor(reannounce),
				// The live half of the payload. Target, window and rule stay empty: the schema's
				// CHECK makes them exclusive to the burn signal.
				alert: domain.ServiceAlert{State: candidateState, ConfirmedOver: streak},
			}); err != nil {
				return out, err
			}
			// FR-022: the incident rides the SAME transaction as the announcement, so an incident without
			// its announcement — or an announcement without its incident — is unrepresentable (FR-022
			// invariant 7). This SUPERSEDES FR-021 invariant 86, which said a service alert touches no
			// incident table; §16.8 carries the note and iter-0156 §4 records why the note, the discharge
			// row and the test had to move in this same change.
			if decision.Close {
				out.Closes++
				resolved, rerr := s.ResolveServiceIncidentTx(ctx, tx, c.serviceID,
					"Resolved automatically: the service is no longer in a pageable state.")
				if rerr != nil {
					return out, rerr
				}
				if resolved {
					out.IncidentsResolved++
				}
			} else {
				out.Onsets++
				// Only a LIVE onset opens one, and never a burn breach (spec D1): a budget signal is not an
				// outage. This evaluator is the live one, so being here is already that guarantee.
				// The SAME revision this verdict was computed from (§16.1's arming half), so the
				// postmortem names the declaration that was governing during the outage rather than
				// one that becomes effective at the next bucket boundary.
				_, created, ierr := s.OpenServiceIncidentTx(ctx, tx, c.serviceID, c.projectID,
					c.name+" — service "+string(candidateState), streak, c.revisionID)
				if ierr != nil {
					return out, ierr
				}
				if created {
					out.IncidentsOpened++
				}
			}
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO service_alert_state
			  (service_id, project_id, observed_state, candidate_state, streak, live_firing,
			   emitted_state, emitted_seq, emitted_at, config_generation, revision_id,
			   evaluated_at, lease_until, last_error)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,
			        CASE WHEN $9 THEN $11::timestamptz ELSE NULL END,$10,$13::uuid,$11,$12,NULL)
			ON CONFLICT (service_id) DO UPDATE SET
			   observed_state = EXCLUDED.observed_state,
			   candidate_state = EXCLUDED.candidate_state,
			   streak = EXCLUDED.streak,
			   live_firing = EXCLUDED.live_firing,
			   emitted_state = EXCLUDED.emitted_state,
			   emitted_seq = EXCLUDED.emitted_seq,
			   -- The last announcement's time survives an evaluation that announced nothing: it is a
			   -- fact about what an operator was told, not about this pass.
			   emitted_at = COALESCE(EXCLUDED.emitted_at, service_alert_state.emitted_at),
			   config_generation = EXCLUDED.config_generation,
			   -- The STAMP: which declaration this verdict was computed from (§16.1 arming).
			   revision_id = EXCLUDED.revision_id,
			   evaluated_at = EXCLUDED.evaluated_at,
			   lease_until = EXCLUDED.lease_until,
			   last_error = NULL
			 -- The CAS against a DEPOSED evaluator: a slower leader that lost the lock must not
			 -- overwrite a newer verdict with its stale one.
			 WHERE service_alert_state.evaluated_at <= EXCLUDED.evaluated_at`,
			c.serviceID, c.projectID, string(observed), string(candidateState), streak,
			decision.NextFiring, nullableState(emittedState), emittedSeq, decision.Notify,
			c.configGeneration, asOf, lease, c.revisionID)
		if err != nil {
			return out, fmt.Errorf("store: write alert state: %w", err)
		}
		// A CAS that changed NOTHING means a newer verdict already exists — and this pass may
		// already have written an episode and an outbox row a few lines above. Failing here rolls
		// BOTH back with the transaction. Ignoring it would leave the worst possible pair: a page
		// that went out and a latch that never moved, so the next pass announces the same edge
		// again while the sequence stands still.
		if tag.RowsAffected() == 0 {
			return out, fmt.Errorf("store: live verdict for service %s lost to a newer evaluation",
				c.serviceID)
		}
		out.Evaluated++
	}
	return out, tx.Commit(ctx)
}

// alertStateOf maps a page projection to the alerting vocabulary. EXCLUDED outranks everything —
// a declared window is a declared silence — and `unknown` stays its own state rather than collapsing
// into `down`.
func alertStateOf(p ServicePageProjection) domain.ServiceAlertState {
	switch {
	case p.Excluded:
		return domain.ServiceAlertExcluded
	case p.SLI == "down":
		return domain.ServiceAlertDown
	case p.SLI == "degraded":
		return domain.ServiceAlertDegraded
	case p.SLI == "healthy":
		return domain.ServiceAlertHealthy
	default:
		return domain.ServiceAlertUnknown
	}
}

func nullableState(s domain.ServiceAlertState) *string {
	if s == "" {
		return nil
	}
	v := string(s)
	return &v
}

// nullableText writes "" as SQL NULL. The alert schema distinguishes the two everywhere it can:
// an empty `rule_key` is the LIVE signal (its CHECK forbids a value), not a burn rule with a blank
// name, and the same holds for the target columns.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeAlertErrorTx records a failed evaluation and COLLAPSES the lease, so delegation dis-arms
// immediately rather than coasting on the previous success until it would have expired.
func writeAlertErrorTx(
	ctx context.Context, tx pgx.Tx, serviceID, projectID string, generation int64,
	asOf time.Time, reason string,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO service_alert_state
		  (service_id, project_id, observed_state, candidate_state, streak, config_generation,
		   evaluated_at, lease_until, last_error)
		VALUES ($1,$2,'unknown','unknown',1,$3,$4,$4,$5)
		ON CONFLICT (service_id) DO UPDATE SET
		   evaluated_at = EXCLUDED.evaluated_at,
		   lease_until = EXCLUDED.evaluated_at,
		   last_error = EXCLUDED.last_error`,
		serviceID, projectID, generation, asOf, reason)
	if err != nil {
		return fmt.Errorf("store: write alert error state: %w", err)
	}
	return nil
}

// serviceAlertEmission is one edge to publish, for EITHER signal.
//
// One episode implementation serves both, because "a close must reach the onset's recipients and
// must survive the deletion of what fired" (§16.4a) is one property, not two — a second copy of it
// for the burn signal would be a second place for that property to rot.
type serviceAlertEmission struct {
	serviceID, projectID, name, slug string
	scheduleID                       string
	// signal scopes the episode as well as the payload: the live signal keeps ONE episode per
	// service, the burn signal one per (target, rule).
	signal domain.ServiceAlertSignal
	// targetID / targetWindow / ruleKey are the burn signal's identity, empty for the live one.
	//
	// The TARGET is part of that identity and not a decoration: `sla_targets` is unique on
	// (service_id, window_name), so one service may carry a 7d and a 30d target whose rules share a
	// canonical key — the key spells severity/long/short/threshold and names no window. Scoping the
	// episode by rule alone would make those two rules collide on one open episode and fence each
	// other's deliveries. On the episode it is stored as `target_snapshot_id`, without a foreign
	// key: a `rule_removed` or target-deletion close routes THROUGH that row, so the identity has to
	// survive the target it names.
	targetID, targetWindow, ruleKey string
	// supersedeReason is why the open episode this onset replaces was ended. Empty means the onset
	// path's ordinary reason; a re-announcement sets `undelivered`, because nothing about the policy
	// changed and saying so would put a false cause in a record an operator reads (D-0187).
	supersedeReason domain.ServiceAlertCloseReason
	// close marks an edge that ENDS an announcement; closeReason is never inferred.
	close       bool
	closeReason domain.ServiceAlertCloseReason
	// episodeState is `service_alert_episodes.state` — §16.4a's "the state or rule that fired": the
	// alerting state for the live signal, the canonical rule key for the burn one.
	episodeState string
	seq          int64
	asOf         time.Time
	// alert carries the SIGNAL-SPECIFIC half of the payload. Every field this function owns —
	// identity, signal, firing, close reason, episode, sequence, rule and recipients — is
	// overwritten below, so no caller can publish an alert its episode does not back.
	alert domain.ServiceAlert
}

// emitServiceAlertTx opens or closes the episode and enqueues the event, in the caller's transaction.
//
// The RECIPIENTS are resolved once, at ONSET, and stored on the episode; a CLOSE reads them back
// rather than re-resolving. A schedule that rotated mid-incident would otherwise page somebody who
// never heard the onset and leave the person who did holding an open alert.
func (s *Store) emitServiceAlertTx(ctx context.Context, tx pgx.Tx, e serviceAlertEmission) error {
	var episodeID string
	var recipients []string

	if e.close {
		// Close the open episode and take ITS recipients. A close with no open episode is possible
		// after a crash mid-onset; it still notifies, with the current route, because somebody may
		// have been told.
		err := tx.QueryRow(ctx, `
			UPDATE service_alert_episodes
			   SET closed_at = $2, close_reason = $3
			 WHERE service_id = $1 AND signal = $4 AND closed_at IS NULL
			   AND COALESCE(rule_key, '') = $5
			   AND COALESCE(target_snapshot_id::text, '') = $6
			 RETURNING id, ARRAY(SELECT jsonb_array_elements_text(recipients))`,
			e.serviceID, e.asOf, string(e.closeReason), string(e.signal), e.ruleKey, e.targetID).
			Scan(&episodeID, &recipients)
		if err != nil && !noRows(err) {
			return fmt.Errorf("store: close alert episode: %w", err)
		}
		if noRows(err) {
			if recipients, err = resolveServiceRecipientsTx(ctx, tx, e.projectID, e.scheduleID, e.asOf); err != nil {
				return err
			}
		}
	} else {
		var err error
		if recipients, err = resolveServiceRecipientsTx(ctx, tx, e.projectID, e.scheduleID, e.asOf); err != nil {
			return err
		}
		snapshot, err := json.Marshal(recipients)
		if err != nil {
			return fmt.Errorf("store: marshal recipients: %w", err)
		}
		// An onset supersedes any episode still open FOR THIS IDENTITY — a state change from one
		// pageable state to another is ONE new announcement, not a close plus an onset, and leaving
		// the old episode open would make "the close" ambiguous. For the burn signal this is the
		// guard against an episode orphaned by a latch that cascaded away with its target: without
		// it the next onset would collide with the open-episode unique index.
		supersede := domain.ClosePolicyChanged
		if e.supersedeReason != "" {
			supersede = e.supersedeReason
		}
		if _, err := tx.Exec(ctx, `
			UPDATE service_alert_episodes SET closed_at = $2, close_reason = $6
			 WHERE service_id = $1 AND signal = $3 AND closed_at IS NULL
			   AND COALESCE(rule_key, '') = $4
			   AND COALESCE(target_snapshot_id::text, '') = $5`,
			e.serviceID, e.asOf, string(e.signal), e.ruleKey, e.targetID,
			string(supersede)); err != nil {
			return fmt.Errorf("store: supersede alert episode: %w", err)
		}
		// `project_id` is the SERVICE'S own project, read in the same snapshot: the episode carries a
		// composite FK to (services.id, project_id), so a caller-supplied tenant would be refused by
		// the database rather than silently filed under the wrong one.
		if err := tx.QueryRow(ctx, `
			INSERT INTO service_alert_episodes
			  (service_id, project_id, service_name, signal, target_snapshot_id, target_window,
			   rule_key, state, started_at, recipients, emitted_seq)
			VALUES ($1,$2,$3,$4,$5::uuid,$6,$7,$8,$9,$10,$11) RETURNING id`,
			e.serviceID, e.projectID, e.name, string(e.signal), nullableText(e.targetID),
			nullableText(e.targetWindow), nullableText(e.ruleKey), e.episodeState, e.asOf,
			snapshot, e.seq).Scan(&episodeID); err != nil {
			return fmt.Errorf("store: open alert episode: %w", err)
		}
	}

	alert := e.alert
	alert.ServiceID, alert.ProjectID = e.serviceID, e.projectID
	alert.ServiceName, alert.ServiceSlug = e.name, e.slug
	alert.Signal, alert.Firing = e.signal, !e.close
	alert.CloseReason = e.closeReason
	alert.SLATargetID, alert.RuleKey = e.targetID, e.ruleKey
	// `Window` is the target's window name, the same field the monitor burn payload carries. The
	// emission owns it rather than the caller's template, so a payload cannot name one target while
	// its episode snapshots another.
	if e.targetWindow != "" {
		alert.Window = e.targetWindow
	}
	alert.EpisodeID, alert.Seq, alert.Recipients = episodeID, e.seq, recipients

	payload, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("store: marshal service alert: %w", err)
	}
	return enqueueOutboxTx(ctx, tx, domain.TopicServiceAlert, payload)
}

// resolveServiceRecipientsTx resolves WHO to notify, in the §16.6 order: the service's on-call
// schedule when it has one, otherwise the project's enabled channels.
//
// `services.escalation_policy_id` is still NOT consulted HERE, and the reason changed with FR-023.
// It used to be that no ladder was possible at all: a ladder is defined relative to an incident start
// with acknowledgement and progress state, and a service alert opened no incident. FR-022 made the
// incident, and FR-023 built the ladder on it — in `AdvanceEscalations`, where every other ladder
// lives. So the division of labour is now deliberate rather than forced: THIS function answers "who
// hears the announcement", and the ladder answers "who hears it next, and next" from the incident's
// own progress. A policy read here would fire a step nobody latched.
func resolveServiceRecipientsTx(
	ctx context.Context, tx pgx.Tx, projectID, scheduleID string, at time.Time,
) ([]string, error) {
	if scheduleID != "" {
		row := tx.QueryRow(ctx, `SELECT `+oncallColumns+` FROM oncall_schedules WHERE id = $1`, scheduleID)
		sc, err := scanOnCallSchedule(row)
		if err != nil && !noRows(err) {
			return nil, fmt.Errorf("store: read service schedule: %w", err)
		}
		if err == nil {
			if sc.Overrides, err = loadOverrides(ctx, tx, sc.ID); err != nil {
				return nil, err
			}
			// The channel the rotation names has to STILL BE A CHANNEL. `participants` is a JSON
			// array of ids that nothing prunes: deleting a notification channel removes its row and
			// leaves the id in every schedule that named it, and disabling one changes no JSON at
			// all. Returning that id unchecked is how a service came to hold a route that cannot
			// receive anything — while the routable clause armed on the same non-empty array, so the
			// member's own alert was suppressed at the same time. Both paths silent, which is the
			// one outcome §16.1 exists to prevent.
			//
			// §16.6 already says what to do instead: a deleted or empty target falls back to the
			// project's channels. A DISABLED one is the same fact wearing a different hat.
			if ch := sc.OnCall(at); ch != "" {
				// A participant that is not a channel id at all is CORRUPTION, not the ordinary
				// "the channel was deleted" that §16.6 falls back for. Falling back would repair a
				// broken configuration silently and make a typo indistinguishable from a deletion;
				// erroring surfaces it as the evaluation error it is, which collapses the lease and
				// dis-arms — so the members keep paging while somebody fixes the schedule. The write
				// path refuses such a value now (`domain.OnCallSchedule.Validate`), so this can only
				// be a row written before that guard existed.
				if !domain.IsChannelID(ch) {
					return nil, fmt.Errorf(
						"store: on-call schedule %s names %q, which is not a channel id", sc.ID, ch)
				}
				var live bool
				if err := tx.QueryRow(ctx, `
					SELECT true FROM notification_channels
					 WHERE id::text = $1 AND project_id = $2 AND enabled`, ch, projectID).Scan(&live); err != nil {
					if !noRows(err) {
						return nil, fmt.Errorf("store: verify on-call channel: %w", err)
					}
				}
				if live {
					return []string{ch}, nil
				}
			}
		}
		// A schedule that resolves to nobody falls through to the project's channels rather than
		// returning an empty route: the arming rule refuses to suppress when neither exists, so the
		// only thing an empty answer here would buy is a silent alert.
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM notification_channels WHERE project_id = $1 AND enabled ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: read project channels: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan channel: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// lockAlertConfigRowsTx takes the row locks that make an evaluation and a paging-config write
// LINEARIZABLE with respect to each other, in the writers' own order: services first, then targets.
//
// Under REPEATABLE READ a locking read of a row that a committed transaction has changed since the
// snapshot raises 40001 rather than returning the stale version, which is exactly the outcome an
// evaluator needs: it aborts before publishing anything about a configuration that has been
// replaced. The order matches `UpdateServiceAlertPolicy` / `SetServiceBurnAlerting` (the service row
// before its target), and the ids are sorted, so two of these can never deadlock each other.
func lockAlertConfigRowsTx(ctx context.Context, tx pgx.Tx, serviceIDs []string, targetIDs ...[]string) error {
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM services WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
		serviceIDs); err != nil {
		return fmt.Errorf("store: linearize alert config: %w", err)
	}
	for _, ids := range targetIDs {
		if len(ids) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM sla_targets WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
			ids); err != nil {
			return fmt.Errorf("store: linearize burn config: %w", err)
		}
	}
	return nil
}

// alertConn is what an evaluator needs from the thing it runs on: the ability to open its OWN
// snapshot transaction. Both a pool and a single pooled CONNECTION satisfy it, which is the whole
// point — the leader passes its LOCK-OWNING connection (§16.7), so losing the lock kills the
// in-flight evaluation instead of letting a deposed leader commit episodes and outbox rows behind
// its successor. A generation/lease CAS cannot cover that on its own: the episode and the outbox
// row are written BEFORE the latch upsert, so a stale evaluator would have already published.
type alertConn interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// beginAlertSnapshot opens the evaluator's transaction: one instant for the whole slice, and
// read-write because the verdicts are written under the same snapshot that produced them.
func (s *Store) beginAlertSnapshot(ctx context.Context, db alertConn) (pgx.Tx, time.Time, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: begin alert snapshot: %w", err)
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		_ = tx.Rollback(ctx)
		return nil, time.Time{}, fmt.Errorf("store: alert clock: %w", err)
	}
	return tx, asOf.UTC(), nil
}

// supersedeReasonFor names why the episode an onset replaces was ended. A re-announcement ends one
// whose announcement reached nobody; anything else keeps the onset path's ordinary reason.
func supersedeReasonFor(reannounce bool) domain.ServiceAlertCloseReason {
	if reannounce {
		return domain.CloseUndelivered
	}
	return ""
}
