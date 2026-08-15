package store

import (
	"context"
	"fmt"
)

// EnqueuePullTest stores a one-off "Test connection" probe for a pull-served region and
// returns its id. TTL bounds how long the API will wait for the agent's result.
func (s *Store) EnqueuePullTest(ctx context.Context, region string, payload []byte, ttlSeconds int) (string, error) {
	return s.enqueuePullTest(ctx, region, payload, ttlSeconds, 1)
}

func (s *Store) EnqueuePullTestV2(ctx context.Context, region string, payload []byte, ttlSeconds int) (string, error) {
	return s.enqueuePullTest(ctx, region, payload, ttlSeconds, 2)
}

func (s *Store) enqueuePullTest(ctx context.Context, region string, payload []byte, ttlSeconds, protocolVersion int) (string, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 20
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at, protocol_version)
		 VALUES ($1, $2, now() + make_interval(secs => $3), $4) RETURNING id`,
		region, payload, ttlSeconds, protocolVersion).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: enqueue pull test: %w", err)
	}
	// Wake a long-polling agent's test loop for this region (best-effort).
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('pull_tests', $1)`, region)
	return id, nil
}

// ClaimPullTest serves the generation-1 endpoint: generation-1 rows only.
func (s *Store) ClaimPullTest(ctx context.Context, region string) (id string, payload []byte, protocolVersion int, ok bool, err error) {
	return s.claimPullTest(ctx, region, 1)
}

// ClaimPullTestV2 serves the capability-2 endpoint and claims EVERY generation at or below
// 2 in one operation — the test-RPC mirror of ClaimPullJobsV2. A jobs-only fix would leave
// test-connection broken for ordinary monitors in exactly the same way (D-0160).
func (s *Store) ClaimPullTestV2(ctx context.Context, region string) (id string, payload []byte, protocolVersion int, ok bool, err error) {
	return s.claimPullTest(ctx, region, 2)
}

// claimPullTest claims one unclaimed, unexpired test of any generation up to
// maxProtocolVersion inclusive (FOR UPDATE SKIP LOCKED so concurrent agents don't
// double-run it), oldest first so generations do not starve each other. The returned
// protocolVersion is the row's carrier generation, stamped by the server — never inferred
// from the payload. ok is false when there is nothing to claim.
func (s *Store) claimPullTest(ctx context.Context, region string, maxProtocolVersion int) (id string, payload []byte, protocolVersion int, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE pull_tests SET claimed_at = now()
		  WHERE id = (
		     SELECT id FROM pull_tests
		      WHERE region = $1 AND protocol_version <= $2 AND claimed_at IS NULL AND result IS NULL AND expires_at > now()
		      ORDER BY created_at
		      LIMIT 1
		      FOR UPDATE SKIP LOCKED
		  )
		  RETURNING id, payload, protocol_version`, region, maxProtocolVersion).Scan(&id, &payload, &protocolVersion)
	if noRows(err) {
		return "", nil, 0, false, nil
	}
	if err != nil {
		return "", nil, 0, false, fmt.Errorf("store: claim pull test: %w", err)
	}
	return id, payload, protocolVersion, true, nil
}

// SavePullTestResult records the heartbeat an agent produced for a test.
func (s *Store) SavePullTestResult(ctx context.Context, id, region string, result []byte) error {
	// The region predicate scopes the write to the agent's own region: an agent
	// authenticated for region A cannot overwrite a test enqueued for region B even
	// if it learns B's test id. A mismatch updates zero rows (silently ignored — the
	// waiting /monitors/test poll simply times out, as it would for an unknown id).
	_, err := s.pool.Exec(ctx, `UPDATE pull_tests SET result = $3 WHERE id = $1 AND region = $2`, id, region, result)
	if err != nil {
		return fmt.Errorf("store: save pull test result: %w", err)
	}
	return nil
}

// GetPullTestResult atomically fetches and removes a test's result once the agent has
// posted it. ok is false while the result is not ready yet (the API keeps polling).
func (s *Store) GetPullTestResult(ctx context.Context, id string) (result []byte, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`DELETE FROM pull_tests WHERE id = $1 AND result IS NOT NULL RETURNING result`, id).Scan(&result)
	if noRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get pull test result: %w", err)
	}
	return result, true, nil
}

// PurgeExpiredPullTests removes tests past their TTL (housekeeping); returns the count.
func (s *Store) PurgeExpiredPullTests(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pull_tests WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: purge pull tests: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
