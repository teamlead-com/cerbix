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
	// Lag is how far behind the oldest evaluated service was, so a stalled evaluator reads as lag
	// rather than as an absence of alerts — which is indistinguishable from "nothing is wrong".
	Lag time.Duration
}

// EvaluateServiceAlerts evaluates ONE slice of alerting services and emits the edges it finds.
//
// The state write and the outbox row commit in ONE transaction (invariant 80), so a delivered alert
// can never be forgotten and a forgotten one can never be delivered twice. Everything else about
// delivery is at-least-once and the payload's sequence is what keeps a duplicate from being a lie.
func (s *Store) evaluateServiceAlertsOn(
	ctx context.Context, db alertConn, cadence time.Duration,
) (ServiceAlertEvaluation, error) {
	var out ServiceAlertEvaluation

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
		evaluatedAt    *time.Time
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.project_id, s.name, s.slug,
		       s.page_on, s.page_on_unknown, s.confirm_evaluations, s.alert_config_generation,
		       COALESCE(s.oncall_schedule_id::text, ''), rev.id::text,
		       st.candidate_state, st.streak, st.live_firing, st.emitted_state, st.emitted_seq,
		       st.evaluated_at
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
			&candState, &streak, &firing, &emitted, &seq, &c.evaluatedAt); err != nil {
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

		emittedState, emittedSeq := c.emitted, c.emittedSeq
		if decision.Notify {
			emittedState = decision.NextEmitted
			emittedSeq = c.emittedSeq + 1
			if err := s.emitServiceAlertTx(ctx, tx, serviceAlertEmission{
				serviceID: c.serviceID, projectID: c.projectID, name: c.name, slug: c.slug,
				scheduleID: c.scheduleID, signal: domain.ServiceSignalHealth,
				close: decision.Close, closeReason: decision.CloseReason,
				episodeState: string(candidateState), seq: emittedSeq, asOf: asOf,
				// The live half of the payload. Target, window and rule stay empty: the schema's
				// CHECK makes them exclusive to the burn signal.
				alert: domain.ServiceAlert{State: candidateState, ConfirmedOver: streak},
			}); err != nil {
				return out, err
			}
			if decision.Close {
				out.Closes++
			} else {
				out.Onsets++
			}
		}
		if _, err := tx.Exec(ctx, `
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
			c.configGeneration, asOf, lease, c.revisionID); err != nil {
			return out, fmt.Errorf("store: write alert state: %w", err)
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
		if _, err := tx.Exec(ctx, `
			UPDATE service_alert_episodes SET closed_at = $2, close_reason = 'policy_changed'
			 WHERE service_id = $1 AND signal = $3 AND closed_at IS NULL
			   AND COALESCE(rule_key, '') = $4
			   AND COALESCE(target_snapshot_id::text, '') = $5`,
			e.serviceID, e.asOf, string(e.signal), e.ruleKey, e.targetID); err != nil {
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
// `services.escalation_policy_id` is deliberately NOT consulted. A ladder is defined relative to an
// incident start with acknowledgement and progress state, and a service alert opens no incident — so
// "the ladder applies unchanged" was not implementable, and pretending otherwise would have shipped
// a policy that silently fires one step.
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
			if ch := sc.OnCall(at); ch != "" {
				return []string{ch}, nil
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
