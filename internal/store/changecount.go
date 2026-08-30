package store

import (
	"context"
	"fmt"
)

// CountServiceChanges is the retention pass's sample for `cerbix_changes_retained` (FR-025 D9,
// D15): every row of service_changes still held — a plain count once a day on a table whose
// design capacity is ~10⁵–10⁶ rows (§5a). It is a read, not a fact; the gauge it feeds is the
// leader's and is cleared when leadership is lost.
func (s *Store) CountServiceChanges(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM service_changes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count service changes: %w", err)
	}
	return n, nil
}
