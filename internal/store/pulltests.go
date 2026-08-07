package store

import (
	"context"
	"fmt"
)

// EnqueuePullTest stores a one-off "Test connection" probe for a pull-served region and
// returns its id. TTL bounds how long the API will wait for the agent's result.
func (s *Store) EnqueuePullTest(ctx context.Context, region string, payload []byte, ttlSeconds int) (string, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 20
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pull_tests (region, payload, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3)) RETURNING id`,
		region, payload, ttlSeconds).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: enqueue pull test: %w", err)
	}
	// Wake a long-polling agent's test loop for this region (best-effort).
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('pull_tests', $1)`, region)
	return id, nil
}

// ClaimPullTest atomically claims one unclaimed, unexpired test for a region (FOR UPDATE
// SKIP LOCKED so concurrent agents don't double-run it) and returns its id and payload.
// ok is false when there is nothing to claim.
func (s *Store) ClaimPullTest(ctx context.Context, region string) (id string, payload []byte, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE pull_tests SET claimed_at = now()
		  WHERE id = (
		     SELECT id FROM pull_tests
		      WHERE region = $1 AND claimed_at IS NULL AND result IS NULL AND expires_at > now()
		      ORDER BY created_at
		      LIMIT 1
		      FOR UPDATE SKIP LOCKED
		  )
		  RETURNING id, payload`, region).Scan(&id, &payload)
	if noRows(err) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("store: claim pull test: %w", err)
	}
	return id, payload, true, nil
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
