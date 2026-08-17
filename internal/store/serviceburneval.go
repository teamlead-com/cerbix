package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.4 / §16.4a / §16.4b / §16.5a — the SEALED evaluator.
//
// The burn arm mirrors the live arm's shapes deliberately (one bounded slice per call, one snapshot
// instant, the verdict and its outbox row in ONE transaction, a DB-clock lease), because the two
// signals are read back through the same arming query and a second dialect of "fresh", "current
// generation" or "successfully evaluated" is exactly how a badge comes to say armed while delivery
// says otherwise.
//
// Three things are its own, and each is load-bearing:
//
//   - the latch is PER RULE (§16.4b), so the unit of the slice is a TARGET (up to 4 rules) while the
//     unit of the verdict, the sequence and the episode is a rule;
//   - the numbers come from SEALED facts through the ONE burn math owner (`computeBurnWindows`), in a
//     SINGLE batched call for the whole slice — one query per window would make the leader's cadence
//     depend on how many rules an operator happened to configure;
//   - a HOLD is a SUCCESSFUL evaluation that cannot speak. It keeps the level and records why, which
//     is what dis-arms burn coverage in `activeBurnDelegationSQL`. Treating an unquotable window as
//     "burn = 0" would silently resolve a live alert, which is the most dangerous mistake available
//     in this feature.
//
// Identity is the FULL primary key (service, project, target, rule) everywhere — never (service,
// rule). `sla_targets` is unique on (service_id, window_name), so one service may legally carry a 7d
// and a 30d target whose rules share a canonical key (the key spells severity/long/short/threshold
// and names no window). Scoping anything by rule alone would let those two collide on one latch, one
// episode and one sequence, and fence each other's deliveries.

// ServiceBurnEvaluation is what one slice did, for the leader's telemetry. It mirrors
// ServiceAlertEvaluation and adds the two counters the burn signal has that the live one does not:
// rules (the cap is in targets, the work is in rules) and holds (a successful evaluation that
// dis-armed rather than spoke, which reads as silence in every other counter).
type ServiceBurnEvaluation struct {
	// Targets is service burn targets successfully evaluated; Rules is the rules inside them.
	Targets int
	Rules   int
	Onsets  int
	Closes  int
	Holds   int
	Errors  int
	// Lag is how far behind the oldest evaluated rule was, so a stalled evaluator reads as lag rather
	// than as an absence of alerts — which is indistinguishable from "nothing is burning".
	Lag time.Duration
}

// burnCandidate is ONE service-scoped burn target in the slice, with everything the evaluation needs
// joined in: the service's route and generation, the target's objective, window and generation, and
// the service's own materialization watermark.
type burnCandidate struct {
	serviceID, projectID, name, slug string
	scheduleID                       string
	configGeneration                 int64

	targetID         string
	window           string
	objective        float64
	targetGeneration int64
	rules            []domain.BurnRule
	// rulesErr is a `burn_rules` payload that would not decode. It is carried rather than returned
	// so ONE corrupt target dis-arms itself instead of aborting the slice and leaving every other
	// target un-evaluated with its lease still coasting.
	rulesErr error

	// era and sealed are the service's materialization era start and watermark. Both nullable: a
	// service with no watermark row, or one that has sealed nothing, cannot produce a sealed verdict.
	era    *time.Time
	sealed *time.Time
	// oldest is min(evaluated_at) over the target's rules — how long this target has gone
	// un-evaluated, which is both the slice order and the lag.
	oldest *time.Time
}

// burnRuleLatch is the level a rule currently holds and the sequence it last announced under.
type burnRuleLatch struct {
	firing bool
	seq    int64
}

// EvaluateServiceBurnAlerts evaluates ONE slice of service burn targets and emits the edges it finds.
//
// The state write and the outbox row commit in ONE transaction (invariant 80): a delivered burn alert
// can never be forgotten and a forgotten one can never be delivered twice. Ordering is the per-rule
// sequence in the payload, which is what keeps an at-least-once duplicate from being a LIE.
func (s *Store) evaluateServiceBurnAlertsOn(
	ctx context.Context, db alertConn, cadence time.Duration,
) (ServiceBurnEvaluation, error) {
	var out ServiceBurnEvaluation

	// A READ-WRITE snapshot: every window in this slice is judged against ONE instant, because
	// §11.3's staleness test compares the watermark to it and two instants would let two rules of one
	// target disagree about whether the service is quotable at all.
	tx, asOf, err := s.beginAlertSnapshot(ctx, db)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// (1) The slice: burn targets of OWNING services, ordered by how long each has gone un-evaluated.
	// A target with no state row at all sorts first — it has never been evaluated, and until it has,
	// its members are paging for themselves. The cap counts TARGETS; a target carries up to 4 rules,
	// so the bound on work is 4× the bound on rows, which is the trade the fixed cap already makes.
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.project_id, s.name, s.slug, s.alert_config_generation,
		       COALESCE(s.oncall_schedule_id::text, ''),
		       t.id, t.window_name, t.objective, t.alert_generation, t.burn_rules,
		       m.era_start, m.sealed_through,
		       st.oldest
		  FROM sla_targets t
		  JOIN services s ON s.id = t.service_id
		  -- LEFT: a service that has never materialized has no row at all, which is a HOLD with a
		  -- reason, not a target to skip. Skipping it would leave a previously armed latch fresh.
		  LEFT JOIN service_materialization m
		         ON m.service_id = s.id AND m.project_id = s.project_id
		  -- An ungrouped aggregate, so it yields exactly one row per target: NULL when no rule of
		  -- this target has ever been evaluated, and otherwise the STALEST rule's instant — the
		  -- target holding the oldest verdict is the one that most needs the next pass.
		  LEFT JOIN LATERAL (
		      SELECT min(b.evaluated_at) AS oldest
		        FROM service_burn_alert_state b
		       WHERE b.service_id = s.id AND b.project_id = s.project_id
		         AND b.sla_target_id = t.id
		  ) st ON true
		 WHERE s.owns_paging AND t.burn_alert_enabled AND t.burn_rules <> '[]'::jsonb
		 ORDER BY st.oldest ASC NULLS FIRST, t.id
		 LIMIT $1`, ServiceAlertSliceCap)
	if err != nil {
		return out, fmt.Errorf("store: service burn slice: %w", err)
	}
	var slice []burnCandidate
	targetIDs := make([]string, 0, ServiceAlertSliceCap)
	for rows.Next() {
		var c burnCandidate
		var raw []byte
		if err := rows.Scan(&c.serviceID, &c.projectID, &c.name, &c.slug, &c.configGeneration,
			&c.scheduleID, &c.targetID, &c.window, &c.objective, &c.targetGeneration, &raw,
			&c.era, &c.sealed, &c.oldest); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: scan burn candidate: %w", err)
		}
		if err := json.Unmarshal(raw, &c.rules); err != nil {
			c.rules, c.rulesErr = nil, err
		}
		slice = append(slice, c)
		targetIDs = append(targetIDs, c.targetID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(slice) == 0 {
		// Zero burn targets costs exactly this one query.
		return out, tx.Commit(ctx)
	}

	// (2) The latches, for the whole slice in one read. Keyed by the FULL identity: two targets of
	// one service may carry the same canonical rule key, and a map keyed by rule alone would hand
	// one target's level to the other.
	latch, err := burnLatchTx(ctx, tx, targetIDs)
	if err != nil {
		return out, err
	}

	// (3) Every window of every rule of the whole slice goes into ONE batch. `ruleSlot` remembers
	// where each rule's two answers land, so the decision loop below never re-derives an index.
	type ruleSlot struct {
		cand, rule, long, short int
	}
	var slots []ruleSlot
	var reqs []burnWindowRequest
	lease := asOf.Add(time.Duration(serviceAlertLeaseMultiplier) * cadence)

	for i := range slice {
		c := &slice[i]
		if c.oldest != nil {
			if lag := asOf.Sub(*c.oldest); lag > out.Lag {
				out.Lag = lag
			}
		}
		switch {
		case c.rulesErr != nil:
			// Nothing here names the rules, so the dis-arming write covers whatever rows the target
			// already had: leaving them fresh would arm coverage on a configuration nobody can read.
			if err := disarmBurnTargetTx(ctx, tx, *c, nil, asOf,
				domain.ServiceReportReasonNothingMeasured,
				"burn rules could not be decoded: "+c.rulesErr.Error()); err != nil {
				return out, err
			}
			out.Errors++
			continue
		case c.sealed == nil || c.era == nil:
			// No watermark, no sealed verdict — and NO invented rate. This is the error state, not a
			// healthy one: last_error is set and the lease collapses, so delegation dis-arms
			// immediately and the members page for themselves until the service seals something.
			if err := disarmBurnTargetTx(ctx, tx, *c, burnRuleKeys(c.rules), asOf,
				domain.ServiceReportReasonNothingSealed,
				"the service has sealed nothing, so no window can be quoted"); err != nil {
				return out, err
			}
			out.Errors++
			continue
		}
		seen := make(map[string]bool, len(c.rules))
		for j := range c.rules {
			// Two rules with one canonical key are a validation error, never a silent merge (§16.4b):
			// one latch cannot answer for both, and whichever wrote last would own the other's firing
			// state. The domain validator rejects them; here the second is refused a slot so the
			// stored latch can never become ambiguous.
			key := c.rules[j].Key()
			if seen[key] {
				continue
			}
			seen[key] = true
			slots = append(slots, ruleSlot{cand: i, rule: j, long: len(reqs), short: len(reqs) + 1})
			reqs = append(reqs,
				burnWindowRequest{
					serviceID: c.serviceID, projectID: c.projectID, label: "long",
					duration:  time.Duration(c.rules[j].LongWindowSeconds) * time.Second,
					objective: c.objective, era: *c.era, sealed: *c.sealed,
				},
				burnWindowRequest{
					serviceID: c.serviceID, projectID: c.projectID, label: "short",
					duration:  time.Duration(c.rules[j].ShortWindowSeconds) * time.Second,
					objective: c.objective, era: *c.era, sealed: *c.sealed,
				})
		}
		out.Targets++
	}

	// (4) ONE call to the one math owner, for every window of the slice.
	windows, err := computeBurnWindows(ctx, tx, reqs, asOf)
	if err != nil {
		return out, err
	}

	// (5) The verdicts. Pure per rule: two window answers and the latch decide, and nothing else.
	for _, sl := range slots {
		c := &slice[sl.cand]
		rule := c.rules[sl.rule]
		key := rule.Key()
		prev := latch[burnLatchKey{targetID: c.targetID, ruleKey: key}]
		long := windows[sl.long]
		decision := domain.DecideBurnRule(long, windows[sl.short], rule.Threshold, prev.firing)

		seq := prev.seq
		if decision.Edge {
			// ONLY an edge announces. A rule that is still firing has nothing new to say, and
			// re-stating it every cadence is how a page becomes something people mute.
			seq = prev.seq + 1
			var rate float64
			if long.Rate != nil {
				rate = *long.Rate
			}
			// The LONG window's rate is the one published: it is the wider evidence and the window
			// that decides whether the burn is real; the short one only confirms it is still going.
			var closeReason domain.ServiceAlertCloseReason
			if !decision.Firing {
				// A CLEAR edge is a genuine recovery: both windows were quotable and one came back
				// under the threshold. Every other way a burn announcement can end (the target
				// disabled, the rule removed, the service deleted) is a lifecycle close and names
				// itself — this path never borrows those reasons.
				closeReason = domain.CloseRecovered
			}
			if err := s.emitServiceAlertTx(ctx, tx, serviceAlertEmission{
				serviceID: c.serviceID, projectID: c.projectID, name: c.name, slug: c.slug,
				scheduleID:   c.scheduleID,
				signal:       domain.ServiceSignalBurn,
				targetID:     c.targetID,
				targetWindow: c.window,
				ruleKey:      key,
				close:        !decision.Firing,
				closeReason:  closeReason,
				episodeState: key,
				seq:          seq,
				asOf:         asOf,
				alert: domain.ServiceAlert{
					// Window (the target's name for itself) is stamped by the emission from the
					// same value the episode snapshots, so it is absent here on purpose.
					WindowSeconds:      rule.LongWindowSeconds,
					ShortWindowSeconds: rule.ShortWindowSeconds, Severity: rule.Severity,
					Objective: c.objective, BurnRate: rate, Threshold: rule.Threshold,
					// Every burn payload states the watermark it was computed from: this signal
					// trails the seal by construction, and a number without its basis is what §11.2
					// spent a phase removing.
					SealedThrough: c.sealed,
				},
			}); err != nil {
				return out, err
			}
			if decision.Firing {
				out.Onsets++
			} else {
				out.Closes++
			}
		}
		if decision.Verdict == domain.BurnHold {
			out.Holds++
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO service_burn_alert_state
			  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict, last_reason,
			   target_generation, config_generation, emitted_seq, emitted_at, evaluated_at,
			   lease_until, last_error)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			        CASE WHEN $11 THEN $12::timestamptz ELSE NULL END,$12,$13,NULL)
			ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO UPDATE SET
			   -- Under a HOLD this is the level BEFORE the evaluation, unchanged: a rule that cannot
			   -- be quoted keeps firing.
			   firing = EXCLUDED.firing,
			   last_verdict = EXCLUDED.last_verdict,
			   last_reason = EXCLUDED.last_reason,
			   target_generation = EXCLUDED.target_generation,
			   config_generation = EXCLUDED.config_generation,
			   emitted_seq = EXCLUDED.emitted_seq,
			   -- The last announcement's time survives an evaluation that announced nothing: it is a
			   -- fact about what an operator was told, not about this pass.
			   emitted_at = COALESCE(EXCLUDED.emitted_at, service_burn_alert_state.emitted_at),
			   evaluated_at = EXCLUDED.evaluated_at,
			   lease_until = EXCLUDED.lease_until,
			   last_error = NULL
			 -- The CAS against a DEPOSED evaluator: a slower leader that lost the lock must not
			 -- overwrite a newer verdict with its stale one.
			 WHERE service_burn_alert_state.evaluated_at <= EXCLUDED.evaluated_at`,
			c.serviceID, c.projectID, c.targetID, key, decision.Firing, string(decision.Verdict),
			nullableText(decision.HoldReason), c.targetGeneration, c.configGeneration,
			seq, decision.Edge, asOf, lease); err != nil {
			return out, fmt.Errorf("store: write burn alert state: %w", err)
		}
		out.Rules++
	}
	return out, tx.Commit(ctx)
}

// burnLatchKey is the part of `service_burn_alert_state`'s primary key that varies inside one slice.
// The target is in it because two targets of one service may carry rules with the same canonical key.
type burnLatchKey struct{ targetID, ruleKey string }

// burnLatchTx reads the current level and sequence of every rule of the sliced targets, in one query.
func burnLatchTx(
	ctx context.Context, tx pgx.Tx, targetIDs []string,
) (map[burnLatchKey]burnRuleLatch, error) {
	out := make(map[burnLatchKey]burnRuleLatch, len(targetIDs))
	rows, err := tx.Query(ctx, `
		SELECT sla_target_id, rule_key, firing, emitted_seq
		  FROM service_burn_alert_state
		 WHERE sla_target_id = ANY($1::uuid[])`, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("store: read burn latches: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k burnLatchKey
		var l burnRuleLatch
		if err := rows.Scan(&k.targetID, &k.ruleKey, &l.firing, &l.seq); err != nil {
			return nil, fmt.Errorf("store: scan burn latch: %w", err)
		}
		out[k] = l
	}
	return out, rows.Err()
}

// burnRuleKeys is the canonical key of each rule, deduplicated — §16.4b's ambiguity guard, applied
// wherever a set of keys addresses a set of latch rows.
func burnRuleKeys(rules []domain.BurnRule) []string {
	seen := make(map[string]bool, len(rules))
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if key := r.Key(); !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// disarmBurnTargetTx records a FAILED evaluation for a target and COLLAPSES the lease, so delegation
// dis-arms immediately rather than coasting on the previous success until it would have expired
// (§16.5a). It is the burn flavour of `writeAlertErrorTx`, and it deliberately shares its one
// omission: `firing` is never touched. An evaluation that could not run is not evidence that the
// budget stopped burning, and resolving a live alert on it would be the worst available lie.
//
// `ruleKeys` names the rules the current configuration declares. When it is empty — the only case is
// a `burn_rules` payload that would not decode — the write falls back to every row the target
// already had, because the alternative is leaving an armed latch fresh under a configuration nobody
// can read.
func disarmBurnTargetTx(
	ctx context.Context, tx pgx.Tx, c burnCandidate, ruleKeys []string, asOf time.Time,
	reason, lastError string,
) error {
	if len(ruleKeys) == 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE service_burn_alert_state
			   SET last_verdict = 'hold', last_reason = $4, evaluated_at = $5,
			       lease_until = $5, last_error = $6
			 WHERE service_id = $1 AND project_id = $2 AND sla_target_id = $3
			   AND evaluated_at <= $5`,
			c.serviceID, c.projectID, c.targetID, reason, asOf, lastError); err != nil {
			return fmt.Errorf("store: dis-arm burn target: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_burn_alert_state
		  (service_id, project_id, sla_target_id, rule_key, firing, last_verdict, last_reason,
		   target_generation, config_generation, evaluated_at, lease_until, last_error)
		SELECT $1::uuid, $2::uuid, $3::uuid, k, false, 'hold', $4::text,
		       $5::bigint, $6::bigint, $7::timestamptz, $7::timestamptz, $8::text
		  FROM unnest($9::text[]) AS k
		ON CONFLICT (service_id, project_id, sla_target_id, rule_key) DO UPDATE SET
		   last_verdict = 'hold',
		   last_reason = EXCLUDED.last_reason,
		   evaluated_at = EXCLUDED.evaluated_at,
		   -- The lease collapses to the evaluation instant: dis-armed NOW, not when the previous
		   -- success would have run out.
		   lease_until = EXCLUDED.evaluated_at,
		   last_error = EXCLUDED.last_error
		 WHERE service_burn_alert_state.evaluated_at <= EXCLUDED.evaluated_at`,
		c.serviceID, c.projectID, c.targetID, reason, c.targetGeneration, c.configGeneration,
		asOf, lastError, ruleKeys); err != nil {
		return fmt.Errorf("store: dis-arm burn rules: %w", err)
	}
	return nil
}
