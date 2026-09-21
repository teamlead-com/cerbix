package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditRetentionMaintenanceLockKey is a maintenance-only advisory-lock slot, separate from
// scheduler leadership, migrations, and the decision-ledger maintenance owner.
const AuditRetentionMaintenanceLockKey int64 = 0x6365726269780004

const auditRetentionPassBudget = 30 * time.Second

// AuditRetentionConfig is validated at bootstrap and then consumed as one immutable runtime
// snapshot by the retention loop.
type AuditRetentionConfig struct {
	RetentionDays  int
	PurgeEvery     time.Duration
	PurgeBatchRows int
}

type AuditRetentionReport struct {
	Deleted             int
	OldestExpiredSecond float64
	LastSuccessUnix     int64
}

// RunAuditRetentionPass removes expired audit evidence in bounded batches on a fenced,
// pinned advisory session. The cutoff comes from PostgreSQL once per pass and is reused for all
// batches; no tenant selector is accepted or used.
func (s *Store) RunAuditRetentionPass(ctx context.Context, cfg AuditRetentionConfig) (AuditRetentionReport, bool, string, error) {
	passCtx, cancel := context.WithTimeout(ctx, auditRetentionPassBudget)
	defer cancel()
	ls, acquired, err := s.TryBecomeLeaderSession(passCtx, AuditRetentionMaintenanceLockKey)
	if err != nil {
		return AuditRetentionReport{}, false, "error", err
	}
	if !acquired {
		return AuditRetentionReport{}, false, "lock_busy", nil
	}
	defer ls.Release()

	var cutoff time.Time
	if err := ls.conn.QueryRow(passCtx,
		`SELECT clock_timestamp() - make_interval(days => $1)`, cfg.RetentionDays).Scan(&cutoff); err != nil {
		return AuditRetentionReport{}, true, "error", fmt.Errorf("store: audit retention cutoff: %w", err)
	}
	return runAuditRetentionBatches(passCtx, ls.conn, cfg, cutoff)
}

func runAuditRetentionBatches(
	ctx context.Context,
	conn *pgxpool.Conn,
	cfg AuditRetentionConfig,
	cutoff time.Time,
) (AuditRetentionReport, bool, string, error) {
	report := AuditRetentionReport{LastSuccessUnix: cutoff.AddDate(0, 0, cfg.RetentionDays).Unix()}
	for {
		if ctx.Err() != nil {
			return report, true, "budget", ctx.Err()
		}
		var deleted int
		err := conn.QueryRow(ctx, `
			WITH candidates AS (
				SELECT id FROM audit_logs
				 WHERE created_at < $1
				 ORDER BY created_at, id
				 LIMIT $2
				 FOR UPDATE SKIP LOCKED
			), removed AS (
				DELETE FROM audit_logs a USING candidates c WHERE a.id = c.id RETURNING 1
			) SELECT count(*) FROM removed`, cutoff, cfg.PurgeBatchRows).Scan(&deleted)
		if err != nil {
			return report, true, "error", fmt.Errorf("store: audit retention delete: %w", err)
		}
		report.Deleted += deleted
		if deleted < cfg.PurgeBatchRows {
			break
		}
	}
	if err := conn.QueryRow(ctx,
		`SELECT COALESCE(EXTRACT(EPOCH FROM ($1::timestamptz - MIN(created_at))), 0)
		   FROM audit_logs WHERE created_at < $1`, cutoff).Scan(&report.OldestExpiredSecond); err != nil {
		return report, true, "error", fmt.Errorf("store: audit retention backlog: %w", err)
	}
	if report.Deleted == 0 {
		return report, true, "empty", nil
	}
	return report, true, "deleted", nil
}
