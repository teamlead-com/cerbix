package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// D10 "Ownership: the registry" (§7 Ownership, invariant 20). The registry is crash-consistent
// and ownership is the marker: these tests crash the pass at EVERY statement boundary of
// create, attach, detaching, detach, detached and drop, plant every reconcilable catalog state,
// stale every relid, and plant foreign relations under our names.

// crashAfter returns hooks that kill the pass's own backend right after the statement `stage`
// has run — at the next statement's hook — which is what a process death between two statements
// looks like to the server: the in-flight transaction is rolled back, committed ones stay, the
// advisory lock is released with the connection.
func crashAfter(t *testing.T, st *Store, ctx context.Context, stage string, crashed *bool) *gateMaintenanceHooks {
	var mu sync.Mutex
	var pid int
	seen := false
	return &gateMaintenanceHooks{beforeStatement: func(hctx context.Context, s string, conn dbConn) error {
		mu.Lock()
		defer mu.Unlock()
		if pid == 0 {
			p, err := backendPidOf(hctx, conn)
			if err != nil {
				return err
			}
			pid = p
		}
		if seen && !*crashed {
			*crashed = true
			var ok bool
			if err := st.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil || !ok {
				t.Errorf("terminate %d: ok=%v err=%v", pid, ok, err)
			}
			waitFor(t, 5*time.Second, "the crashed backend to disappear", func() bool { return !backendAlive(t, st, ctx, pid) })
			return nil // the statement about to run fails on the dead connection
		}
		if s == stage {
			seen = true
		}
		return nil
	}}
}

// gateCreateStages / gateRemoveStages / gateDropStages are the statement boundaries of D10's
// three flows, in execution order.
var (
	gateCreateStages = []string{"create", "create.index", "create.insert", "create.comment", "create.commit",
		"attach", "attach.update", "attach.commit"}
	gateRemoveStages = []string{"detaching", "detaching.commit", "detach", "detached", "detached.commit"}
	gateDropStages   = []string{"drop.gate", "drop", "drop.update", "drop.commit"}
)

// §7 Ownership — the pass crashes after EACH statement of create and attach; the next pass
// converges: the day is attached and registered attached, registry and catalog agree, nothing
// was refused.
func TestGateMaintCrashAfterEveryCreateAndAttachStatementConverges(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	target := today.AddDate(0, 0, 7)
	for _, stage := range gateCreateStages {
		t.Run("after_"+stage, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			dropGateDay(t, st, ctx, target)
			cfg := gateMaintCfg
			cfg.CreateMax = 1
			crashed := false
			m := &gateMetricsFake{}
			_, acquired, err := runGatePass(t, st, ctx, cfg, nil, m, crashAfter(t, st, ctx, stage, &crashed))
			if !acquired {
				t.Fatal("not acquired")
			}
			if !crashed {
				t.Fatalf("the pass never reached the statement after %q", stage)
			}
			if err == nil {
				t.Fatal("a pass whose backend died reported success")
			}
			waitGateLockFree(t, st, ctx)
			// The next pass converges.
			rep, _, err := runGatePass(t, st, ctx, cfg, nil, m, nil)
			if err != nil {
				t.Fatalf("converging pass: %v", err)
			}
			if len(rep.Refusals) != 0 || m.get("partition_identity") != 0 {
				t.Fatalf("a crash outcome was refused: %v", rep.Refusals)
			}
			if c := gateLookup(t, st, ctx, target); !c.attached || c.state != "attached" || c.relid != c.oid {
				t.Fatalf("after convergence: %+v", c)
			}
			gateRegistryAgreesWithCatalog(t, st, ctx)
		})
	}
}

// §7 Ownership — the pass crashes after EACH statement of detaching / detach / detached, and
// then of the deferred drop; every next pass converges with registry and catalog agreeing.
func TestGateMaintCrashAfterEveryRemovalStatementConverges(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	old := today.AddDate(0, 0, -9)
	for _, stage := range gateRemoveStages {
		t.Run("after_"+stage, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			plantGateDay(t, st, ctx, old, "attached", nil)
			crashed := false
			m := &gateMetricsFake{}
			_, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, crashAfter(t, st, ctx, stage, &crashed))
			if !crashed || err == nil {
				t.Fatalf("crashed=%v err=%v", crashed, err)
			}
			rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
			if err != nil {
				t.Fatalf("converging pass: %v", err)
			}
			if len(rep.Refusals) != 0 || m.get("partition_identity") != 0 {
				t.Fatalf("a crash outcome was refused: %v", rep.Refusals)
			}
			if c := gateLookup(t, st, ctx, old); c.attached || c.pending || c.state != "detached" || !c.exists {
				t.Fatalf("after convergence: %+v, want standalone + detached", c)
			}
			gateRegistryAgreesWithCatalog(t, st, ctx)
		})
	}
	for _, stage := range gateDropStages {
		t.Run("after_"+stage, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			plantGateDay(t, st, ctx, old, "detached", nil)
			cfg := gateMaintCfg
			cfg.PurgeEvery = 0 // the drop is due at once (inclusive boundary)
			crashed := false
			m := &gateMetricsFake{}
			_, _, err := runGatePass(t, st, ctx, cfg, nil, m, crashAfter(t, st, ctx, stage, &crashed))
			if !crashed || err == nil {
				t.Fatalf("crashed=%v err=%v", crashed, err)
			}
			rep, _, err := runGatePass(t, st, ctx, cfg, nil, m, nil)
			if err != nil {
				t.Fatalf("converging pass: %v", err)
			}
			if len(rep.Refusals) != 0 || m.get("partition_identity") != 0 {
				t.Fatalf("a crash outcome was refused: %v", rep.Refusals)
			}
			if c := gateLookup(t, st, ctx, old); c.exists || c.state != "dropped" {
				t.Fatalf("after convergence: %+v, want the table gone and the registry dropped", c)
			}
			gateRegistryAgreesWithCatalog(t, st, ctx)
		})
	}
}

// §7 Ownership — the three outcomes of a crash around DETACH … CONCURRENTLY, planted directly:
// `detaching` + still attached → the DETACH runs; + detach-pending → FINALIZE; + standalone →
// state = detached only.
func TestGateMaintReconcilesEveryDetachingOutcome(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	attachedDay := today.AddDate(0, 0, -30)
	pendingDay := today.AddDate(0, 0, -25)
	standaloneDay := today.AddDate(0, 0, -20)
	for _, d := range []time.Time{attachedDay, pendingDay, standaloneDay} {
		plantGateDay(t, st, ctx, d, "attached", nil)
		if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET state = 'detaching' WHERE day = $1::date`, d.Format("2006-01-02")); err != nil {
			t.Fatal(err)
		}
	}
	makeDetachPending(t, st, ctx, pendingDay)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE service_gate_decisions DETACH PARTITION %s`, gateRelname(standaloneDay))); err != nil {
		t.Fatal(err)
	}
	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Detached != 1 || rep.Finalized != 1 || len(rep.Failures) != 0 || len(rep.Refusals) != 0 {
		t.Fatalf("detached=%d finalized=%d failures=%v refusals=%v", rep.Detached, rep.Finalized, rep.Failures, rep.Refusals)
	}
	for _, d := range []time.Time{attachedDay, pendingDay, standaloneDay} {
		if c := gateLookup(t, st, ctx, d); c.attached || c.pending || c.state != "detached" || !c.exists {
			t.Fatalf("%s after the pass: %+v", d.Format("2006-01-02"), c)
		}
	}
	var stamped int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM service_gate_decision_partitions WHERE state = 'detached' AND detached_at IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 3 {
		t.Fatalf("%d detached rows carry detached_at, want 3", stamped)
	}
	if m.get("partition_identity") != 0 {
		t.Fatal("a reconcilable state was refused")
	}
}

// §7 Ownership — every relid in the registry is set to a stale value: the pass rebinds each by
// marker and refuses nothing (the pg_dump/pg_restore case, where every relation has a new OID).
func TestGateMaintStaleRelidsAreRebindedByMarker(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -9), "attached", nil)
	plantGateDay(t, st, ctx, today.AddDate(0, 0, -20), "detached", nil)
	if _, err := st.pool.Exec(ctx, `UPDATE service_gate_decision_partitions SET relid = 4000000000 + (extract(day FROM day))::int`); err != nil {
		t.Fatal(err)
	}
	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Refusals) != 0 || m.get("partition_identity") != 0 {
		t.Fatalf("stale relids were refused: %v", rep.Refusals)
	}
	var stale int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_gate_decision_partitions p
		 WHERE p.state <> 'dropped' AND p.relid IS DISTINCT FROM to_regclass(p.relname)::oid`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d registry rows still carry a stale relid after the pass", stale)
	}
	if rep.Detached != 1 || !rep.GaugesValid || rep.Gauges.Bytes <= 0 {
		t.Fatalf("the pass did not act normally on rebound rows: %+v", rep)
	}
}

// §7 Ownership — a relation planted under next day's name WITHOUT the marker, with another
// owner's token, or with our marker but a different bounds CHECK: that day is refused, counted
// partition_identity, the relation is left untouched, and the OTHER days are still created and
// removed in the same pass.
func TestGateMaintForeignOrMisshapenRelationRefusesOnlyItsDay(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	victim := today.AddDate(0, 0, 6)
	other := today.AddDate(0, 0, 7)
	old := today.AddDate(0, 0, -9)
	rel := gateRelname(victim)
	lo, hi := gateDayBounds(victim)
	wrongLo, wrongHi := gateDayBounds(victim.AddDate(0, 0, 1))

	plant := map[string]func(t *testing.T){
		"no_marker": func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE service_gate_decisions INCLUDING ALL, CONSTRAINT %s_day_chk CHECK (evaluated_at >= '%s'::timestamptz AND evaluated_at < '%s'::timestamptz))`, rel, rel, lo, hi)); err != nil {
				t.Fatal(err)
			}
		},
		"another_token": func(t *testing.T) {
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE service_gate_decisions INCLUDING ALL, CONSTRAINT %s_day_chk CHECK (evaluated_at >= '%s'::timestamptz AND evaluated_at < '%s'::timestamptz))`, rel, rel, lo, hi)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`COMMENT ON TABLE %s IS 'cerbix:gate-ledger:%s'`, rel, "00000000-0000-4000-8000-000000000001")); err != nil {
				t.Fatal(err)
			}
		},
		"our_marker_wrong_bounds": func(t *testing.T) {
			// Registered `created` with token T; the relation carries T but the NEXT day's CHECK.
			var token string
			if err := st.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&token); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				fmt.Sprintf(`CREATE TABLE %s (LIKE service_gate_decisions INCLUDING ALL, CONSTRAINT %s_day_chk CHECK (evaluated_at >= '%s'::timestamptz AND evaluated_at < '%s'::timestamptz))`, rel, rel, wrongLo, wrongHi),
				fmt.Sprintf(`COMMENT ON TABLE %s IS 'cerbix:gate-ledger:%s'`, rel, token),
				fmt.Sprintf(`INSERT INTO service_gate_decision_partitions (day, relname, owner_token, relid, state) VALUES ('%s', '%s', '%s', to_regclass('%s')::oid, 'created')`, victim.Format("2006-01-02"), rel, token, rel),
			} {
				if _, err := st.pool.Exec(ctx, q); err != nil {
					t.Fatalf("%v\n%s", err, q)
				}
			}
		},
	}
	for name, do := range plant {
		t.Run(name, func(t *testing.T) {
			resetGateLedger(t, st, ctx)
			dropGateDay(t, st, ctx, victim)
			dropGateDay(t, st, ctx, other)
			plantGateDay(t, st, ctx, old, "attached", nil)
			do(t)
			var oidBefore uint32
			if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1)::oid`, rel).Scan(&oidBefore); err != nil {
				t.Fatal(err)
			}
			m := &gateMetricsFake{}
			rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
			if err != nil {
				t.Fatalf("pass: %v", err)
			}
			if m.get("partition_identity") != 1 || len(rep.Refusals) != 1 || !strings.Contains(rep.Refusals[0], victim.Format("2006-01-02")) {
				t.Fatalf("partition_identity=%d refusals=%v, want exactly the planted day", m.get("partition_identity"), rep.Refusals)
			}
			// The relation is untouched: same OID, not attached, its CHECK as planted.
			c := gateLookup(t, st, ctx, victim)
			if !c.exists || c.attached || c.oid != oidBefore {
				t.Fatalf("the planted relation was touched: %+v (oid before %d)", c, oidBefore)
			}
			if name != "our_marker_wrong_bounds" && c.state != "" {
				t.Fatalf("a foreign relation was registered: %+v", c)
			}
			// The other missing day was created and the eligible day removed in the same pass.
			if oc := gateLookup(t, st, ctx, other); !oc.attached || oc.state != "attached" {
				t.Fatalf("the other day was not created: %+v", oc)
			}
			if oc := gateLookup(t, st, ctx, old); oc.attached || oc.state != "detached" {
				t.Fatalf("removal did not proceed: %+v", oc)
			}
			if rep.Detached != 1 || rep.Attached != 1 {
				t.Fatalf("attached=%d detached=%d", rep.Attached, rep.Detached)
			}
			// Cleanup for the next variant: the planted relation is not ours to drop in the pass,
			// so the test drops it.
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, rel)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// §7 Ownership — a registry row `attached` whose partition was detached by hand is refused and
// never re-created silently: the relation stays as the operator left it, the registry keeps
// saying attached, and every pass counts partition_identity again.
func TestGateMaintAttachedRowDetachedByHandIsRefusedNotRecreated(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	day := today.AddDate(0, 0, 3)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE service_gate_decisions DETACH PARTITION %s`, gateRelname(day))); err != nil {
		t.Fatal(err)
	}
	m := &gateMetricsFake{}
	for pass := 1; pass <= 2; pass++ {
		rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Refusals) != 1 || !strings.Contains(rep.Refusals[0], "detached by hand") {
			t.Fatalf("pass %d refusals: %v", pass, rep.Refusals)
		}
		if rep.Created != 0 || rep.Attached != 0 {
			t.Fatalf("pass %d re-created the day: created=%d attached=%d", pass, rep.Created, rep.Attached)
		}
		c := gateLookup(t, st, ctx, day)
		if !c.exists || c.attached || c.state != "attached" {
			t.Fatalf("pass %d changed the day: %+v", pass, c)
		}
	}
	if m.get("partition_identity") != 2 {
		t.Fatalf("partition_identity counted %d times over two passes, want 2", m.get("partition_identity"))
	}
	// And a decision for that day fails with 23514 rather than landing anywhere else.
	proj, _, svc := seedService(t, st, ctx)
	at := day.Add(time.Hour)
	allow := "ALLOW"
	if err := insertDecision(st, ctx, gateUUIDv7(t, gateMs(at)), proj, &svc, "ALLOW", &allow, at, `{}`); pgCodeOf(err) != "23514" {
		t.Fatalf("a decision for the hand-detached day: %v", err)
	}
}

// §7 Ownership — a registry row whose relation is gone (state ≠ dropped) and a row whose relation
// is attached again by hand after `detached` are both refused, each once per pass.
func TestGateMaintUnreconcilableStatesAreRefusedOnce(t *testing.T) {
	st, ctx := gateMaintStore(t)
	today := gateToday(t, st, ctx)
	gone := today.AddDate(0, 0, -12)
	plantGateDay(t, st, ctx, gone, "detached", nil)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, gateRelname(gone))); err != nil {
		t.Fatal(err)
	}
	reattached := today.AddDate(0, 0, -15)
	plantGateDay(t, st, ctx, reattached, "detached", nil)
	lo, hi := gateDayBounds(reattached)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE service_gate_decisions ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`, gateRelname(reattached), lo, hi)); err != nil {
		t.Fatal(err)
	}
	m := &gateMetricsFake{}
	rep, _, err := runGatePass(t, st, ctx, gateMaintCfg, nil, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.get("partition_identity") != 2 || len(rep.Refusals) != 2 {
		t.Fatalf("partition_identity=%d refusals=%v, want both days once", m.get("partition_identity"), rep.Refusals)
	}
	if c := gateLookup(t, st, ctx, reattached); !c.attached || c.state != "detached" {
		t.Fatalf("the hand-reattached partition was touched: %+v", c)
	}
	if c := gateLookup(t, st, ctx, gone); c.state != "detached" {
		t.Fatalf("the gone relation's row was rewritten: %+v", c)
	}
}
