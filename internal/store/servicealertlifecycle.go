package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.4a — closing an announcement when the thing that fired is EDITED AWAY.
//
// The evaluator closes what recovers. Everything else that can end a firing announcement is a
// lifecycle event — ownership switched off, a policy that no longer covers the state, a burn target
// disabled, a rule removed, the service deleted — and those all destroy the very rows an evaluator
// would need to notice. So the close is enqueued IN THE SAME TRANSACTION as the removal, from the
// EPISODE rather than from the live configuration:
//
//   - the recipients are the episode's immutable onset snapshot, so the close reaches the people who
//     heard the onset even when the schedule has rotated or the service is gone;
//   - the service name, the target window and the rule key are the episode's own copies, so the
//     message still reads correctly after the target it names no longer exists;
//   - the sequence continues the episode's, so delivery can order the close against its onset.
//
// A close is never a claim about the service. `recovered` is the only reason that says the burn
// stopped or the outage ended; every reason here says a HUMAN changed what we are allowed to say,
// and the channel renders them as themselves (§16.4a) so nobody reads "we stopped watching" as "it
// is fine now".

// episodeCloseFilter narrows WHICH open episodes a lifecycle event ends. The zero value means every
// open episode of the service, which is what deleting it or disowning it implies.
type episodeCloseFilter struct {
	// signal limits the close to one signal. Empty closes both.
	signal domain.ServiceAlertSignal
	// targetID limits the close to episodes snapshotted from ONE burn target. Empty means any.
	targetID string
	// keepRuleKeys, when non-nil, closes the burn episodes whose rule key is NOT in the list — the
	// shape of "these are the rules that still exist". A non-nil EMPTY slice therefore closes every
	// rule of the scope, which is what removing the last rule means. Nil disables the filter, which
	// is why it is a slice and not a variadic.
	keepRuleKeys []string
}

// closeServiceEpisodesTx closes every open episode matching the filter and enqueues ONE close event
// per episode, in the caller's transaction. It returns how many announcements it ended.
//
// It must be called BEFORE the removal it accompanies: the episode rows are read through
// `service_id`, and once the service row is gone the database has (correctly) nulled that column.
func closeServiceEpisodesTx(
	ctx context.Context, tx pgx.Tx, asOf time.Time,
	serviceID, projectID, serviceSlug string,
	filter episodeCloseFilter, reason domain.ServiceAlertCloseReason,
) (int, error) {
	// One statement closes and hands back what the payloads need. Doing it as UPDATE ... RETURNING
	// rather than SELECT-then-UPDATE is what makes two concurrent lifecycle writers unable to both
	// close the same episode and enqueue two endings for one announcement.
	rows, err := tx.Query(ctx, `
		UPDATE service_alert_episodes
		   SET closed_at = $2, close_reason = $3
		 WHERE service_id = $1
		   AND closed_at IS NULL
		   AND ($4 = '' OR signal = $4)
		   AND ($5 = '' OR COALESCE(target_snapshot_id::text, '') = $5)
		   AND ($6::text[] IS NULL OR signal <> 'burn' OR NOT (rule_key = ANY($6::text[])))
		 RETURNING id, service_name, signal, COALESCE(rule_key, ''),
		           COALESCE(target_snapshot_id::text, ''), COALESCE(target_window, ''),
		           ARRAY(SELECT jsonb_array_elements_text(recipients)), emitted_seq, state`,
		serviceID, asOf, string(reason), string(filter.signal), filter.targetID, filter.keepRuleKeys)
	if err != nil {
		return 0, fmt.Errorf("store: close alert episodes: %w", err)
	}
	type ending struct {
		id, name, signal, ruleKey, targetID, window, state string
		recipients                                         []string
		seq                                                int64
	}
	var endings []ending
	for rows.Next() {
		var e ending
		if err := rows.Scan(&e.id, &e.name, &e.signal, &e.ruleKey, &e.targetID, &e.window,
			&e.recipients, &e.seq, &e.state); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan closed episode: %w", err)
		}
		endings = append(endings, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: close alert episodes: %w", err)
	}

	for _, e := range endings {
		signal := domain.ServiceAlertSignal(e.signal)
		// The close continues the episode's sequence. Delivery never DROPS a close on this gate, but
		// the number is what lets a channel order the ending against the onset it ends.
		seq := e.seq + 1
		alert := domain.ServiceAlert{
			ServiceID: serviceID, ProjectID: projectID, ServiceName: e.name, ServiceSlug: serviceSlug,
			Signal: signal, Firing: false, CloseReason: reason,
			EpisodeID: e.id, Seq: seq, Recipients: e.recipients,
		}
		if signal == domain.ServiceSignalBurn {
			alert.SLATargetID, alert.RuleKey, alert.Window = e.targetID, e.ruleKey, e.window
		} else {
			alert.State = domain.ServiceAlertState(e.state)
		}
		payload, err := json.Marshal(alert)
		if err != nil {
			return 0, fmt.Errorf("store: marshal lifecycle close: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox_events (topic, payload) VALUES ($1, $2)`,
			domain.TopicServiceAlert, payload); err != nil {
			return 0, fmt.Errorf("store: enqueue lifecycle close: %w", err)
		}

		// The LEVEL follows the announcement. A latch left FIRING with no open episode would let a
		// later recovery emit a close for an announcement that already ended, and — after the
		// configuration is restored — would swallow the next onset as "no edge". Rows whose target or
		// service is being deleted take care of themselves through the cascade; the UPDATE simply
		// matches nothing.
		switch signal {
		case domain.ServiceSignalBurn:
			if _, err := tx.Exec(ctx, `
				UPDATE service_burn_alert_state
				   SET firing = false, emitted_seq = $4, emitted_at = $5
				 WHERE service_id = $1 AND sla_target_id = $2::uuid AND rule_key = $3`,
				serviceID, e.targetID, e.ruleKey, seq, asOf); err != nil {
				return 0, fmt.Errorf("store: clear burn latch: %w", err)
			}
		default:
			if _, err := tx.Exec(ctx, `
				UPDATE service_alert_state
				   SET live_firing = false, emitted_seq = $2, emitted_at = $3
				 WHERE service_id = $1`, serviceID, seq, asOf); err != nil {
				return 0, fmt.Errorf("store: clear live latch: %w", err)
			}
		}
	}
	return len(endings), nil
}
