package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Agent B — adversarial pass over changeset 2 (func-reliability-gate §7 *One snapshot*,
// *Identity and reads*; invariants 7 and 12). These tests reach the mechanisms A's tests name
// but cannot observe: the ORDER of statements inside the decision transaction, a snapshot
// established under a REAL lock wait, and the id's millisecond as the database's instant.

// sqlTracer records every statement pgx issues, per backend PID, so a test can assert the
// exact statement order of one transaction.
type sqlTracer struct {
	mu    sync.Mutex
	byPID map[uint32][]string
}

func (tr *sqlTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	pid := conn.PgConn().PID()
	tr.byPID[pid] = append(tr.byPID[pid], data.SQL)
	return ctx
}

func (tr *sqlTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *sqlTracer) reset() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.byPID = map[uint32][]string{}
}

// transactions returns, for every connection that ran a BEGIN, the statements from that BEGIN
// through its COMMIT/ROLLBACK (inclusive).
func (tr *sqlTracer) transactions() [][]string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out [][]string
	for _, seq := range tr.byPID {
		for i := 0; i < len(seq); i++ {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(seq[i])), "begin") {
				continue
			}
			tx := []string{seq[i]}
			for j := i + 1; j < len(seq); j++ {
				tx = append(tx, seq[j])
				low := strings.ToLower(strings.TrimSpace(seq[j]))
				if low == "commit" || low == "rollback" {
					i = j
					break
				}
			}
			out = append(out, tx)
		}
	}
	return out
}

// gateTracedPool swaps the store's pool for one whose connections are traced. The original
// pool is restored (and the traced one closed) at cleanup, BEFORE gateStore's own Close runs.
func gateTracedPool(t *testing.T, st *Store, ctx context.Context) *sqlTracer {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(gateTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &sqlTracer{byPID: map[uint32][]string{}}
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := st.pool
	st.pool = traced
	t.Cleanup(func() {
		st.pool = old
		traced.Close()
	})
	return tr
}

func gateTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN")
	}
	return dsn
}

// D6a, structurally: inside the decision transaction the FIRST statement after BEGIN that is
// not one of the deadline wrapper's `SET LOCAL`s is `SELECT statement_timestamp()` — the
// linearization point — and the clock is read exactly once (the report path takes `as_of`,
// it does not re-read it). The mutation that reads the service row before the clock fails here.
func TestGateBDecisionFirstSnapshotBearingStatementIsTheClockRead(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	tr := gateTracedPool(t, st, ctx)
	tr.reset()
	dec := gateDecide(t, st, ctx, f)
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)

	txs := tr.transactions()
	if len(txs) != 1 {
		t.Fatalf("DecideGate ran %d transactions, want exactly one:\n%s", len(txs), renderTxs(txs))
	}
	seq := txs[0]
	if !strings.Contains(strings.ToLower(seq[0]), "repeatable read") {
		t.Errorf("BEGIN = %q, want REPEATABLE READ", seq[0])
	}
	first := -1
	for i := 1; i < len(seq); i++ {
		if strings.HasPrefix(strings.TrimSpace(seq[i]), "SET LOCAL ") {
			continue
		}
		first = i
		break
	}
	if first < 0 {
		t.Fatalf("no snapshot-bearing statement recorded:\n%s", strings.Join(seq, "\n"))
	}
	if got := strings.TrimSpace(seq[first]); got != "SELECT statement_timestamp()" {
		t.Errorf("first snapshot-bearing statement = %q, want the clock read (D6a); sequence:\n%s", got, strings.Join(seq, "\n"))
	}
	if first < 2 {
		t.Errorf("the deadline wrapper's SET LOCALs did not precede the clock read: %v", seq[:first+1])
	}
	clockReads := 0
	for _, s := range seq {
		if strings.TrimSpace(s) == "SELECT statement_timestamp()" {
			clockReads++
		}
	}
	if clockReads != 1 {
		t.Errorf("the decision read the clock %d times, want exactly once (evaluated_at is ONE instant)", clockReads)
	}
	if strings.ToLower(strings.TrimSpace(seq[len(seq)-1])) != "commit" {
		t.Errorf("the transaction ended with %q, want commit", seq[len(seq)-1])
	}
	// Owners are consumed inside THIS transaction: the service row, the policy, the override,
	// the buckets (report), the target, the latches, the incident, coverage, the revision and
	// the ledger INSERT all appear between BEGIN and COMMIT.
	for _, want := range []string{"FROM services", "FROM service_gate_policies", "FROM service_gate_overrides",
		"service_reliability_buckets", "FROM sla_targets", "service_burn_alert_state", "FROM incidents",
		"service_alert_state", "FROM service_definition_revisions", "INSERT INTO service_gate_decisions"} {
		found := false
		for _, s := range seq[1 : len(seq)-1] {
			if strings.Contains(s, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no statement touching %q inside the decision transaction", want)
		}
	}
}

func renderTxs(txs [][]string) string {
	var b strings.Builder
	for i, tx := range txs {
		b.WriteString("-- tx ")
		b.WriteString(string(rune('0' + i)))
		b.WriteString("\n")
		for _, s := range tx {
			b.WriteString("  ")
			b.WriteString(strings.Join(strings.Fields(s), " "))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// waitForLockWaiters polls pg_locks until `want` backends wait on the relation, or fails.
func waitForLockWaiters(t *testing.T, st *Store, ctx context.Context, relation string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE relation = $1::regclass AND NOT granted`, relation).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend waited on %s within %s", relation, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// One snapshot under a REAL blocker (D6a, invariant 7). A blocker connection holds `services`
// ACCESS EXCLUSIVE, so the decision's service-row read — the statement right AFTER the clock
// read — waits. While it waits, a third connection commits an edit that changes TWO things the
// decision reads (the threshold 90 → 50, which the fixture's BurnedPercent = 50 then matches,
// and unknown_behavior), bumps the revision, and flips the page latch to FIRING. The blocker
// releases. The decision must be the pre-edit world ENTIRELY — revision N, ALLOW, no matched
// clause — and evaluated_at must PRECEDE the edit: the snapshot and the clock were taken before
// the wait. The mutation that reads the clock after the service row stamps evaluated_at after
// the release (later than the edit) and fails; READ COMMITTED sees the new policy and fails;
// an id built from the application clock after the wait fails the database's CHECK and the
// decision errors. The next decision is the post-edit world entirely.
func TestGateBDecisionOneSnapshotUnderARealBlocker(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, 45*minute/60, 15*minute/60) // BurnedPercent = 50
	gateFreshLatches(t, st, ctx, f)
	gateArmCoverage(t, st, ctx, f, 90*time.Second)
	rev := gatePut(t, st, ctx, f, nil, gateDoc(nil)) // threshold 90, unknown warn

	blocker, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	btx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer btx.Rollback(ctx) //nolint:errcheck
	if _, err := btx.Exec(ctx, `LOCK TABLE services IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type res struct {
		dec domain.GateDecision
		err error
	}
	done := make(chan res, 1)
	go func() {
		d, err := st.DecideGate(ctx, f.projectID, f.serviceID, 10*time.Second)
		done <- res{d, err}
	}()
	waitForLockWaiters(t, st, ctx, "services", 1, 2500*time.Millisecond)

	// The edit, on a third connection, touching nothing that needs a lock on `services`.
	editor, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Release()
	etx, err := editor.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer etx.Rollback(ctx) //nolint:errcheck
	var editStart time.Time
	if err := etx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&editStart); err != nil {
		t.Fatal(err)
	}
	if _, err := etx.Exec(ctx, `
		UPDATE service_gate_policies
		   SET budget_consumed_percent = 50, unknown_behavior = 'block', revision = revision + 1,
		       updated_at = statement_timestamp()
		 WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := etx.Exec(ctx, `
		UPDATE service_burn_alert_state SET firing = true, last_verdict = 'fire', emitted_seq = 1
		 WHERE sla_target_id = $1 AND rule_key = $2`, f.targetID, gatePageKey); err != nil {
		t.Fatal(err)
	}
	if err := etx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := btx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var r res
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the decision did not return after the blocker released")
	}
	if r.err != nil {
		t.Fatalf("decision under the blocker: %v", r.err)
	}
	dec := r.dec
	consumed := false
	for _, e := range dec.Reasons {
		if e.Clause == domain.ClauseBudgetConsumed {
			consumed = true
		}
	}
	if dec.PolicyRevision == nil {
		t.Fatal("no policy_revision on the decision")
	}
	switch *dec.PolicyRevision {
	case rev:
		if consumed || dec.UnknownBehavior == nil || *dec.UnknownBehavior != domain.GateUnknownWarn {
			t.Errorf("MIXED snapshot: revision %d paired with consumed=%v unknown=%v", rev, consumed, dec.UnknownBehavior)
		}
	case rev + 1:
		if !consumed || dec.UnknownBehavior == nil || *dec.UnknownBehavior != domain.GateUnknownBlock {
			t.Errorf("MIXED snapshot: revision %d paired with consumed=%v unknown=%v", rev+1, consumed, dec.UnknownBehavior)
		}
	default:
		t.Errorf("policy_revision = %d, want %d or %d", *dec.PolicyRevision, rev, rev+1)
	}
	// And it is the PRE-edit side: the snapshot predates the blocked statement.
	wantOutcome(t, dec, domain.GateStateAllow, domain.GateActionAllow)
	wantReasons(t, dec)
	if *dec.PolicyRevision != rev {
		t.Errorf("the decision saw revision %d; the snapshot was taken before the wait, so it must be %d", *dec.PolicyRevision, rev)
	}
	if !dec.EvaluatedAt.Before(editStart) {
		t.Errorf("evaluated_at %s is not before the edit %s: the clock was read after the lock wait, not at the snapshot",
			dec.EvaluatedAt.Format(time.RFC3339Nano), editStart.Format(time.RFC3339Nano))
	}
	if ms, _ := gateDecisionIDMillis(dec.DecisionID); ms != dec.EvaluatedAt.UnixMilli() {
		t.Errorf("id millisecond %d ≠ evaluated_at millisecond %d", ms, dec.EvaluatedAt.UnixMilli())
	}

	after := gateDecide(t, st, ctx, f)
	wantOutcome(t, after, domain.GateStateBlock, domain.GateActionBlock)
	wantReasons(t, after, "page_burn_firing=page_burn_firing", "budget_consumed=budget_consumed")
	if after.PolicyRevision == nil || *after.PolicyRevision != rev+1 || after.UnknownBehavior == nil || *after.UnknownBehavior != domain.GateUnknownBlock {
		t.Errorf("the next decision is not the post-edit world: rev=%v unknown=%v", after.PolicyRevision, after.UnknownBehavior)
	}
}

// The id's millisecond is the DATABASE instant of the decision transaction (§5 identity,
// invariant 12): evaluated_at lies between the transaction's BEGIN (`now()`, frozen at BEGIN)
// and a statement issued later inside the same transaction, the id carries exactly that
// millisecond, and the row satisfies the database's binding CHECK. There is no seam to skew the
// application clock; TestGateBDecisionOneSnapshotUnderARealBlocker supplies the skew instead
// (the wait between the clock read and the id), so an id built from time.Now() fails there.
func TestGateBDecisionIDMillisecondIsTheDBInstantOfTheTransaction(t *testing.T) {
	st, ctx := gateStore(t)
	f := gateService(t, st, ctx, 2*time.Minute, minute, 0)
	gateFreshLatches(t, st, ctx, f)
	gatePut(t, st, ctx, f, nil, gateDoc(nil))

	var txBegin, later time.Time
	gateDecisionHook = func(hctx context.Context, _ int, phase string, tx pgx.Tx) error {
		if phase != gatePhasePolicyRead {
			return nil
		}
		return tx.QueryRow(hctx, `SELECT now(), statement_timestamp()`).Scan(&txBegin, &later)
	}
	t.Cleanup(func() { gateDecisionHook = nil })

	dec := gateDecide(t, st, ctx, f)
	if txBegin.IsZero() || later.IsZero() {
		t.Fatal("the hook did not run")
	}
	if dec.EvaluatedAt.Before(txBegin) || dec.EvaluatedAt.After(later) {
		t.Errorf("evaluated_at %s is outside [BEGIN %s, later statement %s]: not the transaction's own instant",
			dec.EvaluatedAt.Format(time.RFC3339Nano), txBegin.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano))
	}
	if ms, ok := gateDecisionIDMillis(dec.DecisionID); !ok || ms != dec.EvaluatedAt.UnixMilli() {
		t.Errorf("id carries %d, evaluated_at is %d", ms, dec.EvaluatedAt.UnixMilli())
	}
	var stored time.Time
	var bound bool
	if err := st.pool.QueryRow(ctx, `
		SELECT evaluated_at, gate_uuid_ms(id) = floor(extract(epoch FROM evaluated_at) * 1000)
		  FROM service_gate_decisions WHERE id = $1`, dec.DecisionID).Scan(&stored, &bound); err != nil {
		t.Fatal(err)
	}
	if !bound || !stored.Equal(dec.EvaluatedAt) {
		t.Errorf("row: bound=%v stored=%s returned=%s", bound, stored.Format(time.RFC3339Nano), dec.EvaluatedAt.Format(time.RFC3339Nano))
	}
}
