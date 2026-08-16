package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

	accepted := make([]domain.Heartbeat, 0, len(hbs))
	for _, hb := range hbs {
		if !live[hb.MonitorID] {
			skipped++ // monitor gone
			continue
		}
		// Per-row timestamp bounds — identical to scheduled; each invalid row is skipped
		// (never a synthetic "now").
		if hb.Ts.IsZero() || hb.Ts.After(dbNow.Add(skew)) || (s.resultRetention > 0 && hb.Ts.Before(dbNow.Add(-s.resultRetention))) {
			skipped++
			continue
		}
		accepted = append(accepted, hb)
	}
	if len(accepted) == 0 {
		return 0, skipped, nil
	}

	// The RAW inserts take their (monitor_id, ts) keys in one global order too, not just the
	// service marks downstream. Two overlapping backfills inserting the same PK set in
	// different input orders lock the conflicting rows in opposite sequences and deadlock
	// before the service-key sorting is ever reached — the batch runs inside one
	// transaction, so every Queue below is a lock in input order.
	sort.Slice(accepted, func(i, j int) bool {
		if accepted[i].MonitorID != accepted[j].MonitorID {
			return accepted[i].MonitorID < accepted[j].MonitorID
		}
		return accepted[i].Ts.Before(accepted[j].Ts)
	})

	batch := &pgx.Batch{}
	queued := make([]domain.Heartbeat, 0, len(accepted))
	for _, hb := range accepted {
		queued = append(queued, domain.Heartbeat{MonitorID: hb.MonitorID, Ts: hb.Ts})
		batch.Queue(
			`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$2) ON CONFLICT (monitor_id, ts) DO NOTHING`,
			hb.MonitorID, hb.Ts, hb.Up, hb.LatencyMS, hb.Code, hb.Msg)
	}

	// The batch runs inside an EXPLICIT transaction so the service-ingest marks commit with
	// the rows that caused them. §10.4 covers agent historical backfill by name, and the
	// first implementation went straight to the pool with raw inserts — so a backfilled
	// result was invisible to the seal handshake entirely: no dirty mark, no late-data
	// repair, and a sealed window that silently disagreed with the raw table it was
	// computed from.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, skipped, fmt.Errorf("store: begin backfill: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	br := tx.SendBatch(ctx, batch)
	landed := make([]domain.Heartbeat, 0, len(queued))
	for i := range queued {
		ct, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return 0, skipped, fmt.Errorf("store: backfill insert: %w", err)
		}
		if ct.RowsAffected() > 0 {
			inserted++
			landed = append(landed, queued[i])
		}
	}
	if err := br.Close(); err != nil {
		return 0, skipped, fmt.Errorf("store: close backfill batch: %w", err)
	}

	// Gated on ACTUAL insertion, exactly like scheduled ingest: a row absorbed by
	// ON CONFLICT DO NOTHING changed nothing, and marking its bucket dirty would invite a
	// recompute that can only produce the number already there.
	//
	// Sorted so a batch takes the (service_id, bucket_start) keys in one direction; two
	// overlapping backfills taking them in opposite orders deadlock.
	if err := s.noteHeartbeatsForServices(ctx, tx, landed); err != nil {
		return 0, skipped, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, skipped, fmt.Errorf("store: commit backfill: %w", err)
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
