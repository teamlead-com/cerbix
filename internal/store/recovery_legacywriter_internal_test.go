package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// FR-043 §5, I13a (revision 7, owner's decision 2026-11-16): a writer that does not know about the
// recovery run — a binary without FR-043 during a rolling upgrade or after a rollback — ends the run
// by moving the monitor out of `down`, and never loses the check result. Migration 00118's BEFORE
// UPDATE trigger clears the run on any row whose new status is not `down`, so the run-state CHECK
// (monitors_recovery_run_state, not weakened) accepts the write.
//
// The statements below replay the status writers b823aca (pre-FR-043) had: recordCheckStatusTx
// (reached by every result path), the re-enable in updateMonitorTxRow, RetireMonitor,
// ReactivateMonitor, and SetMonitorStatus (test support). Disabling a monitor and a maintenance
// window write no status, then as now. How close each one is to the original:
//   - recordCheckStatusTx: the statement verbatim, its SQL comments dropped; maintEffectiveEnd is the
//     same constant b823aca used. The surrounding result transaction is reduced (legacyRecordResult).
//   - re-enable: only the part of b823aca's UPDATE that changes state on a disabled→enabled edit —
//     `enabled`, `updated_at`, the revision fence and the re-arm CASEs (push_armed_at, status,
//     consecutive_failures, state_sequence), copied verbatim. The edited columns (name, target,
//     interval and the rest) and `RETURNING monitorColumns` are left out; they do not touch the run.
//   - RetireMonitor, ReactivateMonitor: the SET and WHERE verbatim, without `RETURNING monitorColumns`.
//   - SetMonitorStatus: verbatim, without `RETURNING prev.old`.
// RETURNING reads the row after the trigger and the CHECK, so leaving it out changes nothing tested.

// legacyRecordCheckStatusSQL is b823aca's recordCheckStatusTx statement, unchanged.
var legacyRecordCheckStatusSQL = `WITH cur AS (
		   SELECT id, project_id, status AS old_status, consecutive_failures, failure_threshold
		   FROM monitors WHERE id = $1 FOR UPDATE
		 ),
		 maint AS (
		   SELECT EXISTS(
		     SELECT 1 FROM maintenance_windows mw, cur
		     WHERE mw.starts_at <= now() AND ` + maintEffectiveEnd + ` > now()
		       AND (mw.monitor_id = cur.id OR (mw.monitor_id IS NULL AND mw.project_id = cur.project_id))
		   ) AS in_maint
		 )
		 UPDATE monitors m SET
		   consecutive_failures = CASE WHEN $2 THEN 0 ELSE cur.consecutive_failures + 1 END,
		   status = CASE
		     WHEN $2 THEN 'up'
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold THEN 'down'
		     ELSE m.status
		   END,
		   last_notified_at = CASE
		     WHEN $2 THEN NULL
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold AND cur.old_status <> 'down' THEN now()
		     ELSE m.last_notified_at
		   END,
		   updated_at = now()
		 FROM cur, maint
		 WHERE m.id = cur.id
		 RETURNING cur.old_status, m.status, maint.in_maint, m.consecutive_failures, m.failure_threshold, m.confirm_interval_seconds`

// legacyRecordResult is b823aca's RecordScheduledResult for a fresh result, reduced to the statements
// that touch the row and the heartbeat, in its order and in ONE transaction: the row lock, the
// heartbeat insert, the status write, the watermark. A refused status write rolls the heartbeat back
// with it — the loss the trigger exists to prevent.
func legacyRecordResult(ctx context.Context, st *Store, monitorID string, ts time.Time, up bool) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var lastTs *time.Time
	if err := tx.QueryRow(ctx, `SELECT last_result_ts FROM monitors WHERE id = $1 FOR UPDATE`, monitorID).Scan(&lastTs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$2) ON CONFLICT (monitor_id, ts) DO NOTHING`,
		monitorID, ts, up, int64(12), 200, "legacy"); err != nil {
		return err
	}
	var prev, cur string
	var inMaint bool
	var fails, threshold, confirm int
	if err := tx.QueryRow(ctx, legacyRecordCheckStatusSQL, monitorID, up).
		Scan(&prev, &cur, &inMaint, &fails, &threshold, &confirm); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitors SET last_result_ts = $2 WHERE id = $1`, monitorID, ts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func heartbeatExists(t *testing.T, st *Store, ctx context.Context, monitorID string, ts time.Time) bool {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1 AND ts = $2`, monitorID, ts).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A run in progress, reached through the new code: down, then two successes at threshold 3.
func monitorMidRun(t *testing.T, st *Store, ctx context.Context, slug string) *recoveryMonitor {
	t.Helper()
	m := newRecoveryMonitor(t, st, ctx, recoveryProject(t, st, ctx, slug), slug, 3, 1)
	m.feed(false)
	m.feed(true)
	m.feed(true)
	if w := m.row(); w.Status != "down" || w.Successes != 2 || w.StartedAt == nil {
		t.Fatalf("setup: %+v, want down with a run of 2", w)
	}
	return m
}

// The legacy success on a monitor in a run: up at once (the threshold does not apply to an old
// binary), the heartbeat kept, the run cleared, no 23514.
func TestLegacyWriterSuccessEndsTheRunAndKeepsTheResult(t *testing.T) {
	st, ctx := outboxTestStore(t)
	m := monitorMidRun(t, st, ctx, "lw-success")
	ts := m.next.Add(time.Second)
	if err := legacyRecordResult(ctx, st, m.ID, ts, true); err != nil {
		t.Fatalf("the pre-FR-043 result write was refused (code %s): %v — and its heartbeat with it", pgErrCode(err), err)
	}
	if !heartbeatExists(t, st, ctx, m.ID, ts) {
		t.Fatal("the legacy result's heartbeat is missing")
	}
	if w := m.row(); w.Status != "up" || w.Successes != 0 || w.StartedAt != nil {
		t.Fatalf("after the legacy success: %+v, want up / 0 / NULL", w)
	}
}

// The legacy failure on a monitor in a run: the old code keeps it down and does not know the run; the
// row stays valid and the heartbeat is kept.
func TestLegacyWriterFailureMidRunKeepsTheRowValid(t *testing.T) {
	st, ctx := outboxTestStore(t)
	m := monitorMidRun(t, st, ctx, "lw-failure")
	ts := m.next.Add(time.Second)
	if err := legacyRecordResult(ctx, st, m.ID, ts, false); err != nil {
		t.Fatalf("the pre-FR-043 failure write was refused (code %s): %v", pgErrCode(err), err)
	}
	if !heartbeatExists(t, st, ctx, m.ID, ts) {
		t.Fatal("the legacy failure's heartbeat is missing")
	}
	if w := m.row(); w.Status != "down" {
		t.Fatalf("after the legacy failure: %+v, want down", w)
	} // readRecoveryRow asserts I13 on every read: the row is a valid one
}

// Every other pre-FR-043 transition out of down, each on a monitor in a run.
func TestLegacyWritersLeavingDownEndTheRun(t *testing.T) {
	st, ctx := outboxTestStore(t)
	projectID := recoveryProject(t, st, ctx, "lw-writers")

	exec := func(sql string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		}
	}
	for _, c := range []struct {
		name      string
		composite bool // RetireMonitor and ReactivateMonitor are scoped to composites
		before    string
		write     func(id string) func(context.Context, pgx.Tx) error
		want      string
	}{
		{
			// b823aca updateMonitorTxRow, the re-arm half of the statement ($2 = the new `enabled`).
			name: "re-enable", before: `UPDATE monitors SET enabled = false WHERE id = $1`,
			write: func(id string) func(context.Context, pgx.Tx) error {
				return exec(`UPDATE monitors
				    SET enabled = $2, updated_at = now(),
				        `+revisionFenceSetSQL+`,
				        push_armed_at = CASE WHEN type = 'push' AND NOT enabled AND $2 THEN statement_timestamp() ELSE push_armed_at END,
				        status = CASE WHEN NOT enabled AND $2 THEN 'pending' ELSE status END,
				        consecutive_failures = CASE WHEN NOT enabled AND $2 THEN 0 ELSE consecutive_failures END,
				        state_sequence = CASE WHEN NOT enabled AND $2 THEN state_sequence + 1 ELSE state_sequence END
				  WHERE id = $1`, id, true)
			},
			want: "pending",
		},
		{
			name: "RetireMonitor", composite: true,
			write: func(id string) func(context.Context, pgx.Tx) error {
				return exec(`
		UPDATE monitors
		   SET retired_at = now(), enabled = false, updated_at = now(),
		       status = 'pending', consecutive_failures = 0,
		       state_sequence = state_sequence + 1,
		       `+revisionFenceSetSQL+`
		 WHERE id = $1 AND project_id = $2`, id, projectID)
			},
			want: "pending",
		},
		{
			name: "ReactivateMonitor", composite: true,
			before: `UPDATE monitors SET retired_at = now(), enabled = false WHERE id = $1`,
			write: func(id string) func(context.Context, pgx.Tx) error {
				return exec(`
		UPDATE monitors
		   SET retired_at = NULL, enabled = true, updated_at = now(),
		       status = 'pending', consecutive_failures = 0,
		       state_sequence = state_sequence + 1,
		       push_armed_at = CASE WHEN type = 'push' THEN statement_timestamp() ELSE push_armed_at END,
		       `+revisionFenceSetSQL+`
		 WHERE id = $1 AND project_id = $2`, id, projectID)
			},
			want: "pending",
		},
		{
			name: "SetMonitorStatus up",
			write: func(id string) func(context.Context, pgx.Tx) error {
				return exec(`WITH prev AS (SELECT id, status AS old FROM monitors WHERE id = $1)
		 UPDATE monitors m SET status = $2, updated_at = now()
		 FROM prev WHERE m.id = prev.id`, id, "up")
			},
			want: "up",
		},
		{
			name: "SetMonitorStatus pending",
			write: func(id string) func(context.Context, pgx.Tx) error {
				return exec(`WITH prev AS (SELECT id, status AS old FROM monitors WHERE id = $1)
		 UPDATE monitors m SET status = $2, updated_at = now()
		 FROM prev WHERE m.id = prev.id`, id, "pending")
			},
			want: "pending",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var id string
			if c.composite {
				id = recoveryComposite(t, st, projectID)
			} else {
				id = newRecoveryMonitor(t, st, ctx, projectID, "lw-"+c.name, 3, 1).ID
			}
			if c.before != "" {
				if _, err := st.pool.Exec(ctx, c.before, id); err != nil {
					t.Fatal(err)
				}
			}
			seedRun(t, st, ctx, id, 2)
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
			if err := c.write(id)(ctx, tx); err != nil {
				t.Fatalf("the pre-FR-043 %s write was refused (code %s): %v", c.name, pgErrCode(err), err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if w := readRecoveryRow(t, st, ctx, id); w.Status != c.want || w.Successes != 0 || w.StartedAt != nil {
				t.Fatalf("after the legacy %s: %+v, want %s / 0 / NULL", c.name, w, c.want)
			}
		})
	}
}
