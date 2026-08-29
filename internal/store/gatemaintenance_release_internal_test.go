package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// D10 "Fencing and the loop" and "Release is a proof" (§7, invariant 18). The oracle is the
// server: which backend holds the gate lock, whether a terminated pid ever comes back, what SHOW
// answers on a reacquired connection.

// showSessionBounds reads both timeouts on a pooled connection.
func showSessionBounds(t *testing.T, conn *pgxpool.Conn, ctx context.Context) (string, string) {
	t.Helper()
	var lt, st string
	if err := conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&lt); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return lt, st
}

// reacquirePid borrows pool connections until pid is handed out (or tries are spent). Every
// borrowed connection's SHOW must read 0/0 — no setting of the pass may leak to any borrower.
func reacquirePid(t *testing.T, st *Store, ctx context.Context, pid, tries int) (found bool, seen []int) {
	t.Helper()
	for i := 0; i < tries; i++ {
		conn, err := st.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got int
		if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&got); err != nil {
			conn.Release()
			t.Fatal(err)
		}
		lt, stmt := showSessionBounds(t, conn, ctx)
		conn.Release()
		seen = append(seen, got)
		if lt != "0" || stmt != "0" {
			t.Fatalf("borrowed backend %d carries lock_timeout=%s statement_timeout=%s", got, lt, stmt)
		}
		if got == pid {
			found = true
		}
	}
	return found, seen
}

// pidCapture records the pass's backend pid from its first hook.
func pidCapture(pid *int) func(ctx context.Context, stage string, conn dbConn) error {
	var once sync.Once
	return func(ctx context.Context, _ string, conn dbConn) error {
		var err error
		once.Do(func() { *pid, err = backendPidOf(ctx, conn) })
		return err
	}
}

// §7 Fencing — the old side holds the gate session and is blocked in a DETACH behind an ACCESS
// EXCLUSIVE holder on the parent; pg_terminate_backend on its pid; a successor acquires the gate
// session and completes the removal; the old side's next statement fails on the dead connection
// and the partition was removed exactly once — by the successor.
func TestGateMaintFencingTerminatedOldSideCannotActAndSuccessorRemovesOnce(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	old := today.AddDate(0, 0, -9)
	plantGateDay(t, st, ctx, old, "attached", nil)

	// The ACCESS EXCLUSIVE holder on the parent is taken right BEFORE the DETACH statement (in
	// its hook), after the day's ownership reads: pg_get_expr(relpartbound) locks the parent too,
	// so a holder present from the start would block the shape read, not the detach.
	var pid int
	detaching := make(chan struct{}, 1)
	var holder struct {
		mu      sync.Mutex
		release func()
	}
	capture := pidCapture(&pid)
	hooks := &gateMaintenanceHooks{beforeStatement: func(hctx context.Context, stage string, conn dbConn) error {
		if err := capture(hctx, stage, conn); err != nil {
			return err
		}
		if stage == "detach" {
			hc, err := st.pool.Acquire(ctx)
			if err != nil {
				return err
			}
			htx, err := hc.Begin(ctx)
			if err != nil {
				hc.Release()
				return err
			}
			if _, err := htx.Exec(ctx, `LOCK TABLE service_gate_decisions IN ACCESS EXCLUSIVE MODE`); err != nil {
				_ = htx.Rollback(ctx)
				hc.Release()
				return err
			}
			var once sync.Once
			holder.mu.Lock()
			holder.release = func() {
				once.Do(func() {
					_ = htx.Rollback(context.Background())
					hc.Release()
				})
			}
			holder.mu.Unlock()
			select {
			case detaching <- struct{}{}:
			default:
			}
		}
		return nil
	}}
	release := func() {
		holder.mu.Lock()
		defer holder.mu.Unlock()
		if holder.release != nil {
			holder.release()
		}
	}
	t.Cleanup(release)
	oldMetrics := &gateMetricsFake{}
	type result struct {
		rep GateMaintenanceReport
		err error
	}
	res := make(chan result, 1)
	go func() {
		rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, oldMetrics, hooks)
		res <- result{rep, err}
	}()
	select {
	case <-detaching:
	case <-time.After(10 * time.Second):
		t.Fatal("the old side never reached its DETACH")
	}
	// The old side is waiting on the lock, holding the gate session.
	waitFor(t, 5*time.Second, "the old side to wait on the parent lock", func() bool {
		var waiting bool
		if err := st.pool.QueryRow(ctx, `SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting); err != nil {
			return false
		}
		return waiting
	})
	if holder := gateLockHeldBy(t, st, ctx); holder != pid {
		t.Fatalf("gate lock held by %d, the old side is %d", holder, pid)
	}
	var ok bool
	if err := st.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate: %v %v", ok, err)
	}
	r := <-res
	if r.err == nil {
		t.Fatal("the terminated old side reported a clean pass")
	}
	if !strings.Contains(r.err.Error(), "release unproven") {
		t.Fatalf("the old side's release was not treated as unproven: %v", r.err)
	}
	// Its next statement (the RESET after the failed DETACH) failed on the dead connection.
	var sawDeadStatement bool
	for _, f := range r.rep.Failures {
		if strings.HasPrefix(f, "detach:") {
			sawDeadStatement = true
		}
	}
	if !sawDeadStatement || r.rep.Detached != 0 {
		t.Fatalf("old side: detached=%d failures=%v", r.rep.Detached, r.rep.Failures)
	}
	release()
	waitFor(t, 5*time.Second, "the gate lock to be free", func() bool { return gateLockHeldBy(t, st, ctx) == 0 })
	// The intent survived; the partition is still attached — the old side did nothing after death.
	if c := gateLookup(t, st, ctx, old); !c.attached || c.state != "detaching" {
		t.Fatalf("after the old side died: %+v, want attached + detaching", c)
	}
	// The successor acquires and completes the removal.
	rep, acquired, err := runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, nil)
	if err != nil || !acquired || rep.Detached != 1 {
		t.Fatalf("successor: acquired=%v detached=%d err=%v failures=%v", acquired, rep.Detached, err, rep.Failures)
	}
	if c := gateLookup(t, st, ctx, old); c.attached || c.state != "detached" {
		t.Fatalf("after the successor: %+v", c)
	}
	var detachedRows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decision_partitions WHERE day = $1::date AND detached_at IS NOT NULL`, old.Format("2006-01-02")).Scan(&detachedRows); err != nil {
		t.Fatal(err)
	}
	if detachedRows != 1 {
		t.Fatalf("the partition was removed %d times", detachedRows)
	}
}

// §7 Release — after a clean pass, after a lock_timeout refusal and after a statement_timeout
// abort, the released connection is reacquired and SHOW lock_timeout / SHOW statement_timeout are
// both 0. The lock_timeout comes from a SHARE UPDATE EXCLUSIVE holder on the child (the DETACH
// waits for its first lock). The statement_timeout comes from a reader holding ACCESS SHARE on the
// parent: DETACH … CONCURRENTLY takes its locks, marks the partition detach-pending, then waits
// for that reader — a second lock wait with a fresh lock_timeout, while statement_timeout has
// been running since the statement began and fires first (57014), leaving the interrupted detach
// the next pass FINALIZEs.
func TestGateMaintReleaseLeavesNoSessionSettingBehind(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	old := today.AddDate(0, 0, -9)
	for _, tc := range []struct {
		name string
		// setup takes the SUBTEST's t so the lock it holds is released with the subtest, not
		// with the whole test (a holder outliving its subtest blocks the next one's reset). It
		// returns how far into the timeline the pass is acquired: passStart = now − offset.
		setup func(t *testing.T) (offset time.Duration)
		kind  string
	}{
		{"clean", func(*testing.T) time.Duration { return 0 }, ""},
		{"lock_timeout", func(t *testing.T) time.Duration {
			holdLock(t, st, ctx, "SHARE UPDATE EXCLUSIVE", gateRelname(old))
			return 0
		}, "lock_timeout"},
		{"statement_timeout", func(t *testing.T) time.Duration {
			holdLock(t, st, ctx, "ACCESS SHARE", "service_gate_decisions")
			return 25500 * time.Millisecond // ~1.4 s of removal slice left: st = lt = the remainder
		}, "statement_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			plantGateDay(t, st, ctx, old, "attached", nil)
			offset := tc.setup(t)
			clock := offsetClock(offset)
			passStart := clock().Add(-offset)
			var pid int
			log := &stageLog{}
			capture := pidCapture(&pid)
			rec := recordingHooks(log)
			hooks := &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
				if err := capture(ctx, stage, conn); err != nil {
					return err
				}
				return rec.beforeStatement(ctx, stage, conn)
			}}
			m := &gateMetricsFake{}
			rep, acquired, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, clock, m, hooks)
			if err != nil || !acquired {
				t.Fatalf("pass: acquired=%v err=%v", acquired, err)
			}
			if tc.kind != "" && m.get(tc.kind) != 1 {
				det, _ := log.last("detach")
				t.Fatalf("%s counted %d times (counts %v); failures=%v; day=%+v; bounds at DETACH: st=%s lt=%s",
					tc.kind, m.get(tc.kind), m.counts, rep.Failures, gateLookup(t, st, ctx, old), det.st, det.lt)
			}
			if tc.kind == "statement_timeout" {
				if c := gateLookup(t, st, ctx, old); !c.pending || c.state != "detaching" {
					t.Fatalf("the statement-timed-out detach did not leave a detach-pending partition under a detaching intent: %+v", c)
				}
				// The clamp was the mechanism: both bounds were the slice remainder, under 2 s.
				if det, ok := log.last("detach"); !ok || det.st != det.lt || det.st >= gateLockTimeoutMax {
					t.Fatalf("bounds at the DETACH: st=%s lt=%s, want equal and clamped below 2 s", det.st, det.lt)
				}
			}
			if pid == 0 {
				t.Fatal("no pid captured")
			}
			found, seen := reacquirePid(t, st, ctx, pid, 50)
			if !found {
				t.Fatalf("the released backend %d was never handed out again over 50 acquisitions (%v); it should have been returned clean", pid, seen)
			}
		})
	}
}

// §7 Release — after a dead-connection release the poisoned backend's pid is never handed out
// again (compared over 50 reacquisitions), a successor acquires the gate lock, and — under
// MinConns = 0 only — TotalConns drops by one before any replenishment.
func TestGateMaintDeadConnectionIsNeverReturnedToThePool(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)

	// The pass's own backend is terminated before its DETACH; every later statement fails.
	var pid int
	capture := pidCapture(&pid)
	hooks := &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
		if err := capture(ctx, stage, conn); err != nil {
			return err
		}
		if stage == "detach" {
			if _, err := st.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
				return err
			}
			waitFor(t, 5*time.Second, "the backend to die", func() bool { return !backendAlive(t, st, ctx, pid) })
		}
		return nil
	}}
	_, acquired, err := runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, hooks)
	if !acquired || err == nil || !strings.Contains(err.Error(), "release unproven") {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	found, _ := reacquirePid(t, st, ctx, pid, 50)
	if found {
		t.Fatalf("the poisoned backend %d was handed out again", pid)
	}
	rep, acquired, err := runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, nil)
	if err != nil || !acquired || rep.Detached != 1 {
		t.Fatalf("successor: acquired=%v detached=%d err=%v", acquired, rep.Detached, err)
	}

	// Pool cardinality, under MinConns = 0 only: a raw pool the Store's floor cannot describe.
	p := rawPool(t, ctx, 0, 3)
	rawStore := &Store{pool: p}
	warm, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	warm.Release()
	before := p.Stat().TotalConns()
	if before != 1 {
		t.Fatalf("raw pool holds %d connections before the pass, want 1", before)
	}
	resetGateLedger(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)
	var rawPid int
	capture = pidCapture(&rawPid)
	hooks = &gateMaintenanceHooks{beforeStatement: func(ctx context.Context, stage string, conn dbConn) error {
		if err := capture(ctx, stage, conn); err != nil {
			return err
		}
		if stage == "detach" {
			if _, err := st.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, rawPid); err != nil {
				return err
			}
			waitFor(t, 5*time.Second, "the backend to die", func() bool { return !backendAlive(t, st, ctx, rawPid) })
		}
		return nil
	}}
	if _, acquired, err := rawStore.runGateLedgerMaintenancePass(ctx, time.Now(), gateMaintCfg, time.Now, &gateMetricsFake{}, hooks); !acquired || err == nil {
		t.Fatalf("raw pass: acquired=%v err=%v", acquired, err)
	}
	if after := p.Stat().TotalConns(); after != before-1 {
		t.Fatalf("TotalConns %d → %d after a hijacked release, want a drop of one", before, after)
	}
}

// §7 Release — a RESET that blackholes (the hook waits on the release context) and an unlock that
// errors each end the release within the 3 s cleanup deadline, terminate the backend, and let a
// successor acquire. An unlock answering NULL is an UNKNOWN outcome and is treated the same —
// the mutation that ignores the unlock's boolean returns that connection to the pool and fails.
// The mutation that restores context.Background() hangs the blackhole case past the deadline.
func TestGateMaintUnprovenReleaseHijacksWithinTheDeadline(t *testing.T) {
	st, ctx := gateMaintStore(t)
	for _, tc := range []struct {
		name  string
		hooks func(pid *int) *gateMaintenanceHooks
	}{
		{"reset_blackhole", func(pid *int) *gateMaintenanceHooks {
			return &gateMaintenanceHooks{
				beforeStatement: pidCapture(pid),
				beforeRelease: func(ctx context.Context, conn *pgxpool.Conn) error {
					// A RESET that never answers: a statement held on the pass's own connection
					// until the release context expires.
					_, err := conn.Exec(ctx, `SELECT pg_sleep(30)`)
					return err
				},
			}
		}},
		{"unlock_error", func(pid *int) *gateMaintenanceHooks {
			return &gateMaintenanceHooks{beforeStatement: pidCapture(pid), unlockSQL: `SELECT 1/0 WHERE $1::bigint IS NOT NULL`}
		}},
		{"unlock_null", func(pid *int) *gateMaintenanceHooks {
			return &gateMaintenanceHooks{beforeStatement: pidCapture(pid), unlockSQL: `SELECT NULL::boolean WHERE $1::bigint IS NOT NULL`}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			var pid int
			passStart := time.Now()
			_, acquired, err := st.runGateLedgerMaintenancePass(ctx, passStart, gateMaintCfg, time.Now, &gateMetricsFake{}, tc.hooks(&pid))
			elapsed := time.Since(passStart)
			if !acquired {
				t.Fatal("not acquired")
			}
			if err == nil || !strings.Contains(err.Error(), "release unproven") {
				t.Fatalf("release reported proven: %v", err)
			}
			// The whole pass here is a few statements; the release owns at most 3 s of it.
			if elapsed > gateReleaseBudget+2*time.Second {
				t.Fatalf("pass with an unproven release took %s; the release must end within its 3 s", elapsed)
			}
			waitFor(t, 5*time.Second, "the poisoned backend to be terminated", func() bool { return !backendAlive(t, st, ctx, pid) })
			if found, _ := reacquirePid(t, st, ctx, pid, 50); found {
				t.Fatalf("the poisoned backend %d came back from the pool", pid)
			}
			waitGateLockFree(t, st, ctx)
			rep, acquired, err := runGatePass(t, st, ctx, gateMaintCfg, nil, &gateMetricsFake{}, nil)
			if err != nil || !acquired || !rep.GaugesValid {
				t.Fatalf("successor: acquired=%v err=%v", acquired, err)
			}
		})
	}
}

// ReleaseProved is the exported shape the scheduler-independent callers get: a clean release
// returns the connection and the lock, and a second acquisition of the key succeeds at once.
func TestGateMaintReleaseProvedExportedPath(t *testing.T) {
	st, ctx := gateMaintStore(t)
	ls, ok, err := st.TryBecomeLeaderSession(ctx, GateMaintenanceLockKey)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if _, _, err := st.TryBecomeLeaderSession(ctx, GateMaintenanceLockKey); err != nil {
		t.Fatal(err)
	}
	if _, ok2, _ := st.TryBecomeLeaderSession(ctx, GateMaintenanceLockKey); ok2 {
		t.Fatal("the gate lock was acquired twice")
	}
	if err := ls.ReleaseProved(time.Now().Add(gateReleaseBudget)); err != nil {
		t.Fatalf("ReleaseProved: %v", err)
	}
	ls2, ok, err := st.TryBecomeLeaderSession(ctx, GateMaintenanceLockKey)
	if err != nil || !ok {
		t.Fatalf("reacquire after a proven release: ok=%v err=%v", ok, err)
	}
	ls2.Release()
}
