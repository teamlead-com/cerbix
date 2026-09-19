package metrics

import (
	"fmt"
	"maps"
)

var auditRetentionResults = map[string]bool{"deleted": true, "empty": true, "lock_busy": true, "error": true, "budget": true}

type auditRetentionMetrics struct {
	passes      map[string]uint64
	rowsDeleted uint64
	oldest      *float64
	lastSuccess *int64
}

func (r *Registry) RecordAuditRetentionPass(result string, deleted int) error {
	if !auditRetentionResults[result] || deleted < 0 {
		return fmt.Errorf("%w: audit retention result %q", ErrGateMetricLabel, result)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auditRetention.passes = bumpCount(r.auditRetention.passes, result)
	r.auditRetention.rowsDeleted += uint64(deleted)
	return nil
}

func (r *Registry) SetAuditRetentionGauges(oldestSeconds float64, lastSuccessUnix int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auditRetention.oldest = &oldestSeconds
	r.auditRetention.lastSuccess = &lastSuccessUnix
}

func (a auditRetentionMetrics) snapshot() auditRetentionMetrics {
	a.passes = maps.Clone(a.passes)
	if a.oldest != nil {
		value := *a.oldest
		a.oldest = &value
	}
	if a.lastSuccess != nil {
		value := *a.lastSuccess
		a.lastSuccess = &value
	}
	return a
}

func (a auditRetentionMetrics) write(w *prometheusWriter) {
	if len(a.passes) > 0 {
		w.println("# HELP cerbix_audit_retention_passes_total Audit-retention passes by bounded result.")
		w.println("# TYPE cerbix_audit_retention_passes_total counter")
		keys := sortedKeys(a.passes)
		for _, result := range keys {
			w.printf("cerbix_audit_retention_passes_total{result=%q} %d\n", result, a.passes[result])
		}
		w.println("# HELP cerbix_audit_retention_rows_deleted_total Audit rows deleted by bounded retention batches.")
		w.println("# TYPE cerbix_audit_retention_rows_deleted_total counter")
		w.printf("cerbix_audit_retention_rows_deleted_total %d\n", a.rowsDeleted)
	}
	if a.oldest != nil {
		w.println("# HELP cerbix_audit_retention_oldest_expired_seconds Age beyond the retention cutoff of the oldest surviving expired audit row.")
		w.println("# TYPE cerbix_audit_retention_oldest_expired_seconds gauge")
		w.printf("cerbix_audit_retention_oldest_expired_seconds %.3f\n", *a.oldest)
	}
	if a.lastSuccess != nil {
		w.println("# HELP cerbix_audit_retention_last_success_timestamp_seconds Database-clock timestamp of the last successful audit-retention pass.")
		w.println("# TYPE cerbix_audit_retention_last_success_timestamp_seconds gauge")
		w.printf("cerbix_audit_retention_last_success_timestamp_seconds %d\n", *a.lastSuccess)
	}
}
