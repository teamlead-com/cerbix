package store

import (
	"context"
	"testing"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

func (s *Store) partitionExists(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits i
		   JOIN pg_class c ON c.oid = i.inhrelid
		   JOIN pg_class p ON p.oid = i.inhparent
		  WHERE p.relname = 'heartbeats' AND c.relname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("partition lookup: %v", err)
	}
	return n == 1
}

func partitionName(day time.Time) string {
	return heartbeatPartitionPrefix + day.UTC().Format("20060102")
}

// TestEnsurePartitionsRoutesInserts proves EnsureHeartbeatPartitions creates the
// current day's partition and that a fresh heartbeat lands in it (not the default).
func TestEnsurePartitionsRoutesInserts(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if st.timescale {
		t.Skip("hypertable mode: declarative partitions are not managed (see TestHypertableRetention)")
	}
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})

	if err := st.EnsureHeartbeatPartitions(ctx, 2); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if !st.partitionExists(ctx, t, partitionName(today)) {
		t.Fatalf("today's partition %s was not created", partitionName(today))
	}

	if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Up: true, Code: 200}); err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}
	var inToday, inDefault int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM `+partitionName(today)).Scan(&inToday)
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats_default`).Scan(&inDefault)
	if inToday != 1 || inDefault != 0 {
		t.Fatalf("heartbeat routing: today=%d default=%d, want 1/0", inToday, inDefault)
	}
}

// TestPurgeDropsOldKeepsRecent proves retention drops partitions older than the
// cutoff while keeping within-retention data.
func TestPurgeDropsOldKeepsRecent(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if st.timescale {
		t.Skip("hypertable mode: retention runs via drop_chunks (see TestHypertableRetention)")
	}
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := today.AddDate(0, 0, -40)
	recent := today.AddDate(0, 0, -5)

	// Dated partitions for an old and a recent day, each with one row.
	for _, day := range []time.Time{old, recent} {
		if _, err := st.pool.Exec(ctx, `CREATE TABLE `+partitionName(day)+
			` PARTITION OF heartbeats FOR VALUES FROM ('`+day.Format(pgTimestamp)+`') TO ('`+day.AddDate(0, 0, 1).Format(pgTimestamp)+`')`); err != nil {
			t.Fatalf("create partition for %s: %v", day, err)
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO heartbeats (monitor_id, ts, up) VALUES ($1, $2, true)`, mon.ID, day.Add(time.Hour)); err != nil {
			t.Fatalf("seed row for %s: %v", day, err)
		}
	}

	// Retain 30 days: the 40-day-old partition drops, the 5-day-old one stays.
	cutoff := today.AddDate(0, 0, -30)
	dropped, err := st.PurgeOldHeartbeats(ctx, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("dropped = %d, want >= 1", dropped)
	}
	if st.partitionExists(ctx, t, partitionName(old)) {
		t.Fatalf("old partition %s should have been dropped", partitionName(old))
	}
	if !st.partitionExists(ctx, t, partitionName(recent)) {
		t.Fatalf("recent partition %s should have been kept", partitionName(recent))
	}
	var remaining int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&remaining)
	if remaining != 1 {
		t.Fatalf("remaining rows = %d, want 1 (only the recent day)", remaining)
	}
}

// TestHypertableRetention is the hypertable-mode twin of the two declarative
// tests above: chunks appear on demand (Ensure is a no-op), old rows insert
// without any partition pre-created (backfill), idempotency holds on old
// timestamps, and PurgeOldHeartbeats drops whole day-chunks before the cutoff
// while keeping recent data.
func TestHypertableRetention(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if !st.timescale {
		t.Skip("requires the timescaledb extension (declarative mode is covered above)")
	}
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})

	if err := st.EnsureHeartbeatPartitions(ctx, 2); err != nil {
		t.Fatalf("ensure must be a no-op on a hypertable: %v", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := today.AddDate(0, 0, -40).Add(time.Hour)
	recent := today.AddDate(0, 0, -5).Add(time.Hour)
	// No partition pre-creation: chunks materialize on insert, including for a
	// 40-day-old backfill timestamp.
	for _, ts := range []time.Time{old, recent} {
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: ts, Up: true, Code: 200}); err != nil {
			t.Fatalf("insert at %s: %v", ts, err)
		}
	}
	// Idempotency on an already-persisted old timestamp (edge-buffer retry).
	if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: old, Up: false, Code: 500}); err != nil {
		t.Fatalf("duplicate insert must be a no-op: %v", err)
	}
	var total int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&total)
	if total != 2 {
		t.Fatalf("rows after duplicate = %d, want 2", total)
	}

	// Retain 30 days: the 40-day-old chunk drops, the 5-day-old row survives.
	dropped, err := st.PurgeOldHeartbeats(ctx, today.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("dropped chunks = %d, want >= 1", dropped)
	}
	var remaining int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&remaining)
	if remaining != 1 {
		t.Fatalf("remaining rows = %d, want 1 (only the recent day)", remaining)
	}
}

// TestHypertableCompressionRoundTrip compresses an old chunk by hand and proves
// reads and conflict-inserts still work through compression — the path a
// late edge-buffer backfill takes after the compression policy has run.
func TestHypertableCompressionRoundTrip(t *testing.T) {
	st, ctx := outboxTestStore(t)
	if !st.timescale {
		t.Skip("requires the timescaledb extension")
	}
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, _ := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "api", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5,
	})

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -10)
	for i := 0; i < 3; i++ {
		ts := day.Add(time.Duration(i) * time.Minute)
		if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: ts, Up: true, LatencyMS: int64(10 + i), Code: 200}); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	// Compress every chunk of that day (what add_compression_policy does later).
	if _, err := st.pool.Exec(ctx,
		`SELECT compress_chunk(c, if_not_compressed => true) FROM show_chunks('heartbeats') c`); err != nil {
		t.Fatalf("compress chunks: %v", err)
	}

	// Reads go through compression transparently.
	hbs, err := st.ListRecentHeartbeats(ctx, mon.ID, 10)
	if err != nil || len(hbs) != 3 {
		t.Fatalf("list over compressed chunk: n=%d err=%v", len(hbs), err)
	}
	// A duplicate into the compressed chunk stays a no-op (unique survives),
	// and a new row into the compressed chunk inserts fine.
	if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: day, Up: false, Code: 500}); err != nil {
		t.Fatalf("duplicate into compressed chunk: %v", err)
	}
	if err := st.InsertHeartbeat(ctx, domain.Heartbeat{MonitorID: mon.ID, Ts: day.Add(30 * time.Minute), Up: true, Code: 200}); err != nil {
		t.Fatalf("backfill into compressed chunk: %v", err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM heartbeats WHERE monitor_id = $1`, mon.ID).Scan(&n)
	if n != 4 {
		t.Fatalf("rows after compressed-chunk writes = %d, want 4", n)
	}
}
