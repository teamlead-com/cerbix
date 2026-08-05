package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"git.example.com/monitoring/cerbix/internal/domain"
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
		return fmt.Errorf("store: insert heartbeat: %w", err)
	}
	return nil
}

// InsertHeartbeatsBulk appends heartbeats WITHOUT touching monitor status or the alert
// pipeline — for a pull agent's historical backfill after a network outage. It fills the
// SLA/SLI gap only; live status is driven by fresh probes after reconnect. Idempotent on
// (monitor_id, ts). Returns the number of rows actually inserted (conflicts skipped).
func (s *Store) InsertHeartbeatsBulk(ctx context.Context, hbs []domain.Heartbeat) (int, error) {
	if len(hbs) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, hb := range hbs {
		batch.Queue(
			`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg) VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (monitor_id, ts) DO NOTHING`,
			hb.MonitorID, hbTime(hb), hb.Up, hb.LatencyMS, hb.Code, hb.Msg)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	inserted := 0
	for range hbs {
		ct, err := br.Exec()
		if err != nil {
			return inserted, fmt.Errorf("store: bulk insert heartbeat: %w", err)
		}
		inserted += int(ct.RowsAffected())
	}
	return inserted, nil
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
