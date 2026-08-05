package store

import (
	"context"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/metrics"
)

// EnqueuePullJob stores a check-job payload for a pull-served region with a TTL
// (ttlSeconds), so an agent can claim it over HTTP. A non-positive TTL falls back to
// 60s. The payload is an opaque JSON snapshot (a dispatch.CheckJob).
func (s *Store) EnqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds int) error {
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO pull_jobs (region, payload, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
		region, payload, ttlSeconds)
	if err != nil {
		return fmt.Errorf("store: enqueue pull job: %w", err)
	}
	// Wake any long-polling agent for this region (best-effort; a missed notification is
	// covered by the long-poll's max-hold re-poll).
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('pull_jobs', $1)`, region)
	return nil
}

// PullChannel is the Postgres LISTEN/NOTIFY channel that carries a region name whenever
// a pull job is enqueued for it.
const PullChannel = "pull_jobs"

// ClaimPullJobs atomically claims up to max unexpired jobs for a region and returns
// their payloads, removing them so each job is delivered to exactly one agent
// (FOR UPDATE SKIP LOCKED makes concurrent agents safe). Expired jobs are left for the
// purge; they are simply not returned.
func (s *Store) ClaimPullJobs(ctx context.Context, region string, max int) ([][]byte, error) {
	if max <= 0 {
		max = 16
	}
	rows, err := s.pool.Query(ctx,
		`DELETE FROM pull_jobs
		  WHERE id IN (
		     SELECT id FROM pull_jobs
		      WHERE region = $1 AND expires_at > now()
		      ORDER BY created_at
		      LIMIT $2
		      FOR UPDATE SKIP LOCKED
		  )
		  RETURNING payload`,
		region, max)
	if err != nil {
		return nil, fmt.Errorf("store: claim pull jobs: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: scan pull job: %w", err)
		}
		out = append(out, payload)
	}
	return out, rows.Err()
}

// PurgeExpiredPullJobs deletes jobs past their TTL (housekeeping); returns the count.
func (s *Store) PurgeExpiredPullJobs(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pull_jobs WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: purge pull jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PullQueueStats returns per-region pull-queue depth and lag (oldest unclaimed job
// age), for observability. Only unexpired jobs count. Leader samples this on a tick.
func (s *Store) PullQueueStats(ctx context.Context) ([]metrics.PullStat, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT region, count(*)::int,
		        COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8
		   FROM pull_jobs
		  WHERE expires_at > now()
		  GROUP BY region`)
	if err != nil {
		return nil, fmt.Errorf("store: pull queue stats: %w", err)
	}
	defer rows.Close()
	var out []metrics.PullStat
	for rows.Next() {
		var st metrics.PullStat
		if err := rows.Scan(&st.Region, &st.Pending, &st.LagSeconds); err != nil {
			return nil, fmt.Errorf("store: scan pull stat: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RecordAgentHeartbeat upserts a pull agent's last-seen time for its region.
func (s *Store) RecordAgentHeartbeat(ctx context.Context, region, agentID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agent_heartbeats (region, agent_id, seen_at) VALUES ($1, $2, now())
		 ON CONFLICT (region, agent_id) DO UPDATE SET seen_at = now()`,
		region, agentID)
	if err != nil {
		return fmt.Errorf("store: record agent heartbeat: %w", err)
	}
	return nil
}

// PurgeStaleAgentHeartbeats deletes heartbeat rows older than the given age. Each agent
// restart leaves a row under a fresh agent_id, so this housekeeping (leader tick) keeps
// the table from accumulating dead agents. It never touches live agents (age is far
// beyond the liveness window). Returns the number of rows removed.
func (s *Store) PurgeStaleAgentHeartbeats(ctx context.Context, olderThan time.Duration) (int, error) {
	secs := int(olderThan.Seconds())
	if secs <= 0 {
		secs = 3600
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM agent_heartbeats WHERE seen_at < now() - make_interval(secs => $1)`, secs)
	if err != nil {
		return 0, fmt.Errorf("store: purge agent heartbeats: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// LiveAgentRegions returns the set of regions with an agent heartbeat within the given
// window — a pull-transport analogue of a live RabbitMQ consumer.
func (s *Store) LiveAgentRegions(ctx context.Context, within time.Duration) (map[string]bool, error) {
	secs := int(within.Seconds())
	if secs <= 0 {
		secs = 60
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT region FROM agent_heartbeats WHERE seen_at > now() - make_interval(secs => $1)`, secs)
	if err != nil {
		return nil, fmt.Errorf("store: live agent regions: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			return nil, fmt.Errorf("store: scan agent region: %w", err)
		}
		out[region] = true
	}
	return out, rows.Err()
}
