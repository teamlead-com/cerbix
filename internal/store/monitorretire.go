package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.5 — the composite lifecycle.
//
// The owner's decision (D-0167): a composite monitor whose meaning a service now expresses stays
// VISIBLE and ACTIVE. Nothing is hidden by default and nothing is migrated behind an operator's
// back; what the integration adds is a two-ended LINK ("this monitor is superseded by that
// service", "this service supersedes that monitor") and, separately, an explicit retire action.
//
// Retiring is therefore two statements that must not drift apart:
//
//   - `retired_at` is the LIFECYCLE fact — an operator ended this monitor's working life;
//   - `enabled = false` is the EXECUTION consequence, because the scheduler, the dead-man
//     window, ingest and the SLO paths all key on `enabled`. Setting only `retired_at` would
//     leave a monitor that reads "retired" in the UI while still probing targets and paging
//     on-call — the exact class of half-applied lifecycle change §15.5 forbids.
//
// The same transaction also applies the D-0142 config fence and the disable-side state reset, so
// a retire cannot leave an undelivered DOWN to arrive after the monitor stopped existing to the
// operator.

// ErrMonitorAlreadyRetired is returned when a retire targets a monitor that is already retired.
// It is not an error the UI needs to explain away — it means someone else did it first.
var ErrMonitorAlreadyRetired = errors.New("store: monitor is already retired")

// ErrMonitorNotRetired is returned when a reactivate targets a monitor that was never retired.
var ErrMonitorNotRetired = errors.New("store: monitor is not retired")

// ErrNotAComposite is returned when a composite-lifecycle action targets another monitor type.
// §15.5 is a COMPOSITE section — "Retire is available for ANY composite" — so offering these
// actions for every monitor type would be surface this phase was never authorized to add. Widening
// them is an owner decision, not an implementation detail.
var ErrNotAComposite = errors.New("store: this action applies to composite monitors only")

// ErrSuccessorNotAService is returned when the named successor is not a service of the SAME
// project. Cross-project supersession would let a page in one project quietly claim a monitor in
// another, and the composite FK is what makes that impossible at rest.
var ErrSuccessorNotAService = errors.New("store: successor must be a service in the same project")

// SetMonitorSuccessor records (or clears, with an empty serviceID) the service that now expresses
// what this monitor was built to express. It is an ANNOTATION: the monitor keeps probing, keeps
// alerting, keeps its own history, and its status page component keeps rendering it. Nothing
// about this call changes what a customer sees — which is precisely why it is separate from
// retiring, and why it is safe to offer as the default step.
// FILE OWNERSHIP, deliberately asymmetric ([316] disposition): a file-managed composite MAY be
// annotated, while retire/reactivate below refuse. The line is declaration authority, not the row:
// `superseded_by_service_id` does not exist in bundle format 2, enters no canonical hash and no
// generation, and cannot be restated by a reconcile — so refusing it would remove the operator's
// only way to annotate a file-managed composite while protecting nothing. `enabled` is the
// opposite: it IS declared, so a UI write there is one the next apply would silently overwrite.
func (s *Store) SetMonitorSuccessor(ctx context.Context, projectID, monitorID, serviceID string, actor GraphActor) (domain.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: begin set successor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := assertCompositeTx(ctx, tx, projectID, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	if serviceID != "" {
		var one int
		err := tx.QueryRow(ctx,
			`SELECT 1 FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&one)
		if noRows(err) {
			return domain.Monitor{}, ErrSuccessorNotAService
		}
		if err != nil {
			return domain.Monitor{}, fmt.Errorf("store: resolve successor: %w", err)
		}
	}
	// No execution fence here on purpose: the link changes no probe input, so bumping
	// execution_revision would force a needless re-probe of every annotated monitor.
	row := tx.QueryRow(ctx,
		`UPDATE monitors SET superseded_by_service_id = $3, updated_at = now()
		  WHERE id = $1 AND project_id = $2 RETURNING `+monitorColumns,
		monitorID, projectID, nullableID(serviceID))
	m, err := s.scanMonitorNoSecrets(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: set monitor successor: %w", err)
	}
	action, target := "monitor.successor.set", "monitor="+monitorID+" service="+serviceID
	if serviceID == "" {
		action, target = "monitor.successor.cleared", "monitor="+monitorID
	}
	if err := auditProjectActTx(ctx, tx, projectID, action, target+" actor="+actor.Label, actor); err != nil {
		return domain.Monitor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Monitor{}, fmt.Errorf("store: commit set successor: %w", err)
	}
	return m, nil
}

// RetireMonitor ends a monitor's working life in ONE transaction: the lifecycle fact, the
// execution switch, the config fence and the live-state reset together.
//
// It does NOT delete anything. The monitor's heartbeats, incidents and SLO history stay exactly
// where they are — retiring says "stop probing and stop paging", never "this never happened".
// A retired monitor is still readable, still reportable, and can be reactivated.
func (s *Store) RetireMonitor(ctx context.Context, projectID, monitorID string, actor GraphActor) (domain.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: begin retire monitor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The row is locked before the check so two concurrent retires cannot both audit an act only
	// one of them performed.
	var retired *string
	err = tx.QueryRow(ctx,
		`SELECT retired_at::text FROM monitors WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		monitorID, projectID).Scan(&retired)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: lock monitor: %w", err)
	}
	if retired != nil {
		return domain.Monitor{}, ErrMonitorAlreadyRetired
	}
	if err := assertCompositeTx(ctx, tx, projectID, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	// A file-owned monitor's lifecycle belongs to its file: retiring it here would be restated by
	// the very next reconcile, so the refusal is the honest answer.
	if err := assertNotFileManagedTx(ctx, tx, monitorID); err != nil {
		return domain.Monitor{}, err
	}

	// `state_sequence` is bumped so an outbox event enqueued before this moment is dropped as
	// superseded at delivery: a DOWN notification arriving after the operator retired the monitor
	// would page someone about a monitor that no longer runs.
	row := tx.QueryRow(ctx, `
		UPDATE monitors
		   SET retired_at = now(), enabled = false, updated_at = now(),
		       status = 'pending', consecutive_failures = 0,
		       state_sequence = state_sequence + 1,
		       `+revisionFenceSetSQL+`
		 WHERE id = $1 AND project_id = $2 RETURNING `+monitorColumns,
		monitorID, projectID)
	m, err := s.scanMonitorNoSecrets(row)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: retire monitor: %w", err)
	}
	// §6.2 fan-out, the half `retired_at` alone would miss: a service whose SLI names this
	// monitor now executes under different semantics — its input stopped producing observations —
	// so every referencing service opens a new evaluation epoch in THIS transaction. Without it,
	// facts computed after the retire would be attributed to the epoch that described a running
	// monitor, and the two axes would disagree about the same write.
	if err := s.BumpEpochsForMonitor(ctx, tx, projectID, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	if err := auditProjectActTx(ctx, tx, projectID, "monitor.retired",
		"monitor="+monitorID+" actor="+actor.Label, actor); err != nil {
		return domain.Monitor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Monitor{}, fmt.Errorf("store: commit retire monitor: %w", err)
	}
	return m, nil
}

// ReactivateMonitor undoes a retire: it clears the lifecycle fact and re-enables execution,
// re-arming exactly the way UpdateMonitor's disabled→enabled transition does.
//
// Reactivation deliberately does NOT restore a pre-retire status. The monitor starts pending: the
// last observation before retirement is not evidence about the target now, and fabricating a
// `last_result_ts` is what would produce a false dead-man DOWN on the first tick.
func (s *Store) ReactivateMonitor(ctx context.Context, projectID, monitorID string, actor GraphActor) (domain.Monitor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: begin reactivate monitor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var retired *string
	err = tx.QueryRow(ctx,
		`SELECT retired_at::text FROM monitors WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		monitorID, projectID).Scan(&retired)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: lock monitor: %w", err)
	}
	if retired == nil {
		return domain.Monitor{}, ErrMonitorNotRetired
	}
	if err := assertCompositeTx(ctx, tx, projectID, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	if err := assertNotFileManagedTx(ctx, tx, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	row := tx.QueryRow(ctx, `
		UPDATE monitors
		   SET retired_at = NULL, enabled = true, updated_at = now(),
		       status = 'pending', consecutive_failures = 0,
		       state_sequence = state_sequence + 1,
		       -- A push monitor's dead-man window restarts from the re-enable moment; a poll
		       -- monitor's watermark is cleared by the fence below. Both are the same rule as
		       -- UpdateMonitor's re-arm: liveness is proven after the enable, never before it.
		       push_armed_at = CASE WHEN type = 'push' THEN statement_timestamp() ELSE push_armed_at END,
		       `+revisionFenceSetSQL+`
		 WHERE id = $1 AND project_id = $2 RETURNING `+monitorColumns,
		monitorID, projectID)
	m, err := s.scanMonitorNoSecrets(row)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: reactivate monitor: %w", err)
	}
	// §6.2 fan-out, the half `retired_at` alone would miss: a service whose SLI names this
	// monitor now executes under different semantics — its input stopped producing observations —
	// so every referencing service opens a new evaluation epoch in THIS transaction. Without it,
	// facts computed after the retire would be attributed to the epoch that described a running
	// monitor, and the two axes would disagree about the same write.
	if err := s.BumpEpochsForMonitor(ctx, tx, projectID, monitorID); err != nil {
		return domain.Monitor{}, err
	}
	if err := auditProjectActTx(ctx, tx, projectID, "monitor.reactivated",
		"monitor="+monitorID+" actor="+actor.Label, actor); err != nil {
		return domain.Monitor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Monitor{}, fmt.Errorf("store: commit reactivate monitor: %w", err)
	}
	return m, nil
}

// auditProjectActTx writes ONE audit row for a project-scoped lifecycle act, resolving the org
// from the project so a caller cannot mis-attribute the act to another tenant.
func auditProjectActTx(ctx context.Context, tx pgx.Tx, projectID, action, target string, actor GraphActor) error {
	var actorUserID *string
	if actor.UserID != "" {
		actorUserID = &actor.UserID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, $2, $3, $4, $5 FROM projects p WHERE p.id = $1`,
		projectID, actorUserID, actor.ViaToken, action, target); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}

// assertCompositeTx confirms the target is a composite in this project. It reads the type only —
// the callers that need the whole row already have it — so it is cheap enough to sit in front of
// every lifecycle action rather than being remembered per call site.
func assertCompositeTx(ctx context.Context, tx pgx.Tx, projectID, monitorID string) error {
	var typ string
	err := tx.QueryRow(ctx,
		`SELECT type FROM monitors WHERE id = $1 AND project_id = $2`, monitorID, projectID).Scan(&typ)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read monitor type: %w", err)
	}
	if domain.MonitorType(typ) != domain.MonitorComposite {
		return fmt.Errorf("%w: %s is a %s monitor", ErrNotAComposite, monitorID, typ)
	}
	return nil
}
