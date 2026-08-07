package store

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestEnqueueDueSLAReportsNoSelfDeadlock is the regression guard for the SLA-report
// self-deadlock: EnqueueDueSLAReports opens a transaction (holding one connection)
// and then computes each project's SLIs. Computing them on the pool (a second
// acquire) deadlocks the moment the pool has no spare connection — the tx holds the
// only one, and the SLI acquire waits forever. Driven against a MAX-1 pool, the old
// code hangs; the fix (SLIs run on the tx) completes.
func TestEnqueueDueSLAReportsNoSelfDeadlock(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Mark the project due for a weekly SLA report (no prior send).
	if _, err := st.pool.Exec(ctx,
		`UPDATE projects SET sla_report_weekly = true, sla_report_last_at = NULL WHERE id = $1`, proj.ID); err != nil {
		t.Fatalf("mark due: %v", err)
	}

	// A pool with exactly ONE connection: the tx pins it, so any nested pool.Acquire
	// (the pre-fix SLI path) can never get a second one → deadlock.
	cfg, err := pgxpool.ParseConfig(os.Getenv("CERBIX_TEST_DATABASE_DSN"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 1
	p1, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("one-conn pool: %v", err)
	}
	defer p1.Close()
	st1 := &Store{pool: p1}

	done := make(chan error, 1)
	go func() {
		_, err := st1.EnqueueDueSLAReports(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueue due sla reports: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EnqueueDueSLAReports deadlocked on a single-connection pool (nested acquire inside tx)")
	}

	if got := st.countOutbox(ctx, t, domain.TopicSLAReport, "pending"); got != 1 {
		t.Fatalf("sla report events pending = %d, want 1", got)
	}
}
