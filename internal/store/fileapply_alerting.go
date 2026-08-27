package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.6a — applying the `alerting:` declaration of a format-2 bundle.
//
// The UI's writer, `UpdateServiceAlertPolicy`, REFUSES a file-managed service on purpose: these
// fields are part of the desired state, so the file owns them and the UI renders them
// read-only. The file apply therefore needs its own write, and it lives in its own file rather
// than beside the UI one so that the refusal there stays unconditional — a shared entry point
// with an "is this the file provider?" flag is exactly how a UI path eventually slips through.
//
// What it does NOT duplicate is everything that matters:
//
//   - the bounds are the ONE domain validator's (§16.6a), never restated here;
//   - the canonical form is the domain's `Canonical()`, so a `page_on` written in a different
//     order is not a change in one path and a change in the other;
//   - the §16.4a closes go through `closeServiceEpisodesTx`, the single closing path in this
//     package, in the caller's transaction — an operator who removes `owns_paging: true` from a
//     file has destroyed the very rows an evaluator would need to notice the announcement is
//     over, so the ending is enqueued WITH the edit or not at all;
//   - `alert_config_generation` is never written. Migration 00082's trigger owns it, because a
//     PATCH, a MaC apply and a direct UPDATE must all dis-arm delegation, and "every writer
//     remembers to bump" is the assumption phase 4 had to remove twice.
//
// The audit is written here rather than left to the file apply's coarse per-bundle row: §16.6a
// requires EVERY paging-config change to carry its actor and its before/after, and "gen=7
// update=1" does not say that ownership was switched off.
//
// Called from `applyBundleServicesTx` on all three branches — create, changed, and the unchanged-hash
// branch, which is the one an alerting-only edit arrives on, since the declaration hash deliberately
// excludes paging. The paragraph that used to stand here said this function was not yet wired; it was
// wired in the changeset that followed and the note outlived it by several iterations.

// applyServiceAlertingTx writes one service's paging declaration inside the transaction that is
// already applying its bundle, and ends any announcement the new declaration no longer covers.
//
// `declared` is nil when the bundle says NOTHING about paging, and nil is NOT the default
// policy: this returns immediately, writing nothing. A service that has ownership on and whose
// declaration loses its `alerting:` block keeps that ownership — silence is not a request to
// disown, exactly as a format-1 bundle's silence about `services` is not a request to delete
// them (§15.2). A service that never declared paging likewise cannot acquire it by omission.
//
// It reports whether anything was written, so the caller can keep its "did this apply change
// state?" accounting honest. Re-applying an unchanged declaration writes nothing at all: no row
// touch, no generation bump, no audit line.
//
// The caller must already hold the project's `service_membership` advisory lock and must call
// this while the service row exists; it takes the row lock itself, in the same order every other
// service write does.
func applyServiceAlertingTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID, providerID string,
	declared *domain.ServiceAlertPolicy,
) (bool, error) {
	if declared == nil {
		return false, nil
	}

	// Canonical THEN validate, both before any write and both through the domain — the same
	// order and the same owner as the UI path. A store that trusted its caller to have validated
	// would be one bad caller away from a `confirm_evaluations: 0` in the database, which the
	// CHECK would then refuse with a constraint name instead of a sentence.
	next := declared.Canonical()
	if err := next.Validate(); err != nil {
		return false, fmt.Errorf("store: %w", err)
	}

	// A LOCK-FREE look first. This function runs for every service of every bundle on every
	// scan — that is what makes it able to reconcile without a hash — and the overwhelmingly
	// common answer is "nothing changed". Taking a row lock to learn that would put a file
	// provider in contention with the alert evaluators, which now lock the same rows to
	// linearize against config writes (§16.7): every scan would cost the evaluator a
	// serialization failure for a file that declares exactly what the row already says.
	if same, err := alertPolicyMatchesTx(ctx, tx, projectID, serviceID, next); err != nil {
		return false, err
	} else if same {
		return false, nil
	}

	// Something differs, so now take the lock and re-read: the unlocked look above is a filter,
	// never the decision. Between it and this statement another writer may have made the change
	// itself, which the diff below then correctly reports as nothing to do.
	var before domain.ServiceAlertPolicy
	var pageOn []string
	var slug string
	err := tx.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations, renotify_seconds, slug
		  FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`, serviceID, projectID).
		Scan(&before.OwnsPaging, &pageOn, &before.PageOnUnknown, &before.ConfirmEvaluations,
			&before.RenotifySeconds, &slug)
	if noRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: lock service for file alerting apply: %w", err)
	}
	for _, state := range pageOn {
		before.PageOn = append(before.PageOn, domain.ServiceAlertState(state))
	}
	// The stored value is canonicalized for the comparison too, so a re-spelling of the same
	// declaration is never read as a policy change that closes a firing announcement.
	before = before.Canonical()

	// Nothing moved: the reconcile is a no-op for this axis. Skipping the UPDATE is not just an
	// optimization — writing the same values back would still bump `updated_at` on every scan,
	// and the audit trail would fill with lines that record no decision.
	diff := alertPolicyDiff(before, next)
	if diff == "" {
		return false, nil
	}

	// The clock is read AFTER the lock is held, and it is `statement_timestamp()` rather than
	// `now()`: `now()` is transaction-START time, and a bundle apply that queued behind an
	// evaluation would stamp its close with an instant BEFORE the episode that evaluation had
	// just opened — an announcement that ends before it begins is not a record anybody can read.
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return false, fmt.Errorf("store: file alerting apply clock: %w", err)
	}

	// ── The §16.4a closes, from the BEFORE state, in the caller's transaction ─────────────────
	//
	// Identical to the UI path's table, because it is the same table: a close is a statement
	// about what we are still allowed to say, and which surface edited the declaration does not
	// change what the operator was told.
	//
	//	owns_paging true → false   every open episode, both signals, `ownership_disabled`
	//	page_on / page_on_unknown  the open HEALTH episode, when the state it announced is no
	//	                           longer pageable, `policy_changed`
	//	confirm_evaluations        NOTHING — confirmation governs the ONSET of an announcement,
	//	                           not one that is already open
	//	false → true ownership     NOTHING — there is nothing open to end
	switch {
	case before.OwnsPaging && !next.OwnsPaging:
		if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
			episodeCloseFilter{}, domain.CloseOwnershipDisabled); err != nil {
			return false, err
		}
	case pagingStatesChanged(before, next):
		// The health episode records the state it announced. If the new declaration would not
		// page that state, the author has withdrawn the statement — `policy_changed`, never a
		// recovery: nothing here is evidence about the service. The burn signal is deliberately
		// untouched; `page_on` is the LIVE policy and says nothing about a budget.
		var announced string
		err := tx.QueryRow(ctx, `
			SELECT state FROM service_alert_episodes
			 WHERE service_id = $1 AND signal = 'health' AND closed_at IS NULL
			 LIMIT 1`, serviceID).Scan(&announced)
		if err != nil && !noRows(err) {
			return false, fmt.Errorf("store: read open health episode: %w", err)
		}
		if err == nil && !next.Pages(domain.ServiceAlertState(announced)) {
			if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
				episodeCloseFilter{signal: domain.ServiceSignalHealth}, domain.ClosePolicyChanged); err != nil {
				return false, err
			}
		}
	}

	// ── The write. Five columns, and never `alert_config_generation`: the trigger owns it. ────
	if _, err := tx.Exec(ctx, `
		UPDATE services
		   SET owns_paging = $3, page_on = $4, page_on_unknown = $5, confirm_evaluations = $6,
		       renotify_seconds = $7, updated_at = now()
		 WHERE id = $1 AND project_id = $2`,
		serviceID, projectID, next.OwnsPaging, pageOnText(next.PageOn), next.PageOnUnknown,
		next.ConfirmEvaluations, next.RenotifySeconds); err != nil {
		return false, fmt.Errorf("store: write file alerting declaration: %w", err)
	}

	// ── The audit, in the SAME transaction, naming the provider and only what moved ───────────
	//
	// A machine actor: there is no user behind a reconcile, and inventing one would make the
	// audit log lie about who decided. The provider id is what identifies the author instead,
	// the same identity the declaration rows carry as `file:<provider>`.
	if err := insertAlertAudit(ctx, tx, projectID, AlertActor{}, "service.alerting",
		"service="+serviceID+" provider=file:"+providerID+" "+diff); err != nil {
		return false, err
	}
	return true, nil
}

// alertPolicyMatchesTx reports whether the stored declaration already equals `want`, without
// taking any lock. It exists so the common case — a bundle re-declaring what is already true —
// costs one indexed read and no contention with the evaluators.
func alertPolicyMatchesTx(
	ctx context.Context, tx pgx.Tx, projectID, serviceID string, want domain.ServiceAlertPolicy,
) (bool, error) {
	var have domain.ServiceAlertPolicy
	var pageOn []string
	err := tx.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations, renotify_seconds
		  FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).
		Scan(&have.OwnsPaging, &pageOn, &have.PageOnUnknown, &have.ConfirmEvaluations,
			&have.RenotifySeconds)
	if noRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: read alert policy: %w", err)
	}
	for _, state := range pageOn {
		have.PageOn = append(have.PageOn, domain.ServiceAlertState(state))
	}
	return alertPolicyDiff(have.Canonical(), want) == "", nil
}
