package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/metrics"
)

// EnqueuePullJob stores a check-job payload for a pull-served region with a TTL
// (ttlSeconds), so an agent can claim it over HTTP. A non-positive TTL falls back to
// 60s. The payload is an opaque JSON snapshot (a dispatch.CheckJob).
func (s *Store) EnqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds int) error {
	return s.enqueuePullJob(ctx, region, payload, ttlSeconds, 1)
}

func (s *Store) EnqueuePullJobV2(ctx context.Context, region string, payload []byte, ttlSeconds int) error {
	return s.enqueuePullJob(ctx, region, payload, ttlSeconds, 2)
}

func (s *Store) enqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds, protocolVersion int) error {
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO pull_jobs (region, payload, expires_at, protocol_version) VALUES ($1, $2, now() + make_interval(secs => $3), $4)`,
		region, payload, ttlSeconds, protocolVersion)
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

// PullJob is a leased check job: an opaque payload (a dispatch.CheckJob snapshot)
// plus the claim Token the agent echoes back to ack (delete) it once reported.
// ProtocolVersion is the row's carrier generation, stamped by the server: the agent must
// never infer it from the payload, which is the part an attacker can edit
// (func-secret-inventory §4.7, D-0160).
type PullJob struct {
	Token           string
	Payload         []byte
	ProtocolVersion int
}

// ClaimPullJobs serves the generation-1 endpoint: generation-1 rows only.
func (s *Store) ClaimPullJobs(ctx context.Context, region string, max, leaseSeconds int) ([]PullJob, error) {
	return s.claimPullJobs(ctx, region, max, leaseSeconds, 1)
}

// ClaimPullJobsV2 serves the capability-2 endpoint and leases EVERY generation at or below
// 2 in ONE atomic claim, under the caller's single max and one lease.
//
// The barrier is one-directional (func-secret-inventory §4.7, D-0160): it stops an
// incapable executor from receiving a newer generation, and must never stop a capable one
// from receiving an older. Non-credentialed monitors are enqueued as generation-1 rows and
// an `enforced` region's agent is necessarily capable, so a capable claim that returned
// only its own generation left every ordinary monitor's row to expire by TTL — no probe, no
// heartbeat, no DOWN, no alert. Two sequential single-generation claims are NOT an
// equivalent fix: the long poll sleeps out its window on whichever generation is empty, and
// two independent claims each honour `max` separately and over-lease.
func (s *Store) ClaimPullJobsV2(ctx context.Context, region string, max, leaseSeconds int) ([]PullJob, error) {
	return s.claimPullJobs(ctx, region, max, leaseSeconds, 2)
}

// claimPullJobs atomically LEASES up to max claimable jobs for a region, of any generation
// up to maxProtocolVersion inclusive: it stamps each with a fresh claim_token and a
// lease_expires_at (now + leaseSeconds) and returns them. A job is claimable when it is
// unexpired (expires_at > now) AND currently unleased or its lease has lapsed
// (lease_expires_at IS NULL OR <= now) — so a crashed agent's jobs become claimable again
// after the lease, rather than being lost as they were under the old DELETE-on-claim.
// FOR UPDATE SKIP LOCKED keeps concurrent agents safe. Jobs are removed only by AckPullJobs
// (on report) or the TTL purge. A non-positive leaseSeconds falls back to 30s.
//
// Ordering by created_at across the whole set is what keeps generations from starving each
// other: rows compete on age, not on which generation they belong to.
func (s *Store) claimPullJobs(ctx context.Context, region string, max, leaseSeconds, maxProtocolVersion int) ([]PullJob, error) {
	if max <= 0 {
		max = 16
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 30
	}
	rows, err := s.pool.Query(ctx,
		`UPDATE pull_jobs SET claim_token = gen_random_uuid(),
		        lease_expires_at = now() + make_interval(secs => $3)
		  WHERE id IN (
		     SELECT id FROM pull_jobs
		      WHERE region = $1 AND protocol_version <= $4 AND expires_at > now()
		        AND (lease_expires_at IS NULL OR lease_expires_at <= now())
		      ORDER BY created_at
		      LIMIT $2
		      FOR UPDATE SKIP LOCKED
		  )
		  RETURNING claim_token::text, payload, protocol_version`,
		region, max, leaseSeconds, maxProtocolVersion)
	if err != nil {
		return nil, fmt.Errorf("store: claim pull jobs: %w", err)
	}
	defer rows.Close()
	var out []PullJob
	for rows.Next() {
		var job PullJob
		if err := rows.Scan(&job.Token, &job.Payload, &job.ProtocolVersion); err != nil {
			return nil, fmt.Errorf("store: scan pull job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// AckPullJobs removes leased jobs by their claim tokens — the agent's confirmation
// that it has recorded the results. Acking by token (not id) is safe against a slow
// agent whose lease already lapsed and was re-claimed by another: the stale token no
// longer matches, so the late ack is a harmless no-op and the re-lease is preserved.
func (s *Store) AckPullJobs(ctx context.Context, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM pull_jobs WHERE claim_token = ANY($1)`, tokens); err != nil {
		return fmt.Errorf("store: ack pull jobs: %w", err)
	}
	return nil
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
	return s.RecordAgentCapabilities(ctx, region, agentID, 0, false)
}

func (s *Store) RecordAgentCapabilities(ctx context.Context, region, agentID string, credentialEnvelope int, credentialReady bool) error {
	capabilities, err := json.Marshal(map[string]int{"credential_envelope": credentialEnvelope})
	if err != nil {
		return fmt.Errorf("store: encode agent capabilities: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO agent_heartbeats (region, agent_id, seen_at, capabilities, credential_ready)
		 VALUES ($1, $2, now(), $3, $4)
		 ON CONFLICT (region, agent_id) DO UPDATE
		 SET seen_at = now(), capabilities = EXCLUDED.capabilities,
		     credential_ready = EXCLUDED.credential_ready`,
		region, agentID, capabilities, credentialReady)
	if err != nil {
		return fmt.Errorf("store: record agent heartbeat: %w", err)
	}
	return nil
}

// LiveCredentialReadyAgentRegions is existential and never vacuous: a region appears
// only when at least one recent agent reasserted v2 capability and key readiness.
func (s *Store) LiveCredentialReadyAgentRegions(ctx context.Context, within time.Duration) (map[string]bool, error) {
	secs := int(within.Seconds())
	if secs <= 0 {
		secs = 60
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT region FROM agent_heartbeats
		  WHERE seen_at > now() - make_interval(secs => $1)
		    AND credential_ready
		    AND COALESCE((capabilities->>'credential_envelope')::int, 0) >= 1`, secs)
	if err != nil {
		return nil, fmt.Errorf("store: live credential-ready agent regions: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			return nil, fmt.Errorf("store: scan credential-ready agent region: %w", err)
		}
		out[region] = true
	}
	return out, rows.Err()
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
