package scheduler

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/store"
)

type AuditRetentionSink interface {
	RecordAuditRetentionPass(result string, deleted int) error
	SetAuditRetentionGauges(oldestSeconds float64, lastSuccessUnix int64)
}

type auditRetentionRunner interface {
	RunAuditRetentionPass(context.Context, store.AuditRetentionConfig) (store.AuditRetentionReport, bool, string, error)
}

func (s *Scheduler) WithAuditRetention(cfg store.AuditRetentionConfig, sink AuditRetentionSink) *Scheduler {
	s.auditRetentionCfg = cfg
	s.auditRetentionMetrics = sink
	return s
}

func (s *Scheduler) auditRetentionEnabled() bool { return s.auditRetentionCfg.PurgeEvery > 0 }

func (s *Scheduler) auditRetentionLoop(ctx context.Context) {
	runner, ok := s.store.(auditRetentionRunner)
	if !ok {
		s.logger.Error("audit_retention_unavailable")
		return
	}
	run := func() {
		report, acquired, result, err := runner.RunAuditRetentionPass(ctx, s.auditRetentionCfg)
		if s.auditRetentionMetrics != nil {
			_ = s.auditRetentionMetrics.RecordAuditRetentionPass(result, report.Deleted)
		}
		if err != nil {
			s.logger.Warn("audit_retention_pass_failed", "result", result, "error", err.Error())
			return
		}
		if !acquired {
			s.logger.Debug("audit_retention_skipped_not_acquired")
			return
		}
		if s.auditRetentionMetrics != nil {
			s.auditRetentionMetrics.SetAuditRetentionGauges(report.OldestExpiredSecond, report.LastSuccessUnix)
		}
		s.logger.Info("audit_retention_pass", "result", result, "deleted", report.Deleted, "oldest_expired_seconds", report.OldestExpiredSecond)
	}
	run()
	ticker := time.NewTicker(s.auditRetentionCfg.PurgeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
