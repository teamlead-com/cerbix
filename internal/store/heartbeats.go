package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// hbTime returns the heartbeat's probe timestamp, falling back to now() when unset, so
// the stored ts is the probe time (not the insert time) — this keeps SLA timing right
// and lets live inserts and historical backfills of the same probe dedupe on (monitor,ts).
func hbTime(hb domain.Heartbeat) time.Time {
	if hb.Ts.IsZero() {
		return time.Now()
	}
	return hb.Ts
}

// InsertHeartbeat records a check result. Idempotent on (monitor_id, ts): a duplicate
// (e.g. a retried post) is silently skipped rather than double-counted.
func (s *Store) InsertHeartbeat(ctx context.Context, hb domain.Heartbeat) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (monitor_id, ts) DO NOTHING`,
		hb.MonitorID, hbTime(hb), hb.Up, hb.LatencyMS, hb.Code, hb.Msg)
	if err != nil {
		// A result landing after its monitor was deleted is an expected race
		// (the scheduler snapshot keeps probing until the next refresh) — let
		// the caller drop it quietly instead of treating it as a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "monitor_id_fkey") {
			return ErrNotFound
		}
		return fmt.Errorf("store: insert heartbeat: %w", err)
	}
	return nil
}

// RecordHistoricalResults appends a pull agent's buffered historical results (replayed
// after a network outage) WITHOUT touching monitor status or the alert pipeline — the
// fourth, SLA/SLI-only ingest path (spec func-result-protocol §4). Live status is driven
// by fresh probes after reconnect. Each row is timestamp-bounds-validated (missing/future-
// beyond-skew/outside-retention are skipped, never a synthetic "now"), inserted with
// observed_at = ts, and idempotent on (monitor_id, ts). Returns rows inserted and rows
// skipped (deleted monitor or out-of-bounds timestamp).
func (s *Store) RecordHistoricalResults(ctx context.Context, hbs []domain.Heartbeat) (inserted, skipped int, err error) {
	if len(hbs) == 0 {
		return 0, 0, nil
	}
	// A backfilled result is a fact true at its historical moment; it NEVER touches live
	// state (status/counters/incidents/watermark) — this is SLA/audit only — and revision
	// gating (which protects live state) does not apply. But it MUST honor the same
	// timestamp bounds as scheduled ingest, or a future/1970 row lands in the raw table and
	// an SLA `ts >= since` query counts it (spec func-result-protocol §4).
	var dbNow time.Time
	if err := s.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&dbNow); err != nil {
		return 0, 0, fmt.Errorf("store: backfill clock: %w", err)
	}
	skew := s.resultSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}

	// Drop rows for monitors deleted since the agent buffered them: a pgx batch is one
	// implicit transaction, so a single FK violation (23503) aborts every insert and wedges
	// the whole backfill on retry. Filtering by the live monitor set self-heals.
	ids := make([]string, 0, len(hbs))
	seen := map[string]bool{}
	for _, hb := range hbs {
		if !seen[hb.MonitorID] {
			seen[hb.MonitorID] = true
			ids = append(ids, hb.MonitorID)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, 0, fmt.Errorf("store: backfill filter: %w", err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, fmt.Errorf("store: backfill filter scan: %w", err)
		}
		live[id] = true
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: backfill filter iterate: %w", err)
	}

	batch := &pgx.Batch{}
	queued := 0
	for _, hb := range hbs {
		if !live[hb.MonitorID] {
			skipped++ // monitor gone
			continue
		}
		ts := hb.Ts
		// Per-row timestamp bounds — identical to scheduled; each invalid row is skipped
		// (never a synthetic "now").
		if ts.IsZero() || ts.After(dbNow.Add(skew)) || (s.resultRetention > 0 && ts.Before(dbNow.Add(-s.resultRetention))) {
			skipped++
			continue
		}
		queued++
		batch.Queue(
			`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$2) ON CONFLICT (monitor_id, ts) DO NOTHING`,
			hb.MonitorID, ts, hb.Up, hb.LatencyMS, hb.Code, hb.Msg)
	}
	if queued == 0 {
		return 0, skipped, nil
	}
	br := s.pool.SendBatch(ctx, batch)
	for i := 0; i < queued; i++ {
		ct, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return inserted, skipped, fmt.Errorf("store: backfill insert: %w", err)
		}
		inserted += int(ct.RowsAffected())
	}
	if err := br.Close(); err != nil {
		return inserted, skipped, fmt.Errorf("store: close backfill batch: %w", err)
	}
	return inserted, skipped, nil
}

// ListRecentHeartbeats returns the most recent heartbeats for a monitor, newest first.
func (s *Store) ListRecentHeartbeats(ctx context.Context, monitorID string, limit int) ([]domain.Heartbeat, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats
		 WHERE monitor_id = $1 ORDER BY ts DESC LIMIT $2`,
		monitorID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list heartbeats: %w", err)
	}
	defer rows.Close()
	var out []domain.Heartbeat
	for rows.Next() {
		var hb domain.Heartbeat
		if err := rows.Scan(&hb.MonitorID, &hb.Ts, &hb.Up, &hb.LatencyMS, &hb.Code, &hb.Msg); err != nil {
			return nil, fmt.Errorf("store: scan heartbeat: %w", err)
		}
		out = append(out, hb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate heartbeats: %w", err)
	}
	return out, nil
}

// LatestHeartbeat returns the most recent heartbeat for a monitor, or ErrNotFound.
// Test support (assertion helper); no production caller.
func (s *Store) LatestHeartbeat(ctx context.Context, monitorID string) (domain.Heartbeat, error) {
	var hb domain.Heartbeat
	err := s.pool.QueryRow(ctx,
		`SELECT monitor_id, ts, up, latency_ms, code, msg FROM heartbeats
		 WHERE monitor_id = $1 ORDER BY ts DESC LIMIT 1`, monitorID).
		Scan(&hb.MonitorID, &hb.Ts, &hb.Up, &hb.LatencyMS, &hb.Code, &hb.Msg)
	if err == pgx.ErrNoRows {
		return domain.Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("store: latest heartbeat: %w", err)
	}
	return hb, nil
}
