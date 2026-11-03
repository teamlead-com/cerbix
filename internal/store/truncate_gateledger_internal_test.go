package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// FLAKE-0206 (iter-0207): TruncateAll leaves the gate-decision ledger able to take a decision
// TODAY. Only the gate-maintenance tests used to re-run migration 00093's bootstrap (today + 7
// days); the change_/gate_ tests run before them and wrote into whatever partitions the previous run
// left, so a test database idle for more than a week had no partition for today and 57 tests
// cascaded on the first full run. The calendar removed the partition, not a test.
func TestTruncateAllLeavesTodaysGateLedgerPartitionInPlace(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	// Whatever happens below, leave the ledger as the bootstrap makes it for the next test.
	t.Cleanup(func() { resetGateLedger(t, st, context.Background()) })

	today := gateToday(t, st, ctx)
	// The state an idle test database reaches: today's day neither attached nor registered.
	dropGateDay(t, st, ctx, today)
	if c := gateLookup(t, st, ctx, today); c.exists || c.state != "" {
		t.Fatalf("test setup: today's day is still there: %+v", c)
	}

	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	c := gateLookup(t, st, ctx, today)
	if !c.exists || !c.attached || c.state != "attached" {
		t.Fatalf("after TruncateAll today's ledger day = %+v, want attached and registered", c)
	}
	// And it is idempotent: a second TruncateAll with every day in place changes nothing.
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("second truncate: %v", err)
	}
	if again := gateLookup(t, st, ctx, today); again.relid != c.relid {
		t.Fatalf("a second TruncateAll replaced today's partition: %+v -> %+v", c, again)
	}
}
