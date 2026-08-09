package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrManagedByFile is returned by the user-managed write paths (UpdateMonitor/DeleteMonitor)
// when the target monitor is owned by a file provider. Declarative fields of a file-managed
// monitor are read-only through normal CRUD (spec func-monitoring-as-code §8); the API maps
// this to 409 with tenant-safe provenance. Ownership persists even while orphaned — there is
// no automatic release (§10), so an owned monitor stays read-only until an explicit future
// release operation.
var ErrManagedByFile = errors.New("store: monitor is managed by a file provider")

// FileManagement is a monitor's file-provider provenance, surfaced read-only to the API/UI.
// SourcePath is the tenant-safe RELATIVE path (never an absolute filesystem path).
type FileManagement struct {
	Provider   string
	UID        string
	SourcePath string
	Orphaned   bool
}

// MonitorProvenance returns a monitor's file-provider provenance. ok=false means the monitor
// is UI/API-managed (no provenance row) or does not exist. An orphaned row is still returned
// (Orphaned=true) — the resource remains owned.
func (s *Store) MonitorProvenance(ctx context.Context, monitorID string) (FileManagement, bool, error) {
	var (
		fm     FileManagement
		orphAt *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT provider_id, source_uid, source_path, orphaned_at::text
		   FROM managed_monitors WHERE monitor_id = $1`, monitorID).
		Scan(&fm.Provider, &fm.UID, &fm.SourcePath, &orphAt)
	if noRows(err) {
		return FileManagement{}, false, nil
	}
	if err != nil {
		return FileManagement{}, false, fmt.Errorf("store: monitor provenance: %w", err)
	}
	fm.Orphaned = orphAt != nil
	return fm, true, nil
}

// MonitorProvenanceBatch returns provenance for many monitors in one query, keyed by monitor
// id (absent = UI/API-managed). Used by list endpoints to attach management metadata without
// an N+1 (spec §15).
func (s *Store) MonitorProvenanceBatch(ctx context.Context, monitorIDs []string) (map[string]FileManagement, error) {
	out := make(map[string]FileManagement, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT monitor_id, provider_id, source_uid, source_path, orphaned_at::text
		   FROM managed_monitors WHERE monitor_id = ANY($1)`, monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("store: monitor provenance batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var fm FileManagement
		var orphAt *string
		if err := rows.Scan(&id, &fm.Provider, &fm.UID, &fm.SourcePath, &orphAt); err != nil {
			return nil, fmt.Errorf("store: scan provenance: %w", err)
		}
		fm.Orphaned = orphAt != nil
		out[id] = fm
	}
	return out, rows.Err()
}

// assertNotFileManagedTx rejects a user-managed write to a file-owned monitor. The caller
// holds the monitors row lock, so a concurrent file apply claiming ownership serializes
// behind this check — closing the API/reconcile race atomically, not just in an HTTP
// handler. Ownership blocks regardless of orphan state (no automatic release, §10).
func assertNotFileManagedTx(ctx context.Context, tx pgx.Tx, monitorID string) error {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM managed_monitors WHERE monitor_id = $1`, monitorID).Scan(&one)
	if noRows(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: ownership check: %w", err)
	}
	return ErrManagedByFile
}
