package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
)

// EnqueuePullJob stores a check-job payload for a pull-served region with a TTL
// (ttlSeconds), so an agent can claim it over HTTP. A non-positive TTL falls back to
// 60s. The payload is an opaque JSON snapshot (a dispatch.CheckJob).
func (s *Store) EnqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int, workflowKind string) error {
	return s.enqueuePullJob(ctx, region, payload, ttlSeconds, leaseSeconds, 1, workflowKind)
}

func (s *Store) EnqueuePullJobV2(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int, workflowKind string) error {
	return s.enqueuePullJob(ctx, region, payload, ttlSeconds, leaseSeconds, 2, workflowKind)
}

// EnqueuePullJobV3 enqueues on the carrier generation that carries envelope v2. It is used
// only for a region whose executors have declared capability 2 (§4.7, D-0160).
func (s *Store) EnqueuePullJobV3(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds int, workflowKind string) error {
	return s.enqueuePullJob(ctx, region, payload, ttlSeconds, leaseSeconds, 3, workflowKind)
}

// leaseSeconds is the per-JOB claim lease (FR-029 §4.2). Zero means "the endpoint's default", which
// is what every existing caller and every short probe passes — a monitor whose probe outlives the
// default is the only one that needs its own, and a job re-claimed while it still runs is a duplicate
// probe for an ordinary type and a duplicate external TRANSACTION for a canary.
func (s *Store) enqueuePullJob(ctx context.Context, region string, payload []byte, ttlSeconds, leaseSeconds, protocolVersion int, workflowKind string) error {
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	var lease *int
	if leaseSeconds > 0 {
		lease = &leaseSeconds
	}
	// NULL, not "", for a job any agent may run: the claim filter is `IS NULL OR = ANY(...)`, and an
	// empty string would be a capability nobody ever announces — every ordinary job unclaimable.
	var kind *string
	if workflowKind != "" {
		kind = &workflowKind
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO pull_jobs (region, payload, expires_at, protocol_version, lease_seconds, workflow_kind)
		 VALUES ($1, $2, now() + make_interval(secs => $3), $4, $5, $6)`,
		region, payload, ttlSeconds, protocolVersion, lease, kind)
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
func (s *Store) ClaimPullJobs(ctx context.Context, region string, max, leaseSeconds int, workflowKinds []string) ([]PullJob, error) {
	return s.claimPullJobs(ctx, region, max, leaseSeconds, 1, workflowKinds)
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
func (s *Store) ClaimPullJobsV2(ctx context.Context, region string, max, leaseSeconds int, workflowKinds []string) ([]PullJob, error) {
	return s.claimPullJobs(ctx, region, max, leaseSeconds, 2, workflowKinds)
}

// ClaimPullJobsV3 serves the capability-2 endpoint: every generation up to 3.
func (s *Store) ClaimPullJobsV3(ctx context.Context, region string, max, leaseSeconds int, workflowKinds []string) ([]PullJob, error) {
	return s.claimPullJobs(ctx, region, max, leaseSeconds, 3, workflowKinds)
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
func (s *Store) claimPullJobs(ctx context.Context, region string, max, leaseSeconds, maxProtocolVersion int, workflowKinds []string) ([]PullJob, error) {
	if max <= 0 {
		max = 16
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 30
	}
	rows, err := s.pool.Query(ctx,
		`UPDATE pull_jobs SET claim_token = gen_random_uuid(),
		        -- The job's OWN lease when it asked for one, the caller's default otherwise.
		        lease_expires_at = now() + make_interval(secs => COALESCE(lease_seconds, $3))
		  WHERE id IN (
		     SELECT id FROM pull_jobs
		      WHERE region = $1 AND protocol_version <= $4 AND expires_at > now()
		        AND (lease_expires_at IS NULL OR lease_expires_at <= now())
		        -- FR-029 invariant 6: a job that names a capability goes only to a claim that
		        -- DECLARED it. NULL is the ordinary job every agent may run, so nothing about the
		        -- existing path depends on what the claimant announced.
		        AND (workflow_kind IS NULL OR workflow_kind = ANY($5))
		      ORDER BY created_at
		      LIMIT $2
		      FOR UPDATE SKIP LOCKED
		  )
		  RETURNING claim_token::text, payload, protocol_version`,
		region, max, leaseSeconds, maxProtocolVersion, declaredKinds(workflowKinds))
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

// declaredKinds normalizes what a claim announced into a form `= ANY($n)` can take.
//
// It is not load-bearing for the current clause, and a mutation test proved that: pgx sends a nil
// slice as NULL, `workflow_kind = ANY(NULL)` is NULL, and a WHERE treats NULL as not-true — the same
// answer the empty array gives. It stays because that agreement rests on three-valued logic holding
// under a clause nobody has negated yet, and "declared nothing" is better said in the value than
// discovered in the semantics.
func declaredKinds(kinds []string) []string {
	if kinds == nil {
		return []string{}
	}
	return kinds
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
	return s.RecordAgentCapabilities(ctx, region, agentID, 0, false, nil)
}

// RecordAgentCapabilities upserts what an agent says it can do. `workflowKinds` is the FR-029
// announcement: the `<kind>@<version>` tokens this agent's binary executes, alongside the envelope
// generation it can open. Both live in one JSONB document because they answer one question — what
// may core send here — and splitting them would make a reader check two places to answer it.
func (s *Store) RecordAgentCapabilities(ctx context.Context, region, agentID string,
	credentialEnvelope int, credentialReady bool, workflowKinds []string) error {

	if workflowKinds == nil {
		workflowKinds = []string{}
	}
	capabilities, err := json.Marshal(map[string]any{
		"credential_envelope":      credentialEnvelope,
		domain.CanaryCapabilityKey: workflowKinds,
	})
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

// LiveCredentialReadyAgentRegions is existential and never vacuous: a region appears only
// when at least one recent agent reasserted key readiness AND a credential-envelope
// capability of at least minCapability. The floor is a parameter because capability is
// GENERATIONAL: an executor that can only open envelope v1 is not evidence of readiness
// for a region core is about to emit envelope v2 into (§4.7, D-0160).
func (s *Store) LiveCredentialReadyAgentRegions(ctx context.Context, within time.Duration, minCapability int) (map[string]bool, error) {
	secs := int(within.Seconds())
	if secs <= 0 {
		secs = 60
	}
	if minCapability < 1 {
		minCapability = 1
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT region FROM agent_heartbeats
		  WHERE seen_at > now() - make_interval(secs => $1)
		    AND credential_ready
		    AND COALESCE((capabilities->>'credential_envelope')::int, 0) >= $2`, secs, minCapability)
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

// LiveCanaryAgentCapabilities is the FR-029 half of the same existential question
// `LiveCredentialReadyAgentRegions` answers for envelopes: what did the live agents in each region
// ANNOUNCE they can run?
//
// It returns the announced tokens instead of a yes/no because a boolean cannot tell the two
// dispatch failures apart. A region with no canary-capable agent and a region whose agents announce
// only a version core is not emitting are the same "false", but the operator's fix differs — start a
// runner, or finish an upgrade — so the scheduler is given the set and names the reason from it.
// Union across agents: a region is as capable as its most capable live agent, and the in-flight cap
// is what stops one lucky agent from being handed everything.
func (s *Store) LiveCanaryAgentCapabilities(ctx context.Context, within time.Duration) (map[string][]string, error) {
	secs := int(within.Seconds())
	if secs <= 0 {
		secs = 60
	}
	rows, err := s.pool.Query(ctx,
		`SELECT region, capabilities->'`+domain.CanaryCapabilityKey+`'
		   FROM agent_heartbeats
		  WHERE seen_at > now() - make_interval(secs => $1)
		    AND jsonb_typeof(capabilities->'`+domain.CanaryCapabilityKey+`') = 'array'`, secs)
	if err != nil {
		return nil, fmt.Errorf("store: live canary agent capabilities: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	for rows.Next() {
		var region string
		var raw []byte
		if err := rows.Scan(&region, &raw); err != nil {
			return nil, fmt.Errorf("store: scan canary agent capabilities: %w", err)
		}
		var tokens []string
		if err := json.Unmarshal(raw, &tokens); err != nil {
			// A malformed announcement is not a capability. Skipped rather than failing the whole
			// lookup, which would make every region look incapable because one agent wrote junk.
			continue
		}
		if seen[region] == nil {
			seen[region] = map[string]bool{}
		}
		for _, t := range tokens {
			if t == "" || seen[region][t] {
				continue
			}
			seen[region][t] = true
			out[region] = append(out[region], t)
		}
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
